// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Öffentliche Chat-API (Weboberfläche und Widget).

// PublicSettings: Einstellungen plus feste Angaben für die Oberfläche.
type PublicSettings struct {
	Settings
	Reactions   []string      `json:"reactions"`
	EditMinutes int           `json:"editMinutes"`
	Mode        string        `json:"appMode"` // simple oder hosted (nicht verwechseln mit Settings.Mode = Farbschema)
	Hosted      *PublicHosted `json:"hosted,omitempty"`
	About       AboutInfo     `json:"about"` // Version, Lizenz, Quellcode (AGPL)
	Voice       bool          `json:"voice"` // Sprachkanäle verfügbar
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, PublicSettings{Settings: s.hub.Settings(), Reactions: s.hub.Media().Reactions().Reactions, EditMinutes: int(EditWindow / time.Minute),
		Mode: s.cfg.Mode, Hosted: s.publicHosted(), About: s.about(), Voice: s.voice != nil})
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.RoomsFor(s.accessOf(r)))
}

// accessOf: wer fragt an? (Token optional; ohne = Gast)
func (s *Server) accessOf(r *http.Request) *Access {
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		return nil
	}
	return &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}
}

// GET /api/hours – sind wir gerade erreichbar?
func (s *Server) handleHours(w http.ResponseWriter, r *http.Request) {
	st := s.hub.Settings()
	writeJSON(w, http.StatusOK, st.Hours.Status(time.Now().In(s.hub.Location())))
}

// GET /api/widgets/{id} – gespeicherte Widget-Einstellungen (ohne Domains).
func (s *Server) handleWidgetPublic(w http.ResponseWriter, r *http.Request) {
	wd, ok := s.hub.Widgets().Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, ErrNoWidget.Error())
		return
	}
	writeJSON(w, http.StatusOK, wd.public())
}

// pageParams liest ?before=<Nachrichten-ID>&limit=<Anzahl> (Standard: die neuesten 100).
func pageParams(r *http.Request) (before int64, limit int) {
	before, _ = strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	return before, limit
}

// GET /api/rooms/{room}/messages?before=<id>&limit=<n>
// Liefert bis zu limit Nachrichten (Standard 100, höchstens 500) vor der Nachricht before,
// älteste zuerst. Der Header X-Has-More sagt, ob es noch ältere gibt.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	before, limit := pageParams(r)
	msgs, more, err := s.hub.HistoryAs(NormRoom(r.PathValue("room")), s.accessOf(r), before, limit)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	w.Header().Set("X-Has-More", strconv.FormatBool(more))
	writeJSON(w, http.StatusOK, msgs)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func msgID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// me liefert die angemeldete Person (per Token) und merkt sich die Verbindung (für Sperren).
func (s *Server) me(w http.ResponseWriter, r *http.Request) (Identity, string, bool) {
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, ErrNoSession.Error())
		return Identity{}, "", false
	}
	if !me.Member && s.guestPolicy() != GuestsWrite {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": ErrGuestWrite.Error(), "login": true})
		return Identity{}, "", false
	}
	ip := s.hub.Mod().IPHash(s.clientIP(r))
	s.hub.Mod().SeenIP(me.ID, ip)
	return me, ip, true
}

// writeDenied: Grund, warum in room nicht geschrieben werden darf.
func (s *Server) writeDenied(room string) error {
	if s.hub.IsDMBlocked(room) {
		return ErrDMBlocked
	}
	return ErrReadOnly
}

func (s *Server) posterOf(me Identity, ipHash string) Poster {
	return Poster{ID: me.ID, Name: me.Name, Team: me.Team, Since: me.Created, IPHash: ipHash,
		Roles: me.Roles, Perms: s.hub.Roles().PermsOf(me.Roles, me.Team)}
}

// sessionInfo ist die Antwort von GET/POST /api/session.
type sessionInfo struct {
	Token    string        `json:"token,omitempty"`
	Name     string        `json:"name"`
	ID       string        `json:"id"`
	Team     bool          `json:"team,omitempty"`
	Roles    []string      `json:"roles"`
	Perms    Perms         `json:"perms"`
	Upload   bool          `json:"canUpload"` // darf Bilder hochladen (nach den Regeln im Admin-Bereich)
	Viewer   string        `json:"viewer"`    // für die Online-Liste: ?viewer= beim Live-Stream
	Sanction *SanctionInfo `json:"sanction,omitempty"`
	Member   bool          `json:"member,omitempty"` // Konto (Modus „hosted“)
	Pinned   []string      `json:"pinned"`           // angepinnte Kanäle
	Blocked  []string      `json:"blocked"`          // von mir blockierte Personen (IDs)
}

