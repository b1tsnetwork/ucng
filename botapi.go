// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Bot-API – alle Aufrufe mit  Authorization: Bearer gcb_…
//
//   GET  /api/bot/me                          eigener Bot: Name, Kanäle
//   GET  /api/bot/rooms                       erlaubte Kanäle
//   GET  /api/bot/rooms/{room}/messages       Verlauf (?before=&limit= wie bei Nutzern)
//   POST /api/bot/rooms/{room}/messages       {"text": "…"} schreiben
//   GET  /api/bot/events                      Live-Stream (Server-Sent Events)
//
// Nur mit Moderationsrecht (im Admin-Bereich freigeschaltet):
//   DELETE /api/bot/rooms/{room}/messages/{id}  Nachricht moderativ löschen
//   POST   /api/bot/users/{id}/mute             {"minutes": 10, "reason": "…"} stummschalten
// ---------------------------------------------------------------------------

type botCtxKey struct{}

func botFrom(r *http.Request) Bot { return r.Context().Value(botCtxKey{}).(Bot) }

func (s *Server) requireBot(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, ok := s.hub.Bots().Auth(bearer(r))
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Ungültiger oder deaktivierter Bot-Schlüssel")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), botCtxKey{}, b)))
	}
}

type botSelf struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AllRooms    bool     `json:"allRooms"`
	Rooms       []string `json:"rooms"`
	CanModerate bool     `json:"canModerate"`
	Perms       BotPerms `json:"perms"`
}

func selfOf(b Bot) botSelf {
	p := b.P()
	return botSelf{ID: b.ID, Name: b.Name, AllRooms: b.AllRooms, Rooms: append([]string{}, b.Rooms...), CanModerate: p.Moderates(), Perms: p}
}

var (
	errBotNoMod      = errors.New("Dieser Bot darf nicht moderieren")
	errBotNoPerm     = errors.New("Dieser Bot hat dafür keine Berechtigung")
	errBotNoWrite    = errors.New("Dieser Bot darf nicht schreiben")
	errBotOwnSanct   = errors.New("Bots können nur Sperren aufheben, die sie selbst verhängt haben")
	errBotRate       = errors.New("Zu viele Nachrichten von diesem Bot – bitte kurz warten")
	errBotOutOfReach = errors.New("Dieser Bot darf nur Personen sperren, die in seinen Kanälen geschrieben haben")
)

func botAudit(b Bot, e AuditEntry) AuditEntry {
	e.Actor, e.Display = "bot:"+b.ID, b.Name+" (Bot)"
	return e
}

func botLabel(b Bot) string { return b.Name + " (Bot)" }

// botRoom prüft, ob der Bot den Kanal nutzen darf, und liefert den Namen.
func botRoom(w http.ResponseWriter, r *http.Request) (Bot, string, bool) {
	b, room := botFrom(r), NormRoom(r.PathValue("room"))
	if !b.Allowed(room) || isDMName(room) { // Direktnachrichten sind für Bots tabu
		fail(w, ErrBotRoom)
		return b, room, false
	}
	return b, room, true
}

func botPoster(b Bot) Poster {
	return Poster{ID: b.ID, Name: b.Name, Bot: true, NoAutomod: b.P().AutomodExempt}
}

func (s *Server) handleBotMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, selfOf(botFrom(r)))
}

