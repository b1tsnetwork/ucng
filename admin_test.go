// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testAdminPW = "test-admin-passwort"

func adminLogin(t *testing.T, base string) string {
	t.Helper()
	res := do(t, "POST", base+"/api/admin/login", "", `{"password":"`+testAdminPW+`"}`)
	if res.StatusCode != 200 {
		t.Fatalf("Admin-Login: %d", res.StatusCode)
	}
	var out struct{ Token string }
	json.NewDecoder(res.Body).Decode(&out)
	return out.Token
}

func TestAdminLogin(t *testing.T) {
	ts, _ := testServer(t)
	if res := do(t, "POST", ts.URL+"/api/admin/login", "", `{"password":"falsch"}`); res.StatusCode != 401 {
		t.Fatalf("falsches Passwort: %d", res.StatusCode)
	}
	tok := adminLogin(t, ts.URL)
	if res := do(t, "GET", ts.URL+"/api/admin/me", tok, ""); res.StatusCode != 200 {
		t.Fatalf("me: %d", res.StatusCode)
	}
	// Ein Nutzer-Token ist kein Admin-Token.
	user := claim(t, ts.URL, "Mallory")
	for _, c := range []struct{ m, p, b string }{
		{"GET", "/api/admin/me", ""},
		{"PUT", "/api/admin/settings", `{"title":"x","allowUserRooms":true}`},
		{"POST", "/api/admin/rooms", `{"name":"x"}`},
		{"DELETE", "/api/admin/rooms/lobby", ""},
		{"DELETE", "/api/admin/rooms/lobby/messages/1", ""},
	} {
		if res := do(t, c.m, ts.URL+c.p, user, c.b); res.StatusCode != 401 {
			t.Errorf("%s %s ohne Admin: %d", c.m, c.p, res.StatusCode)
		}
	}
	do(t, "POST", ts.URL+"/api/admin/logout", tok, "")
	if res := do(t, "GET", ts.URL+"/api/admin/me", tok, ""); res.StatusCode != 401 {
		t.Fatal("Token nach Logout noch gültig")
	}
}

func TestAdminNoCORS(t *testing.T) {
	ts, _ := testServer(t)
	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/api/admin/login", nil)
	req.Header.Set("Origin", "https://example.com")
	res, _ := http.DefaultClient.Do(req)
	if res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("Admin-API darf nicht per CORS freigegeben sein")
	}
}

func TestSettingsAndUserRooms(t *testing.T) {
	ts, _ := testServer(t)
	admin := adminLogin(t, ts.URL)
	user := claim(t, ts.URL, "Anna")

	res := do(t, "PUT", ts.URL+"/api/admin/settings", admin, `{"title":"  Mein   Chat ","allowUserRooms":false}`)
	var st Settings
	json.NewDecoder(res.Body).Decode(&st)
	if res.StatusCode != 200 || st.Title != "Mein Chat" || st.AllowUserRooms {
		t.Fatalf("Settings: %d %+v", res.StatusCode, st)
	}
	res, _ = http.Get(ts.URL + "/api/settings")
	json.NewDecoder(res.Body).Decode(&st)
	if st.Title != "Mein Chat" {
		t.Fatalf("öffentliche Settings: %+v", st)
	}

	// Unbekannter Raum: weder schreiben noch verbinden noch Verlauf.
	if res := post(t, ts.URL+"/api/rooms/neu/messages", user, `{"text":"x"}`); res.StatusCode != 404 {
		t.Fatalf("Post in unbekannten Raum: %d", res.StatusCode)
	}
	if res, _ := http.Get(ts.URL + "/api/rooms/neu/events"); res.StatusCode != 404 {
		t.Fatalf("Events unbekannter Raum: %d", res.StatusCode)
	}
	if res, _ := http.Get(ts.URL + "/api/rooms/neu/messages"); res.StatusCode != 404 {
		t.Fatalf("Verlauf unbekannter Raum: %d", res.StatusCode)
	}

	// Admin legt den Raum an – jetzt geht es.
	if res := do(t, "POST", ts.URL+"/api/admin/rooms", admin, `{"name":"Neu","description":"Ein neuer Raum"}`); res.StatusCode != 201 {
		t.Fatalf("Raum anlegen: %d", res.StatusCode)
	}
	if res := do(t, "POST", ts.URL+"/api/admin/rooms", admin, `{"name":"neu"}`); res.StatusCode != 409 {
		t.Fatalf("doppelter Raum: %d", res.StatusCode)
	}
	if res := post(t, ts.URL+"/api/rooms/neu/messages", user, `{"text":"hallo"}`); res.StatusCode != 201 {
		t.Fatalf("Post in angelegten Raum: %d", res.StatusCode)
	}
	if res := do(t, "PUT", ts.URL+"/api/admin/settings", admin, `{"title":"","allowUserRooms":true}`); res.StatusCode != 400 {
		t.Fatalf("leerer Titel: %d", res.StatusCode)
	}
}

