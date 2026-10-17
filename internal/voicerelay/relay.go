// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package voicerelay leitet Sprache in Sprachkanälen weiter (SFU-Prinzip): Browser schicken
// Opus-Rahmen über einen WebSocket, der Relay verteilt sie an alle anderen im Kanal – ohne
// sie zu entpacken, zu mischen oder zu speichern. Gemischt wird im Browser.
//
// Der Relay läuft eingebaut im Chat-Server oder als eigenes Programm (cmd/ucng-voice). Über
// Rechte entscheidet immer der Chat-Server: Er stellt signierte Zutrittskarten (Ticket) aus und
// schickt Steuerbefehle (sprechen erlauben, stummschalten, entfernen).
//
// Skalierung: Je Kanal werden nur die gleichzeitig sprechenden Personen weitergeleitet (höchstens
// MaxSpeakers). Alle Rahmen eines 20-ms-Takts gehen gebündelt in EINER Nachricht an jeden Zuhörer.
// Der Aufwand wächst damit linear mit den Zuhörern.
package voicerelay

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ucng/internal/wsock"
)

// Protokoll (Binär, Big Endian):
//
//	Browser → Relay  [1][seq u16][level u8][Opus-Rahmen]            (ein 20-ms-Rahmen)
//	Relay → Browser  [1][n u8] n × ([Teilnehmer u16][seq u16][Länge u16][Opus-Rahmen])
//	Browser → Relay  [2][flags u8][seq u16][Video-Rahmen]           (Bildschirm; flags&1 = Schlüsselbild)
//	Relay → Browser  [2][Teilnehmer u16][flags u8][seq u16][Video-Rahmen]
//	Browser → Relay  [3][Ebene u8][flags u8][seq u16][Video-Rahmen]  (Kamera; Ebene 0 = Miniansicht, 1 = groß)
//	Relay → Browser  [3][Teilnehmer u16][Ebene u8][flags u8][seq u16][Video-Rahmen]
//
// Kamera (Konferenz): Jede Kamera schickt eine kleine Miniansicht an alle. Die „Featured“
// zuletzt sprechenden Personen schicken zusätzlich ein großes Bild – der Relay sagt ihnen, wann
// ({"t":"layer","high":…}). Wird eine Verbindung zu langsam, bekommt sie vorübergehend nur die
// Miniansichten.
//
// Steuerung als JSON-Text: zuerst {"t":"auth","ticket":"…"}; danach {"t":"state","mute":…,"hand":…},
// {"t":"ping","ts":…}, {"t":"share","on":…,"codec":"vp8"}, {"t":"watch","i":…,"on":…} und
// {"t":"keyreq","i":…}. Der Relay schickt {"t":"hello"}, {"t":"peer"}, {"t":"leave"}, {"t":"you"},
// {"t":"pong"}, {"t":"share"}, {"t":"watch"}, {"t":"keyreq"} und {"t":"bye"}.
//
// Bildschirmfreigabe: Video geht nur an Personen, die zuschauen („watch“) – die Bandbreite
// entsteht also nur, wo jemand hinsieht. Neue Zuschauer und verlorene Rahmen lösen eine
// Anfrage nach einem Schlüsselbild beim Teilenden aus.

const (
	MsgAudio      = 1
	MsgVideo      = 2
	MsgCam        = 3
	MsgScreenTone = 4               // Ton einer Bildschirmfreigabe: Browser → Relay [4][seq u16][Opus], Relay → Zuschauer [4][Teilnehmer u16][seq u16][Opus]
	featureHold   = 2 * time.Second // ein großes Bild bleibt mindestens so lange
	degradeFor    = 15 * time.Second
	maxVideoFrame = 4 << 20 // größtes Video-Bild (Schlüsselbild in 4K)
	videoQueue    = 512     // Video-Nachrichten je Verbindung (die Grenze sind die Bytes, siehe vBudget)
	minVBudget    = 384 << 10
	videoLatency  = 0.3 // so viele Sekunden Video dürfen höchstens in der Warteschlange stehen
	keyReqGap     = time.Second
	maxFrame      = 1275 // größter Opus-Rahmen
	maxToneFrame  = 1500 // Ton der Bildschirmfreigabe (Opus, Stereo)
	toneKbps      = 160
	tick          = 20 * time.Millisecond
	activeTimeout = 400 * time.Millisecond
	outQueue      = 64
	maxDrops      = 150 // so viele Nachrichten in Folge verworfen → zu langsame Verbindung trennen
)

// Lebenszeichen (Variablen für Tests): Der Relay pingt alle pingEvery; kommt readTimeout lang
// gar nichts (auch kein Pong), gilt die Verbindung als tot.
var (
	readTimeout = 30 * time.Second
	pingEvery   = 15 * time.Second
)

var (
	ErrFull        = errors.New("Der Sprachkanal ist voll")
	ErrServerFull  = errors.New("Der Sprach-Server ist gerade ausgelastet – bitte später noch einmal")
	ErrTicketReuse = errors.New("Diese Zutrittskarte wurde schon verwendet")
)

// Config des Relays.
type Config struct {
	PublicKey       func() ed25519.PublicKey // Sprach-Schlüssel des Chat-Servers
	MaxParticipants int                      // je Relay
	MaxMbit         float64                  // geschätzte ausgehende Bandbreite je Relay
	OriginOK        func(origin string) bool // nil = jede Herkunft
	// OnChange meldet die Teilnehmer eines Kanals nach jeder Änderung (außerhalb der Sperre).
	OnChange func(channel string, peers []PeerInfo)
	// OnSegment bekommt Sprach-Abschnitte für die Spracherkennung (tap.go), wenn der Mitschnitt
	// eines Kanals an ist. Wird in einer eigenen Goroutine aufgerufen.
	OnSegment func(Segment)
	Now       func() time.Time
}

// PeerInfo: ein Teilnehmer, wie ihn der Chat-Server anzeigt.
type PeerInfo struct {
	Idx      uint16 `json:"i"`
	User     string `json:"u"`
	Name     string `json:"n"`
	Speak    bool   `json:"s"`           // darf sprechen
	Muted    bool   `json:"m,omitempty"` // vom Team stummgeschaltet
	SelfMute bool   `json:"sm,omitempty"`
	Hand     bool   `json:"h,omitempty"`
	HandAt   int64  `json:"ha,omitempty"`  // seit wann (Unix-Millisekunden) – für die Reihenfolge der Meldungen
	Screen   string `json:"sc,omitempty"`  // teilt den Bildschirm (Codec, z. B. "vp8")
	ScreenOK bool   `json:"so,omitempty"`  // darf den Bildschirm teilen
	Cam      string `json:"cm,omitempty"`  // Kamera an (Codec)
	CamOK    bool   `json:"co,omitempty"`  // darf die Kamera nutzen
	Music    int    `json:"mu,omitempty"`  // Musik eines Bots (Lautstärke in Prozent)
	Mod      bool   `json:"mo,omitempty"`  // moderiert hier
	Quality  int    `json:"q,omitempty"`   // Verbindung laut Browser: 1 schlecht, 2 mittel, 3 gut (0 = unbekannt)
	Rec      bool   `json:"rec,omitempty"` // nimmt gerade auf (vom Chat-Server gesetzt)
	PubKey   string `json:"pk,omitempty"`  // Ende-zu-Ende: öffentlicher Schlüssel dieser Sitzung (ECDH P-256)
}

