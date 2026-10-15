// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Temporäre Kanäle: Personen mit dem Rollen-Recht „Temporäre Kanäle“ legen per Befehl
// (Standard „!kanal Name“) einen eigenen Kanal an; Bots mit dem Recht „Kanäle verwalten“ über
// die Bot-API. Ist eine Weile (Standard 5 Minuten) niemand mehr im Kanal – weder im Text-Chat
// noch im Sprachkanal –, wird er mit allen Nachrichten gelöscht.

// TempInfo: Kennzeichen eines temporären Kanals.
type TempInfo struct {
	Owner     string `json:"owner,omitempty"`     // Nutzer-ID der Person, für die er angelegt wurde
	OwnerName string `json:"ownerName,omitempty"` // Name zum Zeitpunkt des Anlegens
	By        string `json:"by"`                  // "command" oder "bot:<id>"
	Idle      int    `json:"idleMinutes"`         // löschen nach so vielen Minuten ohne Personen
}

// TempRoomsConfig: Einstellungen (Admin → Kanäle → Temporäre Kanäle).
type TempRoomsConfig struct {
	Enabled     bool         `json:"enabled"`         // Befehl erlaubt (Bots: eigenes Recht)
	IdleMinutes int          `json:"idleMinutes"`     // Standard für neue Kanäle
	MaxTotal    int          `json:"maxTotal"`        // temporäre Kanäle insgesamt
	MaxPerUser  int          `json:"maxPerUser"`      // je Person
	Group       string       `json:"group,omitempty"` // Gruppe in der Seitenleiste
	Voice       *VoiceConfig `json:"voice,omitempty"` // Vorlage: als Sprachkanal (nil = nur Text)
}

func defaultTempRooms() TempRoomsConfig {
	return TempRoomsConfig{Enabled: true, IdleMinutes: 5, MaxTotal: 20, MaxPerUser: 1,
		Voice: &VoiceConfig{Preset: "standard", MaxPeers: voiceDefaultPeers, MaxSpeakers: voiceDefaultSpeakers, Guests: VoiceGuestsSpeak,
			Screen: ScreenSpeakers, ScreenQuality: "standard", MaxScreens: 1}}
}

var (
	ErrTempOff    = errors.New("Temporäre Kanäle sind ausgeschaltet")
	ErrTempPerm   = errors.New("Du darfst keine temporären Kanäle anlegen")
	ErrTempMax    = errors.New("Es gibt gerade zu viele temporäre Kanäle – bitte später noch einmal")
	ErrTempIdle   = errors.New("Löschen nach: 1 bis 1440 Minuten ohne Personen")
	ErrTempLimits = errors.New("Grenzen: 1 bis 500 temporäre Kanäle insgesamt, 1 bis 20 je Person")
	ErrTempGroup  = errors.New("Diese Gruppe gibt es nicht")
	ErrTempRate   = errors.New("Zu viele neue Kanäle – bitte kurz warten.")
	ErrBotNotOwn  = errors.New("Bots können nur Kanäle ändern und löschen, die sie selbst angelegt haben")
)

// TempMineError: Die Person hat schon so viele temporäre Kanäle wie erlaubt.
type TempMineError struct{ Room string }

func (e *TempMineError) Error() string {
	return fmt.Sprintf("Du hast schon einen temporären Kanal: #%s", e.Room)
}

type tempStore struct {
	mu  sync.Mutex
	cfg TempRoomsConfig
}

func (h *Hub) registerTempDoc() {
	h.temps = &tempStore{cfg: defaultTempRooms()}
	h.RegisterDoc("temprooms.json", func() any { return h.TempRooms() }, func(b []byte) error {
		var c TempRoomsConfig
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		if v, err := h.validTempRooms(c); err == nil {
			h.temps.mu.Lock()
			h.temps.cfg = v
			h.temps.mu.Unlock()
		}
		return nil
	})
}

// TempRooms liefert die Einstellungen.
func (h *Hub) TempRooms() TempRoomsConfig {
	h.temps.mu.Lock()
	defer h.temps.mu.Unlock()
	c := h.temps.cfg
	if c.Voice != nil {
		v := *c.Voice
		c.Voice = &v
	}
	return c
}

