// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "errors"

// Angepinnte Nachrichten: wichtige Infos oben im Kanal (höchstens 10 je Kanal).

const maxPins = 10

var (
	ErrTooManyPins = errors.New("Höchstens 10 angepinnte Nachrichten je Kanal – bitte erst eine lösen")
	ErrNoPerm      = errors.New("Dafür fehlt dir das Recht")
)

// pinnedLocked liefert die angepinnten Nachrichten (öffentliche Fassung, neueste zuerst).
func (h *Hub) pinnedLocked(r *room) []Message {
	out := []Message{}
	for i := len(r.pins) - 1; i >= 0; i-- {
		if j, ok := r.find(r.pins[i]); ok {
			out = append(out, h.out(r, r.history[j], false))
		}
	}
	return out
}

// Pinned liefert die angepinnten Nachrichten eines Kanals.
func (h *Hub) Pinned(roomName string, a *Access) ([]Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, err := h.readableLocked(roomName, a)
	if err != nil {
		return nil, err
	}
	return h.pinnedLocked(r), nil
}

// PinnedAll ohne Zugriffsprüfung (Admin-Bereich).
func (h *Hub) PinnedAll(roomName string) ([]Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return nil, ErrNoRoom
	}
	return h.pinnedLocked(r), nil
}

// SetPin pinnt eine Nachricht an oder löst sie.
func (h *Hub) SetPin(roomName string, id int64, pin bool) ([]Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, p := range r.pins {
		if p == id {
			idx = i
		}
	}
	switch {
	case pin && idx >= 0:
		return h.pinnedLocked(r), nil
	case pin:
		if m.Deleted {
			return nil, ErrMsgDeleted
		}
		if len(r.pins) >= maxPins {
			return nil, ErrTooManyPins
		}
		r.pins = append(r.pins, id)
	case idx >= 0:
		r.pins = append(r.pins[:idx:idx], r.pins[idx+1:]...)
	default:
		return h.pinnedLocked(r), nil
	}
	h.record(storeOp{kind: opRoomMeta, room: r.name})
	list := h.pinnedLocked(r)
	h.broadcast(r, Event{Type: "pins", Messages: list, Online: len(r.clients)})
	return list, nil
}

// prunePinsLocked entfernt Pins endgültig gelöschter Nachrichten. h.mu muss gehalten werden.
func (h *Hub) prunePinsLocked(r *room) {
	keep := r.pins[:0]
	for _, id := range r.pins {
		if _, ok := r.find(id); ok {
			keep = append(keep, id)
		}
	}
	r.pins = keep
}
