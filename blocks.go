// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Schutz in Direktnachrichten:
//   - Blockieren: Zwischen zwei Personen, von denen eine die andere blockiert hat, kann keine
//     neue Unterhaltung beginnen und in einer bestehenden niemand mehr schreiben. Nachrichten von
//     anderen Instanzen werden stillschweigend verworfen. In Kanälen blendet der Chat Beiträge
//     blockierter Personen bei der blockierenden Person ein (aufklappbar) – sie bleiben aber für
//     alle anderen sichtbar; Moderation erledigt das Team.
//   - Unterhaltung löschen: verschwindet für die löschende Person (bis eine neue Nachricht kommt).
//     Haben alle Beteiligten dieser Instanz gelöscht, wird der Verlauf endgültig entfernt.
//     Optional werden die eigenen Nachrichten auch beim Gegenüber gelöscht.

var (
	ErrDMBlocked   = errors.New("In dieser Unterhaltung ist kein Austausch möglich")
	ErrBlockSelf   = errors.New("Du kannst dich nicht selbst blockieren")
	ErrBlockLimit  = errors.New("Du hast zu viele Personen blockiert")
	ErrNotDMMember = errors.New("Diese Unterhaltung gibt es nicht")
)

const maxBlocks = 500

// Blocks: wer wen blockiert (Nutzer-ID → blockierte IDs, auch "fed:name@instanz").
type Blocks struct {
	mu      sync.Mutex
	m       map[string]map[string]time.Time
	changed func()
}

func NewBlocks() *Blocks { return &Blocks{m: map[string]map[string]time.Time{}} }

// Has: hat a die Person target blockiert?
func (b *Blocks) Has(a, target string) bool {
	if b == nil || a == "" || target == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[a][target]
	return ok
}

// Either: hat eine der beiden Personen die andere blockiert?
func (b *Blocks) Either(x, y string) bool { return b.Has(x, y) || b.Has(y, x) }

// List liefert die von a blockierten IDs (älteste zuerst).
func (b *Blocks) List(a string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.m[a]))
	for id := range b.m[a] {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return b.m[a][out[i]].Before(b.m[a][out[j]]) })
	return out
}

// Set blockiert target (on) oder hebt die Blockierung auf. changed: hat sich etwas geändert?
func (b *Blocks) Set(a, target string, on bool) (changed bool, err error) {
	if a == target {
		return false, ErrBlockSelf
	}
	b.mu.Lock()
	set := b.m[a]
	_, had := set[target]
	switch {
	case on && !had:
		if len(set) >= maxBlocks {
			b.mu.Unlock()
			return false, ErrBlockLimit
		}
		if set == nil {
			set = map[string]time.Time{}
			b.m[a] = set
		}
		set[target] = time.Now().UTC()
		changed = true
	case !on && had:
		delete(set, target)
		if len(set) == 0 {
			delete(b.m, a)
		}
		changed = true
	}
	b.mu.Unlock()
	if changed && b.changed != nil {
		b.changed()
	}
	return changed, nil
}

func (b *Blocks) export() map[string]map[string]time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]map[string]time.Time, len(b.m))
	for a, set := range b.m {
		c := make(map[string]time.Time, len(set))
		for id, t := range set {
			c[id] = t
		}
		out[a] = c
	}
	return out
}

func (b *Blocks) restore(m map[string]map[string]time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m = map[string]map[string]time.Time{}
	for a, set := range m {
		if len(set) > 0 {
			b.m[a] = set
		}
	}
}

// --- Hub ---

func (h *Hub) Blocks() *Blocks { return h.blocks }

// dmBlocked: Direktnachricht, in der eine Person die andere blockiert hat?
func (r *room) dmBlocked() bool {
	return r.kind == RoomDM && len(r.members) == 2 && r.blocks.Either(r.members[0], r.members[1])
}

// dmFloor: bis zu dieser Nachrichten-ID hat userID die Unterhaltung gelöscht (0 = nichts).
func (r *room) dmFloor(userID string) int64 {
	if r.kind != RoomDM || userID == "" {
		return 0
	}
	return r.cleared[userID]
}

// DMFloor wie dmFloor, für Handler.
func (h *Hub) DMFloor(roomName, userID string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[roomName]; ok {
		return r.dmFloor(userID)
	}
	return 0
}

// IsDMBlocked: Kann in dieser Direktnachricht niemand schreiben, weil blockiert wurde?
func (h *Hub) IsDMBlocked(roomName string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	return ok && r.dmBlocked()
}