func (h *Hub) validTempRooms(c TempRoomsConfig) (TempRoomsConfig, error) {
	if c.IdleMinutes == 0 {
		c.IdleMinutes = 5
	}
	if c.IdleMinutes < 1 || c.IdleMinutes > 1440 {
		return c, ErrTempIdle
	}
	if c.MaxTotal == 0 {
		c.MaxTotal = 20
	}
	if c.MaxPerUser == 0 {
		c.MaxPerUser = 1
	}
	if c.MaxTotal < 1 || c.MaxTotal > 500 || c.MaxPerUser < 1 || c.MaxPerUser > 20 {
		return c, ErrTempLimits
	}
	if c.Group != "" && !h.groups.has(c.Group) {
		return c, ErrTempGroup
	}
	v, err := h.validVoice(c.Voice)
	if err != nil {
		return c, err
	}
	c.Voice = v
	return c, nil
}

// SetTempRooms speichert die Einstellungen.
func (h *Hub) SetTempRooms(in TempRoomsConfig) (TempRoomsConfig, error) {
	c, err := h.validTempRooms(in)
	if err != nil {
		return c, err
	}
	h.temps.mu.Lock()
	h.temps.cfg = c
	h.temps.mu.Unlock()
	h.touchDoc("temprooms.json")
	return h.TempRooms(), nil
}

// tempRoomName macht aus einer Eingabe einen gültigen Kanalnamen („Mein Raum!“ → „mein-raum“).
func tempRoomName(raw string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case unicode.IsLetter(r) && (unicode.IsLower(r) || !unicode.IsUpper(r)), unicode.IsDigit(r), r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if rs := []rune(name); len(rs) > 28 {
		name = strings.Trim(string(rs[:28]), "-")
	}
	if strings.HasPrefix(name, "dm-") {
		name = "t-" + name
	}
	return name
}

// TempRoomInput: ein neuer temporärer Kanal.
type TempRoomInput struct {
	Name        string
	Description string
	Owner       string // Nutzer-ID (für die Grenze je Person); leer = keine Person
	OwnerName   string
	By          string // "command" oder "bot:<id>"
	Idle        int    // 0 = Einstellung
	Voice       *VoiceConfig
	VoiceSet    bool // Voice gilt (auch nil = nur Text); sonst die Vorlage aus den Einstellungen
	ReadRoles   []string
	WriteRoles  []string
}

// CreateTempRoom legt einen temporären Kanal an (Grenzen insgesamt und je Person).
func (h *Hub) CreateTempRoom(in TempRoomInput) (RoomInfo, error) {
	cfg := h.TempRooms()
	name := tempRoomName(in.Name)
	if name == "" {
		name = tempRoomName("temp-" + in.OwnerName)
		if name == "" || name == "temp" {
			name = "temp"
		}
	}
	idle := in.Idle
	if idle == 0 {
		idle = cfg.IdleMinutes
	}
	if idle < 1 || idle > 1440 {
		return RoomInfo{}, ErrTempIdle
	}
	voice := cfg.Voice
	if in.VoiceSet {
		voice = in.Voice
	}
	voice, err := h.validVoice(voice)
	if err != nil {
		return RoomInfo{}, err
	}
	desc, err := cleanDesc(in.Description)
	if err != nil {
		return RoomInfo{}, err
	}
	readR, writeR := h.validRoleList(in.ReadRoles), h.validRoleList(in.WriteRoles)
	h.mu.Lock()
	defer h.mu.Unlock()
	total, mine, mineName := 0, 0, ""
	for _, r := range h.rooms {
		if r.temp == nil {
			continue
		}
		total++
		if in.Owner != "" && r.temp.Owner == in.Owner {
			mine++
			mineName = r.name
		}
	}
	if in.Owner != "" && mine >= cfg.MaxPerUser {
		return RoomInfo{}, &TempMineError{Room: mineName}
	}
	if total >= cfg.MaxTotal {
		return RoomInfo{}, ErrTempMax
	}
	base := name
	for i := 2; ; i++ {
		if _, taken := h.rooms[name]; !taken && ValidRoom(name) {
			break
		}
		if i > 99 {
			return RoomInfo{}, ErrRoomExists
		}
		name = base + "-" + strconv.Itoa(i)
	}
	r, err := h.newRoomLocked(name, desc)
	if err != nil {
		return RoomInfo{}, err
	}
	r.readRoles, r.writeRoles, r.voice = readR, writeR, voice
	r.temp = &TempInfo{Owner: in.Owner, OwnerName: in.OwnerName, By: in.By, Idle: idle}
	if strings.HasPrefix(in.By, "bot:") {
		r.createdBy = in.By
	}
	if cfg.Group != "" && h.groups.has(cfg.Group) {
		r.group = cfg.Group
	}
	r.emptySince = time.Now()
	h.record(storeOp{kind: opRoomMeta, room: name})
	h.broadcastAll(Event{Type: "layout"}) // Kanalliste bei allen neu laden
	return r.info(), nil
}

