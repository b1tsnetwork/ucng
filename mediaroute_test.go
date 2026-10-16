// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ucng/internal/voicerelay"
)

type mediaResp struct {
	URL, Ticket, Kind, Relay string
	Offloaded                bool
}

// Bildschirm wird ausgelagert, wenn der Sprach-Relay sonst über die Schwelle käme; Kameras
// (andere Schwelle) bleiben. Wer die Sprache verlässt, verliert die Medienverbindungen.
func TestMediaOffload(t *testing.T) {
	var mu sync.Mutex
	var ctl []map[string]any
	agentCtl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &m)
		mu.Lock()
		ctl = append(ctl, m)
		mu.Unlock()
	}))
	defer agentCtl.Close()
	ts, srv := voiceServer(t, func(c *Config) {
		c.VoiceAgentToken = "geheim-agent-token"
		c.VoiceMaxMbit = 10
		c.VoiceOffloadScreen, c.VoiceOffloadVideo = 50, 75
	})
	name := "talk"
	if _, err := srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Screen: ScreenSpeakers, ScreenQuality: "high",
		Video: ScreenSpeakers, VideoQuality: "hd", Featured: 1}}); err != nil {
		t.Fatal(err)
	}
	report := func(role string, media map[string]voicerelay.MediaStat) {
		b, _ := json.Marshal(agentReport{ID: "m", WSURL: "wss://m.example/ws", ControlURL: agentCtl.URL + "/control", Role: role,
			Stats: voicerelay.Stats{Max: 200, MaxMbit: 50, Features: []string{"media"}, Media: media}, Rooms: map[string][]VoicePeer{}})
		post(t, ts.URL+"/api/voice/agents/report", "geheim-agent-token", string(b)).Body.Close()
	}
	report("media", nil)
	anna := claim(t, ts.URL, "Anna")
	media := func(kind string, publish bool) (mediaResp, int) {
		b, _ := json.Marshal(map[string]any{"kind": kind, "publish": publish})
		res := post(t, ts.URL+"/api/voice/talk/media", anna, string(b))
		defer res.Body.Close()
		var out mediaResp
		json.NewDecoder(res.Body).Decode(&out)
		return out, res.StatusCode
	}
	if _, code := media("screen", false); code != http.StatusConflict {
		t.Fatalf("ohne Sprache: %d", code)
	}
	j, code := voiceJoin(t, ts.URL, anna, "talk")
	if code != 200 || j.URL != "/api/voice/ws" { // Sprache: reine Medien-Relays bekommen keine
		t.Fatalf("Beitritt: %d %q", code, j.URL)
	}
	c, _ := voiceDial(t, ts.URL, j)
	waitPeers(t, srv.voice, "talk", 1)

	sc, code := media("screen", true)
	if code != 200 || sc.Relay != "m" || !sc.Offloaded || sc.URL != "wss://m.example/ws" {
		t.Fatalf("Bildschirm: %d %+v", code, sc)
	}
	tk, err := voicerelay.VerifyTicket(srv.voice.pub, sc.Ticket, time.Now())
	if err != nil || tk.Kind != voicerelay.KindScreen || !tk.Screen || tk.Speak || tk.ScreenKbps != 12000 {
		t.Fatalf("Ticket: %+v %v", tk, err)
	}
	vd, _ := media("video", false)
	if vd.Relay != builtinRelay || vd.Offloaded || vd.URL != "/api/voice/ws" {
		t.Fatalf("Kameras: %+v", vd)
	}
	// die Route bleibt, solange jemand sendet
	report("media", map[string]voicerelay.MediaStat{voicerelay.MediaKey("talk", "screen"): {Peers: 1, Pub: 1, Mbit: 12}})
	if again, _ := media("screen", true); again.Relay != "m" {
		t.Fatalf("Route gewechselt: %+v", again)
	}
	// Sprache verlassen → Medienverbindung auf dem Agenten beenden
	c.Close()
	waitPeers(t, srv.voice, "talk", 0)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, m := range ctl {
			found = found || m["action"] == "mediakick" && m["channel"] == "talk"
		}
		mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kein mediakick: %v", ctl)
		}
		time.Sleep(20 * time.Millisecond)
	}
	srv.voice.mu.Lock()
	left := srv.voice.rooms["talk"] != nil && srv.voice.rooms["talk"].media != nil
	srv.voice.mu.Unlock()
	if left {
		t.Fatal("Route nach leerem Kanal noch da")
	}
}

// Ohne andere Relays bleibt alles beim Sprach-Relay – auch über der Schwelle.
func TestMediaStaysHome(t *testing.T) {
	ts, srv := voiceServer(t, func(c *Config) { c.VoiceMaxMbit = 10; c.VoiceOffloadScreen = 50 })
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Screen: ScreenSpeakers, ScreenQuality: "max"}})
	anna := claim(t, ts.URL, "Anna")
	j, _ := voiceJoin(t, ts.URL, anna, "talk")
	voiceDial(t, ts.URL, j)
	waitPeers(t, srv.voice, "talk", 1)
	res := post(t, ts.URL+"/api/voice/talk/media", anna, `{"kind":"screen","publish":true}`)
	var out mediaResp
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if res.StatusCode != 200 || out.Relay != builtinRelay || out.Offloaded {
		t.Fatalf("%d %+v", res.StatusCode, out)
	}
	if res := post(t, ts.URL+"/api/voice/talk/media", anna, `{"kind":"video"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("Kameras aus: %d", res.StatusCode)
	}
}
