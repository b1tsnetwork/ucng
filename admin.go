// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Admin-API. Jede ändernde Aktion wird im Protokoll festgehalten (wer, wann, was).

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return false
	}
	return true
}

// log schreibt einen Protokolleintrag mit der angemeldeten Person.
func (s *Server) log(r *http.Request, e AuditEntry) {
	a := actorFrom(r)
	e.Actor, e.Display = a.Name, a.label()
	s.hub.AuditLog().Add(e)
}

func fail(w http.ResponseWriter, err error) { writeErr(w, statusFor(err), err.Error()) }

// POST /api/admin/login {"username": "...", "password": "..."} – ohne username: Haupt-Admin.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anmeldeversuche. Bitte eine Minute warten.")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tok, actor, ok := s.accounts.Login(in.Username, in.Password)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Anmeldename oder Passwort stimmt nicht")
		return
	}
	s.hub.AuditLog().Add(AuditEntry{Actor: actor.Name, Display: actor.label(), Action: "login"})
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "actor": actor})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	s.accounts.Logout(bearer(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminMe(w http.ResponseWriter, r *http.Request) {
	a := actorFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{"admin": a.Role == RoleAdmin, "actor": a, "backups": s.backups != nil})
}

func (s *Server) handleAdminSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Settings())
}

// PUT /api/admin/settings
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	var in Settings
	if !decodeJSON(w, r, &in) {
		return
	}
	before := s.hub.Settings()
	if in.StartRoom != "" && in.StartRoom != before.StartRoom && !s.hub.IsChannel(NormRoom(strings.TrimPrefix(in.StartRoom, "#"))) {
		fail(w, ErrStartRoom)
		return
	}
	out, err := s.hub.SetSettings(in)
	if err != nil {
		fail(w, err)
		return
	}
	detail := ""
	if before.RetentionDays != out.RetentionDays {
		detail = fmt.Sprintf("Aufbewahrung: %d → %d Tage", before.RetentionDays, out.RetentionDays)
	}
	if before.StartRoom != out.StartRoom {
		if detail != "" {
			detail += ", "
		}
		start := "automatisch"
		if out.StartRoom != "" {
			start = "#" + out.StartRoom
		}
		detail += "Startkanal: " + start
	}
	s.log(r, AuditEntry{Action: "settings.update", Detail: detail})
	if before.RetentionDays != out.RetentionDays {
		go s.applyRetention()
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/rooms {"name": "...", "description": "...", "slowMode": 0, "links": "all"}
func (s *Server) handleAdminCreateRoom(w http.ResponseWriter, r *http.Request) {
	var in RoomInput
	if !decodeJSON(w, r, &in) {
		return
	}
	info, err := s.hub.CreateRoomWith(in)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "room.create", Room: info.Name})
	writeJSON(w, http.StatusCreated, info)
}

