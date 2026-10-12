// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type createdBot struct {
	Bot   BotInfo `json:"bot"`
	Token string  `json:"token"`
}

func createBot(t *testing.T, base, admin, body string) createdBot {
	t.Helper()
	res := do(t, "POST", base+"/api/admin/bots", admin, body)
	if res.StatusCode != 201 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("Bot anlegen: %d %s", res.StatusCode, b)
	}
	var c createdBot
	json.NewDecoder(res.Body).Decode(&c)
	return c
}

// sseReader liest Ereignisse aus einem Server-Sent-Events-Stream.
func sseReader(t *testing.T, url, token string) (chan botEvent, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("Stream: %d", res.StatusCode)
	}
	ch := make(chan botEvent, 50)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				var ev botEvent
				json.Unmarshal([]byte(l[6:]), &ev)
				ch <- ev
			}
		}
		close(ch)
	}()
	return ch, func() { res.Body.Close() }
}

func next(t *testing.T, ch chan botEvent) botEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("Stream beendet")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("kein Ereignis erhalten")
	}
	return botEvent{}
}

func TestBotBasics(t *testing.T) {
	ts, hub := testServer(t)
	admin := adminLogin(t, ts.URL)
	hub.EnsureDefaultRoom()
	hub.CreateRoom("support", "")
	hub.CreateRoom("intern", "")
	anna := claim(t, ts.URL, "Anna")

	// Name ist mit Personen abgestimmt – in beide Richtungen.
	if res := do(t, "POST", ts.URL+"/api/admin/bots", admin, `{"name":"anna","allRooms":true}`); res.StatusCode != 409 {
		t.Fatalf("Bot mit Personen-Namen: %d", res.StatusCode)
	}
	b := createBot(t, ts.URL, admin, `{"name":"Wetter-Bot","description":"Sagt das Wetter an","rooms":["Lobby","support"]}`)
	if !strings.HasPrefix(b.Token, "gcb_") || b.Bot.Rooms[0] != "lobby" || b.Bot.WebhookSecret == "" {
		t.Fatalf("angelegt: %+v", b)
	}
	if res := do(t, "POST", ts.URL+"/api/session", "", `{"name":"WETTER-bot"}`); res.StatusCode != 409 {
		t.Fatalf("Person mit Bot-Namen: %d", res.StatusCode)
	}
	if res := do(t, "POST", ts.URL+"/api/admin/bots", admin, `{"name":"Leer","rooms":[]}`); res.StatusCode != 400 {
		t.Fatalf("Bot ohne Kanäle: %d", res.StatusCode)
	}
	if res := do(t, "POST", ts.URL+"/api/admin/bots", admin, `{"name":"Hook","allRooms":true,"webhookUrl":"ftp://x"}`); res.StatusCode != 400 {
		t.Fatalf("ungültige Webhook-URL: %d", res.StatusCode)
	}

	// Anmeldung
	if res := do(t, "GET", ts.URL+"/api/bot/me", b.Token, ""); res.StatusCode != 200 {
		t.Fatalf("me: %d", res.StatusCode)
	}
	for _, bad := range []string{"", "gcb_falsch", anna, adminLogin(t, ts.URL)} {
		if res := do(t, "GET", ts.URL+"/api/bot/me", bad, ""); res.StatusCode != 401 {
			t.Fatalf("fremder Schlüssel %q: %d", bad[:min(len(bad), 8)], res.StatusCode)
		}
	}

	// Schreiben: erlaubt, verboten, nicht vorhanden
	res := do(t, "POST", ts.URL+"/api/bot/rooms/support/messages", b.Token, `{"text":"Sonnig, 21 Grad"}`)
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	if res.StatusCode != 201 || !m.Bot || m.Name != "Wetter-Bot" || m.UserID != b.Bot.ID {
		t.Fatalf("Bot-Nachricht: %d %+v", res.StatusCode, m)
	}
	if res := do(t, "POST", ts.URL+"/api/bot/rooms/intern/messages", b.Token, `{"text":"x"}`); res.StatusCode != 403 {
		t.Fatalf("verbotener Kanal: %d", res.StatusCode)
	}
	if res := do(t, "GET", ts.URL+"/api/bot/rooms/intern/messages", b.Token, ""); res.StatusCode != 403 {
		t.Fatalf("verbotener Verlauf: %d", res.StatusCode)
	}
	res = do(t, "GET", ts.URL+"/api/bot/rooms", b.Token, "")
	var rooms []RoomInfo
	json.NewDecoder(res.Body).Decode(&rooms)
	if len(rooms) != 2 {
		t.Fatalf("erlaubte Kanäle: %+v", rooms)
	}
	alle := createBot(t, ts.URL, admin, `{"name":"Allwissend","allRooms":true}`)
	if res := do(t, "POST", ts.URL+"/api/bot/rooms/gibtsnicht/messages", alle.Token, `{"text":"x"}`); res.StatusCode != 404 {
		t.Fatalf("Bot darf keine Kanäle anlegen: %d", res.StatusCode)
	}

	// Kanal umbenennen nimmt die Bot-Rechte mit, Löschen entfernt sie.
	hub.UpdateRoom("support", "hilfe", "")
	if got, _ := hub.Bots().Get(b.Bot.ID); !got.Allowed("hilfe") || got.Allowed("support") {
		t.Fatalf("Rechte nach Umbenennen: %+v", got.Rooms)
	}
	hub.DeleteRoom("hilfe")
	if got, _ := hub.Bots().Get(b.Bot.ID); len(got.Rooms) != 1 || got.Rooms[0] != "lobby" {
		t.Fatalf("Rechte nach Löschen: %+v", got.Rooms)
	}
}

