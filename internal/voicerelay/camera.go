// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"encoding/binary"
	"encoding/json"
	"sort"
	"time"
)

// Kamera in Konferenzen (Simulcast): Jede Kamera schickt eine Miniansicht (Ebene 0) an alle.
// Die zuletzt sprechenden Personen (höchstens featuredN) schicken zusätzlich ein großes Bild
// (Ebene 1). Wer groß gezeigt wird, entscheidet der Relay aus den Pegeln der Sprache und sagt es
// allen ({"t":"featured","ids":[…]}) und den Betroffenen ({"t":"layer","high":true|false}).

func (ch *channel) isFeatured(idx uint16) bool {
	for _, f := range ch.featured {
		if f == idx {
			return true
		}
	}
	return false
}

func (p *peer) camKeys(from uint16) *[2]bool {
	k := p.camNeedKey[from]
	if k == nil {
		k = &[2]bool{true, true}
		p.camNeedKey[from] = k
	}
	return k
}

// camOn schaltet die Kamera einer Person an oder aus.
func (r *Relay) camOn(p *peer, on bool, codec string) {
	r.mu.Lock()
	if p.gone {
		r.mu.Unlock()
		return
	}
	ch := p.ch
	reply := map[string]any{"t": "cam", "on": false}
	changed := false
	switch {
	case !on:
		if p.cam != "" {
			r.camOffLocked(p)
			changed = true
		}
		reply = nil
	case !p.videoOK:
		reply["reason"] = "denied"
	case !validCodec(codec):
		reply["reason"] = "codec"
	default:
		if p.cam != codec {
			p.cam = codec
			changed = true
			for _, q := range ch.peers { // alle brauchen ein neues Schlüsselbild
				if q != p {
					*q.camKeys(p.idx) = [2]bool{true, true}
				}
			}
			upd, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
			r.broadcastLocked(ch, textMsg(upd), nil)
			r.refeatureLocked(ch, r.cfg.Now(), true)
		}
		reply["on"], reply["codec"] = true, codec
		reply["high"] = ch.isFeatured(p.idx)
		r.camKeyReqLocked(p, 0)
	}
	if reply != nil {
		b, _ := json.Marshal(reply)
		r.enqueueLocked(p, textMsg(b))
	}
	peers, name := r.peersLocked(ch), ch.name
	r.mu.Unlock()
	if changed {
		r.changed(name, peers)
	}
}

func (r *Relay) camOffLocked(p *peer) {
	p.cam = ""
	for _, q := range p.ch.peers {
		delete(q.camNeedKey, p.idx)
	}
	upd, _ := json.Marshal(map[string]any{"t": "peer", "peer": p.info()})
	r.broadcastLocked(p.ch, textMsg(upd), nil)
	r.enqueueLocked(p, textMsg([]byte(`{"t":"cam","on":false}`)))
	if p.ch.isFeatured(p.idx) {
		r.refeatureLocked(p.ch, r.cfg.Now(), true)
	}
}

// refeatureLocked bestimmt die großen Bilder neu. force: auch ohne neue Sprecher auffüllen
// (Kamera an/aus, jemand ging).
func (r *Relay) refeatureLocked(ch *channel, now time.Time, force bool) {
	ch.featAt = now
	var cur []uint16
	for _, f := range ch.featured {
		if q := ch.peers[f]; q != nil && q.cam != "" {
			cur = append(cur, f)
		}
	}
	var cand []*peer
	for _, q := range ch.peers {
		if q.cam != "" && !containsIdx(cur, q.idx) {
			cand = append(cand, q)
		}
	}
	sort.Slice(cand, func(i, j int) bool {
		if !cand[i].lastSpoke.Equal(cand[j].lastSpoke) {
			return cand[i].lastSpoke.After(cand[j].lastSpoke)
		}
		return cand[i].idx < cand[j].idx
	})
	next := append([]uint16(nil), cur...)
	for _, c := range cand {
		if len(next) < ch.featuredN {
			next = append(next, c.idx)
			continue
		}
		if now.Sub(c.lastSpoke) > time.Second { // spricht gerade nicht
			continue
		}
		// das am längsten stille große Bild ersetzen (wenn es lange genug still war)
		oldest := -1
		for i, f := range next {
			q := ch.peers[f]
			if now.Sub(q.lastSpoke) < featureHold {
				continue
			}
			if oldest < 0 || q.lastSpoke.Before(ch.peers[next[oldest]].lastSpoke) {
				oldest = i
			}
		}
		if oldest >= 0 {
			next[oldest] = c.idx
		}
	}
	if equalIdx(next, ch.featured) && !force {
		return
	}
	old := ch.featured
	ch.featured = next
	if equalIdx(next, old) {
		return
	}
	for _, f := range old { // nicht mehr groß
		if q := ch.peers[f]; q != nil && !containsIdx(next, f) {
			r.enqueueLocked(q, textMsg([]byte(`{"t":"layer","high":false}`)))
		}
	}
	for _, f := range next { // neu groß: alle brauchen ein großes Schlüsselbild
		if containsIdx(old, f) {
			continue
		}
		q := ch.peers[f]
		r.enqueueLocked(q, textMsg([]byte(`{"t":"layer","high":true}`)))
		for _, v := range ch.peers {
			if v != q {
				v.camKeys(f)[1] = true
			}
		}
		r.camKeyReqLocked(q, 1)
	}
	b, _ := json.Marshal(map[string]any{"t": "featured", "ids": next})
	r.broadcastLocked(ch, textMsg(b), nil)
}