// PATCH /api/admin/rooms/{room} {"name": "neuer-name", "description": "...", "slowMode": 10, "links": "known"}
// Fehlen slowMode/links, bleiben sie unverändert.
func (s *Server) handleAdminUpdateRoom(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        *string `json:"name"`
		Description string  `json:"description"`
		SlowMode    *int    `json:"slowMode"`
		Links       *string `json:"links"`
		BotSlowMode *int    `json:"botSlowMode"`
		BotLinks    *string `json:"botLinks"`
		// Zugang: fehlt das Feld, bleibt er unverändert; [] = alle.
		ReadRoles  *[]string `json:"readRoles"`
		WriteRoles *[]string `json:"writeRoles"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	name := NormRoom(r.PathValue("room"))
	var cur RoomInfo
	found := false
	for _, ri := range s.hub.Rooms() {
		if ri.Name == name {
			cur, found = ri, true
		}
	}
	if !found {
		fail(w, ErrNoRoom)
		return
	}
	ri := RoomInput{Name: in.Name, Description: in.Description, SlowMode: cur.SlowMode, Links: cur.Links,
		BotSlowMode: cur.BotSlowMode, BotLinks: cur.BotLinks, ReadRoles: cur.ReadRoles, WriteRoles: cur.WriteRoles}
	if in.ReadRoles != nil {
		ri.ReadRoles = *in.ReadRoles
	}
	if in.WriteRoles != nil {
		ri.WriteRoles = *in.WriteRoles
	}
	if in.SlowMode != nil {
		ri.SlowMode = *in.SlowMode
	}
	if in.Links != nil {
		ri.Links = *in.Links
	}
	if in.BotSlowMode != nil {
		ri.BotSlowMode = *in.BotSlowMode
	}
	if in.BotLinks != nil {
		ri.BotLinks = *in.BotLinks
	}
	info, err := s.hub.UpdateRoomWith(name, ri)
	if err != nil {
		fail(w, err)
		return
	}
	detail := ""
	if info.Name != name {
		detail = "umbenannt von #" + name
	}
	if info.SlowMode != cur.SlowMode || info.Links != cur.Links || info.BotSlowMode != cur.BotSlowMode || info.BotLinks != cur.BotLinks {
		if detail != "" {
			detail += ", "
		}
		detail += fmt.Sprintf("Langsam-Modus %ds, Links: %s; Bots: %ds, Links: %s", info.SlowMode, info.Links, info.BotSlowMode, info.BotLinks)
	}
	if !slices.Equal(info.ReadRoles, cur.ReadRoles) || !slices.Equal(info.WriteRoles, cur.WriteRoles) {
		if detail != "" {
			detail += ", "
		}
		detail += "Zugang: " + s.roleListText(info.ReadRoles) + "; Schreiben: " + s.roleListText(info.WriteRoles)
	}
	s.log(r, AuditEntry{Action: "room.update", Room: info.Name, Detail: detail})
	writeJSON(w, http.StatusOK, info)
}

// roleListText beschreibt den Zugang eines Kanals fürs Protokoll.
func (s *Server) roleListText(ids []string) string {
	if len(ids) == 0 {
		return "alle"
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == RoleTeamOnly {
			names = append(names, "Team")
		} else if ro, ok := s.hub.Roles().Get(id); ok {
			names = append(names, ro.Name)
		}
	}
	return strings.Join(names, ", ")
}

// DELETE /api/admin/rooms/{room}
func (s *Server) handleAdminDeleteRoom(w http.ResponseWriter, r *http.Request) {
	name := NormRoom(r.PathValue("room"))
	if err := s.hub.DeleteRoom(name); err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "room.delete", Room: name})
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/admin/rooms/{room}/messages?before=<id>&limit=<n> – Verlauf mit Originaltexten gelöschter Nachrichten
func (s *Server) handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	before, limit := pageParams(r)
	msgs, more, err := s.hub.AdminHistory(NormRoom(r.PathValue("room")), before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("X-Has-More", strconv.FormatBool(more))
	writeJSON(w, http.StatusOK, msgs)
}

// DELETE /api/admin/rooms/{room}/messages/{id}
func (s *Server) handleAdminDeleteMessage(w http.ResponseWriter, r *http.Request) {
	who := actorFrom(r).label()
	s.moderateMessage(w, r, "message.delete", func(room string, id int64) (Message, error) {
		return s.hub.ModDelete(room, id, "mod", who)
	})
}

// POST /api/admin/rooms/{room}/messages/{id}/restore
func (s *Server) handleAdminRestoreMessage(w http.ResponseWriter, r *http.Request) {
	s.moderateMessage(w, r, "message.restore", s.hub.RestoreMessage)
}

// POST /api/admin/rooms/{room}/messages/{id}/confirm – Automod-Ausblendung bestätigen.
func (s *Server) handleAdminConfirmMessage(w http.ResponseWriter, r *http.Request) {
	s.moderateMessage(w, r, "message.confirm", s.hub.ConfirmMessage)
}

// POST /api/admin/rooms/{room}/messages/{id}/purge – endgültig löschen (auch aus der Datei).
func (s *Server) handleAdminPurgeMessage(w http.ResponseWriter, r *http.Request) {
	s.moderateMessage(w, r, "message.purge", s.hub.PurgeMessage)
}

func (s *Server) moderateMessage(w http.ResponseWriter, r *http.Request, action string, do func(string, int64) (Message, error)) {
	id, ok := msgID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Ungültige Nachrichten-ID")
		return
	}
	room := NormRoom(r.PathValue("room"))
	msg, err := do(room, id)
	if err != nil {
		fail(w, err)
		return
	}
	e := AuditEntry{Action: action, Room: room, Message: id, UserID: msg.UserID, UserName: msg.Name}
	if action != "message.purge" { // beim endgültigen Löschen keinen Text ins Protokoll übernehmen
		e.Detail = snippet(msg.Original+msg.Text, 80)
	}
	s.log(r, e)
	if action == "message.purge" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// GET /api/admin/search?q=…&room=… – auch in gelöschten Nachrichten (Originaltext) und Namen.
func (s *Server) handleAdminSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hits, err := s.hub.Search(NormRoom(q.Get("room")), q.Get("q"), true, 100)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hits)
}

// --- Automoderation ---

// GET /api/admin/automod
func (s *Server) handleAutomodGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Automod().Config())
}

// GET /api/admin/filter – Wortfilter (ersetzen statt ausblenden)
func (s *Server) handleFilterGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Filter())
}

// PUT /api/admin/filter {"patterns": ["mist", "doof*"]}
func (s *Server) handleFilterSet(w http.ResponseWriter, r *http.Request) {
	var in FilterConfig
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	out, err := s.hub.SetFilter(in)
	var pe *PatternError
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Einige Muster sind ungültig.", "problems": pe.Problems})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "filter.update", Detail: fmt.Sprintf("%d Zeilen", len(out.Patterns))})
	writeJSON(w, http.StatusOK, out)
}

// PUT /api/admin/automod {"enabled": true, "patterns": ["arsch*", "*loch"]}
func (s *Server) handleAutomodSet(w http.ResponseWriter, r *http.Request) {
	var in AutomodConfig
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20) // auch sehr lange Wortlisten
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	before := len(s.hub.Automod().Config().Patterns)
	out, err := s.hub.SetAutomod(in)
	var pe *PatternError
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Einige Muster sind ungültig.", "problems": pe.Problems})
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	state := "an"
	if !out.Enabled {
		state = "aus"
	}
	s.log(r, AuditEntry{Action: "automod.update", Detail: fmt.Sprintf("%s, %d → %d Zeilen", state, before, len(out.Patterns))})
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/automod/test {"text": "...", "room": "…"} – prüft einen Beispieltext gegen die
// gespeicherten Regeln (mit room: so, wie sie in diesem Kanal gelten).
func (s *Server) handleAutomodTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Room string `json:"room"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	pattern, normalized, ok := s.hub.TestAutomod(NormRoom(in.Room), in.Text)
	writeJSON(w, http.StatusOK, map[string]any{"match": ok, "pattern": pattern, "normalized": normalized})
}

