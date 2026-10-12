// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testServerWith startet einen Testserver mit angepasster Konfiguration.
func testServerWith(t *testing.T, mod func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	cfg := Config{MaxRooms: 50, RateLimit: 1000, RateWindow: time.Second, ClaimLimit: 1000, AdminPassword: testAdminPW, BotRate: 1000}
	if mod != nil {
		mod(&cfg)
	}
	hub := NewHub(cfg.MaxRooms)
	hub.EnsureDefaultRoom()
	srv := NewServer(cfg, hub, NewUsers(time.Hour, 1000))
	ctx, cancel := context.WithCancel(context.Background())
	go srv.webhooks.run(ctx)
	t.Cleanup(cancel)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() { close(srv.done); ts.Close() })
	return ts, srv
}

func sess(t *testing.T, base, tok string) sessionInfo {
	t.Helper()
	res := do(t, "GET", base+"/api/session", tok, "")
	var s sessionInfo
	json.NewDecoder(res.Body).Decode(&s)
	return s
}

func decode[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	var v T
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	return v
}

func errText(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(b)
}

func expect(t *testing.T, res *http.Response, code int, what string) *http.Response {
	t.Helper()
	if res.StatusCode != code {
		t.Fatalf("%s: Status %d statt %d (%s)", what, res.StatusCode, code, errText(res))
	}
	return res
}

func loginAs(t *testing.T, base, user, pw string) string {
	t.Helper()
	res := expect(t, do(t, "POST", base+"/api/admin/login", "", `{"username":"`+user+`","password":"`+pw+`"}`), 200, "Login "+user)
	return decode[struct{ Token string }](t, res).Token
}

