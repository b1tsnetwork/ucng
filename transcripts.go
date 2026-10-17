// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Live-Transkription: Moderierende (oder der Besitzer eines temporären Kanals) schalten sie im
// Sprachkanal ein. Dann schneidet der Relay alles Gesagte an Sprechpausen in Abschnitte
// (internal/voicerelay/tap.go), der Chat-Server lässt sie von der eigenen Spracherkennung
// (STT_URL) verschriftlichen und schreibt jede Zeile mit Sprecher und Zeit ins Transkript. Der
// Sprecher steht sicher fest: Er kommt aus der Sprachverbindung, nicht aus dem Browser.
//
// Alle, die den Kanal lesen dürfen, sehen das Transkript live und können es danach
// herunterladen; löschen kann es der Admin. Mehrere Kanäle können gleichzeitig laufen.
// Gespeichert wird unter DATA_DIR/transcripts (Kopfdaten als .json, Zeilen als .jsonl).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ucng/internal/voicerelay"
)

var (
	ErrTranscriptOff     = errors.New("Live-Transkription geht hier nicht (eigene Spracherkennung, kein Ende-zu-Ende und „Untertitel“ beim Kanal nötig)")
	ErrTranscriptRunning = errors.New("Die Live-Transkription läuft hier schon")
	ErrTranscriptNone    = errors.New("Hier läuft gerade keine Live-Transkription")
	ErrTranscriptGone    = errors.New("Transkript nicht gefunden")
	ErrTranscriptRelay   = errors.New("Der Sprach-Server dieses Kanals ist zu alt für Untertitel und Transkription – bitte ucng-voice aktualisieren")
)

const (
	transcriptMaxLines = 50000
	transcriptMaxAge   = 12 * time.Hour  // läuft höchstens so lange am Stück
	transcriptIdle     = 2 * time.Minute // niemand mehr im Sprachkanal: beenden
	transcriptGrace    = time.Minute     // späte Abschnitte nach dem Ende zählen noch
)

// TranscriptLine: ein Beitrag.
type TranscriptLine struct {
	At   int64  `json:"at"` // Beginn (Unix-Millisekunden)
	Dur  int64  `json:"dur,omitempty"`
	User string `json:"u"`
	Name string `json:"n"`
	Text string `json:"x"`
}

// TranscriptInfo: Kopfdaten eines Transkripts.
type TranscriptInfo struct {
	ID      string `json:"id"`
	Room    string `json:"room"`
	By      string `json:"by,omitempty"` // Nutzer-ID
	ByName  string `json:"byName,omitempty"`
	Lang    string `json:"lang,omitempty"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended,omitempty"`
	Lines   int    `json:"lines"`
	Active  bool   `json:"active,omitempty"`
}

// TranscriptEvent: live an alle, die den Kanal offen haben.
type TranscriptEvent struct {
	Info *TranscriptInfo `json:"info,omitempty"` // gestartet/beendet
	ID   string          `json:"id,omitempty"`
	Line *TranscriptLine `json:"line,omitempty"` // neue Zeile
}

type transcriptStore struct {
	dir    string // leer = nur im Speicher
	mu     sync.Mutex
	items  map[string]*TranscriptInfo
	lines  map[string][]TranscriptLine // laufende (und ohne dir: alle)
	active map[string]string           // Kanal → ID
	empty  map[string]time.Time        // Kanal: seit wann niemand mehr drin ist
}

func newTranscriptStore(dataDir string) *transcriptStore {
	t := &transcriptStore{items: map[string]*TranscriptInfo{}, lines: map[string][]TranscriptLine{},
		active: map[string]string{}, empty: map[string]time.Time{}}
	if dataDir == "" {
		return t
	}
	t.dir = filepath.Join(dataDir, "transcripts")
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		log.Printf("Transkripte: %v – nur im Speicher", err)
		t.dir = ""
		return t
	}
	files, _ := filepath.Glob(filepath.Join(t.dir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		var info TranscriptInfo
		if err != nil || json.Unmarshal(b, &info) != nil || info.ID == "" {
			continue
		}
		if info.Active || info.Ended == 0 { // beim Beenden des Servers lief es noch
			info.Active = false
			info.Ended = max(info.Started, modTime(strings.TrimSuffix(f, ".json")+".jsonl"))
			t.items[info.ID] = &info
			t.saveInfoLocked(&info)
			continue
		}
		t.items[info.ID] = &info
	}
	return t
}

