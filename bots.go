// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Bots sind Programme, die über die Bot-API schreiben und mitlesen. Der Admin legt sie
// an; jeder Bot hat einen Namen (für Nutzer reserviert), einen geheimen API-Schlüssel,
// erlaubte Kanäle und optional eine Webhook-URL.

type Bot struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	TokenHash     string    `json:"tokenHash"`
	AllRooms      bool      `json:"allRooms"`
	Rooms         []string  `json:"rooms,omitempty"`
	WebhookURL    string    `json:"webhookUrl,omitempty"`
	WebhookSecret string    `json:"webhookSecret,omitempty"`
	ReceiveBots   bool      `json:"receiveBots"`
	CanModerate   bool      `json:"canModerate,omitempty"` // ältere Versionen; wird in Perms übernommen
	Perms         *BotPerms `json:"perms,omitempty"`
	Enabled       bool      `json:"enabled"`
	Created       time.Time `json:"created"`
}

// BotPerms legt fest, was ein Bot darf.
type BotPerms struct {
	// Empfangen und lesen
	Receive  string `json:"receive"`  // "all" = alle Nachrichten, "commands" = nur Befehle (!…, /…), Erwähnungen und Antworten an den Bot
	History  bool   `json:"history"`  // Verlauf abrufen
	Presence bool   `json:"presence"` // Online-Liste abrufen
	// Schreiben
	Write         bool `json:"write"`         // Nachrichten schreiben
	React         bool `json:"react"`         // reagieren
	EditOwn       bool `json:"editOwn"`       // eigene Nachrichten bearbeiten und löschen
	AutomodExempt bool `json:"automodExempt"` // von der Automoderation ausgenommen
	RateLimit     int  `json:"rateLimit"`     // Nachrichten pro 10 Sekunden; 0 = Standard (BOT_RATE)
	// Moderieren
	Delete bool `json:"delete"` // Nachrichten moderativ löschen
	Mute   bool `json:"mute"`   // stummschalten
	Ban    bool `json:"ban"`    // zeitlich ausschließen
	// Kanäle
	Rooms      bool `json:"rooms"`      // Kanäle anlegen (auch temporäre) und die eigenen ändern und löschen
	MaxMinutes int  `json:"maxMinutes"` // längste Sperre, die der Bot verhängen darf
}

const (
	BotReceiveAll      = "all"
	BotReceiveCommands = "commands"
	maxBotSanction     = 7 * 24 * 60
)

// defaultBotPerms: lesen und schreiben, nichts weiter.
func defaultBotPerms() BotPerms {
	return BotPerms{Receive: BotReceiveAll, History: true, Write: true, MaxMinutes: 60}
}

// P liefert die Rechte des Bots (mit Standardwerten für ältere Daten).
func (b Bot) P() BotPerms {
	if b.Perms != nil {
		return *b.Perms
	}
	p := defaultBotPerms()
	if b.CanModerate {
		p.Delete, p.Mute, p.MaxMinutes = true, true, 24*60
	}
	return p
}

// CanModerate: hat der Bot irgendein Moderationsrecht?
func (p BotPerms) Moderates() bool { return p.Delete || p.Mute || p.Ban }

var (
	ErrBotReceive  = errors.New("Empfang muss all oder commands sein")
	ErrBotRateVal  = errors.New("Nachrichten-Limit: 0 (Standard) bis 600 pro 10 Sekunden")
	ErrBotMaxSanct = errors.New("Längste Sperre: 1 Minute bis 7 Tage")
)

func validBotPerms(p BotPerms) (BotPerms, error) {
	if p.Receive == "" {
		p.Receive = BotReceiveAll
	}
	if p.Receive != BotReceiveAll && p.Receive != BotReceiveCommands {
		return p, ErrBotReceive
	}
	if p.RateLimit < 0 || p.RateLimit > 600 {
		return p, ErrBotRateVal
	}
	if p.MaxMinutes == 0 {
		p.MaxMinutes = 60
	}
	if p.MaxMinutes < 1 || p.MaxMinutes > maxBotSanction {
		return p, ErrBotMaxSanct
	}
	if !p.Write { // ohne Schreibrecht auch nicht bearbeiten
		p.EditOwn = false
	}
	return p, nil
}

// Allowed: darf der Bot in diesem Kanal lesen und schreiben?
func (b Bot) Allowed(room string) bool {
	if b.AllRooms {
		return true
	}
	for _, r := range b.Rooms {
		if r == room {
			return true
		}
	}
	return false
}

