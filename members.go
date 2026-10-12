// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// Nutzerkonten (nur im Modus „hosted“).
//
// Ein Konto ist eine Person aus users.go mit zusätzlichen Kontodaten (member). Konten laufen nie
// ab, haben beliebig viele Sitzungen (je Gerät ein Token) und einen festen Namen.
//
// Passwörter verlassen den Browser nie: Der Browser leitet aus Passwort und Salz per PBKDF2
// (WebCrypto) 512 Bit ab. Die erste Hälfte („auth“) geht an den Server und wird dort noch einmal
// gesalzen gehasht; die zweite Hälfte bleibt im Browser und verschlüsselt den privaten Schlüssel
// für die Ende-zu-Ende-verschlüsselten Direktnachrichten. Der Server kann diese daher nicht lesen.

const (
	memberSessionTTL = 30 * 24 * time.Hour // ungenutzte Sitzungen enden danach
	maxMemberSess    = 20
	serverAuthRounds = 20_000 // auth ist bereits ein starker Schlüssel; schützt zusätzlich bei Datenleck
	minKDFIter       = 100_000
	maxKDFIter       = 2_000_000
	ticketTTL        = 24 * time.Hour // E-Mail-Bestätigung
	resetTTL         = time.Hour      // Passwort zurücksetzen
	recoveryCount    = 10
)

var (
	ErrBadLogin      = errors.New("Name oder Passwort falsch")
	ErrNeed2FA       = errors.New("Bitte den Code aus der Authenticator-App eingeben")
	ErrBad2FA        = errors.New("Der Code stimmt nicht")
	ErrNotVerified   = errors.New("Bitte bestätige zuerst deine E-Mail-Adresse (Link in der E-Mail)")
	ErrDisabled      = errors.New("Dieses Konto ist gesperrt")
	ErrHandle        = errors.New("Name: 2–32 Zeichen, nur Buchstaben a–z, Ziffern, Punkt, _ oder -")
	ErrEmail         = errors.New("Bitte eine gültige E-Mail-Adresse angeben")
	ErrEmailTaken    = errors.New("Diese E-Mail-Adresse wird bereits verwendet")
	ErrKDF           = errors.New("Ungültige Schlüsseldaten")
	ErrTicket        = errors.New("Der Link ist ungültig oder abgelaufen")
	ErrAccountRename = errors.New("Der Name eines Kontos kann nicht geändert werden")
	ErrNoMember      = errors.New("Das geht nur mit einem Konto")
	ErrNo2FASetup    = errors.New("Bitte die Einrichtung zuerst starten")
)

var handleRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,30}[A-Za-z0-9_]$`)

type memberSession struct {
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed"`
	Agent    string    `json:"agent,omitempty"`
}

type member struct {
	Email   string `json:"email,omitempty"`
	EmailOK bool   `json:"emailOk,omitempty"`
	// Neue Adresse, bis sie bestätigt ist (die alte bleibt so lange gültig)
	PendingEmail string `json:"pendingEmail,omitempty"`
	// Registrierung mit Bestätigungspflicht: Anmelden erst nach Bestätigung
	MustVerify bool   `json:"mustVerify,omitempty"`
	AuthSalt   string `json:"authSalt"`
	AuthHash   string `json:"authHash"`
	KDFSalt    string `json:"kdfSalt"` // für den Browser (öffentlich)
	KDFIter    int    `json:"kdfIter"`

	TOTP        string   `json:"totp,omitempty"`        // aktives Geheimnis (Base32)
	TOTPPending string   `json:"totpPending,omitempty"` // während der Einrichtung
	TOTPLast    int64    `json:"totpLast,omitempty"`    // zuletzt verwendeter Zeitschritt (gegen Wiederverwendung)
	Recovery    []string `json:"recovery,omitempty"`    // SHA-256 der Wiederherstellungscodes

	Disabled bool `json:"disabled,omitempty"`
	NoFed    bool `json:"noFed,omitempty"` // keine Direktnachrichten von anderen Instanzen

	PubKey  string `json:"pubKey,omitempty"`  // öffentlicher ECDH-Schlüssel (P-256, roh, Base64)
	PrivKey string `json:"privKey,omitempty"` // privater Schlüssel, im Browser verschlüsselt (Base64)

	Sessions []memberSession `json:"sessions,omitempty"`

	VerifyHash  string    `json:"verifyHash,omitempty"`
	VerifyUntil time.Time `json:"verifyUntil,omitempty"`
	ResetHash   string    `json:"resetHash,omitempty"`
	ResetUntil  time.Time `json:"resetUntil,omitempty"`
}

