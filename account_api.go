// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTP-API der Nutzerkonten (nur Modus „hosted“). Siehe members.go.

// PublicHosted steht in /api/settings, damit Web-Oberfläche und Widget wissen, was gilt.
type PublicHosted struct {
	Guests        string `json:"guests"`
	Registration  bool   `json:"registration"`
	Email         bool   `json:"email"`         // E-Mail-Versand eingerichtet
	RequireVerify bool   `json:"requireVerify"` // Registrierung braucht E-Mail-Bestätigung
	Federation    bool   `json:"federation"`
	Domain        string `json:"domain,omitempty"`
}

func (s *Server) publicHosted() *PublicHosted {
	if !s.hosted() {
		return nil
	}
	c := s.hub.Hosted().Config()
	p := &PublicHosted{Guests: c.Guests, Registration: c.Registration, Email: c.SMTP.Enabled(),
		RequireVerify: c.RequireVerify && c.SMTP.Enabled(), Federation: s.fedEnabled()}
	if p.Federation {
		p.Domain = s.fedDomain()
	}
	return p
}

func (s *Server) guestPolicy() string {
	if !s.hosted() {
		return GuestsWrite
	}
	return s.hub.Hosted().Config().Guests
}

// mayRead: Darf die Anfrage Kanäle und Nachrichten sehen? (Gäste-Einstellung im Modus „hosted“)
func (s *Server) mayRead(r *http.Request) bool {
	if s.guestPolicy() != GuestsNone {
		return true
	}
	if me, ok := s.users.Lookup(bearer(r)); ok && me.Member {
		return true
	}
	if v := s.viewerOf(r); v != nil {
		if info, ok := s.users.Get(v.ID); ok && info.Account != nil && !info.Account.Disabled {
			return true
		}
	}
	return false
}

var ErrLoginRequired = errors.New("Bitte melde dich an, um den Chat zu sehen")

// readGate schützt lesende Endpunkte, wenn Gäste nichts sehen dürfen.
func (s *Server) readGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.mayRead(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": ErrLoginRequired.Error(), "login": true})
			return
		}
		next(w, r)
	}
}

// hostedOnly: Endpunkte, die es nur im Modus „hosted“ gibt.
func (s *Server) hostedOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hosted() {
			writeErr(w, http.StatusNotFound, "Nutzerkonten gibt es nur im Modus „hosted“ (MODE=hosted)")
			return
		}
		next(w, r)
	}
}

func (s *Server) acctLimit(w http.ResponseWriter, r *http.Request) bool {
	if !s.acctRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Versuche – bitte eine Minute warten.")
		return false
	}
	return true
}

