// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Speicherung in einzelnen Dateien:
//
//	data/
//	  config.json                   Einstellungen (Titel, Kanäle erstellen, Farben)
//	  automod.json                  Automod-Regeln
//	  users.json                    reservierte Namen (nur Token-Hashes)
//	  bots.json                     Bots (Schlüssel nur als Hash, Webhook-Geheimnis im Klartext)
//	  rooms/
//	    lobby.json                  Kanal: Name, Beschreibung, Erstellt
//	    lobby.messages.jsonl        Nachrichten des Kanals, eine JSON-Nachricht pro Zeile
//
// Kleine Dateien (Einstellungen, Automod, Kanal-Infos) werden bei Änderungen atomar
// ersetzt. Nachrichten werden nur angehängt – auch bei sehr langen Verläufen bleibt das
// Schreiben schnell. Ändert sich eine Nachricht (Moderation, Umbenennung des Autors),
// wird ihre neue Fassung angehängt; beim Laden gilt die letzte Fassung. Überholte Zeilen
// werden beim Start automatisch aufgeräumt.

type Store struct {
	dir   string
	mu    sync.Mutex
	files map[string]*os.File // offene Nachrichten-Dateien je Kanal
}

func NewStore(dir string) *Store { return &Store{dir: dir, files: map[string]*os.File{}} }

func (s *Store) path(name string) string     { return filepath.Join(s.dir, name) }
func (s *Store) roomsDir() string            { return filepath.Join(s.dir, "rooms") }
func (s *Store) metaPath(room string) string { return filepath.Join(s.roomsDir(), room+".json") }
func (s *Store) msgPath(room string) string {
	return filepath.Join(s.roomsDir(), room+".messages.jsonl")
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// writeFileAtomic schreibt in eine temporäre Datei, erzwingt das Schreiben auf die Platte
// (fsync) und ersetzt dann die Zieldatei in einem Schritt – bei einem Absturz bleibt immer
// entweder der alte oder der neue, nie ein halber Stand übrig.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nach erfolgreichem Rename ein No-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// --- Schreiben ---

// Flush speichert alle vorgemerkten Änderungen. Schlägt etwas fehl, bleiben die
// noch nicht gespeicherten Änderungen vorgemerkt und werden beim nächsten Mal erneut versucht.
func (s *Store) Flush(h *Hub, u *Users) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := h.takeOps()
	// Wird das Protokoll ohnehin neu geschrieben, enthält es alle Einträge dieses Durchlaufs
	// schon – einzelne Anhänge würden sie verdoppeln.
	auditRewrite := false
	for _, op := range ops {
		if op.kind == opAuditRewrite {
			auditRewrite = true
		}
	}
	docs := map[string]bool{}
	var docOrder []string
	requeueDocs := func() {
		for _, d := range docOrder {
			h.requeue([]storeOp{{kind: opDoc, doc: d}})
		}
	}
	touched := map[*os.File]bool{}

	for i, op := range ops {
		if op.kind == opDoc {
			if !docs[op.doc] {
				docs[op.doc] = true
				docOrder = append(docOrder, op.doc)
			}
			continue
		}
		if op.kind == opAudit && auditRewrite {
			continue
		}
		if err := s.apply(h, op, touched); err != nil {
			h.requeue(ops[i:])
			requeueDocs()
			return err
		}
	}
	for f := range touched {
		if err := f.Sync(); err != nil {
			requeueDocs()
			return err
		}
	}
	if len(touched) > 0 {
		syncDir(s.roomsDir())
	}
	for k, d := range docOrder {
		spec, ok := h.docs[d]
		if !ok {
			continue
		}
		if err := writeJSONFile(s.path(d), spec.save()); err != nil {
			docOrder = docOrder[k:]
			requeueDocs()
			return err
		}
	}
	if u != nil && u.dirty.Swap(false) {
		if err := writeJSONFile(s.path("users.json"), u.export()); err != nil {
			u.markDirty()
			return err
		}
	}
	return nil
}

