// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ucng/internal/voicerelay"
)

// Sprachkanäle: Jeder Kanal kann zusätzlich ein Sprachkanal sein (RoomInfo.Voice). Der
// Chat-Server entscheidet über Zutritt und Rechte und stellt signierte Zutrittskarten aus; die
// Sprache selbst läuft über einen Relay – eingebaut (/api/voice/ws) oder als eigenes Programm
// (cmd/ucng-voice, beliebig viele). Ein Sprachkanal liegt immer ganz auf einem Relay; neue
// Kanäle kommen auf den am wenigsten ausgelasteten.

// VoicePeer: Teilnehmer eines Sprachkanals (in Ereignissen und /api/voice).
type VoicePeer = voicerelay.PeerInfo

// VoiceConfig: Einstellungen eines Sprachkanals.
type VoiceConfig struct {
	Preset      string   `json:"preset"`               // sparsam, standard, hoch, musik
	MaxPeers    int      `json:"maxPeers"`             // Teilnehmer
	MaxSpeakers int      `json:"maxSpeakers"`          // gleichzeitig hörbare Sprecher
	Guests      string   `json:"guests"`               // speak, listen, none
	Stage       bool     `json:"stage"`                // Bühne: nur Team, Bühnen-Rollen und Freigeschaltete sprechen
	StageRoles  []string `json:"stageRoles,omitempty"` // Rollen, die auf der Bühne sprechen dürfen
	// Bildschirm teilen: "" (aus), "speakers" (alle, die sprechen dürfen) oder "roles" (Team und ScreenRoles)
	Screen        string   `json:"screen,omitempty"`
	ScreenRoles   []string `json:"screenRoles,omitempty"`
	ScreenQuality string   `json:"screenQuality,omitempty"` // text, standard, motion
	MaxScreens    int      `json:"maxScreens,omitempty"`    // gleichzeitige Freigaben
}

// screenPreset: Qualität einer Bildschirmfreigabe.
type screenPreset struct {
	Kbps   int `json:"kbps"`
	FPS    int `json:"fps"`
	Height int `json:"height"` // höchstens so viele Zeilen (Breite folgt dem Seitenverhältnis)
}

var screenPresets = map[string]screenPreset{
	"text":     {Kbps: 1500, FPS: 5, Height: 1080},  // Folien, Code, Dokumente: scharf, wenig Bewegung
	"standard": {Kbps: 2500, FPS: 15, Height: 1080}, // Allgemein
	"motion":   {Kbps: 4000, FPS: 30, Height: 720},  // Videos, Spiele: flüssig
}

const (
	ScreenOff      = ""
	ScreenSpeakers = "speakers"
	ScreenRoles    = "roles"
	maxScreens     = 4
)

type voicePreset struct {
	Bitrate int  `json:"bitrate"` // Opus, bit/s
	Stereo  bool `json:"stereo"`
	Music   bool `json:"music"` // ohne Echo- und Rauschunterdrückung
}

var voicePresets = map[string]voicePreset{
	"sparsam":  {Bitrate: 16000},
	"standard": {Bitrate: 24000},
	"hoch":     {Bitrate: 40000},
	"musik":    {Bitrate: 96000, Stereo: true, Music: true},
}

const (
	VoiceGuestsSpeak  = "speak"
	VoiceGuestsListen = "listen"
	VoiceGuestsNone   = "none"

	voiceDefaultPeers    = 25
	voiceDefaultSpeakers = 4
	voiceMaxPeers        = 1000
	voiceMaxSpeakers     = 16
	voiceKickPause       = time.Minute
	voiceAgentStale      = 15 * time.Second
	voiceAgentForget     = 2 * time.Minute
	voiceMaxAgents       = 64
)

var (
	ErrVoicePreset  = errors.New("Unbekannte Klang-Voreinstellung")
	ErrVoiceGuests  = errors.New("Gäste-Regel muss speak, listen oder none sein")
	ErrNoVoice      = errors.New("Dieser Kanal ist kein Sprachkanal")
	ErrVoiceOff     = errors.New("Sprachkanäle sind auf diesem Server ausgeschaltet")
	ErrVoiceNoGuest = errors.New("Dieser Sprachkanal ist nur für angemeldete Personen")
	ErrVoiceKicked  = errors.New("Du wurdest gerade aus dem Sprachkanal entfernt – bitte kurz warten")
	ErrVoiceFull    = errors.New("Der Sprachkanal ist voll")
	ErrVoiceBusy    = errors.New("Der Sprach-Server ist gerade ausgelastet – bitte später noch einmal")
	ErrScreenMode   = errors.New("Bildschirmfreigabe muss leer, speakers oder roles sein")
	ErrScreenPreset = errors.New("Unbekannte Qualität für die Bildschirmfreigabe")
)

