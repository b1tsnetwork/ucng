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

	"ucng/internal/stt"
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
	ScreenQuality string   `json:"screenQuality,omitempty"` // text, standard, motion, high, max
	MaxScreens    int      `json:"maxScreens,omitempty"`    // gleichzeitige Freigaben
	// Kamera (Konferenz): "" (aus), "speakers" oder "roles" (Team und VideoRoles)
	Video        string   `json:"video,omitempty"`
	VideoRoles   []string `json:"videoRoles,omitempty"`
	VideoQuality string   `json:"videoQuality,omitempty"` // sd, hd, fhd, fhd60 (großes Bild)
	Featured     int      `json:"featured,omitempty"`     // so viele Sprecher groß (1–9)
	BotMusic     bool     `json:"botMusic,omitempty"`     // Bots dürfen Musik einspielen
	Captions     bool     `json:"captions,omitempty"`     // Untertitel über das eigene Whisper (STT_URL)
	E2EE         bool     `json:"e2ee,omitempty"`         // Ende-zu-Ende-verschlüsselt (Schlüssel nur in den Browsern)
	// Wer moderiert im Sprachkanal? "" = Team und Rollen mit dem Recht „Stummschalten“,
	// "team" = nur das Team, "roles" = nur ModRoles (dann auch das Team nur mit einer dieser Rollen).
	// Besitzer temporärer Kanäle und Co-Hosts moderieren immer.
	Mods     string   `json:"mods,omitempty"`
	ModRoles []string `json:"modRoles,omitempty"`
}

const (
	ModsDefault = ""
	ModsTeam    = "team"
	ModsRoles   = "roles"
)

var ErrModsMode = errors.New("Moderation: leer, team oder roles")

// videoPreset: Qualität des großen Kamerabildes; Miniansichten haben immer thumbPreset.
type videoPreset struct {
	Kbps   int `json:"kbps"`
	Width  int `json:"width"`
	Height int `json:"height"`
	FPS    int `json:"fps"`
}

var videoPresets = map[string]videoPreset{
	"sd":    {Kbps: 700, Width: 640, Height: 360, FPS: 30},
	"hd":    {Kbps: 1800, Width: 1280, Height: 720, FPS: 30},
	"fhd":   {Kbps: 3200, Width: 1920, Height: 1080, FPS: 30},
	"fhd60": {Kbps: 5000, Width: 1920, Height: 1080, FPS: 60},
}

var thumbPreset = videoPreset{Kbps: 180, Width: 320, Height: 180, FPS: 15}

// screenPreset: Qualität einer Bildschirmfreigabe.
type screenPreset struct {
	Kbps   int `json:"kbps"`
	FPS    int `json:"fps"`
	Height int `json:"height"` // höchstens so viele Zeilen (Breite folgt dem Seitenverhältnis)
}

