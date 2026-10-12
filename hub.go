// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"ucng/internal/i18n"
	"unicode/utf8"
)

// Message ist eine einzelne Chat-Nachricht. Von Moderatoren gelöschte Nachrichten
// bleiben als Platzhalter im Verlauf (Deleted = true, Text leer). Der Originaltext
// wird in Original aufbewahrt und nur über die Admin-API herausgegeben – siehe public().
type Message struct {
	ID        int64      `json:"id"`
	Room      string     `json:"room"`
	UserID    string     `json:"userId,omitempty"`
	Bot       bool       `json:"bot,omitempty"`  // von einem Bot geschrieben
	Team      bool       `json:"team,omitempty"` // Person mit Team-Abzeichen
	Name      string     `json:"name"`
	Text      string     `json:"text"`
	Time      time.Time  `json:"time"`
	ReplyTo   int64      `json:"replyTo,omitempty"`   // Antwort auf diese Nachricht
	EditedAt  *time.Time `json:"editedAt,omitempty"`  // zuletzt bearbeitet
	Reactions []Reaction `json:"reactions,omitempty"` // 👍 ❤️ …
	Deleted   bool       `json:"deleted,omitempty"`
	// Wer ausgeblendet hat: "mod", "automod", "bot" oder "self" (selbst gelöscht).
	DeletedBy string `json:"deletedBy,omitempty"`

	// Bild an der Nachricht (GIF aus Bibliothek oder Web-Suche, hochgeladenes Bild)
	Media *Media `json:"media,omitempty"`
	// Umfrage
	Poll *Poll `json:"poll,omitempty"`
	// Antwort in einem Thread: ID der Ausgangsnachricht
	Thread int64 `json:"thread,omitempty"`
	// Nur in Ausgaben: Zusammenfassung des Threads an der Ausgangsnachricht
	ThreadInfo *ThreadSummary `json:"threadInfo,omitempty"`
	// Ende-zu-Ende-verschlüsselt (siehe e2e.go); Text ist dann nur ein Platzhalter
	E2E string `json:"e2e,omitempty"`
	// Unterhaltung mit einer anderen Instanz (siehe federation.go)
	Fed *FedMeta `json:"fed,omitempty"`

	// Nur in Ausgaben, nie gespeichert: Vorschau der Nachricht, auf die geantwortet wird.
	Reply *ReplyPreview `json:"reply,omitempty"`

	// Nur für Admins (werden in public() entfernt):
	Original   string     `json:"original,omitempty"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty"`
	Match      string     `json:"match,omitempty"`      // Automod-Muster, das angeschlagen hat
	Reviewed   bool       `json:"reviewed,omitempty"`   // von einem Admin geprüft
	ModBy      string     `json:"modBy,omitempty"`      // wer moderiert hat
	Edits      []string   `json:"edits,omitempty"`      // frühere Fassungen (bearbeitet)
	Unfiltered string     `json:"unfiltered,omitempty"` // Text vor dem Wortfilter
}

type Reaction struct {
	Emoji string      `json:"emoji"`
	Users []ReactUser `json:"users"`
}

type ReactUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ReplyPreview struct {
	ID      int64  `json:"id"`
	Name    string `json:"name,omitempty"`
	Text    string `json:"text,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Missing bool   `json:"missing,omitempty"` // nicht (mehr) vorhanden
	E2E     string `json:"e2e,omitempty"`     // verschlüsselt: der Browser entschlüsselt die Vorschau
}

// public liefert die Nachricht ohne die Moderationsdaten.
func (m Message) public() Message {
	m.Original, m.DeletedAt, m.Match, m.Reviewed, m.ModBy, m.Edits, m.Unfiltered = "", nil, "", false, "", nil, ""
	if m.Deleted {
		m.Media = nil // ausgeblendet: auch das Bild (Admins sehen es weiter)
		m.Poll = nil
		m.E2E = ""
	}
	return m
}

// AwaitingReview: von der Automoderation ausgeblendet und noch nicht geprüft.
func (m Message) AwaitingReview() bool { return m.Deleted && m.DeletedBy == "automod" && !m.Reviewed }

// QueueItem ist ein Eintrag der Automod-Prüfliste.
type QueueItem struct {
	Room    string  `json:"room"`
	Message Message `json:"message"`
}

// Event wird per Server-Sent Events an die Clients geschickt.
//
//	history       die neuesten Nachrichten beim Verbinden (ältere per API nachladen)
//	message       neue Nachricht
//	update        Nachricht geändert (moderiert, bearbeitet, Reaktion)
//	removed       Nachricht endgültig gelöscht (Event.ID)
//	presence      wer ist online (Users, Guests, Online)
//	room          Beschreibung oder Einstellungen des Kanals geändert
//	room_renamed  Kanal heißt jetzt Event.Room
//	room_deleted  Kanal wurde gelöscht
//	settings      Einstellungen geändert
//	user_renamed  Person Event.User heißt jetzt Event.Name (Verlauf neu laden)
//	user_updated  Nachrichten einer Person geändert, z. B. Team-Abzeichen (Verlauf neu laden)
//	sanction      Sperre der Person Event.User geändert (nur diese Person reagiert)
type Event struct {
	Type        string         `json:"type"`
	Message     *Message       `json:"message,omitempty"`
	Messages    []Message      `json:"messages,omitempty"`
	Online      int            `json:"online"`
	Room        string         `json:"room,omitempty"`
	Description *string        `json:"description,omitempty"`
	RoomInfo    *RoomInfo      `json:"roomInfo,omitempty"`
	Settings    *Settings      `json:"settings,omitempty"`
	User        string         `json:"user,omitempty"`
	Name        string         `json:"name,omitempty"`
	More        bool           `json:"more,omitempty"`   // bei "history": es gibt ältere Nachrichten
	Pinned      []Message      `json:"pinned,omitempty"` // bei "history": angepinnte Nachrichten
	ID          int64          `json:"id,omitempty"`
	Users       []PresenceUser `json:"users,omitempty"`
	Guests      int            `json:"guests,omitempty"`
}

// Viewer ist eine angemeldete Person, die einen Kanal geöffnet hat (für die Online-Liste).
type Viewer struct {
	ID      string
	Name    string
	Team    bool
	Roles   []string
	Session string // Konten: Kennung der Sitzung (endet die Sitzung, endet auch der Stream)
}

type PresenceUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Team bool   `json:"team,omitempty"`
}

const (
	LinksAll   = "all"   // jeder darf Links posten
	LinksKnown = "known" // erst, wenn der Name älter als 24 Stunden ist
	LinksTeam  = "team"  // nur das Team
	LinksNone  = "none"  // (nur für Bots) keine Links

	// Automoderation pro Kanal
	AutomodGlobal = "global" // nur die globalen Regeln (Standard)
	AutomodBoth   = "both"   // globale und eigene Regeln
	AutomodOwn    = "own"    // nur die eigenen Regeln des Kanals
	AutomodOff    = "off"    // keine Automoderation in diesem Kanal
)

type room struct {
	name        string
	description string
	created     time.Time
	slowMode    int                      // Sekunden zwischen zwei Nachrichten einer Person (0 = aus)
	links       string                   // LinksAll, LinksKnown, LinksTeam
	botSlow     int                      // eigener Langsam-Modus für Bots
	botLinks    string                   // LinksAll oder LinksNone
	amMode      string                   // AutomodGlobal, AutomodBoth, AutomodOwn, AutomodOff
	am          *Automod                 // eigene Regeln des Kanals (nil = keine)
	readRoles   []string                 // nur diese Rollen (und das Team) sehen den Kanal (leer = alle)
	writeRoles  []string                 // nur diese Rollen (und das Team) schreiben (leer = alle, die lesen)
	kind        string                   // "" oder RoomDM
	members     []string                 // Direktnachrichten: die beiden Nutzer-IDs
	pins        []int64                  // angepinnte Nachrichten (neueste zuletzt)
	group       string                   // Gruppe in der Seitenleiste (leer = ohne)
	pos         int                      // Position in der Kanalliste
	threads     map[int64]*ThreadSummary // Ausgangsnachricht → Zusammenfassung
	history     []Message                // nach ID aufsteigend sortiert, ohne Längenbegrenzung
	clients     map[chan Event]*Viewer
	last        time.Time
}

// find liefert die Position einer Nachricht (Binärsuche über die aufsteigenden IDs).
func (r *room) find(id int64) (int, bool) {
	i := sort.Search(len(r.history), func(i int) bool { return r.history[i].ID >= id })
	return i, i < len(r.history) && r.history[i].ID == id
}

// page liefert bis zu limit Nachrichten vor der Nachricht mit ID before (0 = die neuesten)
// und ob es davor noch ältere gibt.
func (r *room) page(before int64, limit int) ([]Message, bool) {
	end := len(r.history)
	if before > 0 {
		end = sort.Search(len(r.history), func(i int) bool { return r.history[i].ID >= before })
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return append([]Message(nil), r.history[start:end]...), start > 0
}

func (r *room) info() RoomInfo {
	ri := RoomInfo{
		Name: r.name, Description: r.description, Online: len(r.clients), Messages: len(r.history),
		LastAt: r.last, Created: r.created, SlowMode: r.slowMode, Links: r.links,
		BotSlowMode: r.botSlow, BotLinks: r.botLinks, Automod: r.amMode,
		ReadRoles: append([]string{}, r.readRoles...), WriteRoles: append([]string{}, r.writeRoles...), Kind: r.kind,
		Pins: len(r.pins), Group: r.group, Pos: r.pos,
	}
	if n := len(r.history); n > 0 {
		ri.LastID = r.history[n-1].ID
	}
	return ri
}

// RoomInfo ist die öffentliche Übersicht eines Raums.
type RoomInfo struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Online      int         `json:"online"`
	Messages    int         `json:"messages"`
	LastAt      time.Time   `json:"lastAt"`
	LastID      int64       `json:"lastId"`
	Created     time.Time   `json:"created"`
	SlowMode    int         `json:"slowMode"`
	Links       string      `json:"links"`
	BotSlowMode int         `json:"botSlowMode"`
	BotLinks    string      `json:"botLinks"`
	Automod     string      `json:"automod"`    // Automod-Modus des Kanals
	ReadRoles   []string    `json:"readRoles"`  // leer = alle
	WriteRoles  []string    `json:"writeRoles"` // leer = alle, die lesen dürfen
	Kind        string      `json:"kind,omitempty"`
	Pins        int         `json:"pins"`
	CanWrite    bool        `json:"canWrite"`          // nur in RoomsFor: darf die anfragende Person schreiben?
	Group       string      `json:"group,omitempty"`   // Gruppe (siehe layout.go)
	Pos         int         `json:"pos"`               // Position in der Liste
	Threads     []ThreadRef `json:"threads,omitempty"` // nur in RoomsFor: aktive Threads
}

// RoomInput sind die im Admin-Bereich änderbaren Werte eines Kanals.
type RoomInput struct {
	Name        *string  `json:"name"`
	Description string   `json:"description"`
	SlowMode    int      `json:"slowMode"`
	Links       string   `json:"links"`
	BotSlowMode int      `json:"botSlowMode"`
	BotLinks    string   `json:"botLinks"`
	ReadRoles   []string `json:"readRoles"`
	WriteRoles  []string `json:"writeRoles"`
}

// Seitengrößen beim Laden des Verlaufs.
const (
	DefaultPage = 100
	MaxPage     = 500
)

// storeOp beschreibt eine Änderung, die gespeichert werden muss (siehe store.go).
type storeOp struct {
	kind    opKind
	room    string
	newName string
	msg     Message
	doc     string
	audit   AuditEntry
}

type opKind int

const (
	opDoc          opKind = iota // eine Einstellungsdatei (config.json, automod.json …)
	opRoomMeta                   // Name/Beschreibung/Einstellungen eines Kanals
	opMessage                    // Nachricht neu oder geändert (wird angehängt)
	opRoomRename                 // Kanal umbenannt
	opRoomDelete                 // Kanal gelöscht
	opRoomRewrite                // Nachrichtendatei komplett neu schreiben (endgültig löschen, Aufbewahrungsfrist)
	opAudit                      // Protokolleintrag anhängen
	opAuditRewrite               // Protokoll neu schreiben (alte Einträge entfernt)
)

// docSpec beschreibt eine Datei im Datenordner, die als Ganzes gespeichert wird.
type docSpec struct {
	save func() any
	load func([]byte) error
}

// BusEvent geht an Bots (Live-Stream) und den Webhook-Versand – unabhängig von Kanälen.
type BusEvent struct {
	Type    string // "message" (neu) oder "update" (geändert, z. B. moderiert)
	Room    string
	Message *Message // öffentliche Fassung
}

// Hub verwaltet alle Räume, Verlauf, Einstellungen und verbundene Clients.
type Hub struct {
	mu        sync.Mutex
	rooms     map[string]*room
	settings  Settings
	nextID    atomic.Int64
	maxRooms  int // 0 = unbegrenzt
	automod   *Automod
	filter    *Automod         // Wortfilter (ersetzen statt ausblenden)
	pending   map[int64]string // ungeprüfte Automod-Treffer: Nachrichten-ID → Kanal
	bots      *Bots
	mod       *Moderation
	audit     *Audit
	accounts  *Accounts
	widgets   *Widgets
	media     *MediaLib
	commands  *Commands
	roles     *Roles
	reports   *Reports
	schedules *Schedules
	hosted    *Hosted
	groups    *Groups
	beacon    *BeaconStore
	i18n      *i18n.Catalog // Übersetzungen (i18n.go)
	fed       *FedStore
	bus       map[chan BusEvent]struct{}
	loc       atomic.Pointer[time.Location]

	docs  map[string]docSpec
	order []string // Lade-Reihenfolge der Dateien

	opsMu    sync.Mutex
	ops      []storeOp
	persist  atomic.Bool            // false = nichts protokollieren (Speicherung aus)
	onChange atomic.Pointer[func()] // stößt das Speichern an
}

// record merkt eine Änderung für die Speicherung vor.
func (h *Hub) record(op storeOp) {
	if !h.persist.Load() {
		return
	}
	h.opsMu.Lock()
	h.ops = append(h.ops, op)
	h.opsMu.Unlock()
	if f := h.onChange.Load(); f != nil {
		(*f)()
	}
}

func (h *Hub) touchDoc(name string) { h.record(storeOp{kind: opDoc, doc: name}) }

// takeOps übergibt alle vorgemerkten Änderungen an den Speicher.
func (h *Hub) takeOps() []storeOp {
	h.opsMu.Lock()
	defer h.opsMu.Unlock()
	ops := h.ops
	h.ops = nil
	return ops
}

// requeue stellt nicht gespeicherte Änderungen wieder vorne an (z. B. nach einem Schreibfehler).
func (h *Hub) requeue(ops []storeOp) {
	h.opsMu.Lock()
	h.ops = append(append([]storeOp(nil), ops...), h.ops...)
	h.opsMu.Unlock()
}

// Fed liefert Schlüssel und Warteschlange der Föderation.
func (h *Hub) Fed() *FedStore { return h.fed }

// Hosted liefert die Einstellungen des Modus „hosted“.
func (h *Hub) Hosted() *Hosted { return h.hosted }

// OnChange setzt die Funktion, die bei jeder Änderung aufgerufen wird.
func (h *Hub) OnChange(f func()) { h.onChange.Store(&f) }

// EnablePersistence schaltet das Protokollieren von Änderungen ein.
func (h *Hub) EnablePersistence() { h.persist.Store(true) }

// RegisterDoc meldet eine Einstellungsdatei an (Speichern und Laden).
func (h *Hub) RegisterDoc(name string, save func() any, load func([]byte) error) {
	h.docs[name] = docSpec{save: save, load: load}
	h.order = append(h.order, name)
}

var (
	roomNameRe = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{1,32}$`)

	ErrInvalidRoom  = errors.New("Ungültiger Kanalname: 1–32 Zeichen, nur Buchstaben, Ziffern, _ oder -")
	ErrInvalidName  = errors.New("Name muss 1–32 Zeichen lang sein")
	ErrInvalidText  = errors.New("Nachricht muss 1–2000 Zeichen lang sein")
	ErrInvalidDesc  = errors.New("Beschreibung darf höchstens 140 Zeichen lang sein")
	ErrInvalidTitle = errors.New("Titel muss 1–60 Zeichen lang sein")
	ErrTooMany      = errors.New("Maximale Anzahl an Kanälen erreicht")
	ErrNoRoom       = errors.New("Diesen Kanal gibt es nicht")
	ErrRoomExists   = errors.New("Einen Kanal mit diesem Namen gibt es bereits")
	ErrNoMessage    = errors.New("Nachricht nicht gefunden")
	ErrNoReplyMsg   = errors.New("Die Nachricht, auf die du antwortest, gibt es nicht mehr")
	ErrSlowMode     = errors.New("Langsam-Modus: 0 bis 3600 Sekunden")
	ErrLinksMode    = errors.New("Link-Regel muss all, known oder team sein")
	ErrBotLinksMode = errors.New("Link-Regel für Bots muss all oder none sein")
	ErrBotLinks     = errors.New("Bots dürfen in diesem Kanal keine Links posten")
	ErrAutomodMode  = errors.New("Automod-Modus muss global, both, own oder off sein")
)