// KeyMaterial kommt bei Registrierung, Passwortwechsel und Zurücksetzen aus dem Browser.
type KeyMaterial struct {
	Auth    string `json:"auth"`    // Base64, 32 Bytes
	KDFSalt string `json:"kdfSalt"` // Base64, 16–64 Bytes
	KDFIter int    `json:"kdfIter"`
	PubKey  string `json:"pubKey"`  // Base64, 65 Bytes (unkomprimierter P-256-Punkt)
	PrivKey string `json:"privKey"` // Base64, verschlüsselt
}

func (k KeyMaterial) validate(needKeys bool) error {
	if b, err := base64.StdEncoding.DecodeString(k.Auth); err != nil || len(b) != 32 {
		return ErrKDF
	}
	if b, err := base64.StdEncoding.DecodeString(k.KDFSalt); err != nil || len(b) < 16 || len(b) > 64 {
		return ErrKDF
	}
	if k.KDFIter < minKDFIter || k.KDFIter > maxKDFIter {
		return ErrKDF
	}
	if needKeys || k.PubKey != "" || k.PrivKey != "" {
		if b, err := base64.StdEncoding.DecodeString(k.PubKey); err != nil || len(b) != 65 || b[0] != 4 {
			return ErrKDF
		}
		if b, err := base64.StdEncoding.DecodeString(k.PrivKey); err != nil || len(b) < 40 || len(b) > 1024 {
			return ErrKDF
		}
	}
	return nil
}

func (m *member) setAuth(k KeyMaterial) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	auth, _ := base64.StdEncoding.DecodeString(k.Auth)
	m.AuthSalt = hex.EncodeToString(salt)
	m.AuthHash = hex.EncodeToString(pbkdf2(auth, salt, serverAuthRounds))
	m.KDFSalt, m.KDFIter = k.KDFSalt, k.KDFIter
	if k.PubKey != "" {
		m.PubKey, m.PrivKey = k.PubKey, k.PrivKey
	}
}

func (m *member) checkAuth(auth string) bool { return checkAuthHash(m.AuthSalt, m.AuthHash, auth) }

func checkAuthHash(saltHex, hashHex, auth string) bool {
	raw, err := base64.StdEncoding.DecodeString(auth)
	if err != nil || len(raw) != 32 {
		return false
	}
	salt, err1 := hex.DecodeString(saltHex)
	want, err2 := hex.DecodeString(hashHex)
	if err1 != nil || err2 != nil {
		return false
	}
	return subtle.ConstantTimeCompare(pbkdf2(raw, salt, serverAuthRounds), want) == 1
}

// --- TOTP (RFC 6238: SHA-1, 30 s, 6 Ziffern) ---

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

func totpCode(secret string, step int64) string {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	return hotp(key, uint64(step))
}

// hotp nach RFC 4226.
func hotp(key []byte, counter uint64) string {
	mac := hmac.New(sha1.New, key)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], counter)
	mac.Write(b[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return fmt.Sprintf("%06d", v%1_000_000)
}

// checkTOTP prüft einen Code (±1 Zeitschritt) und liefert den Zeitschritt.
func checkTOTP(secret, code string, now time.Time, last int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / 30
	for _, st := range []int64{cur - 1, cur, cur + 1} {
		if st <= last {
			continue // schon benutzt
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, st)), []byte(code)) == 1 {
			return st, true
		}
	}
	return 0, false
}

// --- Wiederherstellungscodes ---

func newRecoveryCodes() (codes, hashes []string) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	for i := 0; i < recoveryCount; i++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var sb strings.Builder
		for j, c := range b {
			if j == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(alphabet[int(c)%len(alphabet)])
		}
		codes = append(codes, sb.String())
		hashes = append(hashes, hashRecovery(sb.String()))
	}
	return
}

func hashRecovery(code string) string {
	code = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(code), " ", ""), "-", ""))
	return hashToken("recovery:" + code)
}