// validVoice prüft die Einstellungen eines Sprachkanals (nil = nur Text) und ergänzt Standards.
// Es entsteht immer eine neue Kopie – gespeicherte Einstellungen werden nie verändert.
func (h *Hub) validVoice(in *VoiceConfig) (*VoiceConfig, error) {
	if in == nil {
		return nil, nil
	}
	v := *in
	v.Preset = strings.ToLower(strings.TrimSpace(v.Preset))
	if v.Preset == "" {
		v.Preset = "standard"
	}
	if _, ok := voicePresets[v.Preset]; !ok {
		return nil, ErrVoicePreset
	}
	if v.MaxPeers <= 0 {
		v.MaxPeers = voiceDefaultPeers
	}
	v.MaxPeers = min(max(v.MaxPeers, 2), voiceMaxPeers)
	if v.MaxSpeakers <= 0 {
		v.MaxSpeakers = voiceDefaultSpeakers
	}
	v.MaxSpeakers = min(max(v.MaxSpeakers, 1), voiceMaxSpeakers)
	switch v.Guests {
	case "":
		v.Guests = VoiceGuestsSpeak
	case VoiceGuestsSpeak, VoiceGuestsListen, VoiceGuestsNone:
	default:
		return nil, ErrVoiceGuests
	}
	v.StageRoles = h.roles.Filter(v.StageRoles)
	if len(v.StageRoles) == 0 {
		v.StageRoles = nil
	}
	switch v.Screen {
	case "off":
		v.Screen = ScreenOff
	case ScreenOff, ScreenSpeakers, ScreenRoles:
	default:
		return nil, ErrScreenMode
	}
	if v.Screen == ScreenOff {
		v.ScreenRoles, v.ScreenQuality, v.MaxScreens = nil, "", 0
		return &v, nil
	}
	if v.ScreenQuality == "" {
		v.ScreenQuality = "standard"
	}
	if _, ok := screenPresets[v.ScreenQuality]; !ok {
		return nil, ErrScreenPreset
	}
	if v.MaxScreens <= 0 {
		v.MaxScreens = 1
	}
	v.MaxScreens = min(v.MaxScreens, maxScreens)
	v.ScreenRoles = h.roles.Filter(v.ScreenRoles)
	if len(v.ScreenRoles) == 0 {
		v.ScreenRoles = nil
	}
	return &v, nil
}

func (v *VoiceConfig) equal(o *VoiceConfig) bool {
	if v == nil || o == nil {
		return v == o
	}
	return reflect.DeepEqual(*v, *o)
}

// describe: kurze Beschreibung fürs Protokoll.
func (v *VoiceConfig) describe() string {
	if v == nil {
		return "aus"
	}
	s := fmt.Sprintf("%s, %d Teilnehmer, %d Sprecher, Gäste: %s", v.Preset, v.MaxPeers, v.MaxSpeakers, v.Guests)
	if v.Stage {
		s += ", Bühne"
	}
	if v.Screen != ScreenOff {
		s += fmt.Sprintf(", Bildschirm: %s (%s, %d)", v.Screen, v.ScreenQuality, v.MaxScreens)
	}
	return s
}

func (v *VoiceConfig) preset() voicePreset { return voicePresets[v.Preset] }

func (v *VoiceConfig) screenPreset() screenPreset { return screenPresets[v.ScreenQuality] }

// voiceAccess: Einstellungen und Zugang eines Kanals für a.
func (h *Hub) voiceAccess(name string, a *Access) (cfg *VoiceConfig, exists, read, write bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.kind != "" {
		return nil, false, false, false
	}
	return r.voice, true, r.canRead(a), r.canWrite(a)
}

// voiceRoomsFor: Sprachkanäle, die a sehen darf.
func (h *Hub) voiceRoomsFor(a *Access) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for name, r := range h.rooms {
		if r.kind == "" && r.voice != nil && r.canRead(a) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// --- Sprach-Schlüssel (signiert die Zutrittskarten) ---

type voiceDoc struct {
	Key string `json:"key"` // Ed25519-Seed, Base64
}

func (h *Hub) registerVoiceDoc() {
	h.RegisterDoc("voice.json", func() any {
		h.voiceMu.Lock()
		defer h.voiceMu.Unlock()
		d := voiceDoc{}
		if h.voiceKey != nil {
			d.Key = base64.StdEncoding.EncodeToString(h.voiceKey.Seed())
		}
		return d
	}, func(b []byte) error {
		var d voiceDoc
		if err := jsonUnmarshal(b, &d); err != nil {
			return err
		}
		seed, err := base64.StdEncoding.DecodeString(d.Key)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil // kein Schlüssel gespeichert: wird neu erzeugt
		}
		h.voiceMu.Lock()
		h.voiceKey = ed25519.NewKeyFromSeed(seed)
		h.voiceMu.Unlock()
		return nil
	})
}