func TestRoomRenameAndDelete(t *testing.T) {
	ts, hub := testServer(t)
	admin := adminLogin(t, ts.URL)
	user := claim(t, ts.URL, "Bob")
	post(t, ts.URL+"/api/rooms/alt/messages", user, `{"text":"vorher"}`)

	// Verbundener Client bekommt die Umbenennung mit und der Stream endet.
	res, _ := http.Get(ts.URL + "/api/rooms/alt/events")
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)

	r := do(t, "PATCH", ts.URL+"/api/admin/rooms/alt", admin, `{"name":"Neu-Name","description":"Beschreibung"}`)
	if r.StatusCode != 200 {
		t.Fatalf("Umbenennen: %d", r.StatusCode)
	}
	select {
	case out := <-done:
		if !strings.Contains(out, `"type":"room_renamed"`) || !strings.Contains(out, `"room":"neu-name"`) {
			t.Fatalf("kein room_renamed-Event: %s", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream wurde nach Umbenennen nicht beendet")
	}

	msgs := hist(hub, "neu-name")
	if len(msgs) != 1 || msgs[0].Room != "neu-name" {
		t.Fatalf("Verlauf nicht mit umgezogen: %+v", msgs)
	}
	for _, ri := range hub.Rooms() {
		if ri.Name == "alt" {
			t.Fatal("alter Raum existiert noch")
		}
		if ri.Name == "neu-name" && ri.Description != "Beschreibung" {
			t.Fatal("Beschreibung nicht gesetzt")
		}
	}

	if r := do(t, "DELETE", ts.URL+"/api/admin/rooms/neu-name", admin, ""); r.StatusCode != 204 {
		t.Fatalf("Löschen: %d", r.StatusCode)
	}
	if r := do(t, "DELETE", ts.URL+"/api/admin/rooms/neu-name", admin, ""); r.StatusCode != 404 {
		t.Fatalf("doppelt löschen: %d", r.StatusCode)
	}
}

func TestModerationDelete(t *testing.T) {
	ts, hub := testServer(t)
	admin := adminLogin(t, ts.URL)
	user := claim(t, ts.URL, "Troll")
	res := post(t, ts.URL+"/api/rooms/lobby/messages", user, `{"text":"böse Nachricht"}`)
	var m Message
	json.NewDecoder(res.Body).Decode(&m)

	r := do(t, "DELETE", ts.URL+"/api/admin/rooms/lobby/messages/"+itoa(m.ID), admin, "")
	if r.StatusCode != 200 {
		t.Fatalf("Moderation: %d", r.StatusCode)
	}
	msgs := hist(hub, "lobby")
	if len(msgs) != 1 || !msgs[0].Deleted || msgs[0].Text != "" || msgs[0].Name != "Troll" {
		t.Fatalf("Nachricht nicht als gelöscht markiert: %+v", msgs)
	}

	// Öffentliche Wege dürfen den Originaltext nicht verraten.
	for _, path := range []string{"/api/rooms/lobby/messages"} {
		res, _ := http.Get(ts.URL + path)
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "böse Nachricht") || strings.Contains(string(body), "deletedAt") {
			t.Fatalf("%s verrät Original: %s", path, body)
		}
	}
	sse, _ := http.Get(ts.URL + "/api/rooms/lobby/events")
	buf := make([]byte, 4096)
	n, _ := sse.Body.Read(buf)
	sse.Body.Close()
	if strings.Contains(string(buf[:n]), "böse Nachricht") {
		t.Fatalf("SSE verrät Original: %s", buf[:n])
	}

	// Admins sehen das Original samt Löschzeitpunkt …
	if res := do(t, "GET", ts.URL+"/api/admin/rooms/lobby/messages", user, ""); res.StatusCode != 401 {
		t.Fatalf("Admin-Verlauf ohne Admin: %d", res.StatusCode)
	}
	res = do(t, "GET", ts.URL+"/api/admin/rooms/lobby/messages", admin, "")
	var full []Message
	json.NewDecoder(res.Body).Decode(&full)
	if len(full) != 1 || full[0].Original != "böse Nachricht" || full[0].DeletedAt == nil {
		t.Fatalf("Admin-Verlauf: %+v", full)
	}

	// … und können die Löschung rückgängig machen.
	r = do(t, "POST", ts.URL+"/api/admin/rooms/lobby/messages/"+itoa(m.ID)+"/restore", admin, "")
	if r.StatusCode != 200 {
		t.Fatalf("Wiederherstellen: %d", r.StatusCode)
	}
	msgs = hist(hub, "lobby")
	if msgs[0].Deleted || msgs[0].Text != "böse Nachricht" {
		t.Fatalf("nicht wiederhergestellt: %+v", msgs[0])
	}
	if r := do(t, "DELETE", ts.URL+"/api/admin/rooms/lobby/messages/999", admin, ""); r.StatusCode != 404 {
		t.Fatalf("unbekannte Nachricht: %d", r.StatusCode)
	}
}

