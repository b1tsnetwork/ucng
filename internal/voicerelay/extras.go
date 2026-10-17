// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"encoding/binary"
	"encoding/json"
	"time"
	"unicode/utf8"
)

// capText kürzt einen Untertitel auf maxCapText Zeichen.
func capText(s string) string {
	if utf8.RuneCountInString(s) <= maxCapText {
		return s
	}
	return string([]rune(s)[:maxCapText])
}

// Kleine Nachrichten der Konferenz:
//
//	{"t":"react","e":"👍"}                  Reaktion – an alle im Kanal ({"t":"react","i":…,"u":…,"e":…})
//	{"t":"e2k","to":…,"ep":…,"x":"…"}       Ende-zu-Ende: verpackter Schlüssel an eine Person ({"t":"e2k","from":…,"ep":…,"x":…})
//	{"t":"pins","ids":[…]}                  Kamera: diese Personen groß sehen (angeheftet, Galerie; höchstens maxPins)
//
// Untertitel kommen nur vom Chat-Server (Steuerbefehl "cap", aus dem eigenen Whisper) – nie aus
// dem Browser. Den Inhalt von Schlüsseln versteht der Relay nicht; er begrenzt Größe und
// Häufigkeit und leitet weiter.

const (
	maxPins    = 4
	maxCapText = 500
	maxKeyText = 600
)

// Reactions: erlaubte Reaktionen.
var Reactions = map[string]bool{"👍": true, "👏": true, "😂": true, "❤️": true, "🎉": true, "😮": true, "👎": true, "🙏": true}

func validKey(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

// ctlAllow: Kontingent für kleine Nachrichten (Spitze 100, dann 20 je Sekunde) – genug, damit
// die schlüsselführende Person nach einem Wechsel allen den neuen Schlüssel schicken kann.
func (p *peer) ctlAllow(now time.Time) bool {
	if p.ctlRefill.IsZero() {
		p.ctlTok = 100
	} else {
		p.ctlTok = minF(100, p.ctlTok+now.Sub(p.ctlRefill).Seconds()*20)
	}
	p.ctlRefill = now
	if p.ctlTok < 1 {
		return false
	}
	p.ctlTok--
	return true
}

func (r *Relay) small(p *peer, m ctlMsg) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.gone || !p.ctlAllow(now) {
		return
	}
	ch := p.ch
	switch m.T {
	case "react":
		if ch.kind != "" || !Reactions[m.E] || now.Sub(p.lastReact) < 600*time.Millisecond {
			return
		}
		p.lastReact = now
		b, _ := json.Marshal(map[string]any{"t": "react", "i": p.idx, "u": p.user, "e": m.E})
		r.broadcastLocked(ch, textMsg(b), nil)
	case "e2k":
		q := ch.peers[m.To]
		if ch.kind != "" || q == nil || q == p || len(m.X) > maxKeyText || !validKey(m.X) {
			return
		}
		b, _ := json.Marshal(map[string]any{"t": "e2k", "from": p.idx, "ep": m.Ep, "x": m.X})
		r.enqueueLocked(q, textMsg(b))
	case "limit": // Sparmodus als Zuschauer
		if m.Drop != nil && *m.Drop >= 0 && *m.Drop <= 2 {
			p.drop = uint8(*m.Drop)
		}
		if m.Pause == nil || *m.Pause == p.paused {
			return
		}
		p.paused = *m.Pause
		if p.paused { // beim Fortsetzen zuerst ein Schlüsselbild abwarten
			for i := range p.watching {
				p.needKey[i] = true
			}
			for _, q := range ch.peers {
				if q != p && q.cam != "" {
					*p.camKeys(q.idx) = [2]bool{true, true}
				}
			}
		} else { // sofort ein Schlüsselbild anfordern – auch wenn gerade erst eins angefordert wurde
			// (das kam womöglich während der Pause an); höchstens einmal je Sekunde und Zuschauer
			force := now.Sub(p.resumedAt) >= keyReqGap
			p.resumedAt = now
			for i := range p.watching {
				if s := ch.peers[i]; s != nil && s.codec != "" {
					if force {
						s.lastKeyRq = time.Time{}
					}
					r.keyReqLocked(s)
				}
			}
			for _, q := range ch.peers {
				if q != p && q.cam != "" {
					if force {
						q.camKeyRq = [2]time.Time{}
					}
					r.camKeyReqLocked(q, 0)
					if q.highSent && ch.seesHigh(p, q) {
						r.camKeyReqLocked(q, 1)
					}
				}
			}
		}
		r.updateHighLocked(ch) // niemand schaut hin: großes Bild nicht mehr senden
	case "pins":
		next := map[uint16]bool{}
		for _, id := range m.IDs {
			if len(next) >= maxPins {
				break
			}
			if q := ch.peers[id]; q != nil && q != p {
				next[id] = true
			}
		}
		for id := range next {
			if !p.pins[id] { // neu angeheftet: großes Schlüsselbild nötig
				p.camKeys(id)[1] = true
			}
		}
		p.pins = next
		r.updateHighLocked(ch)
		for id := range next {
			if q := ch.peers[id]; q != nil && q.highSent {
				r.camKeyReqLocked(q, 1)
			}
		}
	}
}

// wantsHigh: Soll q das große Kamerabild senden? (groß gezeigt oder von jemandem angeheftet)
func (ch *channel) wantsHigh(q *peer) bool {
	if q.cam == "" {
		return false
	}
	feat := ch.isFeatured(q.idx)
	for _, v := range ch.peers {
		if v != q && !v.paused && (feat || v.pins[q.idx]) {
			return true
		}
	}
	return false
}

// seesHigh: Bekommt w das große Bild von q?
func (ch *channel) seesHigh(w, q *peer) bool { return ch.isFeatured(q.idx) || w.pins[q.idx] }

// updateHighLocked sagt jeder Kamera, ob sie das große Bild senden soll.
func (r *Relay) updateHighLocked(ch *channel) {
	for _, q := range ch.peers {
		want := ch.wantsHigh(q)
		if want == q.highSent {
			continue
		}
		q.highSent = want
		if want {
			r.enqueueLocked(q, textMsg([]byte(`{"t":"layer","high":true}`)))
			r.camKeyReqLocked(q, 1)
		} else {
			r.enqueueLocked(q, textMsg([]byte(`{"t":"layer","high":false}`)))
		}
	}
}

// screenTone verteilt den Ton einer Bildschirmfreigabe an ihre Zuschauer (Vorrang wie Sprache).
func (r *Relay) screenTone(p *peer, seq uint16, data []byte) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.gone || p.codec == "" {
		return
	}
	rate := float64(toneKbps) * 1000 / 8 * 1.5
	if p.tRefill.IsZero() {
		p.tBytes = rate
	} else {
		p.tBytes = minF(rate, p.tBytes+now.Sub(p.tRefill).Seconds()*rate)
	}
	p.tRefill = now
	if p.tBytes < float64(len(data)) {
		return
	}
	p.tBytes -= float64(len(data))
	if len(p.watchers) == 0 {
		return
	}
	msg := make([]byte, 5+len(data))
	msg[0] = MsgScreenTone
	binary.BigEndian.PutUint16(msg[1:], p.idx)
	binary.BigEndian.PutUint16(msg[3:], seq)
	copy(msg[5:], data)
	for _, w := range p.watchers {
		r.enqueueLocked(w, msg)
	}
}
