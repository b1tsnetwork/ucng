// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Namensreservierung ohne Registrierung:
// Wer einen freien Namen wählt, bekommt ein geheimes Token. Nur mit diesem Token
// kann unter dem Namen geschrieben werden. Der Server speichert nur einen Hash des
// Tokens. Ungenutzte Namen werden nach einer einstellbaren Zeit wieder frei.

var (
	ErrNameTaken    = errors.New("Dieser Name ist bereits vergeben")
	ErrTooManyUsers = errors.New("Maximale Anzahl an Namen erreicht")
	ErrNoSession    = errors.New("Bitte zuerst einen Namen wählen")
	ErrNoUser       = errors.New("Diese Person gibt es nicht (mehr)")
)

type user struct {
	ID        string    `json:"id"` // öffentlich, stabil – ordnet Nachrichten einer Person zu
	Name      string    `json:"name"`
	TokenHash string    `json:"tokenHash"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"lastSeen"`
	Team      bool      `json:"team,omitempty"`  // Team-Abzeichen (vom Admin vergeben)
	Roles     []string  `json:"roles,omitempty"` // weitere Rollen, siehe roles.go
	// Profil
	Avatar     string `json:"avatar,omitempty"`     // Datei unter media/avatars
	Status     string `json:"status,omitempty"`     // "", away, busy
	StatusText string `json:"statusText,omitempty"` // kurzer Text
	// Angepinnte Kanäle in der Seitenleiste
	Pinned []string `json:"pinned,omitempty"`
	// Konto (nur Modus „hosted“, siehe members.go)
	Member *member `json:"member,omitempty"`
}

type Users struct {
	mu         sync.Mutex
	byKey      map[string]*user // normalisierter Name → Nutzer
	byToken    map[string]*user // Token-Hash → Nutzer
	byID       map[string]*user // Nutzer-ID → Nutzer
	ttl        time.Duration
	max        int
	dirty      atomic.Bool
	now        func() time.Time // für Tests austauschbar
	onChange   atomic.Pointer[func()]
	reserved   atomic.Pointer[func(name string, team bool) error]  // Bot-Namen, geschützte Namen
	sessEnded  atomic.Pointer[func(userID string, alive []string)] // Konten: Sitzungen beendet (Streams schließen)
	privileged atomic.Pointer[func(roles []string) bool]           // Rollen mit Recht auf geschützte Namen
}

// SetReserved legt fest, welche Namen zusätzlich gesperrt sind (Bot-Namen, geschützte Namen).
// team = die Person hat das Team-Abzeichen (darf geschützte Namen tragen).
func (u *Users) SetReserved(f func(name string, team bool) error) { u.reserved.Store(&f) }

// SetPrivileged legt fest, welche Rollen geschützte Namen tragen dürfen.
func (u *Users) SetPrivileged(f func(roles []string) bool) { u.privileged.Store(&f) }

// Taken: ist der Name derzeit von einer Person reserviert?
func (u *Users) Taken(name string) bool {
	name, ok := CleanName(name)
	if !ok {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byKey[nameKey(name)]
	return x != nil && !u.expired(x)
}

func (u *Users) markDirty() {
	u.dirty.Store(true)
	if f := u.onChange.Load(); f != nil {
		(*f)()
	}
}

// OnChange setzt die Funktion, die bei jeder Änderung aufgerufen wird.
func (u *Users) OnChange(f func()) { u.onChange.Store(&f) }

func NewUsers(ttl time.Duration, max int) *Users {
	return &Users{byKey: map[string]*user{}, byToken: map[string]*user{}, byID: map[string]*user{}, ttl: ttl, max: max, now: time.Now}
}

// CleanName entfernt Steuerzeichen und fasst Leerraum zusammen.
func CleanName(n string) (string, bool) {
	n = strings.Join(strings.Fields(n), " ")
	return cleanText(n, maxNameLen)
}

// nameKey macht Namen vergleichbar: "Anna", "anna" und " ANNA " sind derselbe Name.
func nameKey(n string) string { return strings.ToLower(n) }

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func newUserID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Identity ist die zu einem Token gehörende Person.
type Identity struct {
	ID      string
	Name    string
	Team    bool
	Roles   []string
	Created time.Time
	Member  bool   // hat ein Konto (Modus „hosted“)
	Session string // Konten: Kennung der aktuellen Sitzung (Anfang des Token-Hashs)
}

// UserInfo ist die Sicht im Admin-Bereich.
type UserInfo struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Team       bool        `json:"team"`
	Roles      []string    `json:"roles"`
	Avatar     string      `json:"avatar,omitempty"` // Adresse des Profilbilds
	Status     string      `json:"status,omitempty"`
	StatusText string      `json:"statusText,omitempty"`
	Created    time.Time   `json:"created"`
	LastSeen   time.Time   `json:"lastSeen"`
	Account    *MemberInfo `json:"account,omitempty"` // nur bei Konten
}