func TestModeratorAccountsAndAudit(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "POST", ts.URL+"/api/admin/accounts", adm, `{"name":"mia","display":"Mia","role":"moderator","password":"kurz"}`), 400, "zu kurzes Passwort")
	expect(t, do(t, "POST", ts.URL+"/api/admin/accounts", adm, `{"name":"mia","display":"Mia","role":"moderator","password":"mia-passwort-123"}`), 201, "Konto anlegen")
	expect(t, do(t, "POST", ts.URL+"/api/admin/accounts", adm, `{"name":"Mia","role":"moderator","password":"mia-passwort-123"}`), 409, "doppelt")
	if res := do(t, "POST", ts.URL+"/api/admin/login", "", `{"username":"mia","password":"falsch-falsch"}`); res.StatusCode != 401 {
		t.Fatal("falsches Passwort akzeptiert")
	}
	mod := loginAs(t, ts.URL, "mia", "mia-passwort-123")

	tok := claim(t, ts.URL, "Rudi")
	m := decode[Message](t, expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"Hallo zusammen"}`), 201, "post"))
	// Moderatorin darf moderieren …
	del := decode[Message](t, expect(t, do(t, "DELETE", ts.URL+"/api/admin/rooms/lobby/messages/"+itoa64(m.ID), mod, ""), 200, "mod löscht"))
	if del.ModBy != "Mia" || del.DeletedBy != "mod" {
		t.Fatalf("ModBy: %+v", del)
	}
	// … aber keine Einstellungen, Kanäle, Konten, Bots
	for _, c := range [][3]string{
		{"PUT", "/api/admin/settings", `{"title":"x"}`},
		{"POST", "/api/admin/rooms", `{"name":"neu"}`},
		{"GET", "/api/admin/accounts", ""},
		{"GET", "/api/admin/bots", ""},
		{"POST", "/api/admin/rooms/lobby/messages/1/purge", ""},
	} {
		expect(t, do(t, c[0], ts.URL+c[1], mod, c[2]), 403, "Moderator "+c[0]+" "+c[1])
	}
	me := decode[struct {
		Admin bool
		Actor Actor
	}](t, do(t, "GET", ts.URL+"/api/admin/me", mod, ""))
	if me.Admin || me.Actor.Role != RoleModerator {
		t.Fatalf("me: %+v", me)
	}

	// Protokoll
	log := decode[struct{ Entries []AuditEntry }](t, do(t, "GET", ts.URL+"/api/admin/audit", adm, ""))
	found := false
	for _, e := range log.Entries {
		if e.Action == "message.delete" && e.Actor == "mia" && e.UserName == "Rudi" && e.Detail == "Hallo zusammen" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Löschung fehlt im Protokoll: %+v", log.Entries)
	}
	filtered := decode[struct{ Entries []AuditEntry }](t, do(t, "GET", ts.URL+"/api/admin/audit?q=account", adm, ""))
	if len(filtered.Entries) != 1 || filtered.Entries[0].Action != "account.create" {
		t.Fatalf("Filter: %+v", filtered.Entries)
	}

	// Konto deaktivieren beendet die Sitzung
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/accounts/mia", adm, `{"display":"Mia","role":"moderator","disabled":true}`), 200, "deaktivieren")
	expect(t, do(t, "GET", ts.URL+"/api/admin/me", mod, ""), 401, "Sitzung nach Deaktivieren")
	expect(t, do(t, "POST", ts.URL+"/api/admin/login", "", `{"username":"mia","password":"mia-passwort-123"}`), 401, "deaktiviertes Konto")
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/accounts/admin", adm, ""), 403, "Haupt-Admin löschen")
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/accounts/mia", adm, ""), 204, "löschen")
}

func itoa64(i int64) string { return strconv.FormatInt(i, 10) }

func tarNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = true
	}
}

func TestMuteBanAndIP(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	tok := claim(t, ts.URL, "Troll")
	id := sess(t, ts.URL, tok).ID
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"vorher"}`), 201, "vorher")

	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/sanction", adm, `{"kind":"mute","minutes":0}`), 400, "mute ohne Dauer")
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/sanction", adm, `{"kind":"mute","minutes":30,"reason":"Spam"}`), 200, "mute")
	res := post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"darf ich?"}`)
	if body := errText(res); res.StatusCode != 403 || !strings.Contains(body, "stummgeschaltet bis") || !strings.Contains(body, "Spam") {
		t.Fatalf("stumm: %d %s", res.StatusCode, body)
	}
	if s := sess(t, ts.URL, tok); s.Sanction == nil || s.Sanction.Kind != "mute" {
		t.Fatalf("Sitzung zeigt Sperre nicht: %+v", s)
	}
	expect(t, do(t, "POST", ts.URL+"/api/rooms/lobby/messages/1/react", tok, `{"emoji":"👍"}`), 403, "Reaktion trotz Stummschaltung")
	list := decode[[]Sanction](t, do(t, "GET", ts.URL+"/api/admin/sanctions", adm, ""))
	if len(list) != 1 || list[0].Name != "Troll" || list[0].By != "Admin" {
		t.Fatalf("Liste: %+v", list)
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/users/"+id+"/sanction", adm, ""), 204, "aufheben")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"wieder da"}`), 201, "nach Aufheben")

	// Dauerhafte Sperre inkl. Verbindung: auch ein neuer Name geht nicht mehr.
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/sanction", adm, `{"kind":"ban","minutes":0,"ip":true}`), 200, "ban")
	res = post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"hallo?"}`)
	if body := errText(res); res.StatusCode != 403 || !strings.Contains(body, "ausgeschlossen") {
		t.Fatalf("ban: %d %s", res.StatusCode, body)
	}
	res = do(t, "POST", ts.URL+"/api/session", "", `{"name":"NeuerName"}`)
	if res.StatusCode != 403 {
		t.Fatalf("neuer Name trotz IP-Sperre: %d", res.StatusCode)
	}
	u := decode[adminUser](t, do(t, "GET", ts.URL+"/api/admin/users/"+id, adm, ""))
	if u.Sanction == nil || u.Sanction.IPHash != "ja" || u.Sanction.Until != nil {
		t.Fatalf("Admin-Sicht: %+v", u.Sanction)
	}
}

func TestProtectedNamesAndTeam(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	for _, n := range []string{"Admin", "admin123", "Der Administrator", "Support-Team", "M0derator"} {
		res := do(t, "POST", ts.URL+"/api/session", "", `{"name":"`+n+`"}`)
		if res.StatusCode != 409 || !strings.Contains(errText(res), "geschützt") {
			t.Errorf("%q wurde nicht abgelehnt (%d)", n, res.StatusCode)
		}
	}
	tok := claim(t, ts.URL, "Anna")
	id := sess(t, ts.URL, tok).ID
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/team", tok, `{"team":true}`), 401, "Team ohne Admin")
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/team", adm, `{"team":true}`), 200, "Team")
	if !sess(t, ts.URL, tok).Team {
		t.Fatal("Team-Kennzeichen fehlt in der Sitzung")
	}
	// Team darf einen geschützten Namen tragen
	res := expect(t, do(t, "POST", ts.URL+"/api/session", tok, `{"name":"Support Anna"}`), 200, "Team-Umbenennung")
	res.Body.Close()
	m := decode[Message](t, expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"Wir helfen gern"}`), 201, "post"))
	if !m.Team || m.Name != "Support Anna" {
		t.Fatalf("Team-Abzeichen: %+v", m)
	}
	// Team-Mitglieder kann man nicht sperren
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/sanction", adm, `{"kind":"mute","minutes":5}`), 403, "Team sperren")
	// Abzeichen entfernen → alte Nachrichten verlieren es
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/team", adm, `{"team":false}`), 200, "Team weg")
	if hist(srv.hub, "lobby")[0].Team {
		t.Fatal("Abzeichen an alter Nachricht nicht entfernt")
	}
	// geschützte Namen anpassen
	expect(t, do(t, "PUT", ts.URL+"/api/admin/protected-names", adm, `{"patterns":["chef*"]}`), 200, "Liste speichern")
	if res := do(t, "POST", ts.URL+"/api/session", "", `{"name":"Chefin"}`); res.StatusCode != 409 {
		t.Fatalf("Chefin: %d", res.StatusCode)
	}
	claim(t, ts.URL, "Admin") // nicht mehr geschützt
}

