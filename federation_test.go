// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fedServer startet eine Instanz im Modus „hosted“ mit eingeschalteter Föderation (über http).
func fedServer(t *testing.T) *hostedEnv {
	t.Helper()
	e := hostedServer(t)
	e.srv.cfg.PublicURL = e.ts.URL
	e.srv.cfg.FedInsecure = true
	e.srv.fedRL = newRateLimiter(1000, time.Minute) // Standard: 120/min
	e.hub.Hosted().Set(HostedConfig{Guests: GuestsRead, Registration: true, Fed: FedConfig{Enabled: true}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.srv.runFed(ctx)
	return e
}

func (e *hostedEnv) domain() string { return e.srv.fedDomain() }

// e2eMsg baut eine (nicht wirklich verschlüsselte) Nutzlast mit den richtigen Feldern.
func e2eMsg(spk, rpk string) string {
	return jsonBody(e2ePayload{V: 1, IV: b64(rnd(12)), CT: b64(rnd(40)), SPK: spk, RPK: rpk})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("Zeitüberschreitung: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func dmList(t *testing.T, e *hostedEnv, tok string) []DMInfo {
	t.Helper()
	res := do(t, "GET", e.ts.URL+"/api/dms", tok, "")
	var out struct{ DMs []DMInfo }
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return out.DMs
}

func history(t *testing.T, e *hostedEnv, tok, room string) []Message {
	t.Helper()
	res := do(t, "GET", e.ts.URL+"/api/rooms/"+room+"/messages", tok, "")
	var out []Message
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return out
}

func TestE2ELocalDM(t *testing.T) {
	e := hostedServer(t)
	ka, kb := testKeys(), testKeys()
	a := register(t, e, "anna", "", ka)
	b := register(t, e, "bert", "", kb)
	ta, tb := sessionToken(a), sessionToken(b)
	bid := b["session"].(map[string]any)["id"].(string)
	var dm DMInfo
	json.NewDecoder(post(t, e.ts.URL+"/api/dms", ta, jsonBody(map[string]string{"userId": bid})).Body).Decode(&dm)
	if dm.Room == "" {
		t.Fatal("DM nicht geöffnet")
	}
	key := jmap(t, do(t, "GET", e.ts.URL+"/api/dms/"+dm.Room+"/key", ta, ""))
	if key["e2e"] != true || key["pubKey"] != kb.PubKey {
		t.Fatalf("Schlüssel: %v", key)
	}
	if res := post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, `{"text":"Klartext"}`); res.StatusCode < 400 {
		t.Fatalf("Klartext angenommen: %d", res.StatusCode)
	}
	if res := post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, jsonBody(map[string]string{"e2e": "kaputt"})); res.StatusCode != 400 {
		t.Fatalf("kaputte Nutzlast: %d", res.StatusCode)
	}
	payload := e2eMsg(ka.PubKey, kb.PubKey)
	res := post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, jsonBody(map[string]string{"e2e": payload}))
	if res.StatusCode != 201 {
		t.Fatalf("verschlüsselt: %d", res.StatusCode)
	}
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	// Antwort mit Vorschau: der Browser bekommt den Geheimtext der beantworteten Nachricht
	res = post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages", tb, jsonBody(map[string]any{"e2e": e2eMsg(kb.PubKey, ka.PubKey), "replyTo": m.ID}))
	var reply Message
	json.NewDecoder(res.Body).Decode(&reply)
	if reply.Reply == nil || reply.Reply.E2E != payload || reply.Reply.Text != "" {
		t.Fatalf("Antwortvorschau: %+v", reply.Reply)
	}
	h := history(t, e, tb, dm.Room)
	if len(h) != 2 || h[0].E2E != payload || h[0].Text != e2ePlaceholder {
		t.Fatalf("Verlauf: %+v", h)
	}
	// Bearbeiten nur mit neuer Nutzlast
	if res := do(t, "PATCH", e.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(m.ID), ta, `{"text":"neu"}`); res.StatusCode < 400 {
		t.Fatalf("Klartext-Bearbeitung: %d", res.StatusCode)
	}
	p2 := e2eMsg(ka.PubKey, kb.PubKey)
	if res := do(t, "PATCH", e.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(m.ID), ta, jsonBody(map[string]string{"e2e": p2})); res.StatusCode != 200 {
		t.Fatalf("Bearbeiten: %d", res.StatusCode)
	}
	if h := history(t, e, tb, dm.Room); h[0].E2E != p2 || h[0].EditedAt == nil {
		t.Fatalf("bearbeitet: %+v", h[0])
	}
	// keine Bilder/GIFs
	if res := post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, `{"gif":"lib:x"}`); res.StatusCode < 400 {
		t.Fatalf("GIF angenommen: %d", res.StatusCode)
	}
	// Meldung mit entschlüsseltem Text
	if res := post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(m.ID)+"/report", tb, `{"reason":"Spam","plaintext":"böser Text"}`); res.StatusCode != 201 {
		t.Fatalf("melden: %d", res.StatusCode)
	}
	if reps := e.hub.Reports().List(true); len(reps) != 1 || reps[0].Plaintext != "böser Text" {
		t.Fatalf("Meldung: %+v", reps)
	}
	// verschlüsselt nur in DMs
	if res := post(t, e.ts.URL+"/api/rooms/lobby/messages", ta, jsonBody(map[string]string{"e2e": payload})); res.StatusCode != 400 {
		t.Fatalf("E2E im Kanal: %d", res.StatusCode)
	}
	// gelöscht: Geheimtext verschwindet
	do(t, "DELETE", e.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(m.ID), ta, "")
	if h := history(t, e, tb, dm.Room); h[0].E2E != "" || !h[0].Deleted {
		t.Fatalf("gelöscht: %+v", h[0])
	}
}

