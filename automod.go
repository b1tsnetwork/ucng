// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// Automoderation: Nachrichten, die ein gesperrtes Wort oder Muster enthalten,
// werden sofort ausgeblendet und landen in der Prüfliste des Admin-Bereichs.
//
// Muster (eins pro Zeile im Admin-Bereich):
//   arschloch      ganzes Wort
//   arsch*         Wortanfang  (Arsch, Arschloch, ARSCHGEIGE …)
//   *loch          Wortende    (Arschloch, Loch …)
//   arsc*och       Anfang und Ende (Arschloch, Arschkoch …)
//   *idiot*        Wortteil    (Vollidiot, Idioten …)
//   du idiot       mehrere Wörter hintereinander
//
// Sowohl Muster als auch Nachrichten werden vor dem Vergleich vereinheitlicht:
//   - Groß-/Kleinschreibung egal
//   - Zahlen und Zeichen als Buchstaben: 0=o 1=i 3=e 4=a 5=s 7=t 8=b @=a $=s €=e !=i |=i
//   - Umlaute und Akzente: ä=a ö=o ü=u ß=ss é=e …
//   - Wiederholte Buchstaben zählen einfach: Arrrrsch = Arsch
//   - Trennzeichen im Wort werden ignoriert: A.r.s.c.h, Arsch-loch, Arsch_loch
//   - Einzeln gesetzte Buchstaben werden zusammengezogen: "A r s c h"

// leetMap ersetzt typische Tarnungen durch Buchstaben.
var leetMap = map[rune]rune{
	'0': 'o', '1': 'i', '3': 'e', '4': 'a', '5': 's', '7': 't', '8': 'b',
	'@': 'a', '$': 's', '€': 'e', '!': 'i', '|': 'i',
}

// foldMap entfernt Umlaute und Akzente.
var foldMap = map[rune]string{
	'ä': "a", 'ö': "o", 'ü': "u", 'ß': "ss",
	'á': "a", 'à': "a", 'â': "a", 'ã': "a", 'å': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'õ': "o", 'ø': "o",
	'ú': "u", 'ù': "u", 'û': "u",
	'ç': "c", 'ñ': "n", 'ý': "y", 'ÿ': "y",
}

// joiners trennen Wörter nicht, sondern werden innerhalb eines Wortes entfernt.
func isJoiner(r rune) bool {
	return r == '.' || r == '-' || r == '_' || r == '*' || r == '\'' || r == '’'
}

func isWordRune(r rune) bool {
	_, leet := leetMap[r]
	return unicode.IsLetter(r) || unicode.IsDigit(r) || leet
}

// normalizeWord vereinheitlicht ein einzelnes Wort (ohne Wildcards).
func normalizeWord(w string) string { return normalizeWordAs(w, 'i') }

