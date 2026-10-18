// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"time"
)

// Zugriff auf Kanäle: private Kanäle (nur bestimmte Rollen lesen), Kanäle, in denen nur
// bestimmte Rollen schreiben (z. B. Ankündigungen), und Direktnachrichten (nur die beiden
// Beteiligten). Das Team darf in allen Rollen-Kanälen lesen und schreiben, Direktnachrichten
// sehen nur die Beteiligten (und Admins im Admin-Bereich).

// Access beschreibt, wer zugreift (nil = Gast ohne Namen).
type Access struct {
	ID    string
	Team  bool
	Roles []string
}

const RoomDM = "dm"

var (
	ErrReadOnly = errors.New("In diesem Kanal dürfen nur bestimmte Rollen schreiben")
	ErrDMName   = errors.New("Kanalnamen mit „dm-“ am Anfang sind für Direktnachrichten reserviert")
)

func accessOf(p Poster) *Access { return &Access{ID: p.ID, Team: p.Team, Roles: p.Roles} }

func (v *Viewer) access() *Access {
	if v == nil {
		return nil
	}
	return &Access{ID: v.ID, Team: v.Team, Roles: v.Roles}
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// restricted: Sehen nicht alle diesen Kanal?
func (r *room) restricted() bool { return r.kind == RoomDM || len(r.readRoles) > 0 }

// canSee: Darf a den Kanal sehen (Rollen)? Mit Passwort steht er dann mit Schloss in der Liste.
func (r *room) canSee(a *Access) bool {
	if r.kind == RoomDM {
		return a != nil && contains(r.members, a.ID)
	}
	if len(r.readRoles) == 0 {
		return true
	}
	return a != nil && (a.Team || intersects(r.readRoles, a.Roles))
}

// canRead: sehen und – bei einem Passwort – schon entsperrt.
func (r *room) canRead(a *Access) bool { return r.canSee(a) && r.passOK(a) }

func (r *room) canWrite(a *Access) bool {
	if !r.canRead(a) {
		return false
	}
	if r.kind == RoomDM {
		return !r.dmBlocked()
	}
	if r.remote != nil && !r.remote.writable() {
		return false // abonnierter Kanal: nur lesen (oder Freigabe beendet)
	}
	if len(r.writeRoles) == 0 {
		return true
	}
	return a != nil && (a.Team || intersects(r.writeRoles, a.Roles))
}

// readableLocked liefert einen Kanal, wenn a ihn lesen darf (sonst ErrNoRoom – private Kanäle
// sollen für andere gar nicht zu sehen sein). h.mu muss gehalten werden.
func (h *Hub) readableLocked(roomName string, a *Access) (*room, error) {
	r, ok := h.rooms[roomName]
	if !ok || !r.canRead(a) {
		return nil, ErrNoRoom
	}
	return r, nil
}

// CanRead: Darf a den Kanal lesen? (Unbekannte Kanäle: wie bisher – sie entstehen beim Schreiben.)
func (h *Hub) CanRead(roomName string, a *Access) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	return !ok || r.canRead(a)
}

// RoomsFor liefert die Kanäle, die a sehen darf (ohne Direktnachrichten).
func (h *Hub) RoomsFor(a *Access) []RoomInfo {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []RoomInfo{}
	for _, r := range h.rooms {
		if r.kind == "" && r.canSee(a) && r.listedFor(a) {
			ri := r.info()
			ri.CanWrite = r.canWrite(a)
			ri.Locked = !r.passOK(a)
			ri.Group = h.groupOf(r)
			ri.Threads = r.activeThreadsLocked(now)
			if ri.Remote != nil && a != nil && r.remote != nil && contains(r.remote.Subs, a.ID) {
				ri.Remote.Subscribed = true
			}
			out = append(out, ri)
		}
	}
	sortRooms(out)
	return out
}