// tempExpired liefert die temporären Kanäle, in denen lange genug niemand war. busy: Kanäle mit
// Personen im Sprachkanal.
func (h *Hub) tempExpired(now time.Time, busy map[string]bool) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for name, r := range h.rooms {
		if r.temp == nil {
			continue
		}
		if len(r.clients) > 0 || busy[name] {
			r.emptySince = time.Time{}
			continue
		}
		if r.emptySince.IsZero() {
			r.emptySince = now
			continue
		}
		if now.Sub(r.emptySince) >= time.Duration(r.temp.Idle)*time.Minute {
			out = append(out, name)
		}
	}
	return out
}

// sweepTempRooms löscht abgelaufene temporäre Kanäle (alle 15 Sekunden aus main).
func (s *Server) sweepTempRooms(now time.Time) {
	busy := map[string]bool{}
	if s.voice != nil {
		busy = s.voice.busyRooms()
	}
	for _, name := range s.hub.tempExpired(now, busy) {
		if err := s.hub.DeleteRoom(name); err != nil {
			continue
		}
		s.hub.mu.Lock()
		s.hub.broadcastAll(Event{Type: "layout"})
		s.hub.mu.Unlock()
		s.hub.AuditLog().Add(AuditEntry{Actor: "system", Display: "System", Action: "room.temp_delete", Room: name,
			Detail: "niemand mehr im Kanal"})
		log.Printf("Temporärer Kanal #%s gelöscht (niemand mehr drin)", name)
	}
}

// busyRooms: Kanäle, in deren Sprachkanal gerade jemand ist.
func (v *voiceMgr) busyRooms() map[string]bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]bool{}
	for name, st := range v.rooms {
		if len(st.peers) > 0 {
			out[name] = true
		}
	}
	return out
}

// markCreatedBy merkt sich, dass ein Bot den Kanal angelegt hat.
func (h *Hub) markCreatedBy(name, by string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[name]; ok {
		r.createdBy = by
		h.record(storeOp{kind: opRoomMeta, room: name})
		h.broadcastAll(Event{Type: "layout"})
	}
}

func (h *Hub) roomInfo(name string) (RoomInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.kind != "" {
		return RoomInfo{}, false
	}
	return r.info(), true
}

// --- Befehl im Chat ---

