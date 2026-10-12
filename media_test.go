// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upload schickt eine Datei als multipart/form-data.
func upload(t *testing.T, url, token string, fields map[string]string, filename string, data []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEmotesAPI(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	mod := func() string {
		do(t, "POST", ts.URL+"/api/admin/accounts", adm, `{"name":"mo","role":"moderator","password":"mo-passwort-12"}`).Body.Close()
		return loginAs(t, ts.URL, "mo", "mo-passwort-12")
	}()
	img := testPNG(200, 120, color.RGBA{200, 50, 0, 255})
	expect(t, upload(t, ts.URL+"/api/admin/emotes", mod, nil, "katze.png", img), 403, "Moderator")
	e := decode[EmoteInfo](t, expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, map[string]string{"size": "64"}, "Katze.png", img), 201, "hochladen"))
	if e.Name != "katze" || e.Size != 64 || !strings.HasPrefix(e.URL, "/media/emotes/katze-") {
		t.Fatalf("%+v", e)
	}
	expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, map[string]string{"name": "katze"}, "x.png", img), 409, "doppelt")
	expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, map[string]string{"name": "a"}, "x.png", img), 400, "Name zu kurz")
	expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, map[string]string{"name": "text"}, "x.png", []byte("hallo")), 400, "kein Bild")

	// Datei wird ausgeliefert – genau 64×64
	res := expect(t, do(t, "GET", ts.URL+e.URL, "", ""), 200, "Datei")
	if !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatal("Cache-Header fehlt")
	}
	pic, err := png.Decode(res.Body)
	if err != nil || pic.Bounds().Dx() != 64 || pic.Bounds().Dy() != 64 {
		t.Fatalf("%v %v", err, pic.Bounds())
	}
	expect(t, do(t, "GET", ts.URL+"/media/emotes/..%2Fmedia.json", "", ""), 404, "Pfad")

	// öffentlich sichtbar
	pub := decode[PublicMedia](t, do(t, "GET", ts.URL+"/api/media", "", ""))
	if len(pub.Emotes) != 1 || pub.Emotes[0].Name != "katze" || !pub.Gifs || pub.Web != "" {
		t.Fatalf("%+v", pub)
	}
	// Größe ändern (aus dem Original) und umbenennen
	e2 := decode[EmoteInfo](t, expect(t, do(t, "PATCH", ts.URL+"/api/admin/emotes/katze", adm, `{"name":"kater","size":32}`), 200, "ändern"))
	if e2.Name != "kater" || e2.Size != 32 {
		t.Fatalf("%+v", e2)
	}
	pic, _ = png.Decode(do(t, "GET", ts.URL+e2.URL, "", "").Body)
	if pic.Bounds().Dx() != 32 {
		t.Fatalf("%v", pic.Bounds())
	}
	expect(t, do(t, "GET", ts.URL+e.URL, "", ""), 404, "alte Datei entfernt")
	// animiertes Emote
	a := decode[EmoteInfo](t, expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, map[string]string{"name": "tanz"}, "tanz.gif", testGIF(80, 80, 4)), 201, "GIF-Emote"))
	if !a.Animated || a.Size != 32 || !strings.HasSuffix(a.URL, ".gif") {
		t.Fatalf("%+v", a)
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/emotes/tanz", adm, ""), 204, "löschen")
	if len(srv.hub.Media().Emotes()) != 1 {
		t.Fatal("nicht gelöscht")
	}
}

