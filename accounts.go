// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Zugänge zum Admin-Bereich:
//   - der Haupt-Admin „admin“ mit ADMIN_PASSWORD (immer vorhanden, nicht löschbar)
//   - weitere Konten, die der Admin anlegt, mit Rolle „admin“ oder „moderator“
//
// Moderatoren dürfen moderieren (Nachrichten, Automod, Nutzer sperren) und das Protokoll
// lesen; Einstellungen, Kanäle, Bots, Widgets, Konten und Backups bleiben Admins vorbehalten.
// Passwörter werden mit PBKDF2-HMAC-SHA256 (210.000 Runden, zufälliges Salz) gespeichert.

const (
	RoleAdmin       = "admin"
	RoleModerator   = "moderator"
	adminSessionTTL = 12 * time.Hour
	pbkdf2Rounds    = 210_000
	superAdminName  = "admin"
)

type Account struct {
	Name     string    `json:"name"`    // Anmeldename, klein geschrieben
	Display  string    `json:"display"` // Anzeigename im Protokoll
	Role     string    `json:"role"`
	Salt     string    `json:"salt"`
	Hash     string    `json:"hash"`
	Rounds   int       `json:"rounds"`
	Disabled bool      `json:"disabled,omitempty"`
	Created  time.Time `json:"created"`
}

// AccountInfo ist die Sicht im Admin-Bereich (ohne Passwort-Hash).
type AccountInfo struct {
	Name      string     `json:"name"`
	Display   string     `json:"display"`
	Role      string     `json:"role"`
	Disabled  bool       `json:"disabled"`
	Created   time.Time  `json:"created"`
	Super     bool       `json:"super"`
	LastLogin *time.Time `json:"lastLogin,omitempty"`
}

// Actor ist die angemeldete Person einer Admin-Anfrage.
type Actor struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Role    string `json:"role"`
}

func (a Actor) label() string {
	if a.Display != "" {
		return a.Display
	}
	return a.Name
}

type adminSession struct {
	actor Actor
	exp   time.Time
}

var (
	accountNameRe    = regexp.MustCompile(`^[a-z0-9._-]{2,32}$`)
	ErrAccountName   = errors.New("Anmeldename: 2–32 Zeichen, nur a–z, 0–9, Punkt, - und _")
	ErrAccountExists = errors.New("Diesen Anmeldenamen gibt es schon")
	ErrNoAccount     = errors.New("Dieses Konto gibt es nicht")
	ErrWeakPassword  = errors.New("Das Passwort muss mindestens 10 Zeichen lang sein")
	ErrBadRole       = errors.New("Rolle muss admin oder moderator sein")
	ErrSuperAccount  = errors.New("Der Haupt-Admin wird über ADMIN_PASSWORD verwaltet")
	ErrLastAdmin     = errors.New("Das würde den letzten aktiven Admin entfernen")
)

type Accounts struct {
	mu        sync.Mutex
	superHash [32]byte
	accounts  map[string]*Account
	sessions  map[string]*adminSession // Token-Hash → Sitzung
	lastLogin map[string]time.Time
	changed   func()
}

func NewAccounts(superPassword string) *Accounts {
	if superPassword == "" {
		superPassword = randomPassword() // nie mit leerem Passwort anmeldbar
	}
	return &Accounts{
		superHash: sha256.Sum256([]byte(superPassword)), accounts: map[string]*Account{},
		sessions: map[string]*adminSession{}, lastLogin: map[string]time.Time{},
	}
}

// SetSuperPassword setzt das Passwort des Haupt-Admins (ADMIN_PASSWORD). Leer wird ignoriert.
func (a *Accounts) SetSuperPassword(pw string) {
	if pw == "" {
		return
	}
	a.mu.Lock()
	a.superHash = sha256.Sum256([]byte(pw))
	a.mu.Unlock()
}