const (
	maxNameLen  = 32
	maxTextLen  = 2000
	maxDescLen  = 140
	maxTitleLen = 60
)

// NewHub legt einen Hub an. maxRooms = 0 bedeutet: beliebig viele Kanäle.
func NewHub(maxRooms int) *Hub {
	h := &Hub{
		rooms: make(map[string]*room), settings: defaultSettings, maxRooms: maxRooms,
		automod: NewAutomod(), filter: NewAutomod(), pending: map[int64]string{}, bots: NewBots(), mod: NewModeration(),
		audit: NewAudit(), accounts: NewAccounts(""), widgets: NewWidgets(), media: NewMediaLib(), commands: NewCommands(), roles: NewRoles(), reports: NewReports(), schedules: NewSchedules(), hosted: NewHosted(), groups: NewGroups(), beacon: NewBeaconStore(), fed: NewFedStore(), i18n: newCatalog(),
		bus: map[chan BusEvent]struct{}{}, docs: map[string]docSpec{},
	}
	h.loc.Store(loadLocation(defaultSettings.Timezone))
	h.bots.onChange = func() { h.touchDoc("bots.json") }
	h.mod.changed = func() { h.touchDoc("moderation.json") }
	h.accounts.changed = func() { h.touchDoc("accounts.json") }
	h.widgets.changed = func() { h.touchDoc("widgets.json") }
	h.media.changed = h.touchDoc
	h.media.onList = func() {
		h.mu.Lock()
		h.broadcastAll(Event{Type: "media"})
		h.mu.Unlock()
	}
	h.commands.changed = func() { h.touchDoc("commands.json") }
	h.audit.record = func(e AuditEntry) { h.record(storeOp{kind: opAudit, audit: e}) }
	h.audit.rewrite = func() { h.record(storeOp{kind: opAuditRewrite}) }

	h.RegisterDoc("config.json", func() any { return h.Settings() }, func(b []byte) error {
		var s Settings
		if err := jsonUnmarshal(b, &s); err != nil {
			return err
		}
		s = s.normalize()
		h.mu.Lock()
		h.settings = s
		h.mu.Unlock()
		h.loc.Store(loadLocation(s.Timezone))
		return nil
	})
	h.RegisterDoc("automod.json", func() any { return h.automod.Config() }, func(b []byte) error {
		var c AutomodConfig
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		_, err := h.automod.Set(c)
		return err
	})
	h.RegisterDoc("bots.json", func() any { return h.bots.export() }, func(b []byte) error {
		var list []Bot
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.bots.restore(list)
		return nil
	})
	h.RegisterDoc("moderation.json", func() any { return h.mod.export() }, func(b []byte) error {
		var d moderationDoc
		if err := jsonUnmarshal(b, &d); err != nil {
			return err
		}
		return h.mod.restore(d)
	})
	h.RegisterDoc("accounts.json", func() any { return h.accounts.export() }, func(b []byte) error {
		var list []Account
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.accounts.restore(list)
		return nil
	})
	h.RegisterDoc("emotes.json", func() any { return h.media.exportEmotes() }, func(b []byte) error {
		var list []Emote
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.media.restore(list, nil, nil)
		return nil
	})
	h.RegisterDoc("gifs.json", func() any { return h.media.exportGIFs() }, func(b []byte) error {
		var list []LibGIF
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.media.restore(nil, list, nil)
		return nil
	})
	h.RegisterDoc("media.json", func() any { return h.media.Config() }, func(b []byte) error {
		var c MediaConfig
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		h.media.restore(nil, nil, &c)
		return nil
	})
	h.RegisterDoc("commands.json", func() any { return h.commands.Config() }, func(b []byte) error {
		var c CommandsConfig
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		return h.commands.restore(c)
	})
	h.roles.changed = func() {
		h.touchDoc("roles.json")
		h.pruneRoomRoles()
		h.Notify("roles")
	}
	h.reports.changed = func() { h.touchDoc("reports.json") }
	h.hosted.changed = func() { h.touchDoc("hosted.json") }
	h.beacon.changed = func() { h.touchDoc("beacon.json") }
	h.i18n.OnChange(func() { h.touchDoc("i18n.json") })
	h.RegisterDoc("i18n.json", func() any { return h.i18n.Export() }, func(b []byte) error {
		var st i18n.State
		if err := jsonUnmarshal(b, &st); err != nil {
			return err
		}
		h.i18n.Restore(st)
		return nil
	})
	h.media.beaconSubs = h.beacon.Subs
	h.RegisterDoc("beacon.json", func() any { return h.beacon.Get() }, func(b []byte) error {
		var c BeaconSettings
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		h.beacon.restore(c)
		return nil
	})
	h.RegisterDoc("groups.json", func() any { return h.groups.List() }, func(b []byte) error {
		var list []RoomGroup
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.groups.restore(list)
		return nil
	})
	h.fed.changed = func() { h.touchDoc(fedStatePath) }
	h.RegisterDoc(fedStatePath, func() any { return h.fed.state() }, func(b []byte) error {
		var st fedState
		if err := jsonUnmarshal(b, &st); err != nil {
			return err
		}
		return h.fed.restore(st)
	})
	h.RegisterDoc("hosted.json", func() any { return h.hosted.Config() }, func(b []byte) error {
		var c HostedConfig
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		return h.hosted.restore(c)
	})
	h.schedules.changed = func() { h.touchDoc("schedules.json") }
	h.RegisterDoc("schedules.json", func() any { return h.schedules.export() }, func(b []byte) error {
		var list []Schedule
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.schedules.restore(list)
		return nil
	})
	h.RegisterDoc("reports.json", func() any { return h.reports.export() }, func(b []byte) error {
		var list []Report
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.reports.restore(list)
		return nil
	})
	h.RegisterDoc("filter.json", func() any { return h.Filter() }, func(b []byte) error {
		var c FilterConfig
		if err := jsonUnmarshal(b, &c); err != nil {
			return err
		}
		_, err := h.filter.Set(AutomodConfig{Enabled: true, Patterns: c.Patterns})
		return err
	})
	h.RegisterDoc("roles.json", func() any { return h.roles.List() }, func(b []byte) error {
		var list []Role
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.roles.restore(list)
		return nil
	})
	h.RegisterDoc("widgets.json", func() any { return h.widgets.export() }, func(b []byte) error {
		var list []Widget
		if err := jsonUnmarshal(b, &list); err != nil {
			return err
		}
		h.widgets.restore(list)
		return nil
	})
	return h
}

