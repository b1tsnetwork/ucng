// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ucng/internal/wsock"
)

type env struct {
	t    *testing.T
	priv ed25519.PrivateKey
	r    *Relay
	url  string
	mu   sync.Mutex
	last map[string][]PeerInfo
}

func newEnv(t *testing.T, cfg Config) *env {
	pub, priv, _ := ed25519.GenerateKey(nil)
	e := &env{t: t, priv: priv, last: map[string][]PeerInfo{}}
	cfg.PublicKey = func() ed25519.PublicKey { return pub }
	cfg.OnChange = func(ch string, p []PeerInfo) { e.mu.Lock(); e.last[ch] = p; e.mu.Unlock() }
	e.r = New(cfg)
	ts := httptest.NewServer(e.r)
	t.Cleanup(func() { e.r.Close(); ts.Close() })
	e.url = "ws" + strings.TrimPrefix(ts.URL, "http")
	return e
}

type client struct {
	t     *testing.T
	c     *wsock.Client
	you   uint16
	audio chan []byte
	text  chan map[string]any
}

func (e *env) join(tk Ticket) (*client, error) {
	if tk.Bitrate == 0 {
		tk.Bitrate = 24000
	}
	if tk.MaxSpeakers == 0 {
		tk.MaxSpeakers = 4
	}
	if tk.MaxPeers == 0 {
		tk.MaxPeers = 25
	}
	c, err := wsock.Dial(e.url, nil)
	if err != nil {
		return nil, err
	}
	auth, _ := json.Marshal(map[string]string{"t": "auth", "ticket": SignTicket(e.priv, tk, time.Now())})
	c.WriteText(auth)
	cl := &client{t: e.t, c: c, audio: make(chan []byte, 256), text: make(chan map[string]any, 256)}
	op, data, err := c.Read(2 * time.Second)
	if err != nil {
		return nil, err
	}
	var hello map[string]any
	json.Unmarshal(data, &hello)
	if op != wsock.OpText || hello["t"] != "hello" {
		return nil, err
	}
	cl.you = uint16(hello["you"].(float64))
	go func() {
		for {
			op, data, err := c.Read(5 * time.Second)
			if err != nil {
				close(cl.audio)
				close(cl.text)
				return
			}
			if op == wsock.OpBinary {
				cl.audio <- data
			} else {
				var m map[string]any
				json.Unmarshal(data, &m)
				cl.text <- m
			}
		}
	}()
	return cl, nil
}

func (c *client) send(seq uint16, level uint8, payload []byte) {
	b := []byte{MsgAudio, 0, 0, level}
	binary.BigEndian.PutUint16(b[1:], seq)
	c.c.WriteBinary(append(b, payload...))
}

// frames liest Bündel bis timeout und liefert die Absender-Indizes.
func (c *client) frames(timeout time.Duration) map[uint16]int {
	got := map[uint16]int{}
	end := time.After(timeout)
	for {
		select {
		case b, ok := <-c.audio:
			if !ok {
				return got
			}
			n := int(b[1])
			p := 2
			for i := 0; i < n; i++ {
				idx := binary.BigEndian.Uint16(b[p:])
				l := int(binary.BigEndian.Uint16(b[p+4:]))
				got[idx]++
				p += 6 + l
			}
		case <-end:
			return got
		}
	}
}