// normalizeWordAs liest "1" und "|" als oneAs ('i' oder 'l').
func normalizeWordAs(w string, oneAs rune) string {
	var b strings.Builder
	for _, r := range strings.ToLower(w) {
		if r == '1' || r == '|' {
			b.WriteRune(oneAs)
		} else if l, ok := leetMap[r]; ok {
			b.WriteRune(l)
		} else if f, ok := foldMap[r]; ok {
			b.WriteString(f)
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
		// alles andere (Joiner, Satzzeichen) fällt weg
	}
	return collapseRepeats(b.String())
}

func collapseRepeats(s string) string {
	var b strings.Builder
	var last rune = -1
	for _, r := range s {
		if r != last {
			b.WriteRune(r)
		}
		last = r
	}
	return b.String()
}

// edgeTrim entfernt Satzzeichen am Wortrand, die sonst als Buchstaben gelesen würden
// ("Idiot!" ist "idiot", nicht "idioti"). @, $ und € bleiben – sie tarnen meist Buchstaben.
func edgeTrim(w string) string {
	return strings.TrimFunc(w, func(r rune) bool { return r == '!' || r == '|' || isJoiner(r) })
}

// normalizeText macht aus einer Nachricht eine Folge vereinheitlichter Wörter,
// getrennt durch einzelne Leerzeichen.
func normalizeText(text string) string { return normalizeTextAs(text, 'i') }

// normalizeVariants liefert alle Lesarten, die geprüft werden müssen:
// "1" und "|" können für i oder l stehen.
func normalizeVariants(text string) []string {
	v := []string{normalizeTextAs(text, 'i')}
	if strings.ContainsAny(text, "1|") {
		if l := normalizeTextAs(text, 'l'); l != v[0] {
			v = append(v, l)
		}
	}
	return v
}

func normalizeTextAs(text string, oneAs rune) string {
	// 1. In Wörter zerlegen; Joiner bleiben Teil des Wortes.
	var raw []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			raw = append(raw, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(text) {
		if isWordRune(r) || isJoiner(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()

	// 2. Wörter vereinheitlichen; Läufe aus Einzelbuchstaben ("a r s c h") zusammenziehen.
	var words []string
	var run []string
	endRun := func() {
		if len(run) >= 3 {
			words = append(words, collapseRepeats(strings.Join(run, "")))
		} else {
			words = append(words, run...)
		}
		run = run[:0]
	}
	for _, w := range raw {
		n := normalizeWordAs(edgeTrim(w), oneAs)
		if n == "" {
			continue
		}
		if len([]rune(n)) == 1 {
			run = append(run, n)
			continue
		}
		endRun()
		words = append(words, n)
	}
	endRun()
	return strings.Join(words, " ")
}

// Muster werden nach Art einsortiert, damit auch sehr lange Listen schnell bleiben:
// ganze Wörter, Wortanfänge (wort*), Wortenden (*wort) und Wortteile (*wort*) werden
// über Tabellen nachgeschlagen; nur der Rest (a*b, Wortfolgen) läuft über reguläre Ausdrücke.
type patternKind int

const (
	kindExact patternKind = iota
	kindPrefix
	kindSuffix
	kindInfix
	kindAffix // anfang*ende
	kindRegex
)

type compiledPattern struct {
	src  string
	kind patternKind
	key  string         // vereinheitlichter Teil für Tabellen-Arten
	end  string         // Wortende bei anfang*ende
	re   *regexp.Regexp // nur für kindRegex
}

// compilePattern zerlegt ein Muster wie "arsch*", "*loch", "arsc*och" oder "du idiot*".
func compilePattern(p string) (compiledPattern, error) {
	src := strings.Join(strings.Fields(p), " ")
	if src == "" {
		return compiledPattern{}, errors.New("leeres Muster")
	}
	words := strings.Fields(strings.ToLower(src))
	var parts []string
	var segsOfFirst []string
	letters := 0
	for wi, word := range words {
		segs := strings.Split(word, "*")
		norm := make([]string, len(segs))
		for i, seg := range segs {
			norm[i] = normalizeWord(seg)
			letters += len([]rune(norm[i]))
		}
		if wi == 0 {
			segsOfFirst = norm
		}
		quoted := make([]string, len(norm))
		for i, n := range norm {
			quoted[i] = regexp.QuoteMeta(n)
		}
		parts = append(parts, strings.Join(quoted, `[^ ]*`))
	}
	if letters < 2 {
		return compiledPattern{}, fmt.Errorf("„%s“ ist zu kurz – mindestens zwei Buchstaben neben dem *", src)
	}
	c := compiledPattern{src: src, kind: kindRegex}
	if len(words) == 1 {
		s := segsOfFirst
		switch {
		case len(s) == 1:
			c.kind, c.key = kindExact, s[0]
		case len(s) == 2 && s[0] != "" && s[1] == "":
			c.kind, c.key = kindPrefix, s[0]
		case len(s) == 2 && s[0] == "" && s[1] != "":
			c.kind, c.key = kindSuffix, s[1]
		case len(s) == 3 && s[0] == "" && s[2] == "" && s[1] != "":
			c.kind, c.key = kindInfix, s[1]
		case len(s) == 2 && s[0] != "" && s[1] != "":
			c.kind, c.key, c.end = kindAffix, s[0], s[1]
		}
	}
	if c.kind == kindRegex {
		re, err := regexp.Compile(`(?:^| )` + strings.Join(parts, " ") + `(?: |$)`)
		if err != nil {
			return compiledPattern{}, fmt.Errorf("„%s“ ist kein gültiges Muster", src)
		}
		c.re = re
		c.key = re.String()
	}
	return c, nil
}

// maxPatternLen begrenzt nur die Länge eines einzelnen Musters, nicht die Anzahl.
const maxPatternLen = 200

type indexed struct {
	idx int // Position in der Liste – bei mehreren Treffern gewinnt das weiter oben stehende Muster
	src string
	end string // nur bei anfang*ende
}

// matcher ist die vorbereitete, schnell durchsuchbare Form der Regeln.
type matcher struct {
	exact, prefix, suffix, infix map[string]indexed
	affix                        map[string][]indexed // Anfang → mögliche Enden
	regex                        []indexed
	regexps                      []*regexp.Regexp
	count                        int
}

func newMatcher(ps []compiledPattern) *matcher {
	m := &matcher{exact: map[string]indexed{}, prefix: map[string]indexed{}, suffix: map[string]indexed{}, infix: map[string]indexed{}, affix: map[string][]indexed{}}
	for i, p := range ps {
		e := indexed{idx: i, src: p.src}
		switch p.kind {
		case kindExact:
			m.exact[p.key] = e
		case kindPrefix:
			m.prefix[p.key] = e
		case kindSuffix:
			m.suffix[p.key] = e
		case kindInfix:
			m.infix[p.key] = e
		case kindAffix:
			e.end = p.end
			m.affix[p.key] = append(m.affix[p.key], e)
		default:
			m.regex = append(m.regex, e)
			m.regexps = append(m.regexps, p.re)
		}
	}
	m.count = len(ps)
	return m
}

// match prüft einen vereinheitlichten Text und liefert das am weitesten oben stehende Muster.
func (m *matcher) match(normalized string) (string, bool) {
	best := indexed{idx: -1}
	take := func(e indexed, ok bool) {
		if ok && (best.idx < 0 || e.idx < best.idx) {
			best = e
		}
	}
	for _, w := range strings.Split(normalized, " ") {
		if w == "" {
			continue
		}
		e, ok := m.exact[w]
		take(e, ok)
		r := []rune(w)
		for i := 1; i <= len(r); i++ {
			if len(m.prefix) > 0 {
				e, ok = m.prefix[string(r[:i])]
				take(e, ok)
			}
			if len(m.affix) > 0 {
				rest := string(r[i:])
				for _, a := range m.affix[string(r[:i])] {
					take(a, strings.HasSuffix(rest, a.end))
				}
			}
			if len(m.suffix) > 0 {
				e, ok = m.suffix[string(r[len(r)-i:])]
				take(e, ok)
			}
		}
		if len(m.infix) > 0 {
			for i := 0; i < len(r); i++ {
				for j := i + 1; j <= len(r); j++ {
					e, ok = m.infix[string(r[i:j])]
					take(e, ok)
				}
			}
		}
	}
	for i, e := range m.regex {
		if best.idx >= 0 && e.idx > best.idx {
			break // die übrigen stehen weiter unten
		}
		take(e, m.regexps[i].MatchString(normalized))
	}
	return best.src, best.idx >= 0
}

// Automod hält die aktiven Regeln. Sicher für gleichzeitige Nutzung.
type Automod struct {
	mu      sync.RWMutex
	enabled bool
	m       *matcher
	lines   []string // Muster und Kommentare in der eingegebenen Reihenfolge
}

// AutomodConfig ist die gespeicherte und über die API ausgetauschte Konfiguration.
type AutomodConfig struct {
	Enabled  bool     `json:"enabled"`
	Patterns []string `json:"patterns"`
}

func NewAutomod() *Automod { return &Automod{enabled: true, m: newMatcher(nil)} }

// Set prüft und übernimmt neue Regeln. Die Anzahl ist nicht begrenzt; doppelte
// Muster (auch in anderer Schreibweise, z. B. "Arsch*" und "4rsch*") werden zusammengefasst.
func (a *Automod) Set(cfg AutomodConfig) (AutomodConfig, error) {
	var compiled []compiledPattern
	var lines []string
	var errs []string
	seen := map[string]bool{}
	for _, p := range cfg.Patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "#") { // Kommentar: behalten, aber nicht prüfen
			lines = append(lines, p)
			continue
		}
		if len([]rune(p)) > maxPatternLen {
			errs = append(errs, fmt.Sprintf("„%.20s…“ ist zu lang (max. %d Zeichen pro Muster)", p, maxPatternLen))
			continue
		}
		c, err := compilePattern(p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		key := fmt.Sprint(c.kind) + ":" + c.key
		if seen[key] {
			continue
		}
		seen[key] = true
		compiled = append(compiled, c)
		lines = append(lines, c.src)
	}
	if len(errs) > 0 {
		return AutomodConfig{}, &PatternError{Problems: errs}
	}
	m := newMatcher(compiled)
	a.mu.Lock()
	a.enabled = cfg.Enabled
	a.m = m
	a.lines = lines
	a.mu.Unlock()
	return a.Config(), nil
}

// PatternError listet alle ungültigen Muster auf einmal.
type PatternError struct{ Problems []string }

func (e *PatternError) Error() string {
	return fmt.Sprintf("Ungültige Muster: %s", strings.Join(e.Problems, "; "))
}

func (a *Automod) Config() AutomodConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return AutomodConfig{Enabled: a.enabled, Patterns: append([]string{}, a.lines...)}
}

// Check liefert das passende Muster, sofern die Automoderation aktiv ist.
func (a *Automod) Check(text string) (string, bool) {
	a.mu.RLock()
	m, enabled := a.m, a.enabled
	a.mu.RUnlock()
	if !enabled || m.count == 0 {
		return "", false
	}
	for _, n := range normalizeVariants(text) {
		if p, ok := m.match(n); ok {
			return p, true
		}
	}
	return "", false
}

// Test prüft unabhängig vom Schalter – für das Testfeld im Admin-Bereich.
func (a *Automod) Test(text string) (pattern, normalized string, ok bool) {
	a.mu.RLock()
	m := a.m
	a.mu.RUnlock()
	for i, n := range normalizeVariants(text) {
		if i == 0 {
			normalized = n
		}
		if pattern, ok = m.match(n); ok {
			normalized = n
			return
		}
	}
	return
}