func TestE2EGuestDMStaysPlain(t *testing.T) {
	e := hostedServer(t)
	e.hub.Hosted().Set(HostedConfig{Guests: GuestsWrite, Registration: true})
	a := register(t, e, "anna", "", testKeys())
	g := claim(t, e.ts.URL, "Gast")
	var dm DMInfo
	json.NewDecoder(post(t, e.ts.URL+"/api/dms", g, jsonBody(map[string]string{"userId": a["session"].(map[string]any)["id"].(string)})).Body).Decode(&dm)
	if res := post(t, e.ts.URL+"/api/rooms/"+dm.Room+"/messages", g, `{"text":"Hallo"}`); res.StatusCode != 201 {
		t.Fatalf("Gast-DM: %d", res.StatusCode)
	}
}

func TestFederatedDM(t *testing.T) {
	A, B := fedServer(t), fedServer(t)
	ka, kb := testKeys(), testKeys()
	ta := sessionToken(register(t, A, "anna", "", ka))
	tb := sessionToken(register(t, B, "bob", "", kb))

	for path, software := range map[string]string{"/.well-known/ucng": "ucng", "/.well-known/gochat": "gochat"} { // neuer und früherer Name
		wk := jmap(t, do(t, "GET", B.ts.URL+path, "", ""))
		if wk["domain"] != B.domain() || wk["key"] != B.hub.Fed().PublicKey() || wk["software"] != software {
			t.Fatalf("well-known %s: %v", path, wk)
		}
	}
	lk := jmap(t, do(t, "GET", A.ts.URL+"/api/fed/lookup?addr=bob@"+B.domain(), ta, ""))
	if lk["pubKey"] != kb.PubKey {
		t.Fatalf("lookup: %v", lk)
	}
	if res := do(t, "GET", A.ts.URL+"/api/fed/lookup?addr=niemand@"+B.domain(), ta, ""); res.StatusCode != 404 {
		t.Fatalf("unbekannt: %d", res.StatusCode)
	}
	var dm DMInfo
	json.NewDecoder(post(t, A.ts.URL+"/api/dms", ta, jsonBody(map[string]string{"address": "Bob@" + B.domain()})).Body).Decode(&dm)
	if !dm.Fed || dm.Name != "bob@"+B.domain() {
		t.Fatalf("DM öffnen: %+v", dm)
	}
	if res := post(t, A.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, `{"text":"Klartext"}`); res.StatusCode < 400 {
		t.Fatalf("Klartext an andere Instanz: %d", res.StatusCode)
	}
	p1 := e2eMsg(ka.PubKey, kb.PubKey)
	res := post(t, A.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, jsonBody(map[string]string{"e2e": p1}))
	var sent Message
	json.NewDecoder(res.Body).Decode(&sent)
	if res.StatusCode != 201 || sent.Fed == nil || sent.Fed.State != "pending" {
		t.Fatalf("senden: %d %+v", res.StatusCode, sent.Fed)
	}
	// kommt bei B an
	var bdm DMInfo
	waitFor(t, "Nachricht bei B", func() bool {
		for _, d := range dmList(t, B, tb) {
			if d.Fed && d.Name == "anna@"+A.domain() && d.Messages == 1 {
				bdm = d
				return true
			}
		}
		return false
	})
	got := history(t, B, tb, bdm.Room)
	if got[0].E2E != p1 || got[0].UserID != "fed:anna@"+A.domain() || got[0].Name != "anna@"+A.domain() {
		t.Fatalf("bei B: %+v", got[0])
	}
	waitFor(t, "Status sent", func() bool {
		h := history(t, A, ta, dm.Room)
		return h[0].Fed != nil && h[0].Fed.State == "sent"
	})
	// Antwort zurück
	p2 := e2eMsg(kb.PubKey, ka.PubKey)
	if res := post(t, B.ts.URL+"/api/rooms/"+bdm.Room+"/messages", tb, jsonBody(map[string]string{"e2e": p2})); res.StatusCode != 201 {
		t.Fatalf("Antwort: %d", res.StatusCode)
	}
	waitFor(t, "Antwort bei A", func() bool { return len(history(t, A, ta, dm.Room)) == 2 })
	// Bearbeiten und Löschen werden übertragen
	p3 := e2eMsg(ka.PubKey, kb.PubKey)
	do(t, "PATCH", A.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(sent.ID), ta, jsonBody(map[string]string{"e2e": p3}))
	waitFor(t, "Bearbeitung bei B", func() bool { return history(t, B, tb, bdm.Room)[0].E2E == p3 })
	do(t, "DELETE", A.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(sent.ID), ta, "")
	waitFor(t, "Löschung bei B", func() bool { return history(t, B, tb, bdm.Room)[0].Deleted })
	// Reaktionen gehen instanzübergreifend nicht
	if res := post(t, A.ts.URL+"/api/rooms/"+dm.Room+"/messages/"+itoa(sent.ID)+"/react", ta, `{"emoji":"👍"}`); res.StatusCode < 400 {
		t.Fatalf("Reaktion: %d", res.StatusCode)
	}

	// Gefälschte, wiederholte und veraltete Anfragen
	body := []byte(jsonBody(fedPayload{Type: "message", From: "anna", FromKey: ka.PubKey, To: "bob", ID: 999, E2E: e2eMsg(ka.PubKey, kb.PubKey)}))
	send := func(sigDomain string, ts time.Time, tamper bool) int {
		target := B.domain() + fedInboxPath
		tsS, sig := A.hub.Fed().sign(sigDomain, "POST", target, body, ts)
		b := body
		if tamper {
			b = bytes.Replace(body, []byte(`"id":999`), []byte(`"id":998`), 1)
		}
		req, _ := http.NewRequest("POST", B.ts.URL+fedInboxPath, bytes.NewReader(b))
		req.Header.Set("X-GoChat-Domain", A.domain())
		req.Header.Set("X-GoChat-Date", tsS)
		req.Header.Set("X-GoChat-Signature", sig)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := send(A.domain(), time.Now(), true); c != 401 {
		t.Fatalf("manipuliert: %d", c)
	}
	if c := send(A.domain(), time.Now().Add(-10*time.Minute), false); c != 401 {
		t.Fatalf("veraltet: %d", c)
	}
	now := time.Now()
	if c := send(A.domain(), now, false); c != 202 {
		t.Fatalf("echt: %d", c)
	}
	n := len(history(t, B, tb, bdm.Room))
	if c := send(A.domain(), now, false); c != 202 || len(history(t, B, tb, bdm.Room)) != n {
		t.Fatalf("Wiederholung: %d", c)
	}

	// Bob nimmt keine Nachrichten von anderen Instanzen mehr an
	post(t, B.ts.URL+"/api/account/federation", tb, `{"allow":false}`)
	B.hub.Fed().mu.Lock()
	B.hub.Fed().users = map[string]fedUser{}
	B.hub.Fed().mu.Unlock()
	A.hub.Fed().mu.Lock()
	A.hub.Fed().users = map[string]fedUser{}
	A.hub.Fed().mu.Unlock()
	if res := do(t, "GET", A.ts.URL+"/api/fed/lookup?addr=bob@"+B.domain(), ta, ""); res.StatusCode != 404 {
		t.Fatalf("noFed lookup: %d", res.StatusCode)
	}
	post(t, B.ts.URL+"/api/account/federation", tb, `{"allow":true}`)

	// B sperrt A: Zustellung scheitert endgültig
	B.hub.Hosted().Set(HostedConfig{Guests: GuestsRead, Registration: true, Fed: FedConfig{Enabled: true, Deny: []string{strings.Split(A.domain(), ":")[0] + ":" + strings.Split(A.domain(), ":")[1]}}})
	res = post(t, A.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, jsonBody(map[string]string{"e2e": e2eMsg(ka.PubKey, kb.PubKey)}))
	json.NewDecoder(res.Body).Decode(&sent)
	waitFor(t, "Status failed", func() bool {
		for _, m := range history(t, A, ta, dm.Room) {
			if m.ID == sent.ID {
				return m.Fed != nil && m.Fed.State == "failed"
			}
		}
		return false
	})
}

func TestFederationKeyPinning(t *testing.T) {
	A, B := fedServer(t), fedServer(t)
	ka, kb := testKeys(), testKeys()
	ta := sessionToken(register(t, A, "anna", "", ka))
	tb := sessionToken(register(t, B, "bob", "", kb))
	var dm DMInfo
	json.NewDecoder(post(t, A.ts.URL+"/api/dms", ta, jsonBody(map[string]string{"address": "bob@" + B.domain()})).Body).Decode(&dm)
	post(t, A.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, jsonBody(map[string]string{"e2e": e2eMsg(ka.PubKey, kb.PubKey)}))
	waitFor(t, "erste Nachricht", func() bool { return len(dmList(t, B, tb)) == 1 })

	// A bekommt einen neuen Serverschlüssel → B lehnt ab, bis der Admin den alten vergisst
	fresh := NewFedStore()
	st := A.hub.Fed().state()
	st.Seed = fresh.state().Seed
	A.hub.Fed().restore(st)
	res := post(t, A.ts.URL+"/api/rooms/"+dm.Room+"/messages", ta, jsonBody(map[string]string{"e2e": e2eMsg(ka.PubKey, kb.PubKey)}))
	var m Message
	json.NewDecoder(res.Body).Decode(&m)
	time.Sleep(300 * time.Millisecond)
	if h := history(t, B, tb, dmList(t, B, tb)[0].Room); len(h) != 1 {
		t.Fatalf("mit neuem Schlüssel angenommen: %d", len(h))
	}
	adm := adminLogin(t, B.ts.URL)
	info := jmap(t, do(t, "GET", B.ts.URL+"/api/admin/federation", adm, ""))
	if len(info["peers"].([]any)) != 1 {
		t.Fatalf("Peers: %v", info)
	}
	if res := do(t, "DELETE", B.ts.URL+"/api/admin/federation/peers/"+A.domain(), adm, ""); res.StatusCode != 204 {
		t.Fatalf("vergessen: %d", res.StatusCode)
	}
	// erneuter Versuch (nach Backoff) – hier direkt anstoßen
	A.hub.Fed().mu.Lock()
	for i := range A.hub.Fed().queue {
		A.hub.Fed().queue[i].Next = time.Now()
	}
	A.hub.Fed().mu.Unlock()
	A.hub.Fed().wake <- struct{}{}
	waitFor(t, "nach Vergessen angenommen", func() bool { return len(history(t, B, tb, dmList(t, B, tb)[0].Room)) == 2 })
}

func TestFedAddrParsing(t *testing.T) {
	for _, c := range []struct{ in, name, domain string }{
		{"anna@chat.example.org", "anna", "chat.example.org"},
		{"@Anna@Chat.Example.org", "Anna", "chat.example.org"},
		{"bob@127.0.0.1:8080", "bob", "127.0.0.1:8080"},
	} {
		n, d, err := parseFedAddr(c.in)
		if err != nil || n != c.name || d != c.domain {
			t.Errorf("%s → %s %s %v", c.in, n, d, err)
		}
	}
	for _, bad := range []string{"anna", "anna@", "@x", "an na@x.de", "a@b/c", "a@..de", "a@-x.de"} {
		if _, _, err := parseFedAddr(bad); err == nil {
			t.Errorf("%q sollte ungültig sein", bad)
		}
	}
}
