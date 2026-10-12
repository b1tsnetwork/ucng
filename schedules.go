// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Geplante Nachrichten: Ankündigungen, die der Befehls-Bot zu einem festen Zeitpunkt oder
// regelmäßig (täglich, an bestimmten Wochentagen) in einen Kanal schreibt.

type Schedule struct {
	ID       string     `json:"id"`
	Room     string     `json:"room"`
	Text     string     `json:"text"`
	Mode     string     `json:"mode"`               // once, daily, weekly
	At       string     `json:"at,omitempty"`       // once: "2026-10-12T20:00" (Ortszeit)
	Time     string     `json:"time,omitempty"`     // daily/weekly: "20:00"
	Weekdays []int      `json:"weekdays,omitempty"` // weekly: 0 = Montag … 6 = Sonntag
	Pin      bool       `json:"pin"`                // nach dem Senden anpinnen (ältere Ankündigung desselben Plans lösen)
	Enabled  bool       `json:"enabled"`
	LastRun  *time.Time `json:"lastRun,omitempty"`
	LastID   int64      `json:"lastId,omitempty"` // zuletzt gesendete Nachricht (für Pin)
	Next     *time.Time `json:"next,omitempty"`   // nur Ausgabe
	Error    string     `json:"error,omitempty"`  // letzter Fehler beim Senden
}

var (
	ErrScheduleMode = errors.New("Art: einmalig, täglich oder wöchentlich")
	ErrScheduleTime = errors.New("Bitte eine gültige Uhrzeit (HH:MM) bzw. ein gültiges Datum angeben")
	ErrScheduleDays = errors.New("Bitte mindestens einen Wochentag wählen")
	ErrNoSchedule   = errors.New("Diese geplante Nachricht gibt es nicht")
	ErrScheduleMax  = errors.New("Höchstens 100 geplante Nachrichten")
)

type Schedules struct {
	mu      sync.Mutex
	list    []*Schedule
	changed func()
}

func NewSchedules() *Schedules { return &Schedules{} }

func (ss *Schedules) touch() {
	if ss.changed != nil {
		ss.changed()
	}
}

// next berechnet den nächsten Termin nach from (nil = keiner mehr).
func (sc *Schedule) next(from time.Time, loc *time.Location) *time.Time {
	if !sc.Enabled {
		return nil
	}
	switch sc.Mode {
	case "once":
		t, err := time.ParseInLocation("2006-01-02T15:04", sc.At, loc)
		if err != nil || (sc.LastRun != nil && !sc.LastRun.Before(t)) {
			return nil
		}
		return &t
	case "daily", "weekly":
		hm, _ := time.Parse("15:04", sc.Time)
		f := from.In(loc)
		for d := 0; d <= 7; d++ {
			day := f.AddDate(0, 0, d)
			t := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
			if t.Before(f) {
				continue
			}
			if sc.Mode == "weekly" {
				wd := (int(t.Weekday()) + 6) % 7
				ok := false
				for _, x := range sc.Weekdays {
					ok = ok || x == wd
				}
				if !ok {
					continue
				}
			}
			return &t
		}
	}
	return nil
}

func validateSchedule(sc *Schedule, loc *time.Location) error {
	sc.Text = strings.TrimSpace(sc.Text)
	if t, ok := cleanText(sc.Text, maxTextLen); !ok {
		return ErrInvalidText
	} else {
		sc.Text = t
	}
	sc.Room = NormRoom(sc.Room)
	switch sc.Mode {
	case "once":
		if _, err := time.ParseInLocation("2006-01-02T15:04", sc.At, loc); err != nil {
			return ErrScheduleTime
		}
		sc.Time, sc.Weekdays = "", nil
	case "daily":
		if !hhmmRe.MatchString(sc.Time) {
			return ErrScheduleTime
		}
		sc.At, sc.Weekdays = "", nil
	case "weekly":
		if !hhmmRe.MatchString(sc.Time) {
			return ErrScheduleTime
		}
		days := []int{}
		seen := map[int]bool{}
		for _, d := range sc.Weekdays {
			if d >= 0 && d <= 6 && !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		if len(days) == 0 {
			return ErrScheduleDays
		}
		sc.Weekdays, sc.At = days, ""
	default:
		return ErrScheduleMode
	}
	return nil
}

// List liefert alle Pläne mit nächstem Termin.
func (ss *Schedules) List(now time.Time, loc *time.Location) []Schedule {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]Schedule, 0, len(ss.list))
	for _, x := range ss.list {
		c := *x
		c.Next = x.next(now, loc)
		out = append(out, c)
	}
	return out
}

