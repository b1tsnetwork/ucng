// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"time"
)

// Einstellungen des Modus „hosted“ (hosted.json): Gäste, Registrierung, E-Mail, Föderation.

const (
	GuestsNone  = "none"  // ohne Konto nichts sehen
	GuestsRead  = "read"  // ohne Konto mitlesen
	GuestsWrite = "write" // ohne Konto mit Gastnamen schreiben (wie im Modus „simple“)
)

type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from"`
	Security string `json:"security"` // starttls, tls, none
}

func (c SMTPConfig) Enabled() bool { return c.Host != "" && c.From != "" }

type FedConfig struct {
	Enabled bool     `json:"enabled"`
	Allow   []string `json:"allow"` // leer = alle (außer Deny)
	Deny    []string `json:"deny"`
}

type HostedConfig struct {
	Guests        string     `json:"guests"`
	Registration  bool       `json:"registration"`  // jede:r darf ein Konto anlegen
	RequireVerify bool       `json:"requireVerify"` // E-Mail-Bestätigung (braucht SMTP)
	SMTP          SMTPConfig `json:"smtp"`
	Fed           FedConfig  `json:"federation"`
	Secret        string     `json:"secret,omitempty"`  // für erfundene Prelogin-Werte (intern)
	Invites       []invite   `json:"invites,omitempty"` // offene Einladungen (intern)
}

type invite struct {
	Hash  string    `json:"hash"`
	Until time.Time `json:"until"`
	Note  string    `json:"note,omitempty"`
}

const inviteTTL = 7 * 24 * time.Hour

var defaultHosted = HostedConfig{Guests: GuestsRead, Registration: true, SMTP: SMTPConfig{Port: 587, Security: "starttls"}}

var (
	ErrGuests   = errors.New("Gäste: none, read oder write")
	ErrSMTPSec  = errors.New("SMTP-Verschlüsselung: starttls, tls oder none")
	ErrSMTPPort = errors.New("SMTP-Port: 1–65535")
	ErrDomain   = errors.New("Ungültige Domain in der Liste")
)

func cleanDomainList(list []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, d := range list {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		if len(d) > 253 || strings.ContainsAny(d, " /@") {
			return nil, ErrDomain
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

func (c HostedConfig) normalize() (HostedConfig, error) {
	switch c.Guests {
	case GuestsNone, GuestsRead, GuestsWrite:
	case "":
		c.Guests = GuestsRead
	default:
		return c, ErrGuests
	}
	c.SMTP.Host = strings.TrimSpace(c.SMTP.Host)
	c.SMTP.From = strings.TrimSpace(c.SMTP.From)
	if c.SMTP.Port == 0 {
		c.SMTP.Port = 587
	}
	if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
		return c, ErrSMTPPort
	}
	switch c.SMTP.Security {
	case "starttls", "tls", "none":
	case "":
		c.SMTP.Security = "starttls"
	default:
		return c, ErrSMTPSec
	}
	if c.SMTP.From != "" {
		if _, err := cleanEmail(addrOf(c.SMTP.From)); err != nil {
			return c, errors.New("Absender: bitte eine gültige E-Mail-Adresse")
		}
	}
	var err error
	if c.Fed.Allow, err = cleanDomainList(c.Fed.Allow); err != nil {
		return c, err
	}
	if c.Fed.Deny, err = cleanDomainList(c.Fed.Deny); err != nil {
		return c, err
	}
	return c, nil
}

// addrOf holt die Adresse aus "Name <adresse>".
func addrOf(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 && strings.HasSuffix(s, ">") {
		return s[i+1 : len(s)-1]
	}
	return s
}

type Hosted struct {
	mu      sync.Mutex
	cfg     HostedConfig
	changed func()
}

func NewHosted() *Hosted {
	h := &Hosted{cfg: defaultHosted}
	h.ensureSecret()
	return h
}

func (h *Hosted) ensureSecret() {
	if h.cfg.Secret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		h.cfg.Secret = string(b32.EncodeToString(b))
	}
}

func (h *Hosted) Config() HostedConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cfg
	c.Fed.Allow = append([]string{}, c.Fed.Allow...)
	c.Fed.Deny = append([]string{}, c.Fed.Deny...)
	return c
}