func modTime(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().UnixMilli()
	}
	return 0
}

func (t *transcriptStore) saveInfoLocked(info *TranscriptInfo) {
	if t.dir == "" {
		return
	}
	b, _ := json.MarshalIndent(info, "", " ")
	tmp := filepath.Join(t.dir, info.ID+".json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(t.dir, info.ID+".json"))
	}
}

func newTranscriptID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// activeFor: laufendes Transkript eines Kanals (Kopie) oder nil.
func (t *transcriptStore) activeFor(room string) *TranscriptInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id := t.active[room]; id != "" {
		cp := *t.items[id]
		return &cp
	}
	return nil
}

func (t *transcriptStore) start(room string, me Identity, lang string) (TranscriptInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active[room] != "" {
		return TranscriptInfo{}, ErrTranscriptRunning
	}
	info := &TranscriptInfo{ID: newTranscriptID(), Room: room, By: me.ID, ByName: me.Name, Lang: lang, Started: time.Now().UnixMilli(), Active: true}
	t.items[info.ID] = info
	t.active[room] = info.ID
	t.lines[info.ID] = nil
	delete(t.empty, room)
	t.saveInfoLocked(info)
	return *info, nil
}

func (t *transcriptStore) stop(room string) (TranscriptInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.active[room]
	if id == "" {
		return TranscriptInfo{}, false
	}
	delete(t.active, room)
	delete(t.empty, room)
	info := t.items[id]
	info.Active = false
	info.Ended = time.Now().UnixMilli()
	t.saveInfoLocked(info)
	if t.dir != "" { // Zeilen stehen in der Datei; späte Abschnitte schreibt add direkt dorthin
		time.AfterFunc(transcriptGrace+5*time.Second, func() {
			t.mu.Lock()
			if t.active[room] != id {
				delete(t.lines, id)
			}
			t.mu.Unlock()
		})
	}
	return *info, true
}

// target: Transkript, zu dem ein Abschnitt (Beginn at, Länge dur) gehört – das laufende (auch
// wenn der Satz kurz vor dem Start begann) oder eines, das gerade erst beendet wurde.
func (t *transcriptStore) target(room string, at time.Time, dur time.Duration) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := at.UnixMilli()
	if id := t.active[room]; id != "" && at.Add(dur).UnixMilli() > t.items[id].Started {
		return id
	}
	now := time.Now().UnixMilli()
	for id, info := range t.items {
		if info.Room == room && info.Ended > 0 && ms >= info.Started-2000 && ms < info.Ended && now-info.Ended < transcriptGrace.Milliseconds() {
			return id
		}
	}
	return ""
}

func (t *transcriptStore) add(id string, l TranscriptLine) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.items[id]
	if info == nil || info.Lines >= transcriptMaxLines {
		return false
	}
	info.Lines++
	if _, ok := t.lines[id]; ok || t.dir == "" {
		t.lines[id] = append(t.lines[id], l)
	}
	if t.dir != "" {
		if f, err := os.OpenFile(filepath.Join(t.dir, id+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			b, _ := json.Marshal(l)
			f.Write(append(b, '\n'))
			f.Close()
		}
		if info.Lines%50 == 0 || !info.Active {
			t.saveInfoLocked(info)
		}
	}
	return true
}

func (t *transcriptStore) get(id string) (TranscriptInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.items[id]
	if info == nil {
		return TranscriptInfo{}, false
	}
	return *info, true
}

