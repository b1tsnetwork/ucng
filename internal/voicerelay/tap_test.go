// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// oggPackets liest die Pakete einer Ogg-Datei (prüft dabei die Prüfsummen).
func oggPackets(t *testing.T, b []byte) (packets [][]byte, lastGranule uint64) {
	t.Helper()
	var cur []byte
	for len(b) > 0 {
		if len(b) < 27 || string(b[:4]) != "OggS" {
			t.Fatalf("keine Ogg-Seite")
		}
		n := int(b[26])
		lac := b[27 : 27+n]
		size := 27 + n
		for _, l := range lac {
			size += int(l)
		}
		pg := append([]byte(nil), b[:size]...)
		want := binary.LittleEndian.Uint32(pg[22:])
		binary.LittleEndian.PutUint32(pg[22:], 0)
		if oggChecksum(pg) != want {
			t.Fatal("Prüfsumme falsch")
		}
		lastGranule = binary.LittleEndian.Uint64(b[6:])
		body := b[27+n : size]
		for _, l := range lac {
			cur = append(cur, body[:l]...)
			body = body[l:]
			if l < 255 {
				packets = append(packets, cur)
				cur = nil
			}
		}
		b = b[size:]
	}
	return
}

func TestTapSegments(t *testing.T) {
	segs := make(chan Segment, 8)
	e := newEnv(t, Config{MaxMbit: 50, OnSegment: func(s Segment) { segs <- s }})
	a, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true, Tap: TapCaptions, Lang: "de"}, "a"))
	b, _ := e.join(withUser(Ticket{Channel: "talk", Speak: true, Tap: TapCaptions}, "b"))
	frame := func(i int) []byte { return append([]byte{0xF8}, bytes.Repeat([]byte{byte(i)}, 40)...) } // CELT 20 ms
	b.c.WriteText([]byte(`{"t":"state","nc":true}`))                                                  // b möchte keine Untertitel
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 40; i++ {
		a.send(uint16(i), 120, frame(i))
		b.send(uint16(i), 120, frame(i))
		time.Sleep(20 * time.Millisecond)
	}
	var s Segment
	select {
	case s = <-segs:
	case <-time.After(2 * time.Second):
		t.Fatal("kein Abschnitt")
	}
	if s.User != "a" || s.Lang != "de" || s.Mode != TapCaptions || s.Dur != 800*time.Millisecond {
		t.Fatalf("Abschnitt: %+v", s)
	}
	pk, gr := oggPackets(t, s.Ogg)
	if len(pk) != 42 || string(pk[0][:8]) != "OpusHead" || string(pk[1][:8]) != "OpusTags" || gr != 40*960 || !bytes.Equal(pk[2], frame(0)) {
		t.Fatalf("Ogg: %d Pakete, Granule %d", len(pk), gr)
	}
	select {
	case s := <-segs:
		t.Fatalf("Abschnitt trotz „nicht verschriftlichen“: %+v", s.User)
	case <-time.After(300 * time.Millisecond):
	}
	// Live-Transkription: alle, auch b; Ausschalten gibt den laufenden Abschnitt noch weiter
	if !e.r.Control("talk", "", "tap", false, "2") {
		t.Fatal("tap")
	}
	for i := 0; i < 30; i++ {
		b.send(uint16(100+i), 120, frame(i))
		time.Sleep(20 * time.Millisecond)
	}
	e.r.Control("talk", "", "tap", false, "0")
	select {
	case s := <-segs:
		if s.User != "b" || s.Mode != TapTranscript {
			t.Fatalf("Transkript: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("letzter Abschnitt fehlt")
	}
	// aus: nichts mehr
	for i := 0; i < 30; i++ {
		a.send(uint16(200+i), 120, frame(i))
	}
	select {
	case <-segs:
		t.Fatal("Abschnitt trotz ausgeschaltetem Mitschnitt")
	case <-time.After(1200 * time.Millisecond):
	}
}

func TestOpusSamples(t *testing.T) {
	for toc, want := range map[byte]int{0x08: 960, 0x78: 960, 0xF8: 960, 0xF0: 480, 0x09: 1920, 0x0B: 1920} {
		if got := opusSamples([]byte{toc, 2}); got != want {
			t.Errorf("TOC %#x: %d, erwartet %d", toc, got, want)
		}
	}
}
