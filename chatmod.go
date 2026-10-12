// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Moderation direkt im Chat für Personen mit passenden Rollen-Rechten (Löschen, Stummschalten,
// Anpinnen) sowie Meldungen. Team-Mitglieder haben alle Rechte.

// Message liefert eine Nachricht (interne Fassung) – für Rechte-Prüfungen.
func (h *Hub) Message(roomName string, id int64) (Message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, false
	}
	return *m, true
}

// CanWrite: Darf a im Kanal schreiben?
func (h *Hub) CanWrite(roomName string, a *Access) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	return !ok || r.canWrite(a)
}

func chatActor(me Identity) (string, string) {
	return "user:" + me.ID, me.Name + " (Chat)"
}

// meIn: angemeldete Person mit Lesezugriff auf den Kanal und die Nachricht.
func (s *Server) meIn(w http.ResponseWriter, r *http.Request) (Identity, string, string, int64, bool) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return me, "", "", 0, false
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return me, "", "", 0, false
	}
	room := NormRoom(r.PathValue("room"))
	if !s.hub.CanRead(room, &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}) {
		fail(w, ErrNoRoom)
		return me, "", "", 0, false
	}
	return me, ip, room, id, true
}

// POST /api/rooms/{room}/messages/{id}/pin {"pin": true}
func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	me, ip, room, id, ok := s.meIn(w, r)
	if !ok {
		return
	}
	if !s.posterOf(me, ip).Perms.Pin {
		fail(w, ErrNoPerm)
		return
	}
	var in struct {
		Pin bool `json:"pin"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	list, err := s.hub.SetPin(room, id, in.Pin)
	if err != nil {
		fail(w, err)
		return
	}
	actor, display := chatActor(me)
	action := "message.pin"
	if !in.Pin {
		action = "message.unpin"
	}
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: action, Room: room, Message: id})
	writeJSON(w, http.StatusOK, list)
}

// DELETE /api/rooms/{room}/messages/{id}/mod – fremde Nachricht moderativ löschen.
func (s *Server) handleChatModDelete(w http.ResponseWriter, r *http.Request) {
	me, ip, room, id, ok := s.meIn(w, r)
	if !ok {
		return
	}
	if !s.posterOf(me, ip).Perms.Delete {
		fail(w, ErrNoPerm)
		return
	}
	target, found := s.hub.Message(room, id)
	if !found {
		fail(w, ErrNoMessage)
		return
	}
	if target.Team && !me.Team {
		writeErr(w, http.StatusForbidden, "Nachrichten des Teams kann nur das Team löschen")
		return
	}
	msg, err := s.hub.ModDelete(room, id, "mod", me.Name)
	if err != nil {
		fail(w, err)
		return
	}
	actor, display := chatActor(me)
	s.hub.Reports().CloseMessage(room, id, "delete", display)
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "message.delete", Room: room, Message: id,
		UserID: msg.UserID, UserName: msg.Name, Detail: snippet(msg.Original, 80)})
	writeJSON(w, http.StatusOK, msg.public())
}

// POST /api/users/{id}/mute {"minutes": 10, "reason": "…"} – im Chat stummschalten (höchstens 1 Stunde).
func (s *Server) handleChatMute(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	if !s.posterOf(me, ip).Perms.Mute {
		fail(w, ErrNoPerm)
		return
	}
	var in struct {
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Minutes < 1 || in.Minutes > 60 {
		writeErr(w, http.StatusBadRequest, "Im Chat geht Stummschalten für 1 bis 60 Minuten – länger im Admin-Bereich")
		return
	}
	id := r.PathValue("id")
	u, found := s.users.Get(id)
	if !found {
		fail(w, ErrNoUser)
		return
	}
	if id == me.ID {
		writeErr(w, http.StatusBadRequest, "Dich selbst kannst du nicht stummschalten")
		return
	}
	if u.Team || s.hub.Roles().PermsOf(u.Roles, false).Mute && !me.Team {
		writeErr(w, http.StatusForbidden, "Team-Mitglieder und andere Moderierende kann nur das Team sperren")
		return
	}
	until := time.Now().Add(time.Duration(in.Minutes) * time.Minute)
	if cur, ok := s.hub.Mod().Active(id, ""); ok && (cur.Kind == "ban" || cur.Until == nil || cur.Until.After(until)) {
		writeJSON(w, http.StatusOK, map[string]any{"kind": cur.Kind, "until": cur.Until, "unchanged": true})
		return
	}
	actor, display := chatActor(me)
	sa, err := s.hub.Mod().Sanction(id, u.Name, "mute", in.Minutes, strings.TrimSpace(in.Reason), display, false)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.NotifySanction(id)
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "user.mute", UserID: id, UserName: u.Name,
		Detail: sanctionDetail(sa, s.hub.Location())})
	writeJSON(w, http.StatusOK, map[string]any{"kind": sa.Kind, "until": sa.Until})
}

// POST /api/rooms/{room}/messages/{id}/report {"reason": "…"}
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	me, _, room, id, ok := s.meIn(w, r)
	if !ok {
		return
	}
	if !s.reportRL.Allow(me.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Meldungen – bitte kurz warten.")
		return
	}
	var in struct {
		Reason    string `json:"reason"`
		Plaintext string `json:"plaintext"` // nur bei verschlüsselten Nachrichten
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	target, found := s.hub.Message(room, id)
	if !found || target.Deleted {
		fail(w, ErrNoMessage)
		return
	}
	if target.UserID == me.ID {
		fail(w, ErrReportOwn)
		return
	}
	reason := strings.Join(strings.Fields(in.Reason), " ")
	plain := ""
	if target.E2E != "" {
		plain, _ = cleanText(in.Plaintext, maxTextLen)
	}
	rep, err := s.hub.Reports().AddE2E(room, id, me.ID, me.Name, reason, plain)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": rep.ID})
}

// --- Admin: Meldungen ---

type adminReport struct {
	Report
	Message *Message `json:"message,omitempty"`
	Count   int      `json:"count"` // Meldungen zur selben Nachricht
}

// GET /api/admin/reports?status=open|done
func (s *Server) handleAdminReports(w http.ResponseWriter, r *http.Request) {
	open := r.URL.Query().Get("status") != "done"
	list := s.hub.Reports().List(open)
	counts := map[string]int{}
	for _, x := range list {
		counts[fmt.Sprintf("%s/%d", x.Room, x.MessageID)]++
	}
	out := []adminReport{}
	seen := map[string]bool{}
	for _, x := range list {
		key := fmt.Sprintf("%s/%d", x.Room, x.MessageID)
		if open && seen[key] { // offene Meldungen je Nachricht zusammenfassen
			continue
		}
		seen[key] = true
		ar := adminReport{Report: x, Count: counts[key]}
		if m, ok := s.hub.AdminMessage(x.Room, x.MessageID); ok {
			ar.Message = &m
		}
		out = append(out, ar)
		if len(out) >= 200 {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/admin/reports/count
func (s *Server) handleAdminReportCount(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"open": s.hub.Reports().OpenCount()})
}

// POST /api/admin/reports/{id} {"action": "dismiss" | "delete"}
func (s *Server) handleAdminReportClose(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	rep, ok := s.hub.Reports().Get(r.PathValue("id"))
	if !ok {
		fail(w, ErrNoReport)
		return
	}
	who := actorFrom(r).label()
	switch in.Action {
	case "delete":
		msg, err := s.hub.ModDelete(rep.Room, rep.MessageID, "mod", who)
		if err != nil && err != ErrNoMessage {
			fail(w, err)
			return
		}
		s.log(r, AuditEntry{Action: "message.delete", Room: rep.Room, Message: rep.MessageID, UserID: msg.UserID,
			UserName: msg.Name, Detail: snippet(msg.Original, 80)})
	case "dismiss":
		s.log(r, AuditEntry{Action: "report.dismiss", Room: rep.Room, Message: rep.MessageID})
	default:
		writeErr(w, http.StatusBadRequest, "Aktion: dismiss oder delete")
		return
	}
	out, _ := s.hub.Reports().Close(rep.ID, in.Action, who)
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/rooms/{room}/messages/{id}/pin {"pin": true}
func (s *Server) handleAdminPin(w http.ResponseWriter, r *http.Request) {
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	var in struct {
		Pin bool `json:"pin"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	room := NormRoom(r.PathValue("room"))
	list, err := s.hub.SetPin(room, id, in.Pin)
	if err != nil {
		fail(w, err)
		return
	}
	action := "message.pin"
	if !in.Pin {
		action = "message.unpin"
	}
	s.log(r, AuditEntry{Action: action, Room: room, Message: id})
	writeJSON(w, http.StatusOK, list)
}

// AdminMessage liefert eine Nachricht mit Moderationsdaten.
func (h *Hub) AdminMessage(roomName string, id int64) (Message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, false
	}
	return h.out(r, *m, true), true
}