func (s *Store) apply(h *Hub, op storeOp, touched map[*os.File]bool) error {
	switch op.kind {
	case opRoomMeta:
		if meta, ok := h.metaOf(op.room); ok {
			return writeJSONFile(s.metaPath(op.room), meta)
		}
	case opMessage:
		f, err := s.appendFile(op.room)
		if err != nil {
			return err
		}
		line, err := json.Marshal(op.msg)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
		touched[f] = true
	case opRoomRewrite:
		msgs, ok := h.roomMessages(op.room)
		if !ok {
			return nil
		}
		s.closeTouched(op.room, touched)
		return s.writeMessages(op.room, msgs)
	case opAudit:
		f, err := s.appendFile(auditKey)
		if err != nil {
			return err
		}
		line, err := json.Marshal(op.audit)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
		touched[f] = true
	case opAuditRewrite:
		s.closeTouched(auditKey, touched)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, e := range h.audit.all() {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return writeFileAtomic(s.path("audit.jsonl"), buf.Bytes())
	case opRoomRename:
		s.closeTouched(op.room, touched)
		s.closeTouched(op.newName, touched)
		for _, mv := range [][2]string{
			{s.msgPath(op.room), s.msgPath(op.newName)},
			{s.metaPath(op.room), s.metaPath(op.newName)},
		} {
			if err := os.Rename(mv[0], mv[1]); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if meta, ok := h.metaOf(op.newName); ok {
			if err := writeJSONFile(s.metaPath(op.newName), meta); err != nil {
				return err
			}
			// Aus dem Speicher neu schreiben: Ein noch nicht ausgeführtes Neuschreiben unter dem
			// alten Namen (z. B. nach endgültigem Löschen) darf nicht verloren gehen.
			if msgs, ok := h.roomMessages(op.newName); ok {
				return s.writeMessages(op.newName, msgs)
			}
		}
	case opRoomDelete:
		s.closeTouched(op.room, touched)
		for _, p := range []string{s.msgPath(op.room), s.metaPath(op.room)} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		syncDir(s.roomsDir())
	}
	return nil
}

// auditKey ist der interne Schlüssel der Protokolldatei unter den offenen Dateien
// (kann kein Kanalname sein, weil Kanalnamen keine Leerzeichen enthalten).
const auditKey = " audit"

func (s *Store) appendFile(room string) (*os.File, error) {
	if f, ok := s.files[room]; ok {
		return f, nil
	}
	if err := os.MkdirAll(s.roomsDir(), 0o700); err != nil {
		return nil, err
	}
	path := s.msgPath(room)
	if room == auditKey {
		path = s.path("audit.jsonl")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	s.files[room] = f
	return f, nil
}

// closeTouched synchronisiert und schließt eine offene Datei, bevor sie ersetzt wird.
func (s *Store) closeTouched(room string, touched map[*os.File]bool) {
	if f, ok := s.files[room]; ok {
		if touched[f] {
			f.Sync()
			delete(touched, f)
		}
	}
	s.closeFile(room)
}

func (s *Store) closeFile(room string) {
	if f, ok := s.files[room]; ok {
		f.Close()
		delete(s.files, room)
	}
}

// Close schließt alle offenen Dateien.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for room := range s.files {
		s.closeFile(room)
	}
}

// WriteAll schreibt den kompletten Zustand neu (Migration aus dem alten Format).
func (s *Store) WriteAll(h *Hub, u *Users) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range h.order {
		if err := writeJSONFile(s.path(d), h.docs[d].save()); err != nil {
			return err
		}
	}
	if u != nil {
		if err := writeJSONFile(s.path("users.json"), u.export()); err != nil {
			return err
		}
	}
	for name, r := range h.snapshotRooms() {
		if err := writeJSONFile(s.metaPath(name), r.meta); err != nil {
			return err
		}
		if err := s.writeMessages(name, r.msgs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeMessages(room string, msgs []Message) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return writeFileAtomic(s.msgPath(room), buf.Bytes())
}

// --- Laden ---

// Load liest alle Dateien ein. Gibt es noch eine Datei im alten Format (gochat.json)
// und noch keine neuen Dateien, wird sie übernommen und danach umbenannt.
func (s *Store) Load(h *Hub, u *Users) error {
	if err := os.MkdirAll(s.roomsDir(), 0o700); err != nil {
		return err
	}
	if migrated, err := s.migrateLegacy(h, u); err != nil || migrated {
		return err
	}

	for _, d := range h.order {
		data, err := os.ReadFile(s.path(d))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := h.docs[d].load(data); err != nil {
			return corruptErr(s.path(d), err)
		}
	}
	if err := s.loadAudit(h); err != nil {
		return err
	}
	var users []user
	if ok, err := readJSONFile(s.path("users.json"), &users); err != nil {
		return err
	} else if ok && u != nil {
		u.restore(users)
	}

	entries, err := os.ReadDir(s.roomsDir())
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		switch {
		case strings.HasSuffix(n, ".messages.jsonl"):
			names[strings.TrimSuffix(n, ".messages.jsonl")] = true
		case strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, "."):
			names[strings.TrimSuffix(n, ".json")] = true
		}
	}
	for name := range names {
		if !ValidRoom(name) {
			log.Printf("Überspringe unbekannte Datei für Kanal %q in %s", name, s.roomsDir())
			continue
		}
		meta := roomMeta{Name: name}
		if _, err := readJSONFile(s.metaPath(name), &meta); err != nil {
			return err
		}
		meta.Name = name // der Dateiname ist maßgeblich
		msgs, err := s.loadMessages(name)
		if err != nil {
			return err
		}
		h.putRoom(meta, msgs)
	}
	return nil
}

// loadAudit liest das Protokoll (eine Zeile je Eintrag).
func (s *Store) loadAudit(h *Hub) error {
	path := s.path("audit.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := truncatePartialLine(f, path); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var list []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			log.Printf("%s: Zeile übersprungen (%v)", path, err)
			continue
		}
		list = append(list, e)
	}
	h.audit.restore(list)
	return sc.Err()
}