// VoiceKey liefert den Sprach-Schlüssel (beim ersten Aufruf erzeugt und gespeichert).
func (h *Hub) VoiceKey() ed25519.PrivateKey {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()
	if h.voiceKey == nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		h.voiceKey = priv
		h.touchDoc("voice.json")
	}
	return h.voiceKey
}

// --- Verwaltung der Sprachkanäle ---

const builtinRelay = "builtin"

type voiceMgr struct {
	s        *Server
	priv     ed25519.PrivateKey
	pub      ed25519.PublicKey
	builtin  *voicerelay.Relay // nil = nur externe Agenten
	token    string            // für Agenten (VOICE_AGENT_TOKEN)
	conns    *streamLimiter    // Verbindungen zum eingebauten Relay je IP
	ticketRL *rateLimiter      // Zutrittskarten je IP
	client   *http.Client
	csp      atomic.Value // string: zusätzliche connect-src-Quellen (Agenten)
	stop     chan struct{}

	mu     sync.Mutex
	rooms  map[string]*voiceRoom
	agents map[string]*voiceAgent
}

type voiceRoom struct {
	relay    string // Relay, auf dem der Kanal liegt
	peers    []VoicePeer
	grants   map[string]bool      // Bühne: true = freigeschaltet, false = Rederecht entzogen
	muted    map[string]bool      // vom Team stummgeschaltet (bleibt beim erneuten Beitreten)
	kicked   map[string]time.Time // entfernt: Wiedereintritt erst danach
	noScreen map[string]bool      // vom Team: darf hier nicht (mehr) teilen
}

type voiceAgent struct {
	ID         string              `json:"id"`
	WSURL      string              `json:"wsURL"`
	ControlURL string              `json:"-"`
	Stats      voicerelay.Stats    `json:"stats"`
	Seen       time.Time           `json:"seen"`
	Version    string              `json:"version,omitempty"`
	rooms      map[string]struct{} // Kanäle laut letzter Meldung
}

func newVoiceMgr(s *Server) *voiceMgr {
	priv := s.hub.VoiceKey()
	v := &voiceMgr{
		s: s, priv: priv, pub: priv.Public().(ed25519.PublicKey), token: s.cfg.VoiceAgentToken,
		conns:    newStreamLimiter(max(s.cfg.VoiceMaxParticipants, 1)*2, 5),
		ticketRL: newRateLimiter(30, time.Minute),
		client:   &http.Client{Timeout: 5 * time.Second},
		rooms:    map[string]*voiceRoom{}, agents: map[string]*voiceAgent{}, stop: make(chan struct{}),
	}
	v.csp.Store("")
	if !s.cfg.VoiceNoBuiltin {
		v.builtin = voicerelay.New(voicerelay.Config{
			PublicKey:       func() ed25519.PublicKey { return v.pub },
			MaxParticipants: s.cfg.VoiceMaxParticipants,
			MaxMbit:         s.cfg.VoiceMaxMbit,
			OnChange:        func(ch string, peers []voicerelay.PeerInfo) { v.setPeers(builtinRelay, ch, peers) },
		})
	}
	s.hub.mu.Lock()
	s.hub.onUserChange = v.userChanged
	s.hub.onRoomChange = v.roomChanged
	s.hub.mu.Unlock()
	go v.janitor()
	return v
}

// Close beendet alle Sprachverbindungen des eingebauten Relays.
func (v *voiceMgr) Close() {
	select {
	case <-v.stop:
	default:
		close(v.stop)
	}
	if v.builtin != nil {
		v.builtin.Close()
	}
}

func (v *voiceMgr) room(name string) *voiceRoom {
	st := v.rooms[name]
	if st == nil {
		st = &voiceRoom{grants: map[string]bool{}, muted: map[string]bool{}, kicked: map[string]time.Time{}, noScreen: map[string]bool{}}
		v.rooms[name] = st
	}
	return st
}

// setPeers übernimmt die Teilnehmer eines Kanals von einem Relay und verteilt sie an alle,
// die den Kanal sehen dürfen.
func (v *voiceMgr) setPeers(relay, name string, peers []VoicePeer) {
	v.mu.Lock()
	st := v.room(name)
	if relay != st.relay && len(st.peers) > 0 && len(peers) == 0 {
		v.mu.Unlock() // veraltete Meldung eines anderen Relays
		return
	}
	if len(peers) > 0 {
		st.relay = relay
	}
	if reflect.DeepEqual(st.peers, peers) || (len(st.peers) == 0 && len(peers) == 0) {
		v.mu.Unlock()
		return
	}
	st.peers = append([]VoicePeer(nil), peers...)
	if len(peers) == 0 {
		clear(st.grants) // Freischaltungen der Bühne gelten bis der Kanal leer ist
	}
	v.mu.Unlock()
	v.s.hub.SendToReaders(name, Event{Type: "voice", Room: name, Voice: peers})
}

