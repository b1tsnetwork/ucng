// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Bot mit bestimmten Rechten über die Admin-API anlegen; liefert ID und Schlüssel.
func makeBot(t *testing.T, base, adm, body string) (string, string) {
	t.Helper()
	res := expect(t, do(t, "POST", base+"/api/admin/bots", adm, body), 201, "Bot anlegen")
	out := decode[struct {
		Bot   BotInfo
		Token string
	}](t, res)
	return out.Bot.ID, out.Token
}

func TestRoomBotRules(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"news","slowMode":60,"links":"team","botSlowMode":0,"botLinks":"none"}`), 201, "Kanal")
	_, tok := makeBot(t, ts.URL, adm, `{"name":"Ticker","allRooms":true}`)
	url := ts.URL + "/api/bot/rooms/news/messages"
	// Langsam-Modus für Personen gilt nicht für Bots, eigene Link-Regel schon
	expect(t, post(t, url, tok, `{"text":"eins"}`), 201, "1")
	expect(t, post(t, url, tok, `{"text":"zwei"}`), 201, "2 (kein Langsam-Modus für Bots)")
	res := post(t, url, tok, `{"text":"siehe https://example.com"}`)
	if res.StatusCode != 403 || !strings.Contains(errText(res), "Bots dürfen") {
		t.Fatal("Link trotz Bot-Regel none")
	}
	// Bot-Langsam-Modus
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/news", adm, `{"description":"","botSlowMode":30,"botLinks":"all"}`), 200, "Bot-Regeln")
	if res := post(t, url, tok, `{"text":"gleich noch eine"}`); res.StatusCode != 429 {
		t.Fatalf("Bot-Langsam-Modus: %d", res.StatusCode)
	}
	_, tok2 := makeBot(t, ts.URL, adm, `{"name":"Ticker2","allRooms":true}`)
	expect(t, post(t, url, tok2, `{"text":"https://example.com"}`), 201, "Link erlaubt")
	// Personen-Regeln blieben erhalten
	var rooms []RoomInfo
	json.NewDecoder(do(t, "GET", ts.URL+"/api/rooms", "", "").Body).Decode(&rooms)
	for _, r := range rooms {
		if r.Name == "news" && (r.SlowMode != 60 || r.Links != LinksTeam || r.BotSlowMode != 30) {
			t.Fatalf("%+v", r)
		}
	}
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/news", adm, `{"description":"","botLinks":"known"}`), 400, "ungültige Bot-Regel")
}

func TestRoomAutomodModes(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"mist"}})
	h.CreateRoom("kinder", "")
	h.CreateRoom("frei", "")
	p := Poster{ID: "x", Name: "X"}
	if _, err := h.SetRoomAutomod("kinder", RoomAutomod{Mode: AutomodBoth, Patterns: []string{"doof*"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetRoomAutomod("frei", RoomAutomod{Mode: AutomodOff}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetRoomAutomod("frei", RoomAutomod{Mode: "quatsch"}); err != ErrAutomodMode {
		t.Fatal(err)
	}
	cases := []struct {
		room, text string
		hidden     bool
	}{
		{"lobby", "so ein Mist", true}, {"lobby", "du bist doof", false},
		{"kinder", "so ein Mist", true}, {"kinder", "du bist d00f", true},
		{"frei", "so ein Mist", false},
	}
	for i, c := range cases {
		m, err := h.Send(c.room, Poster{ID: p.ID + string(rune('a'+i)), Name: "X"}, c.text, 0)
		if err != nil {
			t.Fatal(err)
		}
		if m.Deleted != c.hidden {
			t.Errorf("#%s %q: ausgeblendet=%v", c.room, c.text, m.Deleted)
		}
	}
	if q := h.AutomodQueue(); len(q) != 3 || !strings.Contains(q[0].Message.Match, "(#kinder)") {
		t.Fatalf("Prüfliste: %+v", q)
	}
	if pat, _, ok := h.TestAutomod("kinder", "doofi"); !ok || pat != "doof* (#kinder)" {
		t.Fatalf("Test: %q %v", pat, ok)
	}
	if _, _, ok := h.TestAutomod("frei", "Mist"); ok {
		t.Fatal("Test im Kanal ohne Automod schlägt an")
	}
	// Nur eigene Regeln
	h.SetRoomAutomod("kinder", RoomAutomod{Mode: AutomodOwn, Patterns: []string{"doof*"}})
	if m, _ := h.Send("kinder", Poster{ID: "y", Name: "Y"}, "Mist!", 0); m.Deleted {
		t.Fatal("global trotz own")
	}
	flush(t, h, u, s)
	s.Close()
	h2, _, _ := openStore(t, dir)
	if cfg, _ := h2.RoomAutomodConfig("kinder"); cfg.Mode != AutomodOwn || len(cfg.Patterns) != 1 {
		t.Fatalf("nach Neustart: %+v", cfg)
	}
	name := "kids"
	h2.UpdateRoomWith("kinder", RoomInput{Name: &name})
	if cfg, _ := h2.RoomAutomodConfig("kids"); cfg.Mode != AutomodOwn {
		t.Fatal("Regeln beim Umbenennen verloren")
	}
}

func TestRoomAutomodAPI(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "PUT", ts.URL+"/api/admin/rooms/lobby/automod", adm, `{"mode":"both","patterns":["sp*m","*"]}`), 400, "ungültiges Muster")
	cfg := decode[RoomAutomod](t, expect(t, do(t, "PUT", ts.URL+"/api/admin/rooms/lobby/automod", adm, `{"mode":"both","patterns":["spam*"]}`), 200, "speichern"))
	if cfg.Mode != "both" || cfg.Patterns[0] != "spam*" {
		t.Fatalf("%+v", cfg)
	}
	r := decode[map[string]any](t, do(t, "POST", ts.URL+"/api/admin/automod/test", adm, `{"text":"Spammer!","room":"lobby"}`))
	if r["match"] != true {
		t.Fatalf("%+v", r)
	}
	r = decode[map[string]any](t, do(t, "POST", ts.URL+"/api/admin/automod/test", adm, `{"text":"Spammer!"}`))
	if r["match"] != false {
		t.Fatal("globaler Test kennt Kanal-Regel")
	}
}

func TestBotPermissions(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"support"}`), 201, "Kanal")
	// Nur-Lese-Bot
	_, ro := makeBot(t, ts.URL, adm, `{"name":"Leser","allRooms":true,"perms":{"write":false,"history":false,"presence":true}}`)
	expect(t, post(t, ts.URL+"/api/bot/rooms/lobby/messages", ro, `{"text":"hi"}`), 403, "schreiben ohne Recht")
	expect(t, do(t, "GET", ts.URL+"/api/bot/rooms/lobby/messages", ro, ""), 403, "Verlauf ohne Recht")
	expect(t, do(t, "GET", ts.URL+"/api/bot/rooms/lobby/presence", ro, ""), 200, "Online-Liste")
	me := decode[botSelf](t, do(t, "GET", ts.URL+"/api/bot/me", ro, ""))
	if me.Perms.Write || !me.Perms.Presence {
		t.Fatalf("me: %+v", me.Perms)
	}

	// Voller Bot: antworten, reagieren, bearbeiten, eigene löschen, von Automod ausgenommen
	srv.hub.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"mist"}})
	_, full := makeBot(t, ts.URL, adm, `{"name":"Helfer","rooms":["support"],"perms":{"write":true,"history":true,"react":true,"editOwn":true,"automodExempt":true,"receive":"commands"}}`)
	user := claim(t, ts.URL, "Gast")
	q := decode[Message](t, post(t, ts.URL+"/api/rooms/support/messages", user, `{"text":"!ping bitte"}`))
	a := decode[Message](t, expect(t, post(t, ts.URL+"/api/bot/rooms/support/messages", full, `{"text":"So ein Mist, hier: …","replyTo":`+itoa64(q.ID)+`}`), 201, "Antwort"))
	if a.Deleted || a.Reply == nil || a.Reply.Name != "Gast" {
		t.Fatalf("Antwort: %+v", a)
	}
	expect(t, post(t, ts.URL+"/api/bot/rooms/lobby/messages", full, `{"text":"x"}`), 403, "fremder Kanal")
	expect(t, do(t, "POST", ts.URL+"/api/bot/rooms/support/messages/"+itoa64(q.ID)+"/react", full, `{"emoji":"👍"}`), 200, "Reaktion")
	ed := decode[Message](t, expect(t, do(t, "PATCH", ts.URL+"/api/bot/rooms/support/messages/"+itoa64(a.ID), full, `{"text":"Erledigt ✓"}`), 200, "bearbeiten"))
	if ed.Text != "Erledigt ✓" || ed.EditedAt == nil {
		t.Fatalf("%+v", ed)
	}
	expect(t, do(t, "PATCH", ts.URL+"/api/bot/rooms/support/messages/"+itoa64(q.ID), full, `{"text":"gehackt"}`), 403, "fremde bearbeiten")
	expect(t, do(t, "DELETE", ts.URL+"/api/bot/rooms/support/messages/"+itoa64(q.ID), full, ""), 403, "fremde löschen ohne Recht")
	del := decode[Message](t, expect(t, do(t, "DELETE", ts.URL+"/api/bot/rooms/support/messages/"+itoa64(a.ID), full, ""), 200, "eigene löschen"))
	if del.DeletedBy != "self" {
		t.Fatalf("%+v", del)
	}

	// Empfangsfilter „nur Befehle“
	b, _ := srv.hub.Bots().Auth(full)
	mk := func(text string, reply *ReplyPreview) BusEvent {
		return BusEvent{Type: "message", Room: "support", Message: &Message{ID: 1, UserID: "u", Name: "Gast", Text: text, Reply: reply}}
	}
	for _, c := range []struct {
		ev   BusEvent
		want bool
	}{
		{mk("!status", nil), true}, {mk("/hilfe", nil), true}, {mk("Hallo @Helfer", nil), true},
		{mk("danke", &ReplyPreview{Name: "Helfer"}), true}, {mk("einfach so", nil), false},
	} {
		if b.wants(c.ev) != c.want {
			t.Errorf("wants(%q) = %v", c.ev.Message.Text, !c.want)
		}
	}
}