// GET /api/admin/rooms/{room}/automod
func (s *Server) handleRoomAutomodGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.hub.RoomAutomodConfig(NormRoom(r.PathValue("room")))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// PUT /api/admin/rooms/{room}/automod {"mode": "global"|"both"|"own"|"off", "patterns": […]}
func (s *Server) handleRoomAutomodSet(w http.ResponseWriter, r *http.Request) {
	var in RoomAutomod
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	room := NormRoom(r.PathValue("room"))
	out, err := s.hub.SetRoomAutomod(room, in)
	var pe *PatternError
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Einige Muster sind ungültig.", "problems": pe.Problems})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "automod.room", Room: room, Detail: fmt.Sprintf("%s, %d Zeilen", out.Mode, len(out.Patterns))})
	writeJSON(w, http.StatusOK, out)
}

// GET /api/admin/automod/queue – ungeprüfte Automod-Treffer aus allen Kanälen.
func (s *Server) handleAutomodQueue(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.AutomodQueue())
}

// --- Personen ---

type adminUser struct {
	UserInfo
	Reserved bool      `json:"reserved"` // Name ist (noch) reserviert
	Sanction *Sanction `json:"sanction,omitempty"`
}

func (s *Server) sanctionOf(id string) *Sanction {
	if sa, ok := s.hub.Mod().Active(id, ""); ok {
		c := *sa
		c.IPHash = boolMark(c.IPHash != "")
		return &c
	}
	return nil
}

