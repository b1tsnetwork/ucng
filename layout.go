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

// Kanal-Gruppen und Reihenfolge: Das Team ordnet Kanäle in aufklappbare Gruppen und legt die
// Reihenfolge fest (Admin-Bereich → Kanäle). Jeder Kanal merkt sich Gruppe und Position
// (roomMeta.Group/Pos), die Gruppen selbst stehen in groups.json.

const (
	maxGroups       = 50
	maxGroupName    = 40
	threadListMax   = 5                  // aktive Threads je Kanal in der Seitenleiste
	threadListSince = 7 * 24 * time.Hour // „aktiv“ = Antwort in den letzten 7 Tagen
	maxUserPins     = 50
)

var (
	ErrGroupName = errors.New("Gruppenname: 1–40 Zeichen")
	ErrTooGroups = errors.New("Höchstens 50 Gruppen")
)

type RoomGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Groups struct {
	mu      sync.Mutex
	list    []RoomGroup
	changed func()
}

func NewGroups() *Groups { return &Groups{} }

func (g *Groups) List() []RoomGroup {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]RoomGroup{}, g.list...)
}

func (g *Groups) has(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, x := range g.list {
		if x.ID == id {
			return true
		}
	}
	return false
}

func (g *Groups) restore(list []RoomGroup) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.list = nil
	for _, x := range list {
		if x.ID != "" && x.Name != "" {
			g.list = append(g.list, x)
		}
	}
}

// ThreadRef: ein aktiver Thread unter einem Kanal in der Seitenleiste.
type ThreadRef struct {
	ID     int64     `json:"id"`
	Title  string    `json:"title"`
	Count  int       `json:"count"`
	LastAt time.Time `json:"lastAt"`
}

// activeThreadsLocked liefert die zuletzt aktiven Threads eines Kanals. h.mu muss gehalten werden.
func (r *room) activeThreadsLocked(now time.Time) []ThreadRef {
	var out []ThreadRef
	for id, t := range r.threads {
		if t.Count == 0 || now.Sub(t.LastAt) > threadListSince {
			continue
		}
		i, ok := r.find(id)
		if !ok || r.history[i].Deleted {
			continue
		}
		root := r.history[i]
		title := snippet(root.Text, 60)
		if title == "" && root.Poll != nil {
			title = "Umfrage: " + snippet(root.Poll.Question, 50)
		}
		if title == "" && root.Media != nil {
			title = "Bild"
			if root.Media.Kind == "gif" {
				title = "GIF"
			}
		}
		out = append(out, ThreadRef{ID: id, Title: title, Count: t.Count, LastAt: t.LastAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	if len(out) > threadListMax {
		out = out[:threadListMax]
	}
	return out
}

// sortRooms: nach Position, dann Name.
func sortRooms(list []RoomInfo) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Pos != list[j].Pos {
			return list[i].Pos < list[j].Pos
		}
		return list[i].Name < list[j].Name
	})
}

// nextPosLocked: Position für einen neuen Kanal (ans Ende). h.mu muss gehalten werden.
func (h *Hub) nextPosLocked() int {
	max := 0
	for _, r := range h.rooms {
		if r.kind == "" && r.pos > max {
			max = r.pos
		}
	}
	return max + 1
}

// LayoutInput: komplette Anordnung aus dem Admin-Bereich.
type LayoutInput struct {
	Groups []struct {
		ID    string   `json:"id"` // leer = neue Gruppe
		Name  string   `json:"name"`
		Rooms []string `json:"rooms"`
	} `json:"groups"`
	Ungrouped []string `json:"ungrouped"` // Kanäle ohne Gruppe (oben), in dieser Reihenfolge
}

// Layout ist die aktuelle Anordnung.
type Layout struct {
	Groups []struct {
		RoomGroup
		Rooms []string `json:"rooms"`
	} `json:"groups"`
	Ungrouped []string `json:"ungrouped"`
}

func (h *Hub) Layout() Layout {
	groups := h.groups.List()
	rooms := h.Rooms()
	var l Layout
	l.Ungrouped = []string{}
	idx := map[string]int{}
	for i, g := range groups {
		idx[g.ID] = i
		l.Groups = append(l.Groups, struct {
			RoomGroup
			Rooms []string `json:"rooms"`
		}{g, []string{}})
	}
	for _, r := range rooms {
		if i, ok := idx[r.Group]; ok {
			l.Groups[i].Rooms = append(l.Groups[i].Rooms, r.Name)
		} else {
			l.Ungrouped = append(l.Ungrouped, r.Name)
		}
	}
	if l.Groups == nil {
		l.Groups = []struct {
			RoomGroup
			Rooms []string `json:"rooms"`
		}{}
	}
	return l
}

