// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ucng/internal/clientip"
	"ucng/internal/voicerelay"
	"ucng/internal/wsock"
)

func TestAgent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var mu sync.Mutex
	var last map[string]any
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "nein", 401)
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		last = in
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"publicKey": base64.StdEncoding.EncodeToString(pub), "origins": []string{"https://chat.example"}})
	}))
	defer chat.Close()

	ipr, _ := clientip.New(false, "")
	a := &agent{cfg: config{ChatURL: chat.URL, Token: "tok", ID: "v1", ControlURL: "http://v1/control", MaxPerIP: 5},
		ws: "wss://v1.example/ws", kick: make(chan struct{}, 1), client: http.DefaultClient, ipr: ipr, perIP: map[string]int{}}
	a.relay = voicerelay.New(voicerelay.Config{
		PublicKey: func() ed25519.PublicKey {
			if k := a.pub.Load(); k != nil {
				return *k
			}
			return nil
		},
		OriginOK: a.originOK, OnChange: func(string, []voicerelay.PeerInfo) { a.soon() },
	})
	defer a.relay.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", a.handleWS)
	mux.HandleFunc("POST /control", a.handleControl)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	if err := a.report(context.Background()); err != nil {
		t.Fatal(err)
	}
	tk := voicerelay.SignTicket(priv, voicerelay.Ticket{Channel: "talk", User: "u1", Name: "Anna", Speak: true, Bitrate: 24000, MaxSpeakers: 4, MaxPeers: 25}, time.Now())
	wsu := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	if _, err := wsock.Dial(wsu, http.Header{"Origin": {"https://evil.example"}}); err == nil {
		t.Fatal("fremde Herkunft")
	}
	c, err := wsock.Dial(wsu, http.Header{"Origin": {"https://chat.example"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := json.Marshal(map[string]string{"t": "auth", "ticket": tk})
	c.WriteText(b)
	if _, p, err := c.Read(2 * time.Second); err != nil || !strings.Contains(string(p), `"hello"`) {
		t.Fatalf("hello: %v %s", err, p)
	}
	a.report(context.Background())
	mu.Lock()
	rooms := last["rooms"].(map[string]any)
	mu.Unlock()
	if len(rooms["talk"].([]any)) != 1 {
		t.Fatalf("Meldung: %v", rooms)
	}
	// Steuerbefehl: entfernen
	req, _ := http.NewRequest("POST", ts.URL+"/control", strings.NewReader(`{"channel":"talk","user":"u1","action":"kick","reason":"weg"}`))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatal("ohne Schlüssel angenommen")
	}
	req, _ = http.NewRequest("POST", ts.URL+"/control", strings.NewReader(`{"channel":"talk","user":"u1","action":"kick","reason":"weg"}`))
	req.Header.Set("Authorization", "Bearer tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("control: %v", err)
	}
	for {
		_, p, err := c.Read(2 * time.Second)
		if err != nil {
			t.Fatal("kein bye")
		}
		if strings.Contains(string(p), `"bye"`) {
			break
		}
	}
}

func TestWSURL(t *testing.T) {
	for in, want := range map[string]string{"https://v.example": "wss://v.example/ws", "wss://v.example/voice/": "wss://v.example/voice/ws", "http://x:8090": "ws://x:8090/ws"} {
		if got, err := wsURL(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, err := wsURL("ftp://x"); err == nil {
		t.Error("ftp angenommen")
	}
}
