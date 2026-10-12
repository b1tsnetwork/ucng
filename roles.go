// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Rollen sind frei definierbare Abzeichen (z. B. „Moderator“, „VIP“, „Entwickler“), die der
// Admin Personen zuweist und die im Chat neben dem Namen erscheinen. Jede Rolle kann
// zusätzlich einzelne Rechte geben (Perms). Das Team-Abzeichen hat immer alle Rechte.

type Role struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`       // #rrggbb
	ColorName   bool   `json:"colorName"`   // Namen der Person in dieser Farbe zeigen
	Mentionable bool   `json:"mentionable"` // per @Rollenname erwähnbar
	Perms       Perms  `json:"perms"`
}

// Perms sind Rechte im Chat, die eine Rolle gibt (das Team hat alle).
type Perms struct {
	Links     bool `json:"links"`     // Links trotz Link-Regel
	NoSlow    bool `json:"noSlow"`    // kein Langsam-Modus, keine Wiederholungs-Bremse
	Protected bool `json:"protected"` // darf geschützte Namen tragen
	Pin       bool `json:"pin"`       // Nachrichten anpinnen
	Upload    bool `json:"upload"`    // Bilder hochladen (wenn auf Rollen beschränkt)
	Delete    bool `json:"delete"`    // fremde Nachrichten im Chat löschen
	Mute      bool `json:"mute"`      // im Chat stummschalten (bis 1 Stunde)
}

// Any: hat irgendein Recht?
func (p Perms) Any() bool {
	return p.Links || p.NoSlow || p.Protected || p.Pin || p.Upload || p.Delete || p.Mute
}

// AllPerms sind die Rechte des Teams.
var AllPerms = Perms{Links: true, NoSlow: true, Protected: true, Pin: true, Upload: true, Delete: true, Mute: true}

func (p Perms) or(q Perms) Perms {
	return Perms{Links: p.Links || q.Links, NoSlow: p.NoSlow || q.NoSlow, Protected: p.Protected || q.Protected,
		Pin: p.Pin || q.Pin, Upload: p.Upload || q.Upload, Delete: p.Delete || q.Delete, Mute: p.Mute || q.Mute}
}

const maxRoles = 30

var (
	ErrRoleName     = errors.New("Name der Rolle: 1–24 Zeichen")
	ErrRoleColor    = errors.New("Farbe bitte als #rrggbb angeben")
	ErrRoleExists   = errors.New("Eine Rolle mit diesem Namen gibt es schon")
	ErrRoleReserved = errors.New("„Team“ und „Bot“ sind schon vergeben – bitte einen anderen Namen wählen")
	ErrTooManyRoles = errors.New("Höchstens 30 Rollen")
	ErrNoRole       = errors.New("Diese Rolle gibt es nicht")
)

var (
	roleColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	roleIDRe    = regexp.MustCompile(`^r_[a-z0-9]{4,16}$`)
)

type Roles struct {
	mu      sync.Mutex
	list    []Role
	changed func()
}

func NewRoles() *Roles { return &Roles{} }

func (rs *Roles) List() []Role {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]Role{}, rs.list...)
}

// Has: gibt es die Rolle?
func (rs *Roles) Has(id string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, r := range rs.list {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Set ersetzt alle Rollen (Reihenfolge = Anzeige-Reihenfolge). Neue Rollen haben keine ID.
func (rs *Roles) Set(in []Role) ([]Role, error) {
	if len(in) > maxRoles {
		return nil, ErrTooManyRoles
	}
	rs.mu.Lock()
	known := map[string]bool{}
	for _, r := range rs.list {
		known[r.ID] = true
	}
	rs.mu.Unlock()
	out := make([]Role, 0, len(in))
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range in {
		r.Name = strings.Join(strings.Fields(r.Name), " ")
		if r.Name == "" || utf8.RuneCountInString(r.Name) > 24 {
			return nil, ErrRoleName
		}
		key := strings.ToLower(r.Name)
		if key == "team" || key == "bot" {
			return nil, ErrRoleReserved
		}
		if names[key] {
			return nil, ErrRoleExists
		}
		names[key] = true
		if !roleColorRe.MatchString(r.Color) {
			return nil, ErrRoleColor
		}
		r.Color = strings.ToLower(r.Color)
		if r.ID == "" || !known[r.ID] || ids[r.ID] {
			r.ID = "r_" + randomHex(4)
		}
		ids[r.ID] = true
		out = append(out, r)
	}
	rs.mu.Lock()
	rs.list = out
	rs.mu.Unlock()
	if rs.changed != nil {
		rs.changed()
	}
	return append([]Role{}, out...), nil
}

// PermsOf: alle Rechte aus den Rollen zusammen (team = alle).
func (rs *Roles) PermsOf(ids []string, team bool) Perms {
	if team {
		return AllPerms
	}
	var p Perms
	if len(ids) == 0 {
		return p
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, r := range rs.list {
		for _, id := range ids {
			if r.ID == id {
				p = p.or(r.Perms)
			}
		}
	}
	return p
}

// Names liefert die Namen der Rollen (für @Erwähnungen nur erwähnbare).
func (rs *Roles) Names(ids []string, mentionableOnly bool) []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []string
	for _, r := range rs.list {
		if mentionableOnly && !r.Mentionable {
			continue
		}
		for _, id := range ids {
			if r.ID == id {
				out = append(out, r.Name)
			}
		}
	}
	return out
}

// Get liefert eine Rolle.
func (rs *Roles) Get(id string) (Role, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, r := range rs.list {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

// Filter behält nur vorhandene Rollen – in der Reihenfolge der Rollenliste.
func (rs *Roles) Filter(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []string
	for _, r := range rs.list {
		if want[r.ID] {
			out = append(out, r.ID)
		}
	}
	return out
}

func (rs *Roles) restore(list []Role) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.list = rs.list[:0]
	for _, r := range list {
		if roleIDRe.MatchString(r.ID) && r.Name != "" && roleColorRe.MatchString(r.Color) && len(rs.list) < maxRoles {
			rs.list = append(rs.list, r)
		}
	}
}

// PublicRoles: Rollen und wer sie trägt (für Chat und Widget).
type PublicRoles struct {
	Roles []Role              `json:"roles"`
	Users map[string][]string `json:"users"` // Nutzer-ID → Rollen-IDs
}
