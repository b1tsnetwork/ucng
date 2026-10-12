// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package i18n: Übersetzungen über Sprachdateien.
//
// Offizielle Sprachdateien (JSON, eingebettet) enthalten flache Schlüssel:
//
//	{"_meta": {"name": "English", "locale": "en-GB"}, "app.send": "Send", "app.unread": "{n} unread"}
//
// Platzhalter stehen in geschweiften Klammern: {name}. Mehrzahl im ICU-Stil:
// {n, plural, =0 {keine} one {# Nachricht} other {# Nachrichten}} – # steht für die Zahl.
// {{ und }} ergeben wörtliche Klammern.
//
// Admins können jeden Text überschreiben und eigene Sprachen anlegen. Diese Änderungen liegen
// getrennt von den offiziellen Dateien (Overrides) und bleiben bei Updates erhalten. Zu jeder
// Änderung wird der offizielle Text gemerkt, den sie ersetzt hat – ändert ein Update diesen Text,
// ist die Änderung als „veraltet“ markiert.
//
// Schlüssel mit dem Präfix "srv." sind Meldungen des Servers. Ihr Schlüssel ist der deutsche
// Originaltext bzw. dessen Go-Format (%s, %d, %v, %q); Übersetzungen setzen Werte mit {1}, {2} … ein.
package i18n

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source ist die Sprache, in der die Texte geschrieben werden (vollständig, Rückfall für alles).
const Source = "de"

const (
	maxTextLen   = 4000
	maxLanguages = 40
	srvPrefix    = "srv."
	metaKey      = "_meta"
)

var (
	codeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

	ErrLangCode   = errors.New("Sprachcode wie de, en, fr oder pt-BR")
	ErrLangName   = errors.New("Name der Sprache: 1–40 Zeichen")
	ErrLangExists = errors.New("Diese Sprache gibt es schon")
	ErrNoLang     = errors.New("Diese Sprache gibt es nicht")
	ErrOfficial   = errors.New("Offizielle Sprachen lassen sich nicht löschen – nur ausblenden")
	ErrNoKey      = errors.New("Diesen Text gibt es nicht")
	ErrTextLen    = errors.New("Text: höchstens 4000 Zeichen")
	ErrTooMany    = errors.New("Höchstens 40 Sprachen")
	ErrSource     = errors.New("Die Ausgangssprache kann nicht ausgeblendet werden")
)

// Meta beschreibt eine Sprache.
type Meta struct {
	Name   string `json:"name"`
	Locale string `json:"locale,omitempty"` // für Datums- und Zahlenformate, z. B. de-DE
	Dir    string `json:"dir,omitempty"`    // rtl für Arabisch, Hebräisch …
}

// Override ist eine Änderung durch die Admins.
type Override struct {
	Text string    `json:"text"`
	Base string    `json:"base"` // offizieller Text zum Zeitpunkt der Änderung
	At   time.Time `json:"at"`
}

// State ist alles, was gespeichert wird (Admin-Änderungen und Einstellungen).
type State struct {
	Default  string                         `json:"default"`           // Sprache für neue Besucher
	Auto     bool                           `json:"auto"`              // Browsersprache bevorzugen
	Hidden   []string                       `json:"hidden,omitempty"`  // nicht in der Auswahl
	Custom   map[string]Meta                `json:"custom,omitempty"`  // eigene Sprachen
	MetaOver map[string]Meta                `json:"meta,omitempty"`    // geänderte Namen/Locale offizieller Sprachen
	Texts    map[string]map[string]Override `json:"texts,omitempty"`   // Sprache → Schlüssel → Änderung
	Packs    map[string]PackSource          `json:"packs,omitempty"`   // aus einem Beacon installierte Sprachpakete
	Version  int64                          `json:"version,omitempty"` // steigt bei jeder Änderung
}

// PackSource: Herkunft eines installierten Sprachpakets (für „Update verfügbar“).
type PackSource struct {
	URL       string    `json:"url"`
	Updated   time.Time `json:"updated"`   // Stand des Pakets beim Beacon
	Installed time.Time `json:"installed"` // wann übernommen
}

// Catalog hält offizielle Texte und Änderungen.
type Catalog struct {
	mu       sync.RWMutex
	official map[string]map[string]string
	meta     map[string]Meta
	keys     []string // alle Schlüssel der Ausgangssprache, sortiert
	st       State
	patterns []srvPattern
	cache    map[string]map[string]string // Sprache → zusammengeführte Texte
	changed  func()
}

type srvPattern struct {
	key   string
	re    *regexp.Regexp
	verbs int
}