// Stats: Auslastung des Relays.
type Stats struct {
	Participants int            `json:"participants"`
	Channels     map[string]int `json:"channels"`
	Mbit         float64        `json:"mbit"`         // gemessen (gleitender Mittelwert)
	EstimateMbit float64        `json:"estimateMbit"` // geschätzt aus den Voreinstellungen
	MaxMbit      float64        `json:"maxMbit"`
	Max          int            `json:"max"`
	MediaPeers   int            `json:"mediaPeers,omitempty"` // Medienverbindungen (Bildschirm/Kamera)
	// Media: Medienkanäle („Kanal\x00screen“ bzw. „Kanal\x00video“) mit Teilnehmern und Sendenden
	Media    map[string]MediaStat `json:"media,omitempty"`
	Features []string             `json:"features,omitempty"`
}

// MediaStat: ein Medienkanal auf diesem Relay.
type MediaStat struct {
	Peers int     `json:"peers"`
	Pub   int     `json:"pub"`  // teilen den Bildschirm bzw. haben die Kamera an
	Mbit  float64 `json:"mbit"` // geschätzt
}

// MediaKey: Schlüssel eines Medienkanals.
func MediaKey(channel, kind string) string { return channel + "\x00" + kind }

type Relay struct {
	cfg      Config
	mu       sync.Mutex
	channels map[string]*channel
	nonces   map[string]time.Time
	total    int // Sprachverbindungen
	media    int // Medienverbindungen
	bytesOut atomic.Int64
	mbit     atomic.Uint64 // float64-Bits
	stop     chan struct{}
	stopOnce sync.Once
}

type channel struct {
	name        string // Schlüssel in Relay.channels
	room        string // Name des Chat-Kanals
	kind        string // "" = Sprache, sonst KindScreen/KindVideo
	peers       map[uint16]*peer
	next        uint16
	maxSpeakers int
	maxPeers    int
	bitrate     int
	maxScreens  int
	screenKbps  int
	featuredN   int
	videoKbps   int
	thumbKbps   int
	featured    []uint16 // große Bilder (Reihenfolge)
	featAt      time.Time
	active      map[uint16]*peer // gerade weitergeleitete Sprecher
	frames      []frameRef       // im laufenden Takt eingegangen
	ticking     bool
	tap         int // Mitschnitt für die Spracherkennung (TapOff, TapCaptions, TapTranscript)
}

type frameRef struct {
	from *peer
	seq  uint16
	data []byte
}

type peer struct {
	idx      uint16
	ch       *channel
	user     string
	name     string
	speak    bool
	muted    bool
	selfMute bool
	hand     bool
	handAt   int64 // Unix-Millisekunden der Meldung (0 = keine)
	bitrate  int
	conn     *wsock.Conn
	out      chan []byte // Steuerung und Sprache (Vorrang)
	vout     chan []byte // Video
	vq       atomic.Int64
	vBudget  int64 // so viele Bytes Video dürfen höchstens anstehen
	drops    int
	last     time.Time // letzter Rahmen
	level    uint8
	tokens   float64 // Rahmen
	bytesTok float64 // Bytes
	refill   time.Time
	gone     bool
	closing  bool
	// Bildschirm
	screenOK  bool             // darf teilen
	codec     string           // teilt gerade (Codec), sonst leer
	watchers  map[uint16]*peer // wer meinem Bildschirm zusieht
	watching  map[uint16]bool  // wem ich zusehe
	needKey   map[uint16]bool  // je Teilende: auf Schlüsselbild warten
	skipTid   map[uint16]uint8 // je Teilende: Zeitebenen ab dieser auslassen (bis eine niedrigere kommt)
	vBytes    float64          // Video-Kontingent (Bytes)
	vRefill   time.Time
	lastKeyRq time.Time
	// Kamera
	videoOK    bool
	cam        string              // Kamera an (Codec)
	camNeedKey map[uint16]*[2]bool // je Kamera und Ebene: auf Schlüsselbild warten
	camSkip    map[uint16]*[2]uint8
	camKeyRq   [2]time.Time
	cBytes     float64
	cRefill    time.Time
	lastSpoke  time.Time
	highOff    bool // zu langsam: vorübergehend nur Miniansichten
	degradedAt time.Time
	music      int // Musik-Datenstrom eines Bots (Lautstärke)
	mod        bool
	// Hinweise der Sprachverbindung, wenn Bildschirm/Kamera über eine Medienverbindung laufen
	hintScreen string
	hintCam    string
	// Sparmodus dieser Person als Zuschauer: pausiert (kein Video) bzw. obere Zeitebenen weglassen
	paused    bool
	drop      uint8
	resumedAt time.Time
	topTid    uint8 // als Sendende: höchste bisher gesehene Zeitebene
	lastSpk   time.Time
	quality   int
	rec       bool
	pubKey    string
	pins      map[uint16]bool // Kamera: diese Personen groß sehen (angeheftet, Galerie)
	highSent  bool            // Kamera: dem Browser gesagt, er soll das große Bild senden
	lastReact time.Time
	ctlTok    float64 // Kontingent für Reaktionen, Untertitel, Schlüssel
	ctlRefill time.Time
	tBytes    float64 // Ton der Bildschirmfreigabe
	tRefill   time.Time
	tap       *tapBuf // laufender Sprach-Abschnitt (Mitschnitt)
	noCap     bool    // möchte nicht verschriftlicht werden (gilt nur für Untertitel)
	lang      string  // Sprache laut Zutrittskarte
}

// New startet einen Relay.
func New(cfg Config) *Relay {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxParticipants <= 0 {
		cfg.MaxParticipants = 200
	}
	if cfg.MaxMbit <= 0 {
		cfg.MaxMbit = 50
	}
	r := &Relay{cfg: cfg, channels: map[string]*channel{}, nonces: map[string]time.Time{}, stop: make(chan struct{})}
	go r.meter()
	go r.tapLoop()
	return r
}

// Close beendet alle Verbindungen.
func (r *Relay) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	var all []*peer
	for _, ch := range r.channels {
		for _, p := range ch.peers {
			all = append(all, p)
		}
	}
	r.mu.Unlock()
	for _, p := range all {
		p.conn.CloseWith(1001, "Server wird beendet")
	}
}

