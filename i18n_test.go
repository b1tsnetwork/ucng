// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"ucng/internal/i18n"
)

// Alle offiziellen Übersetzungen passen zur Ausgangssprache (Platzhalter, Klammern) und
// kennen keine Schlüssel, die es nicht gibt.
func TestOfficialTranslations(t *testing.T) {
	static, _ := fs.Sub(webFiles, "web")
	files, _ := fs.Glob(static, "i18n/*.json")
	read := func(name string) map[string]string {
		b, _ := fs.ReadFile(static, name)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := map[string]string{}
		for k, v := range m {
			if k == "_meta" {
				continue
			}
			var s string
			json.Unmarshal(v, &s)
			out[k] = s
		}
		return out
	}
	src := read("i18n/" + i18n.Source + ".json")
	for _, f := range files {
		texts := read(f)
		for k, v := range texts {
			s, ok := src[k]
			if !ok {
				t.Errorf("%s: unbekannter Schlüssel %s", f, k)
				continue
			}
			warns, err := i18n.Check(k, s, v)
			if err != nil {
				t.Errorf("%s: %s: %v", f, k, err)
			}
			if len(warns) > 0 && !strings.HasSuffix(f, "/"+i18n.Source+".json") {
				t.Errorf("%s: %s: %v", f, k, warns)
			}
		}
		if len(texts) < len(src) && !strings.Contains(f, "/de.") {
			missing := []string{}
			for k := range src {
				if _, ok := texts[k]; !ok {
					missing = append(missing, k)
				}
			}
			sort.Strings(missing)
			if len(missing) > 10 {
				missing = missing[:10]
			}
			t.Errorf("%s: %d Texte fehlen, z. B. %v", f, len(src)-len(texts), missing)
		}
	}
}

// Jeder in HTML und JavaScript benutzte Schlüssel steht in der Ausgangssprache.
func TestUsedKeysExist(t *testing.T) {
	cat := newCatalog()
	dict := cat.Dict(i18n.Source)
	static, _ := fs.Sub(webFiles, "web")
	keyRe := regexp.MustCompile(`(?:_tH?\(|\bTH?\(|\bt\(|data-i18n(?:-html|-placeholder|-title|-aria-label|-alt)?=)["']([a-z]+\.[A-Za-z0-9_]+)["']`)
	fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() || p == "i18n.js" || !(strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".html")) {
			return nil
		}
		b, _ := fs.ReadFile(static, p)
		for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
			if _, ok := dict[m[1]]; !ok {
				t.Errorf("%s: Schlüssel %s fehlt in %s.json", p, m[1], i18n.Source)
			}
		}
		return nil
	})
}

