// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"
)

// Passwortgeschützte Kanäle: Der Kanal steht wie gewohnt in der Liste (mit Schloss). Lesen,
// Schreiben und der Sprachkanal gehen erst, nachdem die Person einmal das Passwort eingegeben hat –
// das wird je Person gemerkt (bis das Passwort geändert wird). Das Team braucht kein Passwort.

var (
	ErrRoomPassword  = errors.New("Falsches Passwort")
	ErrRoomPassLen   = errors.New("Passwort: 4 bis 64 Zeichen")
	ErrRoomNoPass    = errors.New("Dieser Kanal hat kein Passwort")
	ErrRoomPassRate  = errors.New("Zu viele Versuche – bitte eine Minute warten.")
	ErrRoomNeedsPass = errors.New("Dieser Kanal ist mit einem Passwort geschützt")
)

// passOK: Darf a trotz Passwort hinein? (Team immer, sonst nach Eingabe.)
func (r *room) passOK(a *Access) bool {
	if r.passHash == "" {
		return true
	}
	return a != nil && (a.Team || r.unlocked[a.ID])
}

// SetRoomPassword setzt ("" = entfernt) das Passwort eines Kanals. Alle müssen es danach neu
// eingeben; owner (Besitzer eines temporären Kanals) bleibt drin.
func (h *Hub) SetRoomPassword(name, pw, owner string) error {
	hash := ""
	if pw != "" {
		if n := utf8.RuneCountInString(pw); n < 4 || n > 64 {
			return ErrRoomPassLen
		}
		salt, sum := hashPassword(pw)
		hash = salt + ":" + sum
	}
	h.mu.Lock()
	r, ok := h.rooms[name]
	if !ok || r.kind != "" {
		h.mu.Unlock()
		return ErrNoRoom
	}
	r.passHash = hash
	r.unlocked = map[string]bool{}
	if owner != "" && hash != "" {
		r.unlocked[owner] = true
	}
	h.kickUnreadableLocked(r)
	h.record(storeOp{kind: opRoomMeta, room: name})
	info := r.info()
	h.broadcastAll(Event{Type: "layout"})
	h.broadcast(r, Event{Type: "room", RoomInfo: &info, Online: len(r.clients)})
	h.roomChangedLocked(name, name)
	h.mu.Unlock()
	return nil
}

// UnlockRoom prüft das Passwort und merkt sich die Person.
func (h *Hub) UnlockRoom(name, userID, pw string) error {
	h.mu.Lock()
	r, ok := h.rooms[name]
	if !ok || r.kind != "" {
		h.mu.Unlock()
		return ErrNoRoom
	}
	hash := r.passHash
	h.mu.Unlock()
	if hash == "" {
		return ErrRoomNoPass
	}
	salt, sum, _ := strings.Cut(hash, ":")
	s, err1 := hex.DecodeString(salt)
	want, err2 := hex.DecodeString(sum)
	if err1 != nil || err2 != nil || subtle.ConstantTimeCompare(pbkdf2([]byte(pw), s, pbkdf2Rounds), want) != 1 {
		return ErrRoomPassword
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.passHash != hash { // inzwischen geändert
		return ErrRoomPassword
	}
	if r.unlocked == nil {
		r.unlocked = map[string]bool{}
	}
	r.unlocked[userID] = true
	h.record(storeOp{kind: opRoomMeta, room: name})
	return nil
}

func unlockedList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// POST /api/rooms/{room}/unlock {"password": "…"}
func (s *Server) handleRoomUnlock(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	if !s.unlockRL.Allow(s.clientIP(r)) || !s.unlockRL.Allow("u:"+me.ID) {
		writeErr(w, http.StatusTooManyRequests, ErrRoomPassRate.Error())
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	room := NormRoom(r.PathValue("room"))
	// Den Kanal muss man (ohne Passwort) überhaupt sehen dürfen
	if !s.hub.canSeeRoom(room, &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}) {
		fail(w, ErrNoRoom)
		return
	}
	switch err := s.hub.UnlockRoom(room, me.ID, in.Password); {
	case errors.Is(err, ErrRoomPassword):
		writeErr(w, http.StatusForbidden, err.Error())
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (h *Hub) canSeeRoom(name string, a *Access) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	return ok && r.kind == "" && r.canSee(a)
}