// Zugriff auf die Teilbereiche.
func (h *Hub) Bots() *Bots              { return h.bots }
func (h *Hub) Mod() *Moderation         { return h.mod }
func (h *Hub) AuditLog() *Audit         { return h.audit }
func (h *Hub) Accounts() *Accounts      { return h.accounts }
func (h *Hub) Widgets() *Widgets        { return h.widgets }
func (h *Hub) Media() *MediaLib         { return h.media }
func (h *Hub) Commands() *Commands      { return h.commands }
func (h *Hub) Roles() *Roles            { return h.roles }
func (h *Hub) Reports() *Reports        { return h.reports }
func (h *Hub) Schedules() *Schedules    { return h.schedules }
func (h *Hub) Automod() *Automod        { return h.automod }
func (h *Hub) Location() *time.Location { return h.loc.Load() }

// Listen meldet einen Empfänger für alle Nachrichten-Ereignisse an (Bots, Webhooks).
func (h *Hub) Listen(buf int) chan BusEvent {
	c := make(chan BusEvent, buf)
	h.mu.Lock()
	h.bus[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) Unlisten(c chan BusEvent) {
	h.mu.Lock()
	delete(h.bus, c)
	h.mu.Unlock()
}

// emit verteilt ein Ereignis an alle Empfänger. h.mu muss gehalten werden.
func (h *Hub) emit(ev BusEvent) {
	if isDMName(ev.Room) { // Direktnachrichten gehen nie an Bots oder Webhooks
		return
	}
	for c := range h.bus {
		select {
		case c <- ev:
		default: // Empfänger zu langsam – Ereignis überspringen statt den Chat zu blockieren
		}
	}
}

// NormRoom vereinheitlicht Raumnamen: "Lobby" und "lobby" sind derselbe Raum.
func NormRoom(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func ValidRoom(name string) bool { return roomNameRe.MatchString(name) }

func cleanText(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
	if !utf8.ValidString(s) {
		return "", false
	}
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= max
}

func cleanDesc(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", nil
	}
	s, ok := cleanText(s, maxDescLen)
	if !ok {
		return "", ErrInvalidDesc
	}
	return s, nil
}

func clampPage(limit int) int {
	if limit <= 0 {
		return DefaultPage
	}
	if limit > MaxPage {
		return MaxPage
	}
	return limit
}

func validRoomSettings(slow int, links string) (string, error) {
	if slow < 0 || slow > 3600 {
		return "", ErrSlowMode
	}
	switch links {
	case "":
		return LinksAll, nil
	case LinksAll, LinksKnown, LinksTeam:
		return links, nil
	}
	return "", ErrLinksMode
}

func validBotRoomSettings(slow int, links string) (string, error) {
	if slow < 0 || slow > 3600 {
		return "", ErrSlowMode
	}
	switch links {
	case "":
		return LinksAll, nil
	case LinksAll, LinksNone:
		return links, nil
	}
	return "", ErrBotLinksMode
}

// RoomAutomod sind die Automod-Einstellungen eines Kanals.
type RoomAutomod struct {
	Mode     string   `json:"mode"`
	Patterns []string `json:"patterns"`
}

// automodFor liefert Modus und eigene Regeln eines Kanals.
func (h *Hub) automodFor(roomName string) (string, *Automod) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[roomName]; ok && r.amMode != "" {
		return r.amMode, r.am
	}
	return AutomodGlobal, nil
}

// checkAutomod prüft einen Text nach den Regeln, die im Kanal gelten.
// Läuft außerhalb von h.mu (lange Wortlisten können etwas dauern).
func (h *Hub) checkAutomod(roomName, text string) (string, bool) {
	mode, own := h.automodFor(roomName)
	if mode == AutomodGlobal || mode == AutomodBoth {
		if p, hit := h.automod.Check(text); hit {
			return p, true
		}
	}
	if (mode == AutomodBoth || mode == AutomodOwn) && own != nil {
		if p, hit := own.Check(text); hit {
			return p + " (#" + roomName + ")", true
		}
	}
	return "", false
}

// TestAutomod prüft wie checkAutomod, aber unabhängig vom globalen Schalter (Testfeld).
// roomName = "" testet nur die globalen Regeln.
func (h *Hub) TestAutomod(roomName, text string) (pattern, normalized string, ok bool) {
	mode, own := AutomodGlobal, (*Automod)(nil)
	if roomName != "" {
		mode, own = h.automodFor(roomName)
	}
	if mode == AutomodGlobal || mode == AutomodBoth {
		if pattern, normalized, ok = h.automod.Test(text); ok {
			return
		}
	}
	if (mode == AutomodBoth || mode == AutomodOwn) && own != nil {
		var p string
		if p, normalized, ok = own.Test(text); ok {
			return p + " (#" + roomName + ")", normalized, true
		}
	}
	if normalized == "" {
		_, normalized, _ = h.automod.Test(text)
	}
	return "", normalized, false
}

// RoomAutomodConfig liefert die Automod-Einstellungen eines Kanals.
func (h *Hub) RoomAutomodConfig(roomName string) (RoomAutomod, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return RoomAutomod{}, ErrNoRoom
	}
	return r.automodLocked(), nil
}

func (r *room) automodLocked() RoomAutomod {
	out := RoomAutomod{Mode: r.amMode, Patterns: []string{}}
	if out.Mode == "" {
		out.Mode = AutomodGlobal
	}
	if r.am != nil {
		out.Patterns = r.am.Config().Patterns
	}
	return out
}

func compileRoomAutomod(in RoomAutomod) (string, *Automod, error) {
	switch in.Mode {
	case "":
		in.Mode = AutomodGlobal
	case AutomodGlobal, AutomodBoth, AutomodOwn, AutomodOff:
	default:
		return "", nil, ErrAutomodMode
	}
	am := NewAutomod()
	if _, err := am.Set(AutomodConfig{Enabled: true, Patterns: in.Patterns}); err != nil {
		return "", nil, err
	}
	if len(am.Config().Patterns) == 0 {
		am = nil
	}
	return in.Mode, am, nil
}

