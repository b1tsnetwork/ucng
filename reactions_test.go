// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"image/color"
	"strings"
	"testing"
)

func TestConfigurableReactions(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, nil, "party.png", testPNG(40, 40, color.RGBA{0, 90, 200, 255})), 201, "Emote")
	expect(t, upload(t, ts.URL+"/api/admin/emotes", adm, nil, "katze.png", testPNG(40, 40, color.RGBA{200, 90, 0, 255})), 201, "Emote")

	// Standard
	pub := decode[PublicMedia](t, do(t, "GET", ts.URL+"/api/media", "", ""))
	if len(pub.Reactions) != 6 || pub.Reactions[0] != "👍" || !pub.EmoteReactions {
		t.Fatalf("%+v", pub)
	}
	// Ungültiges
	expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":["hallo"]}`), 400, "Text")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":[":gibtsnicht:"]}`), 400, "unbekanntes Emote")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":[]}`), 400, "leer")
	expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":["1","2","3","4","5","6","7","8","9","🔥","🎉","✅","👀"]}`), 400, "Ziffern")
	// Speichern: Emoji + Emote, Emotes sonst nicht erlaubt
	rs := decode[ReactionSettings](t, expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":["🔥"," :Party: ","❤️","🔥"],"emoteReactions":false}`), 200, "speichern"))
	if strings.Join(rs.Reactions, ",") != "🔥,:party:,❤️" || rs.EmoteReactions {
		t.Fatalf("%+v", rs)
	}
	s := decode[PublicSettings](t, do(t, "GET", ts.URL+"/api/settings", "", ""))
	if strings.Join(s.Reactions, ",") != "🔥,:party:,❤️" {
		t.Fatalf("%+v", s.Reactions)
	}

	a, b := claim(t, ts.URL, "Ada"), claim(t, ts.URL, "Bert")
	base := ts.URL + "/api/rooms/lobby/messages"
	m := decode[Message](t, expect(t, post(t, base, a, `{"text":"Party heute?"}`), 201, "senden"))
	react := func(tok, e string, code int) Message {
		t.Helper()
		res := expect(t, do(t, "POST", base+"/"+itoa64(m.ID)+"/react", tok, `{"emoji":"`+e+`"}`), code, "react "+e)
		if code != 200 {
			res.Body.Close()
			return Message{}
		}
		return decode[Message](t, res)
	}
	react(b, "👍", 400)       // nicht mehr in der Auswahl
	react(b, ":katze:", 400) // Emotes nur aus der Schnellauswahl
	react(b, "❤️", 200)
	r := react(b, ":party:", 200)
	if len(r.Reactions) != 2 || r.Reactions[0].Emoji != ":party:" || r.Reactions[1].Emoji != "❤️" {
		t.Fatalf("Reihenfolge wie Auswahl: %+v", r.Reactions)
	}
	// alle Emotes erlauben
	expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":["🔥"],"emoteReactions":true}`), 200, "Emotes an").Body.Close()
	r = react(a, ":katze:", 200)
	if len(r.Reactions) != 3 || r.Reactions[2].Emoji != ":katze:" {
		t.Fatalf("%+v", r.Reactions)
	}
	// ❤️ ist nicht mehr erlaubt: Ada kann nicht mitmachen, Bert kann seine zurücknehmen
	react(a, "❤️", 400)
	r = react(b, "❤️", 200)
	for _, x := range r.Reactions {
		if x.Emoji == "❤️" {
			t.Fatal("Zurücknehmen ging nicht")
		}
	}
	// Umbenennen eines Emotes zieht die Schnellauswahl mit, Löschen nimmt es heraus
	expect(t, do(t, "PUT", ts.URL+"/api/admin/reactions", adm, `{"reactions":["🔥",":party:"],"emoteReactions":true}`), 200, "").Body.Close()
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/emotes/party", adm, `{"name":"fest"}`), 200, "umbenennen").Body.Close()
	if got := srv.hub.Media().Reactions().Reactions; strings.Join(got, ",") != "🔥,:fest:" {
		t.Fatalf("%v", got)
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/emotes/fest", adm, ""), 204, "löschen").Body.Close()
	if got := srv.hub.Media().Reactions().Reactions; strings.Join(got, ",") != "🔥" {
		t.Fatalf("%v", got)
	}
	// Grenze für verschiedene Reaktionen
	for i := 0; i < 30; i++ {
		srv.hub.media.mu.Lock()
		srv.hub.media.emotes["e"+itoa64(int64(i+10))] = &Emote{Name: "e" + itoa64(int64(i+10)), File: "x.png", Size: 32}
		srv.hub.media.mu.Unlock()
	}
	var last int
	for i := 0; i < 30; i++ {
		res := do(t, "POST", base+"/"+itoa64(m.ID)+"/react", a, `{"emoji":":e`+itoa64(int64(i+10))+`:"}`)
		last = res.StatusCode
		res.Body.Close()
	}
	if last != 400 {
		t.Fatalf("Grenze greift nicht: %d", last)
	}
}

func TestIsEmoji(t *testing.T) {
	for _, s := range []string{"👍", "❤️", "👨‍👩‍👧", "🇩🇪", "1️⃣", "✅"} {
		if !isEmoji(s) {
			t.Errorf("%q sollte gehen", s)
		}
	}
	for _, s := range []string{"", "a", "ok", "1", "#", "ä", "👍 👍", "<b>", "€uro"} {
		if isEmoji(s) {
			t.Errorf("%q sollte nicht gehen", s)
		}
	}
}
