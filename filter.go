// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"unicode"
)

// Wortfilter: einzelne Wörter werden durch Sternchen ersetzt, statt die Nachricht
// auszublenden (z. B. für harmlose Schimpfwörter). Gleiche Muster wie bei der Automod,
// inklusive Tarnungs-Erkennung; geprüft wird Wort für Wort.

type FilterConfig struct {
	Patterns []string `json:"patterns"`
}

func isFilterWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '@' || r == '$' || r == '€' || r == '|'
}

// applyFilter ersetzt gefilterte Wörter. Liefert den neuen Text und ob etwas ersetzt wurde.
func (h *Hub) applyFilter(text string) (string, bool) {
	if text == "" || len(h.filter.Config().Patterns) == 0 {
		return text, false
	}
	var b strings.Builder
	changed := false
	rs := []rune(text)
	for i := 0; i < len(rs); {
		if !isFilterWordRune(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && isFilterWordRune(rs[j]) {
			j++
		}
		word := string(rs[i:j])
		// @Erwähnungen und Links nicht anfassen
		if rs[i] != '@' && !strings.Contains(strings.ToLower(text), "://"+strings.ToLower(word)) {
			if _, hit := h.filter.Check(word); hit {
				b.WriteString(strings.Repeat("*", j-i))
				changed = true
				i = j
				continue
			}
		}
		b.WriteString(word)
		i = j
	}
	return b.String(), changed
}

// SetFilter speichert die Muster des Wortfilters.
func (h *Hub) SetFilter(cfg FilterConfig) (FilterConfig, error) {
	out, err := h.filter.Set(AutomodConfig{Enabled: true, Patterns: cfg.Patterns})
	if err != nil {
		return FilterConfig{}, err
	}
	h.touchDoc("filter.json")
	return FilterConfig{Patterns: out.Patterns}, nil
}

func (h *Hub) Filter() FilterConfig { return FilterConfig{Patterns: h.filter.Config().Patterns} }