// New lädt alle *.json aus dir im Dateisystem fsys.
func New(fsys fs.FS, dir string) (*Catalog, error) {
	c := &Catalog{official: map[string]map[string]string{}, meta: map[string]Meta{},
		st: State{Default: Source, Auto: true}}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		if !codeRe.MatchString(code) {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		texts, m, err := parseFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if m.Name == "" {
			m.Name = code
		}
		c.official[code] = texts
		c.meta[code] = m
	}
	src, ok := c.official[Source]
	if !ok {
		return nil, fmt.Errorf("Sprachdatei %s.json fehlt", Source)
	}
	for k := range src {
		c.keys = append(c.keys, k)
		if strings.HasPrefix(k, srvPrefix) && strings.ContainsRune(k, '%') {
			if p, ok := compilePattern(k); ok {
				c.patterns = append(c.patterns, p)
			}
		}
	}
	sort.Strings(c.keys)
	// längere Muster zuerst (spezifischer)
	sort.Slice(c.patterns, func(i, j int) bool { return len(c.patterns[i].key) > len(c.patterns[j].key) })
	c.cache = map[string]map[string]string{}
	return c, nil
}

func parseFile(data []byte) (map[string]string, Meta, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, Meta{}, err
	}
	if f, ok := raw["format"]; ok { // Übersetzungspaket
		var format string
		json.Unmarshal(f, &format)
		if format != PackageFormat && format != legacyPackageFormat {
			return nil, Meta{}, fmt.Errorf("unbekanntes Format %q", format)
		}
		return parsePackage(raw)
	}
	texts := map[string]string{}
	var m Meta
	for k, v := range raw {
		if k == metaKey {
			if err := json.Unmarshal(v, &m); err != nil {
				return nil, Meta{}, err
			}
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, Meta{}, fmt.Errorf("%s: kein Text", k)
		}
		texts[k] = s
	}
	return texts, m, nil
}

// Fresh liefert einen neuen Katalog mit denselben offiziellen Texten, aber ohne Änderungen.
func (c *Catalog) Fresh() *Catalog {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &Catalog{official: c.official, meta: c.meta, keys: c.keys, patterns: c.patterns,
		st: State{Default: Source, Auto: true}, cache: map[string]map[string]string{}}
}

// OnChange wird nach jeder Änderung aufgerufen (Speichern).
func (c *Catalog) OnChange(f func()) { c.changed = f }

func (c *Catalog) touchLocked() {
	c.st.Version++
	c.cache = map[string]map[string]string{}
}

func (c *Catalog) notify() {
	if c.changed != nil {
		c.changed()
	}
}

// Export liefert den zu speichernden Zustand.
func (c *Catalog) Export() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, _ := json.Marshal(c.st)
	var out State
	json.Unmarshal(b, &out)
	return out
}

// Restore übernimmt einen gespeicherten Zustand.
func (c *Catalog) Restore(st State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st.Default == "" {
		st.Default = Source
	}
	c.st = st
	c.cache = map[string]map[string]string{}
}

// --- Sprachen ---

// Lang ist eine Sprache in der Auswahl bzw. im Admin-Bereich.
type Lang struct {
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	Locale   string  `json:"locale"`
	Dir      string  `json:"dir,omitempty"`
	Official bool    `json:"official"`
	Hidden   bool    `json:"hidden,omitempty"`
	Complete float64 `json:"complete"` // Anteil übersetzter Texte (0–1)
	Changed  int     `json:"changed"`  // Anzahl eigener Änderungen
	Outdated int     `json:"outdated"` // Änderungen, deren Original sich geändert hat
}

func (c *Catalog) metaLocked(code string) (Meta, bool) {
	m, ok := c.meta[code]
	if cm, cok := c.st.Custom[code]; cok {
		m, ok = cm, true
	}
	if mo, mok := c.st.MetaOver[code]; mok && ok {
		if mo.Name != "" {
			m.Name = mo.Name
		}
		if mo.Locale != "" {
			m.Locale = mo.Locale
		}
		if mo.Dir != "" {
			m.Dir = mo.Dir
		}
	}
	if m.Locale == "" {
		m.Locale = code
	}
	return m, ok
}

func (c *Catalog) existsLocked(code string) bool {
	_, ok := c.metaLocked(code)
	return ok
}