// read: alle Zeilen, nach Beginn sortiert (Abschnitte werden parallel verschriftlicht).
func (t *transcriptStore) read(id string) []TranscriptLine {
	t.mu.Lock()
	mem, ok := t.lines[id]
	out := append([]TranscriptLine(nil), mem...)
	dir := t.dir
	t.mu.Unlock()
	if dir != "" { // die Datei hat alles, auch späte Abschnitte
		_ = ok
		out = nil
		if f, err := os.Open(filepath.Join(dir, id+".jsonl")); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				var l TranscriptLine
				if json.Unmarshal(sc.Bytes(), &l) == nil {
					out = append(out, l)
				}
			}
			f.Close()
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

func (t *transcriptStore) list(room string) []TranscriptInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []TranscriptInfo
	for _, info := range t.items {
		if room == "" || info.Room == room {
			out = append(out, *info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out
}

func (t *transcriptStore) remove(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.items[id]
	if info == nil {
		return false
	}
	if t.active[info.Room] == id {
		delete(t.active, info.Room)
	}
	delete(t.items, id)
	delete(t.lines, id)
	if t.dir != "" {
		os.Remove(filepath.Join(t.dir, id+".json"))
		os.Remove(filepath.Join(t.dir, id+".jsonl"))
	}
	return true
}

// --- Sprach-Manager ---

// tapMode: Was soll der Relay eines Kanals mitschneiden?
func (v *voiceMgr) tapMode(room string, cfg *VoiceConfig) int {
	if v.stt == nil || cfg == nil || cfg.E2EE {
		return voicerelay.TapOff
	}
	if v.tr.activeFor(room) != nil {
		return voicerelay.TapTranscript
	}
	if cfg.Captions {
		return voicerelay.TapCaptions
	}
	return voicerelay.TapOff
}

// pushTap sagt dem Relay des Kanals den aktuellen Stand.
func (v *voiceMgr) pushTap(room string) {
	info, ok := v.s.hub.roomInfo(room)
	if !ok {
		return
	}
	v.control(room, "", "tap", false, fmt.Sprint(v.tapMode(room, info.Voice)))
}

// relayTaps: Kann der Relay (eingebaut oder Agent) mitschneiden?
func (v *voiceMgr) relayTaps(id string) bool {
	if id == "" || id == builtinRelay {
		return true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	a := v.agents[id]
	if a == nil {
		return false
	}
	for _, f := range a.Stats.Features {
		if f == "tap" {
			return true
		}
	}
	return false
}

// transcriptAllowed: Darf in diesem Kanal transkribiert werden? (Kanal mit „Untertitel“ oder
// temporärer Kanal; eigene Spracherkennung; nicht Ende-zu-Ende)
func (v *voiceMgr) transcriptAllowed(acc voiceRoomAccess) bool {
	return v.stt != nil && acc.Cfg != nil && !acc.Cfg.E2EE && (acc.Cfg.Captions || acc.Owner != "")
}

func (v *voiceMgr) startTranscript(room string, me Identity, lang string) (TranscriptInfo, error) {
	acc := v.s.hub.voiceAccessFull(room, v.s.voiceWhoOf(me, "").access())
	if !acc.Exists || acc.Cfg == nil {
		return TranscriptInfo{}, ErrNoVoice
	}
	if !v.transcriptAllowed(acc) {
		return TranscriptInfo{}, ErrTranscriptOff
	}
	v.mu.Lock()
	relay := ""
	if st := v.rooms[room]; st != nil && len(st.peers) > 0 {
		relay = st.relay
	}
	v.mu.Unlock()
	if !v.relayTaps(relay) {
		return TranscriptInfo{}, ErrTranscriptRelay
	}
	if len(lang) > 8 || strings.ContainsAny(lang, " /&?") {
		lang = ""
	}
	info, err := v.tr.start(room, me, lang)
	if err != nil {
		return info, err
	}
	v.pushTap(room)
	v.s.hub.SendToReaders(room, Event{Type: "transcript", Room: room, Transcript: &TranscriptEvent{Info: &info}})
	v.notice(room, fmt.Sprintf("🔴 **Live-Transkription gestartet** von %s – alles, was im Sprachkanal gesagt wird, wird mit Namen mitgeschrieben. Alle, die diesen Kanal lesen, sehen das Transkript.", me.Name))
	return info, nil
}

func (v *voiceMgr) stopTranscript(room, reason string) (TranscriptInfo, bool) {
	info, ok := v.tr.stop(room)
	if !ok {
		return info, false
	}
	v.pushTap(room)
	v.s.hub.SendToReaders(room, Event{Type: "transcript", Room: room, Transcript: &TranscriptEvent{Info: &info}})
	mins := max(1, (info.Ended-info.Started+30000)/60000)
	text := fmt.Sprintf("📝 **Live-Transkription beendet** (%d Min., %d Beiträge). Herunterladen: 📝 oben im Kanal → Transkripte.", mins, info.Lines)
	if reason != "" {
		text += " " + reason
	}
	v.notice(room, text)
	return info, true
}

// notice: Hinweis des Systems im Kanal (wie der Befehls-Bot).
func (v *voiceMgr) notice(room, text string) {
	bot := Poster{ID: CmdBotID, Name: v.s.hub.Commands().BotName(), Bot: true, NoAutomod: true, System: true}
	if _, err := v.s.hub.Send(room, bot, text, 0); err != nil {
		log.Printf("Transkript: Hinweis in #%s: %v", room, err)
	}
}

// transcriptTick (janitor): leere Kanäle und zu lange Läufe beenden, alte Transkripte löschen.
func (v *voiceMgr) transcriptTick(now time.Time) {
	t := v.tr
	t.mu.Lock()
	rooms := make([]string, 0, len(t.active))
	for room := range t.active {
		rooms = append(rooms, room)
	}
	t.mu.Unlock()
	for _, room := range rooms {
		info := t.activeFor(room)
		if info == nil {
			continue
		}
		people := 0
		for _, p := range v.Peers(room) {
			if p.Music == 0 {
				people++
			}
		}
		t.mu.Lock()
		if people > 0 {
			delete(t.empty, room)
		} else if _, ok := t.empty[room]; !ok {
			t.empty[room] = now
		}
		idle := people == 0 && now.Sub(t.empty[room]) >= transcriptIdle
		t.mu.Unlock()
		switch {
		case idle:
			v.stopTranscript(room, "(Niemand mehr im Sprachkanal.)")
		case now.Sub(time.UnixMilli(info.Started)) >= transcriptMaxAge:
			v.stopTranscript(room, "(Höchstdauer erreicht.)")
		}
	}
	if days := v.s.cfg.TranscriptKeepDays; days > 0 {
		cut := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		for _, info := range t.list("") {
			if !info.Active && info.Ended > 0 && info.Ended < cut {
				t.remove(info.ID)
			}
		}
	}
}

// --- Abschnitte → Spracherkennung ---

type sttJob struct {
	seg voicerelay.Segment
	tid string // Transkript ("" = nur Untertitel)
}

// onSegment: ein Abschnitt vom Relay (eingebaut direkt, Agenten über /api/voice/agents/segment).
func (v *voiceMgr) onSegment(seg voicerelay.Segment) {
	if v.stt == nil || len(seg.Ogg) == 0 {
		return
	}
	info, ok := v.s.hub.roomInfo(seg.Channel)
	if !ok || info.Voice == nil || info.Voice.E2EE {
		return
	}
	job := sttJob{seg: seg, tid: v.tr.target(seg.Channel, seg.Start, seg.Dur)}
	if job.tid == "" {
		if !info.Voice.Captions || !v.stt.rl.Allow(seg.User) {
			return
		}
		if len(v.stt.queue) > cap(v.stt.sem)*3 { // Untertitel: lieber auslassen als verspätet
			return
		}
	}
	select {
	case v.stt.queue <- job:
	default:
		v.stt.warn("Warteschlange voll – Abschnitt von %s in #%s ausgelassen (STT_CONCURRENCY erhöhen?)", seg.Name, seg.Channel)
	}
}

// sttWorker verschriftlicht Abschnitte aus der Warteschlange.
func (v *voiceMgr) sttWorker() {
	for {
		var job sttJob
		select {
		case <-v.stop:
			return
		case job = <-v.stt.queue:
		}
		seg := job.seg
		lang := seg.Lang
		if job.tid != "" {
			if info, ok := v.tr.get(job.tid); ok && info.Lang != "" {
				lang = info.Lang
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		text, err := v.stt.transcribeFile(ctx, seg.Ogg, "audio.ogg", "audio/ogg", lang)
		cancel()
		if err != nil || text == "" {
			continue
		}
		v.control(seg.Channel, seg.User, "cap", true, text) // Untertitel für alle im Sprachkanal
		if job.tid == "" {
			continue
		}
		l := TranscriptLine{At: seg.Start.UnixMilli(), Dur: seg.Dur.Milliseconds(), User: seg.User, Name: seg.Name, Text: text}
		if v.tr.add(job.tid, l) {
			v.s.hub.SendToReaders(seg.Channel, Event{Type: "transcript", Room: seg.Channel, Transcript: &TranscriptEvent{ID: job.tid, Line: &l}})
		}
	}
}

// --- HTTP ---

// POST /api/voice/{room}/transcript {"on":true,"lang":"de"} – starten bzw. beenden (Moderierende).
func (s *Server) handleVoiceTranscript(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, ErrNoSession.Error())
		return
	}
	room := NormRoom(r.PathValue("room"))
	if !v.mayHost(room, me) {
		fail(w, ErrNoPerm)
		return
	}
	var in struct {
		On   bool   `json:"on"`
		Lang string `json:"lang"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	actor, display := chatActor(me)
	if !in.On {
		info, ok := v.stopTranscript(room, "")
		if !ok {
			writeErr(w, http.StatusNotFound, ErrTranscriptNone.Error())
			return
		}
		s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice.transcript_stop", Room: room})
		writeJSON(w, http.StatusOK, info)
		return
	}
	info, err := v.startTranscript(room, me, in.Lang)
	if err != nil {
		switch {
		case errors.Is(err, ErrTranscriptRunning):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrTranscriptOff), errors.Is(err, ErrTranscriptRelay):
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			voiceFail(w, err)
		}
		return
	}
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice.transcript_start", Room: room})
	writeJSON(w, http.StatusOK, info)
}

// GET /api/rooms/{room}/transcripts – laufendes (samt Zeilen) und frühere Transkripte des Kanals.
func (s *Server) handleRoomTranscripts(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []TranscriptInfo{}})
		return
	}
	me, _ := s.users.Lookup(bearer(r))
	room := NormRoom(r.PathValue("room"))
	acc := s.hub.voiceAccessFull(room, s.voiceWhoOf(me, "").access())
	if !acc.Exists || !acc.Read || acc.Locked {
		fail(w, ErrNoPerm)
		return
	}
	out := map[string]any{"items": v.tr.list(room), "allowed": v.transcriptAllowed(acc), "canStart": me.ID != "" && v.transcriptAllowed(acc) && v.mayHost(room, me)}
	if a := v.tr.activeFor(room); a != nil {
		out["active"] = a
		out["lines"] = v.tr.read(a.ID)
	}
	if out["items"] == nil {
		out["items"] = []TranscriptInfo{}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/transcripts/{id}?format=txt|json – herunterladen (wer den Kanal lesen darf oder
// das Transkript gestartet hat).
func (s *Server) handleTranscriptGet(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	info, ok := v.tr.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	me, _ := s.users.Lookup(bearer(r))
	acc := s.hub.voiceAccessFull(info.Room, s.voiceWhoOf(me, "").access())
	if !(acc.Exists && acc.Read && !acc.Locked) && !(me.ID != "" && me.ID == info.By) {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	writeTranscript(w, r, v.tr, info)
}

func writeTranscript(w http.ResponseWriter, r *http.Request, t *transcriptStore, info TranscriptInfo) {
	lines := t.read(info.ID)
	stamp := time.UnixMilli(info.Started).Format("2006-01-02-1504")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="transkript-%s-%s.json"`, info.Room, stamp))
		writeJSON(w, http.StatusOK, map[string]any{"info": info, "lines": lines})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="transkript-%s-%s.txt"`, info.Room, stamp))
	w.Write([]byte(transcriptText(info, lines)))
}

// transcriptText: lesbare Fassung mit Uhrzeit und Sprecher.
func transcriptText(info TranscriptInfo, lines []TranscriptLine) string {
	var b strings.Builder
	start := time.UnixMilli(info.Started)
	fmt.Fprintf(&b, "Live-Transkript #%s\n", info.Room)
	fmt.Fprintf(&b, "Beginn: %s", start.Format("02.01.2006 15:04"))
	if info.Ended > 0 {
		fmt.Fprintf(&b, " · Ende: %s", time.UnixMilli(info.Ended).Format("15:04"))
	}
	if info.ByName != "" {
		fmt.Fprintf(&b, " · gestartet von %s", info.ByName)
	}
	fmt.Fprintf(&b, "\n%d Beiträge (automatisch verschriftlicht – kann Fehler enthalten)\n\n", len(lines))
	last := ""
	for _, l := range lines {
		ts := time.UnixMilli(l.At).Format("15:04:05")
		if l.Name == last {
			fmt.Fprintf(&b, "[%s]   %s\n", ts, l.Text)
		} else {
			fmt.Fprintf(&b, "[%s] %s: %s\n", ts, l.Name, l.Text)
		}
		last = l.Name
	}
	return b.String()
}

// GET /api/admin/transcripts – alle Transkripte (Admin).
func (s *Server) handleAdminTranscripts(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []TranscriptInfo{}, "stt": false})
		return
	}
	items := s.voice.tr.list("")
	if items == nil {
		items = []TranscriptInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "stt": s.voice.stt != nil, "stored": s.voice.tr.dir != "", "keepDays": s.cfg.TranscriptKeepDays})
}

// GET /api/admin/transcripts/{id}?format=txt|json
func (s *Server) handleAdminTranscriptGet(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	info, ok := s.voice.tr.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	writeTranscript(w, r, s.voice.tr, info)
}

// DELETE /api/admin/transcripts/{id} – löschen (beendet ein laufendes vorher).
func (s *Server) handleAdminTranscriptDelete(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	info, ok := v.tr.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, ErrTranscriptGone.Error())
		return
	}
	if info.Active {
		v.stopTranscript(info.Room, "(Vom Admin beendet und gelöscht.)")
	}
	v.tr.remove(info.ID)
	v.s.hub.SendToReaders(info.Room, Event{Type: "transcript", Room: info.Room, Transcript: &TranscriptEvent{ID: info.ID}})
	s.log(r, AuditEntry{Action: "transcript.delete", Room: info.Room, Detail: time.UnixMilli(info.Started).Format("02.01.2006 15:04")})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /api/voice/agents/segment?channel=…&user=…&name=…&lang=…&mode=…&start=…&dur=… – ein
// Sprach-Abschnitt von einem Agenten (Ogg-Opus als Inhalt).
func (s *Server) handleVoiceAgentSegment(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	if !v.agentAuth(r) {
		writeErr(w, http.StatusUnauthorized, "Ungültiger Agenten-Schlüssel")
		return
	}
	q := r.URL.Query()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil || len(body) < 4 || string(body[:4]) != "OggS" {
		writeErr(w, http.StatusBadRequest, "Ogg-Opus erwartet")
		return
	}
	start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
	dur, _ := strconv.ParseInt(q.Get("dur"), 10, 64)
	mode, _ := strconv.Atoi(q.Get("mode"))
	v.onSegment(voicerelay.Segment{Channel: NormRoom(q.Get("channel")), User: q.Get("user"), Name: q.Get("name"), Lang: q.Get("lang"),
		Mode: mode, Start: time.UnixMilli(start), Dur: time.Duration(dur) * time.Millisecond, Ogg: body})
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
