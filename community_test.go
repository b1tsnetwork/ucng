// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Hilfen für die Tests der Community-Funktionen.

func mkRoles(t *testing.T, base, adm, body string) []Role {
	t.Helper()
	return decode[[]Role](t, expect(t, do(t, "PUT", base+"/api/admin/roles", adm, body), 200, "Rollen"))
}

func giveRoles(t *testing.T, base, adm, userID string, ids ...string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"roles": ids})
	expect(t, do(t, "POST", base+"/api/admin/users/"+userID+"/roles", adm, string(b)), 200, "Rollen vergeben").Body.Close()
}

func TestPrivateAndReadOnlyRooms(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	roles := mkRoles(t, ts.URL, adm, `{"roles":[{"name":"VIP","color":"#aa0000"},{"name":"Redaktion","color":"#00aa00"}]}`)
	vip, red := roles[0].ID, roles[1].ID
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"vip-lounge","readRoles":["`+vip+`"]}`), 201, "privat").Body.Close()
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"news","writeRoles":["`+red+`"]}`), 201, "nur lesen").Body.Close()
	expect(t, do(t, "POST", ts.URL+"/api/admin/rooms", adm, `{"name":"dm-hack"}`), 400, "dm-Name").Body.Close()

	ada, bert := claim(t, ts.URL, "Ada"), claim(t, ts.URL, "Bert")
	adaID := sess(t, ts.URL, ada).ID
	giveRoles(t, ts.URL, adm, adaID, vip, red)

	names := func(tok string) string {
		var list []RoomInfo
		json.NewDecoder(do(t, "GET", ts.URL+"/api/rooms", tok, "").Body).Decode(&list)
		var n []string
		for _, r := range list {
			n = append(n, r.Name)
			if r.Name == "news" {
				n = append(n, map[bool]string{true: "(w)", false: "(r)"}[r.CanWrite])
			}
		}
		return strings.Join(n, ",")
	}
	if got := names(bert); strings.Contains(got, "vip-lounge") || !strings.Contains(got, "news,(r)") {
		t.Fatalf("Bert sieht: %s", got)
	}
	if got := names(ada); !strings.Contains(got, "vip-lounge") || !strings.Contains(got, "news,(w)") {
		t.Fatalf("Ada sieht: %s", got)
	}
	if got := names(""); strings.Contains(got, "vip-lounge") {
		t.Fatalf("Gast sieht: %s", got)
	}
	// Verlauf, Schreiben, Stream
	expect(t, post(t, ts.URL+"/api/rooms/vip-lounge/messages", ada, `{"text":"Hallo VIPs"}`), 201, "VIP schreibt").Body.Close()
	expect(t, post(t, ts.URL+"/api/rooms/vip-lounge/messages", bert, `{"text":"rein"}`), 404, "Bert schreibt privat").Body.Close()
	expect(t, do(t, "GET", ts.URL+"/api/rooms/vip-lounge/messages", bert, ""), 404, "Bert liest").Body.Close()
	expect(t, do(t, "GET", ts.URL+"/api/rooms/vip-lounge/messages", "", ""), 404, "Gast liest").Body.Close()
	expect(t, do(t, "GET", ts.URL+"/api/rooms/vip-lounge/messages", ada, ""), 200, "Ada liest").Body.Close()
	expect(t, do(t, "GET", ts.URL+"/api/rooms/vip-lounge/events", "", ""), 404, "Stream Gast").Body.Close()
	expect(t, do(t, "GET", ts.URL+"/api/rooms/vip-lounge/search?q=hallo", bert, ""), 404, "Suche Bert").Body.Close()
	expect(t, post(t, ts.URL+"/api/rooms/news/messages", bert, `{"text":"Ich will auch"}`), 403, "nur lesen").Body.Close()
	news := decode[Message](t, expect(t, post(t, ts.URL+"/api/rooms/news/messages", ada, `{"text":"Neuigkeit!"}`), 201, "Redaktion"))
	expect(t, do(t, "POST", ts.URL+"/api/rooms/news/messages/"+itoa64(news.ID)+"/react", bert, `{"emoji":"👍"}`), 200, "reagieren geht").Body.Close()
	// Rolle weg → kein Zugriff mehr; Rolle löschen → Kanal bleibt privat (nur Team)
	giveRoles(t, ts.URL, adm, adaID)
	expect(t, do(t, "GET", ts.URL+"/api/rooms/vip-lounge/messages", ada, ""), 404, "ohne Rolle").Body.Close()
	mkRoles(t, ts.URL, adm, `{"roles":[{"id":"`+red+`","name":"Redaktion","color":"#00aa00"}]}`)
	if got := names(bert); strings.Contains(got, "vip-lounge") {
		t.Fatalf("Kanal nach Löschen der Rolle öffentlich: %s", got)
	}
	// Unread zählt private Kanäle nur für Berechtigte
	res := decode[map[string]UnreadInfo](t, post(t, ts.URL+"/api/unread", bert, `{"since":{"vip-lounge":0,"news":0}}`))
	if _, ok := res["vip-lounge"]; ok || res["news"].Unread != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestRolePermissions(t *testing.T) {
	ts, srv := testServerWith(t, func(c *Config) { c.RateLimit = 1000 })
	adm := adminLogin(t, ts.URL)
	roles := mkRoles(t, ts.URL, adm, `{"roles":[{"name":"Moderator","color":"#2e7d32","mentionable":true,
		"perms":{"links":true,"noSlow":true,"protected":true,"pin":true,"delete":true,"mute":true}}]}`)
	expect(t, do(t, "PATCH", ts.URL+"/api/admin/rooms/lobby", adm, `{"description":"","slowMode":60,"links":"team"}`), 200, "Regeln").Body.Close()
	mo, gast := claim(t, ts.URL, "Mona"), claim(t, ts.URL, "Gustav")
	moID, gastID := sess(t, ts.URL, mo).ID, sess(t, ts.URL, gast).ID
	giveRoles(t, ts.URL, adm, moID, roles[0].ID)
	if s := sess(t, ts.URL, mo); !s.Perms.Pin || len(s.Roles) != 1 {
		t.Fatalf("Session: %+v", s)
	}
	base := ts.URL + "/api/rooms/lobby/messages"
	expect(t, post(t, base, gast, `{"text":"https://example.com"}`), 403, "Link Gast").Body.Close()
	expect(t, post(t, base, mo, `{"text":"Infos: https://example.com"}`), 201, "Link Rolle").Body.Close()
	expect(t, post(t, base, mo, `{"text":"Zweite Nachricht sofort"}`), 201, "kein Langsam-Modus").Body.Close()
	g := decode[Message](t, expect(t, post(t, base, gast, `{"text":"Doofe Werbung hier"}`), 201, "Gast"))
	expect(t, post(t, base, gast, `{"text":"noch eine"}`), 429, "Langsam-Modus Gast").Body.Close()
	// Anpinnen, Löschen, Stummschalten im Chat
	expect(t, do(t, "POST", base+"/"+itoa64(g.ID)+"/pin", gast, `{"pin":true}`), 403, "Gast pinnt").Body.Close()
	pins := decode[[]Message](t, expect(t, do(t, "POST", base+"/"+itoa64(g.ID)+"/pin", mo, `{"pin":true}`), 200, "pin"))
	if len(pins) != 1 || pins[0].ID != g.ID {
		t.Fatalf("%+v", pins)
	}
	expect(t, do(t, "DELETE", base+"/"+itoa64(g.ID)+"/mod", gast, ""), 403, "Gast löscht").Body.Close()
	d := decode[Message](t, expect(t, do(t, "DELETE", base+"/"+itoa64(g.ID)+"/mod", mo, ""), 200, "Rolle löscht"))
	if !d.Deleted || d.Text != "" || d.Original != "" {
		t.Fatalf("%+v", d)
	}
	expect(t, post(t, ts.URL+"/api/users/"+gastID+"/mute", mo, `{"minutes":120}`), 400, "zu lang").Body.Close()
	expect(t, post(t, ts.URL+"/api/users/"+gastID+"/mute", mo, `{"minutes":15,"reason":"Werbung"}`), 200, "stumm").Body.Close()
	expect(t, post(t, ts.URL+"/api/users/"+moID+"/mute", gast, `{"minutes":5}`), 403, "Gast schaltet stumm").Body.Close()
	if _, ok := srv.hub.Mod().Active(gastID, ""); !ok {
		t.Fatal("nicht stumm")
	}
	// geschützte Namen
	srv.hub.Mod().SetProtected([]string{"support*"})
	res := do(t, "POST", ts.URL+"/api/session", mo, `{"name":"Support Mona"}`)
	if res.StatusCode != 200 {
		t.Fatalf("Rolle darf geschützten Namen nicht: %d %s", res.StatusCode, errText(res))
	}
	// @Rolle zählt als Erwähnung
	other := claim(t, ts.URL, "Olaf")
	expect(t, post(t, base, other, `{"text":"@Moderator Hilfe bitte"}`), 201, "erwähnen").Body.Close()
	u := decode[map[string]UnreadInfo](t, post(t, ts.URL+"/api/unread", mo, `{"since":{"lobby":0}}`))
	if u["lobby"].Mentions != 1 {
		t.Fatalf("%+v", u)
	}
	// Audit vermerkt die Chat-Moderation
	if !strings.Contains(auditActions(srv), "message.delete") {
		t.Fatal("Protokoll fehlt")
	}
}