func (x *user) info() UserInfo {
	return UserInfo{ID: x.ID, Name: x.Name, Team: x.Team, Roles: append([]string{}, x.Roles...), Avatar: avatarURL(x.Avatar),
		Status: x.Status, StatusText: x.StatusText, Created: x.Created, LastSeen: x.LastSeen, Account: x.memberInfo()}
}

func (x *user) memberInfo() *MemberInfo {
	if x.Member == nil {
		return nil
	}
	return x.Member.info()
}

func (x *user) identity() Identity {
	return Identity{ID: x.ID, Name: x.Name, Team: x.Team, Roles: append([]string(nil), x.Roles...), Created: x.Created, Member: x.Member != nil}
}

// ClaimResult beschreibt das Ergebnis einer Namensreservierung.
type ClaimResult struct {
	Token   string
	Name    string
	ID      string
	Renamed bool // bestehende Person hat einen anderen Namen gewählt
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (u *Users) expired(x *user) bool {
	if x.Member != nil {
		return false // Konten laufen nicht ab
	}
	return u.ttl > 0 && u.now().Sub(x.LastSeen) > u.ttl
}

func (u *Users) removeLocked(x *user) {
	delete(u.byKey, nameKey(x.Name))
	delete(u.byToken, x.TokenHash)
	if x.Member != nil {
		for _, s := range x.Member.Sessions {
			delete(u.byToken, s.Hash)
		}
		u.sessionsChangedLocked(x, true)
	}
	delete(u.byID, x.ID)
	u.markDirty()
}

// Claim reserviert einen Namen. Mit gültigem bisherigen Token wird umbenannt
// (Token und ID bleiben gleich, der alte Name wird frei).
func (u *Users) Claim(name, token string) (ClaimResult, error) {
	name, ok := CleanName(name)
	if !ok {
		return ClaimResult{}, ErrInvalidName
	}
	// Bot- und geschützte Namen prüfen, bevor u.mu gesperrt wird (vermeidet Verklemmung
	// mit der Bot-Verwaltung).
	team := false
	if token != "" {
		if me, ok := u.Lookup(token); ok {
			team = me.Team
			if f := u.privileged.Load(); !team && f != nil && len(me.Roles) > 0 {
				team = (*f)(me.Roles)
			}
		}
	}
	if f := u.reserved.Load(); f != nil {
		if err := (*f)(name, team); err != nil {
			return ClaimResult{}, err
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()

	var me *user
	if token != "" {
		if x := u.byToken[hashToken(token)]; x != nil && !u.expired(x) {
			me = x
		}
	}

	if other := u.byKey[nameKey(name)]; other != nil && other != me {
		if !u.expired(other) {
			return ClaimResult{}, ErrNameTaken
		}
		u.removeLocked(other)
	}

	if me != nil && me.Member != nil {
		if nameKey(me.Name) != nameKey(name) {
			return ClaimResult{}, ErrAccountRename
		}
		return ClaimResult{Token: token, Name: me.Name, ID: me.ID}, nil
	}
	if me != nil { // Umbenennen oder nur Groß-/Kleinschreibung ändern
		renamed := me.Name != name
		delete(u.byKey, nameKey(me.Name))
		me.Name, me.LastSeen = name, now
		u.byKey[nameKey(name)] = me
		u.markDirty()
		return ClaimResult{Token: token, Name: name, ID: me.ID, Renamed: renamed}, nil
	}

	if len(u.byKey) >= u.max {
		u.expireLocked()
		if len(u.byKey) >= u.max {
			return ClaimResult{}, ErrTooManyUsers
		}
	}
	tok := newToken()
	x := &user{ID: newUserID(), Name: name, TokenHash: hashToken(tok), Created: now, LastSeen: now}
	u.byKey[nameKey(name)] = x
	u.byToken[x.TokenHash] = x
	u.byID[x.ID] = x
	u.markDirty()
	return ClaimResult{Token: tok, Name: name, ID: x.ID}, nil
}

// Lookup liefert die Person zu einem Token und merkt sich die Aktivität.
func (u *Users) Lookup(token string) (Identity, bool) {
	if token == "" {
		return Identity{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	h := hashToken(token)
	x := u.byToken[h]
	if x == nil {
		return Identity{}, false
	}
	if x.Member != nil {
		if x.Member.Disabled {
			return Identity{}, false
		}
		u.touchSessionLocked(x, h)
	}
	if u.expired(x) {
		u.removeLocked(x)
		return Identity{}, false
	}
	if u.now().Sub(x.LastSeen) > time.Minute {
		x.LastSeen = u.now()
		u.markDirty()
	}
	id := x.identity()
	if x.Member != nil {
		id.Session = sessionID(h)
	}
	return id, true
}

// Get liefert eine Person anhand ihrer ID.
func (u *Users) Get(id string) (UserInfo, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return UserInfo{}, false
	}
	return x.info(), true
}

// List liefert reservierte Namen, zuletzt aktive zuerst; q filtert nach Name oder ID.
func (u *Users) List(q string, limit int) ([]UserInfo, int) {
	q = strings.ToLower(strings.TrimSpace(q))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	u.mu.Lock()
	out := []UserInfo{}
	for _, x := range u.byID {
		if u.expired(x) {
			continue
		}
		if q == "" || strings.Contains(nameKey(x.Name), q) || x.ID == q {
			out = append(out, x.info())
		}
	}
	u.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

// SetTeam vergibt oder entzieht das Team-Abzeichen.
func (u *Users) SetTeam(id string, team bool) (UserInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return UserInfo{}, ErrNoUser
	}
	if x.Team != team {
		x.Team = team
		u.markDirty()
	}
	return x.info(), nil
}

// SetRoles setzt die Rollen einer Person (IDs, bereits geprüft).
func (u *Users) SetRoles(id string, roles []string) (UserInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return UserInfo{}, ErrNoUser
	}
	if strings.Join(x.Roles, ",") != strings.Join(roles, ",") {
		x.Roles = append([]string(nil), roles...)
		u.markDirty()
	}
	return x.info(), nil
}

// RoleMap: alle (nicht abgelaufenen) Personen mit Rollen → Rollen-IDs.
func (u *Users) RoleMap() map[string][]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string][]string{}
	for _, x := range u.byID {
		if len(x.Roles) > 0 && !u.expired(x) {
			out[x.ID] = append([]string{}, x.Roles...)
		}
	}
	return out
}

func avatarURL(file string) string {
	if file == "" {
		return ""
	}
	return "/media/avatars/" + file
}

// Profile ist das öffentliche Profil einer Person.
type Profile struct {
	Avatar string `json:"avatar,omitempty"`
	Status string `json:"status,omitempty"`
	Text   string `json:"text,omitempty"`
}

// SetAvatar setzt das Profilbild (Datei) und liefert die bisherige Datei.
func (u *Users) SetAvatar(id, file string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return "", ErrNoUser
	}
	old := x.Avatar
	x.Avatar = file
	u.markDirty()
	return old, nil
}