// SetLayout übernimmt Gruppen und Reihenfolge. Nicht genannte Kanäle landen am Ende ohne Gruppe.
func (h *Hub) SetLayout(in LayoutInput) (Layout, error) {
	if len(in.Groups) > maxGroups {
		return Layout{}, ErrTooGroups
	}
	type place struct {
		group string
		pos   int
	}
	var groups []RoomGroup
	groupOf := map[string]string{} // Kanal → Gruppe (nur Kanäle in Gruppen)
	var groupOrder []string        // Kanäle in Gruppen, in Reihenfolge
	seen := map[string]bool{}
	for _, g := range in.Groups {
		name := strings.Join(strings.Fields(g.Name), " ")
		if name == "" || len([]rune(name)) > maxGroupName {
			return Layout{}, ErrGroupName
		}
		id := g.ID
		if id == "" || seen[id] || !strings.HasPrefix(id, "g_") {
			id = "g_" + randomHex(5)
		}
		seen[id] = true
		groups = append(groups, RoomGroup{ID: id, Name: name})
		for _, n := range g.Rooms {
			n = NormRoom(n)
			if _, ok := groupOf[n]; !ok {
				groupOf[n] = id
				groupOrder = append(groupOrder, n)
			}
		}
	}
	h.groups.mu.Lock()
	h.groups.list = groups
	h.groups.mu.Unlock()
	h.touchDoc("groups.json")

	h.mu.Lock()
	// Reihenfolge: erst die Kanäle ohne Gruppe (genannte, dann nicht genannte in bisheriger
	// Reihenfolge), danach die Gruppen. So stimmen Positionen und Anzeige überein.
	var all []string
	for n, r := range h.rooms {
		if r.kind == "" {
			all = append(all, n)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := h.rooms[all[i]], h.rooms[all[j]]
		if a.pos != b.pos {
			return a.pos < b.pos
		}
		return a.name < b.name
	})
	places := map[string]place{}
	pos := 1
	add := func(n, g string) {
		if _, ok := h.rooms[n]; !ok {
			return
		}
		if _, ok := places[n]; ok {
			return
		}
		places[n] = place{g, pos}
		pos++
	}
	for _, n := range in.Ungrouped {
		if _, grouped := groupOf[NormRoom(n)]; !grouped {
			add(NormRoom(n), "")
		}
	}
	for _, n := range all {
		if _, grouped := groupOf[n]; !grouped {
			add(n, "")
		}
	}
	for _, n := range groupOrder {
		add(n, groupOf[n])
	}
	for n, p := range places {
		r := h.rooms[n]
		if r.group != p.group || r.pos != p.pos {
			r.group, r.pos = p.group, p.pos
			h.record(storeOp{kind: opRoomMeta, room: n})
		}
	}
	h.broadcastAll(Event{Type: "layout"})
	h.mu.Unlock()
	return h.Layout(), nil
}

// pruneGroupLocked: Kanäle einer unbekannten Gruppe gelten als ohne Gruppe (beim Laden).
func (h *Hub) groupOf(r *room) string {
	if r.group != "" && h.groups.has(r.group) {
		return r.group
	}
	return ""
}

// --- HTTP ---

// GET /api/groups – Gruppen in Reihenfolge (öffentlich; leere Gruppen blendet die Oberfläche aus)
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Groups().List())
}

// GET/PUT /api/admin/layout
func (s *Server) handleLayoutGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Layout())
}

func (s *Server) handleLayoutPut(w http.ResponseWriter, r *http.Request) {
	var in LayoutInput
	if !decodeJSON(w, r, &in) {
		return
	}
	l, err := s.hub.SetLayout(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "room.layout", Detail: strings.Join(groupNames(l), ", ")})
	writeJSON(w, http.StatusOK, l)
}

func groupNames(l Layout) []string {
	out := []string{}
	for _, g := range l.Groups {
		out = append(out, g.Name)
	}
	return out
}

// PUT /api/profile/pins {"rooms": […]} – eigene angepinnte Kanäle
func (s *Server) handlePinsPut(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	var in struct {
		Rooms []string `json:"rooms"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var list []string
	seen := map[string]bool{}
	for _, n := range in.Rooms {
		n = NormRoom(n)
		if !ValidRoom(n) || isDMName(n) || seen[n] {
			continue
		}
		seen[n] = true
		list = append(list, n)
		if len(list) >= maxUserPins {
			break
		}
	}
	if err := s.users.SetPinned(me.ID, list); err != nil {
		fail(w, err)
		return
	}
	if list == nil {
		list = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": list})
}