// HistoryAs wie History, aber nur, wenn a den Kanal lesen darf.
func (h *Hub) HistoryAs(roomName string, a *Access, before int64, limit int) ([]Message, bool, error) {
	if !h.CanRead(roomName, a) {
		return nil, false, ErrNoRoom
	}
	msgs, more, err := h.History(roomName, before, limit)
	if err == nil && a != nil {
		if floor := h.DMFloor(roomName, a.ID); floor > 0 {
			if before > 0 && before <= floor+1 {
				return []Message{}, false, nil
			}
			msgs, more = (&room{}).trimFloor(msgs, more, floor)
		}
	}
	return msgs, more, err
}

// RefreshViewer übernimmt geänderte Rollen/Team einer Person in die offenen Verbindungen und
// beendet Verbindungen zu Kanälen, die sie nicht mehr lesen darf.
func (h *Hub) RefreshViewer(userID string, team bool, roles []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		changed := false
		for c, v := range r.clients {
			if v == nil || v.ID != userID {
				continue
			}
			nv := &Viewer{ID: v.ID, Name: v.Name, Team: team, Roles: append([]string(nil), roles...)}
			if !r.canRead(nv.access()) {
				delete(r.clients, c)
				close(c)
			} else {
				r.clients[c] = nv
			}
			changed = true
		}
		if changed {
			h.presenceSoonLocked(r)
		}
	}
	h.userChangedLocked(userID)
}

// kickUnreadableLocked beendet Verbindungen, die den Kanal nicht (mehr) lesen dürfen.
// DropViewer schließt die Live-Streams eines Kontos, deren Sitzung nicht mehr gilt.
func (h *Hub) DropViewer(userID string, alive []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		for c, v := range r.clients {
			if v != nil && v.ID == userID && !contains(alive, v.Session) {
				delete(r.clients, c)
				close(c)
			}
		}
	}
	h.userChangedLocked(userID)
}

func (h *Hub) kickUnreadableLocked(r *room) {
	for c, v := range r.clients {
		if !r.canRead(v.access()) {
			delete(r.clients, c)
			close(c)
		}
	}
}

// RoleTeamOnly in readRoles/writeRoles: nur das Team (keine weitere Rolle).
const RoleTeamOnly = "team"

// validRoleList prüft Rollen-IDs eines Kanals ("team" = nur das Team).
func (h *Hub) validRoleList(ids []string) []string {
	out := h.roles.Filter(ids)
	if contains(ids, RoleTeamOnly) && len(out) == 0 {
		return []string{RoleTeamOnly}
	}
	return out
}

func isDMName(name string) bool { return len(name) > 3 && name[:3] == "dm-" }

// pruneRoomRoles entfernt gelöschte Rollen aus den Kanal-Einstellungen.
func (h *Hub) pruneRoomRoles() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rooms {
		rr, wr := h.roles.Filter(r.readRoles), h.roles.Filter(r.writeRoles)
		// Wurde die letzte Rolle gelöscht, bleibt der Kanal dem Team vorbehalten (nie plötzlich öffentlich).
		if len(rr) == 0 && len(r.readRoles) > 0 {
			rr = []string{RoleTeamOnly}
		}
		if len(wr) == 0 && len(r.writeRoles) > 0 {
			wr = []string{RoleTeamOnly}
		}
		if r.voice != nil && len(r.voice.StageRoles) > 0 {
			if sr := h.roles.Filter(r.voice.StageRoles); len(sr) != len(r.voice.StageRoles) {
				c := *r.voice // gespeicherte Einstellungen nie verändern: Kopie
				c.StageRoles = sr
				r.voice = &c
				h.record(storeOp{kind: opRoomMeta, room: r.name})
			}
		}
		if len(rr) != len(r.readRoles) || len(wr) != len(r.writeRoles) || (len(rr) > 0 && rr[0] != r.readRoles[0]) {
			r.readRoles, r.writeRoles = rr, wr
			h.record(storeOp{kind: opRoomMeta, room: r.name})
		}
	}
}

// IsRestricted: Sehen nicht alle diesen Kanal (private Kanäle, Direktnachrichten)?
func (h *Hub) IsRestricted(roomName string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	return ok && r.restricted()
}
