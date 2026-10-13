// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Zeitzonen auch in schlanken Containern
)

// Settings sind die im Admin-Bereich einstellbaren Werte (data/config.json).
type Settings struct {
	Title          string `json:"title"`
	AllowUserRooms bool   `json:"allowUserRooms"`
	Accent         string `json:"accent"`  // Akzentfarbe #rrggbb
	Sidebar        string `json:"sidebar"` // Seitenleiste #rrggbb, leer = Standard
	Mode           string `json:"mode"`    // auto, light oder dark

	RetentionDays int    `json:"retentionDays"` // Nachrichten nach X Tagen endgültig löschen (0 = nie)
	PrivacyURL    string `json:"privacyUrl"`    // Link „Datenschutz“
	ImprintURL    string `json:"imprintUrl"`    // Link „Impressum“
	Timezone      string `json:"timezone"`      // für Öffnungszeiten, Backups, Statistik
	Hours         Hours  `json:"hours"`         // Öffnungszeiten (z. B. für einen Support-Chat)
	EmbedPolicy   string `json:"embedPolicy"`   // "any" = Widget überall, "listed" = nur Domains aus den Widgets
	NoDMs         bool   `json:"noDMs"`         // Direktnachrichten ausgeschaltet
	StartRoom     string `json:"startRoom"`     // Kanal beim Öffnen ohne bestimmten Kanal (leer = lobby bzw. der erste)
}

// Hours: Öffnungszeiten je Wochentag (0 = Montag … 6 = Sonntag).
type Hours struct {
	Enabled bool       `json:"enabled"`
	Days    [7]DayHour `json:"days"`
	Closed  string     `json:"closed"` // Hinweis außerhalb der Zeiten
}

type DayHour struct {
	Open bool   `json:"open"`
	From string `json:"from"` // "09:00"
	To   string `json:"to"`   // "18:00"
}

const (
	EmbedAny    = "any"
	EmbedListed = "listed"
)

var defaultSettings = Settings{
	Title: "UCNG", AllowUserRooms: true, Accent: defaultAccent, Mode: ModeAuto,
	Timezone: "Europe/Berlin", EmbedPolicy: EmbedAny, Hours: defaultHours(),
}

func defaultHours() Hours {
	h := Hours{Closed: "Wir sind gerade nicht da. Hinterlasse gern eine Nachricht – wir melden uns."}
	for i := 0; i < 7; i++ {
		h.Days[i] = DayHour{Open: i < 5, From: "09:00", To: "18:00"}
	}
	return h
}

var (
	ErrRetention   = errors.New("Aufbewahrung: 0 (nie löschen) bis 3650 Tage")
	ErrBadURL      = errors.New("Links bitte als vollständige Adresse mit https:// angeben")
	ErrTimezone    = errors.New("Unbekannte Zeitzone (Beispiel: Europe/Berlin)")
	ErrHours       = errors.New("Öffnungszeiten bitte als HH:MM angeben, „bis“ nach „von“")
	ErrClosedText  = errors.New("Der Hinweis außerhalb der Öffnungszeiten darf höchstens 300 Zeichen lang sein")
	ErrEmbedPolicy = errors.New("Einbetten: any oder listed")
	ErrStartRoom   = errors.New("Diesen Kanal gibt es nicht – bitte einen vorhandenen Startkanal wählen")
	hhmmRe         = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

// normalize füllt fehlende Werte (z. B. aus älteren config.json) mit Standards.
func (s Settings) normalize() Settings {
	if s.Accent == "" {
		s.Accent = defaultAccent
	}
	if s.Mode == "" {
		s.Mode = ModeAuto
	}
	s.Accent = strings.ToLower(s.Accent)
	s.Sidebar = strings.ToLower(s.Sidebar)
	if s.Timezone == "" {
		s.Timezone = defaultSettings.Timezone
	}
	if s.EmbedPolicy == "" {
		s.EmbedPolicy = EmbedAny
	}
	if s.Hours.Days[0].From == "" && s.Hours.Days[0].To == "" && !s.Hours.Enabled {
		closed := s.Hours.Closed
		s.Hours = defaultHours()
		if closed != "" {
			s.Hours.Closed = closed
		}
	}
	for i := range s.Hours.Days {
		d := &s.Hours.Days[i]
		if d.From == "" {
			d.From = "09:00"
		}
		if d.To == "" {
			d.To = "18:00"
		}
	}
	return s
}

func cleanURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "", nil
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" || len(u) > 500 {
		return "", ErrBadURL
	}
	return p.String(), nil
}

