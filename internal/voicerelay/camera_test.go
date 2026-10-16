// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"
	"time"

	"ucng/internal/wsock"
)

func (c *client) sendCam(layer int, key bool, seq uint16, size int) {
	b := make([]byte, 5+size)
	b[0], b[1] = MsgCam, byte(layer)
	if key {
		b[2] = 1
	}
	binary.BigEndian.PutUint16(b[3:], seq)
	c.c.WriteBinary(b)
}

// cams sammelt Kamerabilder: Absender → [Miniansichten, große Bilder].
func (c *client) cams(timeout time.Duration) map[uint16]*[2]int {
	got := map[uint16]*[2]int{}
	end := time.After(timeout)
	for {
		select {
		case b, ok := <-c.audio:
			if !ok {
				return got
			}
			if b[0] != MsgCam {
				continue
			}
			idx := binary.BigEndian.Uint16(b[1:])
			if got[idx] == nil {
				got[idx] = &[2]int{}
			}
			got[idx][b[3]]++
		case <-end:
			return got
		}
	}
}

func camTicket(u string) Ticket {
	return withUser(Ticket{Channel: "konf", Speak: true, Video: true, VideoKbps: 1500, ThumbKbps: 150, Featured: 1}, u)
}

func TestCameraSimulcastAndFeatured(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 100})
	a, _ := e.join(camTicket("a"))
	b, _ := e.join(camTicket("b"))
	v, _ := e.join(withUser(Ticket{Channel: "konf", Speak: true}, "v")) // ohne Kamera-Recht
	v.c.WriteText([]byte(`{"t":"cam","on":true,"codec":"vp8"}`))
	if m := v.waitText("cam"); m["on"] != false || m["reason"] != "denied" {
		t.Fatalf("ohne Recht: %v", m)
	}
	a.c.WriteText([]byte(`{"t":"cam","on":true,"codec":"vp8"}`))
	if m := a.waitText("cam"); m["on"] != true || m["high"] != true {
		t.Fatalf("Kamera A: %v", m) // erste Kamera ist gleich groß
	}
	b.c.WriteText([]byte(`{"t":"cam","on":true,"codec":"vp8"}`))
	if m := b.waitText("cam"); m["on"] != true || m["high"] != false {
		t.Fatalf("Kamera B: %v", m)
	}
	// Miniansicht von beiden, großes Bild nur von A
	a.sendCam(0, true, 1, 2000)
	a.sendCam(1, true, 1, 20000)
	b.sendCam(0, true, 1, 2000)
	b.sendCam(1, true, 1, 20000) // nicht featured: verworfen
	got := v.cams(300 * time.Millisecond)
	if got[a.you] == nil || got[a.you][0] != 1 || got[a.you][1] != 1 || got[b.you] == nil || got[b.you][0] != 1 || got[b.you][1] != 0 {
		t.Fatalf("Zuschauer: %v %v", got[a.you], got[b.you])
	}
	// B spricht → wird groß, A (lange still) wird klein
	time.Sleep(featureHold + 100*time.Millisecond)
	for i := 0; i < 5; i++ {
		b.send(uint16(i), 200, make([]byte, 60))
		time.Sleep(20 * time.Millisecond)
	}
	if m := b.waitText("layer"); m["high"] != true {
		t.Fatalf("B groß: %v", m)
	}
	if m := a.waitText("layer"); m["high"] != false {
		t.Fatalf("A klein: %v", m)
	}
	if !v.featuredIs(b.you) {
		t.Fatal("B nicht groß")
	}
	// großes Bild von B kommt erst ab Schlüsselbild
	b.sendCam(1, false, 2, 3000)
	b.sendCam(1, true, 3, 20000)
	got = v.cams(300 * time.Millisecond)
	if got[b.you] == nil || got[b.you][1] != 1 {
		t.Fatalf("großes Bild B: %v", got[b.you])
	}
	// Kamera-Recht entzogen → Kamera aus, A wird wieder groß
	e.r.Control("konf", "b", "video", false, "")
	b.waitText("cam")
	if !v.featuredIs(a.you) {
		t.Fatal("nach Kamera aus: A nicht groß")
	}
}

func TestMusicPeerAndMove(t *testing.T) {
	e := newEnv(t, Config{})
	// Sprecherplätze voll belegt (1), Musik kommt trotzdem durch
	a, _ := e.join(withUser(Ticket{Channel: "lobby", Speak: true, MaxSpeakers: 1}, "a"))
	l, _ := e.join(withUser(Ticket{Channel: "lobby", Speak: false, MaxSpeakers: 1}, "l"))
	// Musik über eine Verbindung im selben Prozess
	srv, cli := net.Pipe()
	go e.r.ServeConn(wsock.NewServerConn(srv, 1<<20))
	mc := wsock.NewClientConn(cli)
	tk := SignTicket(e.priv, Ticket{Channel: "lobby", User: "bot:x", Name: "DJ", Music: 25, Bitrate: 96000, MaxSpeakers: 1, MaxPeers: 25}, time.Now())
	auth, _ := json.Marshal(map[string]string{"t": "auth", "ticket": tk})
	mc.WriteText(auth)
	if _, d, err := mc.Read(2 * time.Second); err != nil || !json.Valid(d) {
		t.Fatal("hello", err)
	}
	go func() { // Rest lesen
		for {
			if _, _, err := mc.Read(5 * time.Second); err != nil {
				return
			}
		}
	}()
	var music uint16
	for _, p := range e.r.Peers("lobby") {
		if p.Music == 25 {
			music = p.Idx
		}
	}
	if music == 0 {
		t.Fatal("Musik-Teilnehmer fehlt")
	}
	for i := 0; i < 10; i++ {
		a.send(uint16(i), 200, make([]byte, 60))
		b := []byte{MsgAudio, 0, byte(i), 100}
		mc.WriteBinary(append(b, make([]byte, 240)...)) // 96 kbit/s-Rahmen
		time.Sleep(20 * time.Millisecond)
	}
	got := l.frames(300 * time.Millisecond)
	if got[a.you] < 8 || got[music] < 8 {
		t.Fatalf("Zuhörer bekam %v (a=%d, Musik=%d)", got, a.you, music)
	}
	// Kanal-Einstellungen bleiben (Bitrate des Kanals nicht 96 kbit/s)
	e.r.mu.Lock()
	br := e.r.channels["lobby"].bitrate
	e.r.mu.Unlock()
	if br != 24000 {
		t.Fatalf("Bitrate des Kanals verändert: %d", br)
	}
	// Verschieben
	e.r.Control("lobby", "l", "move", false, "andere")
	if m := l.waitText("move"); m["room"] != "andere" {
		t.Fatalf("move: %v", m)
	}
}

// featuredIs liest „featured“-Nachrichten, bis genau idx groß ist.
func (c *client) featuredIs(idx uint16) bool {
	end := time.After(2 * time.Second)
	for {
		select {
		case m, ok := <-c.text:
			if !ok {
				return false
			}
			if m["t"] != "featured" {
				continue
			}
			ids := m["ids"].([]any)
			if len(ids) == 1 && uint16(ids[0].(float64)) == idx {
				return true
			}
		case <-end:
			return false
		}
	}
}
