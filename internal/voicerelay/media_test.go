// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"encoding/binary"
	"testing"
	"time"
)

func (c *client) sendVideoTid(key bool, tid uint8, seq uint16, size int) {
	b := make([]byte, 4+size)
	b[0] = MsgVideo
	b[1] = tid << 1
	if key {
		b[1] |= 1
	}
	binary.BigEndian.PutUint16(b[2:], seq)
	c.c.WriteBinary(b)
}

// Bildschirm über eine eigene Medienverbindung: neben der Sprache derselben Person, ohne sie
// zu ersetzen; erscheint nicht als Teilnehmerliste, zählt aber in der Auslastung.
func TestMediaConnection(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 50, MaxParticipants: 2})
	voice := Ticket{Channel: "talk", Speak: true}
	scr := Ticket{Channel: "talk", Kind: KindScreen, Screen: true, ScreenKbps: 4000, MaxScreens: 1}
	a, _ := e.join(withUser(voice, "a"))
	am, err := e.join(withUser(scr, "a"))
	if err != nil || am == nil {
		t.Fatalf("Medienverbindung: %v", err)
	}
	b, _ := e.join(withUser(voice, "b"))
	bm, _ := e.join(withUser(scr, "b"))
	if b == nil || bm == nil {
		t.Fatal("zweite Person: Medienverbindungen zählen nicht als Teilnehmer")
	}
	// die Sprachverbindung von a ist noch da
	a.send(1, 100, []byte("hi"))
	if got := b.frames(300 * time.Millisecond); got[a.you] == 0 {
		t.Fatalf("Sprache von a nach Medienverbindung: %v", got)
	}
	e.mu.Lock()
	_, listed := e.last[MediaKey("talk", KindScreen)]
	n := len(e.last["talk"])
	e.mu.Unlock()
	if listed || n != 2 {
		t.Fatalf("Teilnehmerliste: Medienkanal gemeldet=%v, Sprache=%d", listed, n)
	}
	// teilen und zusehen auf der Medienverbindung
	am.c.WriteText([]byte(`{"t":"share","on":true,"codec":"avc1.64002a"}`))
	if m := am.waitText("share"); m["on"] != true {
		t.Fatalf("teilen: %v", m)
	}
	bm.c.WriteText([]byte(`{"t":"watch","i":` + itoa(am.you) + `,"on":true}`))
	if m := bm.waitText("watch"); m["on"] != true {
		t.Fatalf("zusehen: %v", m)
	}
	st := e.r.Stats()
	ms := st.Media[MediaKey("talk", KindScreen)]
	if st.Participants != 2 || st.MediaPeers != 2 || ms.Peers != 2 || ms.Pub != 1 || ms.Mbit < 4 {
		t.Fatalf("Stats: %+v / %+v", st, ms)
	}
	am.sendVideo(true, 1, 2000)
	if got, key := bm.videos(300 * time.Millisecond); got[am.you] != 1 || !key {
		t.Fatalf("Bild: %v %v", got, key)
	}
	// Hinweis auf der Sprachverbindung: die Seitenleiste zeigt „teilt“
	a.c.WriteText([]byte(`{"t":"state","sc":"avc1.64002a"}`))
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, p := range e.last["talk"] {
			if p.User == "a" {
				return p.Screen == "avc1.64002a"
			}
		}
		return false
	})
	// Verlassen der Sprache: nur die Medienverbindung beenden
	e.r.Control("talk", "b", "mediakick", false, "left")
	waitFor(t, func() bool { return e.r.Stats().MediaPeers == 1 })
	if e.r.Stats().Participants != 2 {
		t.Fatal("mediakick hat die Sprache beendet")
	}
	// kick beendet beides
	e.r.Control("talk", "a", "kick", false, "kicked")
	waitFor(t, func() bool { st := e.r.Stats(); return st.MediaPeers == 0 && st.Participants == 1 })
}

// Zeitebenen: Ist die Leitung eines Zuschauers voll, fallen zuerst Bilder der oberen Ebene weg
// (ohne auf ein Schlüsselbild warten zu müssen).
func TestTemporalLayerDrop(t *testing.T) {
	e := newEnv(t, Config{MaxMbit: 500})
	scr := Ticket{Channel: "talk", Kind: KindScreen, Screen: true, ScreenKbps: 20000, MaxScreens: 1}
	a, _ := e.join(withUser(scr, "a"))
	b, _ := e.join(withUser(scr, "b"))
	a.c.WriteText([]byte(`{"t":"share","on":true,"codec":"vp8"}`))
	a.waitText("share")
	b.c.WriteText([]byte(`{"t":"watch","i":` + itoa(a.you) + `,"on":true}`))
	b.waitText("watch")

	e.r.mu.Lock()
	var w, s *peer
	for _, p := range e.r.channels[MediaKey("talk", KindScreen)].peers {
		if p.user == "b" {
			w = p
		} else {
			s = p
		}
	}
	e.r.mu.Unlock()

	// Regeln der Warteschlange an einem eigenen Teilnehmer ohne Schreiber (kein Wettlauf)
	q := &peer{vout: make(chan []byte, 8), vBudget: 10000}
	e.r.mu.Lock()
	q.vq.Store(6000) // mehr als die Hälfte belegt
	ok1 := e.r.enqueueVideoLocked(q, make([]byte, 100), true)
	ok0 := e.r.enqueueVideoLocked(q, make([]byte, 100), false)
	q.vq.Store(9990)
	full := e.r.enqueueVideoLocked(q, make([]byte, 100), false)
	q.vq.Store(0)
	big := e.r.enqueueVideoLocked(q, make([]byte, 50000), false)
	e.r.mu.Unlock()
	if ok1 || !ok0 || full || !big {
		t.Fatalf("Warteschlange: obere Ebene=%v Grundebene=%v voll=%v leer+groß=%v", ok1, ok0, full, big)
	}

	// Schlüsselbild kommt an (Warteschlange des Zuschauers leer)
	a.sendVideoTid(true, 0, 1, 500)
	if got, _ := b.videos(time.Second); got[a.you] != 1 {
		t.Fatalf("Schlüsselbild: %v", got)
	}
	// warten, bis der Schreiber alles abgegeben hat – danach rührt er vq nicht mehr an
	waitFor(t, func() bool { return w.vq.Load() == 0 && len(w.vout) == 0 })
	e.r.mu.Lock()
	w.vBudget = 10000
	w.vq.Store(9000) // Leitung „voll“: obere Ebene muss weichen
	e.r.mu.Unlock()
	// Auslassen einer oberen Ebene setzt kein „Schlüsselbild nötig“
	a.sendVideoTid(false, 1, 2, 500)
	waitFor(t, func() bool {
		e.r.mu.Lock()
		defer e.r.mu.Unlock()
		_, ok := w.skipTid[s.idx]
		return ok
	})
	e.r.mu.Lock()
	needKey, skip := w.needKey[s.idx], w.skipTid[s.idx]
	e.r.mu.Unlock()
	if needKey || skip != 1 {
		t.Fatalf("obere Ebene ausgelassen: needKey=%v skip=%d", needKey, skip)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Bedingung nicht erreicht")
}
