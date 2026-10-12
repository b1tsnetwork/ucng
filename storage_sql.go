// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SQLStore speichert in SQLite oder MySQL/MariaDB. Tabellen:
//
//	gochat_docs      name → JSON   (config.json, automod.json, users.json, members.json …)
//	gochat_rooms     name → JSON   (Kanal-Einstellungen)
//	gochat_messages  (room, id) → JSON
//	gochat_audit     seq → JSON    (Protokoll)
//
// Jeder Speichervorgang läuft in einer Transaktion: Entweder kommt alles an oder nichts –
// dann wird es beim nächsten Durchlauf erneut versucht.
type SQLStore struct {
	db      *sql.DB
	dialect string // "sqlite" oder "mysql"
	dataDir string // für die einmalige Übernahme aus dem Dateiformat
	dsn     string
	mu      sync.Mutex
}

func OpenSQL(dialect, driver, dsn, dataDir string) (*SQLStore, error) {
	if dialect == "sqlite" && !strings.HasPrefix(dsn, "file:") && !strings.Contains(dsn, ":memory:") {
		if dir := filepath.Dir(dsn); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if dialect == "sqlite" {
		// Eine Verbindung: SQLite erlaubt ohnehin nur einen Schreiber, und PRAGMAs gelten je Verbindung.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
	} else {
		db.SetMaxOpenConns(4)
		db.SetConnMaxLifetime(5 * time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("Datenbank %s nicht erreichbar: %v", redactDSN(dsn), err)
	}
	s := &SQLStore{db: db, dialect: dialect, dataDir: dataDir, dsn: dsn}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("Tabellen konnten nicht angelegt werden: %v", err)
	}
	return s, nil
}

func (s *SQLStore) Kind() string { return s.dialect }
func (s *SQLStore) Lock()        { s.mu.Lock() }
func (s *SQLStore) Unlock()      { s.mu.Unlock() }
func (s *SQLStore) Close()       { s.mu.Lock(); s.db.Close(); s.mu.Unlock() }

func (s *SQLStore) migrate(ctx context.Context) error {
	var stmts []string
	if s.dialect == "mysql" {
		const tail = " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
		stmts = []string{
			"CREATE TABLE IF NOT EXISTS gochat_docs (name VARCHAR(191) NOT NULL PRIMARY KEY, data LONGTEXT NOT NULL)" + tail,
			"CREATE TABLE IF NOT EXISTS gochat_rooms (name VARCHAR(191) NOT NULL PRIMARY KEY, meta LONGTEXT NOT NULL)" + tail,
			"CREATE TABLE IF NOT EXISTS gochat_messages (room VARCHAR(191) NOT NULL, id BIGINT NOT NULL, data LONGTEXT NOT NULL, PRIMARY KEY (room, id))" + tail,
			"CREATE TABLE IF NOT EXISTS gochat_audit (seq BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, data LONGTEXT NOT NULL)" + tail,
		}
	} else {
		stmts = []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA busy_timeout=5000",
			"PRAGMA synchronous=NORMAL",
			"PRAGMA foreign_keys=ON",
			"CREATE TABLE IF NOT EXISTS gochat_docs (name TEXT NOT NULL PRIMARY KEY, data TEXT NOT NULL)",
			"CREATE TABLE IF NOT EXISTS gochat_rooms (name TEXT NOT NULL PRIMARY KEY, meta TEXT NOT NULL)",
			"CREATE TABLE IF NOT EXISTS gochat_messages (room TEXT NOT NULL, id INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY (room, id))",
			"CREATE TABLE IF NOT EXISTS gochat_audit (seq INTEGER PRIMARY KEY AUTOINCREMENT, data TEXT NOT NULL)",
		}
	}
	for _, q := range stmts {
		if strings.HasPrefix(q, "PRAGMA") {
			// Ergebnis-Zeilen ignorieren; manche Treiber liefern bei PRAGMA eine Zeile zurück.
			rows, err := s.db.QueryContext(ctx, q)
			if err != nil {
				return err
			}
			rows.Close()
			continue
		}
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLStore) upsert(table, key, val string) string {
	if s.dialect == "mysql" {
		return fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES (?, ?) ON DUPLICATE KEY UPDATE %s = VALUES(%s)", table, key, val, val, val)
	}
	return fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES (?, ?) ON CONFLICT(%s) DO UPDATE SET %s = excluded.%s", table, key, val, key, val, val)
}

func (s *SQLStore) upsertMsg() string {
	if s.dialect == "mysql" {
		return "INSERT INTO gochat_messages (room, id, data) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE data = VALUES(data)"
	}
	return "INSERT INTO gochat_messages (room, id, data) VALUES (?, ?, ?) ON CONFLICT(room, id) DO UPDATE SET data = excluded.data"
}

// sqlTx bündelt die vorbereiteten Anweisungen eines Speichervorgangs.
type sqlTx struct {
	s  *SQLStore
	tx *sql.Tx
	st map[string]*sql.Stmt
}

func (t *sqlTx) exec(q string, args ...any) error {
	st, ok := t.st[q]
	if !ok {
		var err error
		if st, err = t.tx.Prepare(q); err != nil {
			return err
		}
		t.st[q] = st
	}
	_, err := st.Exec(args...)
	return err
}

func (t *sqlTx) close() {
	for _, st := range t.st {
		st.Close()
	}
}

func (t *sqlTx) putDoc(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return t.exec(t.s.upsert("gochat_docs", "name", "data"), name, string(data))
}

func (t *sqlTx) putMeta(m roomMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return t.exec(t.s.upsert("gochat_rooms", "name", "meta"), m.Name, string(data))
}

func (t *sqlTx) putMsg(room string, m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return t.exec(t.s.upsertMsg(), room, m.ID, string(data))
}

func (t *sqlTx) rewriteMsgs(room string, msgs []Message) error {
	if err := t.exec("DELETE FROM gochat_messages WHERE room = ?", room); err != nil {
		return err
	}
	for _, m := range msgs {
		if err := t.putMsg(room, m); err != nil {
			return err
		}
	}
	return nil
}

func (t *sqlTx) putAudit(e AuditEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return t.exec("INSERT INTO gochat_audit (data) VALUES (?)", string(data))
}

func (s *SQLStore) begin() (*sqlTx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &sqlTx{s: s, tx: tx, st: map[string]*sql.Stmt{}}, nil
}

// Flush schreibt alle vorgemerkten Änderungen in einer Transaktion.
func (s *SQLStore) Flush(h *Hub, u *Users) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := h.takeOps()
	usersDirty := u != nil && u.dirty.Swap(false)
	if len(ops) == 0 && !usersDirty {
		return nil
	}
	fail := func(err error) error {
		h.requeue(ops)
		if usersDirty {
			u.markDirty()
		}
		return err
	}
	t, err := s.begin()
	if err != nil {
		return fail(err)
	}
	defer t.close()
	if err := s.applyOps(t, h, ops); err != nil {
		t.tx.Rollback()
		return fail(err)
	}
	if usersDirty {
		if err := t.putDoc("users.json", u.export()); err != nil {
			t.tx.Rollback()
			return fail(err)
		}
	}
	if err := t.tx.Commit(); err != nil {
		return fail(err)
	}
	return nil
}

