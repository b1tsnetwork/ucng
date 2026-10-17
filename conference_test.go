// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// oggOpus baut eine kleine Ogg-Opus-Datei (n Pakete à 20 ms, Inhalt egal).
func oggOpus(n int) []byte {
	var out bytes.Buffer
	seq := uint32(0)
	page := func(flags byte, packets ...[]byte) {
		var lace []byte
		var body []byte
		for _, p := range packets {
			l := len(p)
			for l >= 255 {
				lace = append(lace, 255)
				l -= 255
			}
			lace = append(lace, byte(l))
			body = append(body, p...)
		}
		h := make([]byte, 27)
		copy(h, "OggS")
		h[5] = flags
		binary.LittleEndian.PutUint32(h[14:], 4711)
		binary.LittleEndian.PutUint32(h[18:], seq)
		h[26] = byte(len(lace))
		seq++
		out.Write(h)
		out.Write(lace)
		out.Write(body)
	}
	head := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xBB, 0, 0, 0, 0, 0)
	page(2, head)
	page(0, []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"))
	for i := 0; i < n; i += 10 {
		var pk [][]byte
		for j := i; j < i+10 && j < n; j++ {
			pk = append(pk, append([]byte{0xF8}, make([]byte, 200)...)) // CELT, 20 ms
		}
		page(0, pk...)
	}
	return out.Bytes()
}

func TestParseOggOpus(t *testing.T) {
	tr, err := parseOggOpus(oggOpus(50))
	if err != nil || len(tr.packets) != 50 || tr.total != time.Second || tr.channels != 2 {
		t.Fatalf("%v %+v", err, tr)
	}
	if tr.maxRate < 70000 || tr.maxRate > 90000 { // 201 Byte je 20 ms ≈ 80 kbit/s
		t.Fatalf("Bitrate %d", tr.maxRate)
	}
	if _, err := parseOggOpus([]byte("kein ogg")); err == nil {
		t.Fatal("Unsinn angenommen")
	}
	if d := opusDuration([]byte{0xFB, 3}); d != 60*time.Millisecond { // Code 3, 3 Rahmen à 20 ms
		t.Fatalf("Dauer %v", d)
	}
}

func TestBotMusic(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	name, other := "talk", "still"
	h.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{BotMusic: true}})
	h.CreateRoomWith(RoomInput{Name: &other, Voice: &VoiceConfig{}})
	_, tok, _ := h.Bots().Create(BotInput{Name: "DJ", AllRooms: true, Perms: &BotPerms{Write: true, Music: true}})
	_, weak, _ := h.Bots().Create(BotInput{Name: "Leise", AllRooms: true})
	file := oggOpus(500) // 10 s
	play := func(tok, room, q string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+"/api/bot/voice/"+room+"/play"+q, bytes.NewReader(file))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "audio/ogg")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := play(weak, "talk", ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("ohne Recht: %d", res.StatusCode)
	}
	if res := play(tok, "still", ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Kanal ohne Musik: %d", res.StatusCode)
	}
	if res := play(tok, "talk", "?volume=15&title=Lounge"); res.StatusCode != http.StatusAccepted {
		t.Fatalf("abspielen: %d", res.StatusCode)
	}
	waitVoice(t, func() bool {
		for _, p := range srv.voice.Peers("talk") {
			if p.Music == 15 && strings.HasPrefix(p.User, "bot:") && p.Name == "DJ" {
				return true
			}
		}
		return false
	})
	// Musik hält einen temporären Kanal nicht am Leben
	if srv.voice.busyRooms()["talk"] {
		t.Fatal("Musik zählt als Person")
	}
	res := do(t, "GET", ts.URL+"/api/bot/voice/talk", tok, "")
	var st struct {
		Playing bool
		Track   struct {
			Title  string
			Length float64 `json:"lengthSeconds"`
		}
	}
	json.NewDecoder(res.Body).Decode(&st)
	if !st.Playing || st.Track.Title != "Lounge" || st.Track.Length != 10 {
		t.Fatalf("Status: %+v", st)
	}
	post(t, ts.URL+"/api/bot/voice/talk/stop", tok, "")
	waitVoice(t, func() bool { return len(srv.voice.Peers("talk")) == 0 })
}