// useRecovery verbraucht einen Wiederherstellungscode.
func (m *member) useRecovery(code string) bool {
	if strings.TrimSpace(code) == "" {
		return false
	}
	h := hashRecovery(code)
	for i, x := range m.Recovery {
		if subtle.ConstantTimeCompare([]byte(x), []byte(h)) == 1 {
			m.Recovery = append(m.Recovery[:i:i], m.Recovery[i+1:]...)
			return true
		}
	}
	return false
}

// second prüft den zweiten Faktor: TOTP-Code oder Wiederherstellungscode.
func (m *member) second(code, recovery string, now time.Time) error {
	if recovery != "" {
		if m.useRecovery(recovery) {
			return nil
		}
		return ErrBad2FA
	}
	if m.TOTP == "" {
		return nil
	}
	if code == "" {
		return ErrNeed2FA
	}
	st, ok := checkTOTP(m.TOTP, code, now, m.TOTPLast)
	if !ok {
		return ErrBad2FA
	}
	m.TOTPLast = st
	return nil
}

// --- Konten in Users ---

// MemberInfo ist die Kontosicht (für die Person selbst und den Admin-Bereich).
type MemberInfo struct {
	Email     string `json:"email,omitempty"`
	Pending   string `json:"pendingEmail,omitempty"`
	EmailOK   bool   `json:"emailOk"`
	TwoFA     bool   `json:"twoFA"`
	Recovery  int    `json:"recoveryLeft"`
	Disabled  bool   `json:"disabled,omitempty"`
	Sessions  int    `json:"sessions"`
	HasKeys   bool   `json:"e2e"`
	NoFed     bool   `json:"noFed,omitempty"`
	CreatedBy string `json:"-"`
}

func (m *member) info() *MemberInfo {
	return &MemberInfo{Email: m.Email, Pending: m.PendingEmail, EmailOK: m.EmailOK, TwoFA: m.TOTP != "", Recovery: len(m.Recovery),
		Disabled: m.Disabled, Sessions: len(m.Sessions), HasKeys: m.PubKey != "", NoFed: m.NoFed}
}

func cleanEmail(e string) (string, error) {
	e = strings.TrimSpace(e)
	if e == "" {
		return "", nil
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 200 || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
		return "", ErrEmail
	}
	return e, nil
}

// RegisterInput sind die Daten einer Registrierung.
type RegisterInput struct {
	Name  string      `json:"name"`
	Email string      `json:"email"`
	Keys  KeyMaterial `json:"keys"`
}

// Register legt ein Konto an. verify = E-Mail muss bestätigt werden; dann wird ein
// Bestätigungs-Token geliefert. codes sind die Wiederherstellungscodes (nur einmal sichtbar).
func (u *Users) Register(in RegisterInput, verify bool) (id, ticket string, codes []string, err error) {
	name := strings.TrimSpace(in.Name)
	if !handleRe.MatchString(name) {
		return "", "", nil, ErrHandle
	}
	email, err := cleanEmail(in.Email)
	if err != nil {
		return "", "", nil, err
	}
	if verify && email == "" {
		return "", "", nil, ErrEmail
	}
	if err := in.Keys.validate(true); err != nil {
		return "", "", nil, err
	}
	if f := u.reserved.Load(); f != nil {
		if err := (*f)(name, false); err != nil {
			return "", "", nil, err
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if other := u.byKey[nameKey(name)]; other != nil {
		if !u.expired(other) {
			return "", "", nil, ErrNameTaken
		}
		u.removeLocked(other)
	}
	if email != "" && u.emailUsedLocked(email, "") {
		return "", "", nil, ErrEmailTaken
	}
	if len(u.byKey) >= u.max {
		u.expireLocked()
		if len(u.byKey) >= u.max {
			return "", "", nil, ErrTooManyUsers
		}
	}
	now := u.now()
	m := &member{Email: email, EmailOK: email != "" && !verify}
	m.setAuth(in.Keys)
	codes, m.Recovery = newRecoveryCodes()
	if verify {
		ticket = newToken()
		m.VerifyHash, m.VerifyUntil, m.MustVerify = hashToken(ticket), now.Add(ticketTTL), true
	}
	x := &user{ID: newUserID(), Name: name, TokenHash: "acct:" + newToken(), Created: now, LastSeen: now, Member: m}
	u.byKey[nameKey(name)] = x
	u.byID[x.ID] = x
	u.markDirty()
	return x.ID, ticket, codes, nil
}

func (u *Users) emailUsedLocked(email, exceptID string) bool {
	for _, x := range u.byID {
		if x.Member != nil && x.ID != exceptID && strings.EqualFold(x.Member.Email, email) {
			return true
		}
	}
	return false
}

// memberByLoginLocked findet ein Konto über Name oder E-Mail-Adresse.
func (u *Users) memberByLoginLocked(login string) *user {
	login = strings.TrimSpace(login)
	if x := u.byKey[nameKey(login)]; x != nil && x.Member != nil {
		return x
	}
	if strings.Contains(login, "@") {
		for _, x := range u.byID {
			if x.Member != nil && x.Member.Email != "" && strings.EqualFold(x.Member.Email, login) {
				return x
			}
		}
	}
	return nil
}

// Prelogin liefert die Parameter für die Schlüsselableitung im Browser. Für unbekannte Namen
// gibt es stabile, erfundene Werte – so verrät die Antwort nicht, ob es ein Konto gibt.
func (u *Users) Prelogin(login string, secret []byte) (salt string, iter int) {
	u.mu.Lock()
	x := u.memberByLoginLocked(login)
	if x != nil {
		salt, iter = x.Member.KDFSalt, x.Member.KDFIter
	}
	u.mu.Unlock()
	if x != nil {
		return
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("prelogin:" + strings.ToLower(strings.TrimSpace(login))))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)[:16]), defaultKDFIter
}