// Languages listet alle Sprachen (all = auch ausgeblendete).
func (c *Catalog) Languages(all bool) []Lang {
	c.mu.RLock()
	defer c.mu.RUnlock()
	codes := map[string]bool{}
	for k := range c.official {
		codes[k] = true
	}
	for k := range c.st.Custom {
		codes[k] = true
	}
	var out []Lang
	for code := range codes {
		hidden := contains(c.st.Hidden, code)
		if hidden && !all {
			continue
		}
		m, _ := c.metaLocked(code)
		l := Lang{Code: code, Name: m.Name, Locale: m.Locale, Dir: m.Dir, Hidden: hidden}
		_, l.Official = c.official[code]
		done := 0
		for _, k := range c.keys {
			if _, ok := c.st.Texts[code][k]; ok {
				done++
			} else if _, ok := c.official[code][k]; ok {
				done++
			}
		}
		if len(c.keys) > 0 {
			l.Complete = float64(done) / float64(len(c.keys))
		}
		for k, o := range c.st.Texts[code] {
			l.Changed++
			if c.outdatedLocked(code, k, o) {
				l.Outdated++
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Code == Source) != (out[j].Code == Source) {
			return out[i].Code == Source
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Settings: Standardsprache und Browsersprache.
func (c *Catalog) Settings() (def string, auto bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.st.Default, c.st.Auto
}

func (c *Catalog) SetSettings(def string, auto bool, hidden []string) error {
	c.mu.Lock()
	if !c.existsLocked(def) {
		c.mu.Unlock()
		return ErrNoLang
	}
	var h []string
	for _, code := range hidden {
		if code == def || code == Source {
			continue
		}
		if c.existsLocked(code) && !contains(h, code) {
			h = append(h, code)
		}
	}
	c.st.Default, c.st.Auto, c.st.Hidden = def, auto, h
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return nil
}

// AddLanguage legt eine eigene Sprache an.
func (c *Catalog) AddLanguage(code, name, locale string) error {
	code = strings.TrimSpace(code)
	name = strings.Join(strings.Fields(name), " ")
	if !codeRe.MatchString(code) {
		return ErrLangCode
	}
	if name == "" || len([]rune(name)) > 40 {
		return ErrLangName
	}
	c.mu.Lock()
	if c.existsLocked(code) {
		c.mu.Unlock()
		return ErrLangExists
	}
	if len(c.official)+len(c.st.Custom) >= maxLanguages {
		c.mu.Unlock()
		return ErrTooMany
	}
	if c.st.Custom == nil {
		c.st.Custom = map[string]Meta{}
	}
	if locale == "" || !codeRe.MatchString(locale) {
		locale = code
	}
	c.st.Custom[code] = Meta{Name: name, Locale: locale}
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return nil
}

// SetMeta ändert Name/Locale/Schreibrichtung einer Sprache.
func (c *Catalog) SetMeta(code string, m Meta) error {
	m.Name = strings.Join(strings.Fields(m.Name), " ")
	if m.Name == "" || len([]rune(m.Name)) > 40 {
		return ErrLangName
	}
	if m.Locale != "" && !codeRe.MatchString(m.Locale) {
		return ErrLangCode
	}
	if m.Dir != "" && m.Dir != "rtl" && m.Dir != "ltr" {
		m.Dir = ""
	}
	c.mu.Lock()
	switch {
	case c.st.Custom[code] != (Meta{}):
		c.st.Custom[code] = m
	case c.official[code] != nil:
		if c.st.MetaOver == nil {
			c.st.MetaOver = map[string]Meta{}
		}
		c.st.MetaOver[code] = m
	default:
		c.mu.Unlock()
		return ErrNoLang
	}
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return nil
}

// DeleteLanguage löscht eine eigene Sprache samt Texten.
func (c *Catalog) DeleteLanguage(code string) error {
	c.mu.Lock()
	if _, ok := c.official[code]; ok {
		c.mu.Unlock()
		return ErrOfficial
	}
	if _, ok := c.st.Custom[code]; !ok {
		c.mu.Unlock()
		return ErrNoLang
	}
	delete(c.st.Custom, code)
	delete(c.st.Texts, code)
	delete(c.st.Packs, code)
	if c.st.Default == code {
		c.st.Default = Source
	}
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return nil
}

// --- Texte ---

// Dict liefert die zusammengeführten Texte einer Sprache (nur Schlüssel mit einem der Präfixe;
// keine Präfixe = alle). Fehlende Texte kommen aus der Ausgangssprache.
func (c *Catalog) Dict(lang string, prefixes ...string) map[string]string {
	all := c.merged(lang)
	if len(prefixes) == 0 {
		return all
	}
	out := map[string]string{}
	for k, v := range all {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				out[k] = v
				break
			}
		}
	}
	return out
}

func (c *Catalog) merged(lang string) map[string]string {
	c.mu.RLock()
	if m, ok := c.cache[lang]; ok {
		c.mu.RUnlock()
		return m
	}
	m := make(map[string]string, len(c.keys))
	for k, v := range c.official[Source] {
		m[k] = v
	}
	for k, o := range c.st.Texts[Source] {
		m[k] = o.Text
	}
	if lang != Source {
		for k, v := range c.official[lang] {
			if _, known := m[k]; known {
				m[k] = v
			}
		}
		for k, o := range c.st.Texts[lang] {
			if _, known := m[k]; known {
				m[k] = o.Text
			}
		}
	}
	c.mu.RUnlock()
	c.mu.Lock()
	c.cache[lang] = m
	c.mu.Unlock()
	return m
}

// Version ändert sich, sobald sich Texte ändern (für ETags).
func (c *Catalog) Version(lang string) string {
	c.mu.RLock()
	v := c.st.Version
	c.mu.RUnlock()
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", lang, v, len(c.keys))))
	return hex.EncodeToString(h[:8])
}

// Meta liefert Name/Locale einer Sprache.
func (c *Catalog) Meta(lang string) Meta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, _ := c.metaLocked(lang)
	return m
}

// Has: gibt es diese (sichtbare oder ausgeblendete) Sprache?
func (c *Catalog) Has(lang string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.existsLocked(lang)
}

// Entry ist eine Zeile im Sprach-Editor.
type Entry struct {
	Key      string    `json:"key"`
	Source   string    `json:"source"`             // Text der Ausgangssprache (offiziell)
	Official string    `json:"official"`           // offizielle Übersetzung (leer = keine)
	Text     string    `json:"text,omitempty"`     // eigene Änderung
	Changed  bool      `json:"changed,omitempty"`  // eigene Änderung vorhanden
	Outdated bool      `json:"outdated,omitempty"` // Original hat sich seit der Änderung geändert
	Base     string    `json:"base,omitempty"`     // Original zum Zeitpunkt der Änderung
	At       time.Time `json:"at,omitempty"`
	Vars     []string  `json:"vars"`
}

// Entries liefert alle Texte einer Sprache für den Editor.
func (c *Catalog) Entries(lang string) ([]Entry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.existsLocked(lang) {
		return nil, ErrNoLang
	}
	out := make([]Entry, 0, len(c.keys))
	for _, k := range c.keys {
		src := c.official[Source][k]
		e := Entry{Key: k, Source: src, Official: c.official[lang][k], Vars: Vars(k, src)}
		if lang == Source {
			e.Official = src
		}
		if o, ok := c.st.Texts[lang][k]; ok {
			e.Text, e.Changed, e.Base, e.At = o.Text, true, o.Base, o.At
			e.Outdated = c.outdatedLocked(lang, k, o)
		}
		out = append(out, e)
	}
	return out, nil
}

// SetText speichert eine Änderung (text leer = Änderung entfernen). Liefert Warnungen.
func (c *Catalog) SetText(lang, key, text string) ([]string, error) {
	c.mu.Lock()
	if !c.existsLocked(lang) {
		c.mu.Unlock()
		return nil, ErrNoLang
	}
	src, ok := c.official[Source][key]
	if !ok {
		c.mu.Unlock()
		return nil, ErrNoKey
	}
	if text == "" {
		if _, had := c.st.Texts[lang][key]; had {
			delete(c.st.Texts[lang], key)
			c.touchLocked()
		}
		c.mu.Unlock()
		c.notify()
		return nil, nil
	}
	if len([]rune(text)) > maxTextLen {
		c.mu.Unlock()
		return nil, ErrTextLen
	}
	warns, err := Check(key, src, text)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	base := c.baseLocked(lang, key)
	if same, ok := c.plainLocked(lang, key); ok && text == same { // entspricht dem Original: keine Änderung nötig
		delete(c.st.Texts[lang], key)
	} else {
		if c.st.Texts == nil {
			c.st.Texts = map[string]map[string]Override{}
		}
		if c.st.Texts[lang] == nil {
			c.st.Texts[lang] = map[string]Override{}
		}
		c.st.Texts[lang][key] = Override{Text: text, Base: base, At: time.Now().UTC()}
	}
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return warns, nil
}

// Confirm markiert eine veraltete Änderung als geprüft (Base = aktuelles Original).
func (c *Catalog) Confirm(lang, key string) error {
	c.mu.Lock()
	o, ok := c.st.Texts[lang][key]
	if !ok {
		c.mu.Unlock()
		return ErrNoKey
	}
	o.Base = c.baseLocked(lang, key)
	c.st.Texts[lang][key] = o
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return nil
}

// ImportResult fasst einen Import zusammen.
type ImportResult struct {
	Applied   int      `json:"applied"`
	Unchanged int      `json:"unchanged"`
	Unknown   int      `json:"unknown"`
	Errors    []string `json:"errors,omitempty"`
}

// Import übernimmt eine Sprachdatei (gleiches Format wie die offiziellen) als Änderungen.
// replace = vorhandene Änderungen dieser Sprache vorher entfernen.
func (c *Catalog) Import(lang string, data []byte, replace bool) (ImportResult, error) {
	texts, m, err := parseFile(data)
	if err != nil {
		return ImportResult{}, fmt.Errorf("Keine gültige Sprachdatei: %v", err)
	}
	var res ImportResult
	c.mu.Lock()
	if !c.existsLocked(lang) {
		c.mu.Unlock()
		return res, ErrNoLang
	}
	if replace {
		delete(c.st.Texts, lang)
	}
	if c.st.Texts == nil {
		c.st.Texts = map[string]map[string]Override{}
	}
	if c.st.Texts[lang] == nil {
		c.st.Texts[lang] = map[string]Override{}
	}
	keys := make([]string, 0, len(texts))
	for k := range texts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		text := texts[k]
		src, ok := c.official[Source][k]
		if !ok {
			res.Unknown++
			continue
		}
		base := c.baseLocked(lang, k)
		if text == "" { // nicht übersetzt (z. B. im Paket offen gelassen): Vorhandenes bleibt
			res.Unchanged++
			continue
		}
		if same, ok := c.plainLocked(lang, k); ok && text == same {
			delete(c.st.Texts[lang], k)
			res.Unchanged++
			continue
		}
		if o, ok := c.st.Texts[lang][k]; ok && o.Text == text { // unverändert: Original und Zeitpunkt behalten
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
		c.st.Texts[lang][k] = Override{Text: text, Base: base, At: time.Now().UTC()}
		res.Applied++
	}
	if _, custom := c.st.Custom[lang]; custom && m.Name != "" {
		cm := c.st.Custom[lang]
		cm.Name = m.Name
		if m.Locale != "" && codeRe.MatchString(m.Locale) {
			cm.Locale = m.Locale
		}
		c.st.Custom[lang] = cm
	}
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return res, nil
}

func appendErr(list []string, e string) []string {
	if len(list) < 50 {
		return append(list, e)
	}
	return list
}

// File liefert eine Sprache als Datei (alle Texte, zusammengeführt; full=false: nur eigene Änderungen).
func (c *Catalog) File(lang string, full bool) ([]byte, error) {
	if !c.Has(lang) {
		return nil, ErrNoLang
	}
	m := c.Meta(lang)
	var texts map[string]string
	if full {
		texts = c.merged(lang)
	} else {
		c.mu.RLock()
		texts = map[string]string{}
		for k, o := range c.st.Texts[lang] {
			texts[k] = o.Text
		}
		c.mu.RUnlock()
	}
	return LangFile(m, texts), nil
}

// --- Formatieren ---

// T übersetzt einen Schlüssel und setzt Werte ein.
func (c *Catalog) T(lang, key string, vars map[string]any) string {
	text, ok := c.merged(lang)[key]
	if !ok {
		return key
	}
	return Format(text, vars, c.Meta(lang).Locale)
}

// Format setzt Werte in einen Text ein ({name}, {n, plural, …}).
func Format(text string, vars map[string]any, locale string) string {
	var b strings.Builder
	formatInto(&b, text, vars, locale, "")
	return b.String()
}

func formatInto(b *strings.Builder, s string, vars map[string]any, locale, hash string) {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '{' && i+1 < len(s) && s[i+1] == '{':
			b.WriteByte('{')
			i++
		case ch == '}' && i+1 < len(s) && s[i+1] == '}':
			b.WriteByte('}')
			i++
		case ch == '#' && hash != "":
			b.WriteString(hash)
		case ch == '{':
			end := matchBrace(s, i)
			if end < 0 {
				b.WriteString(s[i:])
				return
			}
			formatArg(b, s[i+1:end], vars, locale)
			i = end
		default:
			b.WriteByte(ch)
		}
	}
}

