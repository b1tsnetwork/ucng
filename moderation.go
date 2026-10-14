// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Moderation bündelt alles, was Personen betrifft:
//   - Sanktionen: stummschalten (zeitlich) und sperren (zeitlich oder dauerhaft, optional
//     zusätzlich für die Verbindung/IP, damit kein neuer Name gewählt werden kann)
//   - geschützte Namen (z. B. „Admin“, „Support*“) – gleiche Mustersprache wie die Automod
//   - Spam-Bremsen: Langsam-Modus pro Kanal und Erkennung von Wiederholungen
//
// IP-Adressen werden nie gespeichert, nur ein Schlüssel-Hash (HMAC) – der lässt sich ohne den
// geheimen Schlüssel des Servers nicht auf die Adresse zurückrechnen.

type Sanction struct {
	UserID  string     `json:"userId"`
	Name    string     `json:"name"` // Name zum Zeitpunkt der Sanktion
	Kind    string     `json:"kind"` // "mute" oder "ban"
	Until   *time.Time `json:"until,omitempty"`
	Reason  string     `json:"reason,omitempty"`
	By      string     `json:"by"`
	Created time.Time  `json:"created"`
	IPHash  string     `json:"ipHash,omitempty"` // nur bei Sperre „auch für diese Verbindung“
	ByBot   string     `json:"byBot,omitempty"`  // ID des Bots, der die Sperre verhängt hat
}

func (s Sanction) active(now time.Time) bool { return s.Until == nil || now.Before(*s.Until) }

// SanctionInfo ist die öffentliche Sicht für die betroffene Person (ohne IP-Hash und Urheber).
type SanctionInfo struct {
	Kind   string     `json:"kind"`
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

var defaultProtectedNames = []string{
	"# Namen, die Besucher nicht wählen können (gleiche Muster wie bei der Automod)",
	"admin*", "*administrator*", "moderator*", "mod", "support*", "team", "*ucng*", "*gochat*", "system", "bot",
}

type SanctionError struct {
	Kind   string
	Until  *time.Time
	Reason string
	loc    *time.Location
}

func (e *SanctionError) Error() string {
	var msg string
	switch {
	case e.Kind == "mute" && e.Until != nil:
		msg = fmt.Sprintf("Du bist stummgeschaltet bis %s.", fmtUntil(*e.Until, e.loc))
	case e.Kind == "mute":
		msg = "Du bist stummgeschaltet." //i18n
	case e.Until != nil:
		msg = fmt.Sprintf("Du bist vom Chat ausgeschlossen bis %s.", fmtUntil(*e.Until, e.loc))
	default:
		msg = "Du bist vom Chat ausgeschlossen." //i18n
	}
	if e.Reason != "" {
		msg = fmt.Sprintf("%s Grund: %s", msg, e.Reason)
	}
	return msg
}

func fmtUntil(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	now := time.Now().In(loc)
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return fmt.Sprintf("%s Uhr", t.Format("15:04"))
	}
	return fmt.Sprintf("%s Uhr", t.Format("02.01.2006, 15:04"))
}

var (
	ErrNameProtected = errors.New("Dieser Name ist geschützt. Bitte wähle einen anderen.")
	ErrRepeat        = errors.New("Diese Nachricht hast du gerade schon geschickt.")
	ErrLinksKnown    = errors.New("Links sind in diesem Kanal erst erlaubt, wenn dein Name älter als 24 Stunden ist.")
	ErrLinksTeam     = errors.New("Links sind in diesem Kanal nur für das Team erlaubt.")
	ErrNoSanction    = errors.New("Für diese Person gibt es keine Sperre")
)

// SlowModeError: Langsam-Modus – noch Wartezeit.
type SlowModeError struct{ Wait time.Duration }

func (e *SlowModeError) Error() string {
	s := int(e.Wait.Seconds() + 0.999)
	if s <= 1 {
		return "Langsam-Modus: Bitte warte noch eine Sekunde." //i18n
	}
	return fmt.Sprintf("Langsam-Modus: Bitte warte noch %d Sekunden.", s)
}

type recentMsg struct {
	norm string
	at   time.Time
}

type Moderation struct {
	mu        sync.Mutex
	sanctions map[string]*Sanction // Nutzer-ID → Sanktion
	ipKey     []byte
	protected *Automod
	lastIP    map[string]string      // Nutzer-ID → IP-Hash (nur im Speicher)
	recent    map[string][]recentMsg // Nutzer-ID → letzte Nachrichten (Wiederholungen)
	lastPost  map[string]time.Time   // Nutzer-ID + Kanal → letzte Nachricht (Langsam-Modus)
	changed   func()
	now       func() time.Time
}

