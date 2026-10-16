// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ucng/internal/voicerelay"
	"ucng/internal/wsock"
)

// Musik von Bots: Ein Bot mit dem Recht „Musik“ lädt eine Ogg-Opus-Datei hoch; der Chat-Server
// spielt sie in Echtzeit in einen Sprachkanal ein (als eigener Teilnehmer, leise voreingestellt –
// jede Person kann die Lautstärke wie bei allen anderen selbst ändern). Der Kanal muss Musik von
// Bots erlauben. Umwandeln z. B. mit:
//
//	ffmpeg -i lied.mp3 -c:a libopus -b:a 96k -frame_duration 20 lied.opus

const maxMusicFile = 32 << 20

var (
	ErrMusicFormat = errors.New("Bitte eine Ogg-Opus-Datei schicken (z. B. mit ffmpeg -c:a libopus umwandeln)")
	ErrMusicRoom   = errors.New("In diesem Sprachkanal dürfen Bots keine Musik abspielen")
	ErrMusicLarge  = errors.New("Die Datei ist zu groß (höchstens 32 MB)")
)

// opusTrack: die Pakete einer Ogg-Opus-Datei mit ihrer Dauer.
type opusTrack struct {
	channels int
	packets  [][]byte
	durs     []time.Duration
	total    time.Duration
	maxRate  int // höchste Bitrate über eine Sekunde (bit/s)
}

// parseOggOpus liest eine Ogg-Datei mit einem Opus-Strom (RFC 7845).
func parseOggOpus(b []byte) (*opusTrack, error) {
	t := &opusTrack{}
	var pkt []byte
	var serial uint32
	n := 0 // Pakete bisher (0 = OpusHead, 1 = OpusTags)
	for pos := 0; pos < len(b); {
		if len(b)-pos < 27 || string(b[pos:pos+4]) != "OggS" {
			return nil, ErrMusicFormat
		}
		segs := int(b[pos+26])
		if len(b)-pos < 27+segs {
			return nil, ErrMusicFormat
		}
		pageSerial := binary.LittleEndian.Uint32(b[pos+14:])
		if n == 0 {
			serial = pageSerial
		}
		lace := b[pos+27 : pos+27+segs]
		data := pos + 27 + segs
		for _, l := range lace {
			if data+int(l) > len(b) {
				return nil, ErrMusicFormat
			}
			if pageSerial == serial { // andere Ströme (z. B. Bilder) überspringen
				pkt = append(pkt, b[data:data+int(l)]...)
			}
			data += int(l)
			if l < 255 && pageSerial == serial {
				if err := t.add(n, pkt); err != nil {
					return nil, err
				}
				n++
				pkt = nil
			}
		}
		pos = data
	}
	if n < 3 || len(t.packets) == 0 {
		return nil, ErrMusicFormat
	}
	// höchste Bitrate über ein gleitendes Fenster von 1 s
	var sum, start int
	var win time.Duration
	for i := range t.packets {
		sum += len(t.packets[i])
		win += t.durs[i]
		for win > time.Second && start < i {
			sum -= len(t.packets[start])
			win -= t.durs[start]
			start++
		}
		if win >= 500*time.Millisecond {
			t.maxRate = max(t.maxRate, int(float64(sum*8)/win.Seconds()))
		}
	}
	if t.maxRate == 0 {
		t.maxRate = 64000
	}
	return t, nil
}

func (t *opusTrack) add(n int, p []byte) error {
	switch n {
	case 0:
		if len(p) < 19 || string(p[:8]) != "OpusHead" {
			return ErrMusicFormat
		}
		t.channels = int(p[9])
		if t.channels < 1 || t.channels > 2 {
			return errors.New("Nur Mono- oder Stereo-Dateien werden unterstützt")
		}
		return nil
	case 1:
		if len(p) < 8 || string(p[:8]) != "OpusTags" {
			return ErrMusicFormat
		}
		return nil
	}
	if len(p) == 0 {
		return nil
	}
	d := opusDuration(p)
	if d == 0 || len(p) > 1275 {
		return nil // ungültiges oder zu großes Paket: auslassen
	}
	t.packets = append(t.packets, append([]byte(nil), p...))
	t.durs = append(t.durs, d)
	t.total += d
	return nil
}