// readJSONFile liest eine JSON-Datei. Fehlt sie, ist das kein Fehler (ok = false).
func readJSONFile(path string, v any) (ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, corruptErr(path, err)
	}
	return true, nil
}

func corruptErr(path string, err error) error {
	return fmt.Errorf("%s ist beschädigt (%v). Der Server startet nicht, damit nichts überschrieben wird – bitte prüfen oder aus einem Backup wiederherstellen", path, err)
}

// loadMessages liest die Nachrichten eines Kanals. Eine unvollständige letzte Zeile
// (Absturz mitten im Schreiben) wird entfernt; spätere Fassungen einer Nachricht
// ersetzen frühere. Viele überholte Zeilen werden aufgeräumt.
func (s *Store) loadMessages(room string) ([]Message, error) {
	path := s.msgPath(room)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if err := truncatePartialLine(f, path); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	var msgs []Message
	pos := map[int64]int{}
	lines := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		lines++
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, corruptErr(fmt.Sprintf("%s (Zeile %d)", path, lines), err)
		}
		m.Room = room
		if i, ok := pos[m.ID]; ok {
			msgs[i] = m // neuere Fassung
		} else {
			pos[m.ID] = len(msgs)
			msgs = append(msgs, m)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })

	if stale := lines - len(msgs); stale > 1000 && stale*4 > lines {
		start := time.Now()
		if err := s.writeMessages(room, msgs); err != nil {
			return nil, err
		}
		log.Printf("#%s aufgeräumt: %d überholte Zeilen entfernt (%v)", room, stale, time.Since(start).Round(time.Millisecond))
	}
	return msgs, nil
}

// truncatePartialLine entfernt eine nicht abgeschlossene letzte Zeile.
func truncatePartialLine(f *os.File, path string) error {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	size := st.Size()
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, size-1); err != nil {
		return err
	}
	if buf[0] == '\n' {
		return nil
	}
	// rückwärts bis zum letzten Zeilenumbruch suchen
	const chunk = 64 << 10
	end := size
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		b := make([]byte, end-start)
		if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
			return err
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			cut := start + int64(i) + 1
			log.Printf("%s: unvollständige letzte Zeile (%d Bytes) nach Absturz entfernt", path, size-cut)
			return f.Truncate(cut)
		}
		end = start
	}
	log.Printf("%s: unvollständige einzige Zeile nach Absturz entfernt", path)
	return f.Truncate(0)
}

// --- Übernahme aus dem alten Format (eine gemeinsame gochat.json) ---

type legacySnapshot struct {
	NextID   int64                     `json:"nextId"`
	Settings *Settings                 `json:"settings,omitempty"`
	Automod  *AutomodConfig            `json:"automod,omitempty"`
	RoomMeta map[string]legacyRoomMeta `json:"roomMeta,omitempty"`
	Rooms    map[string][]Message      `json:"rooms"`
	Users    []user                    `json:"users,omitempty"`
}

type legacyRoomMeta struct {
	Description string    `json:"description,omitempty"`
	Created     time.Time `json:"created"`
}

func (s *Store) migrateLegacy(h *Hub, u *Users) (bool, error) {
	if _, err := os.Stat(s.path("config.json")); err == nil {
		return false, nil // neues Format vorhanden
	}
	var legacyPath string
	for _, name := range []string{"gochat.json", "chat.json"} {
		if _, err := os.Stat(s.path(name)); err == nil {
			legacyPath = s.path(name)
			break
		}
	}
	if legacyPath == "" {
		return false, nil
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return false, err
	}
	var old legacySnapshot
	if err := json.Unmarshal(data, &old); err != nil {
		return false, corruptErr(legacyPath, err)
	}
	if old.Settings != nil {
		h.settings = old.Settings.normalize()
	}
	if old.Automod != nil {
		if _, err := h.automod.Set(*old.Automod); err != nil {
			return false, fmt.Errorf("Automod-Regeln in %s: %v", legacyPath, err)
		}
	}
	if u != nil {
		u.restore(old.Users)
	}
	names := map[string]bool{}
	for n := range old.RoomMeta {
		names[n] = true
	}
	for n := range old.Rooms {
		names[n] = true
	}
	for n := range names {
		name := NormRoom(n)
		if !ValidRoom(name) {
			continue
		}
		m := old.RoomMeta[n]
		h.putRoom(roomMeta{Name: name, Description: m.Description, Created: m.Created}, old.Rooms[n])
	}
	if old.NextID > h.nextID.Load() {
		h.nextID.Store(old.NextID)
	}
	if err := s.WriteAll(h, u); err != nil {
		return false, err
	}
	done := legacyPath + ".migrated"
	if err := os.Rename(legacyPath, done); err != nil {
		return false, err
	}
	log.Printf("Daten aus %s in einzelne Dateien übernommen. Die alte Datei liegt jetzt unter %s und kann gelöscht werden.", legacyPath, done)
	return true, nil
}
