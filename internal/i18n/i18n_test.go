// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package i18n

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	fsys := fstest.MapFS{
		"l/de.json": {Data: []byte(`{"_meta":{"name":"Deutsch","locale":"de-DE"},
			"app.hello":"Hallo {name}!",
			"app.count":"{n, plural, =0 {keine Nachricht} one {# Nachricht} other {# Nachrichten}}",
			"app.brace":"{{wörtlich}} {x}",
			"app.only":"Nur Deutsch",
			"srv.Diesen Kanal gibt es nicht":"Diesen Kanal gibt es nicht",
			"srv.Bitte noch %d s warten":"Bitte noch {1} s warten",
			"srv.%s Grund: %s":"{1} Grund: {2}",
			"srv.Du bist stummgeschaltet.":"Du bist stummgeschaltet."}`)},
		"l/en.json": {Data: []byte(`{"_meta":{"name":"English","locale":"en-GB"},
			"app.hello":"Hello {name}!",
			"app.count":"{n, plural, =0 {no message} one {# message} other {# messages}}",
			"srv.Diesen Kanal gibt es nicht":"This channel doesn't exist",
			"srv.Bitte noch %d s warten":"Please wait {1} s",
			"srv.%s Grund: %s":"{1} Reason: {2}",
			"srv.Du bist stummgeschaltet.":"You are muted."}`)},
	}
	c, err := New(fsys, "l")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFormatAndFallback(t *testing.T) {
	c := testCatalog(t)
	if got := c.T("en", "app.hello", map[string]any{"name": "Mia"}); got != "Hello Mia!" {
		t.Fatal(got)
	}
	for n, want := range map[int]string{0: "no message", 1: "1 message", 5: "5 messages"} {
		if got := c.T("en", "app.count", map[string]any{"n": n}); got != want {
			t.Fatalf("%d: %q", n, got)
		}
	}
	if got := c.T("de", "app.count", map[string]any{"n": 2}); got != "2 Nachrichten" {
		t.Fatal(got)
	}
	if got := c.T("en", "app.only", nil); got != "Nur Deutsch" {
		t.Fatalf("Rückfall: %q", got)
	}
	if got := c.T("de", "app.brace", map[string]any{"x": 1}); got != "{wörtlich} 1" {
		t.Fatalf("Klammern: %q", got)
	}
	if got := c.T("de", "app.hello", nil); got != "Hallo {name}!" {
		t.Fatalf("fehlender Wert: %q", got)
	}
}

func TestServerMessages(t *testing.T) {
	c := testCatalog(t)
	if got := c.Message("en", "Diesen Kanal gibt es nicht"); got != "This channel doesn't exist" {
		t.Fatal(got)
	}
	if got := c.Message("en", "Bitte noch 12 s warten"); got != "Please wait 12 s" {
		t.Fatal(got)
	}
	// verschachtelt: der erste Teil ist selbst eine Meldung
	if got := c.Message("en", "Du bist stummgeschaltet. Grund: Spam"); got != "You are muted. Reason: Spam" {
		t.Fatal(got)
	}
	if got := c.Message("en", "Unbekannt"); got != "Unbekannt" {
		t.Fatal(got)
	}
	if got := c.Message("de", "Diesen Kanal gibt es nicht"); got != "Diesen Kanal gibt es nicht" {
		t.Fatal(got)
	}
}