// SetRoomAutomod setzt Modus und eigene Regeln eines Kanals.
func (h *Hub) SetRoomAutomod(roomName string, in RoomAutomod) (RoomAutomod, error) {
	mode, am, err := compileRoomAutomod(in)
	if err != nil {
		return RoomAutomod{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return RoomAutomod{}, ErrNoRoom
	}
	r.amMode, r.am = mode, am
	h.record(storeOp{kind: opRoomMeta, room: roomName})
	info := r.info()
	h.broadcast(r, Event{Type: "room", RoomInfo: &info, Online: len(r.clients)})
	return r.automodLocked(), nil
}

// newRoomLocked legt einen Raum an. h.mu muss gehalten werden.
func (h *Hub) newRoomLocked(name, desc string) (*room, error) {
	if h.maxRooms > 0 && len(h.rooms) >= h.maxRooms {
		return nil, ErrTooMany
	}
	r := &room{name: name, description: desc, created: time.Now().UTC(), links: LinksAll, botLinks: LinksAll,
		amMode: AutomodGlobal, clients: make(map[chan Event]*Viewer), pos: h.nextPosLocked()}
	h.rooms[name] = r
	h.record(storeOp{kind: opRoomMeta, room: name})
	return r, nil
}

// userRoomLocked liefert einen Raum für normale Nutzer. Existiert er nicht, wird er
// nur angelegt, wenn Nutzer Räume erstellen dürfen. h.mu muss gehalten werden.
func (h *Hub) userRoomLocked(name string) (*room, error) {
	if !ValidRoom(name) {
		return nil, ErrInvalidRoom
	}
	if r, ok := h.rooms[name]; ok {
		return r, nil
	}
	if !h.settings.AllowUserRooms || isDMName(name) {
		return nil, ErrNoRoom
	}
	return h.newRoomLocked(name, "")
}

// broadcast schickt ein Event an alle Clients eines Raums. h.mu muss gehalten werden.
func (h *Hub) broadcast(r *room, ev Event) {
	for c := range r.clients {
		select {
		case c <- ev:
		default: // langsamer Client: Event überspringen statt den Hub zu blockieren
		}
	}
}

// Notify schickt ein einfaches Ereignis (z. B. "roles", "profiles") an alle Kanäle.
func (h *Hub) Notify(typ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastAll(Event{Type: typ})
}

// broadcastAll: an alle Kanäle. h.mu muss gehalten werden.
func (h *Hub) broadcastAll(ev Event) {
	for _, r := range h.rooms {
		e := ev
		e.Online = len(r.clients)
		h.broadcast(r, e)
	}
}

// closeClientsLocked schickt ein letztes Event und beendet alle Streams des Raums.
func (h *Hub) closeClientsLocked(r *room, ev Event) {
	h.broadcast(r, ev)
	for c := range r.clients {
		close(c)
	}
	r.clients = make(map[chan Event]*Viewer)
}

// out bereitet eine Nachricht für die Ausgabe auf: ohne Moderationsdaten (außer für Admins)
// und mit Vorschau der beantworteten Nachricht. h.mu muss gehalten werden.
func (h *Hub) out(r *room, m Message, admin bool) Message {
	if !admin {
		m = m.public()
	}
	m.Reply, m.ThreadInfo = nil, nil
	if m.Thread == 0 {
		if t := r.threads[m.ID]; t != nil {
			c := *t
			c.Names = append([]string(nil), t.Names...)
			m.ThreadInfo = &c
		}
	}
	if m.ReplyTo > 0 {
		p := &ReplyPreview{ID: m.ReplyTo}
		if i, ok := r.find(m.ReplyTo); ok {
			t := r.history[i]
			p.Name = t.Name
			if t.Deleted {
				p.Deleted = true
			} else if t.E2E != "" {
				p.E2E = t.E2E
			} else {
				p.Text = snippet(t.Text, 120)
				if p.Text == "" && t.Media != nil {
					p.Text = "GIF"
					if t.Media.Kind == "image" {
						p.Text = "Bild"
					}
					if t.Media.Title != "" {
						p.Text += ": " + snippet(t.Media.Title, 80)
					}
				}
				if p.Text == "" && t.Poll != nil {
					p.Text = "Umfrage: " + snippet(t.Poll.Question, 100)
				}
			}
		} else {
			p.Missing = true
		}
		m.Reply = p
	}
	return m
}

func (h *Hub) outAll(r *room, msgs []Message, admin bool) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = h.out(r, m, admin)
	}
	return out
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// presenceLocked baut das Online-Ereignis eines Kanals. h.mu muss gehalten werden.
func presenceLocked(r *room) Event {
	seen := map[string]bool{}
	ev := Event{Type: "presence", Online: len(r.clients), Users: []PresenceUser{}}
	for _, v := range r.clients {
		if v == nil {
			ev.Guests++
			continue
		}
		if !seen[v.ID] {
			seen[v.ID] = true
			ev.Users = append(ev.Users, PresenceUser{ID: v.ID, Name: v.Name, Team: v.Team})
		}
	}
	sort.Slice(ev.Users, func(i, j int) bool {
		if ev.Users[i].Team != ev.Users[j].Team {
			return ev.Users[i].Team
		}
		return strings.ToLower(ev.Users[i].Name) < strings.ToLower(ev.Users[j].Name)
	})
	return ev
}

// --- Nutzer-Aktionen ---

// Poster beschreibt, wer schreibt.
type Poster struct {
	ID     string
	Name   string
	Bot    bool
	Team   bool
	Since  time.Time // seit wann der Name reserviert ist (Link-Regel)
	IPHash string
	// Nur für Bots: von der Automoderation ausgenommen
	NoAutomod bool
	// Der eingebaute Befehls-Bot: keine Kanal-Regeln
	System bool
	Roles  []string // Rollen der Person (Zugriff auf Kanäle)
	Perms  Perms    // Rechte aus Rollen (Team = alle)
}

// Post speichert eine Nachricht ohne Nutzer-ID (z. B. für Tests oder Systemnachrichten).
func (h *Hub) Post(roomName, name, text string) (Message, error) {
	return h.PostAs(roomName, "", name, text)
}

// PostAs speichert eine Nachricht einer Person (ohne Zusatzangaben).
func (h *Hub) PostAs(roomName, userID, name, text string) (Message, error) {
	return h.Send(roomName, Poster{ID: userID, Name: name}, text, 0)
}

// PostBot schreibt als Bot. Der Kanal muss existieren und für den Bot freigegeben sein.
func (h *Hub) PostBot(roomName string, b Bot, text string) (Message, error) {
	if !b.Allowed(roomName) {
		return Message{}, ErrBotRoom
	}
	return h.Send(roomName, Poster{ID: b.ID, Name: b.Name, Bot: true, NoAutomod: b.P().AutomodExempt}, text, 0)
}

// checkRoomRules wendet Link-Regel und Langsam-Modus des Kanals an – für Personen und Bots
// getrennt eingestellt; das Team ist ausgenommen. h.mu muss gehalten werden.
func (h *Hub) checkRoomRules(r *room, p Poster, text string, edit bool) error {
	if (p.Team && !p.Bot) || p.System {
		return nil
	}
	hasLink := linkRe.MatchString(text)
	if p.Bot {
		if r.botLinks == LinksNone && hasLink {
			return ErrBotLinks
		}
		if edit {
			return nil
		}
		return h.mod.CheckSpam(p.ID, r.name, r.botSlow, text, false)
	}
	if r.links != LinksAll && hasLink && !p.Perms.Links {
		if r.links == LinksTeam {
			return ErrLinksTeam
		}
		if p.Since.IsZero() || time.Since(p.Since) < 24*time.Hour {
			return ErrLinksKnown
		}
	}
	if edit || p.Perms.NoSlow {
		return nil
	}
	return h.mod.CheckSpam(p.ID, r.name, r.slowMode, text, true)
}

var linkRe = regexp.MustCompile(`(?i)(https?://|www\.)\S`)

// checkSanction liefert einen Fehler, wenn die Person stummgeschaltet oder gesperrt ist.
func (h *Hub) checkSanction(p Poster) error {
	if p.Bot {
		return nil
	}
	ip := p.IPHash
	if p.Team {
		ip = "" // Team-Mitglieder hinter derselben Verbindung nicht mitsperren
	}
	if s, ok := h.mod.Active(p.ID, ip); ok {
		return &SanctionError{Kind: s.Kind, Until: s.Until, Reason: s.Reason, loc: h.Location()}
	}
	return nil
}

// Send prüft und speichert eine Nachricht und verteilt sie an alle Clients im Raum.
func (h *Hub) Send(roomName string, p Poster, text string, replyTo int64) (Message, error) {
	return h.SendMedia(roomName, p, text, replyTo, nil)
}

// SendMedia wie Send, optional mit Bild (dann darf der Text leer sein).
func (h *Hub) SendMedia(roomName string, p Poster, text string, replyTo int64, media *Media) (Message, error) {
	return h.SendFull(roomName, p, SendInput{Text: text, ReplyTo: replyTo, Media: media})
}

// SendInput: alles, was zu einer neuen Nachricht gehört.
type SendInput struct {
	Text    string
	ReplyTo int64
	Media   *Media
	Poll    *Poll
	Thread  int64
	E2E     string   // verschlüsselte Nachricht (Text wird ignoriert)
	Fed     *FedMeta // Föderation
}

