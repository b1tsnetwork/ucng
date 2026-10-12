// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"testing"
)

func myDMs(t *testing.T, base, tok string) []DMInfo {
	t.Helper()
	return decode[struct{ DMs []DMInfo }](t, do(t, "GET", base+"/api/dms", tok, "")).DMs
}

func TestDMBlock(t *testing.T) {
	ts, _ := testServer(t)
	a, b := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	aID, bID := sess(t, ts.URL, a).ID, sess(t, ts.URL, b).ID
	dm := decode[DMInfo](t, expect(t, post(t, ts.URL+"/api/dms", b, `{"userId":"`+aID+`"}`), 200, "DM öffnen"))
	msgURL := ts.URL + "/api/rooms/" + dm.Room + "/messages"
	m := decode[Message](t, expect(t, post(t, msgURL, b, `{"text":"Hallo Anna"}`), 201, "schreiben"))

	// Anna blockiert Ben.
	expect(t, post(t, ts.URL+"/api/blocks", a, `{"userId":"`+aID+`","block":true}`), 400, "sich selbst").Body.Close()
	expect(t, post(t, ts.URL+"/api/blocks", a, `{"userId":"u_gibtsnicht","block":true}`), 404, "unbekannt").Body.Close()
	expect(t, post(t, ts.URL+"/api/blocks", a, `{"userId":"`+bID+`","block":true}`), 200, "blockieren").Body.Close()
	if s := sess(t, ts.URL, a); len(s.Blocked) != 1 || s.Blocked[0] != bID {
		t.Fatalf("Sitzung: %+v", s.Blocked)
	}
	expect(t, post(t, msgURL, b, `{"text":"Hallo??"}`), 403, "Ben schreibt").Body.Close()
	expect(t, post(t, msgURL, a, `{"text":"Tschüss"}`), 403, "Anna schreibt").Body.Close()
	expect(t, post(t, ts.URL+"/api/dms", b, `{"userId":"`+aID+`"}`), 403, "Ben öffnet").Body.Close()
	expect(t, post(t, msgURL+"/"+itoa(m.ID)+"/react", b, `{"emoji":"👍"}`), 403, "Ben reagiert").Body.Close()
	expect(t, do(t, "GET", msgURL, a, ""), 200, "Anna liest weiter").Body.Close()
	if l := myDMs(t, ts.URL, a); len(l) != 1 || !l[0].Blocked || !l[0].BlockedByMe {
		t.Fatalf("Annas Liste: %+v", l)
	}
	if l := myDMs(t, ts.URL, b); len(l) != 1 || !l[0].Blocked || l[0].BlockedByMe {
		t.Fatalf("Bens Liste: %+v", l)
	}
	// Erwähnungen blockierter Personen zählen nicht als ungelesen.
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", b, `{"text":"@Anna hallo"}`), 201, "Lobby").Body.Close()
	if u := decode[map[string]UnreadInfo](t, post(t, ts.URL+"/api/unread", a, `{"since":{"lobby":0}}`)); u["lobby"].Unread != 0 {
		t.Fatalf("ungelesen: %+v", u)
	}
	bl := decode[map[string][]blockedInfo](t, do(t, "GET", ts.URL+"/api/blocks", a, ""))["blocked"]
	if len(bl) != 1 || bl[0].Name != "Ben" {
		t.Fatalf("Liste: %+v", bl)
	}

	// Aufheben
	expect(t, post(t, ts.URL+"/api/blocks", a, `{"userId":"`+bID+`","block":false}`), 200, "aufheben").Body.Close()
	expect(t, post(t, msgURL, b, `{"text":"Wieder da"}`), 201, "Ben schreibt wieder").Body.Close()
	if l := myDMs(t, ts.URL, a); len(l) != 1 || l[0].Blocked {
		t.Fatalf("nach Aufheben: %+v", l)
	}
	// Ben blockiert Anna – auch Anna kann dann nicht schreiben.
	expect(t, post(t, ts.URL+"/api/blocks", b, `{"userId":"`+aID+`","block":true}`), 200, "Ben blockiert").Body.Close()
	expect(t, post(t, msgURL, a, `{"text":"Ben?"}`), 403, "Anna an Ben").Body.Close()
}

// Reaktionen prüfen den Zugriff (früher konnten Dritte so Nachrichten fremder DMs abrufen).
func TestReactNeedsAccess(t *testing.T) {
	ts, _ := testServer(t)
	a, b, c := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben"), claim(t, ts.URL, "Carla")
	bID := sess(t, ts.URL, b).ID
	dm := decode[DMInfo](t, expect(t, post(t, ts.URL+"/api/dms", a, `{"userId":"`+bID+`"}`), 200, "DM"))
	m := decode[Message](t, expect(t, post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages", a, `{"text":"geheim"}`), 201, "schreiben"))
	r := expect(t, post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(m.ID)+"/react", c, `{"emoji":"👍"}`), 404, "Dritte")
	r.Body.Close()
	expect(t, post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(m.ID)+"/react", b, `{"emoji":"👍"}`), 200, "Ben").Body.Close()
}