// Peers: Teilnehmer eines Kanals.
func (v *voiceMgr) Peers(name string) []VoicePeer {
	v.mu.Lock()
	defer v.mu.Unlock()
	if st := v.rooms[name]; st != nil {
		return append([]VoicePeer{}, st.peers...)
	}
	return []VoicePeer{}
}

// janitor vergisst verstummte Agenten und räumt leere Kanäle auf.
func (v *voiceMgr) janitor() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
		}
		v.ticketRL.cleanup()
		now := time.Now()
		type gone struct{ relay, room string }
		var stale []gone
		v.mu.Lock()
		for id, a := range v.agents {
			if now.Sub(a.Seen) > voiceAgentStale {
				for name, st := range v.rooms {
					if st.relay == id && len(st.peers) > 0 {
						stale = append(stale, gone{id, name})
					}
				}
			}
			if now.Sub(a.Seen) > voiceAgentForget {
				delete(v.agents, id)
				log.Printf("Sprache: Agent %s meldet sich nicht mehr", id)
			}
		}
		for name, st := range v.rooms {
			for u, until := range st.kicked {
				if now.After(until) {
					delete(st.kicked, u)
				}
			}
			if len(st.peers) == 0 && len(st.muted) == 0 && len(st.kicked) == 0 && len(st.noScreen) == 0 {
				delete(v.rooms, name)
			}
		}
		v.mu.Unlock()
		for _, g := range stale {
			v.setPeers(g.relay, g.room, nil)
		}
		v.updateCSP()
	}
}

// --- Rechte ---

type voiceWho struct {
	ID, Name, IPHash  string
	Team, Member      bool
	Roles             []string
	GuestReadOnlyChat bool // Modus „hosted“: Gäste dürfen im Chat nicht schreiben
}

func (w voiceWho) access() *Access { return &Access{ID: w.ID, Team: w.Team, Roles: w.Roles} }

// guest: Person ohne Konto, ohne Rolle und nicht im Team.
func (w voiceWho) guest() bool { return !w.Team && !w.Member && len(w.Roles) == 0 }

type voiceDecision struct {
	Cfg    VoiceConfig
	Speak  bool
	Muted  bool
	Guest  bool
	Screen bool // darf den Bildschirm teilen
}

