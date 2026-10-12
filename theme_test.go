// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestThemeSettingsValidation(t *testing.T) {
	ts, _ := testServer(t)
	admin := adminLogin(t, ts.URL)
	for _, body := range []string{
		`{"title":"X","allowUserRooms":true,"accent":"red","mode":"auto"}`,
		`{"title":"X","allowUserRooms":true,"accent":"#12345","mode":"auto"}`,
		`{"title":"X","allowUserRooms":true,"accent":"#123456","sidebar":"blau","mode":"auto"}`,
		`{"title":"X","allowUserRooms":true,"accent":"#123456","mode":"bunt"}`,
	} {
		if res := do(t, "PUT", ts.URL+"/api/admin/settings", admin, body); res.StatusCode != 400 {
			t.Errorf("%s: erwartet 400, bekam %d", body, res.StatusCode)
		}
	}
	// Ältere Clients ohne Farbfelder: Standardwerte statt Fehler.
	if res := do(t, "PUT", ts.URL+"/api/admin/settings", admin, `{"title":"X","allowUserRooms":true}`); res.StatusCode != 200 {
		t.Fatalf("ohne Farbfelder: %d", res.StatusCode)
	}
	res := do(t, "PUT", ts.URL+"/api/admin/settings", admin, `{"title":"X","allowUserRooms":true,"accent":"#FACC15","sidebar":"#FFFFFF","mode":"dark"}`)
	if res.StatusCode != 200 {
		t.Fatalf("gültige Farben: %d", res.StatusCode)
	}
	css := getCSS(t, ts.URL+"/theme.css")
	for _, want := range []string{"--accent: #facc15;", "--on-accent: #111321;", "--ink: #ffffff;", "--ink-strong: #111321;", "color-scheme: dark;"} {
		if !strings.Contains(css, want) {
			t.Errorf("theme.css enthält nicht %q:\n%s", want, css)
		}
	}
	if strings.Contains(css, "@media") {
		t.Error("bei „immer dunkel“ darf es keine Media-Query geben")
	}
}

func TestThemeCSSDefaultsAndPreview(t *testing.T) {
	ts, _ := testServer(t)
	css := getCSS(t, ts.URL+"/theme.css")
	if !strings.Contains(css, "--accent: #5848f4;") || !strings.Contains(css, "@media (prefers-color-scheme: dark)") {
		t.Fatalf("Standard-Theme:\n%s", css)
	}
	// Vorschau per Parameter, ohne zu speichern.
	prev := getCSS(t, ts.URL+"/theme.css?accent=%23dc2626&sidebar=&mode=light")
	if !strings.Contains(prev, "--accent: #dc2626;") || strings.Contains(prev, "@media") {
		t.Fatalf("Vorschau:\n%s", prev)
	}
	if css2 := getCSS(t, ts.URL+"/theme.css"); !strings.Contains(css2, "#5848f4") {
		t.Fatal("Vorschau hat die gespeicherten Farben verändert")
	}
	// Ungültige Vorschau-Werte werden ignoriert, nicht ins CSS übernommen.
	if bad := getCSS(t, ts.URL+"/theme.css?accent=red;}body{display:none"); strings.Contains(bad, "display:none") {
		t.Fatal("CSS-Injektion über Vorschau-Parameter")
	}
}

func TestReadableText(t *testing.T) {
	cases := map[string]string{"#5848f4": "#ffffff", "#facc15": "#111321", "#ffffff": "#111321", "#111318": "#ffffff",
		"#15803d": "#ffffff", "#0f766e": "#ffffff", "#ea580c": "#111321", "#22d3ee": "#111321"}
	for bg, want := range cases {
		if got := readableOn(parseHex(bg)).hex(); got != want {
			t.Errorf("Schrift auf %s: erwartet %s, bekam %s", bg, want, got)
		}
	}
	// Link-Farbe muss auf hellem Hintergrund mindestens 4,5:1 Kontrast haben – auch bei Gelb.
	for _, accent := range []string{"#facc15", "#5848f4", "#22d3ee"} {
		css := ThemeCSS(Settings{Accent: accent, Mode: ModeLight})
		i := strings.Index(css, "--accent-text: ") + len("--accent-text: ")
		link := parseHex(css[i : i+7])
		if c := contrast(link, white); c < 4.5 {
			t.Errorf("Akzent %s: Linkfarbe %s hat nur Kontrast %.2f", accent, link.hex(), c)
		}
	}
}

func TestOldConfigGetsDefaults(t *testing.T) {
	dir := t.TempDir()
	writeFileAtomic(dir+"/config.json", []byte(`{"title":"Alt","allowUserRooms":false}`))
	h, _, _ := openStore(t, dir)
	if st := h.Settings(); st.Accent != defaultAccent || st.Mode != ModeAuto || st.Title != "Alt" {
		t.Fatalf("Standardwerte fehlen: %+v", st)
	}
}

func getCSS(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type %q", ct)
	}
	b, _ := io.ReadAll(res.Body)
	return string(b)
}
