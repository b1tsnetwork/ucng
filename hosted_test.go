// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type hostedEnv struct {
	ts    *httptest.Server
	hub   *Hub
	srv   *Server
	users *Users
	clock *testClock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func hostedServer(t *testing.T) *hostedEnv {
	t.Helper()
	cfg := Config{MaxRooms: 5, RateLimit: 100, RateWindow: time.Second, ClaimLimit: 100, AdminPassword: testAdminPW, BotRate: 1000,
		Mode: ModeHosted, PublicURL: "https://chat.example.org"}
	hub := NewHub(cfg.MaxRooms)
	users := NewUsers(time.Hour, 100)
	clock := &testClock{t: time.Now()}
	users.now = clock.now
	srv := NewServer(cfg, hub, users)
	srv.acctRL = newRateLimiter(1000, time.Minute)
	srv.mailRL = newRateLimiter(1000, time.Minute)
	hub.EnsureDefaultRoom()
	ctx, cancel := context.WithCancel(context.Background())
	go srv.webhooks.run(ctx)
	t.Cleanup(cancel)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(func() { close(srv.done); ts.Close() })
	return &hostedEnv{ts, hub, srv, users, clock}
}

func rnd(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// testKeys simuliert, was der Browser aus einem Passwort ableitet.
func testKeys() KeyMaterial {
	pub := append([]byte{4}, rnd(64)...)
	return KeyMaterial{Auth: b64(rnd(32)), KDFSalt: b64(rnd(16)), KDFIter: defaultKDFIter, PubKey: b64(pub), PrivKey: b64(rnd(60))}
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jmap(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return out
}

func register(t *testing.T, e *hostedEnv, name, email string, k KeyMaterial) map[string]any {
	t.Helper()
	res := do(t, "POST", e.ts.URL+"/api/account/register", "", jsonBody(map[string]any{"name": name, "email": email, "keys": k}))
	out := jmap(t, res)
	if res.StatusCode >= 300 {
		t.Fatalf("Registrierung %s: %d %v", name, res.StatusCode, out)
	}
	return out
}

func sessionToken(out map[string]any) string {
	if s, ok := out["session"].(map[string]any); ok {
		tok, _ := s["token"].(string)
		return tok
	}
	return ""
}

func login(t *testing.T, e *hostedEnv, login, auth, code, recovery string) (*http.Response, map[string]any) {
	t.Helper()
	res := do(t, "POST", e.ts.URL+"/api/account/login", "", jsonBody(map[string]any{"login": login, "auth": auth, "code": code, "recovery": recovery}))
	return res, jmap(t, res)
}

func TestHostedSimpleModeHasNoAccounts(t *testing.T) {
	ts, _ := testServer(t)
	if res := do(t, "POST", ts.URL+"/api/account/login", "", `{}`); res.StatusCode != 404 {
		t.Fatalf("simple: %d", res.StatusCode)
	}
	res := do(t, "GET", ts.URL+"/api/settings", "", "")
	if out := jmap(t, res); out["appMode"] != "simple" || out["hosted"] != nil {
		t.Fatalf("settings: %v", out)
	}
}

func TestHostedRegisterLoginAndGuests(t *testing.T) {
	e := hostedServer(t)
	base := e.ts.URL
	out := jmap(t, do(t, "GET", base+"/api/settings", "", ""))
	if out["appMode"] != "hosted" || out["hosted"].(map[string]any)["guests"] != "read" {
		t.Fatalf("settings: %v", out)
	}
	// Gäste dürfen standardmäßig nur lesen
	if res := do(t, "POST", base+"/api/session", "", `{"name":"Gast"}`); res.StatusCode != 403 {
		t.Fatalf("Gastname: %d", res.StatusCode)
	}
	if res := do(t, "GET", base+"/api/rooms", "", ""); res.StatusCode != 200 {
		t.Fatalf("lesen als Gast: %d", res.StatusCode)
	}

	k := testKeys()
	out = register(t, e, "anna", "", k)
	tok := sessionToken(out)
	if tok == "" || len(out["recovery"].([]any)) != recoveryCount || out["pubKey"] != k.PubKey || out["privKey"] != k.PrivKey {
		t.Fatalf("Registrierung: %v", out)
	}
	if res := post(t, base+"/api/rooms/lobby/messages", tok, `{"text":"Hallo mit Konto"}`); res.StatusCode != 201 && res.StatusCode != 200 {
		t.Fatalf("schreiben: %d", res.StatusCode)
	}
	// Name ist vergeben – für Konten und Gäste
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "Anna", "keys": testKeys()})); res.StatusCode != 409 {
		t.Fatalf("doppelt: %d", res.StatusCode)
	}
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "an na", "keys": testKeys()})); res.StatusCode != 400 {
		t.Fatalf("ungültiger Name: %d", res.StatusCode)
	}
	// Konten können nicht umbenannt werden
	e.hub.Hosted().Set(HostedConfig{Guests: GuestsWrite, Registration: true})
	if res := do(t, "POST", base+"/api/session", tok, `{"name":"bert"}`); res.StatusCode != 400 {
		t.Fatalf("Umbenennen: %d", res.StatusCode)
	}
	if res := do(t, "POST", base+"/api/session", "", `{"name":"ANNA"}`); res.StatusCode != 409 {
		t.Fatalf("Gast mit Kontonamen: %d", res.StatusCode)
	}
	guest := claim(t, base, "Gast")
	if res := post(t, base+"/api/rooms/lobby/messages", guest, `{"text":"als Gast"}`); res.StatusCode >= 300 {
		t.Fatalf("Gast schreibt: %d", res.StatusCode)
	}
	// Gäste sehen nichts mehr
	e.hub.Hosted().Set(HostedConfig{Guests: GuestsNone, Registration: true})
	for _, p := range []string{"/api/rooms", "/api/rooms/lobby/messages"} {
		if res := do(t, "GET", base+p, "", ""); res.StatusCode != 401 {
			t.Fatalf("%s ohne Konto: %d", p, res.StatusCode)
		}
		if res := do(t, "GET", base+p, guest, ""); res.StatusCode != 401 {
			t.Fatalf("%s als Gast: %d", p, res.StatusCode)
		}
		if res := do(t, "GET", base+p, tok, ""); res.StatusCode != 200 {
			t.Fatalf("%s mit Konto: %d", p, res.StatusCode)
		}
	}
	if res := post(t, base+"/api/rooms/lobby/messages", guest, `{"text":"als Gast"}`); res.StatusCode != 401 {
		t.Fatalf("Gast schreibt trotz none: %d", res.StatusCode)
	}

	// Prelogin: echte Werte für Konten, stabile erfundene für Unbekannte
	pre := jmap(t, do(t, "POST", base+"/api/account/prelogin", "", `{"login":"anna"}`))
	if pre["kdfSalt"] != k.KDFSalt {
		t.Fatalf("prelogin: %v", pre)
	}
	p1 := jmap(t, do(t, "POST", base+"/api/account/prelogin", "", `{"login":"niemand"}`))
	p2 := jmap(t, do(t, "POST", base+"/api/account/prelogin", "", `{"login":"Niemand"}`))
	if p1["kdfSalt"] != p2["kdfSalt"] || p1["kdfSalt"] == "" {
		t.Fatal("erfundene Werte nicht stabil")
	}

	// Abmelden beendet nur diese Sitzung
	tok2 := sessionToken(func() map[string]any { _, o := login(t, e, "anna", k.Auth, "", ""); return o }())
	if do(t, "DELETE", base+"/api/session", tok, "").StatusCode != 204 {
		t.Fatal("abmelden")
	}
	if res := do(t, "GET", base+"/api/session", tok, ""); res.StatusCode != 401 {
		t.Fatalf("altes Token: %d", res.StatusCode)
	}
	if res := do(t, "GET", base+"/api/session", tok2, ""); res.StatusCode != 200 {
		t.Fatalf("zweite Sitzung: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "anna", b64(rnd(32)), "", ""); res.StatusCode != 401 {
		t.Fatalf("falsches Passwort: %d", res.StatusCode)
	}
	acc := jmap(t, do(t, "GET", base+"/api/account", tok2, ""))
	if acc["name"] != "anna" || len(acc["sessions"].([]any)) != 1 {
		t.Fatalf("Konto: %v", acc)
	}
	// Konten laufen nicht ab
	e.clock.add(1000 * time.Hour)
	e.users.Expire()
	if res := do(t, "GET", base+"/api/session", tok2, ""); res.StatusCode != 401 {
		t.Fatalf("Sitzung nach 41 Tagen: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "anna", k.Auth, "", ""); res.StatusCode != 200 {
		t.Fatalf("Konto nach langer Zeit: %d", res.StatusCode)
	}
}

func TestHosted2FAAndRecovery(t *testing.T) {
	e := hostedServer(t)
	base := e.ts.URL
	k := testKeys()
	out := register(t, e, "bert", "", k)
	tok := sessionToken(out)
	codes := out["recovery"].([]any)

	if res := post(t, base+"/api/account/2fa/begin", tok, jsonBody(map[string]string{"auth": b64(rnd(32))})); res.StatusCode != 401 {
		t.Fatalf("begin ohne Passwort: %d", res.StatusCode)
	}
	beg := jmap(t, post(t, base+"/api/account/2fa/begin", tok, jsonBody(map[string]string{"auth": k.Auth})))
	secret := beg["secret"].(string)
	if !strings.HasPrefix(beg["uri"].(string), "otpauth://totp/") || !strings.HasPrefix(beg["svg"].(string), "<svg") {
		t.Fatalf("begin: %v", beg)
	}
	if res := post(t, base+"/api/account/2fa/enable", tok, `{"code":"000000"}`); res.StatusCode != 401 {
		t.Fatalf("falscher Code: %d", res.StatusCode)
	}
	step := e.clock.now().Unix() / 30
	en := jmap(t, post(t, base+"/api/account/2fa/enable", tok, jsonBody(map[string]string{"code": totpCode(secret, step)})))
	if len(en["recovery"].([]any)) != recoveryCount {
		t.Fatalf("enable: %v", en)
	}
	newCodes := en["recovery"].([]any)

	// ohne Code: Hinweis auf 2FA
	res, o := login(t, e, "bert", k.Auth, "", "")
	if res.StatusCode != 401 || o["need2fa"] != true {
		t.Fatalf("2FA nötig: %d %v", res.StatusCode, o)
	}
	// derselbe Code zählt kein zweites Mal
	if res, _ := login(t, e, "bert", k.Auth, totpCode(secret, step), ""); res.StatusCode != 401 {
		t.Fatalf("Code wiederverwendet: %d", res.StatusCode)
	}
	e.clock.add(30 * time.Second)
	if res, _ := login(t, e, "bert", k.Auth, totpCode(secret, step+1), ""); res.StatusCode != 200 {
		t.Fatalf("mit Code: %d", res.StatusCode)
	}
	// alte Wiederherstellungscodes gelten nicht mehr, neue genau einmal
	if res, _ := login(t, e, "bert", k.Auth, "", codes[0].(string)); res.StatusCode != 401 {
		t.Fatalf("alter Code: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "bert", k.Auth, "", strings.ToUpper(newCodes[0].(string))); res.StatusCode != 200 {
		t.Fatalf("Wiederherstellungscode: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "bert", k.Auth, "", newCodes[0].(string)); res.StatusCode != 401 {
		t.Fatalf("Code zweimal: %d", res.StatusCode)
	}

	// Passwort vergessen ohne E-Mail: mit Wiederherstellungscode und neuen Schlüsseln
	k2 := testKeys()
	if res := post(t, base+"/api/account/reset", "", jsonBody(map[string]any{"login": "bert", "recovery": "falsch", "keys": k2})); res.StatusCode != 401 {
		t.Fatalf("Reset mit falschem Code: %d", res.StatusCode)
	}
	if res := post(t, base+"/api/account/reset", "", jsonBody(map[string]any{"login": "bert", "recovery": newCodes[1], "keys": k2})); res.StatusCode != 200 {
		t.Fatalf("Reset: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "bert", k.Auth, "", newCodes[2].(string)); res.StatusCode != 401 {
		t.Fatalf("altes Passwort: %d", res.StatusCode)
	}
	res, o = login(t, e, "bert", k2.Auth, "", newCodes[2].(string))
	if res.StatusCode != 200 || o["pubKey"] != k2.PubKey {
		t.Fatalf("neues Passwort: %d %v", res.StatusCode, o)
	}
	tok = sessionToken(o)

	// Passwort ändern: Schlüsselpaar bleibt, privater Schlüssel neu verpackt
	k3 := testKeys()
	k3.PubKey = k2.PubKey
	if res := post(t, base+"/api/account/password", tok, jsonBody(map[string]any{"auth": k2.Auth, "keys": k3})); res.StatusCode != 401 {
		t.Fatalf("Passwort ändern ohne 2FA-Code: %d", res.StatusCode)
	}
	e.clock.add(30 * time.Second)
	if res := post(t, base+"/api/account/password", tok, jsonBody(map[string]any{"auth": k2.Auth, "code": totpCode(secret, e.clock.now().Unix()/30), "keys": k3})); res.StatusCode != 204 {
		t.Fatalf("Passwort ändern: %d", res.StatusCode)
	}
	// 2FA neu einrichten oder neue Codes nur mit aktuellem Code
	if res := post(t, base+"/api/account/2fa/begin", tok, jsonBody(map[string]string{"auth": k3.Auth})); res.StatusCode != 401 {
		t.Fatalf("2FA neu ohne Code: %d", res.StatusCode)
	}
	if res := post(t, base+"/api/account/recovery", tok, jsonBody(map[string]string{"auth": k3.Auth})); res.StatusCode != 401 {
		t.Fatalf("neue Codes ohne Code: %d", res.StatusCode)
	}
	e.clock.add(30 * time.Second)
	res, o = login(t, e, "bert", k3.Auth, totpCode(secret, e.clock.now().Unix()/30), "")
	if res.StatusCode != 200 || o["privKey"] != k3.PrivKey {
		t.Fatalf("nach Passwortwechsel: %d %v", res.StatusCode, o)
	}
	// 2FA abschalten
	e.clock.add(30 * time.Second)
	if res := post(t, base+"/api/account/2fa/disable", tok, jsonBody(map[string]string{"auth": k3.Auth, "code": totpCode(secret, e.clock.now().Unix()/30)})); res.StatusCode != 204 {
		t.Fatalf("2FA aus: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "bert", k3.Auth, "", ""); res.StatusCode != 200 {
		t.Fatalf("ohne 2FA: %d", res.StatusCode)
	}
}

func TestHostedEmailVerifyAndReset(t *testing.T) {
	e := hostedServer(t)
	base := e.ts.URL
	var mu sync.Mutex
	var mails []string
	old := sendMail
	sendMail = func(c SMTPConfig, to, subject, body string) error {
		mu.Lock()
		mails = append(mails, to+"\n"+subject+"\n"+body)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { sendMail = old })
	lastMail := func() string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			n := len(mails)
			var m string
			if n > 0 {
				m = mails[n-1]
			}
			mu.Unlock()
			if n > 0 || time.Now().After(deadline) {
				return m
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if _, err := e.hub.Hosted().Set(HostedConfig{Guests: GuestsRead, Registration: true, RequireVerify: true,
		SMTP: SMTPConfig{Host: "mail.example.org", Port: 587, From: "GoChat <chat@example.org>", Security: "starttls"}}); err != nil {
		t.Fatal(err)
	}
	k := testKeys()
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "cleo", "keys": k})); res.StatusCode != 400 {
		t.Fatalf("ohne E-Mail: %d", res.StatusCode)
	}
	out := register(t, e, "cleo", "cleo@example.org", k)
	if out["verify"] != true || sessionToken(out) != "" {
		t.Fatalf("Bestätigung nötig: %v", out)
	}
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "cleo2", "email": "CLEO@example.org", "keys": testKeys()})); res.StatusCode != 409 {
		t.Fatalf("E-Mail doppelt: %d", res.StatusCode)
	}
	res, o := login(t, e, "cleo", k.Auth, "", "")
	if res.StatusCode != 403 || o["unverified"] != true {
		t.Fatalf("unbestätigt: %d %v", res.StatusCode, o)
	}
	m := lastMail()
	link := regexp.MustCompile(`https://chat\.example\.org(/api/account/verify\?t=[0-9a-f]+)`).FindStringSubmatch(m)
	if link == nil || !strings.HasPrefix(m, "cleo@example.org") {
		t.Fatalf("Mail: %q", m)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	vr, _ := cl.Get(base + link[1])
	if vr.StatusCode != 303 || vr.Header.Get("Location") != "/?verify=ok" {
		t.Fatalf("Bestätigen: %d %s", vr.StatusCode, vr.Header.Get("Location"))
	}
	if res, _ := login(t, e, "cleo@example.org", k.Auth, "", ""); res.StatusCode != 200 {
		t.Fatalf("Anmelden per E-Mail-Adresse: %d", res.StatusCode)
	}

	// Passwort vergessen per E-Mail
	mu.Lock()
	mails = nil
	mu.Unlock()
	if res := post(t, base+"/api/account/forgot", "", `{"login":"unbekannt"}`); res.StatusCode != 204 {
		t.Fatalf("forgot unbekannt: %d", res.StatusCode)
	}
	if res := post(t, base+"/api/account/forgot", "", `{"login":"cleo"}`); res.StatusCode != 204 {
		t.Fatalf("forgot: %d", res.StatusCode)
	}
	m = lastMail()
	tk := regexp.MustCompile(`\?reset=([0-9a-f]+)`).FindStringSubmatch(m)
	if tk == nil {
		t.Fatalf("Reset-Mail: %q", m)
	}
	info := jmap(t, do(t, "GET", base+"/api/account/reset?t="+tk[1], "", ""))
	if info["name"] != "cleo" || info["twoFA"] != false {
		t.Fatalf("Reset-Info: %v", info)
	}
	k2 := testKeys()
	if res := post(t, base+"/api/account/reset", "", jsonBody(map[string]any{"ticket": tk[1], "keys": k2})); res.StatusCode != 200 {
		t.Fatalf("Reset: %d", res.StatusCode)
	}
	if res := post(t, base+"/api/account/reset", "", jsonBody(map[string]any{"ticket": tk[1], "keys": testKeys()})); res.StatusCode != 410 {
		t.Fatalf("Link zweimal: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "cleo", k2.Auth, "", ""); res.StatusCode != 200 {
		t.Fatalf("neues Passwort: %d", res.StatusCode)
	}
	// Links laufen ab
	post(t, base+"/api/account/forgot", "", `{"login":"cleo"}`)
	time.Sleep(50 * time.Millisecond)
	tk = regexp.MustCompile(`\?reset=([0-9a-f]+)`).FindStringSubmatch(lastMail())
	e.clock.add(2 * time.Hour)
	if res := do(t, "GET", base+"/api/account/reset?t="+tk[1], "", ""); res.StatusCode != 410 {
		t.Fatalf("abgelaufen: %d", res.StatusCode)
	}
}

func TestHostedAdmin(t *testing.T) {
	e := hostedServer(t)
	base := e.ts.URL
	adm := adminLogin(t, base)
	k := testKeys()
	out := register(t, e, "dora", "", k)
	tok := sessionToken(out)
	id := out["session"].(map[string]any)["id"].(string)

	cfg := jmap(t, do(t, "GET", base+"/api/admin/hosted", adm, ""))
	if cfg["mode"] != "hosted" {
		t.Fatalf("admin hosted: %v", cfg)
	}
	// Konto sperren: Sitzungen enden, Anmelden geht nicht mehr
	if res := post(t, base+"/api/admin/users/"+id+"/account", adm, `{"action":"disable"}`); res.StatusCode != 200 {
		t.Fatalf("sperren: %d", res.StatusCode)
	}
	if res := do(t, "GET", base+"/api/session", tok, ""); res.StatusCode != 401 {
		t.Fatalf("gesperrte Sitzung: %d", res.StatusCode)
	}
	if res, _ := login(t, e, "dora", k.Auth, "", ""); res.StatusCode != 403 {
		t.Fatalf("gesperrt anmelden: %d", res.StatusCode)
	}
	post(t, base+"/api/admin/users/"+id+"/account", adm, `{"action":"enable"}`)
	if res, _ := login(t, e, "dora", k.Auth, "", ""); res.StatusCode != 200 {
		t.Fatalf("entsperrt: %d", res.StatusCode)
	}
	lres := do(t, "GET", base+"/api/admin/users", adm, "")
	var list []map[string]any
	json.NewDecoder(lres.Body).Decode(&list)
	if len(list) != 1 || list[0]["account"] == nil {
		t.Fatalf("Kontodaten fehlen in der Liste: %v", list)
	}

	// Registrierung schließen, Einladungslink
	if res := do(t, "PUT", base+"/api/admin/hosted", adm, jsonBody(HostedConfig{Guests: GuestsRead, Registration: false})); res.StatusCode != 200 {
		t.Fatalf("PUT hosted: %d", res.StatusCode)
	}
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "emil", "keys": testKeys()})); res.StatusCode != 403 {
		t.Fatalf("geschlossen: %d", res.StatusCode)
	}
	inv := jmap(t, post(t, base+"/api/admin/invites", adm, `{"note":"für Emil"}`))
	tk := strings.TrimPrefix(inv["link"].(string), "https://chat.example.org/?invite=")
	if res := do(t, "GET", base+"/api/account/invite?t="+tk, "", ""); res.StatusCode != 204 {
		t.Fatalf("Einladung prüfen: %d", res.StatusCode)
	}
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "emil", "keys": testKeys(), "invite": tk})); res.StatusCode != 200 {
		t.Fatalf("mit Einladung: %d", res.StatusCode)
	}
	if res := do(t, "POST", base+"/api/account/register", "", jsonBody(map[string]any{"name": "fritz", "keys": testKeys(), "invite": tk})); res.StatusCode != 403 {
		t.Fatalf("Einladung zweimal: %d", res.StatusCode)
	}
	// Ungültige Einstellungen
	if res := do(t, "PUT", base+"/api/admin/hosted", adm, `{"guests":"alle"}`); res.StatusCode != 400 {
		t.Fatalf("ungültig: %d", res.StatusCode)
	}
	// SMTP-Passwort wird nicht ausgeliefert
	do(t, "PUT", base+"/api/admin/hosted", adm, jsonBody(HostedConfig{Guests: GuestsRead, SMTP: SMTPConfig{Host: "h", From: "a@b.de", Password: "geheim"}}))
	got := jmap(t, do(t, "GET", base+"/api/admin/hosted", adm, ""))
	if strings.Contains(jsonBody(got), "geheim") || strings.Contains(jsonBody(got), "secret\":\"") {
		t.Fatalf("Geheimnis sichtbar: %v", got)
	}
	do(t, "PUT", base+"/api/admin/hosted", adm, jsonBody(HostedConfig{Guests: GuestsRead, SMTP: SMTPConfig{Host: "h", From: "a@b.de", Password: keepSecret}}))
	if e.hub.Hosted().Config().SMTP.Password != "geheim" {
		t.Fatal("Passwort beim Speichern verloren")
	}
}

