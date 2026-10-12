// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestSaverWritesPromptly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "data") // Ordner wird angelegt
	h, u, s := openStore(t, dir)
	sv := newSaver(s, h, u, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sv.run(ctx); close(done) }()

	h.CreateRoom("team", "Interner Kanal")
	h.SetSettings(Settings{Title: "Firma", AllowUserRooms: false})
	cr, _ := u.Claim("Mia", "")
	h.PostAs("team", cr.ID, "Mia", "Hallo")

	deadline := time.Now().Add(time.Second)
	for {
		h2, u2 := NewHub(0), NewUsers(time.Hour, 10)
		if err := NewStore(dir).Load(h2, u2); err == nil {
			// Nachrichten und Namen landen in getrennten Dateien – erst prüfen, wenn beides da ist.
			if me, ok := u2.Lookup(cr.Token); ok && len(adminHist(h2, "team")) == 1 && h2.Settings().Title == "Firma" {
				if me.ID != cr.ID {
					t.Fatal("Name falsch gespeichert")
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("Änderungen wurden nicht innerhalb einer Sekunde gespeichert")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Beim Beenden wird ein letztes Mal gespeichert.
	h.Post("team", "Mia", "kurz vor Schluss")
	cancel()
	<-done
	h3 := NewHub(0)
	NewStore(dir).Load(h3, nil)
	if msgs := adminHist(h3, "team"); len(msgs) != 2 {
		t.Fatalf("letzte Nachricht beim Beenden verloren: %d", len(msgs))
	}
}

func TestFileLayout(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.EnsureDefaultRoom()
	h.CreateRoom("support", "Hilfe")
	h.CreateRoom("über-uns", "")
	h.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"arsch*"}})
	h.SetSettings(Settings{Title: "Test", AllowUserRooms: true})
	u.Claim("Ida", "")
	h.Post("support", "Ida", "eins")
	h.Post("support", "Ida", "zwei")
	flush(t, h, u, s)

	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	want := []string{
		"automod.json", "config.json",
		filepath.Join("rooms", "lobby.json"),
		filepath.Join("rooms", "support.json"),
		filepath.Join("rooms", "support.messages.jsonl"),
		filepath.Join("rooms", "über-uns.json"),
		"users.json",
	}
	sort.Strings(want)
	if len(files) != len(want) {
		t.Fatalf("Dateien:\n%v\nerwartet:\n%v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("Dateien:\n%v\nerwartet:\n%v", files, want)
		}
	}
	lines, _ := os.ReadFile(filepath.Join(dir, "rooms", "support.messages.jsonl"))
	if n := countLines(lines); n != 2 {
		t.Fatalf("erwartet 2 Zeilen, bekam %d", n)
	}
}

func TestRenameAndDeleteRoomFiles(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.CreateRoom("alt", "Beschreibung")
	h.Post("alt", "A", "Hallo")
	h.CreateRoom("weg", "")
	h.Post("weg", "A", "tschüss")
	flush(t, h, u, s)

	h.UpdateRoom("alt", "neu", "Neue Beschreibung")
	h.Post("neu", "A", "nach dem Umbenennen") // im selben Speichervorgang
	h.DeleteRoom("weg")
	flush(t, h, u, s)

	for _, gone := range []string{"alt.json", "alt.messages.jsonl", "weg.json", "weg.messages.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, "rooms", gone)); !os.IsNotExist(err) {
			t.Errorf("%s sollte nicht mehr existieren", gone)
		}
	}
	s.Close()
	h2, _, _ := openStore(t, dir)
	msgs := hist(h2, "neu")
	if len(msgs) != 2 || msgs[1].Text != "nach dem Umbenennen" || msgs[0].Room != "neu" {
		t.Fatalf("umbenannter Kanal: %+v", msgs)
	}
	if r := h2.Rooms(); len(r) != 1 || r[0].Description != "Neue Beschreibung" {
		t.Fatalf("Kanäle: %+v", r)
	}
}

func TestModerationAppendsAndCompacts(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	var ids []int64
	for i := 0; i < 1500; i++ {
		m, _ := h.Post("lobby", "A", "Nachricht")
		ids = append(ids, m.ID)
	}
	for _, id := range ids { // jede Nachricht zweimal geändert → 3000 überholte Zeilen
		h.DeleteMessage("lobby", id)
		h.RestoreMessage("lobby", id)
	}
	h.DeleteMessage("lobby", ids[7])
	flush(t, h, u, s)
	s.Close()

	path := filepath.Join(dir, "rooms", "lobby.messages.jsonl")
	before, _ := os.ReadFile(path)
	if n := countLines(before); n != 4501 {
		t.Fatalf("erwartet 4501 Zeilen vor dem Aufräumen, bekam %d", n)
	}
	h2, _, _ := openStore(t, dir) // räumt beim Laden auf
	after, _ := os.ReadFile(path)
	if n := countLines(after); n != 1500 {
		t.Fatalf("erwartet 1500 Zeilen nach dem Aufräumen, bekam %d", n)
	}
	msgs, _, _ := h2.AdminHistory("lobby", 0, MaxPage)
	_ = msgs
	all := adminHistAll(h2, "lobby")
	if len(all) != 1500 || !all[7].Deleted || all[8].Deleted {
		t.Fatalf("Stand nach Aufräumen falsch (len %d)", len(all))
	}
}

