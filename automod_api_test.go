// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAutomodFlow(t *testing.T) {
	ts, hub := testServer(t)
	admin := adminLogin(t, ts.URL)
	user := claim(t, ts.URL, "Rüpel")

	// Ungültige Muster werden mit Begründung abgelehnt.
	res := do(t, "PUT", ts.URL+"/api/admin/automod", admin, `{"enabled":true,"patterns":["*","ok*"]}`)
	var bad struct{ Problems []string }
	json.NewDecoder(res.Body).Decode(&bad)
	if res.StatusCode != 400 || len(bad.Problems) != 1 {
		t.Fatalf("ungültige Muster: %d %+v", res.StatusCode, bad)
	}
	if r := do(t, "PUT", ts.URL+"/api/admin/automod", admin, `{"enabled":true,"patterns":["Arsch*","*loch","Arsc*och"]}`); r.StatusCode != 200 {
		t.Fatalf("Muster setzen: %d", r.StatusCode)
	}
	if r := do(t, "PUT", ts.URL+"/api/admin/automod", user, `{"enabled":false,"patterns":[]}`); r.StatusCode != 401 {
		t.Fatalf("Nutzer darf Automod nicht ändern: %d", r.StatusCode)
	}

	// Testfeld
	res = do(t, "POST", ts.URL+"/api/admin/automod/test", admin, `{"text":"Du 4rschl0ch!"}`)
	var tr struct {
		Match   bool
		Pattern string
	}
	json.NewDecoder(res.Body).Decode(&tr)
	if !tr.Match || tr.Pattern != "Arsch*" {
		t.Fatalf("Testfeld: %+v", tr)
	}

	// Live-Stream öffnen, dann eine anstößige Nachricht senden.
	sse, err := http.Get(ts.URL + "/api/rooms/lobby/events")
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	lines := make(chan string, 20)
	go func() {
		sc := bufio.NewScanner(sse.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	res = post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"Du bist ein Arschl0ch"}`)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 201 || strings.Contains(string(body), "Arschl0ch") || !strings.Contains(string(body), `"deletedBy":"automod"`) {
		t.Fatalf("Antwort an Absender: %d %s", res.StatusCode, body)
	}
	timeout := time.After(2 * time.Second)
	for got := false; !got; {
		select {
		case l := <-lines:
			if strings.Contains(l, `"type":"message"`) {
				if strings.Contains(l, "Arschl0ch") || strings.Contains(l, "Arsch*") {
					t.Fatalf("Live-Stream verrät Original oder Muster: %s", l)
				}
				got = true
			}
		case <-timeout:
			t.Fatal("keine Live-Nachricht")
		}
	}
	pub, _ := http.Get(ts.URL + "/api/rooms/lobby/messages")
	pb, _ := io.ReadAll(pub.Body)
	if strings.Contains(string(pb), "Arschl0ch") {
		t.Fatal("öffentlicher Verlauf verrät Original")
	}

	// Harmlose Nachricht geht normal durch.
	post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"Entschuldigung!"}`)
	post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"So ein A r s c h"}`)

	// Prüfliste
	res = do(t, "GET", ts.URL+"/api/admin/automod/queue", admin, "")
	var q []QueueItem
	json.NewDecoder(res.Body).Decode(&q)
	if len(q) != 2 || q[1].Message.Original != "Du bist ein Arschl0ch" || q[1].Message.Match != "Arsch*" || q[1].Room != "lobby" {
		t.Fatalf("Prüfliste: %+v", q)
	}

	// Freischalten (Fehlalarm) …
	r := do(t, "POST", ts.URL+"/api/admin/rooms/lobby/messages/"+itoa(q[1].Message.ID)+"/restore", admin, "")
	if r.StatusCode != 200 {
		t.Fatalf("Freischalten: %d", r.StatusCode)
	}
	// … oder ausgeblendet lassen.
	r = do(t, "POST", ts.URL+"/api/admin/rooms/lobby/messages/"+itoa(q[0].Message.ID)+"/confirm", admin, "")
	if r.StatusCode != 200 {
		t.Fatalf("Bestätigen: %d", r.StatusCode)
	}
	if q := hub.AutomodQueue(); len(q) != 0 {
		t.Fatalf("Prüfliste nicht leer: %+v", q)
	}
	msgs := hist(hub, "lobby")
	if msgs[0].Deleted || msgs[0].Text != "Du bist ein Arschl0ch" {
		t.Fatalf("freigeschaltete Nachricht: %+v", msgs[0])
	}
	if !msgs[2].Deleted || msgs[2].DeletedBy != "automod" || msgs[2].Text != "" {
		t.Fatalf("bestätigte Nachricht: %+v", msgs[2])
	}

	// Ausgeschaltet: nichts wird ausgeblendet.
	do(t, "PUT", ts.URL+"/api/admin/automod", admin, `{"enabled":false,"patterns":["Arsch*"]}`)
	res = post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"Arsch"}`)
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	if m.Deleted {
		t.Fatal("ausgeschaltete Automod blendet aus")
	}
}

func TestAutomodPersisted(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"arsch*", "*loch"}})
	h.Post("lobby", "X", "Arschloch")
	flush(t, h, u, s)
	s.Close()
	h2, _, _ := openStore(t, dir)
	if cfg := h2.Automod().Config(); !cfg.Enabled || len(cfg.Patterns) != 2 {
		t.Fatalf("Regeln: %+v", cfg)
	}
	if q := h2.AutomodQueue(); len(q) != 1 || q[0].Message.Match != "arsch*" {
		t.Fatalf("Prüfliste nach Neustart: %+v", q)
	}
}