func NewModeration() *Moderation {
	m := &Moderation{
		sanctions: map[string]*Sanction{}, ipKey: []byte(randomHex(32)), protected: NewAutomod(),
		lastIP: map[string]string{}, recent: map[string][]recentMsg{}, lastPost: map[string]time.Time{},
		now: time.Now,
	}
	m.protected.Set(AutomodConfig{Enabled: true, Patterns: defaultProtectedNames})
	return m
}

func (m *Moderation) markChanged() {
	if m.changed != nil {
		m.changed()
	}
}

// IPHash macht aus einer IP-Adresse einen nicht umkehrbaren Schlüssel.
func (m *Moderation) IPHash(ip string) string {
	if ip == "" {
		return ""
	}
	mac := hmac.New(sha256.New, m.ipKey)
	mac.Write([]byte(ip))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

// PresenceSig signiert eine Nutzer-ID für die Online-Liste (siehe /api/session).
// MediaSig signiert die Adresse eines Bildes aus einem privaten Kanal (gültig bis exp).
func (m *Moderation) MediaSig(file string, exp int64) string {
	mac := hmac.New(sha256.New, m.ipKey)
	fmt.Fprintf(mac, "media:%s:%d", file, exp)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

func (m *Moderation) PresenceSig(userID string) string {
	mac := hmac.New(sha256.New, m.ipKey)
	mac.Write([]byte("presence:" + userID))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

func (m *Moderation) SeenIP(userID, ipHash string) {
	if userID == "" || ipHash == "" {
		return
	}
	m.mu.Lock()
	m.lastIP[userID] = ipHash
	m.mu.Unlock()
}

// --- geschützte Namen ---

func (m *Moderation) Protected(name string) bool {
	_, _, hit := m.protected.Test(name)
	return hit
}

func (m *Moderation) ProtectedConfig() []string { return m.protected.Config().Patterns }

func (m *Moderation) SetProtected(patterns []string) ([]string, error) {
	cfg, err := m.protected.Set(AutomodConfig{Enabled: true, Patterns: patterns})
	if err != nil {
		return nil, err
	}
	m.markChanged()
	return cfg.Patterns, nil
}

// --- Sanktionen ---

// Sanction verhängt oder ersetzt eine Sanktion. minutes = 0 bedeutet dauerhaft (nur bei Sperren).
func (m *Moderation) Sanction(userID, name, kind string, minutes int, reason, by string, withIP bool) (Sanction, error) {
	if kind != "mute" && kind != "ban" {
		return Sanction{}, errors.New("Unbekannte Art der Sanktion")
	}
	if kind == "mute" && minutes <= 0 {
		return Sanction{}, errors.New("Stummschalten braucht eine Dauer")
	}
	if len([]rune(reason)) > 200 {
		return Sanction{}, errors.New("Begründung darf höchstens 200 Zeichen lang sein")
	}
	now := m.now().UTC()
	s := &Sanction{UserID: userID, Name: name, Kind: kind, Reason: strings.TrimSpace(reason), By: by, Created: now}
	if minutes > 0 {
		t := now.Add(time.Duration(minutes) * time.Minute)
		s.Until = &t
	}
	m.mu.Lock()
	if withIP && kind == "ban" {
		s.IPHash = m.lastIP[userID]
	}
	m.sanctions[userID] = s
	m.mu.Unlock()
	m.markChanged()
	return *s, nil
}

// SanctionBot wie Sanction, merkt sich aber den Bot (er darf nur eigene Sperren aufheben).
func (m *Moderation) SanctionBot(userID, name, kind string, minutes int, reason, by, botID string) (Sanction, error) {
	s, err := m.Sanction(userID, name, kind, minutes, reason, by, false)
	if err != nil {
		return s, err
	}
	m.mu.Lock()
	if cur, ok := m.sanctions[userID]; ok {
		cur.ByBot = botID
		s = *cur
	}
	m.mu.Unlock()
	m.markChanged()
	return s, nil
}

func (m *Moderation) Lift(userID string) (Sanction, error) {
	m.mu.Lock()
	s, ok := m.sanctions[userID]
	if ok {
		delete(m.sanctions, userID)
	}
	m.mu.Unlock()
	if !ok {
		return Sanction{}, ErrNoSanction
	}
	m.markChanged()
	return *s, nil
}

// Active liefert die gültige Sanktion für eine Person oder Verbindung.
func (m *Moderation) Active(userID, ipHash string) (*Sanction, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if s, ok := m.sanctions[userID]; ok && userID != "" && s.active(now) {
		c := *s
		return &c, true
	}
	if ipHash != "" {
		for _, s := range m.sanctions {
			if s.IPHash == ipHash && s.Kind == "ban" && s.active(now) {
				c := *s
				return &c, true
			}
		}
	}
	return nil, false
}

// IPBanned: ist von dieser Verbindung aus das Anlegen neuer Namen gesperrt?
func (m *Moderation) IPBanned(ipHash string) bool {
	_, ok := m.Active("", ipHash)
	return ok
}

func (m *Moderation) List() []Sanction {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	out := []Sanction{}
	for _, s := range m.sanctions {
		if s.active(now) {
			c := *s
			c.IPHash = boolMark(c.IPHash != "")
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// boolMark: Im Admin-Bereich nur zeigen, OB eine Verbindung gesperrt ist – nicht den Hash.
func boolMark(b bool) string {
	if b {
		return "ja"
	}
	return ""
}

// Cleanup entfernt abgelaufene Sanktionen und alte Spam-Zähler.
func (m *Moderation) Cleanup() {
	m.mu.Lock()
	now := m.now()
	changed := false
	for id, s := range m.sanctions {
		if !s.active(now) {
			delete(m.sanctions, id)
			changed = true
		}
	}
	for k, t := range m.lastPost {
		if now.Sub(t) > time.Hour {
			delete(m.lastPost, k)
		}
	}
	for k, list := range m.recent {
		if len(list) == 0 || now.Sub(list[len(list)-1].at) > 10*time.Minute {
			delete(m.recent, k)
		}
	}
	m.mu.Unlock()
	if changed {
		m.markChanged()
	}
}

// --- Spam-Bremsen ---

// CheckSpam prüft Langsam-Modus und Wiederholungen (gleicher Text innerhalb von 2 Minuten:
// kurze Texte höchstens zweimal, ab 30 Zeichen nur einmal) und merkt sich die Nachricht.
func (m *Moderation) CheckSpam(userID, room string, slowSeconds int, text string, repeat bool) error {
	if userID == "" {
		return nil
	}
	norm := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	key := userID + "\x00" + room
	if slowSeconds > 0 {
		if last, ok := m.lastPost[key]; ok {
			if wait := time.Duration(slowSeconds)*time.Second - now.Sub(last); wait > 0 {
				return &SlowModeError{Wait: wait}
			}
		}
	}
	if !repeat { // Bots: nur Langsam-Modus
		m.lastPost[key] = now
		return nil
	}
	list := m.recent[userID]
	keep := list[:0]
	for _, r := range list {
		if now.Sub(r.at) < 2*time.Minute {
			keep = append(keep, r)
		}
	}
	// Kurze Nachrichten („ok“, „ja“) dürfen sich einmal wiederholen, längere nicht.
	limit := 2
	if len([]rune(norm)) >= 30 {
		limit = 1
	}
	same := 0
	for _, r := range keep {
		if r.norm == norm && len(norm) > 0 {
			same++
		}
	}
	if same >= limit {
		m.recent[userID] = keep
		return ErrRepeat
	}
	keep = append(keep, recentMsg{norm: norm, at: now})
	if len(keep) > 8 {
		keep = keep[len(keep)-8:]
	}
	m.recent[userID] = keep
	m.lastPost[key] = now
	return nil
}

// --- Speichern ---

type moderationDoc struct {
	IPKey          string     `json:"ipKey"`
	ProtectedNames []string   `json:"protectedNames"`
	Sanctions      []Sanction `json:"sanctions"`
}

func (m *Moderation) export() moderationDoc {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := moderationDoc{IPKey: string(m.ipKey), ProtectedNames: m.protected.Config().Patterns, Sanctions: []Sanction{}}
	for _, s := range m.sanctions {
		d.Sanctions = append(d.Sanctions, *s)
	}
	sort.Slice(d.Sanctions, func(i, j int) bool { return d.Sanctions[i].Created.Before(d.Sanctions[j].Created) })
	return d
}

func (m *Moderation) restore(d moderationDoc) error {
	if d.IPKey != "" {
		m.ipKey = []byte(d.IPKey)
	}
	if d.ProtectedNames != nil {
		if _, err := m.protected.Set(AutomodConfig{Enabled: true, Patterns: d.ProtectedNames}); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range d.Sanctions {
		s := d.Sanctions[i]
		m.sanctions[s.UserID] = &s
	}
	return nil
}