// Put legt an (ID leer) oder ändert.
func (ss *Schedules) Put(in Schedule, loc *time.Location) (Schedule, error) {
	if err := validateSchedule(&in, loc); err != nil {
		return Schedule{}, err
	}
	ss.mu.Lock()
	var cur *Schedule
	for _, x := range ss.list {
		if x.ID == in.ID {
			cur = x
		}
	}
	if cur == nil {
		if in.ID != "" {
			ss.mu.Unlock()
			return Schedule{}, ErrNoSchedule
		}
		if len(ss.list) >= 100 {
			ss.mu.Unlock()
			return Schedule{}, ErrScheduleMax
		}
		in.ID = "s_" + randomHex(5)
		in.LastRun, in.LastID, in.Error = nil, 0, ""
		cur = &in
		ss.list = append(ss.list, cur)
	} else {
		// Bei geänderter Zeit eines einmaligen Plans darf er erneut laufen.
		if in.Mode != cur.Mode || in.At != cur.At {
			cur.LastRun = nil
		}
		cur.Room, cur.Text, cur.Mode, cur.At, cur.Time, cur.Weekdays, cur.Pin, cur.Enabled = in.Room, in.Text, in.Mode, in.At, in.Time, in.Weekdays, in.Pin, in.Enabled
		cur.Error = ""
	}
	out := *cur
	ss.mu.Unlock()
	ss.touch()
	return out, nil
}

func (ss *Schedules) Delete(id string) error {
	ss.mu.Lock()
	for i, x := range ss.list {
		if x.ID == id {
			ss.list = append(ss.list[:i], ss.list[i+1:]...)
			ss.mu.Unlock()
			ss.touch()
			return nil
		}
	}
	ss.mu.Unlock()
	return ErrNoSchedule
}

func (ss *Schedules) Get(id string) (Schedule, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, x := range ss.list {
		if x.ID == id {
			return *x, true
		}
	}
	return Schedule{}, false
}

// due liefert alle Pläne, deren Termin zwischen ihrem letzten Lauf und now liegt.
func (ss *Schedules) due(now time.Time, loc *time.Location) []Schedule {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var out []Schedule
	for _, x := range ss.list {
		if !x.Enabled {
			continue
		}
		from := now.Add(-10 * time.Minute) // verpasste Termine (z. B. nach Neustart) höchstens 10 Minuten nachholen
		if x.LastRun != nil && x.LastRun.After(from) {
			from = x.LastRun.Add(time.Second)
		}
		if t := x.next(from, loc); t != nil && !t.After(now) {
			out = append(out, *x)
		}
	}
	return out
}

func (ss *Schedules) markRun(id string, at time.Time, msgID int64, errText string) {
	ss.mu.Lock()
	for _, x := range ss.list {
		if x.ID == id {
			t := at.UTC()
			x.LastRun, x.Error = &t, errText
			if msgID > 0 {
				x.LastID = msgID
			}
		}
	}
	ss.mu.Unlock()
	ss.touch()
}

func (ss *Schedules) renameRoom(old, name string) {
	ss.mu.Lock()
	for _, x := range ss.list {
		if x.Room == old {
			x.Room = name
		}
	}
	ss.mu.Unlock()
	ss.touch()
}

func (ss *Schedules) export() []Schedule {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]Schedule, len(ss.list))
	for i, x := range ss.list {
		out[i] = *x
		out[i].Next = nil
	}
	return out
}