const defaultKDFIter = 300_000

// LoginInput sind die Anmeldedaten.
type LoginInput struct {
	Login    string `json:"login"` // Name oder E-Mail-Adresse
	Auth     string `json:"auth"`
	Code     string `json:"code"`     // TOTP
	Recovery string `json:"recovery"` // Wiederherstellungscode statt TOTP
}

// LoginResult enthält das neue Sitzungs-Token und die verschlüsselten Schlüssel.
type LoginResult struct {
	Token   string
	ID      string
	Name    string
	PubKey  string
	PrivKey string
}

// Login prüft Passwort (auth) und zweiten Faktor und legt eine Sitzung an.
func (u *Users) Login(in LoginInput, agent string) (LoginResult, error) {
	// Das Hashen dauert einige Millisekunden – nicht unter der Sperre, sonst bremst eine
	// Anmeldewelle den ganzen Chat.
	u.mu.Lock()
	x := u.memberByLoginLocked(in.Login)
	var salt, hash string
	if x != nil {
		salt, hash = x.Member.AuthSalt, x.Member.AuthHash
	}
	u.mu.Unlock()
	if x == nil {
		pbkdf2([]byte(in.Auth), []byte("gochat-dummy-salt"), serverAuthRounds) // gleiche Antwortzeit wie bei echten Konten
		return LoginResult{}, ErrBadLogin
	}
	if !checkAuthHash(salt, hash, in.Auth) {
		return LoginResult{}, ErrBadLogin
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.byID[x.ID] != x || x.Member == nil || x.Member.AuthHash != hash { // inzwischen geändert oder gelöscht
		return LoginResult{}, ErrBadLogin
	}
	m := x.Member
	if m.Disabled {
		return LoginResult{}, ErrDisabled
	}
	if m.MustVerify && !m.EmailOK {
		return LoginResult{}, ErrNotVerified
	}
	before := len(m.Recovery)
	if err := m.second(in.Code, in.Recovery, u.now()); err != nil {
		return LoginResult{}, err
	}
	if len(m.Recovery) != before {
		u.markDirty()
	}
	tok := u.newSessionLocked(x, agent)
	return LoginResult{Token: tok, ID: x.ID, Name: x.Name, PubKey: m.PubKey, PrivKey: m.PrivKey}, nil
}

func (u *Users) newSessionLocked(x *user, agent string) string {
	m := x.Member
	now := u.now()
	tok := newToken()
	if len(agent) > 120 {
		agent = agent[:120]
	}
	m.Sessions = append(m.Sessions, memberSession{Hash: hashToken(tok), Created: now, LastUsed: now, Agent: agent})
	for len(m.Sessions) > maxMemberSess { // älteste zuerst beenden
		delete(u.byToken, m.Sessions[0].Hash)
		m.Sessions = m.Sessions[1:]
	}
	u.byToken[hashToken(tok)] = x
	x.LastSeen = now
	u.markDirty()
	return tok
}

// endSessionsLocked beendet alle Sitzungen eines Kontos außer keep (Token-Hash).
func (u *Users) endSessionsLocked(x *user, keep string) {
	var rest []memberSession
	for _, s := range x.Member.Sessions {
		if s.Hash == keep {
			rest = append(rest, s)
			continue
		}
		delete(u.byToken, s.Hash)
	}
	x.Member.Sessions = rest
	u.sessionsChangedLocked(x, false)
}

// sessionID: Kennung einer Sitzung (für Live-Streams), aus dem Token-Hash.
func sessionID(hash string) string {
	if len(hash) < 16 {
		return hash
	}
	return hash[:16]
}

// OnSessionsEnded: wird aufgerufen, wenn Sitzungen eines Kontos enden (Streams schließen).
func (u *Users) OnSessionsEnded(f func(userID string, alive []string)) { u.sessEnded.Store(&f) }

// sessionsChangedLocked meldet die noch gültigen Sitzungen eines Kontos (gone = Konto weg/gesperrt).
func (u *Users) sessionsChangedLocked(x *user, gone bool) {
	f := u.sessEnded.Load()
	if f == nil || x.Member == nil {
		return
	}
	var alive []string
	if !gone && !x.Member.Disabled {
		for _, s := range x.Member.Sessions {
			alive = append(alive, sessionID(s.Hash))
		}
	}
	go (*f)(x.ID, alive) // außerhalb von u.mu – der Hub hat eine eigene Sperre
}

// MemberSession: gilt diese Sitzung eines Kontos noch?
func (u *Users) MemberSession(id, sid string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil || x.Member == nil || x.Member.Disabled || sid == "" {
		return false
	}
	for _, s := range x.Member.Sessions {
		if sessionID(s.Hash) == sid {
			return true
		}
	}
	return false
}

// memberOfToken liefert das Konto zu einem Sitzungs-Token.
func (u *Users) memberOfTokenLocked(token string) *user {
	x := u.byToken[hashToken(token)]
	if x == nil || x.Member == nil {
		return nil
	}
	return x
}

// Verify bestätigt die E-Mail-Adresse.
func (u *Users) Verify(ticket string) (string, error) {
	h := hashToken(ticket)
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range u.byID {
		m := x.Member
		if m == nil || m.VerifyHash == "" || subtle.ConstantTimeCompare([]byte(m.VerifyHash), []byte(h)) != 1 {
			continue
		}
		if u.now().After(m.VerifyUntil) {
			return "", ErrTicket
		}
		if m.PendingEmail != "" {
			m.Email, m.PendingEmail = m.PendingEmail, ""
		}
		m.EmailOK, m.VerifyHash, m.MustVerify = true, "", false
		u.markDirty()
		return x.Name, nil
	}
	return "", ErrTicket
}

// NewVerifyTicket erzeugt einen neuen Bestätigungslink (z. B. „E-Mail erneut senden“).
// Liefert die Adresse, an die er gehen soll.
func (u *Users) NewVerifyTicket(login string) (name, email, ticket string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberByLoginLocked(login)
	if x == nil {
		return "", "", "", false
	}
	to := x.Member.PendingEmail
	if to == "" && !x.Member.EmailOK {
		to = x.Member.Email
	}
	if to == "" {
		return "", "", "", false
	}
	ticket = newToken()
	x.Member.VerifyHash, x.Member.VerifyUntil = hashToken(ticket), u.now().Add(ticketTTL)
	u.markDirty()
	return x.Name, to, ticket, true
}

// NewResetTicket erzeugt einen Link zum Zurücksetzen des Passworts (nur mit bestätigter E-Mail).
func (u *Users) NewResetTicket(login string) (name, email, ticket string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberByLoginLocked(login)
	if x == nil || x.Member.Email == "" || !x.Member.EmailOK || x.Member.Disabled {
		return "", "", "", false
	}
	ticket = newToken()
	x.Member.ResetHash, x.Member.ResetUntil = hashToken(ticket), u.now().Add(resetTTL)
	u.markDirty()
	return x.Name, x.Member.Email, ticket, true
}

// ResetInfo verrät, ob für das Zurücksetzen ein zweiter Faktor nötig ist.
func (u *Users) ResetInfo(ticket string) (name string, twoFA bool, err error) {
	h := hashToken(ticket)
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, x := range u.byID {
		m := x.Member
		if m != nil && m.ResetHash != "" && subtle.ConstantTimeCompare([]byte(m.ResetHash), []byte(h)) == 1 && u.now().Before(m.ResetUntil) {
			return x.Name, m.TOTP != "", nil
		}
	}
	return "", false, ErrTicket
}

// ResetInput setzt das Passwort zurück – per E-Mail-Link (Ticket) oder per Wiederherstellungscode.
type ResetInput struct {
	Ticket   string      `json:"ticket"`
	Login    string      `json:"login"`    // mit Wiederherstellungscode
	Recovery string      `json:"recovery"` // Wiederherstellungscode
	Code     string      `json:"code"`     // TOTP (falls 2FA aktiv und per E-Mail-Link)
	Keys     KeyMaterial `json:"keys"`
}

// ResetPassword setzt ein neues Passwort. Weil der alte private Schlüssel nur mit dem alten
// Passwort lesbar war, wird auch ein neues Schlüsselpaar gesetzt – ältere verschlüsselte
// Direktnachrichten sind danach nicht mehr lesbar. Alle Sitzungen enden.
func (u *Users) ResetPassword(in ResetInput) (string, error) {
	if err := in.Keys.validate(true); err != nil {
		return "", err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	var x *user
	if in.Ticket != "" {
		h := hashToken(in.Ticket)
		for _, y := range u.byID {
			m := y.Member
			if m != nil && m.ResetHash != "" && subtle.ConstantTimeCompare([]byte(m.ResetHash), []byte(h)) == 1 && u.now().Before(m.ResetUntil) {
				x = y
				break
			}
		}
		if x == nil {
			return "", ErrTicket
		}
		if err := x.Member.second(in.Code, in.Recovery, u.now()); err != nil {
			return "", err
		}
	} else {
		x = u.memberByLoginLocked(in.Login)
		if x == nil || !x.Member.useRecovery(in.Recovery) {
			return "", ErrBad2FA
		}
	}
	if x.Member.Disabled {
		return "", ErrDisabled
	}
	m := x.Member
	m.setAuth(in.Keys)
	m.ResetHash = ""
	u.endSessionsLocked(x, "")
	u.markDirty()
	return x.Name, nil
}

// ChangePassword: neues Passwort, der private Schlüssel wird im Browser neu verschlüsselt
// (gleiches Schlüsselpaar). Andere Sitzungen enden.
func (u *Users) ChangePassword(token, oldAuth, code string, k KeyMaterial) error {
	if err := k.validate(true); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return ErrNoMember
	}
	if !x.Member.checkAuth(oldAuth) {
		return ErrBadLogin
	}
	if err := x.Member.second(code, "", u.now()); err != nil {
		return err
	}
	if k.PubKey != x.Member.PubKey && x.Member.PubKey != "" {
		return ErrKDF // Schlüsselpaar bleibt beim Passwortwechsel gleich
	}
	x.Member.setAuth(k)
	u.endSessionsLocked(x, hashToken(token))
	u.markDirty()
	return nil
}

// SetKeys setzt das Schlüsselpaar neu (z. B. wenn es fehlt). Alte E2E-Nachrichten werden unlesbar.
func (u *Users) SetKeys(token, auth, code, pub, priv string) error {
	k := KeyMaterial{PubKey: pub, PrivKey: priv}
	if b, err := base64.StdEncoding.DecodeString(k.PubKey); err != nil || len(b) != 65 || b[0] != 4 {
		return ErrKDF
	}
	if b, err := base64.StdEncoding.DecodeString(k.PrivKey); err != nil || len(b) < 40 || len(b) > 1024 {
		return ErrKDF
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return ErrNoMember
	}
	if !x.Member.checkAuth(auth) {
		return ErrBadLogin
	}
	if err := x.Member.second(code, "", u.now()); err != nil {
		return err
	}
	x.Member.PubKey, x.Member.PrivKey = pub, priv
	u.markDirty()
	return nil
}

// AccountView ist die Sicht der Person auf ihr eigenes Konto.
type AccountView struct {
	Name     string        `json:"name"`
	ID       string        `json:"id"`
	Member   *MemberInfo   `json:"account"`
	PubKey   string        `json:"pubKey,omitempty"`
	PrivKey  string        `json:"privKey,omitempty"`
	KDFSalt  string        `json:"kdfSalt"`
	KDFIter  int           `json:"kdfIter"`
	Sessions []SessionView `json:"sessions"`
	Pending  bool          `json:"totpPending,omitempty"`
}

type SessionView struct {
	ID       string    `json:"id"` // gekürzter Hash
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed"`
	Agent    string    `json:"agent,omitempty"`
	Current  bool      `json:"current"`
}

// Account liefert die Kontodaten zu einem Sitzungs-Token.
func (u *Users) Account(token string) (AccountView, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return AccountView{}, ErrNoMember
	}
	m := x.Member
	cur := hashToken(token)
	v := AccountView{Name: x.Name, ID: x.ID, Member: m.info(), PubKey: m.PubKey, PrivKey: m.PrivKey,
		KDFSalt: m.KDFSalt, KDFIter: m.KDFIter, Sessions: []SessionView{}, Pending: m.TOTPPending != ""}
	for _, s := range m.Sessions {
		v.Sessions = append(v.Sessions, SessionView{ID: s.Hash[:12], Created: s.Created, LastUsed: s.LastUsed, Agent: s.Agent, Current: s.Hash == cur})
	}
	return v, nil
}