// Public: Sicht im Admin-Bereich (ohne Geheimnis, SMTP-Passwort nur als „gesetzt“).
func (h *Hosted) Public() HostedConfig {
	c := h.Config()
	c.Secret, c.Invites = "", nil
	if c.SMTP.Password != "" {
		c.SMTP.Password = keepSecret
	}
	return c
}

const keepSecret = "••••••••"

func (h *Hosted) Set(c HostedConfig) (HostedConfig, error) {
	c, err := c.normalize()
	if err != nil {
		return HostedConfig{}, err
	}
	h.mu.Lock()
	if c.SMTP.Password == keepSecret {
		c.SMTP.Password = h.cfg.SMTP.Password
	}
	c.Secret, c.Invites = h.cfg.Secret, h.cfg.Invites
	h.cfg = c
	h.mu.Unlock()
	if h.changed != nil {
		h.changed()
	}
	return h.Public(), nil
}

func (h *Hosted) restore(c HostedConfig) error {
	secret, invites := c.Secret, c.Invites
	c, err := c.normalize()
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c.Secret, c.Invites = secret, invites
	h.cfg = c
	h.ensureSecret()
	return nil
}

func (h *Hosted) secret() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return []byte(h.cfg.Secret)
}

// FedAllowed: darf mit dieser Domain föderiert werden?
func (c FedConfig) Allowed(domain string) bool {
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	host := fedHost(domain) // Port zählt nicht: evil.example:443 ist evil.example
	match := func(list []string) bool {
		for _, d := range list {
			d = strings.TrimSuffix(d, ".")
			for _, x := range []string{domain, host} {
				if d == x || (strings.HasPrefix(d, "*.") && strings.HasSuffix(x, d[1:])) {
					return true
				}
			}
		}
		return false
	}
	if match(c.Deny) {
		return false
	}
	return len(c.Allow) == 0 || match(c.Allow)
}

// NewInvite legt einen Einladungslink an (Registrierung auch bei geschlossener Anmeldung).
func (h *Hosted) NewInvite(note string, now time.Time) string {
	tok := newToken()
	if len(note) > 100 {
		note = note[:100]
	}
	h.mu.Lock()
	var keep []invite
	for _, iv := range h.cfg.Invites {
		if now.Before(iv.Until) {
			keep = append(keep, iv)
		}
	}
	h.cfg.Invites = append(keep, invite{Hash: hashToken(tok), Until: now.Add(inviteTTL), Note: note})
	h.mu.Unlock()
	if h.changed != nil {
		h.changed()
	}
	return tok
}

func (h *Hosted) ValidInvite(tok string) bool {
	if tok == "" {
		return false
	}
	hash := hashToken(tok)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, iv := range h.cfg.Invites {
		if iv.Hash == hash && time.Now().Before(iv.Until) {
			return true
		}
	}
	return false
}

// TakeInvite entnimmt eine gültige Einladung (nil = ungültig oder schon benutzt).
func (h *Hosted) TakeInvite(tok string) *invite {
	if tok == "" {
		return nil
	}
	hash := hashToken(tok)
	h.mu.Lock()
	var found *invite
	var keep []invite
	for _, iv := range h.cfg.Invites {
		if found == nil && iv.Hash == hash && time.Now().Before(iv.Until) {
			c := iv
			found = &c
			continue
		}
		keep = append(keep, iv)
	}
	h.cfg.Invites = keep
	h.mu.Unlock()
	if found != nil && h.changed != nil {
		h.changed()
	}
	return found
}

// PutInvite gibt eine entnommene Einladung zurück (Registrierung fehlgeschlagen).
func (h *Hosted) PutInvite(iv *invite) {
	if iv == nil {
		return
	}
	h.mu.Lock()
	h.cfg.Invites = append(h.cfg.Invites, *iv)
	h.mu.Unlock()
	if h.changed != nil {
		h.changed()
	}
}

func (h *Hosted) UseInvite(tok string) {
	hash := hashToken(tok)
	h.mu.Lock()
	var keep []invite
	for _, iv := range h.cfg.Invites {
		if iv.Hash != hash {
			keep = append(keep, iv)
		}
	}
	h.cfg.Invites = keep
	h.mu.Unlock()
	if h.changed != nil {
		h.changed()
	}
}
