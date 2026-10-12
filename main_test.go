// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	cfg := Config{AllowedOrigins: []string{"https://example.com"}, MaxRooms: 5, RateLimit: 100, RateWindow: time.Second, ClaimLimit: 100, AdminPassword: testAdminPW, BotRate: 1000}
	hub := NewHub(cfg.MaxRooms)
	srv := NewServer(cfg, hub, NewUsers(time.Hour, 100))
	ctx, cancel := context.WithCancel(context.Background())
	go srv.webhooks.run(ctx)
	t.Cleanup(cancel)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() { close(srv.done); ts.Close() })
	return ts, hub
}

func do(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// claim reserviert einen Namen und liefert das Token.
func claim(t *testing.T, base, name string) string {
	t.Helper()
	res := do(t, "POST", base+"/api/session", "", `{"name":"`+name+`"}`)
	if res.StatusCode != 200 {
		t.Fatalf("claim %s: %d", name, res.StatusCode)
	}
	var out struct{ Token string }
	json.NewDecoder(res.Body).Decode(&out)
	return out.Token
}

func post(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	return do(t, "POST", url, token, body)
}

func TestPostAndHistory(t *testing.T) {
	ts, _ := testServer(t)
	tok := claim(t, ts.URL, "Anna")
	for i := 0; i < 5; i++ {
		res := post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"Hallo `+string(rune('A'+i))+`"}`)
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("status %d", res.StatusCode)
		}
	}
	res, _ := http.Get(ts.URL + "/api/rooms/lobby/messages")
	var msgs []Message
	json.NewDecoder(res.Body).Decode(&msgs)
	if len(msgs) != 5 || msgs[4].Text != "Hallo E" || msgs[4].Name != "Anna" {
		t.Fatalf("Verlauf: %+v", msgs)
	}
	if res.Header.Get("X-Has-More") != "false" {
		t.Fatal("X-Has-More sollte false sein")
	}
}
func TestValidation(t *testing.T) {
	ts, _ := testServer(t)
	tok := claim(t, ts.URL, "a")
	cases := []struct{ path, body string }{
		{"/api/rooms/bad%20room/messages", `{"text":"b"}`},
		{"/api/rooms/lobby/messages", `{"text":"   "}`},
		{"/api/rooms/lobby/messages", `{"text":"` + strings.Repeat("x", 2001) + `"}`},
		{"/api/rooms/lobby/messages", `kein json`},
	}
	for _, c := range cases {
		if res := post(t, ts.URL+c.path, tok, c.body); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %q: erwartet 400, bekam %d", c.path, c.body[:min(len(c.body), 30)], res.StatusCode)
		}
	}
}

func TestCORS(t *testing.T) {
	ts, _ := testServer(t)
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/api/rooms/lobby/messages", nil)
	req.Header.Set("Origin", "https://example.com")
	res, _ := http.DefaultClient.Do(req)
	if res.Header.Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Fatal("erlaubter Origin wurde nicht freigegeben")
	}
	req.Header.Set("Origin", "https://evil.test")
	res, _ = http.DefaultClient.Do(req)
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("fremder Origin wurde freigegeben")
	}
}

func TestSSE(t *testing.T) {
	ts, _ := testServer(t)
	res, err := http.Get(ts.URL + "/api/rooms/live/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)

	next := func() Event {
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				var ev Event
				json.Unmarshal([]byte(line[6:]), &ev)
				return ev
			}
		}
		t.Fatal("Stream beendet")
		return Event{}
	}

	if ev := next(); ev.Type != "history" {
		t.Fatalf("erstes Event: %s", ev.Type)
	}
	post(t, ts.URL+"/api/rooms/live/messages", claim(t, ts.URL, "Bob"), `{"text":"live!"}`)
	for {
		ev := next()
		if ev.Type == "message" {
			if ev.Message.Text != "live!" || ev.Online != 1 {
				t.Fatalf("falsches Event: %+v", ev)
			}
			return
		}
	}
}

func TestEmbedAssets(t *testing.T) {
	ts, _ := testServer(t)
	for _, p := range []string{"/", "/app.js", "/embed/chat.js", "/embed/chat.css", "/demo.html", "/admin", "/admin.js", "/admin.css"} {
		res, _ := http.Get(ts.URL + p)
		if res.StatusCode != 200 {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	cr, _ := u.Claim("Anna", "")
	tok := cr.Token
	h.PostAs("lobby", cr.ID, "Anna", "bleibt erhalten")
	flush(t, h, u, s)
	s.Close()

	data, _ := os.ReadFile(filepath.Join(dir, "users.json"))
	if strings.Contains(string(data), tok) {
		t.Fatal("Token im Klartext gespeichert")
	}
	h2, u2, _ := openStore(t, dir)
	if me, ok := u2.Lookup(tok); !ok || me.Name != "Anna" || me.ID != cr.ID {
		t.Fatal("reservierter Name nach dem Laden verloren")
	}
	msgs := hist(h2, "lobby")
	if len(msgs) != 1 || msgs[0].Text != "bleibt erhalten" {
		t.Fatalf("geladen: %+v", msgs)
	}
	m, _ := h2.Post("lobby", "Anna", "neu")
	if m.ID <= msgs[0].ID {
		t.Fatal("IDs nach dem Laden nicht fortgesetzt")
	}
}
func TestRateLimit(t *testing.T) {
	rl := newRateLimiter(2, time.Minute)
	if !rl.Allow("x") || !rl.Allow("x") || rl.Allow("x") {
		t.Fatal("Rate-Limit greift nicht")
	}
	if !rl.Allow("y") {
		t.Fatal("andere IP fälschlich blockiert")
	}
}

func TestNameReservation(t *testing.T) {
	ts, _ := testServer(t)
	anna := claim(t, ts.URL, "Anna")

	// Name (auch in anderer Schreibweise) ist vergeben.
	for _, n := range []string{"Anna", "anna", "  ANNA "} {
		if res := do(t, "POST", ts.URL+"/api/session", "", `{"name":"`+n+`"}`); res.StatusCode != http.StatusConflict {
			t.Fatalf("%q: erwartet 409, bekam %d", n, res.StatusCode)
		}
	}

	// Ohne Token kein Senden, mit falschem Token auch nicht.
	if res := post(t, ts.URL+"/api/rooms/lobby/messages", "", `{"text":"x"}`); res.StatusCode != 401 {
		t.Fatalf("ohne Token: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/rooms/lobby/messages", "falsch", `{"text":"x"}`); res.StatusCode != 401 {
		t.Fatalf("falsches Token: %d", res.StatusCode)
	}

	// Ein mitgeschickter Name im Body wird ignoriert.
	res := post(t, ts.URL+"/api/rooms/lobby/messages", anna, `{"name":"Admin","text":"hi"}`)
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	if m.Name != "Anna" {
		t.Fatalf("Name aus Body übernommen: %q", m.Name)
	}

	// Session prüfen.
	res = do(t, "GET", ts.URL+"/api/session", anna, "")
	var s struct{ Name string }
	json.NewDecoder(res.Body).Decode(&s)
	if res.StatusCode != 200 || s.Name != "Anna" {
		t.Fatalf("Session: %d %q", res.StatusCode, s.Name)
	}

	// Umbenennen: gleiches Token, alter Name wird frei.
	res = do(t, "POST", ts.URL+"/api/session", anna, `{"name":"Anna B"}`)
	var out struct{ Token, Name string }
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 || out.Token != anna || out.Name != "Anna B" {
		t.Fatalf("Umbenennen: %d %+v", res.StatusCode, out)
	}
	claim(t, ts.URL, "Anna")

	// Freigeben.
	do(t, "DELETE", ts.URL+"/api/session", anna, "")
	if res := do(t, "GET", ts.URL+"/api/session", anna, ""); res.StatusCode != 401 {
		t.Fatal("Token nach Freigabe noch gültig")
	}
	claim(t, ts.URL, "Anna B")
}

func TestNameExpiry(t *testing.T) {
	u := NewUsers(time.Hour, 10)
	now := time.Now()
	u.now = func() time.Time { return now }
	cr, _ := u.Claim("Carla", "")
	tok := cr.Token
	if _, err := u.Claim("carla", ""); err != ErrNameTaken {
		t.Fatal("Name sollte vergeben sein")
	}
	now = now.Add(2 * time.Hour)
	if _, ok := u.Lookup(tok); ok {
		t.Fatal("abgelaufenes Token noch gültig")
	}
	if _, err := u.Claim("Carla", ""); err != nil {
		t.Fatalf("abgelaufener Name nicht frei: %v", err)
	}
}

func TestMaxUsers(t *testing.T) {
	u := NewUsers(0, 2)
	u.Claim("a", "")
	u.Claim("b", "")
	if _, err := u.Claim("c", ""); err != ErrTooManyUsers {
		t.Fatalf("erwartet ErrTooManyUsers, bekam %v", err)
	}
}

func TestRenameKeepsHistory(t *testing.T) {
	ts, hub := testServer(t)
	anna := claim(t, ts.URL, "Anna")
	bob := claim(t, ts.URL, "Bob")
	post(t, ts.URL+"/api/rooms/lobby/messages", anna, `{"text":"eins"}`)
	post(t, ts.URL+"/api/rooms/zwei/messages", anna, `{"text":"zwei"}`)
	post(t, ts.URL+"/api/rooms/lobby/messages", bob, `{"text":"von Bob"}`)

	res := do(t, "POST", ts.URL+"/api/session", anna, `{"name":"Anna Maria"}`)
	var out struct{ ID, Name string }
	json.NewDecoder(res.Body).Decode(&out)
	if out.ID == "" || out.Name != "Anna Maria" {
		t.Fatalf("Antwort: %+v", out)
	}

	lobby := hist(hub, "lobby")
	zwei := hist(hub, "zwei")
	if lobby[0].Name != "Anna Maria" || zwei[0].Name != "Anna Maria" || lobby[0].UserID != out.ID {
		t.Fatalf("alte Nachrichten nicht umbenannt: %+v %+v", lobby, zwei)
	}
	if lobby[1].Name != "Bob" {
		t.Fatal("fremde Nachricht wurde umbenannt")
	}
	// Den alten Namen kann jetzt jemand anderes nehmen – seine Nachrichten haben eine andere ID.
	neu := claim(t, ts.URL, "Anna")
	res = post(t, ts.URL+"/api/rooms/lobby/messages", neu, `{"text":"neue Anna"}`)
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	if m.UserID == "" || m.UserID == out.ID {
		t.Fatal("neue Anna hat dieselbe ID")
	}
}

func TestHistoryPaging(t *testing.T) {
	ts, hub := testServer(t)
	for i := 1; i <= 1234; i++ {
		hub.Post("lobby", "A", fmt.Sprintf("Nachricht %d", i))
	}
	get := func(q string) ([]Message, string) {
		res, _ := http.Get(ts.URL + "/api/rooms/lobby/messages" + q)
		var m []Message
		json.NewDecoder(res.Body).Decode(&m)
		return m, res.Header.Get("X-Has-More")
	}
	page, more := get("")
	if len(page) != 100 || page[99].Text != "Nachricht 1234" || more != "true" {
		t.Fatalf("erste Seite: %d %q %s", len(page), page[len(page)-1].Text, more)
	}
	// Alles seitenweise zurückblättern – nichts darf fehlen oder doppelt sein.
	seen := len(page)
	for more == "true" {
		page, more = get(fmt.Sprintf("?before=%d&limit=500", page[0].ID))
		seen += len(page)
	}
	if seen != 1234 || page[0].Text != "Nachricht 1" {
		t.Fatalf("insgesamt %d Nachrichten gesehen, älteste %q", seen, page[0].Text)
	}
	if p, _ := get("?limit=99999"); len(p) != MaxPage {
		t.Fatalf("Limit nicht begrenzt: %d", len(p))
	}
	// Live-Stream liefert die neuesten 100 und meldet ältere.
	res, err := http.Get(ts.URL + "/api/rooms/lobby/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			var ev Event
			json.Unmarshal([]byte(line[6:]), &ev)
			if ev.Type != "history" || len(ev.Messages) != 100 || !ev.More {
				t.Fatalf("history-Event: %s, %d Nachrichten, more=%v", ev.Type, len(ev.Messages), ev.More)
			}
			return
		}
	}
}
