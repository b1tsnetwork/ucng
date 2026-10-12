// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Die SQL-Tests laufen nur, wenn mit einem SQLite-Treiber gebaut wurde:
//
//	go test -tags sqlite ./...
func openSQLTest(t *testing.T, path string) (*Hub, *Users, *SQLStore) {
	t.Helper()
	driver, ok := sqlDrivers["sqlite"]
	if !ok {
		t.Skip("ohne SQLite-Treiber gebaut (go test -tags sqlite)")
	}
	s, err := OpenSQL("sqlite", driver, path, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h, u := NewHub(0), NewUsers(time.Hour, 1000)
	if err := s.Load(h, u); err != nil {
		t.Fatal(err)
	}
	h.EnablePersistence()
	return h, u, s
}

func TestSQLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db", "gochat.db")
	h, u, s := openSQLTest(t, path)
	h.EnsureDefaultRoom()
	h.CreateRoom("alt", "Beschreibung")
	h.CreateRoom("weg", "")
	h.SetSettings(Settings{Title: "Datenbank", AllowUserRooms: true})
	cr, _ := u.Claim("Mia", "")
	h.PostAs("alt", cr.ID, "Mia", "eins")
	h.Post("alt", "Bo", "zwei")
	h.Post("weg", "Bo", "tschüss")
	h.audit.Add(AuditEntry{Action: "test.eins"})
	if err := s.Flush(h, u); err != nil {
		t.Fatal(err)
	}

	// Umbenennen, Löschen, Nachricht ändern und endgültig löschen in einem Durchlauf
	h.UpdateRoom("alt", "neu", "Neue Beschreibung")
	h.Post("neu", "Bo", "drei")
	h.DeleteRoom("weg")
	msgs := adminHist(h, "neu")
	if _, err := h.PurgeMessage("neu", msgs[1].ID); err != nil {
		t.Fatal(err)
	}
	h.audit.Add(AuditEntry{Action: "test.zwei"})
	if err := s.Flush(h, u); err != nil {
		t.Fatal(err)
	}
	s.Close()

	h2, u2, _ := openSQLTest(t, path)
	if h2.Settings().Title != "Datenbank" {
		t.Fatalf("Einstellungen: %+v", h2.Settings())
	}
	got := adminHist(h2, "neu")
	if len(got) != 2 || got[0].Text != "eins" || got[1].Text != "drei" || got[0].Room != "neu" {
		t.Fatalf("Nachrichten: %+v", got)
	}
	names := map[string]bool{}
	for _, r := range h2.Rooms() {
		names[r.Name] = true
	}
	if !names["neu"] || names["alt"] || names["weg"] || !names["lobby"] {
		t.Fatalf("Kanäle: %v", names)
	}
	if me, ok := u2.Lookup(cr.Token); !ok || me.ID != cr.ID {
		t.Fatal("Namen nicht gespeichert")
	}
	if a := h2.audit.all(); len(a) != 2 || a[0].Action != "test.eins" || a[1].Action != "test.zwei" {
		t.Fatalf("Protokoll: %+v", a)
	}
	// neue Nachrichten-IDs setzen nach dem Laden fort
	m, _ := h2.Post("neu", "Bo", "vier")
	if m.ID <= got[1].ID {
		t.Fatalf("ID %d nicht größer als %d", m.ID, got[1].ID)
	}
}

