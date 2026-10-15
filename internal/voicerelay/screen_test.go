// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"encoding/binary"
	"testing"
	"time"
)

func (c *client) waitText(typ string) map[string]any {
	c.t.Helper()
	end := time.After(2 * time.Second)
	for {
		select {
		case m, ok := <-c.text:
			if !ok {
				c.t.Fatalf("Verbindung zu, warte auf %q", typ)
			}
			if m["t"] == typ {
				return m
			}
		case <-end:
			c.t.Fatalf("keine Nachricht %q", typ)
		}
	}
}

func (c *client) sendVideo(key bool, seq uint16, size int) {
	b := make([]byte, 4+size)
	b[0] = MsgVideo
	if key {
		b[1] = 1
	}
	binary.BigEndian.PutUint16(b[2:], seq)
	c.c.WriteBinary(b)
}

// videos sammelt Video-Rahmen (Absender → Anzahl, und ob der erste ein Schlüsselbild war).
func (c *client) videos(timeout time.Duration) (map[uint16]int, bool) {
	got, firstKey, first := map[uint16]int{}, false, true
	end := time.After(timeout)
	for {
		select {
		case b, ok := <-c.audio:
			if !ok {
				return got, firstKey
			}
			if b[0] != MsgVideo {
				continue
			}
			if first {
				firstKey, first = b[3]&1 != 0, false
			}
			got[binary.BigEndian.Uint16(b[1:])]++
		case <-end:
			return got, firstKey
		}
	}
}

func TestScreenShare(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 50})
	scr := Ticket{Channel: "talk", Speak: true, Screen: true, ScreenKbps: 2500, MaxScreens: 1}
	a, _ := e.join(withUser(scr, "a"))
	b, _ := e.join(withUser(scr, "b"))
	noScr := scr
	noScr.Screen = false
	c, _ := e.join(withUser(noScr, "c"))

	// ohne Recht: abgelehnt
	c.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	if m := c.waitText("share"); m["on"] != false || m["reason"] != "denied" {
		t.Fatalf("ohne Recht: %v", m)
	}
	a.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	if m := a.waitText("share"); m["on"] != true {
		t.Fatalf("teilen: %v", m)
	}
	// nur eine Freigabe je Kanal
	b.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	if m := b.waitText("share"); m["reason"] != "full" {
		t.Fatalf("zweite Freigabe: %v", m)
	}
	// ohne Zuschauer: kein Video bei anderen
	a.sendVideo(true, 1, 5000)
	if v, _ := c.videos(200 * time.Millisecond); len(v) != 0 {
		t.Fatalf("Video ohne Zusehen: %v", v)
	}
	// Zusehen → Schlüsselbild-Anfrage beim Teilenden
	c.c.WriteText([]byte(`{"t":"watch","i":` + itoa(a.you) + `,"on":true}`))
	if m := c.waitText("watch"); m["on"] != true || m["codec"] != "vp8" {
		t.Fatalf("watch: %v", m)
	}
	a.waitText("keyreq")
	time.Sleep(50 * time.Millisecond)
	if est := e.r.Stats().EstimateMbit; est < 2.5 {
		t.Fatalf("Schätzung ohne Video: %v", est)
	}
	// Erst ein Zwischenbild (verworfen), dann Schlüsselbild und weitere Bilder
	a.sendVideo(false, 2, 3000)
	a.sendVideo(true, 3, 40000)
	for i := 4; i < 10; i++ {
		a.sendVideo(false, uint16(i), 3000)
	}
	v, firstKey := c.videos(400 * time.Millisecond)
	if v[a.you] != 7 || !firstKey {
		t.Fatalf("Video: %v, erstes Schlüsselbild %v", v, firstKey)
	}
	if v, _ := b.videos(100 * time.Millisecond); len(v) != 0 {
		t.Fatalf("Ben sieht nicht zu, bekam %v", v)
	}
	// Recht entziehen (Moderation) beendet die Freigabe
	e.r.Control("talk", "a", "screen", false, "")
	a.waitText("share")
	time.Sleep(50 * time.Millisecond)
	for _, p := range e.r.Peers("talk") {
		if p.Screen != "" {
			t.Fatal("Freigabe läuft noch")
		}
	}
	a.sendVideo(true, 20, 1000)
	if v, _ := c.videos(150 * time.Millisecond); len(v) != 0 {
		t.Fatalf("Video nach Ende: %v", v)
	}
	// Jetzt darf Ben
	b.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp09.00.10.08"}`))
	if m := b.waitText("share"); m["on"] != true {
		t.Fatalf("Ben teilen: %v", m)
	}
}

func TestScreenBandwidthLimit(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 4})
	scr := Ticket{Channel: "talk", Speak: true, Screen: true, ScreenKbps: 2500, MaxScreens: 1}
	a, _ := e.join(withUser(scr, "a"))
	b, _ := e.join(withUser(scr, "b"))
	c, _ := e.join(withUser(scr, "c"))
	a.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	a.waitText("share")
	b.c.WriteText([]byte(`{"t":"watch","i":` + itoa(a.you) + `,"on":true}`))
	if m := b.waitText("watch"); m["on"] != true {
		t.Fatalf("erster Zuschauer: %v", m)
	}
	c.c.WriteText([]byte(`{"t":"watch","i":` + itoa(a.you) + `,"on":true}`))
	if m := c.waitText("watch"); m["on"] != false || m["reason"] != "busy" {
		t.Fatalf("über der Grenze: %v", m)
	}
	// Verlässt die Teilende den Kanal, ist der Zuschauer gelöst
	a.c.Close()
	time.Sleep(100 * time.Millisecond)
	if est := e.r.Stats().EstimateMbit; est > 1 {
		t.Fatalf("Schätzung nach Verlassen: %v", est)
	}
}

func withUser(t Ticket, u string) Ticket { t.User, t.Name = u, u; return t }

func itoa(i uint16) string {
	b := []byte{}
	if i == 0 {
		return "0"
	}
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