func TestForwardAndPermissions(t *testing.T) {
	e := newEnv(t, Config{})
	a, err := e.join(Ticket{Channel: "lobby", User: "a", Name: "Anna", Speak: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.join(Ticket{Channel: "lobby", User: "b", Name: "Ben", Speak: true})
	l, _ := e.join(Ticket{Channel: "lobby", User: "l", Name: "Leo", Speak: false})
	for i := 0; i < 10; i++ {
		a.send(uint16(i), 200, make([]byte, 60))
		l.send(uint16(i), 200, make([]byte, 60)) // darf nicht sprechen
		time.Sleep(20 * time.Millisecond)
	}
	gb := b.frames(300 * time.Millisecond)
	if gb[a.you] < 8 || gb[l.you] != 0 {
		t.Fatalf("Ben bekam %v", gb)
	}
	if ga := a.frames(100 * time.Millisecond); ga[a.you] != 0 {
		t.Fatal("eigene Rahmen zurückbekommen")
	}
	// Bühne: Leo darf sprechen
	e.r.Control("lobby", "l", "speak", true, "")
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 5; i++ {
		l.send(uint16(100+i), 200, make([]byte, 60))
		time.Sleep(20 * time.Millisecond)
	}
	if gb := b.frames(200 * time.Millisecond); gb[l.you] < 4 {
		t.Fatalf("nach Freigabe: %v", gb)
	}
	// Stummgeschaltet vom Team
	e.r.Control("lobby", "a", "mute", true, "")
	for i := 0; i < 5; i++ {
		a.send(uint16(200+i), 200, make([]byte, 60))
		time.Sleep(20 * time.Millisecond)
	}
	if gb := b.frames(200 * time.Millisecond); gb[a.you] != 0 {
		t.Fatal("trotz Stummschaltung weitergeleitet")
	}
	e.mu.Lock()
	n := len(e.last["lobby"])
	e.mu.Unlock()
	if n != 3 {
		t.Fatalf("Teilnehmer gemeldet: %d", n)
	}
	// Rauswurf
	e.r.Control("lobby", "l", "kick", false, "Entfernt")
	time.Sleep(400 * time.Millisecond)
	if len(e.r.Peers("lobby")) != 2 {
		t.Fatalf("nach Rauswurf: %v", e.r.Peers("lobby"))
	}
}

func TestSpeakerLimitAndRate(t *testing.T) {
	e := newEnv(t, Config{})
	var spk []*client
	for _, u := range []string{"a", "b", "c"} {
		c, err := e.join(Ticket{Channel: "k", User: u, Name: u, Speak: true, MaxSpeakers: 2})
		if err != nil {
			t.Fatal(err)
		}
		spk = append(spk, c)
	}
	l, _ := e.join(Ticket{Channel: "k", User: "l", Name: "l", MaxSpeakers: 2})
	for i := 0; i < 10; i++ {
		for _, c := range spk {
			c.send(uint16(i), 100, make([]byte, 60))
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := l.frames(300 * time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("erwartet 2 Sprecher, bekam %v", got)
	}
	// Bitratengrenze: viel zu große Rahmen werden verworfen
	for i := 0; i < 20; i++ {
		spk[0].send(uint16(50+i), 100, make([]byte, 1200))
	}
	if got := l.frames(300 * time.Millisecond); got[spk[0].you] > 3 {
		t.Fatalf("Bitratengrenze: %v", got)
	}
}

func TestLimitsAndReplay(t *testing.T) {
	e := newEnv(t, Config{MaxParticipants: 2})
	tk := Ticket{Channel: "x", User: "a", Name: "a", Bitrate: 24000, MaxSpeakers: 4, MaxPeers: 25, Nonce: "fest"}
	if _, err := e.join(tk); err != nil {
		t.Fatal(err)
	}
	if _, err := e.join(tk); err == nil {
		t.Fatal("Zutrittskarte doppelt verwendet")
	}
	if _, err := e.join(Ticket{Channel: "x", User: "b", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.join(Ticket{Channel: "y", User: "c", Name: "c"}); err == nil {
		t.Fatal("Servergrenze nicht beachtet")
	}
	// Dieselbe Person in einem anderen Kanal: Umzug statt zusätzlicher Platz
	if _, err := e.join(Ticket{Channel: "y", User: "b", Name: "b"}); err != nil {
		t.Fatalf("Umzug: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if st := e.r.Stats(); st.Participants != 2 || st.Channels["y"] != 1 {
		t.Fatalf("%+v", st)
	}
	// Abgelaufen / gefälscht
	c, _ := wsock.Dial(e.url, nil)
	auth, _ := json.Marshal(map[string]string{"t": "auth", "ticket": SignTicket(e.priv, Ticket{Channel: "x", User: "z", Exp: time.Now().Add(-time.Minute).Unix()}, time.Now())})
	c.WriteText(auth)
	if _, _, err := c.Read(time.Second); err == nil {
		t.Fatal("abgelaufene Karte angenommen")
	}
	_, other, _ := ed25519.GenerateKey(nil)
	if _, err := VerifyTicket(e.priv.Public().(ed25519.PublicKey), SignTicket(other, Ticket{Channel: "x", User: "z"}, time.Now()), time.Now()); err == nil {
		t.Fatal("fremde Signatur angenommen")
	}
}

func TestEstimate(t *testing.T) {
	if k := EstimateKbps(24000, 3); k < 100 || k > 115 {
		t.Fatalf("Standard, 3 Sprecher: %.1f kbit/s", k)
	}
}
