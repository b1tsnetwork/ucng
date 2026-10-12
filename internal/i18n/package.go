// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package i18n

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Übersetzungspaket: alles, was eine KI (oder ein Mensch) zum Übersetzen braucht – Anleitung,
// Ausgangstext, Referenzübersetzung, Platzhalter und Zustand je Text. Ausgefüllt wird nur „text“;
// der Import nimmt das Paket unverändert zurück (ebenso die einfache Sprachdatei key → Text).

const (
	PackageFormat       = "ucng-i18n-package/1"
	legacyPackageFormat = "gochat-i18n-package/1" // vor der Umbenennung erzeugte Pakete
)

// PackageEntry ist ein Text im Übersetzungspaket.
type PackageEntry struct {
	Source       string   `json:"source"`                   // Ausgangstext (Deutsch)
	Reference    string   `json:"reference,omitempty"`      // Referenzübersetzung (Englisch) zum Verständnis
	Text         string   `json:"text"`                     // Übersetzung – auszufüllen
	Status       string   `json:"status"`                   // missing | outdated | translated
	Placeholders []string `json:"placeholders,omitempty"`   // müssen erhalten bleiben
	Plural       bool     `json:"plural,omitempty"`         // enthält eine Mehrzahl-Auswahl
	HTML         bool     `json:"html,omitempty"`           // enthält Markup
	Previous     string   `json:"previousSource,omitempty"` // bei outdated: Original zum Zeitpunkt der Übersetzung
}

// Package ist das Übersetzungspaket einer Sprache.
type Package struct {
	Format       string                  `json:"format"`
	App          string                  `json:"app"`
	Instructions []string                `json:"instructions"`
	Source       string                  `json:"sourceLanguage"`
	Reference    string                  `json:"referenceLanguage,omitempty"`
	Target       string                  `json:"targetLanguage"`
	Meta         Meta                    `json:"meta"`
	Total        int                     `json:"total"`
	Open         int                     `json:"open"` // fehlend oder veraltet
	Texts        map[string]PackageEntry `json:"texts"`
}

var (
	pluralRe = regexp.MustCompile(`\{\s*[\w.]+\s*,\s*plural\s*,`)
	htmlRe   = regexp.MustCompile(`</?(b|strong|em|i|code|br|small|kbd|a|span)\b[^>]*>`)
)

// ReferenceLanguage: Englisch als Verständnishilfe, sofern vorhanden und nicht selbst das Ziel.
func (c *Catalog) referenceFor(target string) string {
	if target != "en" && target != Source {
		if _, ok := c.official["en"]; ok {
			return "en"
		}
	}
	return ""
}

// Package erzeugt das Übersetzungspaket für eine Sprache. onlyOpen: nur fehlende und veraltete Texte.
func (c *Catalog) Package(lang, app string, onlyOpen bool) ([]byte, error) {
	ents, err := c.Entries(lang)
	if err != nil {
		return nil, err
	}
	m := c.Meta(lang)
	ref := c.referenceFor(lang)
	p := Package{Format: PackageFormat, App: app, Source: Source, Reference: ref, Target: lang, Meta: m,
		Texts: map[string]PackageEntry{}}
	for _, e := range ents {
		pe := PackageEntry{Source: e.Source, Placeholders: e.Vars, Status: "translated"}
		if ref != "" {
			pe.Reference = c.official[ref][e.Key]
		}
		switch {
		case e.Changed:
			pe.Text = e.Text
			if e.Outdated {
				pe.Status, pe.Previous = "outdated", e.Base
			}
		case e.Official != "" && lang != Source:
			pe.Text = e.Official
		case lang == Source:
			pe.Text = e.Source
		default:
			pe.Status = "missing"
		}
		if strings.HasPrefix(e.Key, srvPrefix) {
			pe.Placeholders = nil
			for _, v := range e.Vars {
				pe.Placeholders = append(pe.Placeholders, "{"+v+"}")
			}
		} else {
			for i, v := range pe.Placeholders {
				pe.Placeholders[i] = "{" + v + "}"
			}
		}
		pe.Plural = pluralRe.MatchString(e.Source)
		pe.HTML = htmlRe.MatchString(e.Source)
		p.Total++
		if pe.Status != "translated" {
			p.Open++
		} else if onlyOpen {
			continue
		}
		p.Texts[e.Key] = pe
	}
	name := m.Name
	if m.Locale != "" {
		name += " (" + m.Locale + ")"
	}
	p.Instructions = packageInstructions(app, name, ref)
	return json.MarshalIndent(p, "", "  ")
}