// EndSession beendet eine bestimmte Sitzung (ID = gekürzter Hash) oder mit "others" alle anderen.
func (u *Users) EndSession(token, id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return ErrNoMember
	}
	cur := hashToken(token)
	if id == "others" {
		u.endSessionsLocked(x, cur)
	} else {
		var rest []memberSession
		for _, s := range x.Member.Sessions {
			if strings.HasPrefix(s.Hash, id) && len(id) >= 8 {
				delete(u.byToken, s.Hash)
				continue
			}
			rest = append(rest, s)
		}
		x.Member.Sessions = rest
		u.sessionsChangedLocked(x, false)
	}
	u.markDirty()
	return nil
}

// Begin2FA erzeugt ein neues TOTP-Geheimnis (noch nicht aktiv).
func (u *Users) Begin2FA(token, auth, code string) (name, secret string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return "", "", ErrNoMember
	}
	if !x.Member.checkAuth(auth) {
		return "", "", ErrBadLogin
	}
	if err := x.Member.second(code, "", u.now()); err != nil { // schon aktiv: nur mit aktuellem Code neu einrichten
		return "", "", err
	}
	x.Member.TOTPPending = newTOTPSecret()
	u.markDirty()
	return x.Name, x.Member.TOTPPending, nil
}

// Enable2FA aktiviert TOTP nach Eingabe eines gültigen Codes und liefert neue Wiederherstellungscodes.
func (u *Users) Enable2FA(token, code string) ([]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return nil, ErrNoMember
	}
	m := x.Member
	if m.TOTPPending == "" {
		return nil, ErrNo2FASetup
	}
	st, ok := checkTOTP(m.TOTPPending, code, u.now(), 0)
	if !ok {
		return nil, ErrBad2FA
	}
	m.TOTP, m.TOTPPending, m.TOTPLast = m.TOTPPending, "", st
	var codes []string
	codes, m.Recovery = newRecoveryCodes()
	u.endSessionsLocked(x, hashToken(token))
	u.markDirty()
	return codes, nil
}