// meter misst die tatsächlich gesendete Bandbreite (gleitender Mittelwert über ~5 s) und
// räumt verbrauchte Zutrittskarten auf.
func (r *Relay) meter() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	ema := 0.0
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			b := float64(r.bytesOut.Swap(0)) * 8 / 1e6
			ema = ema*0.8 + b*0.2
			r.mbit.Store(mathFloat64bits(ema))
			now := r.cfg.Now()
			r.mu.Lock()
			for n, exp := range r.nonces {
				if now.After(exp) {
					delete(r.nonces, n)
				}
			}
			r.mu.Unlock()
		}
	}
}

// Stats liefert die Auslastung.
func (r *Relay) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Stats{Participants: r.total, Channels: map[string]int{}, Mbit: mathFloat64frombits(r.mbit.Load()),
		MaxMbit: r.cfg.MaxMbit, Max: r.cfg.MaxParticipants, EstimateMbit: r.estimateLocked(), MediaPeers: r.media,
		Features: []string{"media", "tap"}}
	for n, ch := range r.channels {
		if ch.kind == "" {
			st.Channels[n] = len(ch.peers)
			continue
		}
		if st.Media == nil {
			st.Media = map[string]MediaStat{}
		}
		ms := MediaStat{Peers: len(ch.peers), Mbit: r.channelKbps(ch) / 1000}
		for _, p := range ch.peers {
			if p.codec != "" || p.cam != "" {
				ms.Pub++
			}
		}
		st.Media[n] = ms
	}
	return st
}

func (r *Relay) estimateLocked() float64 {
	total := 0.0
	for _, ch := range r.channels {
		total += r.channelKbps(ch)
	}
	return total / 1000
}

// channelKbps: geschätzte ausgehende Bandbreite eines Kanals in kbit/s.
func (r *Relay) channelKbps(ch *channel) float64 {
	total := 0.0
	{
		if ch.kind == "" {
			total += float64(len(ch.peers)) * EstimateKbps(ch.bitrate, ch.maxSpeakers)
		}
		for _, p := range ch.peers { // Bildschirm: je Zuschauer die Bitrate der Freigabe
			if p.codec != "" { // Bild und Ton der Freigabe
				total += float64(len(p.watchers)) * (float64(ch.screenKbps) + toneKbps) * 1.05
			}
			if p.cam != "" { // Miniansicht an alle, großes Bild an alle, die nicht gedrosselt sind
				total += float64(len(ch.peers)-1) * float64(ch.thumbKbps) * 1.05
				if p.highSent {
					full := 0.0
					for _, q := range ch.peers {
						if q != p && !q.highOff && ch.seesHigh(q, p) {
							full++
						}
					}
					total += full * float64(ch.videoKbps) * 1.05
				}
			}
			if p.music > 0 {
				total += float64(len(ch.peers)-1) * float64(p.bitrate) / 1000 * 1.1
			}
		}
	}
	return total
}

// Peers liefert die Teilnehmer eines Kanals.
func (r *Relay) Peers(channel string) []PeerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peersLocked(r.channels[channel])
}

