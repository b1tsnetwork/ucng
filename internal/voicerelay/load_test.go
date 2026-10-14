// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build loadtest

// Lasttest des Relays:  go test -tags loadtest -run Load -v ./internal/voicerelay
// Standard: 200 Teilnehmer in 8 Kanälen, je Kanal 4 Sprecher, 10 Sekunden.
package voicerelay

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ucng/internal/wsock"
)

func envN(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func TestLoad(t *testing.T) {
	peers, chans, speakers := envN("PEERS", 200), envN("CHANNELS", 8), envN("SPEAKERS", 4)
	dur := time.Duration(envN("SECONDS", 10)) * time.Second
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := New(Config{PublicKey: func() ed25519.PublicKey { return pub }, MaxParticipants: peers + 10, MaxMbit: 1000})
	defer r.Close()
	ts := httptest.NewServer(r)
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	var recvFrames, recvBytes atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	clients := make([]*wsock.Client, peers)
	for i := 0; i < peers; i++ {
		ch := "c" + strconv.Itoa(i%chans)
		tk := SignTicket(priv, Ticket{Channel: ch, User: "u" + strconv.Itoa(i), Name: "N", Speak: true,
			Bitrate: 24000, MaxSpeakers: 4, MaxPeers: peers}, time.Now())
		c, err := wsock.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]string{"t": "auth", "ticket": tk})
		c.WriteText(b)
		clients[i] = c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				op, p, err := c.Read(5 * time.Second)
				if err != nil {
					return
				}
				if op == wsock.OpBinary && len(p) > 1 {
					recvFrames.Add(int64(p[1]))
					recvBytes.Add(int64(len(p)))
				}
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	// Sprecher: je Kanal die ersten „speakers“ – alle 20 ms ein Rahmen mit 60 Byte (≈ 24 kbit/s)
	frame := make([]byte, 64)
	frame[0], frame[3] = 1, 200
	var sent atomic.Int64
	for i := 0; i < peers; i++ {
		if i/chans >= speakers {
			continue
		}
		c := clients[i]
		go func() {
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			seq := uint16(0)
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					seq++
					f := append([]byte(nil), frame...)
					f[1], f[2] = byte(seq>>8), byte(seq)
					if c.WriteBinary(f) == nil {
						sent.Add(1)
					}
				}
			}
		}()
	}
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	start := time.Now()
	time.Sleep(dur)
	close(stop)
	el := time.Since(start).Seconds()
	runtime.ReadMemStats(&ms1)
	st := r.Stats()
	listeners := peers - 1
	expect := float64(sent.Load()) * float64(peers/chans-1) // jeder Rahmen an alle anderen im Kanal
	t.Logf("%d Teilnehmer, %d Kanäle, %d Sprecher je Kanal, %.0f s", peers, chans, speakers, el)
	t.Logf("gesendet: %d Rahmen (%.0f/s), empfangen: %d Rahmen (%.1f %% von %.0f erwartet)",
		sent.Load(), float64(sent.Load())/el, recvFrames.Load(), 100*float64(recvFrames.Load())/expect, expect)
	t.Logf("ausgehend: %.2f Mbit/s gemessen, %.2f Mbit/s geschätzt (Relay), %.1f kbit/s je Zuhörer",
		float64(recvBytes.Load())*8/el/1e6, st.EstimateMbit, float64(recvBytes.Load())*8/el/1000/float64(listeners))
	t.Logf("Speicher: %.1f MB belegt, %d Goroutinen", float64(ms1.HeapAlloc)/1e6, runtime.NumGoroutine())
	for _, c := range clients {
		c.Close()
	}
	wg.Wait()
	if float64(recvFrames.Load()) < 0.95*expect {
		t.Errorf("zu viele Rahmen verloren")
	}
}