// Disable2FA schaltet TOTP ab (Passwort und Code nötig).
func (u *Users) Disable2FA(token, auth, code string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return ErrNoMember
	}
	m := x.Member
	if !m.checkAuth(auth) {
		return ErrBadLogin
	}
	if err := m.second(code, "", u.now()); err != nil {
		return err
	}
	m.TOTP, m.TOTPPending, m.TOTPLast = "", "", 0
	u.markDirty()
	return nil
}

// NewRecovery erzeugt neue Wiederherstellungscodes (die alten werden ungültig).
func (u *Users) NewRecovery(token, auth, code string) ([]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return nil, ErrNoMember
	}
	if !x.Member.checkAuth(auth) {
		return nil, ErrBadLogin
	}
	if err := x.Member.second(code, "", u.now()); err != nil {
		return nil, err
	}
	var codes []string
	codes, x.Member.Recovery = newRecoveryCodes()
	u.markDirty()
	return codes, nil
}

// SetEmail ändert die E-Mail-Adresse. verify = neue Adresse muss bestätigt werden (Ticket).
func (u *Users) SetEmail(token, auth, code, email string, verify bool) (string, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return "", err
	}
	if verify && email == "" {
		return "", ErrEmail
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return "", ErrNoMember
	}
	if !x.Member.checkAuth(auth) {
		return "", ErrBadLogin
	}
	if err := x.Member.second(code, "", u.now()); err != nil {
		return "", err
	}
	if email != "" && u.emailUsedLocked(email, x.ID) {
		return "", ErrEmailTaken
	}
	m := x.Member
	ticket := ""
	if verify { // alte Adresse bleibt gültig, bis die neue bestätigt ist
		ticket = newToken()
		m.PendingEmail, m.VerifyHash, m.VerifyUntil = email, hashToken(ticket), u.now().Add(ticketTTL)
	} else {
		m.Email, m.EmailOK, m.PendingEmail, m.VerifyHash = email, email != "", "", ""
	}
	u.markDirty()
	return ticket, nil
}