func auditActions(srv *Server) string {
	var a []string
	for _, e := range srv.hub.AuditLog().all() {
		a = append(a, e.Action+"@"+e.Actor)
	}
	return strings.Join(a, " ")
}

func TestReportsAndFilter(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	expect(t, do(t, "PUT", ts.URL+"/api/admin/filter", adm, `{"patterns":["mist","doof*"]}`), 200, "Filter").Body.Close()
	a, b := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	base := ts.URL + "/api/rooms/lobby/messages"
	m := decode[Message](t, post(t, base, a, `{"text":"So ein Mist, total d00f heute. @Ben"}`))
	if m.Text != "So ein ****, total **** heute. @Ben" || m.Unfiltered != "" {
		t.Fatalf("Filter: %q", m.Text)
	}
	if am, _ := srv.hub.AdminMessage("lobby", m.ID); !strings.Contains(am.Unfiltered, "Mist") {
		t.Fatalf("Original für Admins fehlt: %+v", am)
	}
	expect(t, post(t, base+"/"+itoa64(m.ID)+"/report", a, `{"reason":"x"}`), 403, "eigene").Body.Close()
	expect(t, post(t, base+"/"+itoa64(m.ID)+"/report", b, `{"reason":"Beleidigung"}`), 201, "melden").Body.Close()
	expect(t, post(t, base+"/"+itoa64(m.ID)+"/report", b, `{"reason":"nochmal"}`), 409, "doppelt").Body.Close()
	c := decode[map[string]int](t, do(t, "GET", ts.URL+"/api/admin/reports/count", adm, ""))
	if c["open"] != 1 {
		t.Fatalf("%+v", c)
	}
	list := decode[[]adminReport](t, do(t, "GET", ts.URL+"/api/admin/reports", adm, ""))
	if len(list) != 1 || list[0].Message == nil || list[0].Reason != "Beleidigung" {
		t.Fatalf("%+v", list)
	}
	expect(t, post(t, ts.URL+"/api/admin/reports/"+list[0].ID, adm, `{"action":"delete"}`), 200, "löschen").Body.Close()
	if am, _ := srv.hub.AdminMessage("lobby", m.ID); !am.Deleted {
		t.Fatal("nicht gelöscht")
	}
	if srv.hub.Reports().OpenCount() != 0 {
		t.Fatal("Meldung offen")
	}
}