// GET /api/admin/users?q=… – reservierte Namen, zuletzt aktive zuerst.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	list, total := s.users.List(r.URL.Query().Get("q"), 200)
	out := make([]adminUser, len(list))
	for i, u := range list {
		out[i] = adminUser{UserInfo: s.forActor(r, u), Reserved: true, Sanction: s.sanctionOf(u.ID)}
	}
	w.Header().Set("X-Total", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, out)
}

// GET /api/admin/users/{id}
func (s *Server) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, ok := s.users.Get(id)
	if !ok {
		sa := s.sanctionOf(id)
		if sa == nil {
			fail(w, ErrNoUser)
			return
		}
		writeJSON(w, http.StatusOK, adminUser{UserInfo: UserInfo{ID: id, Name: sa.Name}, Sanction: sa})
		return
	}
	writeJSON(w, http.StatusOK, adminUser{UserInfo: s.forActor(r, u), Reserved: true, Sanction: s.sanctionOf(id)})
}

// forActor: Moderatoren sehen keine E-Mail-Adressen von Konten.
func (s *Server) forActor(r *http.Request, u UserInfo) UserInfo {
	if u.Account != nil && actorFrom(r).Role != RoleAdmin {
		a := *u.Account
		a.Email = ""
		u.Account = &a
	}
	return u
}

// POST /api/admin/users/{id}/sanction {"kind": "mute"|"ban", "minutes": 60, "reason": "…", "ip": true, "name": "…"}
// minutes = 0 bei "ban": dauerhaft.
func (s *Server) handleAdminSanction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind    string `json:"kind"`
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
		IP      bool   `json:"ip"`
		Name    string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	name := in.Name
	if u, ok := s.users.Get(id); ok {
		name = u.Name
		if u.Team {
			writeErr(w, http.StatusForbidden, "Personen mit Team-Abzeichen können nicht gesperrt werden – bitte zuerst das Abzeichen entfernen")
			return
		}
	} else if name == "" {
		fail(w, ErrNoUser)
		return
	}
	if in.Minutes < 0 || in.Minutes > 365*24*60 {
		writeErr(w, http.StatusBadRequest, "Dauer: 1 Minute bis 1 Jahr")
		return
	}
	sa, err := s.hub.Mod().Sanction(id, name, in.Kind, in.Minutes, in.Reason, actorFrom(r).label(), in.IP)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.NotifySanction(id)
	s.log(r, AuditEntry{Action: "user." + in.Kind, UserID: id, UserName: name, Detail: sanctionDetail(sa, s.hub.Location())})
	sa.IPHash = boolMark(sa.IPHash != "")
	writeJSON(w, http.StatusOK, sa)
}

func sanctionDetail(sa Sanction, loc *time.Location) string {
	d := "dauerhaft"
	if sa.Until != nil {
		d = "bis " + fmtUntil(*sa.Until, loc)
	}
	if sa.IPHash != "" {
		d += ", auch Verbindung"
	}
	if sa.Reason != "" {
		d += " – " + sa.Reason
	}
	return d
}

// DELETE /api/admin/users/{id}/sanction – Sperre aufheben.
func (s *Server) handleAdminLift(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sa, err := s.hub.Mod().Lift(id)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.NotifySanction(id)
	s.log(r, AuditEntry{Action: "user.unsanction", UserID: id, UserName: sa.Name})
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/admin/sanctions – alle aktiven Sperren.
func (s *Server) handleAdminSanctions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Mod().List())
}

