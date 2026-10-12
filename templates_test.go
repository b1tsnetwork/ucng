// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"image/color"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func tplSetupSource(t *testing.T, srv *Server) {
	t.Helper()
	h := srv.hub
	roles, err := h.Roles().Set([]Role{{Name: "VIP", Color: "#aa0000"}, {Name: "Helfer", Color: "#00aa00", Perms: Perms{Pin: true}}})
	if err != nil {
		t.Fatal(err)
	}
	vip := roles[0].ID
	name := "vip-lounge"
	if _, err := h.CreateRoomWith(RoomInput{Name: &name, Description: "Nur für VIPs", SlowMode: 5, Links: LinksAll, BotLinks: LinksAll, ReadRoles: []string{vip}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetRoomAutomod(name, RoomAutomod{Mode: AutomodBoth, Patterns: []string{"geheim*"}}); err != nil {
		t.Fatal(err)
	}
	var li LayoutInput
	li.Groups = append(li.Groups, struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		Rooms []string `json:"rooms"`
	}{Name: "Exklusiv", Rooms: []string{name}})
	if _, err := h.SetLayout(li); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"spam*", "# Kommentar"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetFilter(FilterConfig{Patterns: []string{"mist"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Bots().Create(BotInput{Name: "Wetterfrosch", Rooms: []string{"lobby"}, WebhookURL: "https://example.com/hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Widgets().Create(Widget{Name: "Shop", Room: "lobby", Title: "Hilfe", Theme: "auto", Position: "right", Mode: "floating", Domains: []string{"shop.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Media().AddEmote("party", 32, testPNG(80, 80, color.RGBA{200, 0, 0, 255})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Media().AddGIF("Tanz", []string{"freude"}, testGIF(120, 90, 3)); err != nil {
		t.Fatal(err)
	}
	st := h.Settings()
	st.Title = "Quelle"
	st.Accent = "#123456"
	if _, err := h.SetSettings(st); err != nil {
		t.Fatal(err)
	}
}

func tplExport(t *testing.T, base, tok, parts string) []byte {
	t.Helper()
	res := do(t, "GET", base+"/api/admin/template?parts="+parts, tok, "")
	if res.StatusCode != 200 {
		t.Fatalf("Export: %d %s", res.StatusCode, errText(res))
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return b
}

func tplImport(t *testing.T, base, tok string, zip []byte, parts []string, mode string) []TplResult {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "vorlage.zip")
	fw.Write(zip)
	mw.Close()
	req, _ := http.NewRequest("POST", base+"/api/admin/template/inspect", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("Inspect: %d %s", res.StatusCode, errText(res))
	}
	ins := decode[struct {
		Token    string           `json:"token"`
		Manifest TemplateManifest `json:"manifest"`
	}](t, res)
	if parts == nil {
		parts = ins.Manifest.Parts
	}
	body, _ := json.Marshal(map[string]any{"token": ins.Token, "parts": parts, "mode": mode})
	res = do(t, "POST", base+"/api/admin/template/apply", tok, string(body))
	if res.StatusCode != 200 {
		t.Fatalf("Apply: %d %s", res.StatusCode, errText(res))
	}
	out := decode[struct {
		Results []TplResult `json:"results"`
	}](t, res)
	// Token ist danach verbraucht
	res = do(t, "POST", base+"/api/admin/template/apply", tok, string(body))
	if res.StatusCode != http.StatusGone {
		t.Fatalf("Token wiederverwendet: %d", res.StatusCode)
	}
	res.Body.Close()
	return out.Results
}

func TestTemplateRoundTrip(t *testing.T) {
	tsA, srvA := testServerWith(t, nil)
	tplSetupSource(t, srvA)
	zip := tplExport(t, tsA.URL, adminLogin(t, tsA.URL), strings.Join(TemplateParts, ","))
	if bytes.Contains(zip, []byte("gcb_")) || bytes.Contains(zip, []byte("tokenHash")) || bytes.Contains(zip, []byte("webhookSecret")) {
		t.Fatal("Vorlage enthält Geheimnisse")
	}

	tsB, srvB := testServerWith(t, nil)
	hb := srvB.hub
	// vorhandene Daten in B: eine gleichnamige Rolle, ein eigenes Emote
	hb.Roles().Set([]Role{{Name: "vip", Color: "#000000"}})
	hb.Media().AddEmote("eigenes", 32, testPNG(40, 40, color.RGBA{0, 0, 200, 255}))
	tokB := adminLogin(t, tsB.URL)
	res := tplImport(t, tsB.URL, tokB, zip, nil, "merge")
	byPart := map[string]TplResult{}
	for _, r := range res {
		byPart[r.Part] = r
		if len(r.Errors) > 0 {
			t.Errorf("%s: %v", r.Part, r.Errors)
		}
	}
	if r := byPart["roles"]; r.Added != 1 || r.Skipped != 1 {
		t.Fatalf("Rollen: %+v", r)
	}
	if r := byPart["bots"]; r.Added != 1 || len(r.Keys) != 1 || !strings.HasPrefix(r.Keys[0].Token, "gcb_") {
		t.Fatalf("Bots: %+v", r)
	}
	if _, ok := hb.Bots().Auth(byPart["bots"].Keys[0].Token); !ok {
		t.Fatal("neuer Bot-Schlüssel gilt nicht")
	}
	// Kanal mit zugeordneter Rolle (über den Namen auf die vorhandene „vip“)
	var vipID string
	for _, r := range hb.Roles().List() {
		if strings.EqualFold(r.Name, "vip") {
			vipID = r.ID
		}
	}
	var found bool
	for _, r := range hb.Rooms() {
		if r.Name == "vip-lounge" {
			found = true
			if r.SlowMode != 5 || len(r.ReadRoles) != 1 || r.ReadRoles[0] != vipID {
				t.Fatalf("Kanal: %+v (vip %s)", r, vipID)
			}
		}
	}
	if !found {
		t.Fatal("Kanal fehlt")
	}
	if am, _ := hb.RoomAutomodConfig("vip-lounge"); am.Mode != AutomodBoth || len(am.Patterns) != 1 {
		t.Fatalf("Kanal-Automod: %+v", am)
	}
	l := hb.Layout()
	if len(l.Groups) != 1 || l.Groups[0].Name != "Exklusiv" || len(l.Groups[0].Rooms) != 1 {
		t.Fatalf("Gruppen: %+v", l)
	}
	if c := hb.Automod().Config(); len(c.Patterns) != 2 {
		t.Fatalf("Automod: %+v", c)
	}
	if f := hb.Filter(); len(f.Patterns) != 1 {
		t.Fatalf("Filter: %+v", f)
	}
	if len(hb.Widgets().List()) != 1 {
		t.Fatal("Widget fehlt")
	}
	if len(hb.Media().Emotes()) != 2 || len(hb.Media().GIFs("")) != 1 {
		t.Fatalf("Medien: %d Emotes, %d GIFs", len(hb.Media().Emotes()), len(hb.Media().GIFs("")))
	}
	if st := hb.Settings(); st.Title != "Quelle" || st.Accent != "#123456" {
		t.Fatalf("Einstellungen: %+v", st)
	}

	// erneut ergänzen: nichts doppelt
	res = tplImport(t, tsB.URL, tokB, zip, nil, "merge")
	for _, r := range res {
		if r.Added != 0 && r.Part != "media" && r.Part != "settings" {
			t.Errorf("doppelt: %+v", r)
		}
	}
	if len(hb.Media().GIFs("")) != 1 || len(hb.Bots().List()) != 1 {
		t.Fatal("doppelte Einträge")
	}

	// ersetzen: eigenes Emote verschwindet, Kanäle bleiben
	name := "extra"
	hb.CreateRoomWith(RoomInput{Name: &name})
	tplImport(t, tsB.URL, tokB, zip, []string{"emotes", "channels"}, "replace")
	if em := hb.Media().Emotes(); len(em) != 1 || em[0].Name != "party" {
		t.Fatalf("Emotes nach Ersetzen: %+v", em)
	}
	ok := false
	for _, r := range hb.Rooms() {
		ok = ok || r.Name == "extra"
	}
	if !ok {
		t.Fatal("Kanal wurde gelöscht")
	}
}

func TestTemplateRejects(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	tok := adminLogin(t, ts.URL)
	res := do(t, "GET", ts.URL+"/api/admin/template?parts=nichts", tok, "")
	if res.StatusCode != 400 {
		t.Fatalf("leere Auswahl: %d", res.StatusCode)
	}
	res.Body.Close()
	if _, err := parseTemplate([]byte("kein zip")); err != ErrTplFormat {
		t.Fatalf("kein zip: %v", err)
	}
	res = do(t, "POST", ts.URL+"/api/admin/template/apply", tok, `{"token":"x","parts":["roles"],"mode":"merge"}`)
	if res.StatusCode != http.StatusGone {
		t.Fatalf("unbekanntes Token: %d", res.StatusCode)
	}
	res.Body.Close()
	res = do(t, "GET", ts.URL+"/api/admin/template?parts=roles", "", "")
	if res.StatusCode != 401 {
		t.Fatalf("ohne Anmeldung: %d", res.StatusCode)
	}
	res.Body.Close()
}

// Eigene Übersetzungen und Sprachen wandern mit der Vorlage (Platzhalter werden geprüft).
func TestTemplateI18n(t *testing.T) {
	tsA, srvA := testServerWith(t, nil)
	tokA := adminLogin(t, tsA.URL)
	catA := srvA.hub.I18n()
	if _, err := catA.SetText("en", "app.senden", "Go!"); err != nil {
		t.Fatal(err)
	}
	catA.AddLanguage("fr", "Français", "fr-FR")
	catA.SetText("fr", "app.senden", "Envoyer")
	zip := tplExport(t, tsA.URL, tokA, "i18n")

	tsB, srvB := testServerWith(t, nil)
	tokB := adminLogin(t, tsB.URL)
	catB := srvB.hub.I18n()
	catB.SetText("en", "app.senden", "Send it")
	res := tplImport(t, tsB.URL, tokB, zip, []string{"i18n"}, "merge")
	if len(res) != 1 || res[0].Added != 1 || res[0].Skipped != 1 {
		t.Fatalf("ergänzen: %+v", res)
	}
	if got := catB.T("en", "app.senden", nil); got != "Send it" {
		t.Fatalf("Vorhandenes muss bleiben: %q", got)
	}
	if got := catB.T("fr", "app.senden", nil); got != "Envoyer" {
		t.Fatalf("eigene Sprache: %q", got)
	}
	tplImport(t, tsB.URL, tokB, zip, []string{"i18n"}, "replace")
	if got := catB.T("en", "app.senden", nil); got != "Go!" {
		t.Fatalf("ersetzen: %q", got)
	}
}