func (s *Server) handleBotRooms(w http.ResponseWriter, r *http.Request) {
	b := botFrom(r)
	out := []RoomInfo{}
	for _, ri := range s.hub.Rooms() {
		if b.Allowed(ri.Name) {
			out = append(out, ri)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/bot/rooms/{room}/messages
func (s *Server) handleBotHistory(w http.ResponseWriter, r *http.Request) {
	b, room, ok := botRoom(w, r)
	if !ok {
		return
	}
	if !b.P().History {
		fail(w, errBotNoPerm)
		return
	}
	before, limit := pageParams(r)
	msgs, more, err := s.hub.History(room, before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("X-Has-More", boolStr(more))
	writeJSON(w, http.StatusOK, msgs)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// GET /api/bot/rooms/{room}/presence
func (s *Server) handleBotPresence(w http.ResponseWriter, r *http.Request) {
	b, room, ok := botRoom(w, r)
	if !ok {
		return
	}
	if !b.P().Presence {
		fail(w, errBotNoPerm)
		return
	}
	ev, err := s.hub.Presence(room)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// POST /api/bot/rooms/{room}/messages {"text": "…", "replyTo": 123}
func (s *Server) handleBotPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text    string `json:"text"`
		ReplyTo int64  `json:"replyTo"`
		Thread  int64  `json:"thread"`
		GIF     string `json:"gif"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	var media *Media
	if in.GIF != "" {
		m, err := s.hub.Media().ResolveGIF(in.GIF)
		if err != nil {
			fail(w, err)
			return
		}
		media = m
	}
	msg, err := s.botSendIn(botFrom(r), NormRoom(r.PathValue("room")), in.Text, in.ReplyTo, in.Thread, media)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// botSay schreibt als Bot – für die REST-API und für Webhook-Antworten.
func (s *Server) botSay(b Bot, room, text string, replyTo int64) (Message, error) {
	return s.botSend(b, room, text, replyTo, nil)
}

func (s *Server) botSend(b Bot, room, text string, replyTo int64, media *Media) (Message, error) {
	return s.botSendIn(b, room, text, replyTo, 0, media)
}

// botSendIn: wie botSend, optional in einem Thread. Antwortet der Bot auf eine Nachricht in
// einem Thread, landet die Antwort automatisch im selben Thread.
func (s *Server) botSendIn(b Bot, room, text string, replyTo, thread int64, media *Media) (Message, error) {
	p := b.P()
	if !p.Write {
		return Message{}, errBotNoWrite
	}
	if !b.Allowed(room) || isDMName(room) {
		return Message{}, ErrBotRoom
	}
	if thread == 0 && replyTo > 0 {
		if m, ok := s.hub.Message(room, replyTo); ok && m.Thread > 0 {
			thread = m.Thread
		}
	}
	limit := p.RateLimit
	if limit <= 0 {
		limit = s.cfg.BotRate
	}
	if !s.botRL.AllowN(b.ID, limit) {
		return Message{}, errBotRate
	}
	return s.hub.SendFull(room, botPoster(b), SendInput{Text: text, ReplyTo: replyTo, Thread: thread, Media: media})
}

// PATCH /api/bot/rooms/{room}/messages/{id} {"text": "…"} – eigene Nachricht bearbeiten.
func (s *Server) handleBotEdit(w http.ResponseWriter, r *http.Request) {
	b, room, ok := botRoom(w, r)
	if !ok {
		return
	}
	if !b.P().EditOwn {
		fail(w, errBotNoPerm)
		return
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	msg, err := s.hub.EditOwn(room, id, botPoster(b), in.Text)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// DELETE /api/bot/rooms/{room}/messages/{id} – eigene Nachricht löschen (Recht „eigene bearbeiten“)
// oder fremde moderativ löschen (Recht „Nachrichten löschen“).
func (s *Server) handleBotDelete(w http.ResponseWriter, r *http.Request) {
	b, room, ok := botRoom(w, r)
	if !ok {
		return
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	p := b.P()
	if p.EditOwn {
		if msg, err := s.hub.DeleteOwnBot(room, id, b.ID); err == nil {
			writeJSON(w, http.StatusOK, msg)
			return
		} else if !errors.Is(err, ErrNotOwn) {
			fail(w, err)
			return
		}
	}
	if !p.Delete {
		fail(w, errBotNoMod)
		return
	}
	msg, err := s.hub.ModDelete(room, id, "bot", b.Name)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: "message.delete", Room: room, Message: id,
		UserID: msg.UserID, UserName: msg.Name, Detail: snippet(msg.Original, 80)}))
	writeJSON(w, http.StatusOK, msg.public())
}

// POST /api/bot/rooms/{room}/messages/{id}/react {"emoji": "👍"}
func (s *Server) handleBotReact(w http.ResponseWriter, r *http.Request) {
	b, room, ok := botRoom(w, r)
	if !ok {
		return
	}
	if !b.P().React {
		fail(w, errBotNoPerm)
		return
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	var in struct {
		Emoji string `json:"emoji"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	msg, err := s.hub.React(room, id, botPoster(b), in.Emoji)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// POST /api/bot/users/{id}/mute bzw. /ban {"minutes": 10, "reason": "…", "name": "…"}
func (s *Server) handleBotMute(w http.ResponseWriter, r *http.Request) { s.botSanction(w, r, "mute") }
func (s *Server) handleBotBan(w http.ResponseWriter, r *http.Request)  { s.botSanction(w, r, "ban") }

func (s *Server) botSanction(w http.ResponseWriter, r *http.Request, kind string) {
	b := botFrom(r)
	p := b.P()
	if (kind == "mute" && !p.Mute) || (kind == "ban" && !p.Ban) {
		fail(w, errBotNoMod)
		return
	}
	var in struct {
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
		Name    string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Minutes < 1 || in.Minutes > p.MaxMinutes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("Dieser Bot darf 1 bis %d Minuten sperren", p.MaxMinutes))
		return
	}
	id := r.PathValue("id")
	if !b.AllRooms && !s.hub.HasPostedIn(id, b.Allowed) {
		fail(w, errBotOutOfReach)
		return
	}
	name := in.Name
	if u, ok := s.users.Get(id); ok {
		if u.Team {
			writeErr(w, http.StatusForbidden, "Team-Mitglieder können nicht gesperrt werden")
			return
		}
		name = u.Name
	} else if name == "" {
		fail(w, ErrNoUser)
		return
	}
	until := time.Now().Add(time.Duration(in.Minutes) * time.Minute)
	if cur, ok := s.hub.Mod().Active(id, ""); ok && (sanctionRank(cur.Kind) > sanctionRank(kind) ||
		(cur.Kind == kind && (cur.Until == nil || cur.Until.After(until)))) {
		writeJSON(w, http.StatusOK, map[string]any{"kind": cur.Kind, "until": cur.Until, "unchanged": true})
		return // eine strengere Sperre bleibt bestehen
	}
	sa, err := s.hub.Mod().SanctionBot(id, name, kind, in.Minutes, in.Reason, botLabel(b), b.ID)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.NotifySanction(id)
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: "user." + kind, UserID: id, UserName: name, Detail: sanctionDetail(sa, s.hub.Location())}))
	writeJSON(w, http.StatusOK, map[string]any{"kind": sa.Kind, "until": sa.Until})
}

func sanctionRank(kind string) int {
	if kind == "ban" {
		return 2
	}
	return 1
}

// DELETE /api/bot/users/{id}/sanction – nur Sperren, die dieser Bot verhängt hat.
func (s *Server) handleBotLift(w http.ResponseWriter, r *http.Request) {
	b := botFrom(r)
	p := b.P()
	if !p.Mute && !p.Ban {
		fail(w, errBotNoMod)
		return
	}
	id := r.PathValue("id")
	cur, ok := s.hub.Mod().Active(id, "")
	if !ok {
		fail(w, ErrNoSanction)
		return
	}
	if cur.ByBot != b.ID {
		fail(w, errBotOwnSanct)
		return
	}
	sa, err := s.hub.Mod().Lift(id)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.NotifySanction(id)
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: "user.unsanction", UserID: id, UserName: sa.Name}))
	w.WriteHeader(http.StatusNoContent)
}