// matchBrace sucht die schließende Klammer zu s[i] == '{'.
func matchBrace(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

func formatArg(b *strings.Builder, arg string, vars map[string]any, locale string) {
	name, rest, hasRest := strings.Cut(arg, ",")
	name = strings.TrimSpace(name)
	val, ok := vars[name]
	if !hasRest {
		if !ok {
			b.WriteString("{" + name + "}")
			return
		}
		b.WriteString(fmt.Sprint(val))
		return
	}
	kind, body, _ := strings.Cut(strings.TrimSpace(rest), ",")
	if strings.TrimSpace(kind) != "plural" {
		b.WriteString(fmt.Sprint(val))
		return
	}
	n := toFloat(val)
	cases := parseCases(body)
	choice, found := cases["="+strconv.FormatFloat(n, 'f', -1, 64)]
	if !found {
		choice, found = cases[pluralCategory(locale, n)]
	}
	if !found {
		choice = cases["other"]
	}
	formatInto(b, choice, vars, locale, strconv.FormatFloat(n, 'f', -1, 64))
}

func parseCases(body string) map[string]string {
	out := map[string]string{}
	s := strings.TrimSpace(body)
	for len(s) > 0 {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			break
		}
		sel := strings.TrimSpace(s[:i])
		end := matchBrace(s, i)
		if end < 0 {
			break
		}
		out[sel] = s[i+1 : end]
		s = strings.TrimSpace(s[end+1:])
	}
	return out
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
	return f
}

