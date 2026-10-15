// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type ephem struct {
	Ephemeral EphemeralReply `json:"ephemeral"`
}

func say(t *testing.T, base, tok, room, text string) (ephem, int) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"text": text})
	res := post(t, base+"/api/rooms/"+room+"/messages", tok, string(b))
	defer res.Body.Close()
	var out ephem
	json.NewDecoder(res.Body).Decode(&out)
	return out, res.StatusCode
}

func TestTempRoomCommand(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	anna := claim(t, ts.URL, "Anna")
	// ohne Recht: nur ein Hinweis, nichts landet im Kanal
	e, code := say(t, ts.URL, anna, "lobby", "!kanal Spielrunde")
	if code != 200 || !e.Ephemeral.Notice || e.Ephemeral.Open != "" {
		t.Fatalf("ohne Recht: %d %+v", code, e)
	}
	if n := len(hist(h, "lobby")); n != 0 {
		t.Fatalf("Befehl landete im Kanal: %d", n)
	}
	roles, err := h.Roles().Set([]Role{{ID: "gamer", Name: "Gamer", Color: "#336699", Perms: Perms{TempRooms: true}}})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := srv.users.List("", 10)
	srv.users.SetRoles(list[0].ID, []string{roles[0].ID})
	e, _ = say(t, ts.URL, anna, "lobby", "!kanal Spiel Runde!")
	if e.Ephemeral.Open != "spiel-runde" || e.Ephemeral.Notice || !strings.Contains(e.Ephemeral.Text, "#spiel-runde") {
		t.Fatalf("anlegen: %+v", e)
	}
	info, ok := h.roomInfo("spiel-runde")
	if !ok || info.Temp == nil || info.Temp.Owner != list[0].ID || info.Temp.Idle != 5 || info.Voice == nil || info.Voice.Screen != ScreenSpeakers {
		t.Fatalf("Kanal: %+v %+v", info, info.Temp)
	}
	// nur einer je Person
	e, _ = say(t, ts.URL, anna, "lobby", "!kanal Noch einer")
	if !e.Ephemeral.Notice || !strings.Contains(e.Ephemeral.Text, "#spiel-runde") {
		t.Fatalf("zweiter Kanal: %+v", e)
	}
	// abgeschaltet
	cfg := h.TempRooms()
	cfg.Enabled = false
	h.SetTempRooms(cfg)
	if e, _ = say(t, ts.URL, anna, "lobby", "!kanal x"); !e.Ephemeral.Notice {
		t.Fatalf("ausgeschaltet: %+v", e)
	}

	// Ablauf: belegt (Sprachkanal) → bleibt; leer → nach der Frist gelöscht
	now := time.Now()
	if got := h.tempExpired(now.Add(10*time.Minute), map[string]bool{"spiel-runde": true}); len(got) != 0 {
		t.Fatalf("belegter Kanal abgelaufen: %v", got)
	}
	srv.sweepTempRooms(now.Add(11 * time.Minute)) // ab jetzt leer
	if _, ok := h.roomInfo("spiel-runde"); !ok {
		t.Fatal("zu früh gelöscht")
	}
	srv.sweepTempRooms(now.Add(17 * time.Minute))
	if _, ok := h.roomInfo("spiel-runde"); ok {
		t.Fatal("nicht gelöscht")
	}
	found := false
	for _, a := range h.AuditLog().all() {
		found = found || a.Action == "room.temp_delete"
	}
	if !found {
		t.Fatal("Löschen nicht protokolliert")
	}
}

func TestTempRoomName(t *testing.T) {
	for in, want := range map[string]string{"Spiel Runde!": "spiel-runde", "  ": "", "Über Größe": "über-größe", "dm-x": "t-dm-x",
		"a___b": "a___b", strings.Repeat("x", 40): strings.Repeat("x", 28)} {
		if got := tempRoomName(in); got != want {
			t.Errorf("%q → %q, erwartet %q", in, got, want)
		}
	}
}