func TestSpamBrakes(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"ruhig","slowMode":30,"links":"known"}`), 201, "Kanal")
	tok := claim(t, ts.URL, "Eilig")
	url := ts.URL + "/api/rooms/ruhig/messages"
	expect(t, post(t, url, tok, `{"text":"eins"}`), 201, "erste")
	res := post(t, url, tok, `{"text":"zwei"}`)
	if body := errText(res); res.StatusCode != 429 || !strings.Contains(body, "Langsam-Modus") {
		t.Fatalf("Langsam-Modus: %d %s", res.StatusCode, body)
	}
	// Links erst nach 24 Stunden
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/ruhig", adm, `{"description":"","slowMode":0}`), 200, "Langsam aus")
	if ri := srv.hub.Rooms(); ri[len(ri)-1].Links != LinksKnown {
		t.Fatalf("Link-Regel verloren: %+v", ri)
	}
	res = post(t, url, tok, `{"text":"schau mal https://example.com"}`)
	if res.StatusCode != 403 || !strings.Contains(errText(res), "24 Stunden") {
		t.Fatal("Link von neuem Namen erlaubt")
	}
	// Wiederholung
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"ok"}`), 201, "kurz 1.")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"OK"}`), 201, "kurz 2.")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"ok"}`), 429, "kurz 3.")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"Kauft  Sachen bei uns, super billig!!"}`), 201, "1.")
	res = post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"kauft sachen bei uns, super billig!!"}`)
	if res.StatusCode != 429 || !strings.Contains(errText(res), "gerade schon") {
		t.Fatal("Wiederholung nicht erkannt")
	}
	// Team ist ausgenommen
	team := claim(t, ts.URL, "Helga")
	do(t, "POST", ts.URL+"/api/admin/users/"+sess(t, ts.URL, team).ID+"/team", adm, `{"team":true}`).Body.Close()
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/ruhig", adm, `{"description":"","slowMode":60,"links":"team"}`), 200, "Team-Links")
	expect(t, post(t, url, team, `{"text":"https://hilfe.example.com"}`), 201, "Team-Link")
	expect(t, post(t, url, team, `{"text":"https://hilfe.example.com"}`), 201, "Team ohne Bremse")
	res = post(t, url, tok, `{"text":"www.example.com"}`)
	if res.StatusCode != 403 || !strings.Contains(errText(res), "nur für das Team") {
		t.Fatal("Link-Regel team")
	}
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/ruhig", adm, `{"description":"","links":"irgendwas"}`), 400, "ungültige Regel")
}

func TestRepliesReactionsEditDelete(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	a, b := claim(t, ts.URL, "Ada"), claim(t, ts.URL, "Bert")
	base := ts.URL + "/api/rooms/lobby/messages"
	q := decode[Message](t, expect(t, post(t, base, a, `{"text":"Wer kommt mit zum Essen heute Abend um sieben?"}`), 201, "Frage"))
	ans := decode[Message](t, expect(t, post(t, base, b, `{"text":"Ich!","replyTo":`+itoa64(q.ID)+`}`), 201, "Antwort"))
	if ans.Reply == nil || ans.Reply.Name != "Ada" || !strings.HasPrefix(ans.Reply.Text, "Wer kommt") {
		t.Fatalf("Antwort-Vorschau: %+v", ans.Reply)
	}
	expect(t, post(t, base, b, `{"text":"x","replyTo":999999}`), 404, "Antwort auf Unbekanntes")

	// Reaktionen umschalten
	r1 := decode[Message](t, expect(t, do(t, "POST", base+"/"+itoa64(q.ID)+"/react", b, `{"emoji":"👍"}`), 200, "react"))
	r1 = decode[Message](t, expect(t, do(t, "POST", base+"/"+itoa64(q.ID)+"/react", a, `{"emoji":"👍"}`), 200, "react"))
	if len(r1.Reactions) != 1 || len(r1.Reactions[0].Users) != 2 {
		t.Fatalf("Reaktionen: %+v", r1.Reactions)
	}
	r1 = decode[Message](t, expect(t, do(t, "POST", base+"/"+itoa64(q.ID)+"/react", b, `{"emoji":"👍"}`), 200, "unreact"))
	if len(r1.Reactions) != 1 || r1.Reactions[0].Users[0].Name != "Ada" {
		t.Fatalf("Umschalten: %+v", r1.Reactions)
	}
	expect(t, do(t, "POST", base+"/"+itoa64(q.ID)+"/react", b, `{"emoji":"🤡"}`), 400, "fremdes Emoji")

	// Bearbeiten
	expect(t, do(t, "PATCH", base+"/"+itoa64(q.ID), b, `{"text":"gehackt"}`), 403, "fremde Nachricht bearbeiten")
	ed := decode[Message](t, expect(t, do(t, "PATCH", base+"/"+itoa64(q.ID), a, `{"text":"Wer kommt mit zum Essen?"}`), 200, "bearbeiten"))
	if ed.EditedAt == nil || ed.Text != "Wer kommt mit zum Essen?" || ed.Edits != nil {
		t.Fatalf("bearbeitet: %+v", ed)
	}
	am := adminHist(srv.hub, "lobby")[0]
	if len(am.Edits) != 1 || !strings.HasPrefix(am.Edits[0], "Wer kommt mit zum Essen heute") {
		t.Fatalf("frühere Fassung fehlt für Admins: %+v", am.Edits)
	}
	// Bearbeitungsfenster abgelaufen
	srv.hub.mu.Lock()
	srv.hub.rooms["lobby"].history[0].Time = time.Now().Add(-20 * time.Minute)
	srv.hub.mu.Unlock()
	expect(t, do(t, "PATCH", base+"/"+itoa64(q.ID), a, `{"text":"zu spät"}`), 403, "nach 15 Minuten")

	// Selbst löschen entfernt den Text auch für Admins
	expect(t, do(t, "DELETE", base+"/"+itoa64(ans.ID), a, ""), 403, "fremde löschen")
	gone := decode[Message](t, expect(t, do(t, "DELETE", base+"/"+itoa64(ans.ID), b, ""), 200, "selbst löschen"))
	if !gone.Deleted || gone.DeletedBy != "self" || gone.Text != "" {
		t.Fatalf("gelöscht: %+v", gone)
	}
	if am := adminHist(srv.hub, "lobby")[1]; am.Original != "" || am.Text != "" {
		t.Fatalf("Admin sieht selbst gelöschten Text: %+v", am)
	}
	if _, err := srv.hub.RestoreMessage("lobby", ans.ID); err != nil {
		t.Fatal(err)
	}
	if hist(srv.hub, "lobby")[1].Deleted == false {
		t.Fatal("selbst gelöschte Nachricht wurde wiederhergestellt")
	}
	// Antwort auf gelöschte Nachricht zeigt „gelöscht“
	r2 := decode[Message](t, expect(t, post(t, base, a, `{"text":"schade","replyTo":`+itoa64(ans.ID)+`}`), 201, "Antwort auf Gelöschte"))
	if r2.Reply == nil || !r2.Reply.Deleted || r2.Reply.Text != "" {
		t.Fatalf("Vorschau gelöschter Nachricht: %+v", r2.Reply)
	}
}