// pluralCategory: vereinfachte CLDR-Regeln (reicht für die Server-Seite; der Browser nutzt Intl.PluralRules).
func pluralCategory(locale string, n float64) string {
	lang, _, _ := strings.Cut(strings.ToLower(locale), "-")
	switch lang {
	case "fr", "pt":
		if n >= 0 && n < 2 {
			return "one"
		}
	case "ja", "zh", "ko", "vi", "th", "id":
		return "other"
	default:
		if n == 1 {
			return "one"
		}
	}
	return "other"
}

// --- Platzhalter prüfen ---

var srvArgRe = regexp.MustCompile(`\{(\d+)\}`)

// Vars liefert die Platzhalter eines Textes (bei srv.-Meldungen {1}, {2} …).
func Vars(key, text string) []string {
	if strings.HasPrefix(key, srvPrefix) {
		n := countVerbs(strings.TrimPrefix(key, srvPrefix))
		out := make([]string, n)
		for i := range out {
			out[i] = strconv.Itoa(i + 1)
		}
		return out
	}
	set := map[string]bool{}
	collectVars(text, set)
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func collectVars(s string, set map[string]bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '{' && i+1 < len(s) && s[i+1] == '{' {
			i++
			continue
		}
		if s[i] != '{' {
			continue
		}
		end := matchBrace(s, i)
		if end < 0 {
			return
		}
		arg := s[i+1 : end]
		name, rest, hasRest := strings.Cut(arg, ",")
		name = strings.TrimSpace(name)
		if name != "" {
			set[name] = true
		}
		if hasRest {
			if _, body, ok := strings.Cut(rest, ","); ok {
				for _, v := range parseCases(body) {
					collectVars(v, set)
				}
			}
		}
		i = end
	}
}