func TestPersistAdminState(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.CreateRoom("leer", "Ohne Nachrichten")
	h.SetSettings(Settings{Title: "Gespeichert", AllowUserRooms: false})
	h.Post("leer", "A", "x")
	m, _ := h.Post("leer", "A", "y")
	h.DeleteMessage("leer", m.ID)
	h.CreateRoom("zweiter", "")
	flush(t, h, u, s)
	s.Close()

	h2, _, _ := openStore(t, dir)
	if st := h2.Settings(); st.Title != "Gespeichert" || st.AllowUserRooms {
		t.Fatalf("Settings: %+v", st)
	}
	rooms := h2.Rooms()
	if len(rooms) != 2 || rooms[0].Name != "leer" || rooms[0].Description != "Ohne Nachrichten" {
		t.Fatalf("Räume: %+v", rooms)
	}
	msgs := adminHist(h2, "leer")
	if len(msgs) != 2 || !msgs[1].Deleted || msgs[1].Original != "y" || msgs[1].DeletedAt == nil {
		t.Fatalf("gelöschte Nachricht nicht vollständig gespeichert: %+v", msgs)
	}
}
func TestRoomNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"lobby": true, "café": true, "über-uns": true, "team_2": true,
		"mit leerzeichen": false, "": false, "a/b": false, strings.Repeat("x", 33): false,
	} {
		if ValidRoom(NormRoom(name)) != ok {
			t.Errorf("%q: erwartet %v", name, ok)
		}
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// Startkanal: nur vorhandene Kanäle; Umbenennen zieht mit, Löschen setzt auf automatisch.
func TestStartRoom(t *testing.T) {
	ts, h := testServer(t)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"willkommen"}`), 201, "anlegen").Body.Close()
	put := func(start string, want int) {
		t.Helper()
		st := h.Settings()
		st.StartRoom = start
		b, _ := json.Marshal(st)
		expect(t, do(t, "PUT", ts.URL+"/api/admin/settings", adm, string(b)), want, "Startkanal "+start).Body.Close()
	}
	put("gibtsnicht", 400)
	put("#Willkommen", 200)
	if h.Settings().StartRoom != "willkommen" {
		t.Fatalf("gespeichert: %q", h.Settings().StartRoom)
	}
	if s := jmap(t, do(t, "GET", ts.URL+"/api/settings", "", "")); s["startRoom"] != "willkommen" {
		t.Fatalf("öffentlich: %v", s["startRoom"])
	}
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/willkommen", adm, `{"name":"hallo","description":""}`), 200, "umbenennen").Body.Close()
	if h.Settings().StartRoom != "hallo" {
		t.Fatalf("nach Umbenennen: %q", h.Settings().StartRoom)
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/rooms/hallo", adm, ""), 204, "löschen").Body.Close()
	if h.Settings().StartRoom != "" {
		t.Fatalf("nach Löschen: %q", h.Settings().StartRoom)
	}
}
