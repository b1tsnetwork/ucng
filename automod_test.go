// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func testAutomod(t *testing.T, patterns ...string) *Automod {
	t.Helper()
	a := NewAutomod()
	if _, err := a.Set(AutomodConfig{Enabled: true, Patterns: patterns}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAutomodPatterns(t *testing.T) {
	a := testAutomod(t, "Arsch*", "*loch", "Arsc*och", "idiot", "du opfer")
	hits := map[string]string{
		"Du Arsch":                  "Arsch*",
		"so ein ARSCHLOCH":          "Arsch*",
		"arschgeige!!":              "Arsch*",
		"4rsch":                     "Arsch*",
		"@rsch":                     "Arsch*",
		"Arrrrrsch":                 "Arsch*",
		"A.r.s.c.h":                 "Arsch*",
		"a r s c h":                 "Arsch*",
		"Ärsche":                    "Arsch*",
		"Was für ein Loch!":         "*loch",
		"Idiot!":                    "idiot",
		"1diot":                     "idiot",
		"IDI0T":                     "idiot",
		"i-d-i-o-t":                 "idiot",
		"Du Opfer":                  "du opfer",
		"du   0pf3r":                "du opfer",
		"Hallo, du Idiot, wirklich": "idiot",
	}
	for text, want := range hits {
		got, ok := a.Check(text)
		if !ok || got != want {
			t.Errorf("%q: erwartet Treffer %q, bekam %q (%v) – normalisiert: %q", text, want, got, ok, normalizeText(text))
		}
	}
	// Arsc*och allein (ohne die anderen Muster) trifft Arschloch, auch getarnt.
	b := testAutomod(t, "Arsc*och")
	for _, text := range []string{"Arschloch", "arschl0ch", "Arsch-loch", "ARSCH_LOCH", "4rschl0ch", "Arschloooch"} {
		if _, ok := b.Check(text); !ok {
			t.Errorf("Arsc*och sollte %q treffen (normalisiert %q)", text, normalizeText(text))
		}
	}
	// Harmlose Texte dürfen nicht anschlagen.
	harmless := testAutomod(t, "Arsch*", "Arsc*och", "idiot", "du opfer")
	for _, text := range []string{
		"Hallo zusammen!", "Ich bin Marsch-Fan", "Barschfilet gibt es heute",
		"Das ist idiotensicher", "Das Opferfest", "Du, Opa!", "Arbeitsschutz", "Sie sagte: Ach so!",
		"Das kostet 4 Euro", "Termin um 10:30", "Hallo!!!", "Ja | Nein",
	} {
		if got, ok := harmless.Check(text); ok {
			t.Errorf("%q sollte nicht treffen (Muster %q, normalisiert %q)", text, got, normalizeText(text))
		}
	}
}

func TestAutomodWildcardInside(t *testing.T) {
	a := testAutomod(t, "*idiot*")
	for _, text := range []string{"Vollidiot", "Idioten", "vollidioten!!", "Idiot"} {
		if _, ok := a.Check(text); !ok {
			t.Errorf("*idiot* sollte %q treffen", text)
		}
	}
}

func TestAutomodOneAsL(t *testing.T) {
	a := testAutomod(t, "*loch")
	for _, text := range []string{"Arschl0ch", "Arsch1och", "Arsch|och"} {
		if _, ok := a.Check(text); !ok {
			t.Errorf("*loch sollte %q treffen (1 bzw. | als l)", text)
		}
	}
}

func TestAutomodDisabledAndValidation(t *testing.T) {
	a := testAutomod(t, "arsch*")
	a.Set(AutomodConfig{Enabled: false, Patterns: []string{"arsch*"}})
	if _, ok := a.Check("Arsch"); ok {
		t.Fatal("ausgeschaltete Automod schlägt an")
	}
	if _, _, ok := a.Test("Arsch"); !ok {
		t.Fatal("Testfeld soll auch bei ausgeschalteter Automod prüfen")
	}
	for _, bad := range []string{"*", "a*", "**", "x"} {
		if _, err := a.Set(AutomodConfig{Enabled: true, Patterns: []string{bad}}); err == nil {
			t.Errorf("Muster %q hätte abgelehnt werden müssen", bad)
		}
	}
	cfg, err := a.Set(AutomodConfig{Enabled: true, Patterns: []string{"  Arsch* ", "arsch*", "", "# Kommentar", "ARSCH*"}})
	if err != nil || len(cfg.Patterns) != 2 || cfg.Patterns[0] != "Arsch*" || cfg.Patterns[1] != "# Kommentar" {
		t.Fatalf("Duplikate/Leerzeilen/Kommentare: %+v %v", cfg, err)
	}
}

func TestAutomodManyPatterns(t *testing.T) {
	// Weit mehr als die frühere Grenze von 1000 – und die Prüfung bleibt schnell.
	patterns := []string{"# Riesige Liste"}
	for i := 0; i < 50000; i++ {
		w := "wort" + letterCode(i) + "x"
		switch i % 5 {
		case 0:
			patterns = append(patterns, w)
		case 1:
			patterns = append(patterns, w+"*")
		case 2:
			patterns = append(patterns, "*"+w)
		case 3:
			patterns = append(patterns, "*"+w+"*")
		case 4:
			patterns = append(patterns, w+"*ende")
		}
		if i%1000 == 0 { // ein paar seltene, komplexe Formen (laufen über reguläre Ausdrücke)
			patterns = append(patterns, w+"*mit*mitte", "du "+w)
		}
	}
	patterns = append(patterns, "Arsch*", "*loch", "Arsc*och")
	start := time.Now()
	a := testAutomod(t, patterns...)
	total := len(a.Config().Patterns) - 1
	t.Logf("%d Muster übernommen in %v", total, time.Since(start))
	if total != 50103 {
		t.Fatalf("erwartet 50.103 Muster, bekam %d", total)
	}

	for text, want := range map[string]string{
		"hallo " + "wort" + letterCode(49995) + "x": "wort" + letterCode(49995) + "x",
		"wort" + letterCode(49996) + "xabc":         "wort" + letterCode(49996) + "x*",
		"abcwort" + letterCode(49997) + "x":         "*wort" + letterCode(49997) + "x",
		"xxwort" + letterCode(49998) + "xyy":        "*wort" + letterCode(49998) + "x*",
		"Du 4rschl0ch":                              "Arsch*",
		"So ein schlüsselloch!!":                    "*loch",
	} {
		if got, ok := a.Check(text); !ok || got != want {
			t.Errorf("%q: erwartet %q, bekam %q", text, want, got)
		}
	}

	msg := strings.Repeat("Das ist ein ganz normaler Satz ohne Auffälligkeiten. ", 35) // ~2000 Zeichen
	start = time.Now()
	const n = 200
	for i := 0; i < n; i++ {
		if _, ok := a.Check(msg); ok {
			t.Fatal("harmloser Text getroffen")
		}
	}
	per := time.Since(start) / n
	t.Logf("Prüfung einer 2000-Zeichen-Nachricht gegen %d Muster: %v", total, per)
	if per > 20*time.Millisecond && !raceEnabled {
		t.Fatalf("zu langsam: %v pro Nachricht", per)
	}
}

func TestAutomodPriority(t *testing.T) {
	// Bei mehreren Treffern gewinnt das weiter oben stehende Muster.
	a := testAutomod(t, "*loch", "Arsch*")
	if got, _ := a.Check("Arschloch"); got != "*loch" {
		t.Fatalf("erwartet *loch, bekam %q", got)
	}
}

// letterCode macht aus einer Zahl eine eindeutige Buchstabenfolge ohne Ziffern und
// ohne doppelte Buchstaben (beides würde bei der Vereinheitlichung zusammenfallen).
func letterCode(i int) string {
	syl := []string{"ka", "ke", "ki", "ko", "ku", "la", "le", "li", "lo", "lu"}
	var b strings.Builder
	for _, d := range fmt.Sprint(i) {
		b.WriteString(syl[d-'0'])
	}
	return b.String()
}
