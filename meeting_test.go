// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func userID(t *testing.T, srv *Server, name string) string {
	t.Helper()
	list, _ := srv.users.List("", 50)
	for _, u := range list {
		if u.Name == name {
			return u.ID
		}
	}
	t.Fatalf("keine Person %q", name)
	return ""
}

// Werkzeuge der Leitung: alle stumm, Kameras aus, Aufnahme-Hinweis, Co-Host, beenden.
func TestVoiceLeaderTools(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{}})
	mia, ben, cleo := claim(t, ts.URL, "Mia"), claim(t, ts.URL, "Ben"), claim(t, ts.URL, "Cleo")
	srv.users.SetTeam(userID(t, srv, "Mia"), true)
	benID := userID(t, srv, "Ben")
	jm, _ := voiceJoin(t, ts.URL, mia, "talk")
	cm, _ := voiceDial(t, ts.URL, jm)
	jb, _ := voiceJoin(t, ts.URL, ben, "talk")
	cb, _ := voiceDial(t, ts.URL, jb)
	jc, _ := voiceJoin(t, ts.URL, cleo, "talk")
	cc, _ := voiceDial(t, ts.URL, jc)
	waitPeers(t, srv.voice, "talk", 3)

	// ohne Recht: nichts
	if res := post(t, ts.URL+"/api/voice/talk/mod", ben, `{"action":"muteall"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Ben: alle stumm: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/voice/talk/mod", mia, `{"action":"muteall"}`); res.StatusCode != 200 {
		t.Fatalf("alle stumm: %d", res.StatusCode)
	}
	readText(t, cb, "forcemute")
	readText(t, cc, "forcemute")
	post(t, ts.URL+"/api/voice/talk/mod", mia, `{"action":"askunmute","userId":"`+benID+`"}`).Body.Close()
	readText(t, cb, "askunmute")
	// Ben wird Co-Host – und darf dann selbst alle stummschalten, aber keine Co-Hosts ernennen
	post(t, ts.URL+"/api/voice/talk/mod", mia, `{"action":"cohost","userId":"`+benID+`"}`).Body.Close()
	isMod := false
	for i := 0; i < 3 && !isMod; i++ { // je ein „you“ für Bildschirm, Kamera, Moderation
		isMod = readText(t, cb, "you")["mod"] == true
	}
	if !isMod {
		t.Fatal("Co-Host: kein mod")
	}
	if res := post(t, ts.URL+"/api/voice/talk/mod", ben, `{"action":"camsoff"}`); res.StatusCode != 200 {
		t.Fatalf("Co-Host: Kameras aus: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/voice/talk/mod", ben, `{"action":"cohost","userId":"`+userID(t, srv, "Cleo")+`"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Co-Host ernennt Co-Host: %d", res.StatusCode)
	}
	// Aufnahme: alle sehen es
	post(t, ts.URL+"/api/voice/talk/mod", ben, `{"action":"recstart"}`).Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := false
		for _, p := range srv.voice.Peers("talk") {
			rec = rec || p.User == benID && p.Rec
		}
		if rec {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Aufnahme-Hinweis fehlt")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// für alle beenden
	post(t, ts.URL+"/api/voice/talk/mod", mia, `{"action":"endall"}`).Body.Close()
	if m := readText(t, cc, "bye"); m["reason"] != "ended" {
		t.Fatalf("beenden: %v", m)
	}
	_ = cm
	waitPeers(t, srv.voice, "talk", 0)
	// leer: Co-Host gilt nicht mehr
	srv.voice.mu.Lock()
	co := srv.voice.rooms["talk"] != nil && srv.voice.rooms["talk"].cohosts[benID]
	srv.voice.mu.Unlock()
	if co {
		t.Fatal("Co-Host nach leerem Kanal")
	}
}

// Breakout-Räume: aufteilen, Countdown, zurückholen.
func TestVoiceBreakout(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Stage: true}})
	mia := claim(t, ts.URL, "Mia")
	srv.users.SetTeam(userID(t, srv, "Mia"), true)
	jm, _ := voiceJoin(t, ts.URL, mia, "talk")
	voiceDial(t, ts.URL, jm)
	var conns = map[string]map[string]any{}
	clients := map[string]chan string{}
	for _, n := range []string{"Ana", "Bo", "Cy", "Di"} {
		tok := claim(t, ts.URL, n)
		j, _ := voiceJoin(t, ts.URL, tok, "talk")
		c, _ := voiceDial(t, ts.URL, j)
		ch := make(chan string, 1)
		clients[n] = ch
		go func() {
			for {
				op, p, err := c.Read(5 * time.Second)
				if err != nil {
					return
				}
				if op == 1 {
					var m map[string]any
					json.Unmarshal(p, &m)
					if m["t"] == "move" {
						ch <- m["room"].(string)
					}
				}
			}
		}()
	}
	_ = conns
	waitPeers(t, srv.voice, "talk", 5)
	ben := claim(t, ts.URL, "Ben")
	if res := post(t, ts.URL+"/api/voice/talk/breakout", ben, `{"groups":2}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("ohne Recht: %d", res.StatusCode)
	}
	res := post(t, ts.URL+"/api/voice/talk/breakout", mia, `{"groups":2,"minutes":1}`)
	var b BreakoutInfo
	json.NewDecoder(res.Body).Decode(&b)
	res.Body.Close()
	if res.StatusCode != 200 || len(b.Rooms) != 2 || b.Ends == 0 {
		t.Fatalf("starten: %d %+v", res.StatusCode, b)
	}
	count := map[string]int{}
	for n, ch := range clients {
		select {
		case r := <-ch:
			count[r]++
		case <-time.After(2 * time.Second):
			t.Fatalf("%s nicht verschoben", n)
		}
	}
	if count[b.Rooms[0]] != 2 || count[b.Rooms[1]] != 2 {
		t.Fatalf("ungleich aufgeteilt: %v", count)
	}
	// Gruppen: temporär, ohne Bühne, gehören zur Runde
	ri, _ := srv.hub.roomInfo(b.Rooms[0])
	if ri.Temp == nil || ri.Temp.Breakout != "talk" || ri.Voice == nil || ri.Voice.Stage {
		t.Fatalf("Gruppe: %+v", ri)
	}
	if res := post(t, ts.URL+"/api/voice/talk/breakout", mia, `{"groups":2}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("zweite Runde: %d", res.StatusCode)
	}
	if bi := srv.voice.breakoutFor(b.Rooms[1]); bi == nil || bi.Parent != "talk" {
		t.Fatalf("Runde für Gruppe: %+v", bi)
	}
	// Countdown abgelaufen → Runde endet
	srv.voice.breakoutTick(time.Now().Add(2 * time.Minute))
	if srv.voice.breakoutFor("talk") != nil {
		t.Fatal("Runde nach Ablauf noch da")
	}
	if res := do(t, "DELETE", ts.URL+"/api/voice/talk/breakout", mia, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("beenden ohne Runde: %d", res.StatusCode)
	}
}

