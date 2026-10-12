// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ucng/internal/i18n"
)

// Gemeinsame Dateien mit dem Chat müssen gleich bleiben (embed kann nicht auf ../../web zugreifen).
func TestSharedI18nFiles(t *testing.T) {
	for _, f := range []string{"i18n.js", "i18n-editor.js", "i18n-editor.css", "logo.svg"} {
		a, err := os.ReadFile("../../web/" + f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile("web/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("web/%s weicht von ../../web/%s ab – bitte kopieren", f, f)
		}
	}
	// Ausgangstexte des Chats für Sprachpakete (chati18n/) = web/i18n/ des Chats
	chatFiles, _ := filepath.Glob("../../web/i18n/*.json")
	for _, f := range chatFiles {
		a, _ := os.ReadFile(f)
		b, err := os.ReadFile("chati18n/" + filepath.Base(f))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("chati18n/%s weicht von web/i18n ab – bitte kopieren", filepath.Base(f))
		}
	}
	// Texte des Editors wie im Chat
	for _, lang := range []string{"de", "en"} {
		var chat, beacon map[string]any
		b, _ := os.ReadFile("../../web/i18n/" + lang + ".json")
		json.Unmarshal(b, &chat)
		b, _ = os.ReadFile("web/i18n/" + lang + ".json")
		json.Unmarshal(b, &beacon)
		for k, v := range chat {
			if strings.HasPrefix(k, "i18ned.") || strings.HasPrefix(k, "common.") {
				if beacon[k] != v {
					t.Errorf("%s: %s weicht vom Chat ab: %v ≠ %v", lang, k, beacon[k], v)
				}
			}
		}
	}
}

func TestBeaconTranslations(t *testing.T) {
	cat, err := officialCatalog()
	if err != nil {
		t.Fatal(err)
	}
	src := cat.Dict(i18n.Source)
	for _, l := range cat.Languages(true) {
		ents, _ := cat.Entries(l.Code)
		for _, e := range ents {
			if e.Official == "" {
				t.Errorf("%s: %s fehlt", l.Code, e.Key)
				continue
			}
			warns, err := i18n.Check(e.Key, e.Source, e.Official)
			if err != nil || (l.Code != i18n.Source && len(warns) > 0) {
				t.Errorf("%s: %s: %v %v", l.Code, e.Key, err, warns)
			}
		}
	}
	// benutzte Schlüssel stehen in der Ausgangssprache
	static, _ := fs.Sub(webFiles, "web")
	keyRe := regexp.MustCompile(`(?:_tH?\(|\bt\(|data-i18n(?:-html|-placeholder|-title|-aria-label|-alt)?=)["']([a-z]+\.[A-Za-z0-9_]+)["']`)
	for _, f := range []string{"index.html", "beacon.js", "i18n-editor.js"} {
		b, _ := fs.ReadFile(static, f)
		for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
			if _, ok := src[m[1]]; !ok {
				t.Errorf("%s: Schlüssel %s fehlt", f, m[1])
			}
		}
	}
}

func TestBeaconI18nHTTP(t *testing.T) {
	ts, s := testBeacon(t)
	// boot.js nach Accept-Language
	req, _ := http.NewRequest("GET", ts.URL+"/api/i18n/boot.js", nil)
	req.Header.Set("Accept-Language", "en-US")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), `"beacon.instanzen":"Instances"`) || !strings.Contains(string(b), `"i18ned.sprachen":"Languages"`) {
		t.Fatalf("boot.js: %.300s", b)
	}
	// Fehlermeldungen in der Sprache der Anfrage
	req, _ = http.NewRequest("POST", ts.URL+"/api/admin/login", strings.NewReader(`{"password":"falsch"}`))
	req.Header.Set("X-Lang", "en")
	res, _ = http.DefaultClient.Do(req)
	var e struct{ Error string }
	json.NewDecoder(res.Body).Decode(&e)
	res.Body.Close()
	if e.Error != "Wrong password (or too many attempts)" {
		t.Fatalf("Fehler (en): %q", e.Error)
	}
	// Änderung über die Verwaltung – bleibt nach einem Neustart erhalten
	tok := adminToken(t, ts.URL)
	r := adminDo(t, "PUT", ts.URL+"/api/admin/i18n/texts/en", tok, []byte(`{"key":"beacon.instanzen","text":"Servers"}`), "application/json")
	if r.StatusCode != 200 {
		t.Fatalf("Text setzen: %d", r.StatusCode)
	}
	r = adminDo(t, "PUT", ts.URL+"/api/admin/i18n/texts/en", tok, []byte(`{"key":"beacon.n_gifs","text":"{count} GIFs"}`), "application/json")
	if r.StatusCode != 400 {
		t.Fatalf("falscher Platzhalter angenommen: %d", r.StatusCode)
	}
	cat, err := openCatalog(s.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cat.T("en", "beacon.instanzen", nil); got != "Servers" {
		t.Fatalf("nach Neustart: %q", got)
	}
}

