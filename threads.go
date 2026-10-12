// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"net/http"
	"time"
)

// Threads: Antworten zu einer Nachricht als eigener, eingeklappter Verlauf.

type ThreadSummary struct {
	Count  int       `json:"count"`
	LastAt time.Time `json:"lastAt"`
	Names  []string  `json:"names"` // bis zu 3 zuletzt Antwortende
}

var ErrThreadNested = errors.New("In einem Thread kann kein weiterer Thread entstehen")

func (r *room) addToThread(m Message) {
	if r.threads == nil {
		r.threads = map[int64]*ThreadSummary{}
	}
	t := r.threads[m.Thread]
	if t == nil {
		t = &ThreadSummary{}
		r.threads[m.Thread] = t
	}
	t.Count++
	if m.Time.After(t.LastAt) {
		t.LastAt = m.Time
	}
	if !m.Deleted {
		names := []string{m.Name}
		for _, n := range t.Names {
			if n != m.Name && len(names) < 3 {
				names = append(names, n)
			}
		}
		t.Names = names
	}
}

func (r *room) rebuildThreads() {
	r.threads = nil
	for _, m := range r.history {
		if m.Thread > 0 {
			r.addToThread(m)
		}
	}
}

// Thread liefert die Ausgangsnachricht und alle Antworten.
func (h *Hub) Thread(roomName string, id int64, a *Access) (Message, []Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, err := h.readableLocked(roomName, a)
	if err != nil {
		return Message{}, nil, err
	}
	i, ok := r.find(id)
	if !ok {
		return Message{}, nil, ErrNoMessage
	}
	root := r.history[i]
	floor := int64(0)
	if a != nil {
		floor = r.dmFloor(a.ID)
	}
	if root.Thread != 0 || root.ID <= floor {
		return Message{}, nil, ErrNoMessage
	}
	out := []Message{}
	for j := i + 1; j < len(r.history); j++ {
		if r.history[j].Thread == id && r.history[j].ID > floor {
			out = append(out, h.out(r, r.history[j], false))
		}
	}
	return h.out(r, root, false), out, nil
}

// GET /api/rooms/{room}/threads/{id}
func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	root, msgs, err := s.hub.Thread(NormRoom(r.PathValue("room")), id, s.accessOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root": root, "messages": msgs})
}