// Check prüft eine Übersetzung gegen den Ausgangstext: unbekannte Platzhalter und kaputte
// Klammern sind Fehler, fehlende Platzhalter nur eine Warnung.
func Check(key, source, text string) (warns []string, err error) {
	if !strings.HasPrefix(key, srvPrefix) && !balanced(text) {
		return nil, errors.New("Geschweifte Klammern passen nicht zusammen – wörtliche Klammern als {{ und }} schreiben")
	}
	want := Vars(key, source)
	var have []string
	if strings.HasPrefix(key, srvPrefix) {
		set := map[string]bool{}
		for _, m := range srvArgRe.FindAllStringSubmatch(text, -1) {
			set[m[1]] = true
		}
		for v := range set {
			have = append(have, v)
		}
	} else {
		have = Vars(key, text)
	}
	for _, v := range have {
		if !contains(want, v) {
			if len(want) == 0 {
				return nil, fmt.Errorf("Unbekannter Platzhalter {%s} – dieser Text hat keine Platzhalter", v)
			}
			return nil, fmt.Errorf("Unbekannter Platzhalter {%s} – erlaubt: %s", v, joinVars(want))
		}
	}
	for _, v := range want {
		if !contains(have, v) {
			warns = append(warns, fmt.Sprintf("Platzhalter {%s} wird nicht verwendet", v))
		}
	}
	return warns, nil
}

func joinVars(v []string) string {
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = "{" + x + "}"
	}
	return strings.Join(out, ", ")
}