// SetStatus setzt Status und Statustext.
func (u *Users) SetStatus(id, status, text string) (UserInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return UserInfo{}, ErrNoUser
	}
	x.Status, x.StatusText = status, text
	u.markDirty()
	return x.info(), nil
}

// ProfileMap: alle Personen mit Profilbild oder Status.
func (u *Users) ProfileMap() map[string]Profile {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]Profile{}
	for _, x := range u.byID {
		if (x.Avatar != "" || x.Status != "" || x.StatusText != "") && !u.expired(x) {
			out[x.ID] = Profile{Avatar: avatarURL(x.Avatar), Status: x.Status, Text: x.StatusText}
		}
	}
	return out
}

// AvatarFiles: alle benutzten Profilbild-Dateien.
func (u *Users) AvatarFiles() map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]bool{}
	for _, x := range u.byID {
		if x.Avatar != "" && !u.expired(x) {
			out[x.Avatar] = true
		}
	}
	return out
}

// RemoveByID gibt den Namen einer Person frei (ihr Token wird ungültig).
func (u *Users) RemoveByID(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return false
	}
	u.removeLocked(x)
	return true
}

// Release gibt den Namen eines Tokens wieder frei.
func (u *Users) Release(token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	h := hashToken(token)
	if x := u.byToken[h]; x != nil {
		if x.Member != nil { // Konto: nur diese Sitzung beenden
			delete(u.byToken, h)
			var rest []memberSession
			for _, s := range x.Member.Sessions {
				if s.Hash != h {
					rest = append(rest, s)
				}
			}
			x.Member.Sessions = rest
			u.sessionsChangedLocked(x, false)
			u.markDirty()
			return
		}
		u.removeLocked(x)
	}
}