func TestMembersPersist(t *testing.T) {
	u := NewUsers(time.Hour, 100)
	k := testKeys()
	if _, _, _, err := u.Register(RegisterInput{Name: "ida", Keys: k}, false); err != nil {
		t.Fatal(err)
	}
	res, err := u.Login(LoginInput{Login: "ida", Auth: k.Auth}, "Test")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(u.export())
	var list []user
	json.Unmarshal(data, &list)
	u2 := NewUsers(time.Hour, 100)
	u2.restore(list)
	if me, ok := u2.Lookup(res.Token); !ok || !me.Member || me.Name != "ida" {
		t.Fatalf("Sitzung nach Neustart: %+v %v", me, ok)
	}
	if _, err := u2.Login(LoginInput{Login: "ida", Auth: k.Auth}, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), k.Auth) {
		t.Fatal("auth im Klartext gespeichert")
	}
}

func TestTOTPReference(t *testing.T) {
	// RFC 6238, Anhang B (SHA-1, Geheimnis "12345678901234567890"): 59 s → 94287082 (8 Stellen)
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	if got := totpCode(secret, 59/30); got != "287082" {
		t.Fatalf("TOTP: %s", got)
	}
	if got := totpCode(secret, 1111111109/30); got != "081804" {
		t.Fatalf("TOTP: %s", got)
	}
}