func TestOverridesSurviveAndValidate(t *testing.T) {
	c := testCatalog(t)
	changed := 0
	c.OnChange(func() { changed++ })
	if _, err := c.SetText("en", "app.hello", "Hi {nme}!"); err == nil {
		t.Fatal("unbekannter Platzhalter angenommen")
	}
	if _, err := c.SetText("en", "app.hello", "Hi {name"); err == nil {
		t.Fatal("kaputte Klammern angenommen")
	}
	warns, err := c.SetText("en", "app.hello", "Hi there!")
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "{name}") {
		t.Fatalf("Warnung erwartet: %v %v", warns, err)
	}
	if _, err := c.SetText("en", "app.hello", "Hey {name} 👋"); err != nil {
		t.Fatal(err)
	}
	if got := c.T("en", "app.hello", map[string]any{"name": "Tom"}); got != "Hey Tom 👋" {
		t.Fatal(got)
	}
	if changed == 0 {
		t.Fatal("OnChange nicht aufgerufen")
	}
	// Server-Meldung überschreiben ({1} statt %d)
	if _, err := c.SetText("en", "srv.Bitte noch %d s warten", "Hold on for {1} seconds"); err != nil {
		t.Fatal(err)
	}
	if got := c.Message("en", "Bitte noch 3 s warten"); got != "Hold on for 3 seconds" {
		t.Fatal(got)
	}
	if _, err := c.SetText("en", "srv.Bitte noch %d s warten", "Hold on {2}"); err == nil {
		t.Fatal("unbekanntes {2} angenommen")
	}

	// Zustand übernehmen (wie nach einem Neustart oder Update)
	st := c.Export()
	c2 := c.Fresh()
	c2.Restore(st)
	if got := c2.T("en", "app.hello", map[string]any{"name": "Tom"}); got != "Hey Tom 👋" {
		t.Fatalf("nach Neustart: %q", got)
	}
	// Ein Update ändert das Original → Änderung bleibt, ist aber „veraltet“
	c2.official["en"]["app.hello"] = "Hello there, {name}!"
	c2.cache = map[string]map[string]string{}
	ents, _ := c2.Entries("en")
	for _, e := range ents {
		if e.Key == "app.hello" && (!e.Changed || !e.Outdated || e.Text != "Hey {name} 👋") {
			t.Fatalf("veraltet: %+v", e)
		}
	}
	if got := c2.T("en", "app.hello", map[string]any{"name": "Tom"}); got != "Hey Tom 👋" {
		t.Fatalf("Änderung muss Update überstehen: %q", got)
	}
	if err := c2.Confirm("en", "app.hello"); err != nil {
		t.Fatal(err)
	}
	ents, _ = c2.Entries("en")
	for _, e := range ents {
		if e.Key == "app.hello" && e.Outdated {
			t.Fatal("nach Bestätigen noch veraltet")
		}
	}
	// zurücksetzen
	c2.SetText("en", "app.hello", "")
	if got := c2.T("en", "app.hello", map[string]any{"name": "Tom"}); got != "Hello there, Tom!" {
		t.Fatal(got)
	}
}

