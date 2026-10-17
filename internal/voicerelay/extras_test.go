// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"encoding/binary"
	"testing"
	"time"
)

// Reaktionen, verpackte Schlüssel, Qualität und öffentliche Schlüssel.
func TestReactionsAndKeys(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 50})
	a, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true}, "a"))
	b, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true}, "b"))
	c, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true}, "c"))
	a.c.WriteText([]byte(`{"t":"react","e":"💩"}`)) // nicht erlaubt
	a.c.WriteText([]byte(`{"t":"react","e":"👍"}`))
	if m := b.waitText("react"); m["e"] != "👍" || m["u"] != "a" {
		t.Fatalf("Reaktion: %v", m)
	}
	// zu schnell hintereinander: verworfen
	a.c.WriteText([]byte(`{"t":"react","e":"🎉"}`))
	time.Sleep(700 * time.Millisecond)
	a.c.WriteText([]byte(`{"t":"react","e":"👏"}`))
	if m := b.waitText("react"); m["e"] != "👏" {
		t.Fatalf("Ratenbegrenzung: %v", m)
	}
	// Schlüssel nur an die gemeinte Person
	a.c.WriteText([]byte(`{"t":"e2k","to":` + itoa(b.you) + `,"ep":3,"x":"QUJD"}`))
	if m := b.waitText("e2k"); m["x"] != "QUJD" || int(m["from"].(float64)) != int(a.you) || int(m["ep"].(float64)) != 3 {
		t.Fatalf("e2k: %v", m)
	}
	select {
	case m := <-c.text:
		if m["t"] == "e2k" {
			t.Fatal("Schlüssel an die falsche Person")
		}
	case <-time.After(150 * time.Millisecond):
	}
	// Qualität und öffentlicher Schlüssel stehen in der Teilnehmerliste
	a.c.WriteText([]byte(`{"t":"state","q":2,"pk":"BPk-_x"}`))
	waitFor(t, func() bool {
		for _, p := range e.r.Peers("talk") {
			if p.User == "a" {
				return p.Quality == 2 && p.PubKey == "BPk-_x"
			}
		}
		return false
	})
}

// Leitung: alle stumm (außer Moderierenden), um Freischalten bitten, Aufnahme, Untertitel.
func TestLeaderControls(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 50})
	mod, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true, ModUser: true}, "mod"))
	a, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true}, "a"))
	e.r.Control("talk", "", "forcemute", false, "")
	a.waitText("forcemute")
	select {
	case m := <-mod.text:
		for m["t"] != "forcemute" {
			select {
			case m = <-mod.text:
			case <-time.After(150 * time.Millisecond):
				goto ok
			}
		}
		t.Fatal("Moderierende wurden stummgeschaltet")
	case <-time.After(150 * time.Millisecond):
	}