func TestBotSanctions(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	_, mod := makeBot(t, ts.URL, adm, `{"name":"Wache","allRooms":true,"perms":{"write":true,"delete":true,"mute":true,"ban":true,"maxMinutes":120}}`)
	_, mute := makeBot(t, ts.URL, adm, `{"name":"Leise","allRooms":true,"perms":{"write":true,"mute":true,"maxMinutes":30}}`)
	tok := claim(t, ts.URL, "Rüpel")
	id := sess(t, ts.URL, tok).ID
	post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"hallo"}`).Body.Close()

	expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+id+"/ban", mute, `{"minutes":10}`), 403, "ban ohne Recht")
	expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+id+"/mute", mute, `{"minutes":60}`), 400, "länger als erlaubt")
	expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+id+"/mute", mute, `{"minutes":20}`), 200, "mute")
	// ein Bot hebt nur eigene Sperren auf
	expect(t, do(t, "DELETE", ts.URL+"/api/bot/users/"+id+"/sanction", mod, ""), 403, "fremde Sperre")
	expect(t, do(t, "DELETE", ts.URL+"/api/bot/users/"+id+"/sanction", mute, ""), 204, "eigene Sperre")
	expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+id+"/ban", mod, `{"minutes":120,"reason":"Spam"}`), 200, "ban")
	sa, ok := srv.hub.Mod().Active(id, "")
	if !ok || sa.Kind != "ban" || sa.Until == nil || time.Until(*sa.Until) > 121*time.Minute {
		t.Fatalf("%+v", sa)
	}
	// eine kürzere Stummschaltung ersetzt den Ausschluss nicht
	out := decode[map[string]any](t, expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+id+"/mute", mute, `{"minutes":5}`), 200, "mute"))
	if out["unchanged"] != true {
		t.Fatalf("%+v", out)
	}
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"noch da?"}`), 403, "gesperrt")
}

func TestLegacyBotCanModerate(t *testing.T) {
	b := Bot{CanModerate: true}
	if p := b.P(); !p.Delete || !p.Mute || p.Ban || !p.Write || p.MaxMinutes != 24*60 {
		t.Fatalf("%+v", p)
	}
	if p := (Bot{}).P(); p.Delete || !p.Write || !p.History || p.Receive != BotReceiveAll {
		t.Fatalf("%+v", p)
	}
}