// POST /api/admin/users/{id}/team {"team": true}
func (s *Server) handleAdminTeam(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Team bool `json:"team"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	u, err := s.users.SetTeam(id, in.Team)
	if err != nil {
		fail(w, err)
		return
	}
	if in.Team {
		s.hub.Mod().Lift(id) // Team-Mitglieder sind nie gesperrt
	}
	s.hub.SetUserTeam(id, in.Team)
	s.hub.RefreshViewer(id, in.Team, u.Roles)
	action := "user.team.add"
	if !in.Team {
		action = "user.team.remove"
	}
	s.log(r, AuditEntry{Action: action, UserID: id, UserName: u.Name})
	writeJSON(w, http.StatusOK, u)
}

// POST /api/admin/users/{id}/purge {"releaseName": true} – alle Nachrichten einer Person
// endgültig löschen (z. B. auf Wunsch nach DSGVO), optional den Namen freigeben.
func (s *Server) handleAdminPurgeUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ReleaseName bool `json:"releaseName"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if old, err := s.users.SetAvatar(id, ""); err == nil {
		s.removeAvatarFile(old)
		s.users.SetStatus(id, "", "")
	}
	n := s.hub.PurgeUser(id)
	s.hub.Reports().ScrubUser(id)
	released := false
	if in.ReleaseName {
		released = s.users.RemoveByID(id)
		s.hub.Mod().Lift(id)
	}
	detail := fmt.Sprintf("%d Nachrichten endgültig gelöscht", n)
	if released {
		detail += ", Name freigegeben"
	}
	s.log(r, AuditEntry{Action: "user.purge", UserID: id, Detail: detail}) // bewusst ohne Namen
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "released": released})
}

// GET/PUT /api/admin/protected-names {"patterns": ["admin*", …]}
func (s *Server) handleProtectedGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"patterns": s.hub.Mod().ProtectedConfig()})
}

func (s *Server) handleProtectedSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Patterns []string `json:"patterns"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.hub.Mod().SetProtected(in.Patterns)
	var pe *PatternError
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Einige Muster sind ungültig.", "problems": pe.Problems})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "names.protected", Detail: fmt.Sprintf("%d Zeilen", len(out))})
	writeJSON(w, http.StatusOK, map[string]any{"patterns": out})
}

// --- Protokoll, Statistik ---

// GET /api/admin/audit?q=…&before=<cursor>
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.Atoi(r.URL.Query().Get("before"))
	list, next := s.hub.AuditLog().Query(r.URL.Query().Get("q"), before, 100)
	writeJSON(w, http.StatusOK, map[string]any{"entries": list, "next": next})
}

// GET /api/admin/stats?days=30
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	st := s.hub.Stats(days, time.Now())
	_, users := s.users.List("", 1)
	writeJSON(w, http.StatusOK, map[string]any{
		"stats": st, "names": users, "sanctions": len(s.hub.Mod().List()), "bots": len(s.hub.Bots().List()),
		"timezone": s.hub.Settings().Timezone,
	})
}

// --- Konten ---

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.accounts.List())
}

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var in AccountInput
	if !decodeJSON(w, r, &in) {
		return
	}
	info, err := s.accounts.Create(in)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "account.create", Detail: info.Name + " (" + info.Role + ")"})
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	var in AccountInput
	if !decodeJSON(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	info, err := s.accounts.Update(name, in)
	if err != nil {
		fail(w, err)
		return
	}
	d := info.Name + " (" + info.Role + ")"
	if info.Disabled {
		d += ", deaktiviert"
	}
	if in.Password != "" {
		d += ", neues Passwort"
	}
	s.log(r, AuditEntry{Action: "account.update", Detail: d})
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.accounts.Delete(name); err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "account.delete", Detail: name})
	w.WriteHeader(http.StatusNoContent)
}

// --- Widgets ---

func (s *Server) handleWidgets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Widgets().List())
}

func (s *Server) handleWidgetCreate(w http.ResponseWriter, r *http.Request) {
	var in Widget
	if !decodeJSON(w, r, &in) {
		return
	}
	wd, err := s.hub.Widgets().Create(in)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "widget.create", Room: wd.Room, Detail: wd.Name})
	writeJSON(w, http.StatusCreated, wd)
}

func (s *Server) handleWidgetUpdate(w http.ResponseWriter, r *http.Request) {
	var in Widget
	if !decodeJSON(w, r, &in) {
		return
	}
	wd, err := s.hub.Widgets().Update(r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "widget.update", Room: wd.Room, Detail: wd.Name})
	writeJSON(w, http.StatusOK, wd)
}

func (s *Server) handleWidgetDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wd, _ := s.hub.Widgets().Get(id)
	if err := s.hub.Widgets().Delete(id); err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "widget.delete", Detail: wd.Name})
	w.WriteHeader(http.StatusNoContent)
}