// decide: Darf w dem Sprachkanal beitreten, sprechen, ist w stummgeschaltet?
func (v *voiceMgr) decide(name string, w voiceWho) (voiceDecision, error) {
	var d voiceDecision
	cfg, exists, read, write := v.s.hub.voiceAccess(name, w.access())
	if !exists || !read {
		return d, ErrNoRoom
	}
	if cfg == nil {
		return d, ErrNoVoice
	}
	d.Cfg = *cfg
	ip := w.IPHash
	if w.Team {
		ip = ""
	}
	if sa, ok := v.s.hub.Mod().Active(w.ID, ip); ok {
		if sa.Kind == "ban" {
			return d, &SanctionError{Kind: sa.Kind, Until: sa.Until, Reason: sa.Reason, loc: v.s.hub.Location()}
		}
		d.Muted = true
	}
	d.Guest = w.guest()
	if d.Guest && cfg.Guests == VoiceGuestsNone {
		return d, ErrVoiceNoGuest
	}
	d.Speak = write && !w.GuestReadOnlyChat
	if d.Guest && cfg.Guests == VoiceGuestsListen {
		d.Speak = false
	}
	if cfg.Stage {
		d.Speak = w.Team || intersects(cfg.StageRoles, w.Roles)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if st := v.rooms[name]; st != nil {
		if until, ok := st.kicked[w.ID]; ok && time.Now().Before(until) {
			return d, ErrVoiceKicked
		}
		if g, ok := st.grants[w.ID]; ok && !w.Team {
			d.Speak = g
		}
		if st.muted[w.ID] {
			d.Muted = true
		}
	}
	// Bildschirm: nur, wer sprechen darf und nicht stummgeschaltet ist
	switch cfg.Screen {
	case ScreenSpeakers:
		d.Screen = d.Speak && !d.Muted
	case ScreenRoles:
		d.Screen = d.Speak && !d.Muted && (w.Team || intersects(cfg.ScreenRoles, w.Roles))
	}
	if st := v.rooms[name]; st != nil && st.noScreen[w.ID] {
		d.Screen = false
	}
	return d, nil
}

func (v *voiceMgr) canModerate(w voiceWho) bool {
	return w.Team || v.s.hub.Roles().PermsOf(w.Roles, w.Team).Mute
}

func (v *voiceMgr) whoByID(id string) (voiceWho, bool) {
	u, ok := v.s.users.Get(id)
	if !ok {
		return voiceWho{}, false
	}
	return voiceWho{ID: u.ID, Name: u.Name, Team: u.Team, Member: u.Account != nil, Roles: u.Roles,
		GuestReadOnlyChat: u.Account == nil && v.s.guestPolicy() != GuestsWrite}, true
}

func (s *Server) voiceWhoOf(me Identity, ipHash string) voiceWho {
	return voiceWho{ID: me.ID, Name: me.Name, Team: me.Team, Member: me.Member, Roles: me.Roles, IPHash: ipHash,
		GuestReadOnlyChat: !me.Member && s.guestPolicy() != GuestsWrite}
}

// reconcile gleicht die Rechte einer Person in einem Kanal ab (nach Sperren, Rollen- oder
// Kanaländerungen): ohne Zutritt → entfernen, sonst Sprechen/Stumm anpassen.
func (v *voiceMgr) reconcile(room string, p VoicePeer) {
	w, ok := v.whoByID(p.User)
	if !ok {
		v.control(room, p.User, "kick", false, "session")
		return
	}
	d, err := v.decide(room, w)
	if err != nil {
		v.control(room, p.User, "kick", false, byeCode(err))
		return
	}
	if d.Speak != p.Speak {
		v.control(room, p.User, "speak", d.Speak, "")
	}
	if d.Muted != p.Muted {
		v.control(room, p.User, "mute", d.Muted, "")
	}
	if d.Screen != p.ScreenOK {
		v.control(room, p.User, "screen", d.Screen, "")
	}
}

// byeCode: Grund fürs Entfernen als Kennung (der Browser zeigt den Text in seiner Sprache).
func byeCode(err error) string {
	var se *SanctionError
	switch {
	case errors.As(err, &se):
		return "banned"
	case errors.Is(err, ErrVoiceNoGuest):
		return "noguest"
	case errors.Is(err, ErrNoVoice):
		return "novoice"
	case errors.Is(err, ErrVoiceKicked):
		return "kicked"
	}
	return "noaccess"
}

func (v *voiceMgr) userChanged(userID string) {
	for _, rp := range v.findUser(userID) {
		v.reconcile(rp.room, rp.peer)
	}
}

type roomPeer struct {
	room string
	peer VoicePeer
}

func (v *voiceMgr) findUser(userID string) []roomPeer {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []roomPeer
	for name, st := range v.rooms {
		for _, p := range st.peers {
			if p.User == userID {
				out = append(out, roomPeer{name, p})
			}
		}
	}
	return out
}

func (v *voiceMgr) roomChanged(oldName, newName string) {
	if oldName != newName { // umbenannt oder gelöscht: Sprachkanal schließen
		reason := "deleted"
		if newName != "" {
			reason = "renamed"
		}
		v.control(oldName, "", "close", false, reason)
		v.mu.Lock()
		delete(v.rooms, oldName)
		v.mu.Unlock()
		v.s.hub.SendToReaders(newName, Event{Type: "voice", Room: newName})
		return
	}
	for _, p := range v.Peers(oldName) {
		v.reconcile(oldName, p)
	}
}

// --- Relays ---

// control schickt einen Steuerbefehl an den Relay des Kanals.
func (v *voiceMgr) control(room, user, action string, value bool, reason string) {
	v.mu.Lock()
	relay := ""
	if st := v.rooms[room]; st != nil {
		relay = st.relay
	}
	var agent *voiceAgent
	if a, ok := v.agents[relay]; ok {
		cp := *a
		agent = &cp
	}
	v.mu.Unlock()
	if relay == builtinRelay || (relay == "" && agent == nil) {
		if v.builtin != nil {
			v.builtin.Control(room, user, action, value, reason)
		}
		return
	}
	if agent == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"channel": room, "user": user, "action": action, "value": value, "reason": reason})
	go func() {
		req, err := http.NewRequest(http.MethodPost, agent.ControlURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+v.token)
		req.Header.Set("Content-Type", "application/json")
		res, err := v.client.Do(req)
		if err != nil {
			log.Printf("Sprache: Agent %s nicht erreichbar: %v", agent.ID, err)
			return
		}
		res.Body.Close()
	}()
}

type relayChoice struct {
	id  string
	url string // leer = eingebaut (/api/voice/ws)
}