// SendFull prüft und speichert eine Nachricht (Text, Bild, Umfrage, Thread).
func (h *Hub) SendFull(roomName string, p Poster, in SendInput) (Message, error) {
	text, replyTo, media := in.Text, in.ReplyTo, in.Media
	name, ok := cleanText(strings.ReplaceAll(p.Name, "\n", " "), maxNameLen)
	if !ok {
		return Message{}, ErrInvalidName
	}
	if in.E2E != "" {
		if err := validE2E(in.E2E); err != nil {
			return Message{}, err
		}
		if media != nil || in.Poll != nil || in.Thread != 0 {
			return Message{}, ErrE2EMedia
		}
		text = e2ePlaceholder
		p.NoAutomod = true
	}
	if (media != nil || in.Poll != nil) && strings.TrimSpace(text) == "" {
		text = ""
	} else if text, ok = cleanText(text, maxTextLen); !ok {
		return Message{}, ErrInvalidText
	}
	if err := h.checkSanction(p); err != nil {
		return Message{}, err
	}
	var pattern string
	var hit bool
	amText := text
	if in.Poll != nil {
		amText = strings.TrimSpace(text + "\n" + in.Poll.text())
	}
	if !p.NoAutomod && amText != "" {
		pattern, hit = h.checkAutomod(roomName, amText) // außerhalb der Sperre – kann bei langen Listen etwas dauern
	}
	unfiltered := ""
	if !hit && !p.Bot && !p.System && in.E2E == "" {
		if t, changed := h.applyFilter(text); changed {
			unfiltered, text = text, t
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	var r *room
	var err error
	if p.Bot { // Bots legen keine Kanäle an
		var ok bool
		if r, ok = h.rooms[roomName]; !ok {
			err = ErrNoRoom
		}
	} else {
		r, err = h.userRoomLocked(roomName)
	}
	if err != nil {
		return Message{}, err
	}
	if !p.Bot && !p.System {
		if !r.canRead(accessOf(p)) {
			return Message{}, ErrNoRoom
		}
		if !r.canWrite(accessOf(p)) {
			return Message{}, ErrReadOnly
		}
	}
	if replyTo > 0 {
		if _, ok := r.find(replyTo); !ok {
			return Message{}, ErrNoReplyMsg
		}
	}
	var root *Message
	if in.Thread > 0 {
		i, ok := r.find(in.Thread)
		if !ok {
			return Message{}, ErrNoReplyMsg
		}
		root = &r.history[i]
		if root.Thread != 0 {
			return Message{}, ErrThreadNested
		}
	}
	if err := h.checkRoomRules(r, p, text, false); err != nil {
		return Message{}, err
	}
	msg := Message{ID: h.nextID.Add(1), Room: roomName, UserID: p.ID, Bot: p.Bot, Team: p.Team && !p.Bot,
		Name: name, Text: text, Time: time.Now().UTC(), ReplyTo: replyTo, Media: media, Unfiltered: unfiltered,
		Poll: in.Poll, Thread: in.Thread, E2E: in.E2E, Fed: in.Fed}
	if hit {
		// Sofort ausblenden; das Original sieht nur der Admin-Bereich.
		t := msg.Time
		msg.Original, msg.Text, msg.Deleted, msg.DeletedBy, msg.DeletedAt, msg.Match = amText, "", true, "automod", &t, pattern
		msg.Poll = nil
		h.pending[msg.ID] = roomName
	}
	rootID := in.Thread
	r.history = append(r.history, msg) // Achtung: root zeigt danach evtl. auf alten Speicher
	r.last = msg.Time
	if rootID > 0 {
		r.addToThread(msg)
	}
	h.record(storeOp{kind: opMessage, room: roomName, msg: msg})
	pub := h.out(r, msg, false) // Original und Automod-Muster bleiben im Admin-Bereich
	h.broadcast(r, Event{Type: "message", Message: &pub, Online: len(r.clients)})
	h.emit(BusEvent{Type: "message", Room: roomName, Message: &pub})
	if rootID > 0 { // Zähler an der Ausgangsnachricht aktualisieren
		if i, ok := r.find(rootID); ok {
			rp := h.out(r, r.history[i], false)
			h.broadcast(r, Event{Type: "update", Message: &rp, Online: len(r.clients)})
		}
	}
	return pub, nil
}

// Subscribe registriert einen Client und liefert die neuesten Nachrichten, ob es ältere
// gibt, und die Kanal-Infos. viewer = nil für Gäste ohne Namen.
func (h *Hub) Subscribe(roomName string, viewer *Viewer) (c chan Event, hist []Message, more bool, info RoomInfo, err error) {
	return h.subscribe(roomName, viewer, false)
}

// SubscribeAdmin: Live-Stream für die Moderation (auch private Kanäle und Direktnachrichten).
func (h *Hub) SubscribeAdmin(roomName string) (c chan Event, hist []Message, more bool, info RoomInfo, err error) {
	return h.subscribe(roomName, nil, true)
}

func (h *Hub) subscribe(roomName string, viewer *Viewer, admin bool) (c chan Event, hist []Message, more bool, info RoomInfo, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var r *room
	if admin {
		var ok bool
		if r, ok = h.rooms[roomName]; !ok {
			return nil, nil, false, RoomInfo{}, ErrNoRoom
		}
	} else if r, err = h.userRoomLocked(roomName); err != nil {
		return nil, nil, false, RoomInfo{}, err
	}
	if !admin && !r.canRead(viewer.access()) {
		return nil, nil, false, RoomInfo{}, ErrNoRoom
	}
	c = make(chan Event, 64)
	r.clients[c] = viewer
	hist, more = r.page(0, DefaultPage)
	h.broadcast(r, presenceLocked(r))
	info = r.info()
	info.CanWrite = r.canWrite(viewer.access())
	return c, h.outAll(r, hist, false), more, info, nil
}

func (h *Hub) Unsubscribe(roomName string, c chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return
	}
	if _, ok := r.clients[c]; !ok {
		return // Stream wurde bereits beim Löschen/Umbenennen beendet
	}
	delete(r.clients, c)
	h.broadcast(r, presenceLocked(r))
}

// Presence liefert die Online-Liste eines Kanals.
func (h *Hub) Presence(roomName string) (Event, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return Event{}, ErrNoRoom
	}
	return presenceLocked(r), nil
}

// History liefert eine Seite des öffentlichen Verlaufs: bis zu limit Nachrichten vor
// der Nachricht mit ID before (before = 0: die neuesten) und ob es ältere gibt.
func (h *Hub) History(roomName string, before int64, limit int) ([]Message, bool, error) {
	if !ValidRoom(roomName) {
		return nil, false, ErrInvalidRoom
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		if !h.settings.AllowUserRooms {
			return nil, false, ErrNoRoom
		}
		return []Message{}, false, nil
	}
	msgs, more := r.page(before, clampPage(limit))
	return h.outAll(r, msgs, false), more, nil
}

// AdminHistory wie History, aber inklusive Originaltexten gelöschter Nachrichten.
func (h *Hub) AdminHistory(roomName string, before int64, limit int) ([]Message, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return nil, false, ErrNoRoom
	}
	msgs, more := r.page(before, clampPage(limit))
	return h.outAll(r, msgs, true), more, nil
}

func (h *Hub) Rooms() []RoomInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RoomInfo, 0, len(h.rooms))
	for _, r := range h.rooms {
		if r.kind == "" {
			ri := r.info()
			ri.Group = h.groupOf(r)
			out = append(out, ri)
		}
	}
	sortRooms(out)
	return out
}

// I18n liefert die Übersetzungen.
func (h *Hub) I18n() *i18n.Catalog { return h.i18n }

// Beacon liefert Verzeichnis-Anmeldung und GIF-Abos.
func (h *Hub) Beacon() *BeaconStore { return h.beacon }

// Groups liefert die Kanal-Gruppen.
func (h *Hub) Groups() *Groups { return h.groups }

func (h *Hub) Settings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

// EnsureDefaultRoom legt "lobby" an, wenn es noch gar keinen Raum gibt.
func (h *Hub) EnsureDefaultRoom() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms) == 0 {
		h.newRoomLocked("lobby", "")
	}
}