// Mit Hardware-Kodierung (H.264, VP9 oder AV1 über die Grafikkarte) und zeitlichen Ebenen: Kommt
// ein Zuschauer nicht mit, bekommt er weniger Bilder je Sekunde statt ein stehendes Bild.
var screenPresets = map[string]screenPreset{
	"text":     {Kbps: 3000, FPS: 15, Height: 1440},  // Folien, Code, Dokumente: gestochen scharf
	"standard": {Kbps: 4500, FPS: 30, Height: 1080},  // Allgemein
	"motion":   {Kbps: 8000, FPS: 60, Height: 1080},  // Videos, Spiele: flüssig
	"high":     {Kbps: 12000, FPS: 60, Height: 1440}, // flüssig und scharf
	"max":      {Kbps: 16000, FPS: 30, Height: 2160}, // 4K
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
	ErrVideoMode    = errors.New("Kamera muss leer, speakers oder roles sein")
	ErrVideoPreset  = errors.New("Unbekannte Qualität für die Kamera")
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
	if v.E2EE { // der Server kennt den Schlüssel nicht: keine Musik von Bots, keine Untertitel
		v.BotMusic, v.Captions = false, false
	}
	switch v.Mods {
	case ModsDefault, ModsTeam:
		v.ModRoles = nil
	case ModsRoles:
		v.ModRoles = h.roles.Filter(v.ModRoles)
		if len(v.ModRoles) == 0 {
			v.ModRoles = nil
		}
	default:
		return nil, ErrModsMode
	}
	switch v.Screen {
	case "off":
		v.Screen = ScreenOff
	case ScreenOff, ScreenSpeakers, ScreenRoles:
	default:
		return nil, ErrScreenMode
	}
	switch v.Video {
	case "off":
		v.Video = ScreenOff
	case ScreenOff, ScreenSpeakers, ScreenRoles:
	default:
		return nil, ErrVideoMode
	}
	if v.Video == ScreenOff {
		v.VideoRoles, v.VideoQuality, v.Featured = nil, "", 0
	} else {
		if v.VideoQuality == "" {
			v.VideoQuality = "hd"
		}
		if _, ok := videoPresets[v.VideoQuality]; !ok {
			return nil, ErrVideoPreset
		}
		if v.Featured <= 0 {
			v.Featured = 1
		}
		v.Featured = min(v.Featured, 9)
		v.VideoRoles = h.roles.Filter(v.VideoRoles)
		if len(v.VideoRoles) == 0 {
			v.VideoRoles = nil
		}
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
	if v.Video != ScreenOff {
		s += fmt.Sprintf(", Kamera: %s (%s, %d groß)", v.Video, v.VideoQuality, v.Featured)
	}
	if v.BotMusic {
		s += ", Musik von Bots"
	}
	if v.Captions {
		s += ", Untertitel"
	}
	if v.E2EE {
		s += ", Ende-zu-Ende-verschlüsselt"
	}
	return s
}

func (v *VoiceConfig) preset() voicePreset { return voicePresets[v.Preset] }

func (v *VoiceConfig) screenPreset() screenPreset { return screenPresets[v.ScreenQuality] }

func (v *VoiceConfig) videoPreset() videoPreset { return videoPresets[v.VideoQuality] }

// voiceAccess: Einstellungen und Zugang eines Kanals für a.
func (h *Hub) voiceAccess(name string, a *Access) (cfg *VoiceConfig, exists, read, write bool) {
	acc := h.voiceAccessFull(name, a)
	return acc.Cfg, acc.Exists, acc.Read, acc.Write
}

type voiceRoomAccess struct {
	Cfg                      *VoiceConfig
	Exists, See, Read, Write bool
	Locked                   bool   // Passwort fehlt noch
	Owner                    string // Besitzer eines temporären Kanals
}

func (h *Hub) voiceAccessFull(name string, a *Access) voiceRoomAccess {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.kind != "" {
		return voiceRoomAccess{}
	}
	acc := voiceRoomAccess{Cfg: r.voice, Exists: true, See: r.canSee(a), Read: r.canRead(a), Write: r.canWrite(a), Locked: !r.passOK(a)}
	if r.temp != nil {
		acc.Owner = r.temp.Owner
	}
	return acc
}

// voiceRoomsFor: Sprachkanäle, die a sehen darf.
func (h *Hub) voiceRoomsFor(a *Access) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for name, r := range h.rooms {
		if r.kind == "" && r.voice != nil && r.canSee(a) {
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
	mediaRL  *rateLimiter      // Zutrittskarten für Medienverbindungen je IP
	client   *http.Client
	csp      atomic.Value // string: zusätzliche connect-src-Quellen (Agenten)
	stop     chan struct{}

	mu        sync.Mutex
	rooms     map[string]*voiceRoom
	agents    map[string]*voiceAgent
	breakouts map[string]*BreakoutInfo // Hauptkanal → laufende Breakout-Runde
	stt       *sttClient               // Untertitel (nil = aus)
	tr        *transcriptStore         // Live-Transkripte
	capRL     *rateLimiter             // Untertitel je Person (gegen Dauerfeuer)
}

type voiceRoom struct {
	relay    string // Relay, auf dem der Kanal liegt
	peers    []VoicePeer
	grants   map[string]bool      // Bühne: true = freigeschaltet, false = Rederecht entzogen
	muted    map[string]bool      // vom Team stummgeschaltet (bleibt beim erneuten Beitreten)
	kicked   map[string]time.Time // entfernt: Wiedereintritt erst danach
	noScreen map[string]bool      // vom Team: darf hier nicht (mehr) teilen
	noVideo  map[string]bool      // vom Team: Kamera hier gesperrt
	movedIn  map[string]time.Time // vom Team hierher verschoben: Zutritt bis (verlängert, solange drin)
	media    map[string]string    // Medienart (screen/video) → Relay (mediaroute.go)
	cohosts  map[string]bool      // Co-Hosts: moderieren hier, solange der Kanal belegt ist
}

type voiceAgent struct {
	ID         string              `json:"id"`
	WSURL      string              `json:"wsURL"`
	ControlURL string              `json:"-"`
	Stats      voicerelay.Stats    `json:"stats"`
	Seen       time.Time           `json:"seen"`
	Version    string              `json:"version,omitempty"`
	Role       string              `json:"role,omitempty"` // "media" = nur Bildschirm und Kameras
	STT        *stt.Stats          `json:"stt,omitempty"`  // eigene Spracherkennung des Agenten
	rooms      map[string]struct{} // Kanäle laut letzter Meldung
}

func perIP(n int) int {
	if n <= 0 {
		return 40
	}
	return n
}

func newVoiceMgr(s *Server) *voiceMgr {
	priv := s.hub.VoiceKey()
	v := &voiceMgr{
		s: s, priv: priv, pub: priv.Public().(ed25519.PublicKey), token: s.cfg.VoiceAgentToken,
		conns:    newStreamLimiter(max(s.cfg.VoiceMaxParticipants, 1)*4, perIP(s.cfg.VoiceMaxPerIP)), // je Person bis zu drei Verbindungen
		ticketRL: newRateLimiter(30, time.Minute),
		mediaRL:  newRateLimiter(90, time.Minute),
		client:   &http.Client{Timeout: 5 * time.Second},
		rooms:    map[string]*voiceRoom{}, agents: map[string]*voiceAgent{}, stop: make(chan struct{}),
		breakouts: map[string]*BreakoutInfo{}, stt: newSTT(s.cfg), tr: newTranscriptStore(s.cfg.DataDir), capRL: newRateLimiter(40, time.Minute),
	}
	v.csp.Store("")
	if !s.cfg.VoiceNoBuiltin {
		v.builtin = voicerelay.New(voicerelay.Config{
			PublicKey:       func() ed25519.PublicKey { return v.pub },
			MaxParticipants: s.cfg.VoiceMaxParticipants,
			MaxMbit:         s.cfg.VoiceMaxMbit,
			OnChange:        func(ch string, peers []voicerelay.PeerInfo) { v.setPeers(builtinRelay, ch, peers) },
			OnSegment:       v.onSegment,
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
		st = &voiceRoom{grants: map[string]bool{}, muted: map[string]bool{}, kicked: map[string]time.Time{}, noScreen: map[string]bool{},
			noVideo: map[string]bool{}, movedIn: map[string]time.Time{}, cohosts: map[string]bool{}}
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
	// Wer die Sprache verlassen hat, verliert auch die Medienverbindungen
	stay := map[string]bool{}
	for _, p := range peers {
		stay[p.User] = true
	}
	var left []string
	for _, p := range st.peers {
		if !stay[p.User] {
			left = append(left, p.User)
		}
	}
	mediaRelays := st.mediaRelays()
	if len(peers) == 0 {
		st.media = nil
	}
	defer func() {
		for _, rel := range mediaRelays {
			for _, u := range left {
				v.sendControl(rel, name, u, "mediakick", false, "left")
			}
		}
	}()
	st.peers = append([]VoicePeer(nil), peers...)
	for _, p := range peers { // Verschobene: Zutritt gilt, solange sie drin sind
		if _, ok := st.movedIn[p.User]; ok {
			st.movedIn[p.User] = time.Now().Add(2 * time.Minute)
		}
	}
	if len(peers) == 0 {
		clear(st.grants) // Freischaltungen der Bühne und Co-Hosts gelten, bis der Kanal leer ist
		clear(st.cohosts)
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
		v.mediaRL.cleanup()
		v.breakoutTick(time.Now())
		v.transcriptTick(time.Now())
		v.capRL.cleanup()
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
			for u, until := range st.movedIn {
				if now.After(until) {
					delete(st.movedIn, u)
				}
			}
			for u, until := range st.kicked {
				if now.After(until) {
					delete(st.kicked, u)
				}
			}
			if len(st.peers) == 0 && len(st.muted) == 0 && len(st.kicked) == 0 && len(st.noScreen) == 0 && len(st.noVideo) == 0 && len(st.movedIn) == 0 && len(st.cohosts) == 0 {
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
	Video  bool // darf die Kamera nutzen
	Mod    bool // moderiert hier (Team, Recht „Stummschalten“ oder Besitzer des temporären Kanals)
	Owner  bool
}

// decide: Darf w dem Sprachkanal beitreten, sprechen, ist w stummgeschaltet?
func (v *voiceMgr) decide(name string, w voiceWho) (voiceDecision, error) {
	var d voiceDecision
	acc := v.s.hub.voiceAccessFull(name, w.access())
	v.mu.Lock()
	moved := false
	if st := v.rooms[name]; st != nil {
		_, moved = st.movedIn[w.ID]
	}
	v.mu.Unlock()
	cfg, write := acc.Cfg, acc.Write || moved
	if !acc.Exists || (!acc.Read && !moved) {
		if acc.See && acc.Locked {
			return d, ErrRoomNeedsPass
		}
		return d, ErrNoRoom
	}
	if cfg == nil {
		return d, ErrNoVoice
	}
	d.Owner = acc.Owner != "" && acc.Owner == w.ID
	d.Mod = v.roomMod(cfg, w) || d.Owner
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
	d.Guest = w.guest() && !moved
	if d.Guest && cfg.Guests == VoiceGuestsNone {
		return d, ErrVoiceNoGuest
	}
	d.Speak = write && !w.GuestReadOnlyChat
	if d.Guest && cfg.Guests == VoiceGuestsListen {
		d.Speak = false
	}
	if cfg.Stage {
		d.Speak = w.Team || d.Owner || intersects(cfg.StageRoles, w.Roles)
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
		if st.cohosts[w.ID] {
			d.Mod = true
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
		d.Screen = d.Speak && !d.Muted && (w.Team || d.Owner || intersects(cfg.ScreenRoles, w.Roles))
	}
	switch cfg.Video {
	case ScreenSpeakers:
		d.Video = d.Speak && !d.Muted
	case ScreenRoles:
		d.Video = d.Speak && !d.Muted && (w.Team || d.Owner || intersects(cfg.VideoRoles, w.Roles))
	}
	if st := v.rooms[name]; st != nil {
		if st.noScreen[w.ID] {
			d.Screen = false
		}
		if st.noVideo[w.ID] {
			d.Video = false
		}
	}
	return d, nil
}

// roomMod: Moderiert w in einem Sprachkanal mit diesen Einstellungen? (ohne Besitzer und Co-Hosts)
func (v *voiceMgr) roomMod(cfg *VoiceConfig, w voiceWho) bool {
	if cfg == nil {
		return v.canModerate(w)
	}
	switch cfg.Mods {
	case ModsTeam:
		return w.Team
	case ModsRoles:
		return intersects(cfg.ModRoles, w.Roles)
	}
	return v.canModerate(w)
}

// canModerate: Team oder Rollen mit dem Chat-Recht „Stummschalten“ (Voreinstellung).
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
	if strings.HasPrefix(p.User, "bot:") { // Musik eines Bots: eigener Ablauf (music.go)
		return
	}
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
	if d.Video != p.CamOK {
		v.control(room, p.User, "video", d.Video, "")
	}
	if d.Mod != p.Mod {
		v.control(room, p.User, "mod", d.Mod, "")
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
	case errors.Is(err, ErrRoomNeedsPass):
		return "password"
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
		if v.tr.activeFor(oldName) != nil {
			v.tr.stop(oldName) // kein Hinweis: der Kanal ist weg
		}
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
	v.pushTap(oldName) // z. B. „Untertitel“ oder Ende-zu-Ende umgestellt
}

// --- Relays ---

// control schickt einen Steuerbefehl an den Relay des Kanals – und an die Relays seiner
// Medienverbindungen, soweit er sie betrifft.
func (v *voiceMgr) control(room, user, action string, value bool, reason string) {
	v.mu.Lock()
	relay := ""
	var media []string
	if st := v.rooms[room]; st != nil {
		relay = st.relay
		media = st.mediaRelays()
	}
	v.mu.Unlock()
	switch action {
	case "speak", "hand", "move", "remedia", "tap": // nur die Sprache
		media = nil
	}
	v.sendControl(relay, room, user, action, value, reason)
	for _, m := range media {
		if m != relay {
			v.sendControl(m, room, user, action, value, reason)
		}
	}
}

// sendControl schickt einen Steuerbefehl an einen bestimmten Relay.
func (v *voiceMgr) sendControl(relay, room, user, action string, value bool, reason string) {
	v.mu.Lock()
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
		if a := v.agents[id]; now.Sub(a.Seen) < voiceAgentStale && a.Role != "media" { // reine Medien-Relays bekommen keine Sprache
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
	case errors.As(err, &se), errors.Is(err, ErrVoiceNoGuest), errors.Is(err, ErrVoiceKicked), errors.Is(err, ErrRoomNeedsPass):
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
	pr, sp, vp := d.Cfg.preset(), d.Cfg.screenPreset(), d.Cfg.videoPreset()
	// Untertitel/Live-Transkription: der Relay schneidet mit, wenn er es kann
	tap, taps := voicerelay.TapOff, v.relayTaps(choice.id)
	if taps {
		tap = v.tapMode(room, &d.Cfg, choice.id)
	}
	lang := r.URL.Query().Get("lang") // Sprache der Oberfläche – Hinweis für die Spracherkennung
	if len(lang) > 8 || strings.ContainsAny(lang, " /&?") {
		lang = ""
	}
	acc := v.s.hub.voiceAccessFull(room, who.access())
	ticket := voicerelay.SignTicket(v.priv, voicerelay.Ticket{
		Channel: room, User: me.ID, Name: me.Name, Speak: d.Speak, Muted: d.Muted,
		Bitrate: pr.Bitrate, Stereo: pr.Stereo, MaxSpeakers: d.Cfg.MaxSpeakers, MaxPeers: d.Cfg.MaxPeers,
		Screen: d.Screen, ScreenKbps: sp.Kbps, MaxScreens: d.Cfg.MaxScreens,
		Video: d.Video, VideoKbps: vp.Kbps, ThumbKbps: thumbPreset.Kbps, Featured: d.Cfg.Featured, ModUser: d.Mod,
		Tap: tap, Lang: lang,
	}, time.Now())
	u := choice.url
	if u == "" {
		u = "/api/voice/ws"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url": u, "ticket": ticket, "preset": d.Cfg.Preset, "bitrate": pr.Bitrate, "stereo": pr.Stereo, "music": pr.Music,
		"maxSpeakers": d.Cfg.MaxSpeakers, "maxPeers": d.Cfg.MaxPeers, "speak": d.Speak, "muted": d.Muted,
		"stage": d.Cfg.Stage, "guest": d.Guest, "moderate": d.Mod,
		"screen": d.Screen, "screenOn": d.Cfg.Screen != ScreenOff, "screenKbps": sp.Kbps, "screenFps": sp.FPS, "screenHeight": sp.Height,
		"video": d.Video, "videoOn": d.Cfg.Video != ScreenOff, "videoPreset": vp, "thumbPreset": thumbPreset,
		"featured": d.Cfg.Featured, "owner": d.Owner,
		"host":     d.Owner || v.roomMod(&d.Cfg, who), // darf Co-Hosts ernennen
		"e2ee":     d.Cfg.E2EE,
		"captions": d.Cfg.Captions && !d.Cfg.E2EE && v.sttFor(choice.id) && taps,
		// Live-Transkription: läuft gerade (alle sehen den Hinweis) bzw. darf ich sie starten
		"transcript":   v.tr.activeFor(room),
		"transcribeOK": taps && v.transcriptAllowed(acc, choice.id) && v.mayHost(room, me),
		"breakout":     v.breakoutFor(room),
		"mediaConn":    v.mediaCapable(), // Bildschirm und Kameras über eigene Verbindungen (/media)
	})
	_ = who
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
	acc := s.hub.voiceAccessFull(room, who.access())
	if !acc.Exists || !acc.Read || acc.Cfg == nil {
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
	host := v.roomMod(acc.Cfg, who) || (acc.Owner != "" && acc.Owner == me.ID) // laut Einstellung des Kanals, oder Besitzer
	v.mu.Lock()
	cohost := v.rooms[room] != nil && v.rooms[room].cohosts[me.ID]
	v.mu.Unlock()
	canMod := host || cohost
	if in.Action != "lowerhand" && !canMod || in.Action == "lowerhand" && in.UserID != me.ID && !canMod ||
		(in.Action == "cohost" || in.Action == "uncohost") && !host { // Co-Hosts ernennt nur, wer selbst Gastgeber ist
		fail(w, ErrNoPerm)
		return
	}
	actor, display := chatActor(me)
	// Befehle für den ganzen Kanal
	switch in.Action {
	case "muteall", "camsoff", "endall", "recstart", "recstop":
		switch in.Action {
		case "muteall": // alle außer Moderierenden; sie können sich selbst wieder einschalten
			v.control(room, "", "forcemute", false, "")
		case "camsoff":
			v.control(room, "", "camsoff", false, "")
		case "endall":
			v.control(room, "", "close", false, "ended")
		case "recstart", "recstop": // eigene Aufnahme: alle im Kanal sehen den Hinweis
			v.control(room, me.ID, "rec", in.Action == "recstart", "")
		}
		s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice." + in.Action, Room: room})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	target, ok := s.users.Get(in.UserID)
	if !ok {
		fail(w, ErrNoUser)
		return
	}
	if in.UserID != me.ID && (target.Team || s.hub.Roles().PermsOf(target.Roles, false).Mute) && !me.Team &&
		in.Action != "speak" && in.Action != "lowerhand" && in.Action != "askunmute" && in.Action != "cohost" {
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
	case "stopvideo":
		st.noVideo[in.UserID] = true
	case "allowvideo":
		delete(st.noVideo, in.UserID)
	case "cohost":
		st.cohosts[in.UserID] = true
	case "uncohost":
		delete(st.cohosts, in.UserID)
	case "askunmute", "lowerhand":
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
	case "askunmute":
		v.control(room, in.UserID, "askunmute", false, "")
	}
	if in.Action != "kick" && in.Action != "lowerhand" && in.Action != "askunmute" { // Rechte hängen an Sprechen/Stumm/Co-Host
		if tw, ok := v.whoByID(in.UserID); ok {
			if d, err := v.decide(room, tw); err == nil {
				v.control(room, in.UserID, "screen", d.Screen, "")
				v.control(room, in.UserID, "video", d.Video, "")
				v.control(room, in.UserID, "mod", d.Mod, "")
			}
		}
	}
	if in.Action != "lowerhand" && in.Action != "askunmute" {
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
	Role       string                 `json:"role"`
	STT        *stt.Stats             `json:"stt,omitempty"`
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
	a.WSURL, a.ControlURL, a.Stats, a.Seen, a.Version, a.STT = in.WSURL, in.ControlURL, in.Stats, time.Now(), in.Version, in.STT
	a.Role = ""
	if in.Role == "media" {
		a.Role = "media"
	}
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
	type roomUse struct {
		Room  string   `json:"room"`
		Peers int      `json:"peers,omitempty"`
		Kinds []string `json:"kinds"` // voice, screen, video – was dieser Relay für den Kanal übernimmt
	}
	type relayInfo struct {
		ID        string           `json:"id"`
		Builtin   bool             `json:"builtin,omitempty"`
		URL       string           `json:"url,omitempty"`
		Version   string           `json:"version,omitempty"`
		Healthy   bool             `json:"healthy"`
		Role      string           `json:"role,omitempty"`
		Seen      *time.Time       `json:"seen,omitempty"`
		Stats     voicerelay.Stats `json:"stats"`
		Functions []string         `json:"functions"`     // voice, media, tap, stt (eigene), stt-chat (über den Chat-Server)
		STT       *stt.Stats       `json:"stt,omitempty"` // eigene Spracherkennung
		Rooms     []roomUse        `json:"rooms"`
	}
	has := func(list []string, f string) bool {
		for _, x := range list {
			if x == f {
				return true
			}
		}
		return false
	}
	funcs := func(role string, features []string, own bool) []string {
		var out []string
		if role != "media" {
			out = append(out, "voice")
		}
		if has(features, "media") {
			out = append(out, "media")
		}
		if role != "media" && has(features, "tap") {
			out = append(out, "tap")
			if own {
				out = append(out, "stt")
			} else if v.stt != nil {
				out = append(out, "stt-chat")
			}
		}
		return out
	}
	relays := []relayInfo{}
	if v.builtin != nil {
		st := v.builtin.Stats()
		ri := relayInfo{ID: builtinRelay, Builtin: true, Healthy: true, Stats: st, Functions: funcs("", st.Features, false)}
		relays = append(relays, ri)
	}
	rooms := map[string]int{}
	v.mu.Lock()
	for _, a := range v.agents {
		seen := a.Seen
		ri := relayInfo{ID: a.ID, URL: a.WSURL, Version: a.Version, Seen: &seen, Role: a.Role,
			Healthy: time.Since(a.Seen) < voiceAgentStale, Stats: a.Stats, STT: a.STT, Functions: funcs(a.Role, a.Stats.Features, a.STT != nil)}
		relays = append(relays, ri)
	}
	use := map[string][]roomUse{}
	for name, st := range v.rooms {
		if len(st.peers) == 0 {
			continue
		}
		rooms[name] = len(st.peers)
		per := map[string]*roomUse{}
		add := func(relay, kind string) {
			if per[relay] == nil {
				per[relay] = &roomUse{Room: name}
			}
			per[relay].Kinds = append(per[relay].Kinds, kind)
		}
		add(st.relay, "voice")
		per[st.relay].Peers = len(st.peers)
		for _, kind := range []string{voicerelay.KindScreen, voicerelay.KindVideo} {
			if id := st.media[kind]; id != "" {
				add(id, kind)
			}
		}
		for id, u := range per {
			use[id] = append(use[id], *u)
		}
	}
	v.mu.Unlock()
	for i := range relays {
		relays[i].Rooms = use[relays[i].ID]
		if relays[i].Rooms == nil {
			relays[i].Rooms = []roomUse{}
		}
		sort.Slice(relays[i].Rooms, func(a, b int) bool { return relays[i].Rooms[a].Room < relays[i].Rooms[b].Room })
	}
	sort.Slice(relays, func(i, j int) bool { return relays[i].Builtin || !relays[j].Builtin && relays[i].ID < relays[j].ID })
	out := map[string]any{"enabled": true, "relays": relays, "rooms": rooms,
		"agents": v.token != "", "builtin": v.builtin != nil, "stt": v.stt != nil || v.anyAgentSTT()}
	if v.stt != nil {
		cs := v.stt.Stats()
		out["chatSTT"] = cs // Spracherkennung beim Chat-Server
	}
	writeJSON(w, http.StatusOK, out)
}

// voiceRoutes meldet die Pfade der Sprachkanäle an.
func (s *Server) voiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/voice", s.handleVoiceList)
	mux.HandleFunc("GET /api/voice/ws", s.handleVoiceWS)
	mux.HandleFunc("POST /api/voice/{room}/join", s.handleVoiceJoin)
	mux.HandleFunc("POST /api/voice/{room}/mod", s.handleVoiceMod)
	mux.HandleFunc("POST /api/voice/{room}/move", s.handleVoiceMove)
	mux.HandleFunc("POST /api/voice/{room}/media", s.handleVoiceMedia)
	mux.HandleFunc("POST /api/voice/{room}/breakout", s.handleVoiceBreakout)
	mux.HandleFunc("DELETE /api/voice/{room}/breakout", s.handleVoiceBreakout)
	mux.HandleFunc("POST /api/voice/{room}/transcript", s.handleVoiceTranscript)
	mux.HandleFunc("GET /api/rooms/{room}/transcripts", s.handleRoomTranscripts)
	mux.HandleFunc("GET /api/transcripts/{id}", s.handleTranscriptGet)
	mux.HandleFunc("POST /api/voice/agents/report", s.handleVoiceAgentReport)
	mux.HandleFunc("POST /api/voice/agents/segment", s.handleVoiceAgentSegment)
	mux.HandleFunc("POST /api/voice/agents/text", s.handleVoiceAgentText)
	mux.HandleFunc("GET /api/admin/voice", s.requireMod(s.handleAdminVoice))
}

// POST /api/voice/{room}/move {"userId": "…", "to": "kanal"} – Person in einen anderen Sprachkanal
// schicken (Team und Recht „Stummschalten“). Sie darf dort hinein, auch ohne Zugang, solange sie drin ist.
func (s *Server) handleVoiceMove(w http.ResponseWriter, r *http.Request) {
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
	var in struct {
		UserID string `json:"userId"`
		To     string `json:"to"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	room, to := NormRoom(r.PathValue("room")), NormRoom(in.To)
	if !v.mayHost(room, me) { // verschieben darf, wer im Ausgangskanal moderiert
		fail(w, ErrNoPerm)
		return
	}
	if room == to {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if acc := s.hub.voiceAccessFull(to, who.access()); !acc.Exists || acc.Cfg == nil {
		voiceFail(w, ErrNoVoice)
		return
	}
	target, ok := s.users.Get(in.UserID)
	if !ok {
		fail(w, ErrNoUser)
		return
	}
	if in.UserID != me.ID && (target.Team || s.hub.Roles().PermsOf(target.Roles, false).Mute) && !me.Team {
		writeErr(w, http.StatusForbidden, "Team-Mitglieder und andere Moderierende kann nur das Team verschieben")
		return
	}
	present := false
	for _, p := range v.Peers(room) {
		present = present || p.User == in.UserID
	}
	if !present {
		writeErr(w, http.StatusNotFound, "Diese Person ist nicht in diesem Sprachkanal")
		return
	}
	v.mu.Lock()
	v.room(to).movedIn[in.UserID] = time.Now().Add(2 * time.Minute)
	v.mu.Unlock()
	v.control(room, in.UserID, "move", false, to)
	actor, display := chatActor(me)
	s.hub.AuditLog().Add(AuditEntry{Actor: actor, Display: display, Action: "voice.move", Room: to,
		UserID: in.UserID, UserName: target.Name, Detail: "aus #" + room})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