// randomPassword erzeugt ein lesbares Zufallspasswort (für den Fall, dass keins gesetzt ist).
func randomPassword() string {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pbkdf2 nach RFC 8018 mit HMAC-SHA256 (32 Byte Ergebnis).
func pbkdf2(password, salt []byte, rounds int) []byte {
	prf := hmac.New(sha256.New, password)
	prf.Write(salt)
	prf.Write(binary.BigEndian.AppendUint32(nil, 1))
	u := prf.Sum(nil)
	out := append([]byte(nil), u...)
	for i := 1; i < rounds; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(u[:0])
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

func hashPassword(pw string) (salt, hash string) {
	s := make([]byte, 16)
	if _, err := rand.Read(s); err != nil {
		panic(err)
	}
	return hex.EncodeToString(s), hex.EncodeToString(pbkdf2([]byte(pw), s, pbkdf2Rounds))
}

func checkPassword(a *Account, pw string) bool {
	salt, err1 := hex.DecodeString(a.Salt)
	want, err2 := hex.DecodeString(a.Hash)
	if err1 != nil || err2 != nil {
		return false
	}
	rounds := a.Rounds
	if rounds <= 0 {
		rounds = pbkdf2Rounds
	}
	return subtle.ConstantTimeCompare(pbkdf2([]byte(pw), salt, rounds), want) == 1
}

// Login prüft Name und Passwort. Ohne Namen (ältere Clients) ist der Haupt-Admin gemeint.
func (a *Accounts) Login(name, password string) (string, Actor, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	var actor Actor
	if name == "" || name == superAdminName {
		h := sha256.Sum256([]byte(password))
		if subtle.ConstantTimeCompare(h[:], a.superHash[:]) != 1 {
			return "", Actor{}, false
		}
		actor = Actor{Name: superAdminName, Display: "Admin", Role: RoleAdmin}
	} else {
		a.mu.Lock()
		acc, ok := a.accounts[name]
		var c Account
		if ok {
			c = *acc
		}
		a.mu.Unlock()
		if !ok || c.Disabled || !checkPassword(&c, password) {
			if !ok { // gleiche Rechenzeit wie bei vorhandenem Konto
				pbkdf2([]byte(password), []byte("x"), pbkdf2Rounds)
			}
			return "", Actor{}, false
		}
		actor = Actor{Name: c.Name, Display: c.Display, Role: c.Role}
	}
	tok := newToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, s := range a.sessions {
		if now.After(s.exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[hashToken(tok)] = &adminSession{actor: actor, exp: now.Add(adminSessionTTL)}
	a.lastLogin[actor.Name] = now.UTC()
	return tok, actor, true
}

// Session liefert die angemeldete Person zu einem Token.
func (a *Accounts) Session(token string) (Actor, bool) {
	if token == "" {
		return Actor{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[hashToken(token)]
	if !ok || time.Now().After(s.exp) {
		delete(a.sessions, hashToken(token))
		return Actor{}, false
	}
	return s.actor, true
}

func (a *Accounts) Logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, hashToken(token))
}

// endSessionsLocked meldet ein Konto überall ab (nach Löschen, Sperren, neuem Passwort, Rollenwechsel).
func (a *Accounts) endSessionsLocked(name string) {
	for k, s := range a.sessions {
		if s.actor.Name == name {
			delete(a.sessions, k)
		}
	}
}

type AccountInput struct {
	Name     string `json:"name"`
	Display  string `json:"display"`
	Role     string `json:"role"`
	Password string `json:"password"` // beim Ändern leer = unverändert
	Disabled bool   `json:"disabled"`
}

func (a *Accounts) Create(in AccountInput) (AccountInfo, error) {
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if !accountNameRe.MatchString(name) {
		return AccountInfo{}, ErrAccountName
	}
	if name == superAdminName {
		return AccountInfo{}, ErrAccountExists
	}
	if in.Role != RoleAdmin && in.Role != RoleModerator {
		return AccountInfo{}, ErrBadRole
	}
	if len([]rune(in.Password)) < 10 {
		return AccountInfo{}, ErrWeakPassword
	}
	display, _ := CleanName(in.Display)
	salt, hash := hashPassword(in.Password)
	a.mu.Lock()
	if _, ok := a.accounts[name]; ok {
		a.mu.Unlock()
		return AccountInfo{}, ErrAccountExists
	}
	acc := &Account{Name: name, Display: display, Role: in.Role, Salt: salt, Hash: hash, Rounds: pbkdf2Rounds, Created: time.Now().UTC()}
	a.accounts[name] = acc
	info := a.infoLocked(acc)
	a.mu.Unlock()
	a.notify()
	return info, nil
}

func (a *Accounts) Update(name string, in AccountInput) (AccountInfo, error) {
	if name == superAdminName {
		return AccountInfo{}, ErrSuperAccount
	}
	if in.Role != RoleAdmin && in.Role != RoleModerator {
		return AccountInfo{}, ErrBadRole
	}
	var salt, hash string
	if in.Password != "" {
		if len([]rune(in.Password)) < 10 {
			return AccountInfo{}, ErrWeakPassword
		}
		salt, hash = hashPassword(in.Password)
	}
	display, _ := CleanName(in.Display)
	a.mu.Lock()
	acc, ok := a.accounts[name]
	if !ok {
		a.mu.Unlock()
		return AccountInfo{}, ErrNoAccount
	}
	if acc.Role != in.Role || acc.Disabled != in.Disabled || hash != "" {
		a.endSessionsLocked(name)
	}
	acc.Display, acc.Role, acc.Disabled = display, in.Role, in.Disabled
	if hash != "" {
		acc.Salt, acc.Hash, acc.Rounds = salt, hash, pbkdf2Rounds
	}
	info := a.infoLocked(acc)
	a.mu.Unlock()
	a.notify()
	return info, nil
}

func (a *Accounts) Delete(name string) error {
	if name == superAdminName {
		return ErrSuperAccount
	}
	a.mu.Lock()
	if _, ok := a.accounts[name]; !ok {
		a.mu.Unlock()
		return ErrNoAccount
	}
	delete(a.accounts, name)
	a.endSessionsLocked(name)
	a.mu.Unlock()
	a.notify()
	return nil
}

func (a *Accounts) List() []AccountInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []AccountInfo{{Name: superAdminName, Display: "Admin", Role: RoleAdmin, Super: true}}
	if t, ok := a.lastLogin[superAdminName]; ok {
		out[0].LastLogin = &t
	}
	var rest []AccountInfo
	for _, acc := range a.accounts {
		rest = append(rest, a.infoLocked(acc))
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append(out, rest...)
}

func (a *Accounts) infoLocked(acc *Account) AccountInfo {
	info := AccountInfo{Name: acc.Name, Display: acc.Display, Role: acc.Role, Disabled: acc.Disabled, Created: acc.Created}
	if t, ok := a.lastLogin[acc.Name]; ok {
		info.LastLogin = &t
	}
	return info
}

func (a *Accounts) notify() {
	if a.changed != nil {
		a.changed()
	}
}

func (a *Accounts) export() []Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []Account{}
	for _, acc := range a.accounts {
		out = append(out, *acc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Accounts) restore(list []Account) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range list {
		acc := list[i]
		if accountNameRe.MatchString(acc.Name) && acc.Name != superAdminName {
			a.accounts[acc.Name] = &acc
		}
	}
}

// --- Middleware ---

type actorCtxKey struct{}

func actorFrom(r *http.Request) Actor {
	if a, ok := r.Context().Value(actorCtxKey{}).(Actor); ok {
		return a
	}
	return Actor{}
}

// requireRole lässt nur angemeldete Konten mit passender Rolle durch.
// RoleModerator: Moderatoren und Admins; RoleAdmin: nur Admins.
func (s *Server) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.accounts.Session(bearer(r))
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Bitte im Admin-Bereich anmelden")
			return
		}
		if role == RoleAdmin && actor.Role != RoleAdmin {
			writeErr(w, http.StatusForbidden, "Dafür sind Admin-Rechte nötig")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), actorCtxKey{}, actor)))
	}
}

// requireAdmin: Kurzform für Admin-Rechte.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(RoleAdmin, next)
}

// requireMod: Moderatoren und Admins.
func (s *Server) requireMod(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRole(RoleModerator, next)
}