// Sprachpakete: hochladen (Paket mit Fehler), auflisten, Datei und Übersetzungspaket abrufen.
func TestLangPacks(t *testing.T) {
	ts, s := testBeacon(t)
	tok := adminToken(t, ts.URL)
	get := func(path string) (int, []byte) {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	// leeres Übersetzungspaket für eine neue Sprache
	code, b := get("/api/v1/languages/fr/package?name=Fran%C3%A7ais&locale=fr-FR")
	if code != 200 {
		t.Fatalf("Paket: %d %s", code, b)
	}
	var p i18n.Package
	json.Unmarshal(b, &p)
	e := p.Texts["app.senden"]
	if p.Format != i18n.PackageFormat || p.Meta.Name != "Français" || e.Source != "Senden" || e.Reference != "Send" || e.Status != "missing" || p.Open != p.Total {
		t.Fatalf("Paket: %+v %+v", p.Meta, e)
	}
	// „KI“ füllt zwei Texte aus, einer mit falschem Platzhalter (wird übersprungen)
	e.Text = "Envoyer"
	p.Texts["app.senden"] = e
	w := p.Texts["i18ned.treffer"]
	w.Text = "{count} sur {total}"
	p.Texts["i18ned.treffer"] = w
	body, _ := json.Marshal(p)
	r := adminDo(t, "POST", ts.URL+"/api/admin/langpacks/fr?note=KI", tok, body, "application/json")
	var up struct {
		Result i18n.ImportResult
		Pack   LangPackView
	}
	json.NewDecoder(r.Body).Decode(&up)
	if r.StatusCode != 200 || up.Result.Applied != 1 || up.Pack.Texts != 1 || up.Pack.Note != "KI" {
		t.Fatalf("Hochladen: %d %+v", r.StatusCode, up)
	}
	// Liste und Datei
	_, b = get("/api/v1/languages")
	if !strings.Contains(string(b), `"code":"fr"`) || !strings.Contains(string(b), `"name":"Français"`) {
		t.Fatalf("Liste: %s", b)
	}
	_, b = get("/api/v1/languages/fr")
	if !strings.Contains(string(b), `"app.senden": "Envoyer"`) || !strings.Contains(string(b), `"_meta": {"name":"Français","locale":"fr-FR"}`) {
		t.Fatalf("Datei: %s", b)
	}
	// Paket enthält den übersetzten Text, nur-offen nicht
	_, b = get("/api/v1/languages/fr/package?open=1")
	var p2 i18n.Package
	json.Unmarshal(b, &p2)
	if _, ok := p2.Texts["app.senden"]; ok || p2.Open != p2.Total-1 {
		t.Fatalf("offene Texte: %d/%d", p2.Open, p2.Total)
	}
	// nach Neustart noch da
	ps, err := openPacks(s.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if pk, ok := ps.Get("fr"); !ok || pk.Texts["app.senden"] != "Envoyer" || pk.Bases["app.senden"] != "Senden" {
		t.Fatalf("gespeichert: %+v", pk)
	}
	if c, _ := get("/api/v1/languages/de/package"); c != 400 {
		t.Fatal("Deutsch darf kein Paket sein")
	}
	r = adminDo(t, "DELETE", ts.URL+"/api/admin/langpacks/fr", tok, nil, "")
	if r.StatusCode != 204 {
		t.Fatalf("Löschen: %d", r.StatusCode)
	}
}