func containsIdx(list []uint16, x uint16) bool {
	for _, y := range list {
		if y == x {
			return true
		}
	}
	return false
}

func equalIdx(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// camKeyReqLocked bittet um ein Schlüsselbild einer Ebene (höchstens einmal je Sekunde).
func (r *Relay) camKeyReqLocked(s *peer, layer int) {
	now := r.cfg.Now()
	if !s.camKeyRq[layer].IsZero() && now.Sub(s.camKeyRq[layer]) < keyReqGap {
		return
	}
	s.camKeyRq[layer] = now
	b, _ := json.Marshal(map[string]any{"t": "camkey", "l": layer})
	r.enqueueLocked(s, textMsg(b))
}

// camFrame verteilt ein Kamerabild. Miniansichten gehen an alle, große Bilder nur von
// Featured-Personen und nur an Verbindungen, die nicht gedrosselt sind.
func (r *Relay) camFrame(p *peer, layer int, flags uint8, seq uint16, data []byte) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := p.ch
	if p.gone || p.cam == "" || len(data) > maxVideoFrame {
		return
	}
	if layer == 1 && !ch.isFeatured(p.idx) {
		return
	}
	// Kontingent beider Ebenen: 1,5 × Bitrate, Vorrat für 2 s bzw. ein großes Schlüsselbild
	rate := float64(ch.thumbKbps+ch.videoKbps) * 1000 / 8 * 1.5
	burst := max(rate*2, 768<<10)
	if p.cRefill.IsZero() {
		p.cBytes = burst
	} else {
		p.cBytes = minF(burst, p.cBytes+now.Sub(p.cRefill).Seconds()*rate)
	}
	p.cRefill = now
	if p.cBytes < float64(len(data)) {
		for _, q := range ch.peers {
			if q != p {
				q.camKeys(p.idx)[layer] = true
			}
		}
		return
	}
	p.cBytes -= float64(len(data))
	key := flags&1 != 0
	msg := make([]byte, 7+len(data))
	msg[0] = MsgCam
	binary.BigEndian.PutUint16(msg[1:], p.idx)
	msg[3] = byte(layer)
	msg[4] = flags
	binary.BigEndian.PutUint16(msg[5:], seq)
	copy(msg[7:], data)
	missing := false
	for _, w := range ch.peers {
		if w == p {
			continue
		}
		if layer == 1 && w.highOff {
			if now.Sub(w.degradedAt) < degradeFor || len(w.out) > 4 {
				continue
			}
			w.highOff = false // wieder versuchen
			w.camKeys(p.idx)[1] = true
			r.enqueueLocked(w, textMsg([]byte(`{"t":"degraded","on":false}`)))
		}
		nk := w.camKeys(p.idx)
		if nk[layer] {
			if !key {
				missing = true
				continue
			}
			nk[layer] = false
		}
		limit := outQueue / 2
		if layer == 1 {
			limit = outQueue / 3
		}
		if len(w.out) > limit { // langsame Verbindung
			nk[layer] = true
			if layer == 1 && !w.highOff {
				w.highOff, w.degradedAt = true, now
				r.enqueueLocked(w, textMsg([]byte(`{"t":"degraded","on":true}`)))
			} else {
				missing = true
			}
			continue
		}
		r.enqueueLocked(w, msg)
	}
	if missing {
		r.camKeyReqLocked(p, layer)
	}
}