// pick wählt den Relay für einen Kanal: bestehende Kanäle bleiben, wo sie sind; neue kommen
// auf den Relay mit der geringsten Auslastung, der noch Platz hat.
func (v *voiceMgr) pick(room string, cfg VoiceConfig) (relayChoice, error) {
	need := voicerelay.EstimateKbps(cfg.preset().Bitrate, cfg.MaxSpeakers) / 1000
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if st := v.rooms[room]; st != nil && len(st.peers) > 0 {
		if len(st.peers) >= cfg.MaxPeers {
			return relayChoice{}, ErrVoiceFull
		}
		if st.relay == builtinRelay && v.builtin != nil {
			return relayChoice{id: builtinRelay}, nil
		}
		if a, ok := v.agents[st.relay]; ok && now.Sub(a.Seen) < voiceAgentStale {
			return relayChoice{id: a.ID, url: a.WSURL}, nil
		}
	}
	best, bestLoad := relayChoice{}, 2.0
	consider := func(c relayChoice, st voicerelay.Stats) {
		if st.Max <= 0 || st.MaxMbit <= 0 || st.Participants >= st.Max || st.EstimateMbit+need > st.MaxMbit {
			return
		}
		load := max(float64(st.Participants)/float64(st.Max), st.EstimateMbit/st.MaxMbit)
		if load < bestLoad {
			best, bestLoad = c, load
		}
	}
	if v.builtin != nil {
		consider(relayChoice{id: builtinRelay}, v.builtin.Stats())
	}
	ids := make([]string, 0, len(v.agents))
	for id := range v.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids) // gleich ausgelastet: immer derselbe
	for _, id := range ids {
		if a := v.agents[id]; now.Sub(a.Seen) < voiceAgentStale {
			consider(relayChoice{id: a.ID, url: a.WSURL}, a.Stats)
		}
	}
	if best.id == "" {
		return best, ErrVoiceBusy
	}
	return best, nil
}

// updateCSP: Agenten-Adressen für connect-src.
func (v *voiceMgr) updateCSP() {
	v.mu.Lock()
	seen := map[string]bool{}
	var list []string
	for _, a := range v.agents {
		if u, err := url.Parse(a.WSURL); err == nil && u.Host != "" {
			o := u.Scheme + "://" + u.Host
			if !seen[o] {
				seen[o] = true
				list = append(list, o)
			}
		}
	}
	v.mu.Unlock()
	sort.Strings(list)
	v.csp.Store(strings.Join(list, " "))
}

// connectSrc: zusätzliche Quellen für die Content Security Policy.
func (s *Server) voiceConnectSrc(r *http.Request) string {
	if s.voice == nil {
		return ""
	}
	src := ""
	if r.Host != "" && !strings.ContainsAny(r.Host, " ;'\"") {
		src = "ws://" + r.Host + " wss://" + r.Host
	}
	if extra, _ := s.voice.csp.Load().(string); extra != "" {
		src += " " + extra
	}
	return src
}

// --- HTTP ---

func voiceStatus(err error) int {
	var se *SanctionError
	switch {
	case errors.As(err, &se), errors.Is(err, ErrVoiceNoGuest), errors.Is(err, ErrVoiceKicked):
		return http.StatusForbidden
	case errors.Is(err, ErrNoRoom), errors.Is(err, ErrNoVoice), errors.Is(err, ErrVoiceOff):
		return http.StatusNotFound
	case errors.Is(err, ErrVoiceFull), errors.Is(err, ErrVoiceBusy):
		return http.StatusServiceUnavailable
	}
	return statusFor(err)
}

func voiceFail(w http.ResponseWriter, err error) { writeErr(w, voiceStatus(err), err.Error()) }

// GET /api/voice – Teilnehmer aller Sprachkanäle, die die Person sehen darf.
func (s *Server) handleVoiceList(w http.ResponseWriter, r *http.Request) {
	if s.voice == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "rooms": map[string]any{}})
		return
	}
	out := map[string][]VoicePeer{}
	for _, name := range s.hub.voiceRoomsFor(s.accessOf(r)) {
		out[name] = s.voice.Peers(name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "rooms": out})
}

// POST /api/voice/{room}/join – Zutrittskarte für einen Sprachkanal.
func (s *Server) handleVoiceJoin(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, ErrNoSession.Error())
		return
	}
	ip := s.clientIP(r)
	if !v.ticketRL.Allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Versuche – bitte kurz warten.")
		return
	}
	ipHash := s.hub.Mod().IPHash(ip)
	s.hub.Mod().SeenIP(me.ID, ipHash)
	room := NormRoom(r.PathValue("room"))
	who := s.voiceWhoOf(me, ipHash)
	d, err := v.decide(room, who)
	if err != nil {
		voiceFail(w, err)
		return
	}
	choice, err := v.pick(room, d.Cfg)
	if err != nil {
		voiceFail(w, err)
		return
	}
	// In einem anderen Kanal auf einem anderen Relay? Dort beenden (eine Sprachverbindung je Person).
	for _, rp := range v.findUser(me.ID) {
		v.mu.Lock()
		other := v.rooms[rp.room].relay
		v.mu.Unlock()
		if rp.room != room && other != choice.id {
			v.control(rp.room, me.ID, "kick", false, "moved")
		}
	}
	pr, sp := d.Cfg.preset(), d.Cfg.screenPreset()
	ticket := voicerelay.SignTicket(v.priv, voicerelay.Ticket{
		Channel: room, User: me.ID, Name: me.Name, Speak: d.Speak, Muted: d.Muted,
		Bitrate: pr.Bitrate, Stereo: pr.Stereo, MaxSpeakers: d.Cfg.MaxSpeakers, MaxPeers: d.Cfg.MaxPeers,
		Screen: d.Screen, ScreenKbps: sp.Kbps, MaxScreens: d.Cfg.MaxScreens,
	}, time.Now())
	u := choice.url
	if u == "" {
		u = "/api/voice/ws"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url": u, "ticket": ticket, "preset": d.Cfg.Preset, "bitrate": pr.Bitrate, "stereo": pr.Stereo, "music": pr.Music,
		"maxSpeakers": d.Cfg.MaxSpeakers, "maxPeers": d.Cfg.MaxPeers, "speak": d.Speak, "muted": d.Muted,
		"stage": d.Cfg.Stage, "guest": d.Guest, "moderate": v.canModerate(who),
		"screen": d.Screen, "screenOn": d.Cfg.Screen != ScreenOff, "screenKbps": sp.Kbps, "screenFps": sp.FPS, "screenHeight": sp.Height,
	})
}