ok:
	e.r.Control("talk", "a", "askunmute", false, "")
	a.waitText("askunmute")
	e.r.Control("talk", "mod", "rec", true, "")
	waitFor(t, func() bool {
		for _, p := range e.r.Peers("talk") {
			if p.User == "mod" {
				return p.Rec
			}
		}
		return false
	})
	e.r.Control("talk", "a", "cap", true, "Hallo zusammen")
	if m := mod.waitText("cap"); m["x"] != "Hallo zusammen" || m["u"] != "a" || m["f"] != true {
		t.Fatalf("Untertitel: %v", m)
	}
	// Browser dürfen keine Untertitel schicken
	a.c.WriteText([]byte(`{"t":"cap","x":"gefälscht"}`))
	select {
	case m := <-mod.text:
		if m["t"] == "cap" {
			t.Fatalf("Untertitel aus dem Browser weitergeleitet: %v", m)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

// Anheften: Wer jemanden anheftet, bekommt dessen großes Bild – andere nicht.
func TestPinsHighLayer(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 100})
	tk := func(u string) Ticket {
		return withUser(Ticket{Channel: "konf", Kind: KindVideo, Video: true, VideoKbps: 1500, ThumbKbps: 150, Featured: 1}, u)
	}
	a, _ := e.join(tk("a"))
	b, _ := e.join(tk("b"))
	v, _ := e.join(tk("v"))
	w, _ := e.join(tk("w"))
	a.c.WriteText([]byte(`{"t":"cam","on":true,"codec":"vp8"}`))
	a.waitText("cam")
	b.c.WriteText([]byte(`{"t":"cam","on":true,"codec":"vp8"}`))
	b.waitText("cam")
	// a ist groß (zuerst an), b nicht; v heftet b an
	v.c.WriteText([]byte(`{"t":"pins","ids":[` + itoa(b.you) + `]}`))
	if m := b.waitText("layer"); m["high"] != true {
		t.Fatalf("b soll groß senden: %v", m)
	}
	b.sendCam(1, true, 1, 300)
	time.Sleep(50 * time.Millisecond)
	if got := v.cams(300 * time.Millisecond); got[b.you] == nil || got[b.you][1] != 1 {
		t.Fatalf("Anheftende bekommt das große Bild nicht: %v", got)
	}
	if got := w.cams(200 * time.Millisecond); got[b.you] != nil && got[b.you][1] > 0 {
		t.Fatal("andere bekommen das große Bild")
	}
	// lösen: b sendet wieder klein
	v.c.WriteText([]byte(`{"t":"pins","ids":[]}`))
	if m := b.waitText("layer"); m["high"] != false {
		t.Fatalf("b soll nicht mehr groß senden: %v", m)
	}
}

// Ton der Bildschirmfreigabe geht an die Zuschauer.
func TestScreenTone(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 50})
	scr := Ticket{Channel: "talk", Kind: KindScreen, Screen: true, ScreenKbps: 4000, MaxScreens: 1}
	a, _ := e.join(withUser(scr, "a"))
	b, _ := e.join(withUser(scr, "b"))
	c, _ := e.join(withUser(scr, "c"))
	a.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	a.waitText("share")
	b.c.WriteText([]byte(`{"t":"watch","i":` + itoa(a.you) + `,"on":true}`))
	b.waitText("watch")
	tone := []byte{MsgScreenTone, 0, 7, 1, 2, 3}
	a.c.WriteBinary(tone)
	got := 0
	end := time.After(time.Second)
loop:
	for {
		select {
		case m := <-b.audio:
			if m[0] == MsgScreenTone && binary.BigEndian.Uint16(m[1:]) == a.you && binary.BigEndian.Uint16(m[3:]) == 7 {
				got++
				break loop
			}
		case <-end:
			break loop
		}
	}
	if got != 1 {
		t.Fatal("Zuschauer bekommt den Ton nicht")
	}
	select {
	case m := <-c.audio:
		if m[0] == MsgScreenTone {
			t.Fatal("Ton an jemanden, der nicht zusieht")
		}
	case <-time.After(150 * time.Millisecond):
	}
}

// Hand heben: Zeitpunkt der Meldung für die Reihenfolge; bleibt beim erneuten Senden gleich.
func TestHandOrder(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 50})
	a, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true}, "a"))
	b, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true}, "b"))
	at := func(u string) int64 {
		for _, p := range e.r.Peers("talk") {
			if p.User == u {
				return p.HandAt
			}
		}
		return -1
	}
	b.c.WriteText([]byte(`{"t":"state","hand":true}`))
	waitFor(t, func() bool { return at("b") > 0 })
	time.Sleep(5 * time.Millisecond)
	a.c.WriteText([]byte(`{"t":"state","hand":true}`))
	waitFor(t, func() bool { return at("a") > 0 })
	first := at("b")
	if at("a") <= first {
		t.Fatalf("Reihenfolge: a=%d b=%d", at("a"), first)
	}
	b.c.WriteText([]byte(`{"t":"state","hand":true,"mute":true}`)) // nochmal gesendet: Platz bleibt
	time.Sleep(50 * time.Millisecond)
	if at("b") != first {
		t.Fatal("Zeitpunkt der Meldung hat sich geändert")
	}
	e.r.Control("talk", "b", "hand", false, "")
	waitFor(t, func() bool { return at("b") == 0 })
}