func TestBotEventsStream(t *testing.T) {
	ts, hub := testServer(t)
	hub.EnsureDefaultRoom()
	admin := adminLogin(t, ts.URL)
	a := createBot(t, ts.URL, admin, `{"name":"Lauscher","allRooms":true}`)
	other := createBot(t, ts.URL, admin, `{"name":"Plauderer","allRooms":true}`)
	user := claim(t, ts.URL, "Mensch")

	ch, stop := sseReader(t, ts.URL+"/api/bot/events", a.Token)
	defer stop()
	if ev := next(t, ch); ev.Event != "ready" || ev.Bot.Name != "Lauscher" {
		t.Fatalf("ready: %+v", ev)
	}
	post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"Hallo Bot"}`)
	if ev := next(t, ch); ev.Event != "message" || ev.Room != "lobby" || ev.Message.Text != "Hallo Bot" {
		t.Fatalf("Nachricht: %+v", ev)
	}
	// Eigene und fremde Bot-Nachrichten kommen nicht an (Standard) …
	do(t, "POST", ts.URL+"/api/bot/rooms/lobby/messages", a.Token, `{"text":"ich selbst"}`)
	do(t, "POST", ts.URL+"/api/bot/rooms/lobby/messages", other.Token, `{"text":"anderer Bot"}`)
	post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"wieder Mensch"}`)
	if ev := next(t, ch); ev.Message.Text != "wieder Mensch" {
		t.Fatalf("Bot-Nachrichten hätten gefiltert werden müssen: %+v", ev)
	}
	// … außer es ist eingeschaltet (wirkt sofort, ohne Neuverbindung).
	do(t, "PATCH", ts.URL+"/api/admin/bots/"+a.Bot.ID, admin, `{"name":"Lauscher","allRooms":true,"receiveBots":true}`)
	do(t, "POST", ts.URL+"/api/bot/rooms/lobby/messages", other.Token, `{"text":"anderer Bot 2"}`)
	if ev := next(t, ch); ev.Message.Text != "anderer Bot 2" || !ev.Message.Bot {
		t.Fatalf("receiveBots: %+v", ev)
	}
	// Deaktivieren beendet den Stream.
	do(t, "PATCH", ts.URL+"/api/admin/bots/"+a.Bot.ID, admin, `{"name":"Lauscher","allRooms":true,"enabled":false}`)
	post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"noch da?"}`)
	if ev := next(t, ch); ev.Event != "revoked" {
		t.Fatalf("erwartet revoked, bekam %+v", ev)
	}
	if res := do(t, "GET", ts.URL+"/api/bot/me", a.Token, ""); res.StatusCode != 401 {
		t.Fatal("deaktivierter Bot kann sich noch anmelden")
	}
}

func TestBotWebhook(t *testing.T) {
	ts, hub := testServer(t)
	hub.EnsureDefaultRoom()
	admin := adminLogin(t, ts.URL)

	var mu sync.Mutex
	var secret string
	var got []botEvent
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-GoChat-Timestamp")
		mu.Lock()
		s := secret
		mu.Unlock()
		// neuer Name und früherer Name (für bestehende Bots) tragen dieselbe Signatur
		if r.Header.Get("X-GoChat-Signature") != Sign(s, ts, body) || r.Header.Get("X-UCNG-Signature") != Sign(s, r.Header.Get("X-UCNG-Timestamp"), body) {
			http.Error(w, "falsche Signatur", 401)
			return
		}
		if n, _ := strconv.ParseInt(ts, 10, 64); time.Since(time.Unix(n, 0)) > time.Minute {
			http.Error(w, "zu alt", 401)
			return
		}
		var ev botEvent
		json.Unmarshal(body, &ev)
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
		if ev.Event == "message" && strings.HasPrefix(ev.Message.Text, "!echo ") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"text": "Echo: " + strings.TrimPrefix(ev.Message.Text, "!echo ")})
			return
		}
		w.WriteHeader(204)
	}))
	defer hook.Close()

	b := createBot(t, ts.URL, admin, `{"name":"Echo","allRooms":true,"webhookUrl":"`+hook.URL+`"}`)
	mu.Lock()
	secret = b.Bot.WebhookSecret
	mu.Unlock()

	// Testknopf im Admin-Bereich
	res := do(t, "POST", ts.URL+"/api/admin/bots/"+b.Bot.ID+"/test", admin, "")
	var d Delivery
	json.NewDecoder(res.Body).Decode(&d)
	if d.Status != 204 || d.Error != "" || d.Event != "ping" {
		t.Fatalf("Webhook-Test: %+v", d)
	}

	user := claim(t, ts.URL, "Frager")
	post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"!echo Hallo Welt"}`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		msgs := hist(hub, "lobby")
		if len(msgs) == 2 {
			if !msgs[1].Bot || msgs[1].Text != "Echo: Hallo Welt" || msgs[1].Name != "Echo" {
				t.Fatalf("Antwort: %+v", msgs[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("keine Webhook-Antwort, Verlauf: %+v", msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 2 { // ping + die eine Nachricht – die eigene Antwort darf nicht zurückkommen
		t.Fatalf("erwartet 2 Webhook-Aufrufe, bekam %d", n)
	}

	// Neue Zugangsdaten: alter Schlüssel ungültig, Webhook mit neuem Geheimnis signiert.
	res = do(t, "POST", ts.URL+"/api/admin/bots/"+b.Bot.ID+"/renew", admin, "")
	var renewed struct{ Token, WebhookSecret string }
	json.NewDecoder(res.Body).Decode(&renewed)
	if do(t, "GET", ts.URL+"/api/bot/me", b.Token, "").StatusCode != 401 || do(t, "GET", ts.URL+"/api/bot/me", renewed.Token, "").StatusCode != 200 {
		t.Fatal("Schlüssel nach Erneuern falsch")
	}
	res = do(t, "POST", ts.URL+"/api/admin/bots/"+b.Bot.ID+"/test", admin, "")
	json.NewDecoder(res.Body).Decode(&d)
	if d.Status != 401 {
		t.Fatalf("mit altem Geheimnis hätte der Webhook ablehnen müssen: %+v", d)
	}
	mu.Lock()
	secret = renewed.WebhookSecret
	mu.Unlock()
	res = do(t, "POST", ts.URL+"/api/admin/bots/"+b.Bot.ID+"/test", admin, "")
	json.NewDecoder(res.Body).Decode(&d)
	if d.Status != 204 {
		t.Fatalf("mit neuem Geheimnis: %+v", d)
	}
	// Status steht in der Bot-Liste
	res = do(t, "GET", ts.URL+"/api/admin/bots", admin, "")
	var list []BotInfo
	json.NewDecoder(res.Body).Decode(&list)
	if len(list) != 1 || list[0].LastDelivery == nil || list[0].LastDelivery.Status != 204 {
		t.Fatalf("Bot-Liste: %+v", list)
	}
}

func TestBotRenameDeleteAndRate(t *testing.T) {
	ts, hub := testServer(t)
	hub.EnsureDefaultRoom()
	admin := adminLogin(t, ts.URL)
	b := createBot(t, ts.URL, admin, `{"name":"Alt","allRooms":true}`)
	do(t, "POST", ts.URL+"/api/bot/rooms/lobby/messages", b.Token, `{"text":"eins"}`)
	do(t, "PATCH", ts.URL+"/api/admin/bots/"+b.Bot.ID, admin, `{"name":"Neu","allRooms":true}`)
	if msgs := hist(hub, "lobby"); msgs[0].Name != "Neu" {
		t.Fatalf("alte Bot-Nachricht nicht umbenannt: %+v", msgs[0])
	}
	// Alter Name ist wieder frei.
	if res := do(t, "POST", ts.URL+"/api/session", "", `{"name":"Alt"}`); res.StatusCode != 200 {
		t.Fatalf("alter Bot-Name nicht frei: %d", res.StatusCode)
	}
	if res := do(t, "DELETE", ts.URL+"/api/admin/bots/"+b.Bot.ID, admin, ""); res.StatusCode != 204 {
		t.Fatalf("Löschen: %d", res.StatusCode)
	}
	if do(t, "GET", ts.URL+"/api/bot/me", b.Token, "").StatusCode != 401 {
		t.Fatal("gelöschter Bot kann sich anmelden")
	}

	// Ratenbegrenzung pro Bot
	s := NewServer(Config{RateLimit: 100, RateWindow: time.Second, ClaimLimit: 100, AdminPassword: "x", BotRate: 2}, NewHub(0), NewUsers(time.Hour, 10))
	s.hub.EnsureDefaultRoom()
	_, tok, _ := s.hub.Bots().Create(BotInput{Name: "Schnell", AllRooms: true})
	bot, _ := s.hub.Bots().Auth(tok)
	for i := 0; i < 2; i++ {
		if _, err := s.botSay(bot, "lobby", "x", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.botSay(bot, "lobby", "x", 0); err != errBotRate {
		t.Fatalf("erwartet Ratenbegrenzung, bekam %v", err)
	}
}

func TestBotsPersisted(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	_, tok, err := h.Bots().Create(BotInput{Name: "Merker", Rooms: []string{"lobby"}, WebhookURL: "https://example.com/hook"})
	if err != nil {
		t.Fatal(err)
	}
	flush(t, h, u, s)
	s.Close()
	h2, _, _ := openStore(t, dir)
	b, ok := h2.Bots().Auth(tok)
	if !ok || b.Name != "Merker" || b.WebhookURL != "https://example.com/hook" || !b.Allowed("lobby") || b.Allowed("x") {
		t.Fatalf("Bot nach Neustart: %+v %v", b, ok)
	}
}
