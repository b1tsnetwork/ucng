// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type chanEnv struct {
	ts    *httptest.Server
	hub   *Hub
	srv   *Server
	admin string
}

func (e *chanEnv) domain() string { return e.srv.fedDomain() }

// chanServer: Instanz im Modus „simple“ mit eingeschalteter Kanal-Föderation (über http).
func chanServer(t *testing.T, userSubs bool) *chanEnv {
	t.Helper()
	cfg := Config{MaxRooms: 20, RateLimit: 100, RateWindow: time.Second, ClaimLimit: 100, AdminPassword: testAdminPW, BotRate: 1000, FedInsecure: true}
	hub := NewHub(cfg.MaxRooms)
	srv := NewServer(cfg, hub, NewUsers(time.Hour, 100))
	srv.fedRL = newRateLimiter(1000, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() { close(srv.done); ts.Close() })
	srv.cfg.PublicURL = ts.URL
	hub.EnsureDefaultRoom()
	hub.chanFed.SetConfig(ChanFedConfig{Enabled: true, UserSubs: userSubs})
	go srv.runFed(ctx)
	go srv.runChanFed(ctx)
	return &chanEnv{ts: ts, hub: hub, srv: srv, admin: adminLogin(t, ts.URL)}
}

func roomHist(t *testing.T, e *chanEnv, tok, room string) []Message {
	t.Helper()
	res := do(t, "GET", e.ts.URL+"/api/rooms/"+room+"/messages", tok, "")
	var out []Message
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return out
}

func roomNames(t *testing.T, e *chanEnv, tok string) map[string]RoomInfo {
	t.Helper()
	res := do(t, "GET", e.ts.URL+"/api/rooms", tok, "")
	var list []RoomInfo
	json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	out := map[string]RoomInfo{}
	for _, r := range list {
		out[r.Name] = r
	}
	return out
}

