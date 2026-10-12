// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLayoutGroupsAndOrder(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	for _, n := range []string{"lobby", "alpha", "beta", "gamma", "news"} {
		h.CreateRoom(n, "")
	}
	l, err := h.SetLayout(LayoutInput{
		Groups: []struct {
			ID    string   `json:"id"`
			Name  string   `json:"name"`
			Rooms []string `json:"rooms"`
		}{{Name: "Projekte", Rooms: []string{"gamma", "alpha"}}, {Name: "Info", Rooms: []string{"news"}}},
		Ungrouped: []string{"lobby"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Groups) != 2 || l.Groups[0].Name != "Projekte" || l.Groups[0].Rooms[0] != "gamma" || l.Groups[0].Rooms[1] != "alpha" {
		t.Fatalf("Layout: %+v", l)
	}
	if l.Ungrouped[0] != "lobby" || l.Ungrouped[1] != "beta" { // nicht genannt → ans Ende
		t.Fatalf("ohne Gruppe: %v", l.Ungrouped)
	}
	rooms := h.RoomsFor(nil)
	var order []string
	for _, r := range rooms {
		order = append(order, r.Name)
	}
	if order[0] != "lobby" || order[2] != "gamma" {
		t.Fatalf("Reihenfolge: %v", order)
	}
	// Umbenennen behält Gruppe und Position, neue Kanäle landen am Ende
	h.UpdateRoom("gamma", "gamma2", "")
	h.CreateRoom("neu", "")
	flush(t, h, u, s)
	s.Close()
	h2, _, _ := openStore(t, dir)
	l2 := h2.Layout()
	if l2.Groups[0].Rooms[0] != "gamma2" || l2.Ungrouped[len(l2.Ungrouped)-1] != "neu" {
		t.Fatalf("nach Neustart: %+v", l2)
	}
	// Gruppe löschen: Kanäle sind wieder ohne Gruppe
	h2.SetLayout(LayoutInput{Ungrouped: []string{"news"}})
	if l3 := h2.Layout(); len(l3.Groups) != 0 || l3.Ungrouped[0] != "news" || len(l3.Ungrouped) != 6 {
		t.Fatalf("ohne Gruppen: %+v", l3)
	}
}

func TestActiveThreadsInRoomList(t *testing.T) {
	h := NewHub(0)
	h.CreateRoom("lobby", "")
	root, _ := h.Post("lobby", "Ida", "Worum geht's?")
	h.SendFull("lobby", Poster{ID: "x", Name: "Bo"}, SendInput{Text: "Antwort", Thread: root.ID})
	rooms := h.RoomsFor(nil)
	if len(rooms[0].Threads) != 1 || rooms[0].Threads[0].Title != "Worum geht's?" || rooms[0].Threads[0].Count != 1 {
		t.Fatalf("Threads: %+v", rooms[0].Threads)
	}
	b, _ := json.Marshal(h.Rooms())
	if json.Valid(b) && len(h.Rooms()[0].Threads) != 0 {
		t.Fatal("Admin-Liste ohne Threads erwartet")
	}
	_ = time.Now
}

func TestUserPins(t *testing.T) {
	ts, _ := testServer(t)
	tok := claim(t, ts.URL, "Pia")
	res := do(t, "PUT", ts.URL+"/api/profile/pins", tok, `{"rooms":["Lobby","lobby","dm-123","news"]}`)
	if res.StatusCode != 200 {
		t.Fatalf("pins: %d", res.StatusCode)
	}
	var s struct{ Pinned []string }
	json.NewDecoder(do(t, "GET", ts.URL+"/api/session", tok, "").Body).Decode(&s)
	if len(s.Pinned) != 2 || s.Pinned[0] != "lobby" || s.Pinned[1] != "news" {
		t.Fatalf("pinned: %v", s.Pinned)
	}
}