func TestEditTriggersAutomod(t *testing.T) {
	h := NewHub(0)
	h.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"arsch*"}})
	p := Poster{ID: "u1", Name: "Max"}
	m, _ := h.Send("lobby", p, "Hallo", 0)
	out, err := h.EditOwn("lobby", m.ID, p, "Du Arschloch")
	if err != nil || !out.Deleted || out.DeletedBy != "automod" {
		t.Fatalf("%v %+v", err, out)
	}
	if len(h.AutomodQueue()) != 1 {
		t.Fatal("bearbeitete Nachricht nicht in der Prüfliste")
	}
	if _, err := h.EditOwn("lobby", m.ID, p, "Hallo nochmal"); err != ErrAwaitsReview {
		t.Fatalf("Bearbeiten während der Prüfung: %v", err)
	}
}

func TestPurgeAndRetentionPersist(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	p1, p2 := Poster{ID: "a1", Name: "Alice"}, Poster{ID: "b2", Name: "Bob"}
	m1, _ := h.Send("lobby", p1, "geheim eins", 0)
	m2, _ := h.Send("lobby", p2, "Hallo Alice", 0)
	m3, _ := h.Send("lobby", p1, "geheim zwei", 0)
	h.React("lobby", m2.ID, p1, "❤️")
	flush(t, h, u, s)

	selfDel, _ := h.Send("lobby", p2, "peinlicher Tippfehler", 0)
	h.EditOwn("lobby", selfDel.ID, p2, "noch peinlicher")
	flush(t, h, u, s)
	if _, err := h.DeleteOwn("lobby", selfDel.ID, "b2"); err != nil {
		t.Fatal(err)
	}
	flush(t, h, u, s)
	if data, _ := os.ReadFile(filepath.Join(dir, "rooms", "lobby.messages.jsonl")); strings.Contains(string(data), "peinlich") {
		t.Fatal("selbst gelöschter Text steht noch in der Datei")
	}
	h.PurgeMessage("lobby", selfDel.ID)
	if _, err := h.PurgeMessage("lobby", m1.ID); err != nil {
		t.Fatal(err)
	}
	flush(t, h, u, s)
	data, _ := os.ReadFile(filepath.Join(dir, "rooms", "lobby.messages.jsonl"))
	if strings.Contains(string(data), "geheim eins") {
		t.Fatal("endgültig gelöschte Nachricht steht noch in der Datei")
	}
	if n := h.PurgeUser("a1"); n != 1 {
		t.Fatalf("PurgeUser: %d", n)
	}
	flush(t, h, u, s)
	data, _ = os.ReadFile(filepath.Join(dir, "rooms", "lobby.messages.jsonl"))
	if strings.Contains(string(data), "geheim") || strings.Contains(string(data), "Alice\"") && strings.Contains(string(data), "a1") {
		t.Fatalf("Daten der Person noch in der Datei: %s", data)
	}
	_ = m3
	s.Close()

	h2, u2, s2 := openStore(t, dir)
	msgs := hist(h2, "lobby")
	if len(msgs) != 1 || msgs[0].ID != m2.ID || len(msgs[0].Reactions) != 0 {
		t.Fatalf("nach Neustart: %+v", msgs)
	}
	// Aufbewahrungsfrist
	h2.mu.Lock()
	h2.rooms["lobby"].history[0].Time = time.Now().Add(-40 * 24 * time.Hour)
	h2.mu.Unlock()
	h2.Send("lobby", p2, "frisch", 0)
	h2.AuditLog().Add(AuditEntry{Time: time.Now().Add(-50 * 24 * time.Hour), Action: "alt"})
	h2.AuditLog().Add(AuditEntry{Action: "neu"})
	if n := h2.ApplyRetention(30, time.Now()); n != 1 {
		t.Fatalf("Aufbewahrung: %d", n)
	}
	flush(t, h2, u2, s2)
	s2.Close()
	h3, _, _ := openStore(t, dir)
	if msgs := hist(h3, "lobby"); len(msgs) != 1 || msgs[0].Text != "frisch" {
		t.Fatalf("nach Aufbewahrung: %+v", msgs)
	}
	if list, _ := h3.AuditLog().Query("", 0, 10); len(list) != 1 || list[0].Action != "neu" {
		t.Fatalf("Protokoll nach Aufbewahrung: %+v", list)
	}
}