// Das Widget enthält die deutschen Texte als Rückfall – sie müssen zu de.json passen.
// Mit UPDATE_I18N=1 go test -run TestWidgetFallback wird der Block neu geschrieben.
func TestWidgetFallback(t *testing.T) {
	dict := newCatalog().Dict(i18n.Source, "widget.", "common.")
	keys := make([]string, 0, len(dict))
	for k := range dict {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("  var I18N_DE = {\n")
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(dict[k])
		b.WriteString("    " + string(kb) + ": " + string(vb))
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  };\n")
	want := b.String()
	src, err := os.ReadFile("web/embed/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)(/\*I18N-DE-START\*/\n)(.*?)(  /\*I18N-DE-END\*/)`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatal("Markierungen I18N-DE-START/END fehlen in chat.js")
	}
	if string(m[2]) == want {
		return
	}
	if os.Getenv("UPDATE_I18N") == "1" {
		out := re.ReplaceAll(src, []byte("${1}"+strings.ReplaceAll(want, "$", "$$")+"${3}"))
		if err := os.WriteFile("web/embed/chat.js", out, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("chat.js aktualisiert")
		return
	}
	t.Error("Deutsche Rückfalltexte in web/embed/chat.js veraltet – UPDATE_I18N=1 go test -run TestWidgetFallback .")
}

func TestI18nHTTP(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	// boot.js nach Cookie, Accept-Language und Standard
	get := func(path string, h map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b := new(strings.Builder)
		buf := make([]byte, 1<<16)
		for {
			n, err := res.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		res.Body.Close()
		return res, b.String()
	}
	_, js := get("/api/i18n/boot.js?scope=app", map[string]string{"Accept-Language": "en-US,en;q=0.9"})
	if !strings.Contains(js, `"lang":"en"`) || !strings.Contains(js, `"app.senden":"Send"`) || strings.Contains(js, `"admin.`) {
		t.Fatalf("boot.js (en): %.300s", js)
	}
	_, js = get("/api/i18n/boot.js?scope=admin", map[string]string{"Cookie": "gochat_lang=de", "Accept-Language": "en"})
	if !strings.Contains(js, `"lang":"de"`) || !strings.Contains(js, `"admin.speichern":"Speichern"`) {
		t.Fatalf("boot.js (Cookie de): %.300s", js)
	}
	res, body := get("/api/i18n/dict?scope=widget&lang=en", nil)
	if res.StatusCode != 200 || !strings.Contains(body, `"widget.senden":"Send"`) || strings.Contains(body, `"app.`) {
		t.Fatalf("dict: %d %.300s", res.StatusCode, body)
	}

	// Fehlermeldungen in der Sprache der Anfrage
	req, _ := http.NewRequest("POST", ts.URL+"/api/session", strings.NewReader(`{"name":""}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lang", "en")
	r2, _ := http.DefaultClient.Do(req)
	var e struct{ Error string }
	json.NewDecoder(r2.Body).Decode(&e)
	r2.Body.Close()
	if e.Error != "A name must be 1–32 characters long" {
		t.Fatalf("Fehler (en): %q", e.Error)
	}

	// Admin ändert einen Text – gilt sofort und wird gespeichert
	tok := adminLogin(t, ts.URL)
	r3 := do(t, "PUT", ts.URL+"/api/admin/i18n/texts/en", tok, `{"key":"app.senden","text":"Go!"}`)
	if r3.StatusCode != 200 {
		t.Fatalf("Text setzen: %d %s", r3.StatusCode, errText(r3))
	}
	r3.Body.Close()
	r3 = do(t, "PUT", ts.URL+"/api/admin/i18n/texts/en", tok, `{"key":"app.willkommen","text":"Hi {nam}!"}`)
	if r3.StatusCode != 400 {
		t.Fatalf("falscher Platzhalter angenommen: %d", r3.StatusCode)
	}
	r3.Body.Close()
	_, js = get("/api/i18n/boot.js?scope=app", map[string]string{"Cookie": "gochat_lang=en"})
	if !strings.Contains(js, `"app.senden":"Go!"`) {
		t.Fatal("Änderung nicht ausgeliefert")
	}
	st := srv.hub.I18n().Export()
	if st.Texts["en"]["app.senden"].Text != "Go!" {
		t.Fatalf("Zustand: %+v", st.Texts["en"])
	}
	// eigene Sprache anlegen und als Standard setzen
	r3 = do(t, "POST", ts.URL+"/api/admin/i18n/languages", tok, `{"code":"fr","name":"Français","locale":"fr-FR"}`)
	r3.Body.Close()
	r3 = do(t, "PUT", ts.URL+"/api/admin/i18n/settings", tok, `{"default":"fr","auto":false}`)
	if r3.StatusCode != 200 {
		t.Fatalf("Standard: %d %s", r3.StatusCode, errText(r3))
	}
	r3.Body.Close()
	_, js = get("/api/i18n/boot.js?scope=app", map[string]string{"Accept-Language": "en"})
	if !strings.Contains(js, `"lang":"fr"`) || !strings.Contains(js, `"app.senden":"Senden"`) {
		t.Fatalf("eigene Sprache als Standard (Rückfall auf de): %.200s", js)
	}
}

// Änderungen der Admins überstehen einen Neustart (Speicherung als Dokument i18n.json).
func TestI18nPersistence(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	if _, err := h.I18n().SetText("en", "app.senden", "Go!"); err != nil {
		t.Fatal(err)
	}
	h.I18n().AddLanguage("nl", "Nederlands", "nl-NL")
	h.I18n().SetText("nl", "app.senden", "Verzenden")
	flush(t, h, u, s)
	h2, _, _ := openStore(t, dir)
	if got := h2.I18n().T("en", "app.senden", nil); got != "Go!" {
		t.Fatalf("nach Neustart: %q", got)
	}
	if got := h2.I18n().T("nl", "app.senden", nil); got != "Verzenden" {
		t.Fatalf("eigene Sprache nach Neustart: %q", got)
	}
}

// Antworten des Befehls-Bots folgen der Sprache (öffentlich: Standardsprache).
func TestCommandsTranslated(t *testing.T) {
	cat := newCatalog()
	c := NewCommands()
	en := func(msg string) string { return cat.Message("en", msg) }
	ctx := CmdContext{Room: "lobby", User: "Anna", Now: time.Now(), Tr: en,
		Online: func() Event { return Event{Online: 3, Guests: 2, Users: []PresenceUser{{Name: "Anna"}}} }}
	if r := c.Run("!würfel 1", ctx); r.Text != "Please give a number between 2 and 1000, e.g. !würfel 20" {
		t.Fatalf("Würfel: %q", r.Text)
	}
	r := c.Run("!online", ctx)
	if !strings.Contains(r.Text, "Anna and 2 guests") {
		t.Fatalf("Online: %q", r.Text)
	}
}

// Sprachpaket von einem Beacon installieren; ein neueres Paket erscheint als Update.
func TestI18nBeaconPacks(t *testing.T) {
	updated := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	beacon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/info":
			json.NewEncoder(w).Encode(map[string]any{"software": "gochat-beacon", "name": "Test-Beacon"})
		case "/api/v1/languages":
			json.NewEncoder(w).Encode(map[string]any{"packs": []map[string]any{
				{"code": "fr", "name": "Français", "locale": "fr-FR", "updated": updated, "texts": 2, "total": 100, "complete": 0.02}}})
		case "/api/v1/languages/fr":
			w.Write([]byte(`{"_meta":{"name":"Français","locale":"fr-FR"},"app.senden":"Envoyer","app.willkommen":"Bienvenue {kaputt}"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer beacon.Close()
	ts, srv := testServerWith(t, nil)
	tok := adminLogin(t, ts.URL)
	list := func() map[string]any {
		r := do(t, "GET", ts.URL+"/api/admin/i18n/beacon?url="+beacon.URL, tok, "")
		var out map[string]any
		json.NewDecoder(r.Body).Decode(&out)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("Liste: %d %v", r.StatusCode, out)
		}
		return out["packs"].([]any)[0].(map[string]any)
	}
	if p := list(); p["installed"] == true {
		t.Fatalf("noch nicht installiert: %v", p)
	}
	r := do(t, "POST", ts.URL+"/api/admin/i18n/beacon/install", tok, `{"url":"`+beacon.URL+`","code":"fr","updated":"`+updated.Format(time.RFC3339)+`"}`)
	var res struct {
		Applied int
		Errors  []string
	}
	json.NewDecoder(r.Body).Decode(&res)
	r.Body.Close()
	if r.StatusCode != 200 || res.Applied != 1 || len(res.Errors) != 1 {
		t.Fatalf("Installieren: %d %+v", r.StatusCode, res)
	}
	cat := srv.hub.I18n()
	if got := cat.T("fr", "app.senden", nil); got != "Envoyer" || cat.Meta("fr").Locale != "fr-FR" {
		t.Fatalf("installiert: %q", got)
	}
	if p := list(); p["fromHere"] != true || p["update"] == true {
		t.Fatalf("Zustand: %v", p)
	}
	updated = updated.Add(24 * time.Hour)
	if p := list(); p["update"] != true {
		t.Fatalf("Update erwartet: %v", p)
	}
}