// RenameUser schreibt den neuen Namen in alle bisherigen Nachrichten (und Reaktionen) einer
// Person und informiert die betroffenen Räume.
func (h *Hub) RenameUser(userID, newName string) {
	if userID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, r := range h.rooms {
		changed := false
		for i := range r.history {
			m := &r.history[i]
			touched := false
			if m.UserID == userID && m.Name != newName {
				m.Name = newName
				touched = true
			}
			if renameInReactions(m, userID, newName) {
				touched = true
			}
			if touched {
				h.record(storeOp{kind: opMessage, room: name, msg: *m})
				changed = true
			}
		}
		seen := false
		for c, v := range r.clients {
			if v != nil && v.ID == userID {
				r.clients[c] = &Viewer{ID: v.ID, Name: newName, Team: v.Team, Roles: v.Roles}
				seen = true
			}
		}
		if seen {
			h.broadcast(r, presenceLocked(r))
		}
		if changed {
			h.broadcast(r, Event{Type: "user_renamed", User: userID, Name: newName, Online: len(r.clients)})
		}
	}
}

func renameInReactions(m *Message, userID, name string) bool {
	changed := false
	var out []Reaction
	for _, re := range m.Reactions {
		users := make([]ReactUser, len(re.Users))
		for i, u := range re.Users {
			if u.ID == userID && u.Name != name {
				u.Name = name
				changed = true
			}
			users[i] = u
		}
		out = append(out, Reaction{Emoji: re.Emoji, Users: users})
	}
	if changed {
		m.Reactions = out
	}
	return changed
}

// SetUserTeam setzt oder entfernt das Team-Abzeichen an allen Nachrichten einer Person.
func (h *Hub) SetUserTeam(userID string, team bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, r := range h.rooms {
		for i := range r.history {
			if m := &r.history[i]; m.UserID == userID && !m.Bot && m.Team != team {
				m.Team = team
				h.record(storeOp{kind: opMessage, room: name, msg: *m})
			}
		}
		seen := false
		for c, v := range r.clients {
			if v != nil && v.ID == userID {
				r.clients[c] = &Viewer{ID: v.ID, Name: v.Name, Team: team, Roles: v.Roles}
				seen = true
			}
		}
		if seen {
			h.broadcast(r, presenceLocked(r))
		}
	}
	h.broadcastAll(Event{Type: "user_updated", User: userID})
}

// NotifySanction informiert die betroffene Person (alle anderen ignorieren das Ereignis).
func (h *Hub) NotifySanction(userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastAll(Event{Type: "sanction", User: userID})
}

// --- Admin-Aktionen ---