// GET /api/voice/ws – eingebauter Relay (WebSocket). Nur von der eigenen Seite.
func (s *Server) handleVoiceWS(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil || v.builtin == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	if !s.sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "Herkunft nicht erlaubt")
		return
	}
	release, ok := v.conns.acquire(s.clientIP(r))
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Sprachverbindungen von dieser Adresse")
		return
	}
	defer release()
	v.builtin.ServeHTTP(w, r)
}

// sameOrigin: kommt die Anfrage von der eigenen Seite (Origin = Host oder PUBLIC_URL)?
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	return s.cfg.PublicURL != "" && strings.EqualFold(o, originOf(s.cfg.PublicURL))
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// POST /api/voice/{room}/mod {"userId": "…", "action": "mute|unmute|kick|speak|nospeak|lowerhand"}
func (s *Server) handleVoiceMod(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, ErrNoSession.Error())
		return
	}
	who := s.voiceWhoOf(me, "")
	room := NormRoom(r.PathValue("room"))
	if cfg, exists, read, _ := s.hub.voiceAccess(room, who.access()); !exists || !read || cfg == nil {
		voiceFail(w, ErrNoVoice)
		return
	}
	var in struct {
		UserID string `json:"userId"`
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Action != "lowerhand" && !v.canModerate(who) || in.Action == "lowerhand" && in.UserID != me.ID && !v.canModerate(who) {
		fail(w, ErrNoPerm)
		return
	}
	target, ok := s.users.Get(in.UserID)
	if !ok {
		fail(w, ErrNoUser)
		return
	}
	if in.UserID != me.ID && (target.Team || s.hub.Roles().PermsOf(target.Roles, false).Mute) && !me.Team &&
		in.Action != "speak" && in.Action != "lowerhand" {
		writeErr(w, http.StatusForbidden, "Team-Mitglieder und andere Moderierende kann nur das Team stummschalten oder entfernen")
		return
	}
	v.mu.Lock()
	st := v.room(room)
	switch in.Action {
	case "mute":
		st.muted[in.UserID] = true
	case "unmute":
		delete(st.muted, in.UserID)
	case "kick":
		st.kicked[in.UserID] = time.Now().Add(voiceKickPause)
	case "speak":
		st.grants[in.UserID] = true
	case "nospeak":
		st.grants[in.UserID] = false
	case "stopscreen":
		st.noScreen[in.UserID] = true
	case "allowscreen":
		delete(st.noScreen, in.UserID)
	case "lowerhand":
	default:
		v.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "Unbekannte Aktion")
		return
	}
	v.mu.Unlock()
	switch in.Action {
	case "mute", "unmute":
		muted := in.Action == "mute"
		if !muted { // eine Chat-Stummschaltung bleibt bestehen
			if sa, ok := s.hub.Mod().Active(in.UserID, ""); ok && sa.Kind == "mute" {
				muted = true
			}
		}
		v.control(room, in.UserID, "mute", muted, "")
	case "kick":
		v.control(room, in.UserID, "kick", false, "kicked")
	case "speak":
		v.control(room, in.UserID, "speak", true, "")
		v.control(room, in.UserID, "hand", false, "")
	case "nospeak":
		v.control(room, in.UserID, "speak", false, "")
	case "lowerhand":
		v.control(room, in.UserID, "hand", false, "")
	}
	if in.Action != "kick" && in.Action != "lowerhand" { // Recht zum Teilen hängt an Sprechen/Stumm
		if tw, ok := v.whoByID(in.UserID); ok {
			if d, err := v.decide(room, tw); err == nil {
				v.control(room, in.UserID, "screen", d.Screen, "")
			}
		}
	}
	if in.Action != "lowerhand" {
		actor, display := chatActor(me)
		s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice." + in.Action, Room: room,
			UserID: in.UserID, UserName: target.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Agenten (cmd/ucng-voice) ---

type agentReport struct {
	ID         string                 `json:"id"`
	WSURL      string                 `json:"wsURL"`
	ControlURL string                 `json:"controlURL"`
	Version    string                 `json:"version"`
	Stats      voicerelay.Stats       `json:"stats"`
	Rooms      map[string][]VoicePeer `json:"rooms"`
}

func (v *voiceMgr) agentAuth(r *http.Request) bool {
	t := bearer(r)
	return v.token != "" && len(t) == len(v.token) && subtle.ConstantTimeCompare([]byte(t), []byte(v.token)) == 1
}

// POST /api/voice/agents/report – ein Agent meldet Auslastung und Teilnehmer (alle paar Sekunden).
// Antwort: öffentlicher Sprach-Schlüssel und erlaubte Herkunft der Browser.
func (s *Server) handleVoiceAgentReport(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		voiceFail(w, ErrVoiceOff)
		return
	}
	if !v.agentAuth(r) {
		writeErr(w, http.StatusUnauthorized, "Ungültiger Agenten-Schlüssel")
		return
	}
	var in agentReport
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	ws, err1 := url.Parse(in.WSURL)
	ctl, err2 := url.Parse(in.ControlURL)
	if err1 != nil || err2 != nil || (ws.Scheme != "ws" && ws.Scheme != "wss") || ws.Host == "" ||
		(ctl.Scheme != "http" && ctl.Scheme != "https") || ctl.Host == "" {
		writeErr(w, http.StatusBadRequest, "wsURL (ws/wss) und controlURL (http/https) werden gebraucht")
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" || len(id) > 64 || id == builtinRelay {
		id = ws.Host
	}
	v.mu.Lock()
	a, known := v.agents[id]
	if !known && len(v.agents) >= voiceMaxAgents {
		v.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "Zu viele Sprach-Agenten")
		return
	}
	if !known {
		a = &voiceAgent{ID: id, rooms: map[string]struct{}{}}
		v.agents[id] = a
		log.Printf("Sprache: Agent %s angemeldet (%s)", id, in.WSURL)
	}
	a.WSURL, a.ControlURL, a.Stats, a.Seen, a.Version = in.WSURL, in.ControlURL, in.Stats, time.Now(), in.Version
	prev := a.rooms
	a.rooms = map[string]struct{}{}
	for name := range in.Rooms {
		a.rooms[name] = struct{}{}
	}
	v.mu.Unlock()
	if !known {
		v.updateCSP()
	}
	for name, peers := range in.Rooms {
		v.setPeers(id, name, peers)
	}
	for name := range prev {
		if _, still := in.Rooms[name]; !still {
			v.setPeers(id, name, nil)
		}
	}
	origins := []string{}
	if o := originOf(s.baseURL()); o != "" {
		origins = append(origins, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"publicKey": base64.StdEncoding.EncodeToString(v.pub), "origins": origins,
	})
}

