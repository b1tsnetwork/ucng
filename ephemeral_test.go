// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEphemeralCommands(t *testing.T) {
	ts, srv := testServerWith(t, nil)
	adm := adminLogin(t, ts.URL)
	paul, ida := claim(t, ts.URL, "Paul"), claim(t, ts.URL, "Ida")
	base := ts.URL + "/api/rooms/lobby/messages"

	// mitlauschen: kein Ereignis im Kanal
	ch := srv.hub.Listen(16)
	defer srv.hub.Unlisten(ch)

	type ephResp struct {
		Ephemeral *EphemeralReply `json:"ephemeral"`
	}
	r := decode[ephResp](t, expect(t, post(t, base, paul, `{"text":"!regeln"}`), 200, "privat"))
	if r.Ephemeral == nil || r.Ephemeral.Name != "Chat-Bot" || !strings.Contains(r.Ephemeral.Text, "Danke, Paul!") ||
		r.Ephemeral.Command != "!regeln" || r.Ephemeral.Notice {
		t.Fatalf("%+v", r.Ephemeral)
	}
	if n := len(hist(srv.hub, "lobby")); n != 0 {
		t.Fatalf("im Kanal gelandet: %d", n)
	}
	select {
	case ev := <-ch:
		t.Fatalf("Ereignis verschickt: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	// Abklingzeit gilt je Person: Paul muss warten, Ida nicht
	r = decode[ephResp](t, expect(t, post(t, base, paul, `{"text":"!rules"}`), 200, "Abklingzeit"))
	if !r.Ephemeral.Notice || !strings.Contains(r.Ephemeral.Text, "warten") {
		t.Fatalf("%+v", r.Ephemeral)
	}
	r = decode[ephResp](t, expect(t, post(t, base, ida, `{"text":"!regeln"}`), 200, "Ida"))
	if r.Ephemeral.Notice {
		t.Fatalf("Ida ausgebremst: %+v", r.Ephemeral)
	}
	// öffentliche Befehle bleiben öffentlich
	expect(t, post(t, base, paul, `{"text":"!würfel"}`), 201, "öffentlich").Body.Close()
	if n := len(hist(srv.hub, "lobby")); n != 2 {
		t.Fatalf("öffentlich: %d", n)
	}
	// gesperrte Personen bekommen keine private Antwort
	expect(t, do(t, "POST", ts.URL+"/api/admin/users/"+sess(t, ts.URL, ida).ID+"/sanction", adm, `{"kind":"mute","minutes":10}`), 200, "stumm").Body.Close()
	expect(t, post(t, base, ida, `{"text":"!hilfe"}`), 403, "stumm")
	// unbekannter Kanal
	expect(t, post(t, ts.URL+"/api/rooms/gibtsnicht/messages", paul, `{"text":"!hilfe"}`), 404, "Kanal")

	// Einstellung umschalten: !regeln wieder öffentlich
	cfg := srv.hub.Commands().Config()
	for i := range cfg.Commands {
		if cfg.Commands[i].Trigger == "regeln" {
			if !cfg.Commands[i].Ephemeral {
				t.Fatal("Standard sollte privat sein")
			}
			cfg.Commands[i].Ephemeral, cfg.Commands[i].Cooldown = false, 0
		}
	}
	body, _ := json.Marshal(cfg)
	expect(t, do(t, "PUT", ts.URL+"/api/admin/commands", adm, string(body)), 200, "speichern").Body.Close()
	expect(t, post(t, base, paul, `{"text":"!regeln"}`), 201, "jetzt öffentlich").Body.Close()
	if n := len(hist(srv.hub, "lobby")); n != 4 {
		t.Fatalf("öffentlich: %d", n)
	}
	// Testfeld meldet privat
	tr := decode[CmdResult](t, do(t, "POST", ts.URL+"/api/admin/commands/test", adm, `{"text":"!hilfe"}`))
	if !tr.Matched || !tr.Ephemeral {
		t.Fatalf("%+v", tr)
	}
}