func TestPollsAndThreads(t *testing.T) {
	ts, _ := testServerWith(t, func(c *Config) { c.RateLimit = 1000 })
	a, b := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	bID := sess(t, ts.URL, b).ID
	base := ts.URL + "/api/rooms/lobby/messages"
	// falsche Eingabe → privater Hinweis
	var eph struct{ Ephemeral *EphemeralReply }
	json.NewDecoder(expect(t, post(t, base, a, `{"text":"!umfrage Nur eine Frage?"}`), 200, "Hinweis").Body).Decode(&eph)
	if eph.Ephemeral == nil || !strings.Contains(eph.Ephemeral.Text, "So geht's") {
		t.Fatalf("%+v", eph)
	}
	p := decode[Message](t, expect(t, post(t, base, a, `{"text":"!umfrage Wann treffen wir uns? | Freitag | Samstag | freitag"}`), 201, "Umfrage"))
	if p.Poll == nil || len(p.Poll.Options) != 2 || p.Text != "" {
		t.Fatalf("%+v", p.Poll)
	}
	vote := func(tok, body string, code int) Message {
		res := expect(t, post(t, base+"/"+itoa64(p.ID)+"/vote", tok, body), code, "vote "+body)
		if code != 200 {
			res.Body.Close()
			return Message{}
		}
		return decode[Message](t, res)
	}
	vote(b, `{"option":0}`, 200)
	v := vote(b, `{"option":1}`, 200) // umstellen
	if len(v.Poll.Options[0].Votes) != 0 || v.Poll.Options[1].Votes[0] != bID {
		t.Fatalf("%+v", v.Poll)
	}
	vote(b, `{"option":5}`, 400)
	vote(b, `{"close":true}`, 403)
	vote(a, `{"close":true}`, 200)
	vote(b, `{"option":0}`, 400)
	m := decode[Message](t, expect(t, post(t, base, a, `{"text":"!umfrage mehrfach: Was essen? | Pizza | Pasta"}`), 201, "mehrfach"))
	post(t, base+"/"+itoa64(m.ID)+"/vote", b, `{"option":0}`).Body.Close()
	v = decode[Message](t, post(t, base+"/"+itoa64(m.ID)+"/vote", b, `{"option":1}`))
	if len(v.Poll.Options[0].Votes) != 1 || len(v.Poll.Options[1].Votes) != 1 {
		t.Fatalf("mehrfach: %+v", v.Poll)
	}
	// Threads
	root := decode[Message](t, post(t, base, a, `{"text":"Thema: Grillfest"}`))
	r1 := decode[Message](t, expect(t, post(t, base, b, `{"text":"Ich bringe Salat","thread":`+itoa64(root.ID)+`}`), 201, "Thread"))
	post(t, base, a, `{"text":"Super!","thread":`+itoa64(root.ID)+`}`).Body.Close()
	expect(t, post(t, base, a, `{"text":"tiefer","thread":`+itoa64(r1.ID)+`}`), 400, "verschachtelt").Body.Close()
	var th struct {
		Root     Message
		Messages []Message
	}
	json.NewDecoder(expect(t, do(t, "GET", ts.URL+"/api/rooms/lobby/threads/"+itoa64(root.ID), "", ""), 200, "Thread lesen").Body).Decode(&th)
	if len(th.Messages) != 2 || th.Root.ThreadInfo == nil || th.Root.ThreadInfo.Count != 2 || th.Root.ThreadInfo.Names[0] != "Anna" {
		t.Fatalf("%+v", th)
	}
	// Befehl im Thread → Antwort des Bots im Thread
	post(t, base, b, `{"text":"!würfel","thread":`+itoa64(root.ID)+`}`).Body.Close()
	json.NewDecoder(do(t, "GET", ts.URL+"/api/rooms/lobby/threads/"+itoa64(root.ID), "", "").Body).Decode(&th)
	if last := th.Messages[len(th.Messages)-1]; !last.Bot || last.Thread != root.ID {
		t.Fatalf("Bot nicht im Thread: %+v", last)
	}
}