func TestHostedViewerBoundToSession(t *testing.T) {
	e := hostedServer(t)
	a := register(t, e, "anna", "", testKeys())
	b := register(t, e, "bert", "", testKeys())
	ta := sessionToken(a)
	viewer := a["session"].(map[string]any)["viewer"].(string)
	if strings.Count(viewer, ".") != 2 {
		t.Fatalf("Viewer ohne Sitzung: %s", viewer)
	}
	var dm DMInfo
	json.NewDecoder(post(t, e.ts.URL+"/api/dms", ta, jsonBody(map[string]string{"userId": b["session"].(map[string]any)["id"].(string)})).Body).Decode(&dm)
	stream := func(v string) int {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/api/rooms/"+dm.Room+"/events?viewer="+v, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if c := stream(viewer); c != 200 {
		t.Fatalf("Stream mit gültiger Sitzung: %d", c)
	}
	// ohne Sitzungsteil (alte Form) gilt der Viewer für Konten nicht
	parts := strings.Split(viewer, ".")
	if c := stream(parts[0] + "." + e.hub.Mod().PresenceSig(parts[0])); c != 404 {
		t.Fatalf("Viewer ohne Sitzung: %d", c)
	}
	do(t, "DELETE", e.ts.URL+"/api/session", ta, "")
	if c := stream(viewer); c != 404 {
		t.Fatalf("Stream nach Abmelden: %d", c)
	}
}

