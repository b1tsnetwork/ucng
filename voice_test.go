// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ucng/internal/voicerelay"
	"ucng/internal/wsock"
)

func voiceServer(t *testing.T, mod func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	cfg := Config{MaxRooms: 10, RateLimit: 100, RateWindow: time.Second, ClaimLimit: 100, AdminPassword: testAdminPW, BotRate: 1000}
	if mod != nil {
		mod(&cfg)
	}
	hub := NewHub(cfg.MaxRooms)
	srv := NewServer(cfg, hub, NewUsers(time.Hour, 100))
	ctx, cancel := context.WithCancel(context.Background())
	go srv.webhooks.run(ctx)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() {
		cancel()
		close(srv.done)
		if srv.voice != nil {
			srv.voice.Close()
		}
		ts.Close()
	})
	return ts, srv
}

type joinResp struct {
	URL, Ticket string
	Speak       bool
	Muted       bool
	Stage       bool
	Guest       bool
	Moderate    bool
	Bitrate     int
	MaxSpeakers int
	Error       string
}

func voiceJoin(t *testing.T, base, tok, room string) (joinResp, int) {
	t.Helper()
	res := post(t, base+"/api/voice/"+room+"/join", tok, `{}`)
	defer res.Body.Close()
	var out joinResp
	json.NewDecoder(res.Body).Decode(&out)
	return out, res.StatusCode
}

