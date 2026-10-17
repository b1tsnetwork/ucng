// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

// Mitschnitt für die Spracherkennung („Tap“): Ist er für einen Kanal an, sammelt der Relay die
// Opus-Rahmen jeder sprechenden Person, schneidet sie an Sprechpausen in Abschnitte und gibt
// jeden Abschnitt als Ogg-Opus-Datei an Config.OnSegment – mit Person und Zeitpunkt. Der Relay
// dekodiert nichts (Opus wird nur in Ogg verpackt); die eigene Spracherkennung (Whisper)
// liest Ogg-Opus direkt. So steht der Sprecher sicher fest – er kommt aus der Verbindung,
// nicht aus einer Angabe des Browsers.
//
// Modi: TapOff, TapCaptions (nur Untertitel; wer „nicht verschriftlichen“ gewählt hat, wird
// ausgelassen) und TapTranscript (Live-Transkription: alle, die sprechen).

import (
	"encoding/binary"
	"encoding/json"
	"time"
)

const (
	TapOff        = 0
	TapCaptions   = 1
	TapTranscript = 2

	tapVoiceLevel = 40                     // ab diesem Pegel zählt ein Rahmen als Sprache
	tapPause      = 750 * time.Millisecond // so lange Pause (oder leise) beendet einen Abschnitt
	tapMaxFrames  = 500                    // höchstens 10 s je Abschnitt (Untertitel sollen nicht zu spät kommen)
	tapMinVoice   = 20                     // unter 0,4 s Sprache: kein Abschnitt
	tapEvery      = 200 * time.Millisecond
)

// Segment: ein Sprach-Abschnitt einer Person.
type Segment struct {
	Channel string        // Chat-Kanal
	User    string        // Nutzer-ID (aus der Zutrittskarte)
	Name    string        // Anzeigename
	Lang    string        // Sprache der Person laut Zutrittskarte (z. B. "de"), sonst leer
	Mode    int           // TapCaptions oder TapTranscript
	Start   time.Time     // Beginn des Abschnitts
	Dur     time.Duration // Länge
	Ogg     []byte        // Ogg-Opus (48 kHz)
}

type tapBuf struct {
	frames  [][]byte
	start   time.Time
	last    time.Time // letzter Rahmen
	voiced  time.Time // letzter Rahmen mit Sprache
	nVoiced int
	quiet   int // leise Rahmen am Ende
	stereo  bool
}

// tapFrameLocked nimmt einen Rahmen in den Abschnitt der Person auf (r.mu gehalten).
func (r *Relay) tapFrameLocked(p *peer, level uint8, data []byte, now time.Time) {
	ch := p.ch
	if ch.tap == TapOff || p.music > 0 || ch.tap == TapCaptions && p.noCap || r.cfg.OnSegment == nil || len(data) == 0 {
		return
	}
	b := p.tap
	if b == nil {
		if level < tapVoiceLevel { // Abschnitte beginnen mit Sprache
			return
		}
		b = &tapBuf{start: now, stereo: data[0]&0x4 != 0}
		p.tap = b
	}
	b.frames = append(b.frames, append([]byte(nil), data...))
	b.last = now
	if level >= tapVoiceLevel {
		b.voiced, b.quiet = now, 0
		b.nVoiced++
	} else {
		b.quiet++
	}
	if len(b.frames) >= tapMaxFrames || now.Sub(b.voiced) >= tapPause {
		r.tapFlushLocked(p)
	}
}

// tapFlushLocked beendet den laufenden Abschnitt der Person und gibt ihn weiter.
func (r *Relay) tapFlushLocked(p *peer) {
	b := p.tap
	p.tap = nil
	if b == nil || b.nVoiced < tapMinVoice || r.cfg.OnSegment == nil {
		return
	}
	frames := b.frames
	if cut := b.quiet - 10; cut > 0 && cut < len(frames) { // lange Stille am Ende weglassen
		frames = frames[:len(frames)-cut]
	}
	mode := p.ch.tap
	if mode == TapOff {
		mode = TapCaptions
	}
	seg := Segment{Channel: p.ch.room, User: p.user, Name: p.name, Lang: p.lang, Mode: mode, Start: b.start,
		Dur: time.Duration(len(frames)) * 20 * time.Millisecond, Ogg: OggOpus(frames, b.stereo)}
	go r.cfg.OnSegment(seg)
}

// tapLoop schließt Abschnitte, nach denen nichts mehr kam (der Browser sendet in Pausen nicht).
func (r *Relay) tapLoop() {
	t := time.NewTicker(tapEvery)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		now := r.cfg.Now()
		r.mu.Lock()
		for _, ch := range r.channels {
			if ch.kind != "" {
				continue
			}
			for _, p := range ch.peers {
				if p.tap != nil && now.Sub(p.tap.last) >= tapPause {
					r.tapFlushLocked(p)
				}
			}
		}
		r.mu.Unlock()
	}
}