func TestSchedules(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	loc := srv.hub.Location()
	now := time.Now().In(loc)
	at := now.Add(-time.Minute).Format("2006-01-02T15:04")
	expect(t, post(t, ts.URL+"/api/admin/schedules", adm, `{"room":"gibtsnicht","text":"x","mode":"once","at":"`+at+`","enabled":true}`), 404, "Kanal").Body.Close()
	expect(t, post(t, ts.URL+"/api/admin/schedules", adm, `{"room":"lobby","text":"x","mode":"weekly","time":"20:00","enabled":true}`), 400, "Wochentage").Body.Close()
	sc := decode[Schedule](t, expect(t, post(t, ts.URL+"/api/admin/schedules", adm,
		`{"room":"lobby","text":"Heute {date}: Stream um 20 Uhr!","mode":"once","at":"`+at+`","pin":true,"enabled":true}`), 201, "anlegen"))
	daily := decode[Schedule](t, post(t, ts.URL+"/api/admin/schedules", adm, `{"room":"lobby","text":"Täglich","mode":"daily","time":"03:17","enabled":true}`))
	if daily.Next == nil || daily.Next.Before(now) {
		t.Fatalf("next: %+v", daily.Next)
	}
	srv.runSchedules(time.Now())
	msgs := hist(srv.hub, "lobby")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, now.Format("02.01.2006")) || !msgs[0].Bot {
		t.Fatalf("%+v", msgs)
	}
	if pins, _ := srv.hub.Pinned("lobby", nil); len(pins) != 1 {
		t.Fatal("nicht angepinnt")
	}
	srv.runSchedules(time.Now()) // nicht doppelt
	if len(hist(srv.hub, "lobby")) != 1 {
		t.Fatal("doppelt gesendet")
	}
	expect(t, post(t, ts.URL+"/api/admin/schedules/"+daily.ID+"/run", adm, ""), 200, "jetzt").Body.Close()
	if len(hist(srv.hub, "lobby")) != 2 {
		t.Fatal("von Hand")
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/schedules/"+sc.ID, adm, ""), 204, "löschen").Body.Close()
	// Wochenplan: nächster Termin am richtigen Tag
	w := Schedule{Mode: "weekly", Time: "10:00", Weekdays: []int{2}, Enabled: true} // Mittwoch
	n := w.next(time.Date(2026, 10, 10, 12, 0, 0, 0, loc), loc)                     // Samstag
	if n == nil || n.Weekday() != time.Wednesday || n.Day() != 14 {
		t.Fatalf("%v", n)
	}
}

