// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Umfragen: eine Nachricht mit Frage und 2–10 Antworten; Abstimmen per Klick, live für alle.

type Poll struct {
	Question string       `json:"question"`
	Options  []PollOption `json:"options"`
	Multi    bool         `json:"multi,omitempty"`  // mehrere Antworten erlaubt
	Closed   bool         `json:"closed,omitempty"` // beendet
}

type PollOption struct {
	Text  string   `json:"text"`
	Votes []string `json:"votes"` // Nutzer-IDs
}

var (
	ErrPollFormat = errors.New("Eine Umfrage braucht eine Frage und 2 bis 10 Antworten.")
	ErrPollLen    = errors.New("Frage höchstens 200, Antworten höchstens 100 Zeichen.")
	ErrNoPoll     = errors.New("Das ist keine Umfrage")
	ErrPollClosed = errors.New("Diese Umfrage ist beendet")
	ErrPollOption = errors.New("Diese Antwort gibt es nicht")
)

// ParsePoll liest "Frage? | A | B" (mit "mehrfach:" davor: mehrere Antworten erlaubt).
func ParsePoll(args string) (*Poll, error) {
	args = strings.TrimSpace(args)
	multi := false
	for _, pre := range []string{"mehrfach:", "multi:"} {
		if strings.HasPrefix(strings.ToLower(args), pre) {
			multi, args = true, strings.TrimSpace(args[len(pre):])
		}
	}
	parts := strings.Split(args, "|")
	if len(parts) < 3 || len(parts) > 11 {
		return nil, ErrPollFormat
	}
	p := &Poll{Question: strings.Join(strings.Fields(parts[0]), " "), Multi: multi}
	if p.Question == "" || utf8.RuneCountInString(p.Question) > 200 {
		return nil, ErrPollLen
	}
	seen := map[string]bool{}
	for _, o := range parts[1:] {
		o = strings.Join(strings.Fields(o), " ")
		if o == "" || seen[strings.ToLower(o)] {
			continue
		}
		if utf8.RuneCountInString(o) > 100 {
			return nil, ErrPollLen
		}
		seen[strings.ToLower(o)] = true
		p.Options = append(p.Options, PollOption{Text: o, Votes: []string{}})
	}
	if len(p.Options) < 2 {
		return nil, ErrPollFormat
	}
	return p, nil
}

func (p *Poll) text() string {
	parts := []string{p.Question}
	for _, o := range p.Options {
		parts = append(parts, o.Text)
	}
	return strings.Join(parts, "\n")
}

func (p *Poll) clone() *Poll {
	c := *p
	c.Options = make([]PollOption, len(p.Options))
	for i, o := range p.Options {
		c.Options[i] = PollOption{Text: o.Text, Votes: append([]string{}, o.Votes...)}
	}
	return &c
}

// Vote setzt oder entfernt eine Stimme (bei nur einer erlaubten Antwort wird umgestellt).
func (h *Hub) Vote(roomName string, id int64, p Poster, option int) (Message, error) {
	if err := h.checkSanction(p); err != nil {
		return Message{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	if !r.canRead(accessOf(p)) {
		return Message{}, ErrNoRoom
	}
	if m.Poll == nil || m.Deleted {
		return Message{}, ErrNoPoll
	}
	if m.Poll.Closed {
		return Message{}, ErrPollClosed
	}
	if option < 0 || option >= len(m.Poll.Options) {
		return Message{}, ErrPollOption
	}
	poll := m.Poll.clone()
	had := contains(poll.Options[option].Votes, p.ID)
	for i := range poll.Options {
		if i == option || !poll.Multi {
			poll.Options[i].Votes = removeStr(poll.Options[i].Votes, p.ID)
		}
	}
	if !had {
		poll.Options[option].Votes = append(poll.Options[option].Votes, p.ID)
	}
	m.Poll = poll
	h.afterChangeLocked(r, m)
	return h.out(r, *m, false), nil
}

// ClosePoll beendet eine Umfrage (Ersteller, Team oder Personen mit Löschrecht).
func (h *Hub) ClosePoll(roomName string, id int64, p Poster, open bool) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	if m.Poll == nil {
		return Message{}, ErrNoPoll
	}
	if m.UserID != p.ID && !p.Perms.Delete {
		return Message{}, ErrNotOwn
	}
	poll := m.Poll.clone()
	poll.Closed = !open
	m.Poll = poll
	h.afterChangeLocked(r, m)
	return h.out(r, *m, false), nil
}

func removeStr(list []string, s string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// POST /api/rooms/{room}/messages/{id}/vote {"option": 1} oder {"close": true} / {"reopen": true}
func (s *Server) handleVote(w http.ResponseWriter, r *http.Request) {
	me, ip, room, id, ok := s.meIn(w, r)
	if !ok {
		return
	}
	if !s.rl.Allow("vote:" + s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Klicks – bitte kurz warten.")
		return
	}
	var in struct {
		Option *int `json:"option"`
		Close  bool `json:"close"`
		Reopen bool `json:"reopen"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	p := s.posterOf(me, ip)
	var msg Message
	var err error
	switch {
	case in.Close || in.Reopen:
		msg, err = s.hub.ClosePoll(room, id, p, in.Reopen)
	case in.Option != nil:
		msg, err = s.hub.Vote(room, id, p, *in.Option)
	default:
		writeErr(w, http.StatusBadRequest, "option, close oder reopen fehlt")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// postPoll erstellt eine Umfrage aus einem Befehl (bei Fehlern privater Hinweis).
func (s *Server) postPoll(w http.ResponseWriter, room, args, cmd string, me Identity, ip string, replyTo, thread int64) {
	poll, err := ParsePoll(args)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ephemeral": EphemeralReply{Name: s.hub.Commands().BotName(), Notice: true,
			Text:    fmt.Sprintf("%s So geht's: %s Wann treffen wir uns? | Freitag | Samstag (mit „mehrfach:“ vor der Frage sind mehrere Antworten erlaubt)", err.Error(), cmd),
			Command: cmd + " " + args, Time: time.Now().UTC()}})
		return
	}
	msg, err := s.hub.SendFull(room, s.posterOf(me, ip), SendInput{ReplyTo: replyTo, Thread: thread, Poll: poll})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}