func TestCustomLanguageImportExport(t *testing.T) {
	c := testCatalog(t)
	if err := c.AddLanguage("fr", "Français", "fr-FR"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLanguage("fr", "x", ""); err != ErrLangExists {
		t.Fatal(err)
	}
	if err := c.AddLanguage("Französisch", "x", ""); err != ErrLangCode {
		t.Fatal(err)
	}
	res, err := c.Import("fr", []byte(`{"_meta":{"name":"Français"},"app.hello":"Bonjour {name} !","app.count":"{n, plural, one {# message} other {# messages}}","app.unknown":"x","app.only":"Seulement {oops}"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 2 || res.Unknown != 1 || len(res.Errors) != 1 {
		t.Fatalf("Import: %+v", res)
	}
	if got := c.T("fr", "app.hello", map[string]any{"name": "Léa"}); got != "Bonjour Léa !" {
		t.Fatal(got)
	}
	var fr Lang
	for _, l := range c.Languages(true) {
		if l.Code == "fr" {
			fr = l
		}
	}
	if fr.Official || fr.Complete <= 0 || fr.Complete >= 1 {
		t.Fatalf("Vollständigkeit: %+v", fr)
	}
	b, err := c.File("fr", false)
	if err != nil || !strings.Contains(string(b), "Bonjour {name} !") || strings.Contains(string(b), "Nur Deutsch") {
		t.Fatalf("Export (nur Änderungen): %s %v", b, err)
	}
	b, _ = c.File("fr", true)
	if !strings.Contains(string(b), "Nur Deutsch") {
		t.Fatal("Export (vollständig) ohne Rückfalltexte")
	}
	if err := c.DeleteLanguage("en"); err != ErrOfficial {
		t.Fatal(err)
	}
	if err := c.DeleteLanguage("fr"); err != nil || c.Has("fr") {
		t.Fatal("Löschen")
	}
}

func TestResolve(t *testing.T) {
	c := testCatalog(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "fr-CH, en;q=0.8, de;q=0.5")
	if got := c.Resolve(r); got != "en" {
		t.Fatalf("Accept-Language: %s", got)
	}
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "de"})
	if got := c.Resolve(r); got != "de" {
		t.Fatalf("Cookie: %s", got)
	}
	r.Header.Set("X-Lang", "en")
	if got := c.Resolve(r); got != "en" {
		t.Fatalf("X-Lang: %s", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Accept-Language", "en")
	c.SetSettings("de", false, nil)
	if got := c.Resolve(r2); got != "de" {
		t.Fatalf("ohne Browsersprache: %s", got)
	}
	c.SetSettings("de", true, []string{"en"})
	if got := c.Resolve(r2); got != "de" {
		t.Fatalf("ausgeblendet: %s", got)
	}
}

// Übersetzungspaket: Ausgangstext, Referenz und Platzhalter rein, ausgefülltes Paket zurück.
func TestPackageRoundTrip(t *testing.T) {
	c := testCatalog(t)
	c.AddLanguage("fr", "Français", "fr-FR")
	c.SetText("fr", "app.only", "Seulement")
	b, err := c.Package("fr", "GoChat", false)
	if err != nil {
		t.Fatal(err)
	}
	var p Package
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	h := p.Texts["app.hello"]
	if p.Format != PackageFormat || p.Reference != "en" || h.Source != "Hallo {name}!" || h.Reference != "Hello {name}!" ||
		h.Status != "missing" || len(h.Placeholders) != 1 || h.Placeholders[0] != "{name}" {
		t.Fatalf("Paket: %+v", h)
	}
	if !p.Texts["app.count"].Plural || p.Texts["app.only"].Status != "translated" || p.Open != len(p.Texts)-1 {
		t.Fatalf("Zustand: %+v / %d", p.Texts["app.count"], p.Open)
	}
	if s := p.Texts["srv.Bitte noch %d s warten"]; len(s.Placeholders) != 1 || s.Placeholders[0] != "{1}" {
		t.Fatalf("srv: %+v", s)
	}
	// nur offene Texte
	b2, _ := c.Package("fr", "GoChat", true)
	var p2 Package
	json.Unmarshal(b2, &p2)
	if _, ok := p2.Texts["app.only"]; ok || len(p2.Texts) != p2.Open {
		t.Fatal("nur offene Texte erwartet")
	}
	// „KI“ füllt aus (eins mit falschem Platzhalter, eins leer)
	h.Text = "Bonjour {name} !"
	p.Texts["app.hello"] = h
	cnt := p.Texts["app.count"]
	cnt.Text = "{n, plural, one {# message} other {# messages {x}}}"
	p.Texts["app.count"] = cnt
	out, _ := json.Marshal(p)
	res, err := c.Import("fr", out, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 || len(res.Errors) != 1 {
		t.Fatalf("Import: %+v", res)
	}
	if got := c.T("fr", "app.hello", map[string]any{"name": "Léa"}); got != "Bonjour Léa !" {
		t.Fatal(got)
	}
	if got := c.T("fr", "app.only", nil); got != "Seulement" {
		t.Fatalf("leere Texte dürfen Vorhandenes nicht löschen: %q", got)
	}
	// eigene Sprache: ändert sich der deutsche Text, ist die Übersetzung veraltet
	c.official[Source]["app.hello"] = "Hallo {name}, schön dich zu sehen!"
	c.cache = map[string]map[string]string{}
	b3, _ := c.Package("fr", "GoChat", true)
	var p3 Package
	json.Unmarshal(b3, &p3)
	if e := p3.Texts["app.hello"]; e.Status != "outdated" || e.Previous != "Hallo {name}!" || e.Text != "Bonjour {name} !" {
		t.Fatalf("veraltet: %+v", e)
	}
	c.official[Source]["app.hello"] = "Hallo {name}!"
	c.cache = map[string]map[string]string{}
	// Pakete von vor der Umbenennung (Format gochat-i18n-package/1) werden weiter angenommen
	if _, _, _, err := c.Validate([]byte(`{"format":"gochat-i18n-package/1","texts":{"app.hello":{"text":"Salut {name}"}}}`)); err != nil {
		t.Fatalf("früheres Paketformat: %v", err)
	}
	// Validate (für den Beacon)
	texts, m, vr, err := c.Validate(out)
	if err != nil || m.Name != "Français" || texts["app.hello"] != "Bonjour {name} !" || len(vr.Errors) != 1 {
		t.Fatalf("Validate: %v %+v %+v", err, m, vr)
	}
}