// ClearDM löscht die Unterhaltung für userID. alsoOwn: eigene Nachrichten auch für das Gegenüber
// löschen (liefert deren IDs – für die Föderation). Haben alle Beteiligten dieser Instanz
// gelöscht, wird der Verlauf bis dahin endgültig entfernt.
func (h *Hub) ClearDM(roomName, userID string, alsoOwn bool) (own []int64, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok || r.kind != RoomDM || !contains(r.members, userID) {
		return nil, ErrNotDMMember
	}
	if alsoOwn {
		now := time.Now().UTC()
		for i := range r.history {
			m := &r.history[i]
			if m.UserID != userID || (m.Deleted && m.DeletedBy != "automod") {
				continue
			}
			m.Text, m.Original, m.Edits, m.Match, m.Reactions, m.Media, m.Poll, m.Unfiltered, m.E2E = "", "", nil, "", nil, nil, nil, "", ""
			m.Deleted, m.DeletedBy, m.DeletedAt, m.Reviewed, m.ModBy = true, "self", &now, true, ""
			h.afterChangeLocked(r, m)
			own = append(own, m.ID)
		}
		if len(own) > 0 {
			h.record(storeOp{kind: opRoomRewrite, room: r.name})
		}
	}
	n := len(r.history)
	if n == 0 {
		return own, nil
	}
	last := r.history[n-1].ID
	if r.cleared == nil {
		r.cleared = map[string]int64{}
	}
	r.cleared[userID] = last
	// Endgültig entfernen, was alle Beteiligten dieser Instanz gelöscht haben.
	floor := int64(-1)
	for _, mem := range r.members {
		if isFedID(mem) {
			continue // die andere Instanz hat ihre eigene Kopie
		}
		c := r.cleared[mem]
		if floor < 0 || c < floor {
			floor = c
		}
	}
	if floor > 0 {
		h.removeLocked(r, func(m *Message) bool { return m.ID <= floor })
	}
	h.record(storeOp{kind: opRoomMeta, room: r.name})
	return own, nil
}

// --- HTTP ---

type blockedInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Fed  bool   `json:"fed,omitempty"`
}

func (s *Server) blockedList(meID string) []blockedInfo {
	out := []blockedInfo{}
	for _, id := range s.hub.Blocks().List(meID) {
		bi := blockedInfo{ID: id}
		if isFedID(id) {
			bi.Name, bi.Fed = strings.TrimPrefix(id, fedPrefix), true
		} else if u, ok := s.users.Get(id); ok {
			bi.Name = u.Name
		} else {
			bi.Name = "(Name freigegeben)"
		}
		out = append(out, bi)
	}
	return out
}

// GET /api/blocks – von mir blockierte Personen.
func (s *Server) handleBlocks(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": s.blockedList(me.ID)})
}

// POST /api/blocks {"userId": "…", "block": true, "clear": false} – blockieren oder aufheben.
// clear: die Unterhaltung mit der Person zusätzlich für mich löschen.
func (s *Server) handleBlockSet(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	var in struct {
		UserID string `json:"userId"`
		Block  bool   `json:"block"`
		Clear  bool   `json:"clear"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	target := strings.TrimSpace(in.UserID)
	if isFedID(target) {
		addr := strings.ToLower(strings.TrimPrefix(target, fedPrefix))
		if _, _, err := parseFedAddr(addr); err != nil {
			fail(w, ErrNoUser)
			return
		}
		target = fedPrefix + addr
	} else if _, found := s.users.Get(target); !found && in.Block {
		fail(w, ErrNoUser)
		return
	}
	if _, err := s.hub.Blocks().Set(me.ID, target, in.Block); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	room := dmRoomName(me.ID, target)
	if in.Block && in.Clear && s.hub.HasRoom(room) {
		s.hub.ClearDM(room, me.ID, false)
	}
	// Beide Seiten aktualisieren ihre Liste der Unterhaltungen (Schreibfeld sperren/freigeben).
	s.hub.NotifyUser("dm", me.ID)
	if !isFedID(target) {
		s.hub.NotifyUser("dm", target)
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": s.blockedList(me.ID)})
}

// DELETE /api/dms/{room}?own=1 – Unterhaltung für mich löschen; own=1: meine Nachrichten auch
// beim Gegenüber löschen.
func (s *Server) handleDMClear(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	room := NormRoom(r.PathValue("room"))
	own, err := s.hub.ClearDM(room, me.ID, r.URL.Query().Get("own") == "1")
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if len(own) > 0 {
		if _, peer, fed := s.e2eMode(room, me); fed {
			now := time.Now().UTC()
			for _, id := range own {
				s.fedSend(peer, fedPayload{Type: "delete", From: me.Name, ID: id, Time: now}, "", 0)
			}
		}
	}
	s.hub.NotifyUser("dm", me.ID)
	w.WriteHeader(http.StatusNoContent)
}