func (s *Server) sessionOf(me Identity, ipHash string) sessionInfo {
	viewer := me.ID + "." + s.hub.Mod().PresenceSig(me.ID)
	if me.Member { // an die Sitzung gebunden: endet sie, endet auch der Live-Stream
		viewer = me.ID + "." + s.hub.Mod().PresenceSig(me.ID+"|"+me.Session) + "." + me.Session
	}
	info := sessionInfo{Name: me.Name, ID: me.ID, Team: me.Team, Viewer: viewer, Member: me.Member, Pinned: s.users.PinnedOf(me.ID),
		Blocked: s.hub.Blocks().List(me.ID),
		Roles:   s.hub.Roles().Filter(me.Roles), Perms: s.hub.Roles().PermsOf(me.Roles, me.Team)}
	if info.Roles == nil {
		info.Roles = []string{}
	}
	info.Upload = s.mayUpload(s.posterOf(me, ipHash)) == nil
	if sa, ok := s.hub.Mod().Active(me.ID, ipHash); ok {
		info.Sanction = &SanctionInfo{Kind: sa.Kind, Until: sa.Until, Reason: sa.Reason}
	}
	return info
}

// GET /api/session – prüft ein gespeichertes Token und liefert den Namen.
func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.sessionOf(me, ip))
}

// POST /api/session {"name": "..."} – Namen reservieren oder umbenennen.
func (s *Server) handleSessionClaim(w http.ResponseWriter, r *http.Request) {
	if s.guestPolicy() != GuestsWrite {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": ErrGuestWrite.Error(), "login": true})
		return
	}
	ip := s.clientIP(r)
	if !s.claimRL.Allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Versuche – bitte kurz warten.")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	ipHash := s.hub.Mod().IPHash(ip)
	tok := bearer(r)
	if _, known := s.users.Lookup(tok); !known && s.hub.Mod().IPBanned(ipHash) {
		writeErr(w, http.StatusForbidden, "Von dieser Verbindung aus ist der Chat gesperrt.")
		return
	}
	res, err := s.users.Claim(in.Name, tok)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if res.Renamed {
		s.hub.RenameUser(res.ID, res.Name) // alte Nachrichten mitnehmen
	}
	me, _ := s.users.Lookup(res.Token)
	s.hub.Mod().SeenIP(me.ID, ipHash)
	info := s.sessionOf(me, ipHash)
	info.Token = res.Token
	writeJSON(w, http.StatusOK, info)
}

// DELETE /api/session – Namen freigeben.
func (s *Server) handleSessionRelease(w http.ResponseWriter, r *http.Request) {
	if t := bearer(r); t != "" {
		s.users.Release(t)
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/rooms/{room}/messages {"text": "…", "replyTo": 123}
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	if !s.rl.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Nachrichten – bitte kurz warten.")
		return
	}
	var in struct {
		Text    string `json:"text"`
		ReplyTo int64  `json:"replyTo"`
		Thread  int64  `json:"thread"` // Antwort in einem Thread
		GIF     string `json:"gif"`    // "lib:<id>" oder "giphy:<id>" / "tenor:<id>"
		E2E     string `json:"e2e"`    // Ende-zu-Ende-verschlüsselt (nur Direktnachrichten im Modus „hosted“)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	room := NormRoom(r.PathValue("room"))
	a := &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}
	if !s.hub.CanRead(room, a) {
		fail(w, ErrNoRoom)
		return
	}
	if !s.hub.CanWrite(room, a) {
		fail(w, s.writeDenied(room))
		return
	}
	if enc, peer, fed := s.e2eMode(room, me); enc || in.E2E != "" {
		s.postE2E(w, room, me, ip, enc, peer, fed, in.E2E, in.GIF, in.ReplyTo, in.Thread)
		return
	}
	// private Befehle: weder Befehl noch Antwort landen im Kanal
	if in.GIF == "" && s.hub.Commands().IsEphemeral(in.Text, room, me.Team) {
		s.runEphemeral(w, room, in.Text, me, ip)
		return
	}
	// Umfrage per Befehl
	if in.GIF == "" {
		if args, cmd, ok := s.hub.Commands().PollArgs(in.Text, room, me.Team); ok {
			s.postPoll(w, room, args, cmd, me, ip, in.ReplyTo, in.Thread)
			return
		}
	}
	var media *Media
	if in.GIF != "" {
		// vorher prüfen, ob die Person überhaupt schreiben darf – bevor ein GIF geladen wird
		if err := s.hub.checkSanction(s.posterOf(me, ip)); err != nil {
			fail(w, err)
			return
		}
		m, err := s.hub.Media().ResolveGIF(in.GIF)
		if err != nil {
			fail(w, err)
			return
		}
		media = m
	}
	msg, err := s.hub.SendFull(room, s.posterOf(me, ip), SendInput{Text: in.Text, ReplyTo: in.ReplyTo, Thread: in.Thread, Media: media})
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, msg)
	s.runCommand(room, msg, me) // eingebauter Befehls-Bot
}