// runTempRoomCommand: „!kanal Name“ – Antwort nur für die Person, mit Sprung in den neuen Kanal.
func (s *Server) runTempRoomCommand(w http.ResponseWriter, room, text string, cmd Command, args, prefix string, me Identity, ip string) {
	now := time.Now().In(s.hub.Location())
	out := EphemeralReply{Name: s.hub.Commands().BotName(), Command: text, Time: now.UTC()}
	notice := func(msg string) {
		out.Text, out.Notice = msg, true
		writeJSON(w, http.StatusOK, map[string]any{"ephemeral": out})
	}
	p := s.posterOf(me, ip)
	if err := s.hub.checkSanction(p); err != nil {
		fail(w, err)
		return
	}
	if !s.hub.TempRooms().Enabled {
		notice(tr(w, ErrTempOff.Error()))
		return
	}
	if !p.Perms.TempRooms {
		notice(tr(w, ErrTempPerm.Error()))
		return
	}
	if !s.tempRL.Allow(me.ID) {
		notice(tr(w, ErrTempRate.Error()))
		return
	}
	info, err := s.hub.CreateTempRoom(TempRoomInput{Name: args, Owner: me.ID, OwnerName: me.Name, By: "command"})
	if err != nil {
		var mine *TempMineError
		if errors.As(err, &mine) {
			notice(trf(w, "Du hast schon einen temporären Kanal: #%s", mine.Room))
			return
		}
		notice(tr(w, err.Error()))
		return
	}
	actor, display := chatActor(me)
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "room.temp_create", Room: info.Name,
		Detail: fmt.Sprintf("per %s%s, löscht sich nach %d min ohne Personen", prefix, cmd.Trigger, info.Temp.Idle)})
	out.Text = strings.NewReplacer("{user}", me.Name, "{room}", room, "{args}", args, "{result}", info.Name,
		"{time}", now.Format("15:04"), "{date}", now.Format("02.01.2006"), "{online}", "").Replace(cmd.Response)
	if strings.TrimSpace(out.Text) == "" {
		out.Text = "#" + info.Name
	}
	out.Open = info.Name
	writeJSON(w, http.StatusOK, map[string]any{"ephemeral": out})
}

// --- Bot-API: Kanäle verwalten ---