// BotInput sind die im Admin-Bereich änderbaren Felder.
type BotInput struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	AllRooms    bool      `json:"allRooms"`
	Rooms       []string  `json:"rooms"`
	WebhookURL  string    `json:"webhookUrl"`
	ReceiveBots bool      `json:"receiveBots"`
	CanModerate bool      `json:"canModerate"` // ältere Clients: löschen + stummschalten
	Perms       *BotPerms `json:"perms"`       // fehlt = unverändert (bzw. Standard beim Anlegen)
	Enabled     *bool     `json:"enabled"`     // fehlt = unverändert (bzw. aktiv beim Anlegen)
}

// Delivery ist das Ergebnis des letzten Webhook-Aufrufs.
type Delivery struct {
	At         time.Time `json:"at"`
	Event      string    `json:"event"`
	Status     int       `json:"status,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"durationMs"`
}

// BotInfo ist die Sicht des Admin-Bereichs (ohne Token-Hash, mit Laufzeitstatus).
type BotInfo struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	AllRooms      bool       `json:"allRooms"`
	Rooms         []string   `json:"rooms"`
	WebhookURL    string     `json:"webhookUrl"`
	WebhookSecret string     `json:"webhookSecret"`
	ReceiveBots   bool       `json:"receiveBots"`
	CanModerate   bool       `json:"canModerate"`
	Perms         BotPerms   `json:"perms"`
	Enabled       bool       `json:"enabled"`
	Created       time.Time  `json:"created"`
	Connected     int        `json:"connected"`              // offene Live-Verbindungen
	LastActive    *time.Time `json:"lastActive,omitempty"`   // letzter API-Zugriff
	LastDelivery  *Delivery  `json:"lastDelivery,omitempty"` // letzter Webhook-Aufruf
}

type botStatus struct {
	connected    int
	lastActive   time.Time
	lastDelivery *Delivery
}

var (
	ErrNoBot       = errors.New("Diesen Bot gibt es nicht")
	ErrBotWebhook  = errors.New("Webhook-URL muss mit http:// oder https:// beginnen")
	ErrBotRoom     = errors.New("Dieser Bot hat keinen Zugriff auf den Kanal")
	ErrBotNoRooms  = errors.New("Wähle mindestens einen Kanal oder „Alle Kanäle“")
	ErrBotDescLong = errors.New("Beschreibung darf höchstens 140 Zeichen lang sein")
)

type Bots struct {
	mu       sync.RWMutex
	byID     map[string]*Bot
	byToken  map[string]*Bot
	status   map[string]*botStatus
	users    *Users // für die Prüfung, ob ein Name schon von einer Person belegt ist
	onChange func()
}

func NewBots() *Bots {
	return &Bots{byID: map[string]*Bot{}, byToken: map[string]*Bot{}, status: map[string]*botStatus{}}
}

