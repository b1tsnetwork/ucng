// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Gespeicherte Widgets: Einstellungen für das eingebettete Chat-Fenster, die der Admin
// verwaltet. Das Snippet auf der Webseite braucht dann nur noch data-widget="w_…".
// Mit Einbetten = "listed" funktioniert das Widget nur auf den hier eingetragenen Domains.

type Widget struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`               // interne Bezeichnung, z. B. "Shop – Support"
	Room      string    `json:"room"`               // Kanal
	Title     string    `json:"title"`              // Überschrift im Widget
	Color     string    `json:"color,omitempty"`    // Akzentfarbe; leer = die der Anwendung
	Theme     string    `json:"theme"`              // light, dark, auto
	Position  string    `json:"position"`           // right, left
	Mode      string    `json:"mode"`               // floating, inline
	Target    string    `json:"target,omitempty"`   // CSS-Selektor für inline
	Greeting  string    `json:"greeting,omitempty"` // Begrüßung als Sprechblase
	ShowHours bool      `json:"showHours"`          // Öffnungszeiten anzeigen
	Domains   []string  `json:"domains"`            // erlaubte Domains (example.com, *.example.com)
	Created   time.Time `json:"created"`
}

// PublicWidget ist die Sicht für das Widget selbst (ohne Domains).
type PublicWidget struct {
	ID        string `json:"id"`
	Room      string `json:"room"`
	Title     string `json:"title"`
	Color     string `json:"color,omitempty"`
	Theme     string `json:"theme"`
	Position  string `json:"position"`
	Mode      string `json:"mode"`
	Target    string `json:"target,omitempty"`
	Greeting  string `json:"greeting,omitempty"`
	ShowHours bool   `json:"showHours"`
}

func (w Widget) public() PublicWidget {
	return PublicWidget{ID: w.ID, Room: w.Room, Title: w.Title, Color: w.Color, Theme: w.Theme,
		Position: w.Position, Mode: w.Mode, Target: w.Target, Greeting: w.Greeting, ShowHours: w.ShowHours}
}

var (
	ErrNoWidget     = errors.New("Dieses Widget gibt es nicht")
	ErrWidgetName   = errors.New("Bitte eine Bezeichnung (1–60 Zeichen) angeben")
	ErrWidgetField  = errors.New("Ungültige Widget-Einstellung")
	ErrWidgetDomain = errors.New("Ungültige Domain – bitte z. B. example.com oder *.example.com eingeben")
	ErrGreeting     = errors.New("Die Begrüßung darf höchstens 300 Zeichen lang sein")
)

type Widgets struct {
	mu      sync.Mutex
	list    map[string]*Widget
	changed func()
}

func NewWidgets() *Widgets { return &Widgets{list: map[string]*Widget{}} }

func (ws *Widgets) notify() {
	if ws.changed != nil {
		ws.changed()
	}
}

// normDomain macht aus Eingaben wie "https://www.Example.com/" → "www.example.com".
func normDomain(d string) (string, error) {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return "", nil
	}
	if strings.Contains(d, "://") {
		u, err := url.Parse(d)
		if err != nil || u.Host == "" {
			return "", ErrWidgetDomain
		}
		d = u.Host
	}
	d = strings.TrimRight(d, "/")
	wild := strings.HasPrefix(d, "*.")
	host := strings.TrimPrefix(d, "*.")
	if host == "" || len(host) > 253 {
		return "", ErrWidgetDomain
	}
	for _, part := range strings.Split(strings.Split(host, ":")[0], ".") {
		if part == "" {
			return "", ErrWidgetDomain
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r > 127) {
				return "", ErrWidgetDomain
			}
		}
	}
	if wild {
		if !strings.Contains(strings.Split(host, ":")[0], ".") {
			return "", ErrWidgetDomain // "*.com" wäre zu weit
		}
		return "*." + host, nil
	}
	return host, nil
}

// domainMatches prüft, ob die Origin (https://shop.example.com) zu einer Domain-Regel passt.
func domainMatches(rule, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	hostNoPort := strings.ToLower(u.Hostname())
	if strings.HasPrefix(rule, "*.") {
		base, h := rule[2:], hostNoPort
		if strings.Contains(base, ":") {
			h = host // Regel mit Port: Port muss passen
		}
		return h == base || strings.HasSuffix(h, "."+base)
	}
	if strings.Contains(rule, ":") {
		return host == rule
	}
	return hostNoPort == rule
}

func validateWidget(in Widget) (Widget, error) {
	name, ok := cleanText(strings.Join(strings.Fields(in.Name), " "), 60)
	if !ok {
		return Widget{}, ErrWidgetName
	}
	in.Name = name
	in.Room = NormRoom(in.Room)
	if in.Room == "" {
		in.Room = "lobby"
	}
	if !ValidRoom(in.Room) {
		return Widget{}, ErrInvalidRoom
	}
	in.Title = strings.Join(strings.Fields(in.Title), " ")
	if in.Title == "" {
		in.Title = "Chat"
	}
	if t, ok := cleanText(in.Title, maxTitleLen); ok {
		in.Title = t
	} else {
		return Widget{}, ErrInvalidTitle
	}
	in.Color = strings.ToLower(strings.TrimSpace(in.Color))
	if in.Color != "" && !hexColorRe.MatchString(in.Color) {
		return Widget{}, ErrInvalidColor
	}
	if in.Theme == "" {
		in.Theme = "light"
	}
	if in.Position == "" {
		in.Position = "right"
	}
	if in.Mode == "" {
		in.Mode = "floating"
	}
	if (in.Theme != "light" && in.Theme != "dark" && in.Theme != "auto") ||
		(in.Position != "right" && in.Position != "left") || (in.Mode != "floating" && in.Mode != "inline") {
		return Widget{}, ErrWidgetField
	}
	in.Target = strings.TrimSpace(in.Target)
	if in.Mode == "inline" && in.Target == "" {
		in.Target = "#chat"
	}
	if in.Mode != "inline" {
		in.Target = ""
	}
	if len(in.Target) > 100 || strings.ContainsAny(in.Target, "<>\"'") {
		return Widget{}, ErrWidgetField
	}
	in.Greeting = strings.TrimSpace(in.Greeting)
	if len([]rune(in.Greeting)) > 300 {
		return Widget{}, ErrGreeting
	}
	doms := []string{}
	seen := map[string]bool{}
	for _, d := range in.Domains {
		n, err := normDomain(d)
		if err != nil {
			return Widget{}, err
		}
		if n != "" && !seen[n] {
			seen[n] = true
			doms = append(doms, n)
		}
	}
	if len(doms) > 50 {
		return Widget{}, ErrWidgetDomain
	}
	in.Domains = doms
	return in, nil
}

func (ws *Widgets) Create(in Widget) (Widget, error) {
	w, err := validateWidget(in)
	if err != nil {
		return Widget{}, err
	}
	w.ID = "w_" + randomHex(5)
	w.Created = time.Now().UTC()
	ws.mu.Lock()
	ws.list[w.ID] = &w
	ws.mu.Unlock()
	ws.notify()
	return w, nil
}

func (ws *Widgets) Update(id string, in Widget) (Widget, error) {
	w, err := validateWidget(in)
	if err != nil {
		return Widget{}, err
	}
	ws.mu.Lock()
	old, ok := ws.list[id]
	if !ok {
		ws.mu.Unlock()
		return Widget{}, ErrNoWidget
	}
	w.ID, w.Created = old.ID, old.Created
	*old = w
	ws.mu.Unlock()
	ws.notify()
	return w, nil
}

func (ws *Widgets) Delete(id string) error {
	ws.mu.Lock()
	_, ok := ws.list[id]
	delete(ws.list, id)
	ws.mu.Unlock()
	if !ok {
		return ErrNoWidget
	}
	ws.notify()
	return nil
}

func (ws *Widgets) Get(id string) (Widget, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	w, ok := ws.list[id]
	if !ok {
		return Widget{}, false
	}
	c := *w
	c.Domains = append([]string{}, w.Domains...)
	return c, true
}

func (ws *Widgets) List() []Widget {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := []Widget{}
	for _, w := range ws.list {
		c := *w
		c.Domains = append([]string{}, w.Domains...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// OriginAllowed: Darf eine Webseite (Origin) auf diesen Kanal zugreifen? room = "" heißt:
// irgendein Widget erlaubt diese Origin (für kanalunabhängige Abfragen wie Einstellungen).
func (ws *Widgets) OriginAllowed(origin, room string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, w := range ws.list {
		if room != "" && w.Room != room {
			continue
		}
		for _, d := range w.Domains {
			if domainMatches(d, origin) {
				return true
			}
		}
	}
	return false
}

func (ws *Widgets) renameRoom(old, new string) {
	ws.mu.Lock()
	changed := false
	for _, w := range ws.list {
		if w.Room == old {
			w.Room = new
			changed = true
		}
	}
	ws.mu.Unlock()
	if changed {
		ws.notify()
	}
}

func (ws *Widgets) export() []Widget { return ws.List() }

func (ws *Widgets) restore(list []Widget) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for i := range list {
		w := list[i]
		if w.ID != "" {
			if w.Domains == nil {
				w.Domains = []string{}
			}
			ws.list[w.ID] = &w
		}
	}
}