func (r *Relay) peersLocked(ch *channel) []PeerInfo {
	out := []PeerInfo{}
	if ch == nil {
		return out
	}
	for _, p := range ch.peers {
		out = append(out, p.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
}

// setHand: melden bzw. Meldung zurücknehmen; der Zeitpunkt bleibt bei erneutem Melden erhalten.
func (p *peer) setHand(v bool, now time.Time) {
	if v && !p.hand {
		p.handAt = now.UnixMilli()
	} else if !v {
		p.handAt = 0
	}
	p.hand = v
}

func (p *peer) info() PeerInfo {
	sc, cm := p.codec, p.cam
	if sc == "" {
		sc = p.hintScreen
	}
	if cm == "" {
		cm = p.hintCam
	}
	return PeerInfo{Idx: p.idx, User: p.user, Name: p.name, Speak: p.speak, Muted: p.muted, SelfMute: p.selfMute, Hand: p.hand, HandAt: p.handAt, Screen: sc, ScreenOK: p.screenOK,
		Cam: cm, CamOK: p.videoOK, Music: p.music, Mod: p.mod, Quality: p.quality, Rec: p.rec, PubKey: p.pubKey}
}

// --- Verbindung ---

type ctlMsg struct {
	T      string   `json:"t"`
	Ticket string   `json:"ticket,omitempty"`
	Mute   *bool    `json:"mute,omitempty"`
	Hand   *bool    `json:"hand,omitempty"`
	TS     int64    `json:"ts,omitempty"`
	On     *bool    `json:"on,omitempty"`
	I      uint16   `json:"i,omitempty"`
	L      int      `json:"l,omitempty"`
	Codec  string   `json:"codec,omitempty"`
	Sc     *string  `json:"sc,omitempty"`    // state: Hinweis „teile den Bildschirm“ (über die Medienverbindung)
	Cm     *string  `json:"cm,omitempty"`    // state: Hinweis „Kamera an“
	Q      *int     `json:"q,omitempty"`     // state: Verbindungsqualität 1–3
	PK     *string  `json:"pk,omitempty"`    // state: öffentlicher Schlüssel (Ende-zu-Ende)
	NC     *bool    `json:"nc,omitempty"`    // state: „meine Sprache nicht verschriftlichen“ (nur Untertitel)
	E      string   `json:"e,omitempty"`     // react: Emoji
	X      string   `json:"x,omitempty"`     // e2k: verpackter Schlüssel
	To     uint16   `json:"to,omitempty"`    // e2k: Empfänger
	Ep     int      `json:"ep,omitempty"`    // e2k: Schlüssel-Generation
	IDs    []uint16 `json:"ids,omitempty"`   // pins
	Pause  *bool    `json:"pause,omitempty"` // limit: gerade kein Video (Tab versteckt, Ansicht zu)
	Drop   *int     `json:"drop,omitempty"`  // limit: so viele obere Zeitebenen weglassen (0–2: alle, ½, ¼ der Bilder)
}

// ServeHTTP nimmt eine Sprachverbindung an (GET, WebSocket).
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	conn, err := wsock.Upgrade(w, req, r.cfg.OriginOK, maxVideoFrame+16)
	if err != nil {
		return
	}
	r.ServeConn(conn)
}

// ServeConn betreut eine schon geöffnete Verbindung (z. B. Musik eines Bots im selben Prozess).
func (r *Relay) ServeConn(conn *wsock.Conn) {
	conn.SetReadDeadline(r.cfg.Now().Add(10 * time.Second))
	op, data, err := conn.Read()
	if err != nil || op != wsock.OpText {
		conn.CloseWith(4400, "Anmeldung fehlt")
		return
	}
	var m ctlMsg
	if json.Unmarshal(data, &m) != nil || m.T != "auth" {
		conn.CloseWith(4400, "Anmeldung fehlt")
		return
	}
	var pub ed25519.PublicKey
	if r.cfg.PublicKey != nil {
		pub = r.cfg.PublicKey()
	}
	t, err := VerifyTicket(pub, m.Ticket, r.cfg.Now())
	if err != nil {
		conn.CloseWith(4401, err.Error())
		return
	}
	p, err := r.join(t, conn)
	if err != nil {
		conn.CloseWith(4409, err.Error())
		return
	}
	go r.writer(p)
	r.reader(p)
}

// join nimmt eine Person in den Kanal auf (Grenzen geprüft); eine frühere Verbindung derselben
// Person auf diesem Relay wird beendet (eine Sprachverbindung je Person).
func (r *Relay) join(t Ticket, conn *wsock.Conn) (*peer, error) {
	now := r.cfg.Now()
	r.mu.Lock()
	if _, used := r.nonces[t.Nonce]; used {
		r.mu.Unlock()
		return nil, ErrTicketReuse
	}
	r.nonces[t.Nonce] = time.Unix(t.Exp, 0).Add(time.Minute)
	key := t.Channel
	if t.Kind != "" {
		key = MediaKey(t.Channel, t.Kind)
	}
	var replaced []*peer
	for _, ch := range r.channels {
		if ch.kind != t.Kind || (t.Kind != "" && ch.name != key) { // Sprache: eine je Person; Medien: eine je Kanal und Art
			continue
		}
		for _, p := range ch.peers {
			if p.user == t.User {
				replaced = append(replaced, p)
			}
		}
	}
	for _, p := range replaced { // vor den Grenzen: ein Umzug ist kein zusätzlicher Platz
		r.removeLocked(p)
	}
	ch := r.channels[key]
	if ch == nil {
		ch = &channel{name: key, room: t.Channel, kind: t.Kind, peers: map[uint16]*peer{}, active: map[uint16]*peer{}}
	}
	if t.Music == 0 || len(ch.peers) == 0 { // Musik eines Bots ändert die Einstellungen des Kanals nicht
		ch.maxSpeakers, ch.maxPeers, ch.bitrate = clamp(t.MaxSpeakers, 1, 16), clamp(t.MaxPeers, 1, 5000), clamp(t.Bitrate, 6000, 256000)
		if t.Kind == "" {
			r.setTapLocked(ch, t.Tap) // die Zutrittskarte kennt den aktuellen Stand
		}
		ch.maxScreens, ch.screenKbps = clamp(t.MaxScreens, 0, 16), clamp(t.ScreenKbps, 100, 20000)
		ch.featuredN, ch.videoKbps, ch.thumbKbps = clamp(t.Featured, 1, 9), clamp(t.VideoKbps, 100, 20000), clamp(t.ThumbKbps, 50, 2000)
	}
	if len(ch.peers) >= ch.maxPeers {
		r.mu.Unlock()
		r.notifyReplaced(replaced)
		return nil, ErrFull
	}
	if t.Kind == "" && (r.total >= r.cfg.MaxParticipants || r.estimateLocked()+EstimateKbps(ch.bitrate, ch.maxSpeakers)/1000 > r.cfg.MaxMbit) ||
		t.Kind != "" && (r.media >= r.cfg.MaxParticipants*3 || r.estimateLocked() > r.cfg.MaxMbit) {
		r.mu.Unlock()
		r.notifyReplaced(replaced)
		return nil, ErrServerFull
	}
	r.channels[key] = ch
	for {
		ch.next++
		if ch.next == 0 {
			ch.next = 1
		}
		if _, taken := ch.peers[ch.next]; !taken {
			break
		}
	}
	p := &peer{idx: ch.next, ch: ch, user: t.User, name: t.Name, speak: t.Speak, muted: t.Muted, hand: t.Hand, lang: t.Lang,
		bitrate: ch.bitrate, conn: conn, videoOK: t.Video, camNeedKey: map[uint16]*[2]bool{}, camSkip: map[uint16]*[2]uint8{}, mod: t.ModUser, pins: map[uint16]bool{},
		out: make(chan []byte, outQueue), vout: make(chan []byte, videoQueue), tokens: 10, refill: now,
		screenOK: t.Screen && ch.maxScreens > 0, watchers: map[uint16]*peer{}, watching: map[uint16]bool{}, needKey: map[uint16]bool{}, skipTid: map[uint16]uint8{}}
	if t.Hand {
		p.handAt = now.UnixMilli()
	}
	// Video-Vorrat: etwa 0,3 s der größten möglichen Bitrate, mindestens ein Schlüsselbild
	kbps := ch.screenKbps + ch.videoKbps*ch.featuredN + ch.thumbKbps*8
	p.vBudget = max(int64(minVBudget), int64(float64(kbps)*125*videoLatency))
	if t.Music > 0 { // eigener Datenstrom: eigene Bitrate, kein Sprecherplatz
		p.music, p.bitrate, p.speak = clamp(t.Music, 1, 100), clamp(t.Bitrate, 6000, 256000), true
	}
	p.bytesTok = float64(p.bitrate) / 8 * 1.5 / 2
	ch.peers[p.idx] = p
	if ch.kind == "" {
		r.total++
	} else {
		r.media++
	}
	hello, _ := json.Marshal(map[string]any{"t": "hello", "you": p.idx, "peers": r.peersLocked(ch), "kind": ch.kind,
		"maxSpeakers": ch.maxSpeakers, "speak": p.speak, "muted": p.muted, "screen": p.screenOK, "screenKbps": ch.screenKbps,
		"video": p.videoOK, "featured": ch.featured, "featuredN": ch.featuredN})
	p.out <- textMsg(hello)
	join, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
	r.broadcastLocked(ch, textMsg(join), p)
	peers := r.peersLocked(ch)
	r.mu.Unlock()
	r.notifyReplaced(replaced)
	r.changedCh(ch, peers)
	return p, nil
}

func (r *Relay) notifyReplaced(ps []*peer) {
	for _, p := range ps {
		p.conn.CloseWith(4000, "In einem anderen Fenster beigetreten")
	}
}

func (r *Relay) changed(name string, peers []PeerInfo) {
	if r.cfg.OnChange != nil {
		r.cfg.OnChange(name, peers)
	}
}

// changedCh meldet nur Sprachkanäle (Medienkanäle erscheinen nicht als Teilnehmerliste).
func (r *Relay) changedCh(ch *channel, peers []PeerInfo) {
	if ch.kind == "" {
		r.changed(ch.name, peers)
	}
}

// Text- und Binärnachrichten in der Ausgangswarteschlange: das erste Byte unterscheidet sie
// (0 = Text, sonst Binär mit dem Typ als erstem Byte).
func textMsg(b []byte) []byte { return append([]byte{0}, b...) }

// writer schreibt die Warteschlangen: Steuerung und Sprache haben Vorrang vor Video, damit ein
// großes Schlüsselbild nie die Sprache aufhält.
func (r *Relay) writer(p *peer) {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	write := func(msg []byte) bool {
		var err error
		if msg[0] == 0 {
			err = p.conn.WriteText(msg[1:])
		} else {
			err = p.conn.WriteBinary(msg)
		}
		if err != nil {
			p.conn.Close()
			return false
		}
		r.bytesOut.Add(int64(len(msg) + 4))
		return true
	}
	for {
		select { // zuerst alles Dringende
		case msg, ok := <-p.out:
			if !ok || !write(msg) {
				return
			}
			continue
		default:
		}
		select {
		case msg, ok := <-p.out:
			if !ok || !write(msg) {
				return
			}
		case msg := <-p.vout:
			p.vq.Add(-int64(len(msg)))
			if !write(msg) {
				return
			}
		case <-ping.C:
			if p.conn.Ping() != nil {
				p.conn.Close()
				return
			}
		}
	}
}

// enqueueVideoLocked stellt ein Video-Bild in die Warteschlange, solange höchstens etwa
// videoLatency Sekunden Video anstehen (eine leere Warteschlange nimmt jedes Bild an).
// soft: ein verzichtbares Bild (höhere Zeitebene) – nur bei halb leerer Warteschlange.
func (r *Relay) enqueueVideoLocked(p *peer, msg []byte, soft bool) bool {
	if p.gone {
		return false
	}
	q, n := p.vq.Load(), int64(len(msg))
	limit := p.vBudget
	if soft {
		limit /= 2
	}
	if q > 0 && q+n > limit {
		return false
	}
	select {
	case p.vout <- msg:
		p.vq.Add(n)
		return true
	default:
		return false
	}
}

// videoBusy: steht schon mehr als der Anteil frac des Video-Vorrats an?
func (p *peer) videoBusy(frac float64) bool { return float64(p.vq.Load()) > float64(p.vBudget)*frac }

func (r *Relay) reader(p *peer) {
	defer r.leave(p)
	// Lebenszeichen: jedes Frame zählt – auch die Antwort des Browsers auf unsere Pings (alle 15 s).
	// Ein Zuschauer, der selbst nichts sendet, bleibt so verbunden.
	p.conn.SetIdleTimeout(readTimeout)
	for {
		op, data, err := p.conn.Read()
		if err != nil {
			return
		}
		if op == wsock.OpBinary {
			if len(data) >= 4 && data[0] == MsgAudio && len(data)-4 <= maxFrame {
				r.audio(p, binary.BigEndian.Uint16(data[1:3]), data[3], data[4:])
			} else if len(data) > 4 && data[0] == MsgVideo {
				r.video(p, data[1], binary.BigEndian.Uint16(data[2:4]), data[4:])
			} else if len(data) > 5 && data[0] == MsgCam {
				r.camFrame(p, int(data[1]&1), data[2], binary.BigEndian.Uint16(data[3:5]), data[5:])
			} else if len(data) > 3 && data[0] == MsgScreenTone && len(data)-3 <= maxToneFrame {
				r.screenTone(p, binary.BigEndian.Uint16(data[1:3]), data[3:])
			}
			continue
		}
		var m ctlMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case "ping":
			b, _ := json.Marshal(map[string]any{"t": "pong", "ts": m.TS})
			r.enqueue(p, textMsg(b))
		case "state":
			r.mu.Lock()
			if m.Mute != nil {
				p.selfMute = *m.Mute
			}
			if m.Hand != nil {
				p.setHand(*m.Hand, r.cfg.Now())
			}
			if m.Sc != nil && (*m.Sc == "" || validCodec(*m.Sc)) {
				p.hintScreen = *m.Sc
			}
			if m.Cm != nil && (*m.Cm == "" || validCodec(*m.Cm)) {
				p.hintCam = *m.Cm
			}
			if m.Q != nil && *m.Q >= 0 && *m.Q <= 3 {
				p.quality = *m.Q
			}
			if m.PK != nil && len(*m.PK) <= 200 && validKey(*m.PK) {
				p.pubKey = *m.PK
			}
			if m.NC != nil {
				p.noCap = *m.NC
				if p.noCap && p.ch.tap == TapCaptions {
					p.tap = nil // laufenden Abschnitt verwerfen
				}
			}
			upd, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
			r.broadcastLocked(p.ch, textMsg(upd), nil)
			peers := r.peersLocked(p.ch)
			ch := p.ch
			r.mu.Unlock()
			r.changedCh(ch, peers)
		case "react", "e2k", "pins", "limit":
			r.small(p, m)
		case "spk": // Medienverbindung (Kamera): „ich spreche gerade“ – für die großen Bilder
			now := r.cfg.Now()
			r.mu.Lock()
			if !p.gone && now.Sub(p.lastSpk) > 200*time.Millisecond {
				p.lastSpk, p.lastSpoke = now, now
				if ch := p.ch; p.cam != "" && !ch.isFeatured(p.idx) && now.Sub(ch.featAt) > 300*time.Millisecond {
					r.refeatureLocked(ch, now, false)
				}
			}
			r.mu.Unlock()
		case "share":
			r.share(p, m.On != nil && *m.On, m.Codec)
		case "watch":
			r.watch(p, m.I, m.On != nil && *m.On)
		case "keyreq":
			r.mu.Lock()
			if s := p.ch.peers[m.I]; s != nil && s.codec != "" && p.watching[m.I] {
				r.keyReqLocked(s)
			}
			r.mu.Unlock()
		case "cam":
			r.camOn(p, m.On != nil && *m.On, m.Codec)
		case "camkey":
			r.mu.Lock()
			if s := p.ch.peers[m.I]; s != nil && s.cam != "" && s != p {
				p.camKeys(m.I)[m.L&1] = true
				r.camKeyReqLocked(s, m.L&1)
			}
			r.mu.Unlock()
		}
	}
}