// SetNoFed: Direktnachrichten von anderen Instanzen annehmen oder nicht.
func (u *Users) SetNoFed(token string, off bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return ErrNoMember
	}
	x.Member.NoFed = off
	u.markDirty()
	return nil
}

// DeleteOwn löscht das eigene Konto (Passwort nötig).
func (u *Users) DeleteOwn(token, auth, code string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.memberOfTokenLocked(token)
	if x == nil {
		return "", ErrNoMember
	}
	if !x.Member.checkAuth(auth) {
		return "", ErrBadLogin
	}
	if err := x.Member.second(code, "", u.now()); err != nil {
		return "", err
	}
	u.removeLocked(x)
	return x.ID, nil
}

// --- Admin ---

// AdminMember ändert ein Konto aus dem Admin-Bereich.
func (u *Users) AdminMember(id, action string) (UserInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return UserInfo{}, ErrNoUser
	}
	if x.Member == nil {
		return UserInfo{}, ErrNoMember
	}
	m := x.Member
	switch action {
	case "disable":
		m.Disabled = true
		u.endSessionsLocked(x, "")
		u.sessionsChangedLocked(x, true)
	case "enable":
		m.Disabled = false
	case "reset2fa":
		m.TOTP, m.TOTPPending, m.TOTPLast = "", "", 0
	case "verify":
		if m.Email != "" {
			m.EmailOK, m.VerifyHash = true, ""
		}
	case "logout":
		u.endSessionsLocked(x, "")
	default:
		return UserInfo{}, errors.New("Unbekannte Aktion")
	}
	u.markDirty()
	return x.info(), nil
}