// Untertitel: WAV → eigenes Whisper → Untertitel an alle; ohne Freigabe im Kanal kein Weg.
func TestVoiceCaptions(t *testing.T) {
	var gotModel, gotLang string
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "model":
				gotModel = string(b)
			case "language":
				gotLang = string(b)
			}
		}
		w.Write([]byte(`{"text":"  Hallo   zusammen "}`))
	}))
	defer whisper.Close()
	ts, srv := voiceServer(t, func(c *Config) { c.STTURL = whisper.URL; c.STTModel = "large-v3" })
	a, b := "talk", "geheim"
	srv.hub.CreateRoomWith(RoomInput{Name: &a, Voice: &VoiceConfig{Captions: true}})
	srv.hub.CreateRoomWith(RoomInput{Name: &b, Voice: &VoiceConfig{Captions: true, E2EE: true}})
	if ri, _ := srv.hub.roomInfo("geheim"); ri.Voice.Captions || ri.Voice.BotMusic {
		t.Fatal("verschlüsselt: Untertitel/Musik nicht abgeschaltet")
	}
	ana, bo := claim(t, ts.URL, "Ana"), claim(t, ts.URL, "Bo")
	ja, _ := voiceJoin(t, ts.URL, ana, "talk")
	voiceDial(t, ts.URL, ja)
	jb, _ := voiceJoin(t, ts.URL, bo, "talk")
	cb, _ := voiceDial(t, ts.URL, jb)
	waitPeers(t, srv.voice, "talk", 2)
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), make([]byte, 100)...)
	req, _ := http.NewRequest("POST", ts.URL+"/api/voice/talk/stt?lang=de", strings.NewReader(string(wav)))
	req.Header.Set("Authorization", "Bearer "+ana)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("stt: %v %d", err, res.StatusCode)
	}
	res.Body.Close()
	if m := readText(t, cb, "cap"); m["x"] != "Hallo zusammen" || m["u"] != userID(t, srv, "Ana") {
		t.Fatalf("Untertitel: %v", m)
	}
	if gotModel != "large-v3" || gotLang != "de" {
		t.Fatalf("an Whisper: model=%q lang=%q", gotModel, gotLang)
	}
	// kein WAV
	req, _ = http.NewRequest("POST", ts.URL+"/api/voice/talk/stt", strings.NewReader("hallo"))
	req.Header.Set("Authorization", "Bearer "+ana)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("kein WAV: %d", res.StatusCode)
	}
	// nicht im Kanal
	cleo := claim(t, ts.URL, "Cleo")
	req, _ = http.NewRequest("POST", ts.URL+"/api/voice/talk/stt", strings.NewReader(string(wav)))
	req.Header.Set("Authorization", "Bearer "+cleo)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusConflict {
		t.Fatalf("nicht im Kanal: %d", res.StatusCode)
	}
	if cleanCaption("Untertitel im Auftrag des ZDF, 2021") != "" {
		t.Fatal("Whisper-Halluzination nicht gefiltert")
	}
}