func (s *SQLStore) applyOps(t *sqlTx, h *Hub, ops []storeOp) error {
	auditRewrite := false
	for _, op := range ops {
		if op.kind == opAuditRewrite {
			auditRewrite = true
		}
	}
	docs := map[string]bool{}
	for _, op := range ops {
		switch op.kind {
		case opDoc:
			docs[op.doc] = true
		case opRoomMeta:
			if meta, ok := h.metaOf(op.room); ok {
				if err := t.putMeta(meta); err != nil {
					return err
				}
			}
		case opMessage:
			if err := t.putMsg(op.room, op.msg); err != nil {
				return err
			}
		case opRoomRewrite:
			if msgs, ok := h.roomMessages(op.room); ok {
				if err := t.rewriteMsgs(op.room, msgs); err != nil {
					return err
				}
			}
		case opRoomRename:
			if err := t.exec("DELETE FROM gochat_rooms WHERE name = ?", op.room); err != nil {
				return err
			}
			if err := t.exec("DELETE FROM gochat_messages WHERE room = ?", op.room); err != nil {
				return err
			}
			if meta, ok := h.metaOf(op.newName); ok {
				if err := t.putMeta(meta); err != nil {
					return err
				}
				if msgs, ok := h.roomMessages(op.newName); ok {
					if err := t.rewriteMsgs(op.newName, msgs); err != nil {
						return err
					}
				}
			}
		case opRoomDelete:
			if err := t.exec("DELETE FROM gochat_rooms WHERE name = ?", op.room); err != nil {
				return err
			}
			if err := t.exec("DELETE FROM gochat_messages WHERE room = ?", op.room); err != nil {
				return err
			}
		case opAudit:
			if auditRewrite {
				continue
			}
			if err := t.putAudit(op.audit); err != nil {
				return err
			}
		}
	}
	if auditRewrite {
		if err := t.exec("DELETE FROM gochat_audit"); err != nil {
			return err
		}
		for _, e := range h.audit.all() {
			if err := t.putAudit(e); err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(docs))
	for d := range docs {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		spec, ok := h.docs[d]
		if !ok {
			continue
		}
		if err := t.putDoc(d, spec.save()); err != nil {
			return err
		}
	}
	return nil
}

// WriteAll schreibt den kompletten Zustand (Übernahme aus dem Dateiformat).
func (s *SQLStore) WriteAll(h *Hub, u *Users) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.begin()
	if err != nil {
		return err
	}
	defer t.close()
	err = func() error {
		for _, q := range []string{"DELETE FROM gochat_docs", "DELETE FROM gochat_rooms", "DELETE FROM gochat_messages", "DELETE FROM gochat_audit"} {
			if err := t.exec(q); err != nil {
				return err
			}
		}
		for _, d := range h.order {
			if err := t.putDoc(d, h.docs[d].save()); err != nil {
				return err
			}
		}
		if u != nil {
			if err := t.putDoc("users.json", u.export()); err != nil {
				return err
			}
		}
		for name, r := range h.snapshotRooms() {
			if err := t.putMeta(r.meta); err != nil {
				return err
			}
			if err := t.rewriteMsgs(name, r.msgs); err != nil {
				return err
			}
		}
		for _, e := range h.audit.all() {
			if err := t.putAudit(e); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		t.tx.Rollback()
		return err
	}
	return t.tx.Commit()
}

// Load liest alles aus der Datenbank. Ist sie leer und liegen im Datenordner noch Daten im
// Dateiformat, werden diese einmalig übernommen (die Dateien bleiben unverändert liegen).
func (s *SQLStore) Load(h *Hub, u *Users) error {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM gochat_docs").Scan(&n); err != nil {
		return err
	}
	if n == 0 && s.dataDir != "" {
		if _, err := os.Stat(filepath.Join(s.dataDir, "config.json")); err == nil {
			log.Printf("Datenbank ist leer – übernehme Daten aus %s …", s.dataDir)
			fs := NewStore(s.dataDir)
			if err := fs.Load(h, u); err != nil {
				return err
			}
			fs.Close()
			if err := s.WriteAll(h, u); err != nil {
				return fmt.Errorf("Übernahme in die Datenbank fehlgeschlagen: %v", err)
			}
			log.Printf("Daten in die Datenbank übernommen. Die Dateien in %s werden nicht mehr verwendet (außer media/).", s.dataDir)
			return nil
		}
	}

	docs := map[string][]byte{}
	rows, err := s.db.Query("SELECT name, data FROM gochat_docs")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			rows.Close()
			return err
		}
		docs[name] = []byte(data)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range h.order {
		data, ok := docs[d]
		if !ok {
			continue
		}
		if err := h.docs[d].load(data); err != nil {
			return corruptErr("Datenbank: "+d, err)
		}
	}
	if data, ok := docs["users.json"]; ok && u != nil {
		var list []user
		if err := json.Unmarshal(data, &list); err != nil {
			return corruptErr("Datenbank: users.json", err)
		}
		u.restore(list)
	}

	var audit []AuditEntry
	rows, err = s.db.Query("SELECT data FROM gochat_audit ORDER BY seq")
	if err != nil {
		return err
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		var e AuditEntry
		if err := json.Unmarshal([]byte(data), &e); err == nil {
			audit = append(audit, e)
		}
	}
	rows.Close()
	h.audit.restore(audit)

	metas := map[string]roomMeta{}
	rows, err = s.db.Query("SELECT name, meta FROM gochat_rooms")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			rows.Close()
			return err
		}
		m := roomMeta{Name: name}
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			rows.Close()
			return corruptErr("Datenbank: Kanal "+name, err)
		}
		m.Name = name
		metas[name] = m
	}
	rows.Close()

	msgs := map[string][]Message{}
	rows, err = s.db.Query("SELECT room, data FROM gochat_messages ORDER BY room, id")
	if err != nil {
		return err
	}
	for rows.Next() {
		var room, data string
		if err := rows.Scan(&room, &data); err != nil {
			rows.Close()
			return err
		}
		var m Message
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			rows.Close()
			return corruptErr("Datenbank: Nachricht in "+room, err)
		}
		m.Room = room
		msgs[room] = append(msgs[room], m)
		if _, ok := metas[room]; !ok {
			metas[room] = roomMeta{Name: room}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for name, meta := range metas {
		if !ValidRoom(name) {
			log.Printf("Überspringe unbekannten Kanal %q in der Datenbank", name)
			continue
		}
		h.putRoom(meta, msgs[name])
	}
	return nil
}

// ExportFiles schreibt den aktuellen Stand im Dateiformat in einen Ordner (für Backups).
func ExportFiles(dir string, h *Hub, u *Users) error {
	fs := NewStore(dir)
	defer fs.Close()
	if err := os.MkdirAll(fs.roomsDir(), 0o700); err != nil {
		return err
	}
	if err := fs.WriteAll(h, u); err != nil {
		return err
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for _, e := range h.audit.all() {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return writeFileAtomic(fs.path("audit.jsonl"), []byte(buf.String()))
}