// botEvent ist das Format für Live-Stream und Webhooks.
type botEvent struct {
	Event   string   `json:"event"` // ready, message, update, removed, revoked
	Room    string   `json:"room,omitempty"`
	Message *Message `json:"message,omitempty"`
	Bot     *botSelf `json:"bot,omitempty"`
}

// wants: soll der Bot dieses Ereignis bekommen?
func (b Bot) wants(ev BusEvent) bool {
	if ev.Message == nil || !b.Allowed(ev.Room) || ev.Message.UserID == b.ID {
		return false // fremder Kanal oder eigene Nachricht
	}
	if ev.Message.Bot && !b.ReceiveBots {
		return false // Nachrichten anderer Bots nur auf Wunsch (verhindert Bot-Schleifen)
	}
	if ev.Type != "removed" && b.P().Receive == BotReceiveCommands && !addressedTo(b, ev.Message) {
		return false
	}
	return true
}

// addressedTo: Ist die Nachricht ein Befehl oder an den Bot gerichtet?
func addressedTo(b Bot, m *Message) bool {
	t := strings.TrimSpace(m.Text)
	if strings.HasPrefix(t, "!") || strings.HasPrefix(t, "/") {
		return true
	}
	if Mentions(t, b.Name) {
		return true
	}
	return m.Reply != nil && strings.EqualFold(m.Reply.Name, b.Name)
}