func TestPurgeAPI(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	tok := claim(t, ts.URL, "Weg")
	id := sess(t, ts.URL, tok).ID
	m := decode[Message](t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"eins"}`))
	post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"zwei"}`).Body.Close()
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms/lobby/messages/"+itoa64(m.ID)+"/purge", adm, ""), 204, "purge")
	if len(hist(srv.hub, "lobby")) != 1 {
		t.Fatal("Nachricht nicht entfernt")
	}
	out := decode[map[string]any](t, expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+id+"/purge", adm, `{"releaseName":true}`), 200, "Person"))
	if out["deleted"].(float64) != 1 || out["released"] != true {
		t.Fatalf("%+v", out)
	}
	expect(t, do(t, "GET", ts.URL+"/api/session", tok, ""), 401, "Token nach Freigabe")
	claim(t, ts.URL, "Weg") // Name wieder frei
	log := decode[struct{ Entries []AuditEntry }](t, do(t, "GET", ts.URL+"/api/admin/audit?q=purge", adm, ""))
	for _, e := range log.Entries {
		if strings.Contains(e.Detail, "eins") {
			t.Fatal("Protokoll enthält Text einer endgültig gelöschten Nachricht")
		}
	}
}

func TestUnreadMentionsSearch(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	a, b := claim(t, ts.URL, "Lena"), claim(t, ts.URL, "Tom Bauer")
	base := ts.URL + "/api/rooms/lobby/messages"
	first := decode[Message](t, post(t, base, a, `{"text":"Guten Morgen"}`))
	post(t, base, a, `{"text":"@Tom Bauer kannst du helfen?"}`).Body.Close()
	post(t, base, a, `{"text":"@tom bauerhof ist keine Erwähnung"}`).Body.Close()
	post(t, base, b, `{"text":"Klar, @lena!"}`).Body.Close()
	body := `{"since":{"lobby":` + itoa64(first.ID) + `,"gibtsnicht":0}}`
	got := decode[map[string]UnreadInfo](t, expect(t, do(t, "POST", ts.URL+"/api/unread", b, body), 200, "unread"))
	if got["lobby"] != (UnreadInfo{Unread: 2, Mentions: 1}) {
		t.Fatalf("Tom: %+v", got)
	}
	got = decode[map[string]UnreadInfo](t, do(t, "POST", ts.URL+"/api/unread", a, body))
	if got["lobby"] != (UnreadInfo{Unread: 1, Mentions: 1}) {
		t.Fatalf("Lena: %+v", got)
	}
	if !Mentions("Hi @Tom Bauer.", "Tom Bauer") || Mentions("mail@tombauer.de", "Tom Bauer") {
		t.Fatal("Mentions")
	}

	// Suche
	srv.hub.ModDelete("lobby", first.ID, "mod", "x")
	hits := decode[[]SearchHit](t, expect(t, do(t, "GET", ts.URL+"/api/rooms/lobby/search?q=morgen", "", ""), 200, "Suche"))
	if len(hits) != 0 {
		t.Fatal("öffentliche Suche findet gelöschte Nachricht")
	}
	hits = decode[[]SearchHit](t, do(t, "GET", ts.URL+"/api/rooms/lobby/search?q=HELFEN", "", ""))
	if len(hits) != 1 || hits[0].Message.Original != "" {
		t.Fatalf("Suche: %+v", hits)
	}
	expect(t, do(t, "GET", ts.URL+"/api/rooms/lobby/search?q=a", "", ""), 400, "zu kurz")
	adm := adminLogin(t, ts.URL)
	hits = decode[[]SearchHit](t, expect(t, do(t, "GET", ts.URL+"/api/admin/search?q=morgen", adm, ""), 200, "Admin-Suche"))
	if len(hits) != 1 || hits[0].Message.Original != "Guten Morgen" {
		t.Fatalf("Admin-Suche: %+v", hits)
	}
}