func voiceDial(t *testing.T, base string, j joinResp) (*wsock.Client, map[string]any) {
	t.Helper()
	c, err := wsock.Dial("ws"+strings.TrimPrefix(base, "http")+j.URL, http.Header{"Origin": {base}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	b, _ := json.Marshal(map[string]string{"t": "auth", "ticket": j.Ticket})
	c.WriteText(b)
	hello := readText(t, c, "hello")
	return c, hello
}

// readText liest bis zur nächsten Textnachricht vom Typ typ (Audio dazwischen wird übersprungen).
func readText(t *testing.T, c *wsock.Client, typ string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		op, p, err := c.Read(3 * time.Second)
		if err != nil {
			t.Fatalf("warte auf %q: %v (%s)", typ, err, p)
		}
		if op != wsock.OpText {
			continue
		}
		var m map[string]any
		json.Unmarshal(p, &m)
		if m["t"] == typ {
			return m
		}
	}
	t.Fatalf("keine Nachricht %q", typ)
	return nil
}

func waitPeers(t *testing.T, v *voiceMgr, room string, n int) []VoicePeer {
	t.Helper()
	for i := 0; i < 100; i++ {
		if p := v.Peers(room); len(p) == n {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: erwartet %d Teilnehmer, sind %d", room, n, len(v.Peers(room)))
	return nil
}

func TestVoiceChannel(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	name := "talk"
	if _, err := h.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{}}); err != nil {
		t.Fatal(err)
	}
	anna, ben := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")

	// Text-Kanal: kein Sprachkanal
	if _, code := voiceJoin(t, ts.URL, anna, "lobby"); code != http.StatusNotFound {
		t.Fatalf("Text-Kanal: %d", code)
	}
	ja, code := voiceJoin(t, ts.URL, anna, "talk")
	if code != 200 || !ja.Speak || ja.Bitrate != 24000 || ja.MaxSpeakers != 4 || !ja.Guest {
		t.Fatalf("Beitritt: %d %+v", code, ja)
	}
	ca, _ := voiceDial(t, ts.URL, ja)
	jb, _ := voiceJoin(t, ts.URL, ben, "talk")
	cb, hello := voiceDial(t, ts.URL, jb)
	if len(hello["peers"].([]any)) != 2 {
		t.Fatalf("hello: %v", hello)
	}
	peers := waitPeers(t, srv.voice, "talk", 2)

	// Fremde Herkunft wird abgewiesen
	if _, err := wsock.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/voice/ws", http.Header{"Origin": {"https://evil.example"}}); err == nil {
		t.Fatal("fremde Herkunft angenommen")
	}

	// Sprache wird weitergeleitet
	ca.WriteBinary([]byte{1, 0, 1, 200, 0xAA, 0xBB})
	op, p, err := cb.Read(2 * time.Second)
	for err == nil && op != wsock.OpBinary {
		op, p, err = cb.Read(2 * time.Second)
	}
	if err != nil || len(p) < 8 || p[0] != 1 || p[1] != 1 {
		t.Fatalf("Audio: %v %v", err, p)
	}

	// GET /api/voice zeigt die Teilnehmer
	res := do(t, "GET", ts.URL+"/api/voice", "", "")
	var list struct {
		Enabled bool
		Rooms   map[string][]VoicePeer
	}
	json.NewDecoder(res.Body).Decode(&list)
	if !list.Enabled || len(list.Rooms["talk"]) != 2 {
		t.Fatalf("Liste: %+v", list)
	}

	// Gäste nur zuhören: Rechte werden sofort angepasst
	upd := func(v VoiceConfig) {
		t.Helper()
		if _, err := h.UpdateRoomWith("talk", RoomInput{Links: LinksAll, BotLinks: "all", Voice: &v}); err != nil {
			t.Fatal(err)
		}
	}
	upd(VoiceConfig{Guests: VoiceGuestsListen})
	if you := readText(t, cb, "you"); you["speak"] != false {
		t.Fatalf("Gast spricht noch: %v", you)
	}

	// Bühne: Team spricht, andere nach Freischaltung
	users := srv.users
	users.SetTeam(peers[0].User, true)
	h.RefreshViewer(peers[0].User, true, nil)
	upd(VoiceConfig{Stage: true})
	readText(t, ca, "you")
	waitVoice(t, func() bool { return srv.voice.Peers("talk")[0].Speak })
	// Ben meldet sich
	cb.WriteText([]byte(`{"t":"state","hand":true}`))
	waitVoice(t, func() bool { return srv.voice.Peers("talk")[1].Hand })
	// Ben darf sich nicht selbst freischalten
	if res := post(t, ts.URL+"/api/voice/talk/mod", ben, `{"userId":"`+peers[1].User+`","action":"speak"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Selbst-Freischaltung: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/voice/talk/mod", anna, `{"userId":"`+peers[1].User+`","action":"speak"}`); res.StatusCode != 200 {
		t.Fatalf("Freischalten: %d", res.StatusCode)
	}
	waitVoice(t, func() bool { p := srv.voice.Peers("talk")[1]; return p.Speak && !p.Hand })
	// Rechte bleiben beim Abgleich erhalten (Freischaltung gespeichert)
	upd(VoiceConfig{Stage: true, Preset: "hoch"})
	time.Sleep(100 * time.Millisecond)
	if !srv.voice.Peers("talk")[1].Speak {
		t.Fatal("Freischaltung verloren")
	}
	// Stummschalten durch das Team
	post(t, ts.URL+"/api/voice/talk/mod", anna, `{"userId":"`+peers[1].User+`","action":"mute"}`)
	waitVoice(t, func() bool { return srv.voice.Peers("talk")[1].Muted })

	// Entfernen: Wiedereintritt erst nach einer Pause
	post(t, ts.URL+"/api/voice/talk/mod", anna, `{"userId":"`+peers[1].User+`","action":"kick"}`)
	readText(t, cb, "bye")
	waitPeers(t, srv.voice, "talk", 1)
	if _, code := voiceJoin(t, ts.URL, ben, "talk"); code != http.StatusForbidden {
		t.Fatalf("Wiedereintritt nach Entfernen: %d", code)
	}

	// Sperre im Chat beendet die Sprachverbindung
	h.Mod().Sanction(peers[0].User, "Anna", "ban", 10, "", "test", false)
	h.NotifySanction(peers[0].User)
	readText(t, ca, "bye")
	waitPeers(t, srv.voice, "talk", 0)

	// Protokoll
	found := false
	for _, e := range h.AuditLog().all() {
		if e.Action == "voice.kick" {
			found = true
		}
	}
	if !found {
		t.Fatal("voice.kick fehlt im Protokoll")
	}
}

func waitVoice(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Bedingung nicht erfüllt")
}

func TestVoiceGuestsNoneAndDelete(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Guests: VoiceGuestsNone}})
	tok := claim(t, ts.URL, "Gast")
	if _, code := voiceJoin(t, ts.URL, tok, "talk"); code != http.StatusForbidden {
		t.Fatalf("Gast ohne Zutritt: %d", code)
	}
	// mit Rolle kein Gast mehr
	roles, err := srv.hub.Roles().Set([]Role{{ID: "vip", Name: "VIP", Color: "#336699"}})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := srv.users.List("", 10)
	srv.users.SetRoles(list[0].ID, []string{roles[0].ID})
	j, code := voiceJoin(t, ts.URL, tok, "talk")
	if code != 200 {
		t.Fatalf("mit Rolle: %d %s", code, j.Error)
	}
	c, _ := voiceDial(t, ts.URL, j)
	waitPeers(t, srv.voice, "talk", 1)
	srv.hub.DeleteRoom("talk")
	readText(t, c, "bye")
	waitPeers(t, srv.voice, "talk", 0)
}

func TestVoiceAgents(t *testing.T) {
	ts, srv := voiceServer(t, func(c *Config) { c.VoiceNoBuiltin = true; c.VoiceAgentToken = "geheim-agent-token" })
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{}})
	tok := claim(t, ts.URL, "Anna")
	if _, code := voiceJoin(t, ts.URL, tok, "talk"); code != http.StatusServiceUnavailable {
		t.Fatalf("ohne Relay: %d", code)
	}
	report := func(id string, participants int, token string) int {
		b, _ := json.Marshal(agentReport{ID: id, WSURL: "wss://" + id + ".example/ws", ControlURL: "http://" + id + ":8090/control",
			Stats: voicerelay.Stats{Participants: participants, Max: 200, MaxMbit: 50}, Rooms: map[string][]VoicePeer{}})
		res := post(t, ts.URL+"/api/voice/agents/report", token, string(b))
		res.Body.Close()
		return res.StatusCode
	}
	if report("a", 0, "falsch") != http.StatusUnauthorized {
		t.Fatal("falscher Schlüssel angenommen")
	}
	report("a", 150, "geheim-agent-token")
	report("b", 10, "geheim-agent-token")
	j, code := voiceJoin(t, ts.URL, tok, "talk")
	if code != 200 || j.URL != "wss://b.example/ws" {
		t.Fatalf("Agent-Wahl: %d %q", code, j.URL)
	}
	// Ticket ist mit dem Sprach-Schlüssel signiert
	if _, err := voicerelay.VerifyTicket(srv.voice.pub, j.Ticket, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Ein Kanal bleibt auf seinem Relay
	b, _ := json.Marshal(agentReport{ID: "a", WSURL: "wss://a.example/ws", ControlURL: "http://a:8090/control",
		Stats: voicerelay.Stats{Participants: 151, Max: 200, MaxMbit: 50},
		Rooms: map[string][]VoicePeer{"talk": {{Idx: 1, User: "x", Name: "X"}}}})
	post(t, ts.URL+"/api/voice/agents/report", "geheim-agent-token", string(b)).Body.Close()
	j, _ = voiceJoin(t, ts.URL, tok, "talk")
	if j.URL != "wss://a.example/ws" {
		t.Fatalf("nicht beim bestehenden Relay: %q", j.URL)
	}
	// CSP enthält die Agenten
	res := do(t, "GET", ts.URL+"/", "", "")
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "wss://a.example") {
		t.Fatalf("CSP: %s", csp)
	}
	if pp := res.Header.Get("Permissions-Policy"); !strings.Contains(pp, "microphone=(self)") {
		t.Fatalf("Permissions-Policy: %s", pp)
	}
}

func TestValidVoice(t *testing.T) {
	h := NewHub(0)
	v, err := h.validVoice(&VoiceConfig{MaxPeers: 99999, MaxSpeakers: 99, StageRoles: []string{"gibtsnicht"}})
	if err != nil || v.MaxPeers != voiceMaxPeers || v.MaxSpeakers != voiceMaxSpeakers || v.Preset != "standard" || v.Guests != "speak" || v.StageRoles != nil {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := h.validVoice(&VoiceConfig{Preset: "laut"}); err == nil {
		t.Fatal("unbekannte Voreinstellung")
	}
	if _, err := h.validVoice(&VoiceConfig{Guests: "alle"}); err == nil {
		t.Fatal("unbekannte Gäste-Regel")
	}
}

func TestVoicePersist(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	name := "talk"
	if _, err := h.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Preset: "musik", Stage: true, Guests: VoiceGuestsListen}}); err != nil {
		t.Fatal(err)
	}
	key := h.VoiceKey()
	flush(t, h, u, s)
	h2, _, _ := openStore(t, dir)
	var got *VoiceConfig
	for _, r := range h2.Rooms() {
		if r.Name == "talk" {
			got = r.Voice
		}
	}
	if got == nil || got.Preset != "musik" || !got.Stage || got.Guests != VoiceGuestsListen || got.MaxPeers != 25 {
		t.Fatalf("Einstellungen: %+v", got)
	}
	if !h2.VoiceKey().Equal(key) {
		t.Fatal("Sprach-Schlüssel nicht gespeichert")
	}
}

func TestVoiceScreenRights(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	name := "talk"
	if _, err := h.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Screen: ScreenSpeakers, ScreenQuality: "text"}}); err != nil {
		t.Fatal(err)
	}
	anna, ben := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	var ja struct {
		joinResp
		Screen     bool
		ScreenKbps int
		ScreenFps  int
	}
	res := post(t, ts.URL+"/api/voice/talk/join", anna, `{}`)
	json.NewDecoder(res.Body).Decode(&ja)
	if !ja.Screen || ja.ScreenKbps != 1500 || ja.ScreenFps != 5 {
		t.Fatalf("Beitritt: %+v", ja)
	}
	ca, _ := voiceDial(t, ts.URL, ja.joinResp)
	jb, _ := voiceJoin(t, ts.URL, ben, "talk")
	voiceDial(t, ts.URL, jb)
	peers := waitPeers(t, srv.voice, "talk", 2)
	ca.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	if m := readText(t, ca, "share"); m["on"] != true {
		t.Fatalf("teilen: %v", m)
	}
	waitVoice(t, func() bool { return srv.voice.Peers("talk")[0].Screen == "vp8" })
	// Team-Mitglied beendet die Freigabe
	srv.users.SetTeam(peers[1].User, true)
	res = post(t, ts.URL+"/api/voice/talk/mod", ben, `{"userId":"`+peers[0].User+`","action":"stopscreen"}`)
	if res.StatusCode != 200 {
		t.Fatalf("stopscreen: %d", res.StatusCode)
	}
	waitVoice(t, func() bool { p := srv.voice.Peers("talk")[0]; return p.Screen == "" && !p.ScreenOK })
	// Kanal ohne Bildschirm: Recht weg
	post(t, ts.URL+"/api/voice/talk/mod", ben, `{"userId":"`+peers[0].User+`","action":"allowscreen"}`)
	waitVoice(t, func() bool { return srv.voice.Peers("talk")[0].ScreenOK })
	if _, err := h.UpdateRoomWith("talk", RoomInput{Links: LinksAll, BotLinks: "all", Voice: &VoiceConfig{}}); err != nil {
		t.Fatal(err)
	}
	waitVoice(t, func() bool { return !srv.voice.Peers("talk")[0].ScreenOK })
	if _, err := h.validVoice(&VoiceConfig{Screen: "alle"}); err == nil {
		t.Fatal("ungültiger Modus angenommen")
	}
}