func TestHostedEmailChangeKeepsOldAddress(t *testing.T) {
	e := hostedServer(t)
	old := sendMail
	sendMail = func(SMTPConfig, string, string, string) error { return nil }
	t.Cleanup(func() { sendMail = old })
	e.hub.Hosted().Set(HostedConfig{Guests: GuestsRead, Registration: true, SMTP: SMTPConfig{Host: "h", Port: 587, From: "a@b.de", Security: "starttls"}})
	k := testKeys()
	tok := sessionToken(register(t, e, "ida", "ida@example.org", k))
	if res := post(t, e.ts.URL+"/api/account/email", tok, jsonBody(map[string]string{"auth": k.Auth, "email": "neu@exampel.org"})); res.StatusCode != 200 {
		t.Fatalf("E-Mail ändern: %d", res.StatusCode)
	}
	acc := jmap(t, do(t, "GET", e.ts.URL+"/api/account", tok, ""))["account"].(map[string]any)
	if acc["email"] != "ida@example.org" || acc["pendingEmail"] != "neu@exampel.org" {
		t.Fatalf("Adressen: %v", acc)
	}
	if res, _ := login(t, e, "ida", k.Auth, "", ""); res.StatusCode != 200 {
		t.Fatalf("Anmelden mit offener Änderung: %d", res.StatusCode)
	}
}

func TestHostedInviteOnce(t *testing.T) {
	h := NewHosted()
	tok := h.NewInvite("x", time.Now())
	var wg sync.WaitGroup
	got := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); got <- h.TakeInvite(tok) != nil }()
	}
	wg.Wait()
	close(got)
	n := 0
	for ok := range got {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("Einladung %d-mal verwendet", n)
	}
}

func TestFedDenyIgnoresPortAndDot(t *testing.T) {
	c := FedConfig{Deny: []string{"evil.com", "*.bad.org"}}
	for _, d := range []string{"evil.com", "evil.com:443", "evil.com.", "x.bad.org:443", "x.bad.org"} {
		if c.Allowed(d) {
			t.Errorf("%s sollte gesperrt sein", d)
		}
	}
	if !c.Allowed("good.org") {
		t.Error("good.org sollte erlaubt sein")
	}
	if _, _, err := parseFedAddr("a1@evil.com."); err == nil {
		t.Error("Punkt am Ende sollte ungültig sein")
	}
}