func (h *Hub) SetSettings(s Settings) (Settings, error) {
	s, err := validateSettings(s)
	if err != nil {
		return Settings{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settings = s
	h.loc.Store(loadLocation(s.Timezone))
	h.touchDoc("config.json")
	for _, r := range h.rooms {
		cp := s
		h.broadcast(r, Event{Type: "settings", Settings: &cp, Online: len(r.clients)})
	}
	return s, nil
}

func (h *Hub) CreateRoom(name, desc string) (RoomInfo, error) {
	return h.CreateRoomWith(RoomInput{Name: &name, Description: desc})
}

func (h *Hub) CreateRoomWith(in RoomInput) (RoomInfo, error) {
	if in.Name == nil {
		return RoomInfo{}, ErrInvalidRoom
	}
	name := NormRoom(*in.Name)
	if !ValidRoom(name) {
		return RoomInfo{}, ErrInvalidRoom
	}
	desc, err := cleanDesc(in.Description)
	if err != nil {
		return RoomInfo{}, err
	}
	links, err := validRoomSettings(in.SlowMode, in.Links)
	if err != nil {
		return RoomInfo{}, err
	}
	botLinks, err := validBotRoomSettings(in.BotSlowMode, in.BotLinks)
	if err != nil {
		return RoomInfo{}, err
	}
	if isDMName(name) {
		return RoomInfo{}, ErrDMName
	}
	readR, writeR := h.validRoleList(in.ReadRoles), h.validRoleList(in.WriteRoles)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.rooms[name]; ok {
		return RoomInfo{}, ErrRoomExists
	}
	r, err := h.newRoomLocked(name, desc)
	if err != nil {
		return RoomInfo{}, err
	}
	r.slowMode, r.links, r.botSlow, r.botLinks = in.SlowMode, links, in.BotSlowMode, botLinks
	r.readRoles, r.writeRoles = readR, writeR
	return r.info(), nil
}

// UpdateRoom ändert Beschreibung und/oder Namen (ältere Schnittstelle).
func (h *Hub) UpdateRoom(name, newName, desc string) (RoomInfo, error) {
	h.mu.Lock()
	r, ok := h.rooms[name]
	in := RoomInput{Name: &newName, Description: desc}
	if ok {
		in.SlowMode, in.Links, in.BotSlowMode, in.BotLinks = r.slowMode, r.links, r.botSlow, r.botLinks
		in.ReadRoles, in.WriteRoles = r.readRoles, r.writeRoles
	}
	h.mu.Unlock()
	return h.UpdateRoomWith(name, in)
}

// UpdateRoomWith ändert Name, Beschreibung, Langsam-Modus und Link-Regel. Beim Umbenennen
// werden verbundene Clients informiert und ihr Stream beendet, damit sie neu verbinden.
func (h *Hub) UpdateRoomWith(name string, in RoomInput) (RoomInfo, error) {
	newName := name
	if in.Name != nil {
		newName = NormRoom(*in.Name)
	}
	if !ValidRoom(newName) {
		return RoomInfo{}, ErrInvalidRoom
	}
	if newName != name && isDMName(newName) {
		return RoomInfo{}, ErrDMName
	}
	readR, writeR := h.validRoleList(in.ReadRoles), h.validRoleList(in.WriteRoles)
	desc, err := cleanDesc(in.Description)
	if err != nil {
		return RoomInfo{}, err
	}
	links, err := validRoomSettings(in.SlowMode, in.Links)
	if err != nil {
		return RoomInfo{}, err
	}
	botLinks, err := validBotRoomSettings(in.BotSlowMode, in.BotLinks)
	if err != nil {
		return RoomInfo{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok {
		return RoomInfo{}, ErrNoRoom
	}
	if r.kind == RoomDM {
		return RoomInfo{}, ErrNoRoom
	}
	if newName != name {
		if _, exists := h.rooms[newName]; exists {
			return RoomInfo{}, ErrRoomExists
		}
	}
	r.slowMode, r.links, r.botSlow, r.botLinks = in.SlowMode, links, in.BotSlowMode, botLinks
	r.readRoles, r.writeRoles = readR, writeR
	h.kickUnreadableLocked(r)
	if newName != name {
		delete(h.rooms, name)
		r.name = newName
		for i := range r.history {
			r.history[i].Room = newName
		}
		for id, rn := range h.pending {
			if rn == name {
				h.pending[id] = newName
			}
		}
		h.rooms[newName] = r
		r.description = desc
		h.record(storeOp{kind: opRoomRename, room: name, newName: newName})
		h.bots.renameRoom(name, newName)
		h.widgets.renameRoom(name, newName)
		h.commands.renameRoom(name, newName)
		h.reports.renameRoom(name, newName)
		h.schedules.renameRoom(name, newName)
		h.closeClientsLocked(r, Event{Type: "room_renamed", Room: newName})
	} else {
		r.description = desc
		h.record(storeOp{kind: opRoomMeta, room: name})
		d, info := desc, r.info()
		h.broadcast(r, Event{Type: "room", Description: &d, RoomInfo: &info, Online: len(r.clients)})
	}
	return r.info(), nil
}

func (h *Hub) DeleteRoom(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok {
		return ErrNoRoom
	}
	h.closeClientsLocked(r, Event{Type: "room_deleted", Room: name})
	delete(h.rooms, name)
	for id, rn := range h.pending {
		if rn == name {
			delete(h.pending, id)
		}
	}
	h.record(storeOp{kind: opRoomDelete, room: name})
	h.bots.removeRoom(name)
	return nil
}

// DeleteMessage ersetzt eine Nachricht für alle Leser durch den Hinweis
// „von einem Moderator gelöscht“. Der Originaltext bleibt für Admins erhalten.
func (h *Hub) DeleteMessage(roomName string, id int64) (Message, error) {
	return h.ModDelete(roomName, id, "mod", "")
}

// ModDelete löscht moderativ; by ist "mod" oder "bot", who der Name für das Protokoll.
func (h *Hub) ModDelete(roomName string, id int64, by, who string) (Message, error) {
	return h.moderate(roomName, id, func(m *Message) bool {
		if m.Deleted {
			return false
		}
		now := time.Now().UTC()
		m.Original, m.Text, m.Deleted, m.DeletedAt = m.Text, "", true, &now
		m.DeletedBy, m.Reviewed, m.ModBy = by, true, who
		return true
	})
}

// ConfirmMessage bestätigt eine Automod-Ausblendung („ausgeblendet lassen“).
func (h *Hub) ConfirmMessage(roomName string, id int64) (Message, error) {
	return h.moderate(roomName, id, func(m *Message) bool {
		if !m.Deleted || m.Reviewed {
			return false
		}
		m.Reviewed = true
		return true
	})
}

// RestoreMessage macht eine moderative Löschung rückgängig.
func (h *Hub) RestoreMessage(roomName string, id int64) (Message, error) {
	return h.moderate(roomName, id, func(m *Message) bool {
		if !m.Deleted || m.DeletedBy == "self" || (m.Original == "" && m.Media == nil) {
			return false
		}
		m.Text, m.Original, m.Deleted, m.DeletedAt = m.Original, "", false, nil
		m.DeletedBy, m.Reviewed, m.ModBy = "", false, "" // Match bleibt für Statistik/Admin erhalten
		return true
	})
}

// moderate ändert eine Nachricht und verteilt die öffentliche Fassung an alle Clients.
func (h *Hub) moderate(roomName string, id int64, change func(*Message) bool) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok {
		return Message{}, ErrNoRoom
	}
	i, ok := r.find(id)
	if !ok {
		return Message{}, ErrNoMessage
	}
	m := &r.history[i]
	if change(m) {
		h.afterChangeLocked(r, m)
	}
	return h.out(r, *m, true), nil
}

// afterChangeLocked speichert eine geänderte Nachricht und verteilt sie. h.mu muss gehalten werden.
func (h *Hub) afterChangeLocked(r *room, m *Message) {
	if m.AwaitingReview() {
		h.pending[m.ID] = r.name
	} else {
		delete(h.pending, m.ID)
	}
	h.record(storeOp{kind: opMessage, room: r.name, msg: *m})
	pub := h.out(r, *m, false)
	h.broadcast(r, Event{Type: "update", Message: &pub, Online: len(r.clients)})
	h.emit(BusEvent{Type: "update", Room: r.name, Message: &pub})
}

// AutomodQueue liefert alle noch ungeprüften Automod-Treffer, neueste zuerst.
func (h *Hub) AutomodQueue() []QueueItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]QueueItem, 0, len(h.pending))
	for id, rn := range h.pending {
		r, ok := h.rooms[rn]
		if !ok {
			continue
		}
		if i, ok := r.find(id); ok && r.history[i].AwaitingReview() {
			out = append(out, QueueItem{Room: rn, Message: h.out(r, r.history[i], true)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Message.ID > out[j].Message.ID })
	return out
}

// SetAutomod übernimmt neue Regeln und speichert sie.
func (h *Hub) SetAutomod(cfg AutomodConfig) (AutomodConfig, error) {
	out, err := h.automod.Set(cfg)
	if err == nil {
		h.touchDoc("automod.json")
	}
	return out, err
}

// roomMeta ist der Inhalt der Kanal-Datei rooms/<name>.json.
type roomMeta struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Created     time.Time    `json:"created"`
	SlowMode    int          `json:"slowMode,omitempty"`
	Links       string       `json:"links,omitempty"`
	BotSlowMode int          `json:"botSlowMode,omitempty"`
	BotLinks    string       `json:"botLinks,omitempty"`
	Automod     *RoomAutomod `json:"automod,omitempty"`
	ReadRoles   []string     `json:"readRoles,omitempty"`
	WriteRoles  []string     `json:"writeRoles,omitempty"`
	Kind        string       `json:"kind,omitempty"`
	Members     []string     `json:"members,omitempty"`
	Pins        []int64      `json:"pins,omitempty"`
	Group       string       `json:"group,omitempty"`
	Pos         int          `json:"pos,omitempty"`
}

func metaOfRoom(r *room) roomMeta {
	m := roomMeta{Name: r.name, Description: r.description, Created: r.created, SlowMode: r.slowMode, Links: r.links,
		BotSlowMode: r.botSlow, BotLinks: r.botLinks, ReadRoles: r.readRoles, WriteRoles: r.writeRoles,
		Kind: r.kind, Members: r.members, Pins: r.pins, Group: r.group, Pos: r.pos}
	if a := r.automodLocked(); a.Mode != AutomodGlobal || len(a.Patterns) > 0 {
		m.Automod = &a
	}
	return m
}

// metaOf liefert die Metadaten eines Kanals für die Speicherung.
func (h *Hub) metaOf(name string) (roomMeta, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok {
		return roomMeta{}, false
	}
	return metaOfRoom(r), true
}

// roomMessages liefert alle Nachrichten eines Kanals (für das Neuschreiben der Datei).
func (h *Hub) roomMessages(name string) ([]Message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok {
		return nil, false
	}
	return append([]Message(nil), r.history...), true
}

// putRoom übernimmt einen geladenen Kanal (nur beim Start).
func (h *Hub) putRoom(meta roomMeta, msgs []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	links, err := validRoomSettings(meta.SlowMode, meta.Links)
	if err != nil {
		links, meta.SlowMode = LinksAll, 0
	}
	botLinks, err := validBotRoomSettings(meta.BotSlowMode, meta.BotLinks)
	if err != nil {
		botLinks, meta.BotSlowMode = LinksAll, 0
	}
	r := &room{name: meta.Name, description: meta.Description, created: meta.Created, slowMode: meta.SlowMode,
		links: links, botSlow: meta.BotSlowMode, botLinks: botLinks, amMode: AutomodGlobal,
		history: msgs, clients: make(map[chan Event]*Viewer),
		readRoles: meta.ReadRoles, writeRoles: meta.WriteRoles, kind: meta.Kind, members: meta.Members, pins: meta.Pins,
		group: meta.Group, pos: meta.Pos}
	if meta.Automod != nil {
		if mode, am, err := compileRoomAutomod(*meta.Automod); err == nil {
			r.amMode, r.am = mode, am
		} else {
			log.Printf("#%s: Automod-Regeln des Kanals ungültig (%v) – es gelten die globalen", meta.Name, err)
		}
	}
	for i := range msgs {
		msgs[i].Room = meta.Name
		msgs[i].Reply = nil
		if msgs[i].AwaitingReview() {
			h.pending[msgs[i].ID] = meta.Name
		}
		if msgs[i].ID > h.nextID.Load() {
			h.nextID.Store(msgs[i].ID)
		}
	}
	r.rebuildThreads()
	if len(msgs) > 0 {
		r.last = msgs[len(msgs)-1].Time
		if r.created.IsZero() {
			r.created = msgs[0].Time
		}
	}
	h.rooms[meta.Name] = r
}

type roomSnapshot struct {
	meta roomMeta
	msgs []Message
}

// snapshotRooms liefert alle Kanäle mit vollständigem Verlauf (für Migration und Komplett-Export).
func (h *Hub) snapshotRooms() map[string]roomSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]roomSnapshot{}
	for name, r := range h.rooms {
		out[name] = roomSnapshot{
			metaOfRoom(r),
			append([]Message(nil), r.history...),
		}
	}
	return out
}