// PATCH /api/rooms/{room}/messages/{id} {"text": "…"} – eigene Nachricht bearbeiten.
func (s *Server) handleEditOwn(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	if !s.rl.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Änderungen – bitte kurz warten.")
		return
	}
	var in struct {
		Text string `json:"text"`
		E2E  string `json:"e2e"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	room := NormRoom(r.PathValue("room"))
	if in.E2E != "" {
		msg, err := s.hub.EditOwnE2E(room, id, s.posterOf(me, ip), in.E2E)
		if err != nil {
			fail(w, err)
			return
		}
		if _, peer, fed := s.e2eMode(room, me); fed {
			s.fedSend(peer, fedPayload{Type: "edit", From: me.Name, FromKey: e2eSender(in.E2E), ID: id, Time: time.Now().UTC(), E2E: in.E2E}, "", 0)
		}
		writeJSON(w, http.StatusOK, msg)
		return
	}
	msg, err := s.hub.EditOwn(room, id, s.posterOf(me, ip), in.Text)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// DELETE /api/rooms/{room}/messages/{id} – eigene Nachricht löschen.
func (s *Server) handleDeleteOwn(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	room := NormRoom(r.PathValue("room"))
	msg, err := s.hub.DeleteOwn(room, id, me.ID)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if _, peer, fed := s.e2eMode(room, me); fed {
		s.fedSend(peer, fedPayload{Type: "delete", From: me.Name, ID: id, Time: time.Now().UTC()}, "", 0)
	}
	writeJSON(w, http.StatusOK, msg)
}

// POST /api/rooms/{room}/messages/{id}/react {"emoji": "👍"} – Reaktion setzen/entfernen.
func (s *Server) handleReact(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	if !s.rl.Allow("react:" + s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Reaktionen – bitte kurz warten.")
		return
	}
	var in struct {
		Emoji string `json:"emoji"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, _, fed := s.e2eMode(NormRoom(r.PathValue("room")), me); fed {
		fail(w, ErrFedFeature)
		return
	}
	msg, err := s.hub.React(NormRoom(r.PathValue("room")), id, s.posterOf(me, ip), in.Emoji)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// GET /api/rooms/{room}/search?q=… – sichtbare Nachrichten durchsuchen (neueste zuerst).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	room := NormRoom(r.PathValue("room"))
	if !s.hub.CanRead(room, s.accessOf(r)) {
		fail(w, ErrNoRoom)
		return
	}
	hits, err := s.hub.Search(room, r.URL.Query().Get("q"), false, 50)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if a := s.accessOf(r); a != nil {
		if floor := s.hub.DMFloor(room, a.ID); floor > 0 {
			keep := hits[:0]
			for _, h := range hits {
				if h.Message.ID > floor {
					keep = append(keep, h)
				}
			}
			hits = keep
		}
	}
	writeJSON(w, http.StatusOK, hits)
}

// GET /api/rooms/{room}/presence – wer ist gerade im Kanal?
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	room := NormRoom(r.PathValue("room"))
	if !s.hub.CanRead(room, s.accessOf(r)) {
		fail(w, ErrNoRoom)
		return
	}
	ev, err := s.hub.Presence(room)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// POST /api/unread {"since": {"lobby": 120, "support": 98}} – ungelesene Nachrichten und