func balanced(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			if depth == 0 && i+1 < len(s) && s[i+1] == '{' {
				i++
				continue
			}
			depth++
		case '}':
			if depth == 0 && i+1 < len(s) && s[i+1] == '}' {
				i++
				continue
			}
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// --- Meldungen des Servers ---

var verbRe = regexp.MustCompile(`%[-+# 0-9.]*[sdvqfx]`)

func countVerbs(format string) int {
	return len(verbRe.FindAllString(strings.ReplaceAll(format, "%%", ""), -1))
}

func compilePattern(key string) (srvPattern, bool) {
	format := strings.TrimPrefix(key, srvPrefix)
	parts := verbRe.Split(format, -1)
	if len(parts) < 2 {
		return srvPattern{}, false
	}
	var re strings.Builder
	re.WriteString("^")
	for i, p := range parts {
		re.WriteString(regexp.QuoteMeta(strings.ReplaceAll(p, "%%", "%")))
		if i < len(parts)-1 {
			re.WriteString("(.*?)")
		}
	}
	re.WriteString("$")
	r, err := regexp.Compile(re.String())
	if err != nil {
		return srvPattern{}, false
	}
	return srvPattern{key: key, re: r, verbs: len(parts) - 1}, true
}

// Message übersetzt eine (deutsche) Meldung des Servers. Unbekannte Meldungen bleiben, wie sie sind.
func (c *Catalog) Message(lang, msg string) string {
	if msg == "" {
		return msg
	}
	dict := c.merged(lang)
	if t, ok := dict[srvPrefix+msg]; ok {
		return t
	}
	for _, p := range c.patternsSnapshot() {
		m := p.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		t := dict[p.key]
		return srvArgRe.ReplaceAllStringFunc(t, func(s string) string {
			n, _ := strconv.Atoi(s[1 : len(s)-1])
			if n >= 1 && n < len(m) {
				return c.Message(lang, m[n]) // eingebettete Meldungen ebenfalls übersetzen
			}
			return s
		})
	}
	return msg
}

func (c *Catalog) patternsSnapshot() []srvPattern {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.patterns
}

// --- Sprache einer Anfrage ---

// CookieName ist das Cookie mit der gewählten Sprache.
const CookieName = "gochat_lang"

// Resolve bestimmt die Sprache einer Anfrage: Header X-Lang, Cookie, Browsersprache, Standard.
func (c *Catalog) Resolve(r *http.Request) string {
	if l := c.pick(r.Header.Get("X-Lang")); l != "" {
		return l
	}
	if ck, err := r.Cookie(CookieName); err == nil {
		if l := c.pick(ck.Value); l != "" {
			return l
		}
	}
	def, auto := c.Settings()
	if auto {
		if l := c.FromAcceptLanguage(r.Header.Get("Accept-Language")); l != "" {
			return l
		}
	}
	return def
}

// pick: gültige, vorhandene Sprache (sonst "").
func (c *Catalog) pick(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 16 || !codeRe.MatchString(code) {
		return ""
	}
	if c.Has(code) {
		return code
	}
	return ""
}

// FromAcceptLanguage wählt die beste sichtbare Sprache aus dem Accept-Language-Header.
func (c *Catalog) FromAcceptLanguage(h string) string {
	type cand struct {
		code string
		q    float64
	}
	var list []cand
	for _, part := range strings.Split(h, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			q, _ = strconv.ParseFloat(v, 64)
		}
		if tag != "" && tag != "*" {
			list = append(list, cand{tag, q})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].q > list[j].q })
	visible := map[string]bool{}
	for _, l := range c.Languages(false) {
		visible[l.Code] = true
	}
	for _, cd := range list {
		tag := cd.code
		if visible[tag] {
			return tag
		}
		base, _, _ := strings.Cut(tag, "-")
		base = strings.ToLower(base)
		if visible[base] {
			return base
		}
	}
	return ""
}