// GET /api/bot/events – Live-Stream aller Nachrichten in den erlaubten Kanälen.
func (s *Server) handleBotEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "Streaming nicht unterstützt")
		return
	}
	b := botFrom(r)
	release, ok := s.streamSlot(w, "bot:"+b.ID)
	if !ok {
		return
	}
	defer release()
	bots := s.hub.Bots()
	ch := s.hub.Listen(256)
	defer s.hub.Unlisten(ch)
	bots.connect(b.ID, +1)
	defer bots.connect(b.ID, -1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(ev botEvent) bool {
		data, _ := json.Marshal(ev)
		if _, err := w.Write([]byte("event: " + ev.Event + "\ndata: " + string(data) + "\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	self := selfOf(b)
	w.Write([]byte("retry: 3000\n\n"))
	if !send(botEvent{Event: "ready", Bot: &self}) {
		return
	}

	// Rechte können sich ändern oder der Bot wird gelöscht/deaktiviert – regelmäßig nachsehen.
	refresh := func() bool {
		cur, ok := bots.Get(b.ID)
		if !ok || !cur.Enabled {
			send(botEvent{Event: "revoked"})
			return false
		}
		b = cur
		return true
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	check := time.NewTicker(3 * time.Second) // deaktivierte oder gelöschte Bots zügig trennen
	defer check.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-check.C:
			if !refresh() {
				return
			}
		case <-ping.C:
			if !refresh() {
				return
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-ch:
			if !refresh() {
				return
			}
			if b.wants(ev) && !send(botEvent{Event: ev.Type, Room: ev.Room, Message: ev.Message}) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Admin: Bots verwalten
// ---------------------------------------------------------------------------

func (s *Server) handleAdminBots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Bots().List())
}

func (s *Server) handleAdminCreateBot(w http.ResponseWriter, r *http.Request) {
	var in BotInput
	if !decodeJSON(w, r, &in) {
		return
	}
	info, token, err := s.hub.Bots().Create(in)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bot": info, "token": token})
}

func (s *Server) handleAdminUpdateBot(w http.ResponseWriter, r *http.Request) {
	var in BotInput
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	info, renamed, err := s.hub.Bots().Update(id, in)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if renamed {
		s.hub.RenameUser(id, info.Name) // bisherige Bot-Nachrichten tragen den neuen Namen
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleAdminDeleteBot(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.Bots().Delete(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminRenewBot(w http.ResponseWriter, r *http.Request) {
	token, secret, err := s.hub.Bots().Renew(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "webhookSecret": secret})
}

// POST /api/admin/bots/{id}/test – schickt ein „ping“ an die Webhook-URL und meldet das Ergebnis.
func (s *Server) handleAdminTestBot(w http.ResponseWriter, r *http.Request) {
	b, ok := s.hub.Bots().Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, ErrNoBot.Error())
		return
	}
	if strings.TrimSpace(b.WebhookURL) == "" {
		writeErr(w, http.StatusBadRequest, "Für diesen Bot ist keine Webhook-URL eingetragen")
		return
	}
	self := selfOf(b)
	d, _ := s.webhooks.deliver(r.Context(), b, botEvent{Event: "ping", Bot: &self})
	writeJSON(w, http.StatusOK, d)
}
