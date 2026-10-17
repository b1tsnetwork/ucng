// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Breakout-Räume: Wer im Sprachkanal moderiert, teilt die Anwesenden auf mehrere temporäre
// Kanäle auf (gleiche Einstellungen und Zugangsregeln wie der Kanal selbst). Optional mit
// Countdown – danach (oder per „Alle zurückholen“) kommen alle in den Hauptkanal zurück. Die
// Räume löschen sich danach wie jeder temporäre Kanal von selbst.

var (
	ErrBreakoutGroups = errors.New("2 bis 20 Gruppen")
	ErrBreakoutTime   = errors.New("Dauer: 0 (ohne) bis 240 Minuten")
	ErrBreakoutFew    = errors.New("Zu wenige Personen im Sprachkanal zum Aufteilen")
	ErrBreakoutBusy   = errors.New("Es läuft schon eine Breakout-Runde – zuerst alle zurückholen")
	ErrBreakoutNone   = errors.New("Gerade läuft keine Breakout-Runde")
)

// BreakoutInfo: eine laufende Breakout-Runde (in Events und in der Antwort beim Beitreten).
type BreakoutInfo struct {
	Parent string   `json:"parent"`
	Rooms  []string `json:"rooms"`
	Ends   int64    `json:"ends,omitempty"` // Unix-Millisekunden; 0 = ohne Countdown
	By     string   `json:"by,omitempty"`
	Ended  bool     `json:"ended,omitempty"`
}

// breakoutFor: Runde, zu der ein Kanal gehört (als Hauptkanal oder als Gruppe).
func (v *voiceMgr) breakoutFor(room string) *BreakoutInfo {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, b := range v.breakouts {
		if b.Parent == room {
			cp := *b
			return &cp
		}
		for _, r := range b.Rooms {
			if r == room {
				cp := *b
				return &cp
			}
		}
	}
	return nil
}

func (v *voiceMgr) announceBreakout(b BreakoutInfo) {
	ev := Event{Type: "breakout", Room: b.Parent, Breakout: &b}
	v.s.hub.SendToReaders(b.Parent, ev)
	for _, r := range b.Rooms {
		v.s.hub.SendToReaders(r, ev)
	}
}

// startBreakout teilt die Anwesenden (außer Moderierenden) gleichmäßig und zufällig auf.
func (v *voiceMgr) startBreakout(parent string, groups, minutes int, me Identity) (BreakoutInfo, error) {
	var b BreakoutInfo
	if groups < 2 || groups > 20 {
		return b, ErrBreakoutGroups
	}
	if minutes < 0 || minutes > 240 {
		return b, ErrBreakoutTime
	}
	if v.breakoutFor(parent) != nil {
		return b, ErrBreakoutBusy
	}
	info, ok := v.s.hub.roomInfo(parent)
	if !ok || info.Voice == nil {
		return b, ErrNoVoice
	}
	var move []VoicePeer
	for _, p := range v.Peers(parent) {
		if !p.Mod && p.Music == 0 && p.User != me.ID {
			move = append(move, p)
		}
	}
	if len(move) < 2 {
		return b, ErrBreakoutFew
	}
	groups = min(groups, len(move))
	cfg := *info.Voice
	cfg.Stage, cfg.StageRoles = false, nil // in kleinen Gruppen darf jede Person sprechen
	b = BreakoutInfo{Parent: parent, By: me.Name}
	base := strings.TrimPrefix(parent, "t-")
	for i := 1; i <= groups; i++ {
		ri, err := v.s.hub.CreateTempRoom(TempRoomInput{
			Name: fmt.Sprintf("%s-gruppe-%d", base, i), Description: fmt.Sprintf("Breakout-Gruppe %d von #%s", i, parent),
			Owner: me.ID, OwnerName: me.Name, By: "breakout", Voice: &cfg, VoiceSet: true,
			ReadRoles: info.ReadRoles, WriteRoles: info.WriteRoles, Breakout: parent,
		})
		if err != nil {
			for _, r := range b.Rooms { // halb angelegt: wieder weg
				v.s.hub.DeleteRoom(r)
			}
			return BreakoutInfo{}, err
		}
		v.s.hub.copyRoomAccess(parent, ri.Name)
		b.Rooms = append(b.Rooms, ri.Name)
	}
	if minutes > 0 {
		b.Ends = time.Now().Add(time.Duration(minutes) * time.Minute).UnixMilli()
	}
	rand.Shuffle(len(move), func(i, j int) { move[i], move[j] = move[j], move[i] })
	v.mu.Lock()
	cp := b
	v.breakouts[parent] = &cp
	for i, p := range move {
		v.room(b.Rooms[i%groups]).movedIn[p.User] = time.Now().Add(2 * time.Minute)
	}
	v.mu.Unlock()
	v.announceBreakout(b)
	for i, p := range move {
		v.control(parent, p.User, "move", false, b.Rooms[i%groups])
	}
	return b, nil
}