func TestGIFLibraryAndMessages(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	g := decode[GIFInfo](t, expect(t, upload(t, ts.URL+"/api/admin/gifs", adm, map[string]string{"title": "Applaus", "tags": "klatschen, super"}, "applaus.gif", testGIF(480, 270, 6)), 201, "GIF"))
	if g.W != 300 || g.H != 169 || len(g.Tags) != 2 {
		t.Fatalf("%+v", g)
	}
	expect(t, upload(t, ts.URL+"/api/admin/gifs", adm, nil, "bild.png", testPNG(10, 10, color.White)), 400, "kein GIF")
	if hits := decode[[]GIFInfo](t, do(t, "GET", ts.URL+"/api/gifs?q=super", "", "")); len(hits) != 1 {
		t.Fatalf("Suche: %+v", hits)
	}
	if hits := decode[[]GIFInfo](t, do(t, "GET", ts.URL+"/api/gifs?q=katze", "", "")); len(hits) != 0 {
		t.Fatal("falscher Treffer")
	}

	tok := claim(t, ts.URL, "Gina")
	m := decode[Message](t, expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"lib:`+g.ID+`"}`), 201, "GIF senden"))
	if m.Media == nil || m.Media.Src != g.URL || m.Media.W != 300 || m.Text != "" {
		t.Fatalf("%+v", m.Media)
	}
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"lib:g_gibtsnicht"}`), 404, "unbekannt")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":""}`), 400, "leer ohne GIF")
	// Antwort auf ein GIF zeigt „GIF: Titel“
	r := decode[Message](t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, fmt.Sprintf(`{"text":"haha","replyTo":%d}`, m.ID)))
	if r.Reply == nil || r.Reply.Text != "GIF: Applaus" {
		t.Fatalf("%+v", r.Reply)
	}
	// moderativ gelöscht: Bild verschwindet für alle, Admins sehen es; Wiederherstellen geht
	srv.hub.ModDelete("lobby", m.ID, "mod", "x")
	if hist(srv.hub, "lobby")[0].Media != nil || adminHist(srv.hub, "lobby")[0].Media == nil {
		t.Fatal("Bild nach Löschen")
	}
	if _, err := srv.hub.RestoreMessage("lobby", m.ID); err != nil || hist(srv.hub, "lobby")[0].Media == nil {
		t.Fatal("Wiederherstellen eines GIFs")
	}
	// GIFs ausschalten
	expect(t, do(t, "PUT", ts.URL+"/api/admin/media/settings", adm, `{"emoteSize":32,"gifsEnabled":false,"gifWidth":300}`), 200, "aus")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"lib:`+g.ID+`"}`), 403, "GIFs aus")
	expect(t, do(t, "GET", ts.URL+"/api/gifs", "", ""), 403, "Bibliothek aus")
	// neue Größe skaliert die Bibliothek neu
	expect(t, do(t, "PUT", ts.URL+"/api/admin/media/settings", adm, `{"emoteSize":32,"gifsEnabled":true,"gifWidth":200}`), 200, "kleiner")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l := srv.hub.Media().GIFs(""); len(l) == 1 && l[0].W == 200 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nicht neu skaliert: %+v", srv.hub.Media().GIFs(""))
}

// fakeGiphy bildet die GIPHY-API nach (Suche, Abruf per ID, Bilddateien).
func fakeGiphy(t *testing.T) (*httptest.Server, *int) {
	calls := 0
	var srv *httptest.Server
	item := func(id string) map[string]any {
		img := func(name string, w, h int) map[string]string {
			return map[string]string{"url": srv.URL + "/media/" + id + "/" + name, "width": fmt.Sprint(w), "height": fmt.Sprint(h)}
		}
		return map[string]any{"id": id, "title": "Katze " + id, "images": map[string]any{
			"downsized": img("full.gif", 480, 360), "fixed_width_small": img("small.gif", 100, 75)}}
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.URL.Path == "/v1/gifs/search" || r.URL.Path == "/v1/gifs/trending":
			if r.URL.Query().Get("api_key") != "geheim" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{item("abc"), item("def")}})
		case r.URL.Path == "/v1/gifs/xyz":
			json.NewEncoder(w).Encode(map[string]any{"data": item("xyz")})
		case strings.HasPrefix(r.URL.Path, "/media/"):
			w.Header().Set("Content-Type", "image/gif")
			if strings.HasSuffix(r.URL.Path, "small.gif") {
				w.Write(testGIF(100, 75, 2))
			} else {
				w.Write(testGIF(480, 360, 5))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestWebGIFSearch(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	giphy, calls := fakeGiphy(t)
	srv.hub.Media().apiBase["giphy"] = giphy.URL

	expect(t, do(t, "GET", ts.URL+"/api/gifs/web?q=katze", "", ""), 403, "nicht eingerichtet")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/media/settings", adm, `{"emoteSize":32,"gifsEnabled":true,"gifWidth":250,"provider":"giphy","apiKey":"geheim","rating":"pg"}`), 200, "einrichten")
	set := decode[adminMedia](t, do(t, "GET", ts.URL+"/api/admin/media", adm, ""))
	if !set.Settings.HasKey || set.Settings.Provider != "giphy" {
		t.Fatalf("%+v", set.Settings)
	}
	raw, _ := io.ReadAll(do(t, "GET", ts.URL+"/api/admin/media", adm, "").Body)
	if strings.Contains(string(raw), "geheim") {
		t.Fatal("API-Schlüssel wird ausgeliefert")
	}
	if pub := decode[PublicMedia](t, do(t, "GET", ts.URL+"/api/media", "", "")); pub.Web != "giphy" {
		t.Fatal("Web-Suche nicht gemeldet")
	}
	hits := decode[[]WebGIF](t, expect(t, do(t, "GET", ts.URL+"/api/gifs/web?q=katze", "", ""), 200, "Suche"))
	if len(hits) != 2 || hits[0].ID != "giphy:abc" || !strings.HasPrefix(hits[0].Preview, "/api/gifs/web/preview/giphy:abc") {
		t.Fatalf("%+v", hits)
	}
	// Vorschau läuft über den Server
	res := expect(t, do(t, "GET", ts.URL+hits[0].Preview, "", ""), 200, "Vorschau")
	if res.Header.Get("Content-Type") != "image/gif" {
		t.Fatal(res.Header.Get("Content-Type"))
	}
	expect(t, do(t, "GET", ts.URL+"/api/gifs/web/preview/giphy:unbekannt", "", ""), 404, "fremde Vorschau")

	tok := claim(t, ts.URL, "Web")
	m := decode[Message](t, expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"giphy:abc","text":"so süß"}`), 201, "Web-GIF senden"))
	if m.Media == nil || m.Media.Source != "giphy" || m.Media.W != 250 || m.Media.H != 188 || m.Text != "so süß" || !strings.HasPrefix(m.Media.Src, "/media/web/giphy_abc-250") {
		t.Fatalf("%+v", m.Media)
	}
	g, err := gif.DecodeAll(do(t, "GET", ts.URL+m.Media.Src, "", "").Body)
	if err != nil || len(g.Image) != 5 || g.Config.Width != 250 {
		t.Fatalf("%v", err)
	}
	// zweites Senden nutzt die gespeicherte Datei
	before := *calls
	post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"giphy:abc"}`).Body.Close()
	if *calls != before {
		t.Fatal("GIF erneut geladen")
	}
	// ID, die nicht aus einer Suche stammt, wird beim Anbieter nachgeschlagen
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"giphy:xyz"}`), 201, "per ID")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"gif":"http://evil/x.gif"}`), 502, "keine Adressen")
}

func TestCommandsRun(t *testing.T) {
	c := NewCommands()
	c.rnd = func(n int) int { return n - 1 } // immer das Maximum
	now := time.Date(2026, 10, 9, 21, 5, 0, 0, time.UTC)
	online := func() Event {
		return Event{Online: 3, Users: []PresenceUser{{Name: "Anna"}, {Name: "Tom"}}, Guests: 1}
	}
	ctx := CmdContext{Room: "lobby", User: "Anna", Now: now, Online: online}
	run := func(text string) CmdResult { return c.Run(text, ctx) }

	if r := run("!würfel"); r.Text != "🎲 Anna würfelt eine **6**." || !r.Reply {
		t.Fatalf("%+v", r)
	}
	if r := run("!dice 20"); r.Text != "🎲 Anna würfelt eine **20**." {
		t.Fatalf("Alias: %+v", r)
	}
	if r := run("!würfel 1"); !strings.Contains(r.Text, "zwischen 2 und 1000") {
		t.Fatalf("%+v", r)
	}
	if r := run("!münze"); r.Text != "🪙 **Zahl**!" {
		t.Fatalf("%+v", r)
	}
	if r := run("!zeit"); r.Text != "🕒 Es ist 21:05 Uhr am 09.10.2026." {
		t.Fatalf("%+v", r)
	}
	if r := run("!online"); r.Text != "👥 3 online: Anna, Tom und 1 Gast" {
		t.Fatalf("%+v", r)
	}
	if r := run("!wähle Pizza | Pasta | Salat"); r.Text != "🤔 Ich nehme: **Salat**" {
		t.Fatalf("%+v", r)
	}
	if r := run("!wähle Pizza oder Pasta"); r.Text != "🤔 Ich nehme: **Pasta**" {
		t.Fatalf("%+v", r)
	}
	if r := run("!hilfe"); !strings.Contains(r.Text, "**!regeln** – Die Regeln im Chat") || !strings.Contains(r.Text, "**!würfel**") {
		t.Fatalf("%+v", r)
	}
	if r := run("Hallo !würfel"); r.Matched {
		t.Fatal("kein Befehl am Anfang")
	}
	if r := run("!gibtsnicht"); r.Matched {
		t.Fatal("unbekannt")
	}
	// Abklingzeit (regeln: 30 s) je Kanal
	if r := run("!regeln"); !r.Matched || !strings.Contains(r.Text, "Danke, Anna!") {
		t.Fatalf("%+v", r)
	}
	if r := run("!regeln"); r.Matched {
		t.Fatal("Abklingzeit ignoriert")
	}
	if r := c.Run("!regeln", CmdContext{Room: "andere", User: "Bo", Now: now}); !r.Matched {
		t.Fatal("Abklingzeit gilt je Kanal")
	}

	// eigener Befehl, nur für das Team, nur in #support
	cfg := c.Config()
	cfg.Prefix = "?"
	cfg.Commands = append(cfg.Commands, Command{Trigger: "!Ticket", Aliases: []string{"t"}, Kind: CmdText, Enabled: true,
		TeamOnly: true, Rooms: []string{"support"}, Response: "Ticket für {args} angelegt von {user} in #{room}"})
	out, err := c.Set(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Commands[len(out.Commands)-1].Trigger != "ticket" || out.Commands[len(out.Commands)-1].ID == "" {
		t.Fatalf("%+v", out.Commands[len(out.Commands)-1])
	}
	team := CmdContext{Room: "support", User: "Tina", Team: true, Now: now}
	if r := c.Run("?t Drucker kaputt", team); r.Text != "Ticket für Drucker kaputt angelegt von Tina in #support" {
		t.Fatalf("%+v", r)
	}
	if r := c.Run("?ticket x", CmdContext{Room: "support", User: "Gast", Now: now}); r.Matched {
		t.Fatal("Team-Befehl für Gast")
	}
	if r := c.Run("?ticket x", CmdContext{Room: "lobby", User: "Tina", Team: true, Now: now}); r.Matched {
		t.Fatal("falscher Kanal")
	}
	if r := c.Run("?hilfe", CmdContext{Room: "lobby", User: "Gast", Now: now}); strings.Contains(r.Text, "ticket") {
		t.Fatal("Hilfe zeigt Team-Befehl")
	}
	// Prüfungen
	bad := c.Config()
	bad.Commands = append(bad.Commands, Command{Trigger: "hilfe", Kind: CmdText, Response: "x"})
	if _, err := c.Set(bad); err == nil {
		t.Fatal("doppelter Befehl")
	}
	bad = c.Config()
	bad.Prefix = "x"
	if _, err := c.Set(bad); err != ErrCmdPrefix {
		t.Fatal(err)
	}
}

func TestCommandBotInChat(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	// Name des Bots ist reserviert
	if res := do(t, "POST", ts.URL+"/api/session", "", `{"name":"Chat-Bot"}`); res.StatusCode != 409 {
		t.Fatalf("Bot-Name wählbar: %d", res.StatusCode)
	}
	tok := claim(t, ts.URL, "Paul")
	m := decode[Message](t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"!würfel 12"}`))
	msgs := hist(srv.hub, "lobby")
	if len(msgs) != 2 {
		t.Fatalf("keine Antwort: %+v", msgs)
	}
	bot := msgs[1]
	if !bot.Bot || bot.Name != "Chat-Bot" || bot.UserID != CmdBotID || bot.ReplyTo != m.ID || !strings.Contains(bot.Text, "Paul würfelt eine") {
		t.Fatalf("%+v", bot)
	}
	pub := decode[PublicCommands](t, do(t, "GET", ts.URL+"/api/commands?room=lobby", "", ""))
	if !pub.Enabled || pub.Prefix != "!" || len(pub.Commands) < 5 {
		t.Fatalf("%+v", pub)
	}
	// Testfeld im Admin-Bereich
	r := decode[CmdResult](t, do(t, "POST", ts.URL+"/api/admin/commands/test", adm, `{"text":"!regeln"}`))
	if !r.Matched || !strings.Contains(r.Text, "Danke, Admin!") {
		t.Fatalf("%+v", r)
	}
	// Bot-Name umbenennen auf einen vergebenen Namen geht nicht
	cfg := srv.hub.Commands().Config()
	cfg.BotName = "Paul"
	body, _ := json.Marshal(cfg)
	expect(t, do(t, "PUT", ts.URL+"/api/admin/commands", adm, string(body)), 409, "vergebener Name")
	// ausschalten
	cfg.BotName, cfg.Enabled = "Helferlein", false
	body, _ = json.Marshal(cfg)
	expect(t, do(t, "PUT", ts.URL+"/api/admin/commands", adm, string(body)), 200, "aus")
	post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"!zeit"}`).Body.Close()
	if n := len(hist(srv.hub, "lobby")); n != 3 {
		t.Fatalf("Bot antwortet trotz aus: %d", n)
	}
}

func TestMediaAndCommandsPersist(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.Media().SetDir(dir + "/media")
	if _, err := h.Media().AddEmote("party", 64, testGIF(64, 64, 3)); err != nil {
		t.Fatal(err)
	}
	h.Media().AddGIF("Tanz", []string{"party"}, testGIF(120, 90, 3))
	k := "schluessel"
	h.Media().SetConfig(MediaSettingsInput{EmoteSize: 64, GifsEnabled: true, GifWidth: 400, Provider: "tenor", APIKey: &k, Rating: "pg"})
	cfg := h.Commands().Config()
	cfg.BotName = "Robo"
	h.Commands().Set(cfg)
	flush(t, h, u, s)
	s.Close()
	h2, _, _ := openStore(t, dir)
	h2.Media().SetDir(dir + "/media")
	if e := h2.Media().Emotes(); len(e) != 1 || !e[0].Animated || e[0].Size != 64 {
		t.Fatalf("%+v", e)
	}
	if g := h2.Media().GIFs("party"); len(g) != 1 {
		t.Fatal("GIF verloren")
	}
	if c := h2.Media().Config(); c.APIKey != "schluessel" || c.GifWidth != 400 || c.Provider != "tenor" {
		t.Fatalf("%+v", c)
	}
	if h2.Commands().BotName() != "Robo" {
		t.Fatal("Befehle nicht gespeichert")
	}
	// Größe aus dem gespeicherten Original ändern
	if e, err := h2.Media().UpdateEmote("party", "", 32); err != nil || e.Size != 32 {
		t.Fatal(err)
	}
}