// GET /api/admin/voice – Relays und belegte Sprachkanäle.
func (s *Server) handleAdminVoice(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	type relayInfo struct {
		ID      string           `json:"id"`
		Builtin bool             `json:"builtin,omitempty"`
		URL     string           `json:"url,omitempty"`
		Version string           `json:"version,omitempty"`
		Healthy bool             `json:"healthy"`
		Seen    *time.Time       `json:"seen,omitempty"`
		Stats   voicerelay.Stats `json:"stats"`
	}
	relays := []relayInfo{}
	if v.builtin != nil {
		relays = append(relays, relayInfo{ID: builtinRelay, Builtin: true, Healthy: true, Stats: v.builtin.Stats()})
	}
	rooms := map[string]int{}
	v.mu.Lock()
	for _, a := range v.agents {
		seen := a.Seen
		relays = append(relays, relayInfo{ID: a.ID, URL: a.WSURL, Version: a.Version, Seen: &seen,
			Healthy: time.Since(a.Seen) < voiceAgentStale, Stats: a.Stats})
	}
	for name, st := range v.rooms {
		if len(st.peers) > 0 {
			rooms[name] = len(st.peers)
		}
	}
	v.mu.Unlock()
	sort.Slice(relays, func(i, j int) bool { return relays[i].Builtin || !relays[j].Builtin && relays[i].ID < relays[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "relays": relays, "rooms": rooms,
		"agents": v.token != "", "builtin": v.builtin != nil})
}

// voiceRoutes meldet die Pfade der Sprachkanäle an.
func (s *Server) voiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/voice", s.handleVoiceList)
	mux.HandleFunc("GET /api/voice/ws", s.handleVoiceWS)
	mux.HandleFunc("POST /api/voice/{room}/join", s.handleVoiceJoin)
	mux.HandleFunc("POST /api/voice/{room}/mod", s.handleVoiceMod)
	mux.HandleFunc("POST /api/voice/agents/report", s.handleVoiceAgentReport)
	mux.HandleFunc("GET /api/admin/voice", s.requireMod(s.handleAdminVoice))
}
