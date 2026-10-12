// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
)

// Farbgestaltung: Der Admin wählt Akzentfarbe, Seitenleistenfarbe und Hell/Dunkel/System.
// /theme.css berechnet daraus alle abgeleiteten Töne (Hover, zarte Flächen, gut lesbare
// Schrift auf Akzent und Seitenleiste) – Weboberfläche und Admin-Bereich binden die Datei ein.

const (
	defaultAccent = "#5848f4"
	ModeAuto      = "auto"
	ModeLight     = "light"
	ModeDark      = "dark"
)

var (
	hexColorRe      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	ErrInvalidColor = errors.New("Farben bitte als #rrggbb angeben, z. B. #5848f4")
	ErrInvalidMode  = errors.New("Darstellung muss hell, dunkel oder automatisch sein")
)

func validateTheme(s Settings) error {
	if !hexColorRe.MatchString(s.Accent) || (s.Sidebar != "" && !hexColorRe.MatchString(s.Sidebar)) {
		return ErrInvalidColor
	}
	if s.Mode != ModeAuto && s.Mode != ModeLight && s.Mode != ModeDark {
		return ErrInvalidMode
	}
	return nil
}

// --- Farbrechnung ---

type rgb struct{ r, g, b float64 } // 0..255

func parseHex(h string) rgb {
	var c rgb
	var r, g, b int
	fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b)
	c.r, c.g, c.b = float64(r), float64(g), float64(b)
	return c
}

func (c rgb) hex() string {
	cl := func(v float64) int { return int(math.Round(math.Max(0, math.Min(255, v)))) }
	return fmt.Sprintf("#%02x%02x%02x", cl(c.r), cl(c.g), cl(c.b))
}

// mix mischt c mit o; t = Anteil von o (0..1).
func (c rgb) mix(o rgb, t float64) rgb {
	return rgb{c.r + (o.r-c.r)*t, c.g + (o.g-c.g)*t, c.b + (o.b-c.b)*t}
}

var (
	white = rgb{255, 255, 255}
	black = rgb{0, 0, 0}
)

// luminance nach WCAG.
func (c rgb) luminance() float64 {
	ch := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.r) + 0.7152*ch(c.g) + 0.0722*ch(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := a.luminance()+0.05, b.luminance()+0.05
	if la < lb {
		la, lb = lb, la
	}
	return la / lb
}

// readableOn liefert die Schriftfarbe für bg: Weiß, wenn es gut lesbar ist (WCAG AA, 4,5:1),
// sonst das, was besser lesbar ist – Weiß oder Fast-Schwarz.
func readableOn(bg rgb) rgb {
	dark := rgb{17, 19, 33}
	if cw := contrast(white, bg); cw >= 4.5 || cw >= contrast(dark, bg) {
		return white
	}
	return dark
}