func (r *Relay) leave(p *peer) {
	r.mu.Lock()
	if p.gone {
		r.mu.Unlock()
		p.conn.Close()
		return
	}
	ch := p.ch
	r.removeLocked(p)
	peers := r.peersLocked(ch)
	r.mu.Unlock()
	p.conn.Close()
	r.changedCh(ch, peers)
}

// removeLocked nimmt eine Person aus ihrem Kanal (r.mu muss gehalten werden).
func (r *Relay) removeLocked(p *peer) {
	if p.gone {
		return
	}
	r.tapFlushLocked(p) // letzter Satz vor dem Verlassen
	p.gone = true
	ch := p.ch
	for i := range p.watching { // als Zuschauer abmelden
		if s := ch.peers[i]; s != nil {
			delete(s.watchers, p.idx)
		}
	}
	for _, w := range p.watchers { // als Teilende: Zuschauer lösen
		delete(w.watching, p.idx)
		delete(w.needKey, p.idx)
		delete(w.skipTid, p.idx)
	}
	for _, q := range ch.peers {
		delete(q.camNeedKey, p.idx)
		delete(q.camSkip, p.idx)
		delete(q.pins, p.idx)
	}
	delete(ch.peers, p.idx)
	if ch.isFeatured(p.idx) {
		r.refeatureLocked(ch, r.cfg.Now(), true)
	}
	if len(p.pins) > 0 { // wer nur von p angeheftet war, braucht kein großes Bild mehr
		r.updateHighLocked(ch)
	}
	delete(ch.active, p.idx)
	if ch.kind == "" {
		r.total--
	} else {
		r.media--
	}
	close(p.out)
	b, _ := json.Marshal(map[string]any{"t": "leave", "i": p.idx})
	r.broadcastLocked(ch, textMsg(b), nil)
	if len(ch.peers) == 0 {
		delete(r.channels, ch.name)
	}
}