func packageInstructions(app, target, ref string) []string {
	out := []string{
		fmt.Sprintf("You are translating the user interface of %s, a web chat application, from German (sourceLanguage) into %s.", app, target),
		`Fill in only the "text" field of each entry in "texts". Do not change keys, "source", "reference" or any other field, do not add or remove entries. Return the complete document as valid JSON (UTF-8).`,
		`"status": "missing" = still to translate; "outdated" = the German source changed since the existing translation ("previousSource" shows the old source) – update "text" to match the current source; "translated" = already done, change only if clearly wrong.`,
		`Placeholders listed in "placeholders" (e.g. {name}, {n}) must appear unchanged in "text" – never translate or rename what is inside the braces, but you may move them to fit the sentence. Server messages (keys starting with "srv.") use numbered placeholders {1}, {2}.`,
		`Plurals look like {n, plural, =0 {…} one {# …} other {# …}}: keep the variable, the word "plural" and the selectors, translate only the text inside the inner braces; "#" is the number. Add the CLDR categories the target language needs (zero, one, two, few, many, other) and always keep "other".`,
		`"{{" and "}}" are literal braces and must stay as they are. Keep line breaks (\n) in e-mail texts (keys starting with "mail.").`,
		`Entries with "html": true contain markup (<strong>, <em>, <code>, <br>, <a href="…">): keep tags and attributes exactly, translate only the visible text.`,
		`Tone: friendly, short and clear; the German source addresses the user informally ("du") – use the usual tone for user interfaces in the target language. Keep button and menu labels short.`,
		`Do not translate product names (UCNG, Beacon, GIPHY, Tenor), environment variables (e.g. PUBLIC_URL), code, file names, URLs, keyboard keys or command examples (e.g. !würfel).`,
	}
	if ref != "" {
		out = append(out, `"reference" is the English translation – use it to understand the meaning, but translate from the German "source".`)
	}
	return out
}

// parsePackage liest ein Übersetzungspaket (texts → text). Leere Texte zählen als nicht übersetzt.
func parsePackage(raw map[string]json.RawMessage) (map[string]string, Meta, error) {
	var m Meta
	if mb, ok := raw["meta"]; ok {
		json.Unmarshal(mb, &m)
	}
	var texts map[string]json.RawMessage
	if err := json.Unmarshal(raw["texts"], &texts); err != nil {
		return nil, m, errors.New("texts fehlt")
	}
	out := map[string]string{}
	for k, v := range texts {
		var s string
		if json.Unmarshal(v, &s) == nil { // auch einfache Paare key → Text
			out[k] = s
			continue
		}
		var e PackageEntry
		if err := json.Unmarshal(v, &e); err != nil {
			return nil, m, fmt.Errorf("%s: kein Text", k)
		}
		out[k] = e.Text
	}
	return out, m, nil
}

// Validate prüft eine Sprachdatei oder ein Paket gegen die Ausgangssprache und liefert die gültigen
// Texte (ohne leere und unbekannte). Für Sprachpakete, die nicht sofort importiert werden (Beacon).
func (c *Catalog) Validate(data []byte) (map[string]string, Meta, ImportResult, error) {
	texts, m, err := parseFile(data)
	if err != nil {
		return nil, m, ImportResult{}, fmt.Errorf("Keine gültige Sprachdatei: %v", err)
	}
	var res ImportResult
	out := map[string]string{}
	keys := make([]string, 0, len(texts))
	for k := range texts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range keys {
		text := texts[k]
		src, ok := c.official[Source][k]
		if !ok {
			res.Unknown++
			continue
		}
		if text == "" {
			res.Unchanged++
			continue
		}
		if len([]rune(text)) > maxTextLen {
			res.Errors = appendErr(res.Errors, k+": "+ErrTextLen.Error())
			continue
		}
		if _, err := Check(k, src, text); err != nil {
			res.Errors = appendErr(res.Errors, k+": "+err.Error())
			continue
		}
		out[k] = text
		res.Applied++
	}
	return out, m, res, nil
}

// Total: Anzahl der Texte der Ausgangssprache.
func (c *Catalog) Total() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.keys)
}

// LangFile baut eine einfache Sprachdatei (_meta + key → Text, sortiert).
func LangFile(m Meta, texts map[string]string) []byte {
	keys := make([]string, 0, len(texts))
	for k := range texts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{\n")
	mb, _ := json.Marshal(m)
	b.WriteString(`  "_meta": ` + string(mb))
	for _, k := range keys {
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(texts[k])
		b.WriteString(",\n  " + string(kb) + ": " + string(vb))
	}
	b.WriteString("\n}\n")
	return []byte(b.String())
}