func TestDMClear(t *testing.T) {
	ts, h := testServer(t)
	a, b := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	bID := sess(t, ts.URL, b).ID
	dm := decode[DMInfo](t, expect(t, post(t, ts.URL+"/api/dms", a, `{"userId":"`+bID+`"}`), 200, "DM"))
	msgURL := ts.URL + "/api/rooms/" + dm.Room + "/messages"
	expect(t, post(t, msgURL, a, `{"text":"eins"}`), 201, "1").Body.Close()
	expect(t, post(t, msgURL, b, `{"text":"zwei"}`), 201, "2").Body.Close()
	c := claim(t, ts.URL, "Carla")
	expect(t, do(t, "DELETE", ts.URL+"/api/dms/"+dm.Room, c, ""), 404, "Dritte").Body.Close()

	// Anna löscht für sich.
	expect(t, do(t, "DELETE", ts.URL+"/api/dms/"+dm.Room, a, ""), 204, "Anna löscht").Body.Close()
	if msgs := decode[[]Message](t, do(t, "GET", msgURL, a, "")); len(msgs) != 0 {
		t.Fatalf("Anna sieht noch %d", len(msgs))
	}
	if l := myDMs(t, ts.URL, a); len(l) != 0 {
		t.Fatalf("Annas Liste: %+v", l)
	}
	if msgs := decode[[]Message](t, do(t, "GET", msgURL, b, "")); len(msgs) != 2 {
		t.Fatalf("Ben sieht %d", len(msgs))
	}
	if hits := decode[[]SearchHit](t, do(t, "GET", ts.URL+"/api/rooms/"+dm.Room+"/search?q=eins", a, "")); len(hits) != 0 {
		t.Fatalf("Suche: %+v", hits)
	}
	// Neue Nachricht: Unterhaltung erscheint wieder – nur mit dem Neuen.
	expect(t, post(t, msgURL, b, `{"text":"drei"}`), 201, "3").Body.Close()
	if l := myDMs(t, ts.URL, a); len(l) != 1 || l[0].Messages != 1 {
		t.Fatalf("wieder da: %+v", l)
	}
	if msgs := decode[[]Message](t, do(t, "GET", msgURL, a, "")); len(msgs) != 1 || msgs[0].Text != "drei" {
		t.Fatalf("Anna: %+v", msgs)
	}
	// Ben löscht – was beide gelöscht haben, ist endgültig weg.
	expect(t, do(t, "DELETE", ts.URL+"/api/dms/"+dm.Room, b, ""), 204, "Ben löscht").Body.Close()
	if msgs, _, _ := h.AdminHistory(dm.Room, 0, 100); len(msgs) != 1 || msgs[0].Text != "drei" {
		t.Fatalf("nach beiden: %+v", msgs)
	}
	// Eigene Nachrichten auch beim Gegenüber löschen.
	expect(t, post(t, msgURL, a, `{"text":"vier"}`), 201, "4").Body.Close()
	expect(t, do(t, "DELETE", ts.URL+"/api/dms/"+dm.Room+"?own=1", a, ""), 204, "Anna löscht alles").Body.Close()
	msgs := decode[[]Message](t, do(t, "GET", msgURL, b, ""))
	if len(msgs) != 1 || !msgs[0].Deleted || msgs[0].Text != "" {
		t.Fatalf("bei Ben: %+v", msgs)
	}
	_ = http.StatusOK
}

func TestBlocksPersist(t *testing.T) {
	dir := t.TempDir()
	h, u, st := openStore(t, dir)
	a, _ := u.Claim("Ada", "")
	b, _ := u.Claim("Ben", "")
	room := h.OpenDM(a.ID, b.ID)
	if _, err := h.SendFull(room, Poster{ID: b.ID, Name: "Ben"}, SendInput{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h.Blocks().Set(a.ID, b.ID, true)
	h.ClearDM(room, a.ID, false)
	flush(t, h, u, st)
	st.Close()

	h2, _, _ := openStore(t, dir)
	if !h2.Blocks().Has(a.ID, b.ID) || !h2.IsDMBlocked(room) {
		t.Fatal("Blockierung nicht gespeichert")
	}
	if f := h2.DMFloor(room, a.ID); f == 0 {
		t.Fatal("Löschstand nicht gespeichert")
	}
}