// endBreakout holt alle aus den Gruppen in den Hauptkanal zurück.
func (v *voiceMgr) endBreakout(parent string) error {
	v.mu.Lock()
	b := v.breakouts[parent]
	if b == nil {
		v.mu.Unlock()
		return ErrBreakoutNone
	}
	delete(v.breakouts, parent)
	type mv struct{ from, user string }
	var moves []mv
	for _, r := range b.Rooms {
		if st := v.rooms[r]; st != nil {
			for _, p := range st.peers {
				moves = append(moves, mv{r, p.User})
				v.room(parent).movedIn[p.User] = time.Now().Add(2 * time.Minute)
			}
		}
	}
	done := *b
	v.mu.Unlock()
	done.Ended = true
	v.announceBreakout(done)
	for _, m := range moves {
		v.control(m.from, m.user, "move", false, parent)
	}
	return nil
}

// breakoutTick: abgelaufene Runden beenden (aus dem janitor).
func (v *voiceMgr) breakoutTick(now time.Time) {
	v.mu.Lock()
	var due []string
	for p, b := range v.breakouts {
		if b.Ends > 0 && now.UnixMilli() >= b.Ends {
			due = append(due, p)
		}
	}
	v.mu.Unlock()
	sort.Strings(due)
	for _, p := range due {
		v.endBreakout(p)
	}
}

// copyRoomAccess: Passwort (samt Freigaben) des Hauptkanals für eine Gruppe übernehmen.
func (h *Hub) copyRoomAccess(from, to string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, t := h.rooms[from], h.rooms[to]
	if f == nil || t == nil || f.passHash == "" {
		return
	}
	t.passHash = f.passHash
	t.unlocked = map[string]bool{}
	for u := range f.unlocked {
		t.unlocked[u] = true
	}
	h.record(storeOp{kind: opRoomMeta, room: to})
}

// POST /api/voice/{room}/breakout {"groups":3,"minutes":10} – starten
// DELETE /api/voice/{room}/breakout – alle zurückholen
func (s *Server) handleVoiceBreakout(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, ErrNoSession.Error())
		return
	}
	room := NormRoom(r.PathValue("room"))
	if !v.mayHost(room, me) {
		fail(w, ErrNoPerm)
		return
	}
	actor, display := chatActor(me)
	if r.Method == http.MethodDelete {
		if err := v.endBreakout(room); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice.breakout_end", Room: room})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var in struct {
		Groups  int `json:"groups"`
		Minutes int `json:"minutes"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	b, err := v.startBreakout(room, in.Groups, in.Minutes, me)
	if err != nil {
		var tm *TempMineError
		switch {
		case errors.Is(err, ErrBreakoutBusy):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrBreakoutGroups), errors.Is(err, ErrBreakoutTime), errors.Is(err, ErrBreakoutFew):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.As(err, &tm):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			voiceFail(w, err)
		}
		return
	}
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice.breakout", Room: room,
		Detail: fmt.Sprintf("%d Gruppen", len(b.Rooms))})
	writeJSON(w, http.StatusOK, b)
}

// mayHost: Darf me im Kanal moderieren (Team, Recht „Stummschalten“, Besitzer oder Co-Host)?
func (v *voiceMgr) mayHost(room string, me Identity) bool {
	who := v.s.voiceWhoOf(me, "")
	acc := v.s.hub.voiceAccessFull(room, who.access())
	if !acc.Exists || !acc.Read || acc.Cfg == nil {
		return false
	}
	if v.canModerate(who) || acc.Owner != "" && acc.Owner == me.ID {
		return true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.rooms[room] != nil && v.rooms[room].cohosts[me.ID]
}