// opusDuration: Länge eines Opus-Pakets aus dem TOC-Byte (RFC 6716, 3.1).
func opusDuration(p []byte) time.Duration {
	cfg := int(p[0] >> 3)
	var frame time.Duration
	switch {
	case cfg < 12: // SILK
		frame = []time.Duration{10, 20, 40, 60}[cfg%4] * time.Millisecond
	case cfg < 16: // Hybrid
		frame = []time.Duration{10, 20}[cfg%2] * time.Millisecond
	default: // CELT
		frame = []time.Duration{2500, 5000, 10000, 20000}[cfg%4] * time.Microsecond
	}
	count := 1
	switch p[0] & 3 {
	case 1, 2:
		count = 2
	case 3:
		if len(p) < 2 {
			return 0
		}
		count = int(p[1] & 63)
	}
	return frame * time.Duration(count)
}

// --- Abspielen ---

type musicPlayers struct {
	mu   sync.Mutex
	byRm map[string]*musicPlayer
}

func newMusicPlayers() *musicPlayers { return &musicPlayers{byRm: map[string]*musicPlayer{}} }

type musicPlayer struct {
	Room    string    `json:"room"`
	Bot     string    `json:"bot"`
	Title   string    `json:"title,omitempty"`
	Volume  int       `json:"volume"`
	Loop    bool      `json:"loop"`
	Started time.Time `json:"started"`
	Length  float64   `json:"lengthSeconds"`
	stop    chan struct{}
	once    sync.Once
}

func (p *musicPlayer) Stop() { p.once.Do(func() { close(p.stop) }) }