func (ss *Schedules) restore(list []Schedule) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.list = nil
	for i := range list {
		x := list[i]
		if x.ID != "" {
			ss.list = append(ss.list, &x)
		}
	}
}

// --- Ausführen ---

// postSchedule schreibt die Nachricht eines Plans als Befehls-Bot.
func (s *Server) postSchedule(sc Schedule, now time.Time) (Message, error) {
	loc := s.hub.Location()
	text := strings.NewReplacer("{date}", now.In(loc).Format("02.01.2006"), "{time}", now.In(loc).Format("15:04")).Replace(sc.Text)
	bot := Poster{ID: CmdBotID, Name: s.hub.Commands().BotName(), Bot: true, NoAutomod: true, System: true}
	msg, err := s.hub.SendMedia(sc.Room, bot, text, 0, nil)
	if err != nil {
		return msg, err
	}
	if sc.Pin {
		if sc.LastID > 0 {
			s.hub.SetPin(sc.Room, sc.LastID, false)
		}
		s.hub.SetPin(sc.Room, msg.ID, true)
	}
	return msg, nil
}

// runSchedules sendet fällige Nachrichten (vom Hintergrund-Takt aufgerufen).
func (s *Server) runSchedules(now time.Time) {
	for _, sc := range s.hub.Schedules().due(now, s.hub.Location()) {
		msg, err := s.postSchedule(sc, now)
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		s.hub.Schedules().markRun(sc.ID, now, msg.ID, errText)
		s.hub.AuditLog().Add(AuditEntry{Actor: "system", Display: "Zeitplan", Action: "schedule.run", Room: sc.Room,
			Message: msg.ID, Detail: snippet(sc.Text, 60) + map[bool]string{true: "", false: " – Fehler: " + errText}[err == nil]})
	}
}

// --- Admin-API ---

// GET /api/admin/schedules
func (s *Server) handleAdminSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Schedules().List(time.Now(), s.hub.Location()))
}

// POST /api/admin/schedules (neu) bzw. PUT /api/admin/schedules/{id}
func (s *Server) handleAdminSchedulePut(w http.ResponseWriter, r *http.Request) {
	var in Schedule
	if !decodeJSON(w, r, &in) {
		return
	}
	in.ID = r.PathValue("id")
	if !s.roomExists(NormRoom(in.Room)) {
		fail(w, ErrNoRoom)
		return
	}
	out, err := s.hub.Schedules().Put(in, s.hub.Location())
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "schedule.update", Room: out.Room, Detail: snippet(out.Text, 60)})
	out.Next = out.next(time.Now(), s.hub.Location())
	code := http.StatusOK
	if r.PathValue("id") == "" {
		code = http.StatusCreated
	}
	writeJSON(w, code, out)
}

func (s *Server) roomExists(name string) bool {
	for _, ri := range s.hub.Rooms() {
		if ri.Name == name {
			return true
		}
	}
	return false
}

// DELETE /api/admin/schedules/{id}
func (s *Server) handleAdminScheduleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.Schedules().Delete(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "schedule.delete"})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/admin/schedules/{id}/run – jetzt sofort senden (Termine bleiben unverändert).
func (s *Server) handleAdminScheduleRun(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.hub.Schedules().Get(r.PathValue("id"))
	if !ok {
		fail(w, ErrNoSchedule)
		return
	}
	msg, err := s.postSchedule(sc, time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	if sc.Pin {
		s.hub.Schedules().setLastID(sc.ID, msg.ID)
	}
	s.log(r, AuditEntry{Action: "schedule.run", Room: sc.Room, Message: msg.ID, Detail: fmt.Sprintf("von Hand: %s", snippet(sc.Text, 60))})
	writeJSON(w, http.StatusOK, msg)
}

func (ss *Schedules) setLastID(id string, msgID int64) {
	ss.mu.Lock()
	for _, x := range ss.list {
		if x.ID == id {
			x.LastID = msgID
		}
	}
	ss.mu.Unlock()
	ss.touch()
}