// POST /api/bot/rooms {"name","description","temporary","idleMinutes","forUser","voice","readRoles","writeRoles"}
func (s *Server) handleBotRoomCreate(w http.ResponseWriter, r *http.Request) {
	b := botFrom(r)
	if !b.P().Rooms {
		fail(w, errBotNoPerm)
		return
	}
	var in struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Temporary   bool            `json:"temporary"`
		IdleMinutes int             `json:"idleMinutes"`
		ForUser     string          `json:"forUser"` // temporär: für diese Person (Grenze je Person)
		Voice       json.RawMessage `json:"voice"`   // fehlt: Vorlage (temporär) bzw. nur Text; null: nur Text
		ReadRoles   []string        `json:"readRoles"`
		WriteRoles  []string        `json:"writeRoles"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	voice, voiceSet, err := rawVoice(in.Voice)
	if err != nil {
		fail(w, err)
		return
	}
	if !s.tempRL.Allow("bot:" + b.ID) {
		writeErr(w, http.StatusTooManyRequests, ErrTempRate.Error())
		return
	}
	var info RoomInfo
	by := "bot:" + b.ID
	if in.Temporary {
		ti := TempRoomInput{Name: in.Name, Description: in.Description, By: by, Idle: in.IdleMinutes,
			Voice: voice, VoiceSet: voiceSet, ReadRoles: in.ReadRoles, WriteRoles: in.WriteRoles}
		if in.ForUser != "" {
			u, ok := s.users.Get(in.ForUser)
			if !ok {
				fail(w, ErrNoUser)
				return
			}
			ti.Owner, ti.OwnerName = u.ID, u.Name
		}
		info, err = s.hub.CreateTempRoom(ti)
	} else {
		name := in.Name
		info, err = s.hub.CreateRoomWith(RoomInput{Name: &name, Description: in.Description, Links: LinksAll, BotLinks: LinksAll,
			ReadRoles: in.ReadRoles, WriteRoles: in.WriteRoles, Voice: voice})
		if err == nil {
			s.hub.markCreatedBy(info.Name, by)
			info.CreatedBy = by
		}
	}
	if err != nil {
		var mine *TempMineError
		if errors.As(err, &mine) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, ErrTempMax) {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		fail(w, err)
		return
	}
	s.hub.Bots().addRoom(b.ID, info.Name)
	action := "room.create"
	if in.Temporary {
		action = "room.temp_create"
	}
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: action, Room: info.Name}))
	writeJSON(w, http.StatusCreated, info)
}

// rawVoice: Sprach-Einstellungen aus JSON (fehlt → nicht gesetzt, null → nur Text).
func rawVoice(raw json.RawMessage) (*VoiceConfig, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var v VoiceConfig
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, errors.New("Ungültige Einstellungen für den Sprachkanal")
	}
	return &v, true, nil
}

// botOwnRoom: Kanal, den dieser Bot angelegt hat.
func (s *Server) botOwnRoom(w http.ResponseWriter, r *http.Request) (Bot, RoomInfo, bool) {
	b := botFrom(r)
	if !b.P().Rooms {
		fail(w, errBotNoPerm)
		return b, RoomInfo{}, false
	}
	info, ok := s.hub.roomInfo(NormRoom(r.PathValue("room")))
	if !ok {
		fail(w, ErrNoRoom)
		return b, info, false
	}
	if info.CreatedBy != "bot:"+b.ID {
		writeErr(w, http.StatusForbidden, ErrBotNotOwn.Error())
		return b, info, false
	}
	return b, info, true
}

// PATCH /api/bot/rooms/{room} {"name","description","voice","readRoles","writeRoles"} – fehlende Felder bleiben
func (s *Server) handleBotRoomUpdate(w http.ResponseWriter, r *http.Request) {
	b, cur, ok := s.botOwnRoom(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        *string         `json:"name"`
		Description *string         `json:"description"`
		Voice       json.RawMessage `json:"voice"`
		ReadRoles   *[]string       `json:"readRoles"`
		WriteRoles  *[]string       `json:"writeRoles"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ri := RoomInput{Name: in.Name, Description: cur.Description, SlowMode: cur.SlowMode, Links: cur.Links, BotSlowMode: cur.BotSlowMode,
		BotLinks: cur.BotLinks, ReadRoles: cur.ReadRoles, WriteRoles: cur.WriteRoles, Voice: cur.Voice}
	if in.Description != nil {
		ri.Description = *in.Description
	}
	if in.ReadRoles != nil {
		ri.ReadRoles = *in.ReadRoles
	}
	if in.WriteRoles != nil {
		ri.WriteRoles = *in.WriteRoles
	}
	if v, set, err := rawVoice(in.Voice); err != nil {
		fail(w, err)
		return
	} else if set {
		ri.Voice = v
	}
	info, err := s.hub.UpdateRoomWith(cur.Name, ri)
	if err != nil {
		fail(w, err)
		return
	}
	detail := ""
	if info.Name != cur.Name {
		detail = "umbenannt von #" + cur.Name
	}
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: "room.update", Room: info.Name, Detail: detail}))
	writeJSON(w, http.StatusOK, info)
}

// DELETE /api/bot/rooms/{room}
func (s *Server) handleBotRoomDelete(w http.ResponseWriter, r *http.Request) {
	b, cur, ok := s.botOwnRoom(w, r)
	if !ok {
		return
	}
	if err := s.hub.DeleteRoom(cur.Name); err != nil {
		fail(w, err)
		return
	}
	s.hub.mu.Lock()
	s.hub.broadcastAll(Event{Type: "layout"})
	s.hub.mu.Unlock()
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: "room.delete", Room: cur.Name}))
	w.WriteHeader(http.StatusNoContent)
}

// --- Admin ---

// GET /api/admin/temprooms
func (s *Server) handleAdminTempGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.TempRooms())
}

// PUT /api/admin/temprooms
func (s *Server) handleAdminTempPut(w http.ResponseWriter, r *http.Request) {
	var in TempRoomsConfig
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.hub.SetTempRooms(in)
	if err != nil {
		fail(w, err)
		return
	}
	state := "aus"
	if out.Enabled {
		state = "an"
	}
	s.log(r, AuditEntry{Action: "settings.temprooms", Detail: fmt.Sprintf("Befehl %s, löschen nach %d min, höchstens %d (je Person %d), Sprache: %s",
		state, out.IdleMinutes, out.MaxTotal, out.MaxPerUser, out.Voice.describe())})
	writeJSON(w, http.StatusOK, out)
}