func TestPresenceList(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	tok := claim(t, ts.URL, "Paula")
	s := sess(t, ts.URL, tok)
	if !strings.HasPrefix(s.Viewer, s.ID+".") {
		t.Fatalf("viewer: %q", s.Viewer)
	}
	open := func(q string) (*http.Response, func() Event) {
		res, err := http.Get(ts.URL + "/api/rooms/lobby/events" + q)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(res.Body)
		return res, func() Event {
			for sc.Scan() {
				if line := sc.Text(); strings.HasPrefix(line, "data: ") {
					var ev Event
					json.Unmarshal([]byte(line[6:]), &ev)
					return ev
				}
			}
			return Event{}
		}
	}
	r1, next1 := open("?viewer=" + s.Viewer)
	defer r1.Body.Close()
	if ev := next1(); ev.Type != "history" || ev.RoomInfo == nil {
		t.Fatalf("history: %+v", ev)
	}
	r2, _ := open("?viewer=" + s.ID + ".gefaelscht")
	defer r2.Body.Close()
	time.Sleep(100 * time.Millisecond)
	p := decode[Event](t, do(t, "GET", ts.URL+"/api/rooms/lobby/presence", "", ""))
	if len(p.Users) != 1 || p.Users[0].Name != "Paula" || p.Guests != 1 || p.Online != 2 {
		t.Fatalf("presence: %+v", p)
	}
}

func TestBotModeration(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	res := expect(t, do(t, "POST", ts.URL+"/api/admin/bots", adm, `{"name":"Wächter","allRooms":true}`), 201, "Bot")
	created := decode[struct {
		Bot   BotInfo
		Token string
	}](t, res)
	tok := claim(t, ts.URL, "Spammer")
	uid := sess(t, ts.URL, tok).ID
	m := decode[Message](t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"billig kaufen"}`))
	del := ts.URL + "/api/bot/rooms/lobby/messages/" + itoa64(m.ID)
	expect(t, do(t, "DELETE", del, created.Token, ""), 403, "ohne Recht")
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/bots/"+created.Bot.ID, adm, `{"name":"Wächter","allRooms":true,"canModerate":true}`), 200, "Recht geben")
	out := decode[Message](t, expect(t, do(t, "DELETE", del, created.Token, ""), 200, "Bot löscht"))
	if !out.Deleted || out.DeletedBy != "bot" || out.Original != "" {
		t.Fatalf("%+v", out)
	}
	if am := adminHist(srv.hub, "lobby")[0]; am.Original != "billig kaufen" || am.ModBy != "Wächter" {
		t.Fatalf("Admin-Sicht: %+v", am)
	}
	expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+uid+"/mute", created.Token, `{"minutes":5000}`), 400, "zu lang")
	expect(t, do(t, "POST", ts.URL+"/api/bot/users/"+uid+"/mute", created.Token, `{"minutes":10,"reason":"Werbung"}`), 200, "mute")
	expect(t, post(t, ts.URL+"/api/rooms/lobby/messages", tok, `{"text":"noch mal"}`), 403, "stumm")
	log := decode[struct{ Entries []AuditEntry }](t, do(t, "GET", ts.URL+"/api/admin/audit?q=bot", adm, ""))
	n := 0
	for _, e := range log.Entries {
		if strings.HasPrefix(e.Actor, "bot:") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("Bot-Aktionen im Protokoll: %d (%+v)", n, log.Entries)
	}
}

func TestWidgetsAndEmbedPolicy(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	preflight := func(origin, path string) string {
		req, _ := http.NewRequest(http.MethodOptions, ts.URL+path, nil)
		req.Header.Set("Origin", origin)
		res, _ := http.DefaultClient.Do(req)
		return res.Header.Get("Access-Control-Allow-Origin")
	}
	if preflight("https://irgendwo.test", "/api/rooms/lobby/messages") == "" {
		t.Fatal("Standard „überall“ greift nicht")
	}
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"support"}`), 201, "Kanal")
	res := expect(t, do(t, "POST", ts.URL+"/api/admin/widgets", adm, `{"name":"Shop","room":"support","title":"Hilfe","greeting":"Hallo! Wie können wir helfen?","showHours":true,"domains":["https://www.Shop.example/","*.kunde.test","kaputt domain"]}`), 400, "ungültige Domain")
	res.Body.Close()
	wd := decode[Widget](t, expect(t, do(t, "POST", ts.URL+"/api/admin/widgets", adm, `{"name":"Shop","room":"support","title":"Hilfe","greeting":"Hallo! Wie können wir helfen?","showHours":true,"domains":["https://www.Shop.example/","*.kunde.test"]}`), 201, "Widget"))
	if !strings.HasPrefix(wd.ID, "w_") || wd.Domains[0] != "www.shop.example" || wd.Mode != "floating" {
		t.Fatalf("%+v", wd)
	}
	pub := decode[map[string]any](t, expect(t, do(t, "GET", ts.URL+"/api/widgets/"+wd.ID, "", ""), 200, "öffentlich"))
	if pub["greeting"] != "Hallo! Wie können wir helfen?" || pub["domains"] != nil {
		t.Fatalf("öffentliche Sicht: %+v", pub)
	}
	st := srv.hub.Settings()
	st.EmbedPolicy = EmbedListed
	if _, err := srv.hub.SetSettings(st); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		origin, path string
		ok           bool
	}{
		{"https://www.shop.example", "/api/rooms/support/messages", true},
		{"https://www.shop.example", "/api/rooms/lobby/messages", false}, // anderer Kanal
		{"https://www.shop.example", "/api/session", true},
		{"https://a.b.kunde.test", "/api/rooms/support/events", true},
		{"https://kunde.test", "/api/rooms/support/events", true},
		{"https://evilkunde.test", "/api/rooms/support/events", false},
		{"https://irgendwo.test", "/api/settings", false},
	}
	for _, c := range cases {
		if got := preflight(c.origin, c.path) != ""; got != c.ok {
			t.Errorf("%s %s: %v statt %v", c.origin, c.path, got, c.ok)
		}
	}
	// Umbenennen des Kanals nimmt das Widget mit
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/support", adm, `{"name":"hilfe","description":""}`), 200, "umbenennen")
	if w, _ := srv.hub.Widgets().Get(wd.ID); w.Room != "hilfe" {
		t.Fatalf("Widget-Kanal: %s", w.Room)
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/widgets/"+wd.ID, adm, ""), 204, "löschen")
	expect(t, do(t, "GET", ts.URL+"/api/widgets/"+wd.ID, "", ""), 404, "gelöscht")
}