// themeVars berechnet alle CSS-Variablen für einen Modus (dark = false/true).
func themeVars(s Settings, dark bool) []string {
	accent := parseHex(s.Accent)
	var v []string
	add := func(name string, c rgb) { v = append(v, fmt.Sprintf("--%s: %s;", name, c.hex())) }

	surface, canvas := white, rgb{242, 243, 247}
	if dark {
		surface, canvas = rgb{22, 25, 41}, rgb{13, 15, 28}
	}

	// Akzent: im Dunkelmodus sehr dunkle Akzente etwas aufhellen, damit sie sichtbar bleiben.
	a := accent
	if dark && contrast(a, canvas) < 3 {
		for i := 0; i < 10 && contrast(a, canvas) < 3; i++ {
			a = a.mix(white, 0.12)
		}
	}
	add("accent", a)
	if dark || a.luminance() < 0.08 {
		add("accent-hover", a.mix(white, 0.12))
	} else {
		add("accent-hover", a.mix(black, 0.12))
	}
	add("on-accent", readableOn(a))
	if dark {
		add("accent-soft", surface.mix(a, 0.28))
	} else {
		add("accent-soft", white.mix(a, 0.12))
	}
	// Links und Akzent-Text auf normalem Hintergrund müssen lesbar sein.
	link := a
	for i := 0; i < 10 && contrast(link, surface) < 4.5; i++ {
		if dark {
			link = link.mix(white, 0.15)
		} else {
			link = link.mix(black, 0.15)
		}
	}
	add("accent-text", link)

	// Seitenleiste: eigene Farbe oder Standard (dunkles Blau, im Dunkelmodus noch dunkler).
	var ink rgb
	switch {
	case s.Sidebar != "":
		ink = parseHex(s.Sidebar)
	case dark:
		ink = rgb{10, 11, 21}
	default:
		ink = rgb{21, 23, 46}
	}
	add("ink", ink)
	if ink.luminance() < 0.3 { // dunkle Seitenleiste
		add("ink-raised", ink.mix(white, 0.08))
		add("ink-text", ink.mix(white, 0.72))
		add("ink-muted", ink.mix(white, 0.46))
		add("ink-strong", white)
		add("ink-line", ink.mix(white, 0.12))
	} else { // helle Seitenleiste
		add("ink-raised", ink.mix(black, 0.07))
		add("ink-text", ink.mix(black, 0.72))
		add("ink-muted", ink.mix(black, 0.5))
		add("ink-strong", rgb{17, 19, 33})
		add("ink-line", ink.mix(black, 0.12))
	}

	if dark {
		v = append(v,
			"--canvas: #0d0f1c;", "--surface: #161929;", "--text: #e8e9f2;", "--muted: #8a8ea6;",
			"--line: #262a40;", "--bubble-other: #1d2034;", "--bubble-other-line: transparent;",
			"--shadow-float: 0 1px 2px rgba(0, 0, 0, .3), 0 10px 30px -8px rgba(0, 0, 0, .6);",
			"--warn-bg: #2e2610;", "--warn-text: #f0c56b;", "--ok-text: #4ad6a0;", "--auto-text: #f0c56b;",
			"color-scheme: dark;",
		)
	} else {
		v = append(v, "color-scheme: light;")
	}
	return v
}

// ThemeCSS erzeugt die komplette theme.css für die Einstellungen.
func ThemeCSS(s Settings) string {
	var b strings.Builder
	block := func(indent string, vars []string) {
		for _, x := range vars {
			b.WriteString(indent + "  " + x + "\n")
		}
	}
	fmt.Fprintf(&b, "/* UCNG – Farbgestaltung (automatisch erzeugt: Akzent %s, Seitenleiste %s, Darstellung %s) */\n",
		s.Accent, orDefault(s.Sidebar, "Standard"), s.Mode)
	switch s.Mode {
	case ModeDark:
		b.WriteString(":root {\n")
		block("", themeVars(s, true))
		b.WriteString("}\n")
	case ModeLight:
		b.WriteString(":root {\n")
		block("", themeVars(s, false))
		b.WriteString("}\n")
	default:
		b.WriteString(":root {\n")
		block("", themeVars(s, false))
		b.WriteString("}\n@media (prefers-color-scheme: dark) {\n  :root {\n")
		block("  ", themeVars(s, true))
		b.WriteString("  }\n}\n")
	}
	return b.String()
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// GET /theme.css – optional mit ?accent=&sidebar=&mode= für die Vorschau im Admin-Bereich.
func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	st := s.hub.Settings()
	q := r.URL.Query()
	preview := false
	if v := strings.ToLower(q.Get("accent")); hexColorRe.MatchString(v) {
		st.Accent, preview = v, true
	}
	if q.Has("sidebar") {
		if v := strings.ToLower(q.Get("sidebar")); v == "" || hexColorRe.MatchString(v) {
			st.Sidebar, preview = v, true
		}
	}
	if v := q.Get("mode"); v == ModeAuto || v == ModeLight || v == ModeDark {
		st.Mode, preview = v, true
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if preview {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "no-cache") // bei jedem Laden kurz prüfen (ETag)
	}
	css := ThemeCSS(st)
	etag := fmt.Sprintf(`"%x"`, hashString(css))
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write([]byte(css))
}

func hashString(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