// Erwähnungen je Kanal seit der zuletzt gelesenen Nachricht. Mit Token werden eigene
// Nachrichten nicht mitgezählt und @Name-Erwähnungen erkannt.
func (s *Server) handleUnread(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Since map[string]int64 `json:"since"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	if len(in.Since) > 500 {
		writeErr(w, http.StatusBadRequest, "Zu viele Kanäle")
		return
	}
	var id, name string
	var a *Access
	var roleNames []string
	if me, ok := s.users.Lookup(bearer(r)); ok {
		id, name = me.ID, me.Name
		a = &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}
		roleNames = s.hub.Roles().Names(me.Roles, true)
	}
	writeJSON(w, http.StatusOK, s.hub.Unread(in.Since, a, id, name, roleNames))
}

// viewerOf prüft ?viewer=<id>.<signatur> und liefert die Person für die Online-Liste.
func (s *Server) viewerOf(r *http.Request) *Viewer {
	parts := strings.Split(r.URL.Query().Get("viewer"), ".")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" {
		return nil
	}
	id, sid := parts[0], ""
	if len(parts) == 3 {
		sid = parts[2]
		if !hmac.Equal([]byte(parts[1]), []byte(s.hub.Mod().PresenceSig(id+"|"+sid))) || !s.users.MemberSession(id, sid) {
			return nil
		}
	} else if !hmac.Equal([]byte(parts[1]), []byte(s.hub.Mod().PresenceSig(id))) {
		return nil
	}
	u, ok := s.users.Get(id)
	if !ok || (u.Account != nil && sid == "") { // Konten nur mit gültiger Sitzung
		return nil
	}
	return &Viewer{ID: u.ID, Name: u.Name, Team: u.Team, Roles: u.Roles, Session: sid}
}

// handleEvents streamt Nachrichten per Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) { s.streamEvents(w, r, false) }

// handleAdminEvents: Live-Stream für die Moderation (Token als Authorization-Kopfzeile).
func (s *Server) handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, true)
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, admin bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "Streaming nicht unterstützt")
		return
	}
	key := "ip:" + s.clientIP(r)
	if admin {
		key = "admin:" + key
	}
	release, ok := s.streamSlot(w, key)
	if !ok {
		return
	}
	defer release()
	roomName := NormRoom(r.PathValue("room"))
	var ch chan Event
	var hist []Message
	var more bool
	var info RoomInfo
	var err error
	if admin {
		ch, hist, more, info, err = s.hub.SubscribeAdmin(roomName)
	} else {
		ch, hist, more, info, err = s.hub.Subscribe(roomName, s.viewerOf(r))
	}
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	defer s.hub.Unsubscribe(roomName, ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx-Puffer abschalten

	send := func(ev Event) bool {
		data, _ := json.Marshal(ev)
		if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	w.Write([]byte("retry: 3000\n\n"))
	desc := info.Description
	pinned, _ := s.hub.Pinned(roomName, s.viewerOf(r).access())
	if admin {
		pinned, _ = s.hub.PinnedAll(roomName)
	}
	if !send(Event{Type: "history", Messages: hist, More: more, Room: roomName, Description: &desc, RoomInfo: &info, Online: info.Online, Pinned: pinned}) {
		return
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok { // Raum gelöscht oder umbenannt
				return
			}
			if !send(ev) {
				return
			}
		}
	}
}

// applyRetention löscht Nachrichten nach Ablauf der Aufbewahrungsfrist.
func (s *Server) applyRetention() {
	days := s.hub.Settings().RetentionDays
	if days <= 0 {
		return
	}
	if n := s.hub.ApplyRetention(days, time.Now()); n > 0 {
		s.hub.AuditLog().Add(AuditEntry{Actor: "system", Display: "System", Action: "retention",
			Detail: strconv.Itoa(n) + " Nachrichten älter als " + strconv.Itoa(days) + " Tage endgültig gelöscht"})
	}
}

// postE2E: Nachricht in einer verschlüsselten Unterhaltung (ggf. an eine andere Instanz).
func (s *Server) postE2E(w http.ResponseWriter, room string, me Identity, ip string, enc bool, peer string, fed bool, e2e, gif string, replyTo, thread int64) {
	if !enc {
		fail(w, ErrE2ENotHere)
		return
	}
	if e2e == "" {
		fail(w, ErrE2ERequired)
		return
	}
	if gif != "" || thread != 0 {
		fail(w, ErrE2EMedia)
		return
	}
	var fm *FedMeta
	if fed {
		_, domain, err := parseFedAddr(strings.TrimPrefix(peer, fedPrefix))
		if err == nil {
			err = s.fedCheckDomain(domain)
		}
		if err != nil {
			fail(w, err)
			return
		}
		fm, replyTo = &FedMeta{State: "pending"}, 0 // Antworten gibt es instanzübergreifend nicht
	}
	msg, err := s.hub.SendFull(room, s.posterOf(me, ip), SendInput{E2E: e2e, ReplyTo: replyTo, Fed: fm})
	if err != nil {
		fail(w, err)
		return
	}
	if fed {
		s.fedSend(peer, fedPayload{Type: "message", From: me.Name, FromKey: e2eSender(e2e), ID: msg.ID, Time: msg.Time, E2E: e2e}, room, msg.ID)
	}
	writeJSON(w, http.StatusCreated, msg)
}