// enqueue legt eine Nachricht in die Warteschlange; ist sie voll, wird verworfen (lieber eine
// Lücke als eine wachsende Verzögerung). Bleibt sie voll, wird die Verbindung getrennt.
func (r *Relay) enqueue(p *peer, msg []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueueLocked(p, msg)
}

func (r *Relay) enqueueLocked(p *peer, msg []byte) {
	if p.gone {
		return
	}
	select {
	case p.out <- msg:
		p.drops = 0
	default:
		p.drops++
		if p.drops > maxDrops && !p.closing {
			p.closing = true
			go p.conn.CloseWith(4408, "Verbindung zu langsam")
		}
	}
}

func (r *Relay) broadcastLocked(ch *channel, msg []byte, except *peer) {
	for _, q := range ch.peers {
		if q != except {
			r.enqueueLocked(q, msg)
		}
	}
}

// audio nimmt einen Rahmen an: nur wer sprechen darf, nicht stummgeschaltet ist, im Rahmen der
// Paket- und Bitratengrenzen bleibt und einen der Sprecherplätze hat.
func (r *Relay) audio(p *peer, seq uint16, level uint8, data []byte) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.gone || !p.speak || p.muted || p.selfMute || p.ch.kind != "" {
		return
	}
	if p.music > 0 { // Musik eines Bots: eigener Strom, kein Sprecherplatz
		dt := now.Sub(p.refill).Seconds()
		p.refill = now
		p.tokens = minF(20, p.tokens+dt*120)
		maxBytes := float64(p.bitrate) / 8 * 1.5
		p.bytesTok = minF(maxBytes, p.bytesTok+dt*maxBytes)
		if p.tokens < 1 || p.bytesTok < float64(len(data)) {
			return
		}
		p.tokens--
		p.bytesTok -= float64(len(data))
		p.ch.frames = append(p.ch.frames, frameRef{from: p, seq: seq, data: append([]byte(nil), data...)})
		if !p.ch.ticking {
			p.ch.ticking = true
			go r.ticker(p.ch)
		}
		return
	}
	// Grenzen: höchstens 60 Rahmen/s und 1,5 × Bitrate (kleiner Vorrat für Spitzen)
	dt := now.Sub(p.refill).Seconds()
	p.refill = now
	p.tokens = minF(10, p.tokens+dt*60)
	maxBytes := float64(p.bitrate) / 8 * 1.5
	p.bytesTok = minF(maxBytes/2, p.bytesTok+dt*maxBytes)
	if p.tokens < 1 || p.bytesTok < float64(len(data)) {
		return
	}
	p.tokens--
	p.bytesTok -= float64(len(data))
	if p.ch.tap != TapOff { // auch ohne Sprecherplatz: es zählt, was gesagt wurde
		r.tapFrameLocked(p, level, data, now)
	}

	ch := p.ch
	for i, a := range ch.active {
		if now.Sub(a.last) > activeTimeout {
			delete(ch.active, i)
		}
	}
	if _, ok := ch.active[p.idx]; !ok {
		if len(ch.active) >= ch.maxSpeakers {
			// Alle Plätze belegt: nur deutlich Lauteres verdrängt das Leiseste
			var quiet *peer
			for _, a := range ch.active {
				if quiet == nil || a.level < quiet.level {
					quiet = a
				}
			}
			if quiet == nil || int(level) < int(quiet.level)+48 {
				return
			}
			delete(ch.active, quiet.idx)
		}
		ch.active[p.idx] = p
	}
	p.last, p.level = now, level
	if level >= 64 { // spricht hörbar (über etwa −45 dB): für die großen Bilder
		p.lastSpoke = now
		if p.cam != "" && !ch.isFeatured(p.idx) && now.Sub(ch.featAt) > 300*time.Millisecond {
			r.refeatureLocked(ch, now, false)
		}
	}
	ch.frames = append(ch.frames, frameRef{from: p, seq: seq, data: append([]byte(nil), data...)})
	if !ch.ticking {
		ch.ticking = true
		go r.ticker(ch)
	}
}

// ticker bündelt alle 20 ms die eingegangenen Rahmen eines Kanals und verteilt sie. Er endet,
// wenn eine Weile niemand spricht.
func (r *Relay) ticker(ch *channel) {
	t := time.NewTicker(tick)
	defer t.Stop()
	idle := 0
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		r.mu.Lock()
		frames := ch.frames
		ch.frames = nil
		if len(frames) == 0 {
			if idle++; idle > 50 || len(ch.peers) == 0 {
				ch.ticking = false
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			continue
		}
		idle = 0
		// Ein Bündel für alle Zuhörer; wer selbst Rahmen geschickt hat, bekommt eins ohne die eigenen.
		senders := map[*peer]bool{}
		for _, f := range frames {
			senders[f.from] = true
		}
		common := bundle(frames, nil)
		for _, q := range ch.peers {
			if senders[q] {
				if b := bundle(frames, q); b != nil {
					r.enqueueLocked(q, b)
				}
				continue
			}
			r.enqueueLocked(q, common)
		}
		r.mu.Unlock()
	}
}

func bundle(frames []frameRef, without *peer) []byte {
	n := 0
	size := 2
	for _, f := range frames {
		if f.from != without {
			n++
			size += 6 + len(f.data)
		}
	}
	if n == 0 {
		return nil
	}
	if n > 255 {
		n = 255
	}
	b := make([]byte, 2, size)
	b[0], b[1] = MsgAudio, byte(n)
	written := 0
	for _, f := range frames {
		if f.from == without || written == n {
			continue
		}
		var h [6]byte
		binary.BigEndian.PutUint16(h[0:], f.from.idx)
		binary.BigEndian.PutUint16(h[2:], f.seq)
		binary.BigEndian.PutUint16(h[4:], uint16(len(f.data)))
		b = append(b, h[:]...)
		b = append(b, f.data...)
		written++
	}
	return b
}