func TestSQLImportsFiles(t *testing.T) {
	if _, ok := sqlDrivers["sqlite"]; !ok {
		t.Skip("ohne SQLite-Treiber gebaut (go test -tags sqlite)")
	}
	dir := t.TempDir()
	h, u, fs := openStore(t, dir)
	h.EnsureDefaultRoom()
	h.CreateRoom("support", "Hilfe")
	h.Post("support", "Ida", "aus den Dateien")
	h.SetSettings(Settings{Title: "Dateien", AllowUserRooms: true})
	cr, _ := u.Claim("Ida", "")
	h.audit.Add(AuditEntry{Action: "alt"})
	flush(t, h, u, fs)
	fs.Close()

	h2, u2, s := openSQLTest(t, filepath.Join(dir, "gochat.db"))
	if h2.Settings().Title != "Dateien" || len(adminHist(h2, "support")) != 1 {
		t.Fatal("Daten nicht übernommen")
	}
	if _, ok := u2.Lookup(cr.Token); !ok {
		t.Fatal("Namen nicht übernommen")
	}
	h2.Post("support", "Ida", "in der Datenbank")
	if err := s.Flush(h2, u2); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// zweiter Start: nichts wird erneut übernommen, die neue Nachricht ist da
	h3, _, _ := openSQLTest(t, filepath.Join(dir, "gochat.db"))
	if got := adminHist(h3, "support"); len(got) != 2 {
		t.Fatalf("nach Neustart: %d Nachrichten", len(got))
	}
	if a := h3.audit.all(); len(a) != 1 {
		t.Fatalf("Protokoll doppelt übernommen: %d", len(a))
	}
}

func TestSQLBackupExport(t *testing.T) {
	if _, ok := sqlDrivers["sqlite"]; !ok {
		t.Skip("ohne SQLite-Treiber gebaut (go test -tags sqlite)")
	}
	dir := t.TempDir()
	h, u, s := openSQLTest(t, filepath.Join(dir, "gochat.db"))
	h.EnsureDefaultRoom()
	h.Post("lobby", "Ida", "gesichert")
	os.MkdirAll(filepath.Join(dir, "media", "photos"), 0o700)
	os.WriteFile(filepath.Join(dir, "media", "photos", "a.jpg"), []byte("x"), 0o600)
	b := NewBackups(dir, filepath.Join(dir, "backups"), 3)
	b.store = s
	b.export = func(d string) error { return ExportFiles(d, h, u) }
	info, err := b.Create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, filepath.Join(dir, "backups", info.Name))
	for _, want := range []string{"config.json", "rooms/lobby.messages.jsonl", "media/photos/a.jpg"} {
		if !names[want] {
			t.Errorf("%s fehlt im Backup: %v", want, names)
		}
	}
	for n := range names {
		if filepath.Ext(n) == ".db" || filepath.Ext(n) == ".db-wal" {
			t.Errorf("Datenbankdatei im Backup: %s", n)
		}
	}
}

func TestExportFilesRoundTrip(t *testing.T) {
	h, u := NewHub(0), NewUsers(time.Hour, 100)
	h.EnsureDefaultRoom()
	h.Post("lobby", "Ida", "exportiert")
	cr, _ := u.Claim("Ida", "")
	h.audit.Add(AuditEntry{Action: "x"})
	dir := t.TempDir()
	if err := ExportFiles(dir, h, u); err != nil {
		t.Fatal(err)
	}
	h2, u2, _ := openStore(t, dir)
	if len(adminHist(h2, "lobby")) != 1 || len(h2.audit.all()) != 1 {
		t.Fatal("Export unvollständig")
	}
	if _, ok := u2.Lookup(cr.Token); !ok {
		t.Fatal("Namen fehlen im Export")
	}
}

func TestOpenBackendWithoutDriver(t *testing.T) {
	if _, ok := sqlDrivers["mysql"]; ok {
		t.Skip("mit MySQL gebaut")
	}
	if _, err := OpenBackend("mysql", t.TempDir(), "x"); err == nil {
		t.Fatal("Fehler erwartet")
	}
	if b, err := OpenBackend("files", t.TempDir(), ""); err != nil || b.Kind() != "files" {
		t.Fatal(err)
	}
	if redactDSN("gochat:geheim@tcp(db:3306)/gochat") != "gochat:***@tcp(db:3306)/gochat" {
		t.Fatal(redactDSN("gochat:geheim@tcp(db:3306)/gochat"))
	}
}