func TestUploadsProfilesDMs(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	a, b := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben")
	aID, bID := sess(t, ts.URL, a).ID, sess(t, ts.URL, b).ID
	img := testPNG(1200, 800, color.RGBA{10, 120, 200, 255})
	// Standard: erst nach 24 Stunden
	res := upload(t, ts.URL+"/api/rooms/lobby/upload", a, map[string]string{"text": "Urlaub"}, "foto.png", img)
	if res.StatusCode != 403 {
		t.Fatalf("known: %d", res.StatusCode)
	}
	expect(t, do(t, "PUT", ts.URL+"/api/admin/uploads", adm, `{"mode":"all","size":640,"avatars":true}`), 200, "Regeln").Body.Close()
	m := decode[Message](t, expect(t, upload(t, ts.URL+"/api/rooms/lobby/upload", a, map[string]string{"text": "Urlaub"}, "foto.png", img), 201, "hochladen"))
	if m.Media == nil || m.Media.Kind != "image" || m.Media.W != 640 || m.Media.H != 427 || !strings.HasSuffix(m.Media.Src, ".jpg") || m.Text != "Urlaub" {
		t.Fatalf("%+v %+v", m, m.Media)
	}
	r := expect(t, do(t, "GET", ts.URL+m.Media.Src, "", ""), 200, "Datei")
	if ct := r.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Typ %s", ct)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	expect(t, upload(t, ts.URL+"/api/rooms/lobby/upload", a, nil, "x.txt", []byte("kein Bild")), 400, "kein Bild").Body.Close()
	// Profilbild und Status
	av := decode[map[string]string](t, expect(t, upload(t, ts.URL+"/api/profile/avatar", b, nil, "ich.png", testPNG(300, 200, color.RGBA{200, 0, 0, 255})), 200, "Avatar"))
	expect(t, do(t, "PUT", ts.URL+"/api/profile/status", b, `{"status":"away","text":"Bin  gleich zurück"}`), 200, "Status").Body.Close()
	expect(t, do(t, "PUT", ts.URL+"/api/profile/status", b, `{"status":"weg"}`), 400, "Status falsch").Body.Close()
	prof := decode[map[string]Profile](t, do(t, "GET", ts.URL+"/api/profiles", "", ""))
	if p := prof[bID]; p.Avatar != av["avatar"] || p.Status != "away" || p.Text != "Bin gleich zurück" {
		t.Fatalf("%+v", prof)
	}
	expect(t, do(t, "DELETE", ts.URL+"/api/admin/users/"+bID+"/avatar", adm, ""), 204, "Admin entfernt").Body.Close()
	if p := decode[map[string]Profile](t, do(t, "GET", ts.URL+"/api/profiles", "", ""))[bID]; p.Avatar != "" {
		t.Fatalf("%+v", p)
	}
	// Direktnachrichten
	dm := decode[DMInfo](t, expect(t, post(t, ts.URL+"/api/dms", a, `{"userId":"`+bID+`"}`), 200, "DM öffnen"))
	if !strings.HasPrefix(dm.Room, "dm-") || dm.Name != "Ben" {
		t.Fatalf("%+v", dm)
	}
	expect(t, post(t, ts.URL+"/api/dms", a, `{"userId":"`+aID+`"}`), 400, "selbst").Body.Close()
	expect(t, post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages", a, `{"text":"Psst, Ben!"}`), 201, "DM schreiben").Body.Close()
	c := claim(t, ts.URL, "Carla")
	expect(t, do(t, "GET", ts.URL+"/api/rooms/"+dm.Room+"/messages", c, ""), 404, "Dritte lesen").Body.Close()
	expect(t, post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages", c, `{"text":"Hallo?"}`), 404, "Dritte schreiben").Body.Close()
	expect(t, do(t, "GET", ts.URL+"/api/rooms/"+dm.Room+"/messages", b, ""), 200, "Ben liest").Body.Close()
	var dl struct {
		Enabled bool
		DMs     []DMInfo
	}
	json.NewDecoder(do(t, "GET", ts.URL+"/api/dms", b, "").Body).Decode(&dl)
	if !dl.Enabled || len(dl.DMs) != 1 || dl.DMs[0].Name != "Anna" || dl.DMs[0].LastID == 0 {
		t.Fatalf("%+v", dl)
	}
	u := decode[map[string]UnreadInfo](t, post(t, ts.URL+"/api/unread", b, `{"since":{"`+dm.Room+`":0}}`))
	if u[dm.Room].Mentions != 1 {
		t.Fatalf("%+v", u)
	}
	for _, ri := range decode[[]RoomInfo](t, do(t, "GET", ts.URL+"/api/rooms", b, "")) {
		if strings.HasPrefix(ri.Name, "dm-") {
			t.Fatal("DM in Kanalliste")
		}
	}
	// Bots dürfen nicht in Direktnachrichten
	ch := srv.hub.Listen(8)
	defer srv.hub.Unlisten(ch)
	post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages", b, `{"text":"Antwort"}`).Body.Close()
	select {
	case ev := <-ch:
		t.Fatalf("Bot-Ereignis aus DM: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	// ausgeschaltet
	st := srv.hub.Settings()
	st.NoDMs = true
	srv.hub.SetSettings(st)
	expect(t, post(t, ts.URL+"/api/dms", c, `{"userId":"`+bID+`"}`), 403, "aus").Body.Close()
	_ = http.StatusOK
}

func TestCommunityPersist(t *testing.T) {
	dir := t.TempDir()
	h, u, st := openStore(t, dir)
	roles, _ := h.Roles().Set([]Role{{Name: "VIP", Color: "#123456", Perms: Perms{Pin: true}, Mentionable: true}})
	h.CreateRoomWith(RoomInput{Name: strp("vip"), ReadRoles: []string{roles[0].ID}, WriteRoles: []string{RoleTeamOnly}})
	a, _ := u.Claim("Ada", "")
	b, _ := u.Claim("Ben", "")
	u.SetAvatar(a.ID, "x.png")
	u.SetStatus(a.ID, "busy", "Meeting")
	dm := h.OpenDM(a.ID, b.ID)
	m, err := h.SendFull(dm, Poster{ID: a.ID, Name: "Ada"}, SendInput{Text: "Hallo"})
	if err != nil {
		t.Fatal(err)
	}
	h.SetPin(dm, m.ID, true)
	root, _ := h.Send("lobby", Poster{ID: a.ID, Name: "Ada"}, "Thema", 0)
	h.SendFull("lobby", Poster{ID: b.ID, Name: "Ben"}, SendInput{Text: "Antwort", Thread: root.ID})
	poll, _ := ParsePoll("Frage? | A | B")
	h.SendFull("lobby", Poster{ID: b.ID, Name: "Ben"}, SendInput{Poll: poll})
	h.Reports().Add("lobby", root.ID, b.ID, "Ben", "Grund")
	h.Schedules().Put(Schedule{Room: "lobby", Text: "Hi", Mode: "daily", Time: "09:00", Enabled: true}, h.Location())
	h.SetFilter(FilterConfig{Patterns: []string{"mist"}})
	flush(t, h, u, st)
	st.Close()

	h2, u2, _ := openStore(t, dir)
	if ri := h2.RoomsFor(nil); len(ri) != 1 || ri[0].Name != "lobby" {
		t.Fatalf("Gast sieht: %+v", ri)
	}
	all := h2.Rooms()
	var vip RoomInfo
	for _, r := range all {
		if r.Name == "vip" {
			vip = r
		}
		if strings.HasPrefix(r.Name, "dm-") {
			t.Fatal("DM in Rooms()")
		}
	}
	if len(vip.ReadRoles) != 1 || vip.WriteRoles[0] != RoleTeamOnly {
		t.Fatalf("%+v", vip)
	}
	if d := h2.DMsOf(b.ID); len(d) != 1 || d[0].Room != dm {
		t.Fatalf("%+v", d)
	}
	if p, _ := h2.PinnedAll(dm); len(p) != 1 {
		t.Fatal("Pin verloren")
	}
	_, replies, err := h2.Thread("lobby", root.ID, nil)
	if err != nil || len(replies) != 1 {
		t.Fatalf("Thread: %v %+v", err, replies)
	}
	msgs := hist(h2, "lobby")
	if msgs[0].ThreadInfo == nil || msgs[0].ThreadInfo.Count != 1 || msgs[len(msgs)-1].Poll == nil {
		t.Fatalf("%+v", msgs)
	}
	if p := u2.ProfileMap()[a.ID]; p.Status != "busy" || p.Avatar != "/media/avatars/x.png" {
		t.Fatalf("%+v", p)
	}
	if h2.Reports().OpenCount() != 1 || len(h2.Schedules().export()) != 1 || len(h2.Filter().Patterns) != 1 {
		t.Fatal("Meldungen/Pläne/Filter verloren")
	}
	if r, _ := h2.Roles().Get(roles[0].ID); !r.Perms.Pin || !r.Mentionable {
		t.Fatalf("%+v", r)
	}
}

func strp(s string) *string { return &s }