// --- Steuerung durch den Chat-Server ---

// Control ändert eine Person in einem Kanal: "speak"/"mute" (value), "hand" (value), "kick".
// user = "" und action "close" beendet den ganzen Kanal. Liefert, ob es die Person gab.
// Die Befehle gelten auch für die Medienverbindungen des Kanals auf diesem Relay; "mediakick"
// beendet nur diese (die Person hat den Sprachkanal verlassen).
func (r *Relay) Control(channel, user, action string, value bool, reason string) bool {
	if action == "tap" { // Mitschnitt für die Spracherkennung: reason = Modus (0, 1, 2)
		r.mu.Lock()
		defer r.mu.Unlock()
		ch := r.channels[channel]
		if ch == nil || len(reason) != 1 {
			return false
		}
		r.setTapLocked(ch, int(reason[0])-'0')
		return true
	}
	if action == "remedia" { // eine Medienart ist auf einen anderen Relay umgezogen (reason = Art): alle verbinden neu
		r.mu.Lock()
		defer r.mu.Unlock()
		ch := r.channels[channel]
		kind, to, _ := strings.Cut(reason, "@") // „screen@relay“: Art und neuer Relay
		if ch == nil || (kind != KindScreen && kind != KindVideo) {
			return false
		}
		b, _ := json.Marshal(map[string]any{"t": "media", "kind": kind, "relay": to})
		r.broadcastLocked(ch, textMsg(b), nil)
		return true
	}
	keys := []string{channel, MediaKey(channel, KindScreen), MediaKey(channel, KindVideo)}
	if action == "mediakick" {
		keys, action = keys[1:], "kick"
	}
	found := false
	for _, k := range keys {
		if r.controlOne(k, user, action, value, reason) {
			found = true
		}
	}
	return found
}

func (r *Relay) controlOne(key, user, action string, value bool, reason string) bool {
	r.mu.Lock()
	ch := r.channels[key]
	if ch == nil {
		r.mu.Unlock()
		return false
	}
	switch action {
	case "speak", "hand", "move", "forcemute", "askunmute", "rec", "cap": // betrifft nur die Sprache
		if ch.kind != "" {
			r.mu.Unlock()
			return false
		}
	}
	var hit []*peer
	for _, p := range ch.peers {
		// user "" bei forcemute/camsoff: alle außer Moderierenden
		if p.user == user || action == "close" || user == "" && !p.mod && (action == "forcemute" || action == "camsoff") {
			hit = append(hit, p)
		}
	}
	if action == "cap" { // Untertitel aus dem eigenen Whisper (value = endgültig, reason = Text)
		if len(hit) > 0 {
			b, _ := json.Marshal(map[string]any{"t": "cap", "i": hit[0].idx, "u": hit[0].user, "x": capText(reason), "f": value})
			r.broadcastLocked(ch, textMsg(b), nil)
		}
		r.mu.Unlock()
		return len(hit) > 0
	}
	if len(hit) == 0 {
		r.mu.Unlock()
		return false
	}
	var closeList []*peer
	for _, p := range hit {
		switch action {
		case "speak":
			p.speak = value
		case "mute":
			p.muted = value
		case "hand":
			p.setHand(value, r.cfg.Now())
		case "screen": // Recht zum Teilen; false beendet eine laufende Freigabe
			p.screenOK = value && ch.maxScreens > 0
			if !p.screenOK && p.codec != "" {
				r.stopShareLocked(p)
			}
		case "video": // Recht für die Kamera; false schaltet sie aus
			p.videoOK = value
			if !value && p.cam != "" {
				r.camOffLocked(p)
			}
		case "mod":
			p.mod = value
		case "rec": // nimmt auf – alle sehen es
			p.rec = value
		case "musicvol": // Musik eines Bots: neue Voreinstellung der Lautstärke (reason = Prozent)
			if p.music == 0 {
				continue
			}
			if n, err := strconv.Atoi(reason); err == nil {
				p.music = clamp(n, 1, 100)
			}
		case "forcemute": // „alle stummschalten“: selbst wieder einschaltbar
			r.enqueueLocked(p, textMsg([]byte(`{"t":"forcemute"}`)))
			continue
		case "askunmute": // „bitte Mikrofon einschalten“ – die Person entscheidet
			r.enqueueLocked(p, textMsg([]byte(`{"t":"askunmute"}`)))
			continue
		case "camsoff": // „alle Kameras aus“: wieder einschaltbar
			if p.cam != "" {
				r.camOffLocked(p)
			}
			continue
		case "move": // in einen anderen Kanal schicken (reason = Ziel); der Browser tritt dort bei
			b, _ := json.Marshal(map[string]any{"t": "move", "room": reason})
			r.enqueueLocked(p, textMsg(b))
			continue
		case "kick", "close":
			closeList = append(closeList, p)
			continue
		}
		if !p.speak || p.muted {
			delete(ch.active, p.idx)
		}
		you, _ := json.Marshal(map[string]any{"t": "you", "speak": p.speak, "muted": p.muted, "hand": p.hand, "screen": p.screenOK, "sharing": p.codec != "", "video": p.videoOK, "mod": p.mod})
		r.enqueueLocked(p, textMsg(you))
		upd, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
		r.broadcastLocked(ch, textMsg(upd), p)
	}
	for _, p := range closeList {
		bye, _ := json.Marshal(map[string]any{"t": "bye", "reason": reason})
		r.enqueueLocked(p, textMsg(bye))
		r.removeLocked(p)
	}
	peers := r.peersLocked(ch)
	r.mu.Unlock()
	for _, p := range closeList {
		go func(p *peer) { // Abschiedsnachricht noch zustellen lassen
			time.Sleep(200 * time.Millisecond)
			p.conn.CloseWith(4003, reason)
		}(p)
	}
	r.changedCh(ch, peers)
	return true
}

