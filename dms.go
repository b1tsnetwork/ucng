// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Direktnachrichten: ein privater Kanal zwischen genau zwei Personen. Er taucht nicht in der
// Kanalliste auf, Bots und Webhooks bekommen nichts davon mit. Admins können ihn über
// Meldungen und die Suche im Admin-Bereich einsehen (kein Ende-zu-Ende-Schutz).

var (
	ErrDMsOff  = errors.New("Direktnachrichten sind ausgeschaltet")
	ErrDMSelf  = errors.New("An dich selbst kannst du keine Direktnachricht schreiben")
	ErrDMLimit = errors.New("Zu viele neue Unterhaltungen – bitte später noch einmal")
)

func dmRoomName(a, b string) string {
	if a > b {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte("gochat-dm:" + a + "|" + b))
	return "dm-" + hex.EncodeToString(sum[:8])
}

// OpenDM liefert (und legt bei Bedarf an) den Kanal zwischen zwei Personen.
func (h *Hub) OpenDM(a, b string) string {
	name := dmRoomName(a, b)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.rooms[name]; !ok {
		r := &room{name: name, created: time.Now().UTC(), links: LinksAll, botLinks: LinksAll, amMode: AutomodGlobal,
			clients: make(map[chan Event]*Viewer), kind: RoomDM, members: []string{a, b}, blocks: h.blocks}
		h.rooms[name] = r
		h.record(storeOp{kind: opRoomMeta, room: name})
	}
	return name
}

// DMInfo: eine Unterhaltung aus Sicht einer Person.
type DMInfo struct {
	Room     string    `json:"room"`
	With     string    `json:"with"` // Nutzer-ID des Gegenübers
	Name     string    `json:"name"` // aktueller Name des Gegenübers
	LastID   int64     `json:"lastId"`
	LastAt   time.Time `json:"lastAt"`
	Messages int       `json:"messages"`
	Fed      bool      `json:"fed,omitempty"` // Gegenüber auf einer anderen Instanz (With = "fed:name@domain")
	E2E      bool      `json:"e2e,omitempty"` // Ende-zu-Ende-verschlüsselt
	// Blockiert: niemand kann mehr schreiben. BlockedByMe: ich habe blockiert (sonst das Gegenüber).
	Blocked     bool `json:"blocked,omitempty"`
	BlockedByMe bool `json:"blockedByMe,omitempty"`
}

// DMsOf liefert alle Unterhaltungen einer Person (neueste zuerst).
func (h *Hub) DMsOf(userID string) []DMInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []DMInfo{}
	for _, r := range h.rooms {
		if r.kind != RoomDM || !contains(r.members, userID) {
			continue
		}
		other := r.members[0]
		if other == userID && len(r.members) > 1 {
			other = r.members[1]
		}
		floor := r.dmFloor(userID)
		start, _ := r.find(floor + 1)
		if floor > 0 && start >= len(r.history) {
			continue // für mich gelöscht, seitdem nichts Neues
		}
		d := DMInfo{Room: r.name, With: other, LastAt: r.last, Messages: len(r.history) - start}
		if n := len(r.history); n > 0 {
			d.LastID = r.history[n-1].ID
		}
		d.BlockedByMe = h.blocks.Has(userID, other)
		d.Blocked = d.BlockedByMe || h.blocks.Has(other, userID)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	return out
}

// GET /api/dms
func (s *Server) handleDMs(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	list := s.hub.DMsOf(me.ID)
	out := list[:0]
	for _, d := range list {
		if isFedID(d.With) {
			d.Name, d.Fed, d.E2E = strings.TrimPrefix(d.With, fedPrefix), true, true
			out = append(out, d)
			continue
		}
		if enc, _, _ := s.e2eMode(d.Room, me); enc {
			d.E2E = true
		}
		if u, ok := s.users.Get(d.With); ok {
			d.Name = u.Name
		} else if d.Messages == 0 {
			continue // leere Unterhaltung mit freigegebenem Namen
		} else {
			d.Name = "(Name freigegeben)"
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": !s.hub.Settings().NoDMs, "dms": out})
}

// POST /api/dms {"userId": "…"} – Unterhaltung öffnen.
func (s *Server) handleDMOpen(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	if s.hub.Settings().NoDMs {
		fail(w, ErrDMsOff)
		return
	}
	var in struct {
		UserID  string `json:"userId"`
		Address string `json:"address"` // name@andere-instanz (Modus „hosted“)
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Address != "" {
		if err := s.hub.checkSanction(s.posterOf(me, ip)); err != nil {
			fail(w, err)
			return
		}
		if !s.hosted() {
			writeErr(w, http.StatusBadRequest, ErrFedOff.Error())
			return
		}
		s.openFedDM(w, r, me, in.Address)
		return
	}
	if in.UserID == me.ID {
		fail(w, ErrDMSelf)
		return
	}
	other, found := s.users.Get(in.UserID)
	if !found {
		fail(w, ErrNoUser)
		return
	}
	if s.hub.Blocks().Either(me.ID, other.ID) {
		writeErr(w, http.StatusForbidden, ErrDMBlocked.Error())
		return
	}
	if err := s.hub.checkSanction(s.posterOf(me, ip)); err != nil {
		fail(w, err)
		return
	}
	name := dmRoomName(me.ID, other.ID)
	if !s.hub.HasRoom(name) && !s.dmRL.Allow(me.ID) {
		fail(w, ErrDMLimit)
		return
	}
	room := s.hub.OpenDM(me.ID, other.ID)
	s.hub.NotifyUser("dm", other.ID)
	enc, _, _ := s.e2eMode(room, me)
	writeJSON(w, http.StatusOK, DMInfo{Room: room, With: other.ID, Name: other.Name, E2E: enc})
}

// HasRoom: gibt es den Kanal (auch Direktnachrichten)?
func (h *Hub) HasRoom(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.rooms[name]
	return ok
}

// NotifyUser schickt ein Ereignis an alle Verbindungen (nur die genannte Person reagiert).
func (h *Hub) NotifyUser(typ, userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastAll(Event{Type: typ, User: userID})
}

// IsChannel: gibt es den Kanal (keine Direktnachricht)?
func (h *Hub) IsChannel(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	return ok && r.kind == ""
}