func (bs *Bots) changed() {
	if bs.onChange != nil {
		bs.onChange()
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// newBotToken: Bot-Schlüssel beginnen mit "gcb_", damit man sie leicht erkennt.
func newBotToken() string { return "gcb_" + randomHex(24) }

// validate prüft die Eingaben und liefert die bereinigten Werte.
func (bs *Bots) validate(in BotInput, selfID string) (BotInput, error) {
	name, ok := CleanName(in.Name)
	if !ok {
		return in, ErrInvalidName
	}
	in.Name = name
	desc := strings.Join(strings.Fields(in.Description), " ")
	if len([]rune(desc)) > maxDescLen {
		return in, ErrBotDescLong
	}
	in.Description = desc
	in.WebhookURL = strings.TrimSpace(in.WebhookURL)
	if in.WebhookURL != "" {
		u, err := url.Parse(in.WebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return in, ErrBotWebhook
		}
	}
	var rooms []string
	seen := map[string]bool{}
	for _, r := range in.Rooms {
		r = NormRoom(r)
		if !ValidRoom(r) {
			return in, ErrInvalidRoom
		}
		if !seen[r] {
			seen[r] = true
			rooms = append(rooms, r)
		}
	}
	sort.Strings(rooms)
	in.Rooms = rooms
	if in.AllRooms {
		in.Rooms = nil
	} else if len(in.Rooms) == 0 {
		return in, ErrBotNoRooms
	}
	if in.Perms != nil {
		p, err := validBotPerms(*in.Perms)
		if err != nil {
			return in, err
		}
		in.Perms = &p
	}
	// Name darf weder von einem anderen Bot noch von einer Person belegt sein.
	key := nameKey(name)
	for id, b := range bs.byID {
		if id != selfID && nameKey(b.Name) == key {
			return in, ErrNameTaken
		}
	}
	if bs.users != nil && bs.users.Taken(name) {
		if selfID == "" || nameKey(bs.byID[selfID].Name) != key {
			return in, ErrNameTaken
		}
	}
	return in, nil
}

// Create legt einen Bot an und liefert den API-Schlüssel (wird nur dieses eine Mal angezeigt).
func (bs *Bots) Create(in BotInput) (BotInfo, string, error) {
	bs.mu.Lock()
	in, err := bs.validate(in, "")
	if err != nil {
		bs.mu.Unlock()
		return BotInfo{}, "", err
	}
	token := newBotToken()
	perms := defaultBotPerms()
	if in.Perms != nil {
		perms = *in.Perms
	} else if in.CanModerate {
		perms.Delete, perms.Mute, perms.MaxMinutes = true, true, 24*60
	}
	b := &Bot{
		ID: "bot_" + randomHex(8), Name: in.Name, Description: in.Description,
		TokenHash: hashToken(token), AllRooms: in.AllRooms, Rooms: in.Rooms,
		WebhookURL: in.WebhookURL, WebhookSecret: randomHex(24),
		ReceiveBots: in.ReceiveBots, Perms: &perms, Enabled: in.Enabled == nil || *in.Enabled,
		Created: time.Now().UTC(),
	}
	bs.byID[b.ID] = b
	bs.byToken[b.TokenHash] = b
	info := bs.infoLocked(b)
	bs.mu.Unlock()
	bs.changed()
	return info, token, nil
}

// Update ändert einen Bot. renamed ist true, wenn sich der Name geändert hat.
func (bs *Bots) Update(id string, in BotInput) (info BotInfo, renamed bool, err error) {
	bs.mu.Lock()
	b, ok := bs.byID[id]
	if !ok {
		bs.mu.Unlock()
		return BotInfo{}, false, ErrNoBot
	}
	in, err = bs.validate(in, id)
	if err != nil {
		bs.mu.Unlock()
		return BotInfo{}, false, err
	}
	renamed = b.Name != in.Name
	b.Name, b.Description, b.AllRooms, b.Rooms = in.Name, in.Description, in.AllRooms, in.Rooms
	b.WebhookURL, b.ReceiveBots = in.WebhookURL, in.ReceiveBots
	perms := b.P()
	if in.Perms != nil {
		perms = *in.Perms
	} else {
		perms.Delete, perms.Mute = in.CanModerate, in.CanModerate
	}
	b.Perms, b.CanModerate = &perms, false
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	info = bs.infoLocked(b)
	bs.mu.Unlock()
	bs.changed()
	return info, renamed, nil
}

// Renew erzeugt neuen API-Schlüssel und neues Webhook-Geheimnis; die alten werden ungültig.
func (bs *Bots) Renew(id string) (token, secret string, err error) {
	bs.mu.Lock()
	b, ok := bs.byID[id]
	if !ok {
		bs.mu.Unlock()
		return "", "", ErrNoBot
	}
	delete(bs.byToken, b.TokenHash)
	token = newBotToken()
	b.TokenHash, b.WebhookSecret = hashToken(token), randomHex(24)
	bs.byToken[b.TokenHash] = b
	secret = b.WebhookSecret
	bs.mu.Unlock()
	bs.changed()
	return token, secret, nil
}

func (bs *Bots) Delete(id string) error {
	bs.mu.Lock()
	b, ok := bs.byID[id]
	if !ok {
		bs.mu.Unlock()
		return ErrNoBot
	}
	delete(bs.byID, id)
	delete(bs.byToken, b.TokenHash)
	delete(bs.status, id)
	bs.mu.Unlock()
	bs.changed()
	return nil
}

// Auth liefert den (aktiven) Bot zu einem API-Schlüssel.
func (bs *Bots) Auth(token string) (Bot, bool) {
	if !strings.HasPrefix(token, "gcb_") {
		return Bot{}, false
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	b, ok := bs.byToken[hashToken(token)]
	if !ok || !b.Enabled {
		return Bot{}, false
	}
	bs.statusLocked(b.ID).lastActive = time.Now().UTC()
	return bs.copy(b), true
}

// Get liefert den aktuellen Stand eines Bots (z. B. um geänderte Rechte zu übernehmen).
func (bs *Bots) Get(id string) (Bot, bool) {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	b, ok := bs.byID[id]
	if !ok {
		return Bot{}, false
	}
	return bs.copy(b), true
}

func (bs *Bots) copy(b *Bot) Bot {
	c := *b
	c.Rooms = append([]string(nil), b.Rooms...)
	p := b.P()
	c.Perms, c.CanModerate = &p, false
	return c
}

func (bs *Bots) List() []BotInfo {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	out := make([]BotInfo, 0, len(bs.byID))
	for _, b := range bs.byID {
		out = append(out, bs.infoLocked(b))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Webhooks liefert alle aktiven Bots mit Webhook-URL.
func (bs *Bots) Webhooks() []Bot {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	var out []Bot
	for _, b := range bs.byID {
		if b.Enabled && b.WebhookURL != "" {
			out = append(out, bs.copy(b))
		}
	}
	return out
}

func (bs *Bots) statusLocked(id string) *botStatus {
	s, ok := bs.status[id]
	if !ok {
		s = &botStatus{}
		bs.status[id] = s
	}
	return s
}

func (bs *Bots) infoLocked(b *Bot) BotInfo {
	info := BotInfo{
		ID: b.ID, Name: b.Name, Description: b.Description, AllRooms: b.AllRooms,
		Rooms: append([]string{}, b.Rooms...), WebhookURL: b.WebhookURL, WebhookSecret: b.WebhookSecret,
		ReceiveBots: b.ReceiveBots, CanModerate: b.P().Moderates(), Perms: b.P(), Enabled: b.Enabled, Created: b.Created,
	}
	if s, ok := bs.status[b.ID]; ok {
		info.Connected = s.connected
		if !s.lastActive.IsZero() {
			t := s.lastActive
			info.LastActive = &t
		}
		if s.lastDelivery != nil {
			d := *s.lastDelivery
			info.LastDelivery = &d
		}
	}
	return info
}

// connect zählt offene Live-Verbindungen (+1 / -1).
func (bs *Bots) connect(id string, delta int) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if _, ok := bs.byID[id]; ok {
		s := bs.statusLocked(id)
		s.connected += delta
		s.lastActive = time.Now().UTC()
	}
}

func (bs *Bots) recordDelivery(id string, d Delivery) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if _, ok := bs.byID[id]; ok {
		bs.statusLocked(id).lastDelivery = &d
	}
}

// HasNameKey: ist der (normalisierte) Name von einem Bot belegt?
func (bs *Bots) HasNameKey(key string) bool {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	for _, b := range bs.byID {
		if nameKey(b.Name) == key {
			return true
		}
	}
	return false
}

// renameRoom und removeRoom halten die Kanal-Listen der Bots aktuell.
func (bs *Bots) renameRoom(old, neu string) {
	bs.mu.Lock()
	changed := false
	for _, b := range bs.byID {
		for i, r := range b.Rooms {
			if r == old {
				b.Rooms[i] = neu
				sort.Strings(b.Rooms)
				changed = true
				break
			}
		}
	}
	bs.mu.Unlock()
	if changed {
		bs.changed()
	}
}

// addRoom gibt einem Bot einen weiteren Kanal (z. B. einen, den er selbst angelegt hat).
func (bs *Bots) addRoom(id, name string) {
	bs.mu.Lock()
	b, ok := bs.byID[id]
	if !ok || b.AllRooms || slices.Contains(b.Rooms, name) {
		bs.mu.Unlock()
		return
	}
	b.Rooms = append(b.Rooms, name)
	sort.Strings(b.Rooms)
	bs.mu.Unlock()
	bs.changed()
}

func (bs *Bots) removeRoom(name string) {
	bs.mu.Lock()
	changed := false
	for _, b := range bs.byID {
		for i, r := range b.Rooms {
			if r == name {
				b.Rooms = append(b.Rooms[:i:i], b.Rooms[i+1:]...)
				changed = true
				break
			}
		}
	}
	bs.mu.Unlock()
	if changed {
		bs.changed()
	}
}

func (bs *Bots) export() []Bot {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	out := make([]Bot, 0, len(bs.byID))
	for _, b := range bs.byID {
		out = append(out, bs.copy(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (bs *Bots) restore(list []Bot) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	for i := range list {
		b := list[i]
		if b.ID == "" || b.TokenHash == "" {
			continue
		}
		bs.byID[b.ID] = &b
		bs.byToken[b.TokenHash] = &b
	}
}