// Moderation je Sprachkanal: unabhängig vom Chat-Recht „Stummschalten“.
func TestVoiceModsSetting(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	roles, err := srv.hub.Roles().Set([]Role{{Name: "Staff", Color: "#ff0000", Perms: Perms{Mute: true}}, {Name: "Moderation", Color: "#00ff00"}})
	if err != nil {
		t.Fatal(err)
	}
	staff, modr := roles[0].ID, roles[1].ID
	for _, n := range []string{"standard", "nurteam", "rollen"} {
		name := n
		cfg := &VoiceConfig{}
		switch n {
		case "nurteam":
			cfg.Mods = ModsTeam
		case "rollen":
			cfg.Mods, cfg.ModRoles = ModsRoles, []string{modr}
		}
		if _, err := srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	claim(t, ts.URL, "Sam")
	claim(t, ts.URL, "Tea")
	claim(t, ts.URL, "Mo")
	sam, tea, mo := userID(t, srv, "Sam"), userID(t, srv, "Tea"), userID(t, srv, "Mo")
	srv.users.SetRoles(sam, []string{staff}) // Chat-Recht „Stummschalten“
	srv.users.SetTeam(tea, true)
	srv.users.SetRoles(mo, []string{modr})
	want := map[string][3]bool{ // standard, nurteam, rollen
		"Sam": {true, false, false},
		"Tea": {true, true, false},
		"Mo":  {false, false, true},
	}
	for name, exp := range want {
		w, _ := srv.voice.whoByID(userID(t, srv, name))
		for i, room := range []string{"standard", "nurteam", "rollen"} {
			d, err := srv.voice.decide(room, w)
			if err != nil || d.Mod != exp[i] {
				t.Errorf("%s in #%s: moderiert=%v, erwartet %v (%v)", name, room, d.Mod, exp[i], err)
			}
		}
	}
	if _, err := srv.hub.validVoice(&VoiceConfig{Mods: "alle"}); err == nil {
		t.Fatal("unbekannte Moderation angenommen")
	}
}

// Breakout mit Moderierenden: werden mit aufgeteilt (außer der aufteilenden Person).
func TestVoiceBreakoutWithMods(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{}})
	toks := map[string]string{}
	for _, n := range []string{"Mia", "Ana", "Bo", "Cy"} {
		toks[n] = claim(t, ts.URL, n)
		srv.users.SetTeam(userID(t, srv, n), true) // alle moderieren
		j, _ := voiceJoin(t, ts.URL, toks[n], "talk")
		voiceDial(t, ts.URL, j)
	}
	waitPeers(t, srv.voice, "talk", 4)
	res := post(t, ts.URL+"/api/voice/talk/breakout", toks["Mia"], `{"groups":2}`)
	if res.StatusCode != http.StatusBadRequest { // ohne Häkchen: niemand zum Aufteilen
		t.Fatalf("ohne Moderierende: %d", res.StatusCode)
	}
	res = post(t, ts.URL+"/api/voice/talk/breakout", toks["Mia"], `{"groups":2,"withMods":true}`)
	var b BreakoutInfo
	json.NewDecoder(res.Body).Decode(&b)
	if res.StatusCode != 200 || len(b.Rooms) != 2 {
		t.Fatalf("mit Moderierenden: %d %+v", res.StatusCode, b)
	}
}