func TestChannelFederation(t *testing.T) {
	O, S := chanServer(t, true), chanServer(t, true)
	O.hub.CreateRoom("news", "Neuigkeiten")
	O.hub.CreateRoom("random", "")
	O.hub.CreateRoom("geheim", "")
	alice := claim(t, O.ts.URL, "alice")
	post(t, O.ts.URL+"/api/rooms/news/messages", alice, `{"text":"Erste"}`)
	post(t, O.ts.URL+"/api/rooms/news/messages", alice, `{"text":"Zweite"}`)

	// nicht freigegeben: kein Abo
	if res := post(t, S.ts.URL+"/api/admin/channelfed/subscribe", S.admin, jsonBody(map[string]string{"address": "#geheim@" + O.domain()})); res.StatusCode != 404 {
		t.Fatalf("nicht freigegeben: %d", res.StatusCode)
	}
	if res := do(t, "PUT", O.ts.URL+"/api/admin/rooms/news/share", O.admin, `{"mode":"write"}`); res.StatusCode != 200 {
		t.Fatalf("freigeben: %d", res.StatusCode)
	}
	do(t, "PUT", O.ts.URL+"/api/admin/rooms/random/share", O.admin, `{"mode":"read"}`)
	// Liste beim Ursprung (öffentlich)
	lst := jmap(t, do(t, "GET", O.ts.URL+"/api/fed/channels", "", ""))
	if chs, _ := lst["channels"].([]any); len(chs) != 2 {
		t.Fatalf("Liste: %v", lst)
	}

	// Admin abonniert für alle
	res := post(t, S.ts.URL+"/api/admin/channelfed/subscribe", S.admin, jsonBody(map[string]string{"address": "#news@" + O.domain()}))
	sub := jmap(t, res)
	if res.StatusCode != 200 || sub["room"] != "news" {
		t.Fatalf("abonnieren: %d %v", res.StatusCode, sub)
	}
	bob := claim(t, S.ts.URL, "bob")
	rooms := roomNames(t, S, bob)
	if ri := rooms["news"]; ri.Remote == nil || ri.Remote.Mode != ShareWrite || ri.Description != "Neuigkeiten" || !ri.CanWrite {
		t.Fatalf("Spiegel: %+v %+v", ri, ri.Remote)
	}
	h := roomHist(t, S, bob, "news")
	if len(h) != 2 || h[0].Text != "Erste" || h[0].Fed == nil || h[0].Fed.From != O.domain() || !strings.HasPrefix(h[0].UserID, "fed:"+O.domain()+"/") {
		t.Fatalf("Verlauf beim Abo: %+v", h)
	}

	// neue Nachricht beim Ursprung kommt an
	post(t, O.ts.URL+"/api/rooms/news/messages", alice, `{"text":"Dritte"}`)
	waitFor(t, "Dritte bei S", func() bool { h := roomHist(t, S, bob, "news"); return len(h) == 3 && h[2].Text == "Dritte" })

	// Bob schreibt im Spiegel – landet beim Ursprung und genau einmal bei S
	res = post(t, S.ts.URL+"/api/rooms/news/messages", bob, `{"text":"Hallo von S","replyTo":`+jsonBody(roomHist(t, S, bob, "news")[2].ID)+`}`)
	var mine Message
	json.NewDecoder(res.Body).Decode(&mine)
	if res.StatusCode != 201 || mine.Text != "Hallo von S" {
		t.Fatalf("schreiben im Spiegel: %d %+v", res.StatusCode, mine)
	}
	oh := hist(O.hub, "news")
	last := oh[len(oh)-1]
	if last.Text != "Hallo von S" || last.Fed == nil || last.Fed.From != S.domain() || !strings.HasPrefix(last.UserID, "fed:"+S.domain()+"/") || last.ReplyTo != oh[2].ID {
		t.Fatalf("beim Ursprung: %+v", last)
	}
	time.Sleep(300 * time.Millisecond)
	if h := roomHist(t, S, bob, "news"); len(h) != 4 || h[3].ReplyTo != h[2].ID {
		t.Fatalf("doppelt oder ohne Antwort-Bezug: %d %+v", len(h), h[len(h)-1])
	}
	// bearbeiten (eigene) und fremde nicht
	if res := do(t, "PATCH", S.ts.URL+"/api/rooms/news/messages/"+jsonBody(mine.ID), bob, `{"text":"Hallo von S!"}`); res.StatusCode != 200 {
		t.Fatalf("bearbeiten: %d", res.StatusCode)
	}
	if oh := hist(O.hub, "news"); oh[len(oh)-1].Text != "Hallo von S!" {
		t.Fatalf("Bearbeitung beim Ursprung: %+v", oh[len(oh)-1])
	}
	if res := do(t, "PATCH", S.ts.URL+"/api/rooms/news/messages/"+jsonBody(roomHist(t, S, bob, "news")[0].ID), bob, `{"text":"x"}`); res.StatusCode != 403 {
		t.Fatalf("fremde bearbeiten: %d", res.StatusCode)
	}
	// Ursprung bearbeitet – kommt an
	do(t, "PATCH", O.ts.URL+"/api/rooms/news/messages/"+jsonBody(oh[2].ID), alice, `{"text":"Dritte (neu)"}`)
	waitFor(t, "Bearbeitung bei S", func() bool { return roomHist(t, S, bob, "news")[2].Text == "Dritte (neu)" })
	// Reaktionen und Bilder gehen nicht
	if res := post(t, S.ts.URL+"/api/rooms/news/messages/"+jsonBody(mine.ID)+"/react", bob, `{"emoji":"👍"}`); res.StatusCode != 403 {
		t.Fatalf("Reaktion: %d", res.StatusCode)
	}

	// nur lesen: Schreiben abgelehnt
	do(t, "PUT", O.ts.URL+"/api/admin/rooms/news/share", O.admin, `{"mode":"read"}`)
	waitFor(t, "Modus read bei S", func() bool {
		ri := roomNames(t, S, bob)["news"]
		return ri.Remote != nil && ri.Remote.Mode == ShareRead && !ri.CanWrite
	})
	if res := post(t, S.ts.URL+"/api/rooms/news/messages", bob, `{"text":"geht nicht"}`); res.StatusCode != 403 {
		t.Fatalf("nur lesen: %d", res.StatusCode)
	}

	// Durchsuchen
	br := jmap(t, do(t, "GET", S.ts.URL+"/api/channels/remote/browse?domain="+O.domain(), bob, ""))
	chs, _ := br["channels"].([]any)
	if len(chs) != 2 || chs[0].(map[string]any)["room"] != "news" {
		t.Fatalf("durchsuchen: %v", br)
	}

	// persönliches Abo: nur bei carl in der Liste
	carl := claim(t, S.ts.URL, "carl")
	res = post(t, S.ts.URL+"/api/channels/remote", carl, jsonBody(map[string]string{"address": "random@" + O.domain()}))
	ps := jmap(t, res)
	if res.StatusCode != 200 || ps["room"] != "random" {
		t.Fatalf("persönlich: %d %v", res.StatusCode, ps)
	}
	if _, ok := roomNames(t, S, carl)["random"]; !ok {
		t.Fatal("carl sieht sein Abo nicht")
	}
	if _, ok := roomNames(t, S, bob)["random"]; ok {
		t.Fatal("bob sieht carls Abo")
	}
	if !O.hub.chanFed.hasSub("random", S.domain()) {
		t.Fatal("Ursprung kennt das Abo nicht")
	}
	if res := do(t, "DELETE", S.ts.URL+"/api/channels/remote/random", carl, ""); res.StatusCode != 204 {
		t.Fatalf("abbestellen: %d", res.StatusCode)
	}
	if S.hub.HasRoom("random") {
		t.Fatal("Spiegel ohne Abos bleibt")
	}
	waitFor(t, "Abmeldung beim Ursprung", func() bool { return !O.hub.chanFed.hasSub("random", S.domain()) })

	// Umbenennen beim Ursprung: Spiegel folgt
	name := "neuigkeiten"
	if _, err := O.hub.UpdateRoomWith("news", RoomInput{Name: &name, Description: "Neu"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Umbenennen bei S", func() bool { rc, _ := S.hub.Remote("news"); return rc.Channel == "neuigkeiten" })
	post(t, O.ts.URL+"/api/rooms/neuigkeiten/messages", alice, `{"text":"Nach dem Umbenennen"}`)
	waitFor(t, "Nachricht nach Umbenennen", func() bool { h := roomHist(t, S, bob, "news"); return h[len(h)-1].Text == "Nach dem Umbenennen" })

	// Freigabe beenden: Spiegel bleibt lesbar, ist aber beendet
	do(t, "PUT", O.ts.URL+"/api/admin/rooms/neuigkeiten/share", O.admin, `{"mode":""}`)
	waitFor(t, "beendet bei S", func() bool { rc, _ := S.hub.Remote("news"); return rc.Ended })
	if O.hub.chanFed.hasSub("neuigkeiten", S.domain()) {
		t.Fatal("Abonnent bleibt nach Ende der Freigabe")
	}

	// Kanäle mit Leserollen lassen sich nicht freigeben
	O.hub.UpdateRoomWith("geheim", RoomInput{ReadRoles: []string{RoleTeamOnly}})
	if res := do(t, "PUT", O.ts.URL+"/api/admin/rooms/geheim/share", O.admin, `{"mode":"read"}`); res.StatusCode != 403 {
		t.Fatalf("privater Kanal freigegeben: %d", res.StatusCode)
	}
}

func TestChannelFederationOff(t *testing.T) {
	O, S := chanServer(t, false), chanServer(t, false)
	O.hub.CreateRoom("news", "")
	do(t, "PUT", O.ts.URL+"/api/admin/rooms/news/share", O.admin, `{"mode":"read"}`)
	bob := claim(t, S.ts.URL, "bob")
	if res := post(t, S.ts.URL+"/api/channels/remote", bob, jsonBody(map[string]string{"address": "#news@" + O.domain()})); res.StatusCode != 403 {
		t.Fatalf("Personen ohne Erlaubnis: %d", res.StatusCode)
	}
	O.hub.chanFed.SetConfig(ChanFedConfig{Enabled: false})
	if res := post(t, S.ts.URL+"/api/admin/channelfed/subscribe", S.admin, jsonBody(map[string]string{"address": "#news@" + O.domain()})); res.StatusCode < 400 {
		t.Fatalf("Ursprung aus: %d", res.StatusCode)
	}
	if res := do(t, "GET", O.ts.URL+"/api/fed/channels", "", ""); res.StatusCode != 404 {
		t.Fatalf("Liste bei ausgeschalteter Föderation: %d", res.StatusCode)
	}
	// gesperrte Instanz
	O.hub.chanFed.SetConfig(ChanFedConfig{Enabled: true, Deny: []string{fedHost(S.domain())}})
	if res := post(t, S.ts.URL+"/api/admin/channelfed/subscribe", S.admin, jsonBody(map[string]string{"address": "#news@" + O.domain()})); res.StatusCode < 400 {
		t.Fatalf("gesperrt: %d", res.StatusCode)
	}
	if _, _, err := parseChanAddr("https://chat.example.org/#Lobby"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"news", "#@x.org", "#a b@x.org", "#dm-x@x.org"} {
		if _, _, err := parseChanAddr(bad); err == nil {
			t.Fatalf("%q angenommen", bad)
		}
	}
}

// Ein Ursprung kann keine Nachrichten als „eigene“ von Personen des Abonnenten unterschieben.
func TestChannelFederationForgedOwn(t *testing.T) {
	O, S := chanServer(t, true), chanServer(t, true)
	O.hub.CreateRoom("news", "")
	do(t, "PUT", O.ts.URL+"/api/admin/rooms/news/share", O.admin, `{"mode":"write"}`)
	post(t, S.ts.URL+"/api/admin/channelfed/subscribe", S.admin, jsonBody(map[string]string{"address": "#news@" + O.domain()}))
	bob := claim(t, S.ts.URL, "bob")
	bobID := ""
	for _, u := range S.srv.users.export() {
		if u.Name == "bob" {
			bobID = u.ID
		}
	}
	// gefälscht: „von bob@S“, kommt aber nur als Lieferung des Ursprungs
	S.srv.mirrorPut("news", chanMsg{ID: 999, User: fedPrefix + S.domain() + "/" + bobID, Name: "bob", From: S.domain(), Text: "gefälscht", Time: time.Now()}, O.domain(), false)
	h := roomHist(t, S, bob, "news")
	last := h[len(h)-1]
	if last.Text != "gefälscht" || last.UserID == fedPrefix+S.domain()+"/"+bobID || last.Fed.From != O.domain() {
		t.Fatalf("gefälschte eigene Nachricht: %+v %+v", last, last.Fed)
	}
	if res := do(t, "DELETE", S.ts.URL+"/api/rooms/news/messages/"+jsonBody(last.ID), bob, ""); res.StatusCode != 403 {
		t.Fatalf("gefälschte löschen: %d", res.StatusCode)
	}
	// echte eigene Nachricht: bob darf sie löschen
	res := post(t, S.ts.URL+"/api/rooms/news/messages", bob, `{"text":"echt"}`)
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	if m.UserID != fedPrefix+S.domain()+"/"+bobID {
		t.Fatalf("eigene Nachricht: %+v", m)
	}
	if res := do(t, "DELETE", S.ts.URL+"/api/rooms/news/messages/"+jsonBody(m.ID), bob, ""); res.StatusCode != 200 {
		t.Fatalf("eigene löschen: %d", res.StatusCode)
	}
	waitFor(t, "gelöscht beim Ursprung", func() bool { oh := hist(O.hub, "news"); return oh[len(oh)-1].Deleted })
}