func TestOpeningHours(t *testing.T) {
	loc := loadLocation("Europe/Berlin")
	h := defaultHours()
	h.Enabled = true
	mon10 := time.Date(2026, 10, 5, 10, 0, 0, 0, loc) // Montag
	if st := h.Status(mon10); !st.Open || st.Today != "09:00–18:00" {
		t.Fatalf("Mo 10 Uhr: %+v", st)
	}
	if st := h.Status(mon10.Add(9 * time.Hour)); st.Open || st.Next != "morgen 09:00" || st.Message == "" {
		t.Fatalf("Mo 19 Uhr: %+v", st)
	}
	if st := h.Status(time.Date(2026, 10, 10, 12, 0, 0, 0, loc)); st.Open || st.Next != "Mo 09:00" {
		t.Fatalf("Samstag: %+v", st)
	}
	if st := h.Status(mon10.Add(-3 * time.Hour)); st.Open || st.Next != "heute 09:00" {
		t.Fatalf("Mo 7 Uhr: %+v", st)
	}
	s := defaultSettings
	s.Hours.Days[0].To = "08:00"
	s.Hours.Days[0].Open = true
	if _, err := validateSettings(s); err != ErrHours {
		t.Fatalf("bis vor von: %v", err)
	}
	s = defaultSettings
	s.Timezone = "Mond/Krater"
	if _, err := validateSettings(s); err != ErrTimezone {
		t.Fatalf("Zeitzone: %v", err)
	}
	s = defaultSettings
	s.PrivacyURL = "javascript:alert(1)"
	if _, err := validateSettings(s); err != ErrBadURL {
		t.Fatalf("URL: %v", err)
	}
}

func TestBackups(t *testing.T) {
	data := t.TempDir()
	h, u, s := openStore(t, data)
	h.Send("lobby", Poster{ID: "x", Name: "Xaver"}, "Sicher ist sicher", 0)
	h.touchDoc("config.json")
	flush(t, h, u, s)
	b := NewBackups(data, filepath.Join(data, "backups"), 2) // Backup-Ordner im Datenordner wird übersprungen
	b.store = s
	base := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := b.Create(base.Add(time.Duration(i) * time.Second)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	list, _ := b.List()
	if len(list) != 2 {
		t.Fatalf("aufbewahrt: %d", len(list))
	}
	p, err := b.Path(list[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, p)
	if !names["rooms/lobby.messages.jsonl"] || !names["config.json"] || names["backups/"] {
		t.Fatalf("Archiv: %v", names)
	}
	if _, err := b.Path("../config.json"); err == nil {
		t.Fatal("Pfad außerhalb erlaubt")
	}
	// Tick: einmal täglich ab 3 Uhr
	b2 := NewBackups(data, t.TempDir(), 7)
	b2.Tick(time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC), nil)
	b2.Tick(time.Date(2026, 10, 9, 3, 1, 0, 0, time.UTC), nil)
	b2.Tick(time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC), nil)
	if l, _ := b2.List(); len(l) != 1 {
		t.Fatalf("Tick: %d", len(l))
	}
}

func TestStats(t *testing.T) {
	h := NewHub(0)
	h.SetAutomod(AutomodConfig{Enabled: true, Patterns: []string{"mist"}})
	h.Send("lobby", Poster{ID: "a", Name: "A"}, "eins", 0)
	h.Send("lobby", Poster{ID: "a", Name: "A"}, "zwei", 0)
	h.Send("lobby", Poster{ID: "b", Name: "B"}, "so ein Mist", 0)
	h.Send("andere", Poster{ID: "b", Name: "B"}, "drei", 0)
	st := h.Stats(30, time.Now())
	last := st.Days[len(st.Days)-1]
	if len(st.Days) != 30 || last.Messages != 4 || last.Users != 2 || last.Automod != 1 {
		t.Fatalf("heute: %+v", last)
	}
	if st.TopRooms[0].Room != "lobby" || st.TopRooms[0].Messages != 3 || st.Pending != 1 || st.Period != 4 {
		t.Fatalf("%+v", st)
	}
}