// PubKeyOf liefert den öffentlichen E2E-Schlüssel eines Kontos.
func (u *Users) PubKeyOf(id string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil || x.Member == nil || x.Member.PubKey == "" {
		return "", false
	}
	return x.Member.PubKey, true
}

// MemberByName findet ein Konto über den Namen (für die Föderation).
func (u *Users) MemberByName(name string) (id, display, pub string, noFed, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byKey[nameKey(name)]
	if x == nil || x.Member == nil || x.Member.Disabled {
		return "", "", "", false, false
	}
	return x.ID, x.Name, x.Member.PubKey, x.Member.NoFed, true
}

// IsMember: hat diese Person ein Konto?
func (u *Users) IsMember(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	return x != nil && x.Member != nil
}

// expireSessionsLocked beendet lange ungenutzte Sitzungen.
func (u *Users) expireSessionsLocked() {
	cut := u.now().Add(-memberSessionTTL)
	for _, x := range u.byID {
		if x.Member == nil {
			continue
		}
		var rest []memberSession
		for _, s := range x.Member.Sessions {
			if s.LastUsed.Before(cut) {
				delete(u.byToken, s.Hash)
				u.dirty.Store(true)
				continue
			}
			rest = append(rest, s)
		}
		x.Member.Sessions = rest
	}
}

// touchSessionLocked merkt sich die letzte Nutzung einer Sitzung (höchstens einmal je Minute).
func (u *Users) touchSessionLocked(x *user, hash string) {
	for i := range x.Member.Sessions {
		s := &x.Member.Sessions[i]
		if s.Hash == hash {
			if u.now().Sub(s.LastUsed) > time.Minute {
				s.LastUsed = u.now()
				u.markDirty()
			}
			return
		}
	}
}