func (u *Users) expireLocked() {
	u.expireSessionsLocked()
	for _, x := range u.byKey {
		if u.expired(x) {
			u.removeLocked(x)
		}
	}
}

// Expire gibt alle abgelaufenen Namen frei.
func (u *Users) Expire() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.expireLocked()
}

func (u *Users) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.byKey)
}

func (u *Users) export() []user {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]user, 0, len(u.byKey))
	for _, x := range u.byKey {
		c := *x
		c.Roles = append([]string(nil), x.Roles...)
		if x.Member != nil { // tiefe Kopie: wird außerhalb der Sperre serialisiert
			m := *x.Member
			m.Sessions = append([]memberSession(nil), x.Member.Sessions...)
			m.Recovery = append([]string(nil), x.Member.Recovery...)
			c.Member = &m
		}
		out = append(out, c)
	}
	return out
}

func (u *Users) restore(list []user) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range list {
		x := list[i]
		if _, ok := CleanName(x.Name); !ok || x.TokenHash == "" || u.expired(&x) {
			continue
		}
		if x.ID == "" { // Daten aus älteren Versionen
			x.ID = newUserID()
			u.markDirty()
		}
		u.byKey[nameKey(x.Name)] = &x
		u.byToken[x.TokenHash] = &x
		u.byID[x.ID] = &x
		if x.Member != nil {
			for _, s := range x.Member.Sessions {
				u.byToken[s.Hash] = &x
			}
		}
	}
}

// SetPinned setzt die angepinnten Kanäle einer Person.
func (u *Users) SetPinned(id string, rooms []string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.byID[id]
	if x == nil {
		return ErrNoUser
	}
	x.Pinned = append([]string(nil), rooms...)
	u.markDirty()
	return nil
}

// PinnedOf liefert die angepinnten Kanäle einer Person.
func (u *Users) PinnedOf(id string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if x := u.byID[id]; x != nil && len(x.Pinned) > 0 {
		return append([]string{}, x.Pinned...)
	}
	return []string{}
}