func TestNewDocsPersist(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.Accounts().Create(AccountInput{Name: "otto", Role: RoleModerator, Password: "otto-passwort-1"})
	h.Mod().Sanction("u1", "Ulf", "ban", 0, "", "Admin", false)
	h.Mod().SetProtected([]string{"boss*"})
	h.Widgets().Create(Widget{Name: "Test", Room: "lobby"})
	h.AuditLog().Add(AuditEntry{Actor: "admin", Action: "test"})
	st := h.Settings()
	st.RetentionDays, st.ImprintURL = 90, "https://example.com/impressum"
	if _, err := h.SetSettings(st); err != nil {
		t.Fatal(err)
	}
	flush(t, h, u, s)
	s.Close()
	h2, _, _ := openStore(t, dir)
	if _, _, ok := h2.Accounts().Login("otto", "otto-passwort-1"); !ok {
		t.Fatal("Konto nicht gespeichert")
	}
	if _, ok := h2.Mod().Active("u1", ""); !ok || !h2.Mod().Protected("Bossi") {
		t.Fatal("Moderation nicht gespeichert")
	}
	if len(h2.Widgets().List()) != 1 || h2.Settings().RetentionDays != 90 || h2.Settings().ImprintURL == "" {
		t.Fatal("Widgets/Einstellungen nicht gespeichert")
	}
	if l, _ := h2.AuditLog().Query("test", 0, 5); len(l) != 1 {
		t.Fatal("Protokoll nicht gespeichert")
	}
	// IP-Schlüssel bleibt gleich (sonst wären IP-Sperren nach Neustart wirkungslos)
	if h.Mod().IPHash("1.2.3.4") != h2.Mod().IPHash("1.2.3.4") {
		t.Fatal("IP-Schlüssel nicht gespeichert")
	}
}

func TestPurgeSurvivesRenameAndScrubsAudit(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.CreateRoom("a", "")
	m, _ := h.Send("a", Poster{ID: "u1", Name: "Ulla"}, "geheim123", 0)
	h.Send("a", Poster{ID: "u2", Name: "Udo"}, "bleibt", 0)
	h.AuditLog().Add(AuditEntry{Action: "message.delete", Room: "a", Message: m.ID, UserID: "u1", UserName: "Ulla", Detail: "geheim123"})
	flush(t, h, u, s)
	h.PurgeMessage("a", m.ID)
	name := "b"
	if _, err := h.UpdateRoomWith("a", RoomInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	flush(t, h, u, s)
	s.Close()
	for _, f := range []string{"rooms/b.messages.jsonl", "audit.jsonl"} {
		if data, _ := os.ReadFile(filepath.Join(dir, f)); strings.Contains(string(data), "geheim123") {
			t.Fatalf("%s enthält gelöschten Text", f)
		}
	}
	h2, _, _ := openStore(t, dir)
	if msgs := hist(h2, "b"); len(msgs) != 1 || msgs[0].Text != "bleibt" {
		t.Fatalf("nach Umbenennen: %+v", msgs)
	}
	// Person endgültig löschen entfernt den Namen aus dem Protokoll
	h2.AuditLog().Add(AuditEntry{Action: "user.mute", UserID: "u2", UserName: "Udo", Detail: "bis 12:00 Uhr"})
	h2.PurgeUser("u2")
	for _, e := range h2.AuditLog().all() {
		if e.UserName == "Udo" {
			t.Fatal("Name nach PurgeUser noch im Protokoll")
		}
	}
}

func TestAuditNotDuplicatedOnRewrite(t *testing.T) {
	dir := t.TempDir()
	h, u, s := openStore(t, dir)
	h.AuditLog().Add(AuditEntry{Time: time.Now().Add(-100 * 24 * time.Hour), Action: "alt"})
	flush(t, h, u, s)
	h.AuditLog().Trim(time.Now().Add(-24 * time.Hour))
	h.AuditLog().Add(AuditEntry{Action: "neu"})
	flush(t, h, u, s)
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if n := strings.Count(string(data), `"neu"`); n != 1 || strings.Contains(string(data), `"alt"`) {
		t.Fatalf("audit.jsonl: %s", data)
	}
}

func TestFailedRenameKeepsSettings(t *testing.T) {
	h := NewHub(0)
	h.CreateRoom("eins", "")
	h.CreateRoom("zwei", "")
	n := "zwei"
	if _, err := h.UpdateRoomWith("eins", RoomInput{Name: &n, SlowMode: 60, Links: LinksTeam}); err != ErrRoomExists {
		t.Fatal(err)
	}
	for _, r := range h.Rooms() {
		if r.Name == "eins" && (r.SlowMode != 0 || r.Links != LinksAll) {
			t.Fatalf("Einstellungen trotz Fehler geändert: %+v", r)
		}
	}
}

func TestWidgetDomainRules(t *testing.T) {
	if _, err := normDomain("*.com"); err == nil {
		t.Fatal("*.com erlaubt")
	}
	if !domainMatches("*.example.com:8443", "https://a.example.com:8443") || domainMatches("*.example.com:8443", "https://a.example.com") {
		t.Fatal("Wildcard mit Port")
	}
}