func accountStatus(err error) int {
	switch {
	case errors.Is(err, ErrBadLogin), errors.Is(err, ErrNeed2FA), errors.Is(err, ErrBad2FA), errors.Is(err, ErrNoMember):
		return http.StatusUnauthorized
	case errors.Is(err, ErrNotVerified), errors.Is(err, ErrDisabled):
		return http.StatusForbidden
	case errors.Is(err, ErrNameTaken), errors.Is(err, ErrEmailTaken), errors.Is(err, ErrNameProtected):
		return http.StatusConflict
	case errors.Is(err, ErrTicket):
		return http.StatusGone
	case errors.Is(err, ErrTooManyUsers):
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

func acctFail(w http.ResponseWriter, err error) {
	body := map[string]any{"error": err.Error()}
	if errors.Is(err, ErrNeed2FA) {
		body["need2fa"] = true
	}
	if errors.Is(err, ErrNotVerified) {
		body["unverified"] = true
	}
	writeJSON(w, accountStatus(err), body)
}

// POST /api/account/prelogin {"login"} → Parameter für die Schlüsselableitung
func (s *Server) handlePrelogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Login string `json:"login"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	salt, iter := s.users.Prelogin(in.Login, s.hub.Hosted().secret())
	writeJSON(w, http.StatusOK, map[string]any{"kdfSalt": salt, "kdfIter": iter})
}

// POST /api/account/register
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in struct {
		RegisterInput
		Invite string `json:"invite"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	cfg := s.hub.Hosted().Config()
	var inv *invite
	if !cfg.Registration {
		// Einladung sofort verbrauchen (gleichzeitige Registrierungen mit demselben Link scheitern);
		// klappt die Registrierung nicht, wird sie zurückgegeben.
		if inv = s.hub.Hosted().TakeInvite(in.Invite); inv == nil {
			writeErr(w, http.StatusForbidden, "Die Registrierung ist geschlossen – nur mit Einladungslink.")
			return
		}
	}
	ipHash := s.hub.Mod().IPHash(s.clientIP(r))
	if s.hub.Mod().IPBanned(ipHash) {
		s.hub.Hosted().PutInvite(inv)
		writeErr(w, http.StatusForbidden, "Von dieser Verbindung aus ist der Chat gesperrt.")
		return
	}
	verify := cfg.RequireVerify && cfg.SMTP.Enabled()
	id, ticket, codes, err := s.users.Register(in.RegisterInput, verify)
	if err != nil {
		s.hub.Hosted().PutInvite(inv)
		acctFail(w, err)
		return
	}
	s.hub.AuditLog().Add(AuditEntry{Action: "account.register", UserID: id, UserName: in.Name, Actor: "system", Display: "Registrierung"})
	out := map[string]any{"recovery": codes, "verify": verify}
	if verify {
		if err := s.mailVerify(s.langOf(r), in.Name, in.Email, ticket); err != nil {
			log.Printf("Bestätigungs-Mail an %s fehlgeschlagen: %v", in.Email, err)
			out["mailError"] = "Die E-Mail konnte nicht verschickt werden – bitte später „E-Mail erneut senden“ wählen."
		}
		writeJSON(w, http.StatusCreated, out)
		return
	}
	// ohne Bestätigung direkt anmelden
	res, err := s.users.Login(LoginInput{Login: in.Name, Auth: in.Keys.Auth}, r.UserAgent())
	if err != nil {
		acctFail(w, err)
		return
	}
	s.writeLogin(w, r, res, out)
}

func (s *Server) writeLogin(w http.ResponseWriter, r *http.Request, res LoginResult, extra map[string]any) {
	me, _ := s.users.Lookup(res.Token)
	ipHash := s.hub.Mod().IPHash(s.clientIP(r))
	s.hub.Mod().SeenIP(me.ID, ipHash)
	info := s.sessionOf(me, ipHash)
	info.Token = res.Token
	out := map[string]any{"session": info, "pubKey": res.PubKey, "privKey": res.PrivKey}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/account/login
func (s *Server) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in LoginInput
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := s.users.Login(in, r.UserAgent())
	if err != nil {
		acctFail(w, err)
		return
	}
	s.writeLogin(w, r, res, nil)
}

// GET /api/account/verify?t=… – Link aus der E-Mail
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if _, err := s.users.Verify(r.URL.Query().Get("t")); err != nil {
		http.Redirect(w, r, "/?verify=failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?verify=ok", http.StatusSeeOther)
}

// POST /api/account/resend {"login"} – Bestätigungs-Mail erneut senden (Antwort immer gleich)
func (s *Server) handleResend(w http.ResponseWriter, r *http.Request) {
	if !s.mailRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele E-Mails angefordert – bitte später erneut versuchen.")
		return
	}
	var in struct {
		Login string `json:"login"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if name, email, ticket, ok := s.users.NewVerifyTicket(in.Login); ok {
		lang := s.langOf(r)
		go func() {
			if err := s.mailVerify(lang, name, email, ticket); err != nil {
				log.Printf("Bestätigungs-Mail fehlgeschlagen: %v", err)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/account/forgot {"login"} – Link zum Zurücksetzen (Antwort immer gleich)
func (s *Server) handleForgot(w http.ResponseWriter, r *http.Request) {
	if !s.mailRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele E-Mails angefordert – bitte später erneut versuchen.")
		return
	}
	if !s.hub.Hosted().Config().SMTP.Enabled() {
		writeErr(w, http.StatusConflict, "Hier ist kein E-Mail-Versand eingerichtet. Bitte einen Wiederherstellungscode verwenden.")
		return
	}
	var in struct {
		Login string `json:"login"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if name, email, ticket, ok := s.users.NewResetTicket(in.Login); ok {
		lang := s.langOf(r)
		go func() {
			if err := s.mailReset(lang, name, email, ticket); err != nil {
				log.Printf("Mail zum Zurücksetzen fehlgeschlagen: %v", err)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/account/reset?t=… – gilt der Link noch, braucht es 2FA?
func (s *Server) handleResetInfo(w http.ResponseWriter, r *http.Request) {
	name, twoFA, err := s.users.ResetInfo(r.URL.Query().Get("t"))
	if err != nil {
		acctFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "twoFA": twoFA})
}

// POST /api/account/reset
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in ResetInput
	if !decodeJSON(w, r, &in) {
		return
	}
	name, err := s.users.ResetPassword(in)
	if err != nil {
		acctFail(w, err)
		return
	}
	s.hub.AuditLog().Add(AuditEntry{Action: "account.reset", UserName: name, Actor: "system", Display: "Konto"})
	writeJSON(w, http.StatusOK, map[string]any{"name": name})
}

// GET /api/account – eigene Kontodaten (inkl. verschlüsseltem privatem Schlüssel)
func (s *Server) handleAccountGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.users.Account(bearer(r))
	if err != nil {
		acctFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type authBody struct {
	Auth     string      `json:"auth"`
	Code     string      `json:"code"`
	Email    string      `json:"email"`
	Keys     KeyMaterial `json:"keys"`
	PubKey   string      `json:"pubKey"`
	PrivKey  string      `json:"privKey"`
	Allow    *bool       `json:"allow"`
	Recovery string      `json:"recovery"`
}

// POST /api/account/password {"auth": alt, "keys": neu}
func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.users.ChangePassword(bearer(r), in.Auth, in.Code, in.Keys); err != nil {
		acctFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/account/email {"auth","email"}
func (s *Server) handleAccountEmail(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	cfg := s.hub.Hosted().Config()
	verify := cfg.SMTP.Enabled() && strings.TrimSpace(in.Email) != ""
	ticket, err := s.users.SetEmail(bearer(r), in.Auth, in.Code, in.Email, verify)
	if err != nil {
		acctFail(w, err)
		return
	}
	if ticket != "" {
		me, _ := s.users.Lookup(bearer(r))
		if err := s.mailVerify(s.langOf(r), me.Name, strings.TrimSpace(in.Email), ticket); err != nil {
			log.Printf("Bestätigungs-Mail fehlgeschlagen: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"verify": ticket != ""})
}

// POST /api/account/2fa/begin {"auth"} → Geheimnis, otpauth-Adresse und QR-Code (SVG)
func (s *Server) handle2FABegin(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	name, secret, err := s.users.Begin2FA(bearer(r), in.Auth, in.Code)
	if err != nil {
		acctFail(w, err)
		return
	}
	issuer := s.hub.Settings().Title
	label := issuer + ":" + name
	if s.cfg.PublicURL != "" {
		if u, err := url.Parse(s.cfg.PublicURL); err == nil {
			label = issuer + ":" + name + "@" + u.Host
		}
	}
	uri := "otpauth://totp/" + url.PathEscape(label) + "?secret=" + secret + "&issuer=" + url.QueryEscape(issuer) + "&algorithm=SHA1&digits=6&period=30"
	out := map[string]any{"secret": secret, "uri": uri}
	if q, err := EncodeQR(uri, -1); err == nil {
		out["svg"] = q.SVG()
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/account/2fa/enable {"code"} → neue Wiederherstellungscodes
func (s *Server) handle2FAEnable(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	codes, err := s.users.Enable2FA(bearer(r), in.Code)
	if err != nil {
		acctFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recovery": codes})
}

// POST /api/account/2fa/disable {"auth","code"}
func (s *Server) handle2FADisable(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.users.Disable2FA(bearer(r), in.Auth, in.Code); err != nil {
		acctFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/account/recovery {"auth"} → neue Wiederherstellungscodes
func (s *Server) handleRecoveryNew(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	codes, err := s.users.NewRecovery(bearer(r), in.Auth, in.Code)
	if err != nil {
		acctFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recovery": codes})
}

// DELETE /api/account/sessions/{id} – eine Sitzung beenden ("others" = alle anderen)
func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	if err := s.users.EndSession(bearer(r), r.PathValue("id")); err != nil {
		acctFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/account/keys {"auth","pubKey","privKey"} – neues Schlüsselpaar
func (s *Server) handleAccountKeys(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.users.SetKeys(bearer(r), in.Auth, in.Code, in.PubKey, in.PrivKey); err != nil {
		acctFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/account/federation {"allow": bool}
func (s *Server) handleAccountFed(w http.ResponseWriter, r *http.Request) {
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Allow == nil {
		writeErr(w, http.StatusBadRequest, "allow fehlt")
		return
	}
	if err := s.users.SetNoFed(bearer(r), !*in.Allow); err != nil {
		acctFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/account/delete {"auth"} – eigenes Konto löschen
func (s *Server) handleMemberDelete(w http.ResponseWriter, r *http.Request) {
	if !s.acctLimit(w, r) {
		return
	}
	var in authBody
	if !decodeJSON(w, r, &in) {
		return
	}
	id, err := s.users.DeleteOwn(bearer(r), in.Auth, in.Code)
	if err != nil {
		acctFail(w, err)
		return
	}
	s.hub.AuditLog().Add(AuditEntry{Action: "account.delete", UserID: id, Actor: "system", Display: "Konto"})
	w.WriteHeader(http.StatusNoContent)
}

// --- Admin ---

// GET/PUT /api/admin/hosted – Gäste, Registrierung, SMTP, Föderation
func (s *Server) handleAdminHostedGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"mode": s.cfg.Mode, "config": s.hub.Hosted().Public(),
		"publicUrl": s.cfg.PublicURL, "domain": s.fedDomain(), "storage": s.storageKind})
}

func (s *Server) handleAdminHostedPut(w http.ResponseWriter, r *http.Request) {
	var in HostedConfig
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.hub.Hosted().Set(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "hosted.update", Detail: "Gäste: " + out.Guests})
	s.hub.Notify("settings")
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/hosted/testmail {"to"}
func (s *Server) handleAdminTestMail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		To string `json:"to"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	to, err := cleanEmail(in.To)
	if err != nil || to == "" {
		writeErr(w, http.StatusBadRequest, ErrEmail.Error())
		return
	}
	vars := map[string]any{"title": s.hub.Settings().Title, "time": time.Now().Format(time.RFC1123)}
	lang := s.langOf(r)
	if err := sendMail(s.hub.Hosted().Config().SMTP, to, s.hub.I18n().T(lang, "mail.test_subject", vars), s.hub.I18n().T(lang, "mail.test_body", vars)); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Versand fehlgeschlagen: %s", err.Error()))
		return
	}
	s.log(r, AuditEntry{Action: "hosted.testmail", Detail: to})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/admin/users/{id}/account {"action": disable|enable|reset2fa|verify|logout|resetmail}
func (s *Server) handleAdminMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if in.Action == "resetmail" {
		info, ok := s.users.Get(id)
		if !ok || info.Account == nil {
			fail(w, ErrNoUser)
			return
		}
		name, email, ticket, ok := s.users.NewResetTicket(info.Name)
		if !ok {
			writeErr(w, http.StatusConflict, "Für dieses Konto gibt es keine bestätigte E-Mail-Adresse.")
			return
		}
		if err := s.mailReset(s.langOf(r), name, email, ticket); err != nil {
			writeErr(w, http.StatusBadGateway, "Versand fehlgeschlagen: "+err.Error())
			return
		}
		s.log(r, AuditEntry{Action: "account.resetmail", UserID: id, UserName: name})
		writeJSON(w, http.StatusOK, info)
		return
	}
	info, err := s.users.AdminMember(id, in.Action)
	if err != nil {
		if errors.Is(err, ErrNoMember) {
			writeErr(w, http.StatusConflict, "Diese Person hat kein Konto (Gastname).")
			return
		}
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "account." + in.Action, UserID: id, UserName: info.Name})
	writeJSON(w, http.StatusOK, info)
}

// POST /api/admin/invites {"note"} → Einladungslink (7 Tage, einmalig)
func (s *Server) handleAdminInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tok := s.hub.Hosted().NewInvite(in.Note, time.Now())
	s.log(r, AuditEntry{Action: "account.invite", Detail: in.Note})
	writeJSON(w, http.StatusCreated, map[string]any{"link": s.baseURL() + "/?invite=" + tok, "valid": inviteTTL.String()})
}

// GET /api/account/invite?t= – gilt die Einladung?
func (s *Server) handleInviteCheck(w http.ResponseWriter, r *http.Request) {
	if !s.hub.Hosted().ValidInvite(r.URL.Query().Get("t")) {
		acctFail(w, ErrTicket)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var ErrGuestWrite = errors.New("Zum Schreiben bitte anmelden oder registrieren")