// KickUser entfernt eine Person aus allen Kanälen dieses Relays (z. B. gesperrt).
func (r *Relay) KickUser(user, reason string) {
	r.mu.Lock()
	var chans []string
	for n, ch := range r.channels {
		for _, p := range ch.peers {
			if p.user == user {
				chans = append(chans, n)
			}
		}
	}
	r.mu.Unlock()
	for _, n := range chans {
		r.controlOne(n, user, "kick", false, reason)
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// --- Bildschirmfreigabe ---

// share startet oder beendet die Freigabe einer Person.
func (r *Relay) share(p *peer, on bool, codec string) {
	r.mu.Lock()
	if p.gone {
		r.mu.Unlock()
		return
	}
	ch := p.ch
	reply := map[string]any{"t": "share", "on": false}
	changed := false
	switch {
	case !on:
		if p.codec != "" {
			r.stopShareLocked(p)
			changed = true
		}
		reply = nil // stopShareLocked hat schon geantwortet
	case !p.screenOK:
		reply["reason"] = "denied"
	case !validCodec(codec):
		reply["reason"] = "codec"
	case p.codec == "" && r.sharersLocked(ch) >= ch.maxScreens:
		reply["reason"] = "full"
	default:
		if p.codec != codec { // neuer Codec: Zuschauer brauchen ein Schlüsselbild
			for _, w := range p.watchers {
				w.needKey[p.idx] = true
			}
			p.codec = codec
			changed = true
			upd, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
			r.broadcastLocked(ch, textMsg(upd), nil)
		}
		reply["on"], reply["codec"] = true, codec
	}
	if reply != nil {
		b, _ := json.Marshal(reply)
		r.enqueueLocked(p, textMsg(b))
	}
	peers := r.peersLocked(ch)
	r.mu.Unlock()
	if changed {
		r.changedCh(ch, peers)
	}
}

func validCodec(c string) bool {
	if len(c) == 0 || len(c) > 40 {
		return false
	}
	for _, x := range c {
		if !(x >= 'a' && x <= 'z' || x >= 'A' && x <= 'Z' || x >= '0' && x <= '9' || x == '.') {
			return false
		}
	}
	return true
}

func (r *Relay) sharersLocked(ch *channel) int {
	n := 0
	for _, q := range ch.peers {
		if q.codec != "" {
			n++
		}
	}
	return n
}

// stopShareLocked beendet eine Freigabe (Zuschauer werden gelöst).
func (r *Relay) stopShareLocked(p *peer) {
	p.codec = ""
	for _, w := range p.watchers {
		delete(w.watching, p.idx)
		delete(w.needKey, p.idx)
	}
	p.watchers = map[uint16]*peer{}
	upd, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
	r.broadcastLocked(p.ch, textMsg(upd), nil)
	r.enqueueLocked(p, textMsg([]byte(`{"t":"share","on":false}`)))
}

// watch: Zusehen beginnen oder beenden. Beginnen nur, solange der Relay noch Bandbreite hat.
func (r *Relay) watch(p *peer, idx uint16, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.gone {
		return
	}
	s := p.ch.peers[idx]
	reply := map[string]any{"t": "watch", "i": idx, "on": false}
	switch {
	case !on:
		if s != nil {
			delete(s.watchers, p.idx)
		}
		delete(p.watching, idx)
		delete(p.needKey, idx)
	case s == nil || s.codec == "" || s == p:
		reply["reason"] = "gone"
	case p.watching[idx]:
		reply["on"], reply["codec"] = true, s.codec
		p.needKey[idx] = true
		r.keyReqLocked(s)
	case r.estimateLocked()+float64(p.ch.screenKbps)*1.05/1000 > r.cfg.MaxMbit:
		reply["reason"] = "busy"
	default:
		s.watchers[p.idx] = p
		p.watching[idx] = true
		p.needKey[idx] = true
		reply["on"], reply["codec"] = true, s.codec
		r.keyReqLocked(s)
	}
	b, _ := json.Marshal(reply)
	r.enqueueLocked(p, textMsg(b))
}

// keyReqLocked bittet die Teilende um ein Schlüsselbild (höchstens einmal je Sekunde).
func (r *Relay) keyReqLocked(s *peer) {
	now := r.cfg.Now()
	if !s.lastKeyRq.IsZero() && now.Sub(s.lastKeyRq) < keyReqGap {
		return
	}
	s.lastKeyRq = now
	r.enqueueLocked(s, textMsg([]byte(`{"t":"keyreq"}`)))
}

// video verteilt ein Bild an alle Zuschauer. Wer ein Bild verpasst (volle Warteschlange),
// bekommt bis zum nächsten Schlüsselbild nichts und löst eine Anfrage danach aus.
func (r *Relay) video(p *peer, flags uint8, seq uint16, data []byte) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.gone || p.codec == "" || len(data) > maxVideoFrame {
		return
	}
	// Kontingent: 1,5 × Bitrate, Vorrat für 2 s bzw. ein großes Schlüsselbild
	rate := float64(p.ch.screenKbps) * 1000 / 8 * 1.5
	burst := max(rate*2, 768<<10)
	if p.vRefill.IsZero() {
		p.vBytes = burst
	} else {
		p.vBytes = minF(burst, p.vBytes+now.Sub(p.vRefill).Seconds()*rate)
	}
	p.vRefill = now
	key := flags&1 != 0
	if p.vBytes < float64(len(data)) { // zu viel: Bild verwerfen, Zuschauer warten aufs nächste Schlüsselbild
		for _, w := range p.watchers {
			w.needKey[p.idx] = true
		}
		return
	}
	p.vBytes -= float64(len(data))
	if len(p.watchers) == 0 {
		return
	}
	msg := make([]byte, 6+len(data))
	msg[0] = MsgVideo
	binary.BigEndian.PutUint16(msg[1:], p.idx)
	msg[3] = flags
	binary.BigEndian.PutUint16(msg[4:], seq)
	copy(msg[6:], data)
	tid := frameTid(flags)
	p.topTid = max(p.topTid, tid)
	missing := false
	for _, w := range p.watchers {
		if w.paused || !w.allowTid(p, tid) { // Sparmodus: nichts bzw. nur die unteren Zeitebenen
			continue
		}
		if w.needKey[p.idx] {
			if !key {
				missing = true
				continue
			}
			delete(w.needKey, p.idx)
			delete(w.skipTid, p.idx)
		}
		if s, ok := w.skipTid[p.idx]; ok { // eine Zeitebene wurde ausgelassen: alles darüber auch
			if tid >= s {
				continue
			}
			delete(w.skipTid, p.idx)
		}
		if r.enqueueVideoLocked(w, msg, tid > 0) {
			continue
		}
		// langsame Verbindung: erst Bilder der oberen Zeitebenen opfern (das Bild bleibt flüssig,
		// nur mit weniger Bildern je Sekunde), erst danach bis zum nächsten Schlüsselbild warten
		if tid > 0 && !key {
			w.skipTid[p.idx] = tid
			continue
		}
		w.needKey[p.idx] = true
		missing = true
	}
	if missing {
		r.keyReqLocked(p)
	}
}

// allowTid: Bekommt w (Sparmodus) Bilder dieser Zeitebene von s? Mit drop=1 fällt die oberste
// Ebene weg (halbe Bildrate), mit drop=2 auch die darunter (ein Viertel) – nie die Grundebene.
func (w *peer) allowTid(s *peer, tid uint8) bool {
	if w.drop == 0 || tid == 0 {
		return true
	}
	return tid+w.drop <= s.topTid
}

// frameTid: Zeitebene eines Video-Bildes (flags Bit 1–2; 0 = Grundebene, ohne SVC immer 0).
func frameTid(flags uint8) uint8 { return (flags >> 1) & 3 }
