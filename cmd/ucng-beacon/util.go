// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"ucng/internal/i18n"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	v = i18n.TranslateError(w, v) // Fehlermeldungen in der Sprache der Anfrage
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return false
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// --- Rate-Limit ---

type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	h := l.hits[key]
	i := 0
	for i < len(h) && now.Sub(h[i]) > l.window {
		i++
	}
	h = h[i:]
	if len(h) >= l.limit {
		l.hits[key] = h
		return false
	}
	l.hits[key] = append(h, now)
	return true
}

func (l *limiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, h := range l.hits {
		if len(h) == 0 || time.Since(h[len(h)-1]) > l.window {
			delete(l.hits, k)
		}
	}
}

// --- Verwaltung: ein Passwort, Sitzungen im Speicher ---

type adminAuth struct {
	mu       sync.Mutex
	hash     [32]byte
	sessions map[string]time.Time
	tries    *limiter
}

func newAdminAuth(pw string) *adminAuth {
	return &adminAuth{hash: sha256.Sum256([]byte(pw)), sessions: map[string]time.Time{}, tries: newLimiter(10, time.Minute)}
}

func (a *adminAuth) login(pw, ip string) (string, bool) {
	if !a.tries.allow(ip) {
		return "", false
	}
	h := sha256.Sum256([]byte(pw))
	if subtle.ConstantTimeCompare(h[:], a.hash[:]) != 1 {
		return "", false
	}
	tok := randomHex(24)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(12 * time.Hour)
	a.mu.Unlock()
	return tok, true
}

func (a *adminAuth) valid(tok string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.sessions[tok]
	if !ok || time.Now().After(until) {
		delete(a.sessions, tok)
		return false
	}
	return true
}

func (a *adminAuth) logout(tok string) {
	a.mu.Lock()
	delete(a.sessions, tok)
	a.mu.Unlock()
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (a *adminAuth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(bearer(r)) {
			writeErr(w, http.StatusUnauthorized, "Bitte anmelden")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tok, ok := s.admin.login(in.Password, s.clientIP(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "Passwort falsch (oder zu viele Versuche)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.admin.logout(bearer(r))
	w.WriteHeader(http.StatusNoContent)
}

// --- Ausgehende Verbindungen ---

var errPrivate = errors.New("Dieser Beacon nimmt nur Instanzen mit öffentlicher Adresse an (PUBLIC_ONLY)")

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2002::/16"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// newClient: HTTP-Client; mit publicOnly erreicht er keine Adressen im lokalen Netz.
func newClient(publicOnly bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if publicOnly {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				return errPrivate
			}
			for _, n := range blockedNets {
				if n.Contains(ip) {
					return errPrivate
				}
			}
			return nil
		}
	}
	return &http.Client{Timeout: 20 * time.Second,
		Transport:     &http.Transport{DialContext: dialer.DialContext, ResponseHeaderTimeout: 15 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// cleanSourceURL: nur vollständige http(s)-Adressen (Quellcode einer Fassung), sonst leer.
func cleanSourceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if raw == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(raw) > 300 {
		return ""
	}
	return raw
}