func TestBotRooms(t *testing.T) {
	ts, srv := voiceServer(t, nil)
	h := srv.hub
	_, tok, _ := h.Bots().Create(BotInput{Name: "Raumwart", Rooms: []string{"lobby"}, Perms: &BotPerms{Write: true, Rooms: true}})
	_, tok2, _ := h.Bots().Create(BotInput{Name: "Andere", AllRooms: true, Perms: &BotPerms{Write: true, Rooms: true}})
	_, weak, _ := h.Bots().Create(BotInput{Name: "Schwach", AllRooms: true})
	anna := claim(t, ts.URL, "Anna")
	list, _ := srv.users.List("", 10)

	if res := post(t, ts.URL+"/api/bot/rooms", weak, `{"name":"x"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("ohne Recht: %d", res.StatusCode)
	}
	// temporär für eine Person, ohne Sprache
	res := post(t, ts.URL+"/api/bot/rooms", tok, `{"name":"Treffen","temporary":true,"idleMinutes":2,"forUser":"`+list[0].ID+`","voice":null}`)
	var info RoomInfo
	json.NewDecoder(res.Body).Decode(&info)
	if res.StatusCode != 201 || info.Name != "treffen" || info.Temp == nil || info.Temp.Idle != 2 || info.Voice != nil || info.Temp.OwnerName != "Anna" {
		t.Fatalf("temporär: %d %+v", res.StatusCode, info)
	}
	// dieselbe Person: Grenze
	if res := post(t, ts.URL+"/api/bot/rooms", tok, `{"name":"zwei","temporary":true,"forUser":"`+list[0].ID+`"}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("Grenze je Person: %d", res.StatusCode)
	}
	// dauerhafter Kanal mit Sprache; der Bot darf ihn danach nutzen
	info = RoomInfo{}
	res = post(t, ts.URL+"/api/bot/rooms", tok, `{"name":"news","description":"Neuigkeiten","voice":{"preset":"hoch"}}`)
	json.NewDecoder(res.Body).Decode(&info)
	if res.StatusCode != 201 || info.Temp != nil || info.Voice == nil || info.Voice.Preset != "hoch" {
		t.Fatalf("dauerhaft: %d %+v", res.StatusCode, info)
	}
	if res := post(t, ts.URL+"/api/bot/rooms/news/messages", tok, `{"text":"Hallo"}`); res.StatusCode != 201 {
		t.Fatalf("schreiben im eigenen Kanal: %d", res.StatusCode)
	}
	// ändern: eigener ja, fremder nein
	if res := do(t, "PATCH", ts.URL+"/api/bot/rooms/news", tok, `{"name":"nachrichten","voice":null}`); res.StatusCode != 200 {
		t.Fatalf("umbenennen: %d", res.StatusCode)
	}
	if i, ok := h.roomInfo("nachrichten"); !ok || i.Voice != nil || i.CreatedBy == "" {
		t.Fatalf("nach dem Ändern: %+v", i)
	}
	if res := do(t, "PATCH", ts.URL+"/api/bot/rooms/nachrichten", tok2, `{"description":"fremd"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("fremder Bot: %d", res.StatusCode)
	}
	name := "lobby"
	h.CreateRoomWith(RoomInput{Name: &name})
	if res := do(t, "DELETE", ts.URL+"/api/bot/rooms/lobby", tok, ``); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Lobby löschen: %d", res.StatusCode)
	}
	if res := do(t, "DELETE", ts.URL+"/api/bot/rooms/treffen", tok2, ``); res.StatusCode != http.StatusForbidden {
		t.Fatalf("fremden temporären Kanal löschen: %d", res.StatusCode)
	}
	if res := do(t, "DELETE", ts.URL+"/api/bot/rooms/nachrichten", tok, ``); res.StatusCode != http.StatusNoContent {
		t.Fatalf("löschen: %d", res.StatusCode)
	}
	if _, ok := h.roomInfo("nachrichten"); ok {
		t.Fatal("noch da")
	}
	_ = anna
}

func TestTempConfigAndMigration(t *testing.T) {
	h := NewHub(0)
	if _, err := h.SetTempRooms(TempRoomsConfig{IdleMinutes: 5000}); err == nil {
		t.Fatal("Frist zu lang angenommen")
	}
	if _, err := h.SetTempRooms(TempRoomsConfig{Group: "gibtsnicht"}); err == nil {
		t.Fatal("unbekannte Gruppe angenommen")
	}
	c, err := h.SetTempRooms(TempRoomsConfig{Enabled: true})
	if err != nil || c.IdleMinutes != 5 || c.MaxTotal != 20 || c.MaxPerUser != 1 || c.Voice != nil {
		t.Fatalf("%+v %v", c, err)
	}
	// alte Befehls-Einstellungen bekommen den neuen Befehl
	cmds := NewCommands()
	old := defaultCommands()
	old.Version = 2
	var keep []Command
	for _, c := range old.Commands {
		if c.Kind != CmdTempRoom {
			keep = append(keep, c)
		}
	}
	old.Commands = keep
	if err := cmds.restore(old); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := cmds.Match("!kanal test", "lobby", false, CmdTempRoom); !ok {
		t.Fatal("Befehl fehlt nach dem Update")
	}
}