func TestRoomPassword(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	name := "geheim"
	h.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{}})
	if err := h.SetRoomPassword("geheim", "abc", ""); err == nil {
		t.Fatal("zu kurzes Passwort angenommen")
	}
	h.SetRoomPassword("geheim", "sesam123", "")
	anna := claim(t, ts.URL, "Anna")
	// sichtbar, aber gesperrt
	res := do(t, "GET", ts.URL+"/api/rooms", anna, "")
	var rooms []RoomInfo
	json.NewDecoder(res.Body).Decode(&rooms)
	if len(rooms) != 1 || !rooms[0].Locked || !rooms[0].HasPassword {
		t.Fatalf("Liste: %+v", rooms)
	}
	if res := do(t, "GET", ts.URL+"/api/rooms/geheim/messages", anna, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("Verlauf ohne Passwort: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/rooms/geheim/messages", anna, `{"text":"hallo"}`); res.StatusCode == 201 {
		t.Fatal("schreiben ohne Passwort")
	}
	if _, code := voiceJoin(t, ts.URL, anna, "geheim"); code != http.StatusForbidden {
		t.Fatalf("Sprache ohne Passwort: %d", code)
	}
	if res := post(t, ts.URL+"/api/rooms/geheim/unlock", anna, `{"password":"falsch"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("falsches Passwort: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/rooms/geheim/unlock", anna, `{"password":"sesam123"}`); res.StatusCode != 200 {
		t.Fatalf("entsperren: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/rooms/geheim/messages", anna, `{"text":"hallo"}`); res.StatusCode != 201 {
		t.Fatalf("schreiben nach dem Entsperren: %d", res.StatusCode)
	}
	if _, code := voiceJoin(t, ts.URL, anna, "geheim"); code != 200 {
		t.Fatalf("Sprache nach dem Entsperren: %d", code)
	}
	// neues Passwort: wieder gesperrt
	h.SetRoomPassword("geheim", "neues-wort", "")
	if res := do(t, "GET", ts.URL+"/api/rooms/geheim/messages", anna, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("nach Passwortwechsel: %d", res.StatusCode)
	}
	// zu viele Versuche
	for i := 0; i < 10; i++ {
		post(t, ts.URL+"/api/rooms/geheim/unlock", anna, `{"password":"x"}`)
	}
	if res := post(t, ts.URL+"/api/rooms/geheim/unlock", anna, `{"password":"neues-wort"}`); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Bremse: %d", res.StatusCode)
	}
}

func TestVoiceMove(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	a, b := "lobby", "team"
	h.CreateRoomWith(RoomInput{Name: &a, Voice: &VoiceConfig{}})
	h.CreateRoomWith(RoomInput{Name: &b, Voice: &VoiceConfig{}, ReadRoles: []string{RoleTeamOnly}})
	mod, ben := claim(t, ts.URL, "Mia"), claim(t, ts.URL, "Ben")
	list, _ := srv.users.List("", 10)
	var modID, benID string
	for _, u := range list {
		if u.Name == "Mia" {
			modID = u.ID
		} else {
			benID = u.ID
		}
	}
	srv.users.SetTeam(modID, true)
	jb, _ := voiceJoin(t, ts.URL, ben, "lobby")
	cb, _ := voiceDial(t, ts.URL, jb)
	waitPeers(t, srv.voice, "lobby", 1)
	// Ben darf #team nicht
	if _, code := voiceJoin(t, ts.URL, ben, "team"); code != http.StatusNotFound {
		t.Fatalf("ohne Zugang: %d", code)
	}
	if res := post(t, ts.URL+"/api/voice/lobby/move", ben, `{"userId":"`+modID+`","to":"team"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Ben verschiebt: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/voice/lobby/move", mod, `{"userId":"`+benID+`","to":"team"}`); res.StatusCode != 200 {
		t.Fatalf("verschieben: %d", res.StatusCode)
	}
	if m := readText(t, cb, "move"); m["room"] != "team" {
		t.Fatalf("move: %v", m)
	}
	// jetzt darf Ben hinein
	jt, code := voiceJoin(t, ts.URL, ben, "team")
	if code != 200 {
		t.Fatalf("nach dem Verschieben: %d", code)
	}
	voiceDial(t, ts.URL, jt)
	waitPeers(t, srv.voice, "team", 1)
	// beim Abgleich (z. B. Kanaländerung) bleibt Ben drin
	srv.voice.roomChanged("team", "team")
	time.Sleep(100 * time.Millisecond)
	if len(srv.voice.Peers("team")) != 1 {
		t.Fatal("beim Abgleich entfernt")
	}
}

func TestTempOwnerSettings(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	anna, ben := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	list, _ := srv.users.List("", 10)
	var annaID string
	for _, u := range list {
		if u.Name == "Anna" {
			annaID = u.ID
		}
	}
	info, err := h.CreateTempRoom(TempRoomInput{Name: "runde", Owner: annaID, OwnerName: "Anna", By: "command"})
	if err != nil {
		t.Fatal(err)
	}
	if res := do(t, "PATCH", ts.URL+"/api/rooms/runde/settings", ben, `{"description":"meins"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("fremd: %d", res.StatusCode)
	}
	if res := do(t, "PATCH", ts.URL+"/api/rooms/runde/settings", anna, `{"voice":{"maxPeers":500}}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("zu viele Plätze: %d", res.StatusCode)
	}
	res := do(t, "PATCH", ts.URL+"/api/rooms/runde/settings", anna, `{"name":"Spielrunde","description":"Heute: Brettspiele","password":"wuerfel","voice":{"maxPeers":6,"stage":true,"video":"speakers","guests":"listen"}}`)
	json.NewDecoder(res.Body).Decode(&info)
	if res.StatusCode != 200 || info.Name != "spielrunde" || info.Voice.MaxPeers != 6 || !info.Voice.Stage || info.Voice.Video != "speakers" || !info.HasPassword || info.Temp == nil {
		t.Fatalf("Besitzer: %d %+v", res.StatusCode, info)
	}
	// Besitzerin ist trotz Passwort drin und moderiert ihren Sprachkanal
	j, code := voiceJoin(t, ts.URL, anna, "spielrunde")
	if code != 200 || !j.Speak || !j.Moderate {
		t.Fatalf("Besitzerin im Sprachkanal: %d %+v", code, j)
	}
	if _, code := voiceJoin(t, ts.URL, ben, "spielrunde"); code != http.StatusForbidden {
		t.Fatalf("Ben ohne Passwort: %d", code)
	}
	// dauerhaft machen geht nicht (Feld wird nicht angeboten)
	do(t, "PATCH", ts.URL+"/api/rooms/spielrunde/settings", anna, `{"temp":null,"permanent":true}`)
	if i, _ := h.roomInfo("spielrunde"); i.Temp == nil {
		t.Fatal("dauerhaft geworden")
	}
}

// Streaming: Abspielen während des Hochladens; Antwort erst am Ende; Lautstärke live ändern.
func TestBotMusicStream(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	name := "talk"
	srv.hub.CreateRoomWith(RoomInput{Name: &name, Voice: &VoiceConfig{BotMusic: true}})
	_, tok, _ := srv.hub.Bots().Create(BotInput{Name: "DJ", AllRooms: true, Perms: &BotPerms{Write: true, Music: true}})
	file := oggOpus(150) // 3 s
	pr, pw := io.Pipe()
	type result struct {
		code int
		body map[string]any
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("POST", ts.URL+"/api/bot/voice/talk/play?stream=1&volume=20&title=Live", pr)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{0, nil}
			return
		}
		var m map[string]any
		json.NewDecoder(res.Body).Decode(&m)
		res.Body.Close()
		done <- result{res.StatusCode, m}
	}()
	// erst den Kopf, dann langsam weiter – der Bot ist schon im Kanal
	pw.Write(file[:len(file)/3])
	waitVoice(t, func() bool {
		for _, p := range srv.voice.Peers("talk") {
			if p.Music == 20 && p.Name == "DJ" {
				return true
			}
		}
		return false
	})
	select {
	case r := <-done:
		t.Fatalf("Antwort vor dem Ende: %+v", r)
	default:
	}
	// Lautstärke während des Abspielens
	if res := post(t, ts.URL+"/api/bot/voice/talk/volume?volume=35", tok, ""); res.StatusCode != 200 {
		t.Fatalf("Lautstärke: %d", res.StatusCode)
	}
	waitVoice(t, func() bool {
		for _, p := range srv.voice.Peers("talk") {
			if p.Music == 35 {
				return true
			}
		}
		return false
	})
	pw.Write(file[len(file)/3:])
	pw.Close()
	select {
	case r := <-done:
		if r.code != 200 || r.body["ended"] != true || r.body["reason"] != "eof" {
			t.Fatalf("Ende: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("kein Ende")
	}
	waitVoice(t, func() bool { return len(srv.voice.Peers("talk")) == 0 })
	// kein Ogg: Fehler
	req, _ := http.NewRequest("POST", ts.URL+"/api/bot/voice/talk/play?stream=1", strings.NewReader("kein ogg"))
	req.Header.Set("Authorization", "Bearer "+tok)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusBadRequest || len(srv.voice.Peers("talk")) != 0 {
		t.Fatalf("kein Ogg: %d", res.StatusCode)
	}
}