// validateSettings prüft und vereinheitlicht alle Einstellungen.
func validateSettings(s Settings) (Settings, error) {
	title, ok := cleanText(strings.Join(strings.Fields(s.Title), " "), maxTitleLen)
	if !ok {
		return Settings{}, ErrInvalidTitle
	}
	s.Title = title
	s = s.normalize()
	if err := validateTheme(s); err != nil {
		return Settings{}, err
	}
	if s.RetentionDays < 0 || s.RetentionDays > 3650 {
		return Settings{}, ErrRetention
	}
	var err error
	if s.PrivacyURL, err = cleanURL(s.PrivacyURL); err != nil {
		return Settings{}, err
	}
	if s.ImprintURL, err = cleanURL(s.ImprintURL); err != nil {
		return Settings{}, err
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil || s.Timezone == "Local" {
		return Settings{}, ErrTimezone
	}
	for _, d := range s.Hours.Days {
		if !hhmmRe.MatchString(d.From) || !hhmmRe.MatchString(d.To) || (d.Open && d.To <= d.From) {
			return Settings{}, ErrHours
		}
	}
	s.Hours.Closed = strings.TrimSpace(s.Hours.Closed)
	if len([]rune(s.Hours.Closed)) > 300 {
		return Settings{}, ErrClosedText
	}
	if s.EmbedPolicy != EmbedAny && s.EmbedPolicy != EmbedListed {
		return Settings{}, ErrEmbedPolicy
	}
	if s.StartRoom = NormRoom(strings.TrimPrefix(strings.TrimSpace(s.StartRoom), "#")); s.StartRoom != "" && (!ValidRoom(s.StartRoom) || isDMName(s.StartRoom)) {
		return Settings{}, ErrStartRoom
	}
	return s, nil
}

var (
	locMu    sync.Mutex
	locCache = map[string]*time.Location{}
)

// loadLocation lädt eine Zeitzone (zwischengespeichert); unbekannt → UTC.
func loadLocation(name string) *time.Location {
	locMu.Lock()
	defer locMu.Unlock()
	if l, ok := locCache[name]; ok {
		return l
	}
	l, err := time.LoadLocation(name)
	if err != nil || name == "" {
		l = time.UTC
	}
	locCache[name] = l
	return l
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// OpenStatus: Ist gerade geöffnet? Ohne aktivierte Öffnungszeiten immer offen.
type OpenStatus struct {
	Enabled bool   `json:"enabled"`
	Open    bool   `json:"open"`
	Message string `json:"message,omitempty"`
	Today   string `json:"today,omitempty"` // z. B. "09:00–18:00" oder "geschlossen"
	Next    string `json:"next,omitempty"`  // nächste Öffnung, z. B. "Mo 09:00"
}

var weekdayShort = []string{"Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"}

func (hs Hours) Status(now time.Time) OpenStatus {
	st := OpenStatus{Enabled: hs.Enabled, Open: true}
	if !hs.Enabled {
		return st
	}
	wd := (int(now.Weekday()) + 6) % 7 // Montag = 0
	hm := now.Format("15:04")
	d := hs.Days[wd]
	st.Open = d.Open && hm >= d.From && hm < d.To
	if d.Open {
		st.Today = d.From + "–" + d.To
	} else {
		st.Today = "geschlossen"
	}
	if !st.Open {
		st.Message = hs.Closed
		for i := 0; i < 8; i++ {
			k := (wd + i) % 7
			dd := hs.Days[k]
			if !dd.Open || (i == 0 && hm >= dd.From) {
				continue
			}
			if i == 0 {
				st.Next = "heute " + dd.From
			} else if i == 1 {
				st.Next = "morgen " + dd.From
			} else {
				st.Next = weekdayShort[k] + " " + dd.From
			}
			break
		}
	}
	return st
}
