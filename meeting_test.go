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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ucng/internal/stt"
	"ucng/internal/voicerelay"
	"ucng/internal/wsock"
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
// Untertitel und Live-Transkription: Der Relay schneidet mit (Ogg-Opus), der Chat-Server
// verschriftlicht über das eigene Whisper – mit dem Sprecher aus der Verbindung.
func TestVoiceCaptions(t *testing.T) {
	var mu sync.Mutex
	var gotModel, gotLang, gotType string
	var gotOgg bool
	reply := "  Hallo   zusammen "
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		mu.Lock()
		defer mu.Unlock()
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
			case "file":
				gotType, gotOgg = p.Header.Get("Content-Type"), len(b) > 4 && string(b[:4]) == "OggS"
			}
		}
		w.Write([]byte(`{"text":` + strconv.Quote(reply) + `}`))
	}))
	defer whisper.Close()
	ts, srv := voiceServer(t, func(c *Config) { c.STTURL = whisper.URL; c.STTModel = "large-v3"; c.DataDir = t.TempDir() })
	a, b := "talk", "geheim"
	srv.hub.CreateRoomWith(RoomInput{Name: &a, Voice: &VoiceConfig{Captions: true}})
	srv.hub.CreateRoomWith(RoomInput{Name: &b, Voice: &VoiceConfig{Captions: true, E2EE: true}})
	if ri, _ := srv.hub.roomInfo("geheim"); ri.Voice.Captions || ri.Voice.BotMusic {
		t.Fatal("verschlüsselt: Untertitel/Musik nicht abgeschaltet")
	}
	ana, bo := claim(t, ts.URL, "Ana"), claim(t, ts.URL, "Bo")
	srv.users.SetTeam(userID(t, srv, "Ana"), true)
	join := func(tok string) joinResp {
		res := post(t, ts.URL+"/api/voice/talk/join?lang=de", tok, `{}`)
		defer res.Body.Close()
		var out joinResp
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	ca, _ := voiceDial(t, ts.URL, join(ana))
	cb, _ := voiceDial(t, ts.URL, join(bo))
	waitPeers(t, srv.voice, "talk", 2)
	speak := func(c *wsock.Client, n int) { // n Rahmen à 20 ms „Sprache“ (CELT-TOC, Pegel hoch)
		for i := 0; i < n; i++ {
			c.WriteBinary(append([]byte{1, 0, byte(i), 120, 0xF8}, make([]byte, 30)...))
			time.Sleep(20 * time.Millisecond)
		}
	}
	speak(ca, 30)
	if m := readText(t, cb, "cap"); m["x"] != "Hallo zusammen" || m["u"] != userID(t, srv, "Ana") {
		t.Fatalf("Untertitel: %v", m)
	}
	mu.Lock()
	if gotModel != "large-v3" || gotLang != "de" || gotType != "audio/ogg" || !gotOgg {
		t.Fatalf("an Whisper: model=%q lang=%q type=%q ogg=%v", gotModel, gotLang, gotType, gotOgg)
	}
	mu.Unlock()

	// Live-Transkription: nur Moderierende starten; Bo will keine Untertitel – im Transkript zählt er trotzdem
	if res := post(t, ts.URL+"/api/voice/talk/transcript", bo, `{"on":true}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Bo darf nicht starten: %d", res.StatusCode)
	}
	res := post(t, ts.URL+"/api/voice/talk/transcript", ana, `{"on":true}`)
	var info TranscriptInfo
	json.NewDecoder(res.Body).Decode(&info)
	if res.StatusCode != 200 || !info.Active || info.ID == "" {
		t.Fatalf("Start: %d %+v", res.StatusCode, info)
	}
	if res := post(t, ts.URL+"/api/voice/talk/transcript", ana, `{"on":true}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("doppelt gestartet: %d", res.StatusCode)
	}
	cb.WriteText([]byte(`{"t":"state","nc":true}`))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	reply = "Ich bin Bo"
	mu.Unlock()
	speak(cb, 30)
	waitCap(t, ca, "Ich bin Bo")
	mu.Lock()
	reply = "Und ich Ana"
	mu.Unlock()
	speak(ca, 30)
	waitCap(t, cb, "Und ich Ana")
	var room struct {
		Active *TranscriptInfo
		Lines  []TranscriptLine
		Items  []TranscriptInfo
	}
	getJSON(t, ts.URL+"/api/rooms/talk/transcripts", bo, &room)
	if room.Active == nil || len(room.Lines) != 2 || room.Lines[0].Name != "Bo" || room.Lines[0].Text != "Ich bin Bo" || room.Lines[1].Name != "Ana" {
		t.Fatalf("laufendes Transkript: %+v %+v", room.Active, room.Lines)
	}
	if res := post(t, ts.URL+"/api/voice/talk/transcript", ana, `{"on":false}`); res.StatusCode != 200 {
		t.Fatalf("Ende: %d", res.StatusCode)
	}
	res = do(t, "GET", ts.URL+"/api/transcripts/"+info.ID, bo, "")
	txt, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(txt), "] Bo: Ich bin Bo") || !strings.Contains(string(txt), "] Ana: Und ich Ana") {
		t.Fatalf("Download: %d\n%s", res.StatusCode, txt)
	}
	// Hinweise im Kanal (Start und Ende)
	if msgs, _, _ := srv.hub.History("talk", 0, 10); len(msgs) < 2 || !strings.Contains(msgs[len(msgs)-1].Text, "beendet") {
		t.Fatalf("Hinweise: %v", msgs)
	}
	// Admin: Liste, Löschen; danach weg
	adm := adminLogin(t, ts.URL)
	var all struct{ Items []TranscriptInfo }
	getJSON(t, ts.URL+"/api/admin/transcripts", adm, &all)
	if len(all.Items) != 1 || all.Items[0].Lines != 2 {
		t.Fatalf("Admin-Liste: %+v", all.Items)
	}
	if res := do(t, "DELETE", ts.URL+"/api/admin/transcripts/"+info.ID, adm, ""); res.StatusCode != 200 {
		t.Fatalf("Löschen: %d", res.StatusCode)
	}
	if res := do(t, "GET", ts.URL+"/api/transcripts/"+info.ID, bo, ""); res.StatusCode != 404 {
		t.Fatalf("nach dem Löschen: %d", res.StatusCode)
	}
	// verschlüsselter Kanal: keine Transkription
	if res := post(t, ts.URL+"/api/voice/geheim/transcript", ana, `{"on":true}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("E2EE: %d", res.StatusCode)
	}
	if cleanCaption("Untertitel im Auftrag des ZDF, 2021") != "" {
		t.Fatal("Whisper-Halluzination nicht gefiltert")
	}
}

func waitCap(t *testing.T, c *wsock.Client, text string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if m := readText(t, c, "cap"); m["x"] == text {
			return
		}
	}
	t.Fatalf("Untertitel %q kam nicht", text)
}

func getJSON(t *testing.T, url, tok string, out any) {
	t.Helper()
	res := do(t, "GET", url, tok, "")
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, b)
	}
	json.NewDecoder(res.Body).Decode(out)
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

// Mehrere Kanäle gleichzeitig, Abschnitte von einem Agenten, automatisches Ende im leeren Kanal.
func TestTranscriptParallel(t *testing.T) {
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		f, _, _ := r.FormFile("file")
		b, _ := io.ReadAll(f)
		w.Write([]byte(`{"text":` + strconv.Quote("Text "+strconv.Itoa(len(b))) + `}`))
	}))
	defer whisper.Close()
	ts, srv := voiceServer(t, func(c *Config) { c.STTURL = whisper.URL; c.VoiceAgentToken = "agent-geheim-123" })
	for _, n := range []string{"eins", "zwei"} {
		name := n
		srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{Captions: true}})
	}
	mia := claim(t, ts.URL, "Mia")
	srv.users.SetTeam(userID(t, srv, "Mia"), true)
	ids := map[string]string{}
	for _, room := range []string{"eins", "zwei"} {
		res := post(t, ts.URL+"/api/voice/"+room+"/transcript", mia, `{"on":true,"lang":"de"}`)
		var info TranscriptInfo
		json.NewDecoder(res.Body).Decode(&info)
		ids[room] = info.ID
	}
	ogg := string(voicerelay.OggOpus([][]byte{{0xF8, 1, 2, 3}}, false))
	send := func(room, tok string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/api/voice/agents/segment?channel="+room+"&user=u1&name=Lu&mode=2&dur=600&start="+
			strconv.FormatInt(time.Now().UnixMilli(), 10), strings.NewReader(ogg))
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if send("eins", "falsch") != http.StatusUnauthorized {
		t.Fatal("Agent ohne Schlüssel angenommen")
	}
	for _, room := range []string{"eins", "zwei", "zwei"} {
		if code := send(room, "agent-geheim-123"); code != http.StatusAccepted {
			t.Fatalf("Abschnitt: %d", code)
		}
	}
	waitFor(t, "Zeilen in beiden Transkripten", func() bool {
		a, _ := srv.voice.tr.get(ids["eins"])
		b, _ := srv.voice.tr.get(ids["zwei"])
		return a.Lines == 1 && b.Lines == 2
	})
	if l := srv.voice.tr.read(ids["zwei"]); l[0].Name != "Lu" || !strings.HasPrefix(l[0].Text, "Text ") {
		t.Fatalf("Zeile: %+v", l)
	}
	// niemand im Sprachkanal: nach zwei Minuten beendet
	now := time.Now()
	srv.voice.transcriptTick(now)
	srv.voice.transcriptTick(now.Add(3 * time.Minute))
	if srv.voice.tr.activeFor("eins") != nil || srv.voice.tr.activeFor("zwei") != nil {
		t.Fatal("leere Kanäle transkribieren weiter")
	}
}

// Temporäre Kanäle: Der Besitzer darf transkribieren, auch ohne „Untertitel“ beim Kanal.
func TestTranscriptTempRoom(t *testing.T) {
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"text":"ok"}`)) }))
	defer whisper.Close()
	ts, srv := voiceServer(t, func(c *Config) { c.STTURL = whisper.URL })
	owner, other := claim(t, ts.URL, "Olga"), claim(t, ts.URL, "Paul")
	info, err := srv.hub.CreateTempRoom(TempRoomInput{Name: "treffen", Owner: userID(t, srv, "Olga"), OwnerName: "Olga", By: "command",
		Voice: &VoiceConfig{}, VoiceSet: true})
	if err != nil {
		t.Fatal(err)
	}
	var room struct{ Allowed, CanStart bool }
	getJSON(t, ts.URL+"/api/rooms/"+info.Name+"/transcripts", other, &room)
	if !room.Allowed || room.CanStart {
		t.Fatalf("Paul: %+v", room)
	}
	if res := post(t, ts.URL+"/api/voice/"+info.Name+"/transcript", other, `{"on":true}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Paul startet: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/voice/"+info.Name+"/transcript", owner, `{"on":true}`); res.StatusCode != 200 {
		t.Fatalf("Besitzerin startet: %d", res.StatusCode)
	}
	if srv.voice.tr.activeFor(info.Name) == nil {
		t.Fatal("läuft nicht")
	}
}

// Sprach-Server mit eigener Spracherkennung: Der Chat-Server braucht kein STT_URL; der Agent
// schickt nur Text. Die Übersicht zeigt die Funktionen je Server.
func TestAgentOwnSTT(t *testing.T) {
	ts, srv := voiceServer(t, func(c *Config) { c.VoiceAgentToken = "agent-geheim-123"; c.VoiceNoBuiltin = true })
	room := "runde"
	srv.hub.CreateRoomWith(RoomInput{Name: &room, Voice: &VoiceConfig{Captions: true}})
	adm := adminLogin(t, ts.URL)
	var vs struct{ STT bool }
	getJSON(t, ts.URL+"/api/admin/voice", adm, &vs)
	if vs.STT {
		t.Fatal("ohne Spracherkennung als verfügbar gemeldet")
	}
	b, _ := json.Marshal(agentReport{ID: "w1", WSURL: "wss://w1.example/ws", ControlURL: "http://127.0.0.1:1/control",
		Stats: voicerelay.Stats{Max: 200, MaxMbit: 50, Features: []string{"media", "tap", "stt"}}, Rooms: map[string][]VoicePeer{},
		STT: &stt.Stats{Host: "gpu1:8000", Model: "large-v3", Concurrency: 2, Done: 7}})
	post(t, ts.URL+"/api/voice/agents/report", "agent-geheim-123", string(b)).Body.Close()
	var full struct {
		STT    bool
		Relays []struct {
			ID        string
			Functions []string
			STT       *stt.Stats
		}
	}
	getJSON(t, ts.URL+"/api/admin/voice", adm, &full)
	if !full.STT || len(full.Relays) != 1 || strings.Join(full.Relays[0].Functions, ",") != "voice,media,tap,stt" || full.Relays[0].STT.Model != "large-v3" {
		t.Fatalf("Übersicht: %+v", full)
	}
	mia := claim(t, ts.URL, "Mia")
	srv.users.SetTeam(userID(t, srv, "Mia"), true)
	res := post(t, ts.URL+"/api/voice/runde/transcript", mia, `{"on":true}`)
	var info TranscriptInfo
	json.NewDecoder(res.Body).Decode(&info)
	if res.StatusCode != 200 {
		t.Fatalf("Start: %d", res.StatusCode)
	}
	text := func(tok string) int {
		b, _ := json.Marshal(map[string]any{"channel": "runde", "user": "u1", "name": "Lu", "mode": 2,
			"start": time.Now().UnixMilli(), "dur": 900, "text": "  Guten   Abend "})
		r := post(t, ts.URL+"/api/voice/agents/text", tok, string(b))
		r.Body.Close()
		return r.StatusCode
	}
	if text("falsch") != http.StatusUnauthorized || text("agent-geheim-123") != http.StatusAccepted {
		t.Fatal("Text vom Agenten")
	}
	if l := srv.voice.tr.read(info.ID); len(l) != 1 || l[0].Text != "Guten Abend" || l[0].Name != "Lu" {
		t.Fatalf("Transkript: %+v", l)
	}
}