// ApplyState übernimmt eigene Sprachen und Änderungen aus einer Vorlage (anderer Instanz).
// Texte werden wie beim Import geprüft; das gespeicherte Original (Base) bleibt erhalten, damit
// Änderungen, deren Original sich inzwischen geändert hat, als veraltet erscheinen.
// replace: bisherige Änderungen und Einstellungen werden ersetzt; sonst nur ergänzt (Vorhandenes bleibt).
func (c *Catalog) ApplyState(st State, replace bool) ImportResult {
	var res ImportResult
	c.mu.Lock()
	// eigene Sprachen
	codes := make([]string, 0, len(st.Custom))
	for code := range st.Custom {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		m := st.Custom[code]
		m.Name = strings.Join(strings.Fields(m.Name), " ")
		if !codeRe.MatchString(code) || m.Name == "" || len([]rune(m.Name)) > 40 {
			res.Errors = appendErr(res.Errors, code+": "+ErrLangCode.Error())
			continue
		}
		if m.Locale == "" || !codeRe.MatchString(m.Locale) {
			m.Locale = code
		}
		if m.Dir != "rtl" {
			m.Dir = ""
		}
		if _, official := c.official[code]; official {
			continue
		}
		if _, have := c.st.Custom[code]; have && !replace {
			continue
		}
		if c.st.Custom == nil {
			c.st.Custom = map[string]Meta{}
		}
		if _, have := c.st.Custom[code]; !have && len(c.official)+len(c.st.Custom) >= maxLanguages {
			res.Errors = appendErr(res.Errors, code+": "+ErrTooMany.Error())
			continue
		}
		c.st.Custom[code] = m
	}
	// Texte
	if replace {
		c.st.Texts = map[string]map[string]Override{}
	}
	if c.st.Texts == nil {
		c.st.Texts = map[string]map[string]Override{}
	}
	langs := make([]string, 0, len(st.Texts))
	for l := range st.Texts {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		if !c.existsLocked(lang) {
			res.Unknown += len(st.Texts[lang])
			continue
		}
		if c.st.Texts[lang] == nil {
			c.st.Texts[lang] = map[string]Override{}
		}
		for k, o := range st.Texts[lang] {
			src, ok := c.official[Source][k]
			if !ok {
				res.Unknown++
				continue
			}
			if _, have := c.st.Texts[lang][k]; have && !replace {
				res.Unchanged++
				continue
			}
			if o.Text == "" || len([]rune(o.Text)) > maxTextLen {
				res.Errors = appendErr(res.Errors, lang+"/"+k+": "+ErrTextLen.Error())
				continue
			}
			if _, err := Check(k, src, o.Text); err != nil {
				res.Errors = appendErr(res.Errors, lang+"/"+k+": "+err.Error())
				continue
			}
			if o.At.IsZero() {
				o.At = time.Now().UTC()
			}
			c.st.Texts[lang][k] = Override{Text: o.Text, Base: o.Base, At: o.At}
			res.Applied++
		}
	}
	// Einstellungen nur beim Ersetzen
	if replace && st.Default != "" && c.existsLocked(st.Default) {
		c.st.Default, c.st.Auto = st.Default, st.Auto
		c.st.Hidden = nil
		for _, h := range st.Hidden {
			if h != st.Default && h != Source && c.existsLocked(h) && !contains(c.st.Hidden, h) {
				c.st.Hidden = append(c.st.Hidden, h)
			}
		}
	}
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
	return res
}

// Overrides: Anzahl eigener Änderungen über alle Sprachen.
func (c *Catalog) Overrides() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, m := range c.st.Texts {
		n += len(m)
	}
	return n
}

// SetPackSource merkt sich, aus welchem Beacon-Paket eine Sprache stammt.
func (c *Catalog) SetPackSource(lang string, src PackSource) {
	c.mu.Lock()
	if c.st.Packs == nil {
		c.st.Packs = map[string]PackSource{}
	}
	c.st.Packs[lang] = src
	c.touchLocked()
	c.mu.Unlock()
	c.notify()
}

// PackSources liefert die installierten Sprachpakete.
func (c *Catalog) PackSources() map[string]PackSource {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]PackSource, len(c.st.Packs))
	for k, v := range c.st.Packs {
		out[k] = v
	}
	return out
}

// baseLocked: Bezug einer Änderung – die offizielle Übersetzung; ohne sie (eigene Sprachen, neue
// Texte) der deutsche Ausgangstext. Ändert ein Update den Bezug, gilt die Änderung als veraltet.
func (c *Catalog) baseLocked(lang, key string) string {
	if lang != Source {
		if v, ok := c.official[lang][key]; ok {
			return v
		}
	}
	return c.official[Source][key]
}

// plainLocked: der Text, der ohne Änderung gälte – nur wenn es ihn in dieser Sprache gibt.
// Gleiche Texte brauchen dann keine Änderung; in eigenen Sprachen bleibt auch ein Text, der dem
// deutschen gleicht („Community“), eine Übersetzung.
func (c *Catalog) plainLocked(lang, key string) (string, bool) {
	if lang == Source {
		v, ok := c.official[Source][key]
		return v, ok
	}
	v, ok := c.official[lang][key]
	return v, ok
}

func (c *Catalog) outdatedLocked(lang, key string, o Override) bool {
	if o.Base == "" { // ältere Änderungen eigener Sprachen ohne gespeicherten Bezug
		if _, ok := c.official[lang][key]; !ok {
			return false
		}
	}
	return o.Base != c.baseLocked(lang, key)
}

// Base: Bezug einer Übersetzung (offizielle Übersetzung, sonst deutscher Ausgangstext).
func (c *Catalog) Base(lang, key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseLocked(lang, key)
}

// ValidCode: Sprachkürzel nach BCP 47 (z. B. fr, pt-BR).
func ValidCode(code string) bool { return codeRe.MatchString(code) }