// play verbindet sich mit dem Relay des Kanals und spielt die Datei in Echtzeit ab.
func (s *Server) playMusic(b Bot, room string, cfg VoiceConfig, track *opusTrack, mp *musicPlayer) error {
	v := s.voice
	choice, err := v.pick(room, cfg)
	if err != nil {
		return err
	}
	pr := cfg.preset()
	ticket := voicerelay.SignTicket(v.priv, voicerelay.Ticket{
		Channel: room, User: "bot:" + b.ID, Name: b.Name, Speak: true, Music: mp.Volume,
		Bitrate: min(max(track.maxRate*5/4, 24000), 256000), MaxSpeakers: cfg.MaxSpeakers, MaxPeers: cfg.MaxPeers, Stereo: pr.Stereo,
	}, time.Now())
	var conn *wsock.Client
	if choice.id == builtinRelay {
		srv, cli := net.Pipe()
		go v.builtin.ServeConn(wsock.NewServerConn(srv, 1<<20))
		conn = wsock.NewClientConn(cli)
	} else {
		conn, err = wsock.Dial(choice.url, http.Header{"Origin": {originOf(s.baseURL())}})
		if err != nil {
			return err
		}
	}
	auth, _ := json.Marshal(map[string]string{"t": "auth", "ticket": ticket})
	conn.WriteText(auth)
	op, data, err := conn.Read(5 * time.Second)
	if err != nil || op != wsock.OpText || !json.Valid(data) {
		conn.Close()
		return errors.New("Der Sprach-Server hat die Musik abgelehnt")
	}
	var hello struct {
		T string `json:"t"`
	}
	json.Unmarshal(data, &hello)
	if hello.T != "hello" {
		conn.Close()
		return errors.New("Der Sprach-Server hat die Musik abgelehnt")
	}
	done := make(chan struct{})
	go func() { // Nachrichten lesen (Ping beantworten); Ende, wenn der Relay trennt
		defer close(done)
		for {
			if _, _, err := conn.Read(40 * time.Second); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() {
			conn.Close()
			s.music.mu.Lock()
			if s.music.byRm[room] == mp {
				delete(s.music.byRm, room)
			}
			s.music.mu.Unlock()
		}()
		seq := uint16(0)
		for {
			start := time.Now()
			var at time.Duration
			for i, pkt := range track.packets {
				select {
				case <-mp.stop:
					return
				case <-done:
					return
				default:
				}
				if wait := time.Until(start.Add(at)); wait > 0 {
					select {
					case <-time.After(wait):
					case <-mp.stop:
						return
					case <-done:
						return
					}
				}
				seq++
				msg := make([]byte, 4+len(pkt))
				msg[0] = voicerelay.MsgAudio
				binary.BigEndian.PutUint16(msg[1:], seq)
				msg[3] = 128
				copy(msg[4:], pkt)
				if conn.WriteBinary(msg) != nil {
					return
				}
				at += track.durs[i]
			}
			if !mp.Loop {
				time.Sleep(200 * time.Millisecond) // Rest abspielen lassen
				return
			}
		}
	}()
	return nil
}

// botMusicRoom: Kanal, in dem dieser Bot Musik abspielen darf.
func (s *Server) botMusicRoom(w http.ResponseWriter, r *http.Request) (Bot, string, *VoiceConfig, bool) {
	b, room, ok := botRoom(w, r)
	if !ok {
		return b, room, nil, false
	}
	if !b.P().Music {
		fail(w, errBotNoPerm)
		return b, room, nil, false
	}
	if s.voice == nil {
		voiceFail(w, ErrVoiceOff)
		return b, room, nil, false
	}
	info, found := s.hub.roomInfo(room)
	if !found || info.Voice == nil {
		voiceFail(w, ErrNoVoice)
		return b, room, nil, false
	}
	if !info.Voice.BotMusic {
		writeErr(w, http.StatusForbidden, ErrMusicRoom.Error())
		return b, room, nil, false
	}
	return b, room, info.Voice, true
}

// POST /api/bot/voice/{room}/play?volume=20&loop=1&title=… – Ogg-Opus-Datei als Inhalt
func (s *Server) handleBotMusicPlay(w http.ResponseWriter, r *http.Request) {
	b, room, cfg, ok := s.botMusicRoom(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMusicFile))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, ErrMusicLarge.Error())
		return
	}
	track, err := parseOggOpus(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	vol := 20
	if q := r.URL.Query().Get("volume"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			vol = min(max(n, 1), 100)
		}
	}
	title := r.URL.Query().Get("title")
	if len([]rune(title)) > 100 {
		title = string([]rune(title)[:100])
	}
	mp := &musicPlayer{Room: room, Bot: b.Name, Title: title, Volume: vol, Loop: r.URL.Query().Get("loop") == "1",
		Started: time.Now().UTC(), Length: track.total.Seconds(), stop: make(chan struct{})}
	s.music.mu.Lock()
	if old := s.music.byRm[room]; old != nil { // ein Musikstück je Kanal
		old.Stop()
	}
	s.music.byRm[room] = mp
	s.music.mu.Unlock()
	if err := s.playMusic(b, room, *cfg, track, mp); err != nil {
		s.music.mu.Lock()
		delete(s.music.byRm, room)
		s.music.mu.Unlock()
		voiceFail(w, err)
		return
	}
	s.hub.AuditLog().Add(botAudit(b, AuditEntry{Action: "voice.music", Room: room, Detail: title}))
	log.Printf("Musik von %s in #%s (%.0f s, Lautstärke %d %%)", b.Name, room, track.total.Seconds(), vol)
	writeJSON(w, http.StatusAccepted, mp)
}

// POST /api/bot/voice/{room}/stop
func (s *Server) handleBotMusicStop(w http.ResponseWriter, r *http.Request) {
	_, room, _, ok := s.botMusicRoom(w, r)
	if !ok {
		return
	}
	s.music.mu.Lock()
	mp := s.music.byRm[room]
	delete(s.music.byRm, room)
	s.music.mu.Unlock()
	if mp != nil {
		mp.Stop()
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": mp != nil})
}

// GET /api/bot/voice/{room} – läuft gerade Musik?
func (s *Server) handleBotMusicStatus(w http.ResponseWriter, r *http.Request) {
	_, room, _, ok := s.botMusicRoom(w, r)
	if !ok {
		return
	}
	s.music.mu.Lock()
	mp := s.music.byRm[room]
	s.music.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"playing": mp != nil, "track": mp})
}