// setTapLocked schaltet den Mitschnitt eines Kanals um; laufende Abschnitte werden beim
// Ausschalten noch weitergegeben (der letzte Satz geht nicht verloren).
func (r *Relay) setTapLocked(ch *channel, mode int) {
	if mode < TapOff || mode > TapTranscript || ch.kind != "" {
		return
	}
	if mode < ch.tap {
		for _, p := range ch.peers {
			if mode == TapOff || p.noCap {
				r.tapFlushLocked(p)
			}
		}
	}
	if mode != ch.tap && (mode == TapTranscript || ch.tap == TapTranscript) && len(ch.peers) > 0 {
		// alle im Sprachkanal wissen sofort, ob mitgeschrieben wird
		b, _ := json.Marshal(map[string]any{"t": "tap", "mode": mode})
		r.broadcastLocked(ch, textMsg(b), nil)
	}
	ch.tap = mode
}

// --- Ogg-Opus (RFC 7845) ---

// oggChecksum: Ogg nutzt CRC-32 ohne Spiegelung (anders als crc32.IEEE).
func oggChecksum(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = crc<<8 ^ oggTable[byte(crc>>24)^c]
	}
	return crc
}

var oggTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return
}()

// OggOpus verpackt Opus-Pakete (je 20 ms, 48 kHz) als Ogg-Datei.
func OggOpus(packets [][]byte, stereo bool) []byte {
	ch := byte(1)
	if stereo {
		ch = 2
	}
	var out []byte
	serial := uint32(0x55434e47) // „UCNG“
	seq := uint32(0)
	page := func(flags byte, granule uint64, segs [][]byte) {
		var lacing []byte
		var body []byte
		for _, s := range segs {
			n := len(s)
			for n >= 255 {
				lacing = append(lacing, 255)
				n -= 255
			}
			lacing = append(lacing, byte(n))
			body = append(body, s...)
		}
		h := make([]byte, 27, 27+len(lacing)+len(body))
		copy(h, "OggS")
		h[5] = flags
		binary.LittleEndian.PutUint64(h[6:], granule)
		binary.LittleEndian.PutUint32(h[14:], serial)
		binary.LittleEndian.PutUint32(h[18:], seq)
		h[26] = byte(len(lacing))
		h = append(h, lacing...)
		h = append(h, body...)
		binary.LittleEndian.PutUint32(h[22:], oggChecksum(h))
		out = append(out, h...)
		seq++
	}
	head := []byte("OpusHead")
	head = append(head, 1, ch, 0, 0) // Version, Kanäle, Pre-Skip 0
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = append(head, 0, 0, 0) // Gain 0, Mapping 0
	page(2, 0, [][]byte{head})
	tags := []byte("OpusTags")
	tags = binary.LittleEndian.AppendUint32(tags, 4)
	tags = append(tags, "UCNG"...)
	tags = binary.LittleEndian.AppendUint32(tags, 0)
	page(0, 0, [][]byte{tags})
	var granule uint64
	for i := 0; i < len(packets); {
		// bis zu 50 Pakete (1 s) je Seite; eine Seite hat höchstens 255 Lacing-Werte
		var segs [][]byte
		lac := 0
		for i < len(packets) && len(segs) < 50 {
			need := len(packets[i])/255 + 1
			if lac+need > 255 {
				break
			}
			segs = append(segs, packets[i])
			lac += need
			granule += uint64(opusSamples(packets[i]))
			i++
		}
		flags := byte(0)
		if i >= len(packets) {
			flags = 4 // letzte Seite
		}
		page(flags, granule, segs)
	}
	return out
}

// opusSamples: Länge eines Opus-Pakets in Abtastwerten bei 48 kHz (aus dem TOC-Byte, RFC 6716).
func opusSamples(p []byte) int {
	if len(p) == 0 {
		return 960
	}
	toc := p[0]
	cfg := toc >> 3
	var frame int
	switch {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		frame = []int{480, 960, 1920, 2880}[cfg&3]
	case cfg < 16: // Hybrid: 10, 20 ms
		frame = []int{480, 960}[cfg&1]
	default: // CELT: 2,5, 5, 10, 20 ms
		frame = []int{120, 240, 480, 960}[cfg&3]
	}
	switch toc & 3 {
	case 0:
		return frame
	case 1, 2:
		return 2 * frame
	default:
		if len(p) < 2 {
			return frame
		}
		return frame * int(p[1]&0x3f)
	}
}
