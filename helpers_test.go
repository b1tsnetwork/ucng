// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"
	"time"
)

// hist liefert den (öffentlichen) Verlauf eines Kanals für Tests.
func hist(h *Hub, room string) []Message {
	msgs, _, _ := h.History(room, 0, MaxPage)
	return msgs
}

// adminHist liefert den Verlauf inklusive Moderationsdaten.
func adminHist(h *Hub, room string) []Message {
	msgs, _, _ := h.AdminHistory(room, 0, MaxPage)
	return msgs
}

// openStore lädt (oder legt an) einen Datenordner wie beim Serverstart.
func openStore(t *testing.T, dir string) (*Hub, *Users, *Store) {
	t.Helper()
	h, u, s := NewHub(0), NewUsers(time.Hour, 1000), NewStore(dir)
	if err := s.Load(h, u); err != nil {
		t.Fatal(err)
	}
	h.EnablePersistence()
	t.Cleanup(s.Close)
	return h, u, s
}

// flush speichert alle vorgemerkten Änderungen.
func flush(t *testing.T, h *Hub, u *Users, s *Store) {
	t.Helper()
	if err := s.Flush(h, u); err != nil {
		t.Fatal(err)
	}
}