func TestCrashLeavesPartialLine(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.Post("lobby", "A", "eins")
	h.Post("lobby", "A", "zwei")
	flush(t, h, u, s)
	s.Close()
	// Absturz mitten im Schreiben simulieren: halbe Zeile anhängen.
	path := filepath.Join(dir, "rooms", "lobby.messages.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"id":3,"room":"lobby","name":"A","te`)
	f.Close()

	h2, u2, s2 := openStore(t, dir)
	if msgs := hist(h2, "lobby"); len(msgs) != 2 {
		t.Fatalf("erwartet 2 Nachrichten, bekam %d", len(msgs))
	}
	h2.Post("lobby", "A", "drei") // muss sauber in einer neuen Zeile landen
	flush(t, h2, u2, s2)
	s2.Close()
	h3, _, _ := openStore(t, dir)
	if msgs := hist(h3, "lobby"); len(msgs) != 3 || msgs[2].Text != "drei" {
		t.Fatalf("nach Reparatur: %+v", msgs)
	}
}

func TestCorruptFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{kaputt"), 0o600)
	if err := NewStore(dir).Load(NewHub(0), nil); err == nil {
		t.Fatal("beschädigte Datei wurde stillschweigend akzeptiert")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.json")); string(b) != "{kaputt" {
		t.Fatal("beschädigte Datei wurde verändert")
	}
	// Kaputte Zeile mitten in einer Nachrichtendatei: ebenfalls Abbruch statt Datenverlust.
	dir2 := t.TempDir()
	os.MkdirAll(filepath.Join(dir2, "rooms"), 0o700)
	os.WriteFile(filepath.Join(dir2, "rooms", "lobby.messages.jsonl"), []byte("{\"id\":1}\nMÜLL\n{\"id\":2}\n"), 0o600)
	if err := NewStore(dir2).Load(NewHub(0), nil); err == nil {
		t.Fatal("kaputte Zeile wurde stillschweigend übersprungen")
	}
}

func TestMigrationFromSingleFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"nextId":7,"settings":{"title":"Alt","allowUserRooms":false},
	 "automod":{"enabled":true,"patterns":["arsch*"]},
	 "roomMeta":{"lobby":{"description":"Alte Lobby","created":"2026-10-01T10:00:00Z"},"leer":{"created":"2026-10-01T10:00:00Z"}},
	 "rooms":{"lobby":[{"id":3,"room":"lobby","name":"A","text":"hallo","time":"2026-10-01T10:00:00Z"},
	                   {"id":5,"room":"lobby","name":"B","text":"","time":"2026-10-01T10:01:00Z","deleted":true,"deletedBy":"automod","original":"Arsch","match":"arsch*"}]},
	 "users":[{"id":"u1","name":"A","tokenHash":"abc","created":"2026-10-01T10:00:00Z","lastSeen":"2099-01-01T00:00:00Z"}]}`
	os.WriteFile(filepath.Join(dir, "gochat.json"), []byte(legacy), 0o600)

	h, _, _ := openStore(t, dir)
	if h.Settings().Title != "Alt" || len(h.Rooms()) != 2 || len(hist(h, "lobby")) != 2 {
		t.Fatalf("Migration unvollständig: %+v %+v", h.Settings(), h.Rooms())
	}
	if q := h.AutomodQueue(); len(q) != 1 {
		t.Fatalf("Prüfliste nach Migration: %+v", q)
	}
	if m, _ := h.Post("lobby", "C", "neu"); m.ID <= 7 {
		t.Fatalf("ID-Zähler nicht übernommen: %d", m.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, "gochat.json.migrated")); err != nil {
		t.Fatal("alte Datei wurde nicht umbenannt")
	}
	for _, f := range []string{"config.json", "automod.json", "users.json", "rooms/lobby.json", "rooms/lobby.messages.jsonl", "rooms/leer.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s fehlt nach Migration", f)
		}
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// adminHistAll lädt den gesamten Verlauf seitenweise (wie ein Client beim Hochscrollen).
func adminHistAll(h *Hub, room string) []Message {
	var all []Message
	var before int64
	for {
		page, more, _ := h.AdminHistory(room, before, MaxPage)
		all = append(page, all...)
		if !more || len(page) == 0 {
			return all
		}
		before = page[0].ID
	}
}
