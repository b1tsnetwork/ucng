// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"ucng/internal/voicerelay"
)

// Lastverteilung innerhalb eines Kanals: Die Sprache bleibt auf dem Relay, dem der Kanal
// zugewiesen ist. Bildschirm und Kameras laufen über eigene Verbindungen („Medienverbindungen“)
// und können auf andere Relays ausgelagert werden – der Bildschirm zuerst (ab
// VOICE_OFFLOAD_SCREEN Prozent Auslastung), die Kameras danach (VOICE_OFFLOAD_VIDEO).
//
// Entschieden wird, wenn die erste Person eines Kanals eine Medienverbindung braucht, und erneut,
// wenn jemand zu senden beginnt, während in dieser Art gerade niemand sendet: Dann kann die Art
// ohne Unterbrechung umziehen – die Browser im Kanal verbinden sich neu („media“-Nachricht).
// Ausgelagerte Bandbreite zählt beim Sprach-Relay sofort nicht mehr; er nimmt also wieder mehr
// Personen auf.

var (
	ErrMediaKind    = errors.New("Unbekannte Medienart")
	ErrMediaOff     = errors.New("In diesem Kanal ist das nicht eingeschaltet")
	ErrMediaNoVoice = errors.New("Zuerst dem Sprachkanal beitreten")
)

type relayView struct {
	id, url string
	role    string
	media   bool // kann Medienverbindungen (aktuelle Fassung)
	st      voicerelay.Stats
}

// relaysLocked: alle erreichbaren Relays (v.mu muss gehalten werden).
func (v *voiceMgr) relaysLocked() []relayView {
	var out []relayView
	if v.builtin != nil {
		out = append(out, relayView{id: builtinRelay, media: true, st: v.builtin.Stats()})
	}
	now := time.Now()
	ids := make([]string, 0, len(v.agents))
	for id := range v.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := v.agents[id]
		if now.Sub(a.Seen) >= voiceAgentStale {
			continue
		}
		rv := relayView{id: a.ID, url: a.WSURL, role: a.Role, st: a.Stats}
		for _, f := range a.Stats.Features {
			if f == "media" {
				rv.media = true
			}
		}
		out = append(out, rv)
	}
	return out
}

// mediaCapable: Gibt es überhaupt einen Relay für Medienverbindungen? (Sonst laufen Bildschirm
// und Kameras wie früher über die Sprachverbindung.)
func (v *voiceMgr) mediaCapable() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, r := range v.relaysLocked() {
		if r.media {
			return true
		}
	}
	return false
}

// mediaNeedMbit: geschätzter Bedarf einer Medienart im Kanal (alle sehen zu).
func mediaNeedMbit(kind string, cfg VoiceConfig, peers int) float64 {
	if kind == voicerelay.KindScreen {
		return float64((cfg.screenPreset().Kbps+160)*max(1, peers-1)) * 1.05 / 1000 // Bild und Ton
	}
	vp := cfg.videoPreset()
	others := float64(max(1, peers-1))
	return others * (float64(cfg.Featured*vp.Kbps) + float64(min(peers, 9))*float64(thumbPreset.Kbps)) * 1.05 / 1000
}

func loadOf(st voicerelay.Stats, extra float64) float64 {
	if st.MaxMbit <= 0 {
		return 2
	}
	return (st.EstimateMbit + extra) / st.MaxMbit
}

// chooseMediaLocked: Relay für eine Medienart. Der Sprach-Relay behält sie, solange er danach
// unter der Schwelle bleibt; sonst kommt sie auf den Relay mit der meisten Luft (reine
// Medien-Relays zuerst).
// discount: Bandbreite, die auf einem Relay schon für diese Art eingerechnet ist (zählt nicht doppelt).
func (v *voiceMgr) chooseMediaLocked(audio, kind string, need float64, discount map[string]float64) (relayView, bool) {
	threshold := float64(v.s.cfg.VoiceOffloadScreen) / 100
	if kind == voicerelay.KindVideo {
		threshold = float64(v.s.cfg.VoiceOffloadVideo) / 100
	}
	relays := v.relaysLocked()
	for i := range relays {
		relays[i].st.EstimateMbit = max(0, relays[i].st.EstimateMbit-discount[relays[i].id])
	}
	var home *relayView
	for i := range relays {
		if relays[i].id == audio && relays[i].media {
			home = &relays[i]
		}
	}
	if home != nil && loadOf(home.st, need) <= threshold {
		return *home, true
	}
	var best *relayView
	better := func(c *relayView) bool {
		if best == nil {
			return true
		}
		if (c.role == "media") != (best.role == "media") {
			return c.role == "media"
		}
		return loadOf(c.st, need) < loadOf(best.st, need)
	}
	for i := range relays {
		c := &relays[i]
		if !c.media || c.id == audio || loadOf(c.st, need) > 1 {
			continue
		}
		if better(c) {
			best = c
		}
	}
	if best != nil && (home == nil || loadOf(best.st, need) < loadOf(home.st, need)) {
		return *best, true
	}
	if home != nil {
		return *home, true
	}
	// notfalls irgendein Relay mit Medienverbindungen
	for i := range relays {
		if relays[i].media && (best == nil || loadOf(relays[i].st, 0) < loadOf(best.st, 0)) {
			best = &relays[i]
		}
	}
	if best == nil {
		return relayView{}, false
	}
	return *best, true
}

func (v *voiceMgr) relayByIDLocked(id string) (relayView, bool) {
	for _, r := range v.relaysLocked() {
		if r.id == id && r.media {
			return r, true
		}
	}
	return relayView{}, false
}

// pickMedia: Relay für die Medienverbindung einer Person. publish: sie will gleich senden.
// moved: die Art ist umgezogen – alle anderen müssen sich neu verbinden.
func (v *voiceMgr) pickMedia(room, kind string, cfg VoiceConfig, publish bool) (rv relayView, moved bool, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := v.rooms[room]
	if st == nil || len(st.peers) == 0 {
		return rv, false, ErrMediaNoVoice
	}
	need := mediaNeedMbit(kind, cfg, len(st.peers))
	cur, ok := st.media[kind]
	if ok {
		r, alive := v.relayByIDLocked(cur)
		if alive {
			if !publish || v.publishersLocked(r, room, kind) > 0 {
				return r, false, nil
			}
			// niemand sendet gerade: neu entscheiden (der eigene Anteil zählt nicht doppelt)
			ms := r.st.Media[voicerelay.MediaKey(room, kind)]
			next, found := v.chooseMediaLocked(st.relay, kind, need, map[string]float64{r.id: ms.Mbit})
			if !found || next.id == r.id {
				return r, false, nil
			}
			st.media[kind] = next.id
			return next, true, nil
		}
	}
	next, found := v.chooseMediaLocked(st.relay, kind, need, nil)
	if !found {
		return rv, false, ErrVoiceBusy
	}
	if st.media == nil {
		st.media = map[string]string{}
	}
	st.media[kind] = next.id
	return next, ok, nil // ok: war schon zugewiesen (Relay weg) – die anderen müssen umziehen
}

func (v *voiceMgr) publishersLocked(r relayView, room, kind string) int {
	return r.st.Media[voicerelay.MediaKey(room, kind)].Pub
}

// mediaRelaysLocked: Relays, auf denen Medienverbindungen des Kanals laufen (ohne doppelte).
func (st *voiceRoom) mediaRelays() []string {
	var out []string
	for _, id := range st.media {
		dup := false
		for _, o := range out {
			dup = dup || o == id
		}
		if !dup {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// POST /api/voice/{room}/media {"kind":"screen"|"video","publish":true} – Zutrittskarte für eine
// Medienverbindung (nur, wer schon im Sprachkanal ist).
func (s *Server) handleVoiceMedia(w http.ResponseWriter, r *http.Request) {
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
	if !v.mediaRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Versuche – bitte kurz warten.")
		return
	}
	var in struct {
		Kind    string `json:"kind"`
		Publish bool   `json:"publish"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Kind != voicerelay.KindScreen && in.Kind != voicerelay.KindVideo {
		writeErr(w, http.StatusBadRequest, ErrMediaKind.Error())
		return
	}
	room := NormRoom(r.PathValue("room"))
	inVoice := false
	for _, rp := range v.findUser(me.ID) {
		inVoice = inVoice || rp.room == room
	}
	if !inVoice {
		writeErr(w, http.StatusConflict, ErrMediaNoVoice.Error())
		return
	}
	d, err := v.decide(room, s.voiceWhoOf(me, s.hub.Mod().IPHash(s.clientIP(r))))
	if err != nil {
		voiceFail(w, err)
		return
	}
	if in.Kind == voicerelay.KindScreen && d.Cfg.Screen == ScreenOff || in.Kind == voicerelay.KindVideo && d.Cfg.Video == ScreenOff {
		writeErr(w, http.StatusNotFound, ErrMediaOff.Error())
		return
	}
	rv, moved, err := v.pickMedia(room, in.Kind, d.Cfg, in.Publish)
	if err != nil {
		voiceFail(w, err)
		return
	}
	if moved { // die anderen im Kanal verbinden sich neu
		v.control(room, "", "remedia", false, in.Kind+"@"+rv.id)
	}
	sp, vp := d.Cfg.screenPreset(), d.Cfg.videoPreset()
	t := voicerelay.Ticket{Channel: room, Kind: in.Kind, User: me.ID, Name: me.Name,
		// Speak bleibt aus: ältere Relays ohne Medienverbindungen sähen darin nur eine stumme Zuhörerin
		MaxSpeakers: 1, MaxPeers: d.Cfg.MaxPeers, Bitrate: 6000, Featured: d.Cfg.Featured, ModUser: d.Mod}
	if in.Kind == voicerelay.KindScreen {
		t.Screen, t.ScreenKbps, t.MaxScreens = d.Screen, sp.Kbps, d.Cfg.MaxScreens
	} else {
		t.Video, t.VideoKbps, t.ThumbKbps = d.Video, vp.Kbps, thumbPreset.Kbps
	}
	u := rv.url
	if u == "" {
		u = "/api/voice/ws"
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": u, "ticket": voicerelay.SignTicket(v.priv, t, time.Now()),
		"kind": in.Kind, "relay": rv.id, "offloaded": rv.id != v.audioRelay(room)})
}

func (v *voiceMgr) audioRelay(room string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if st := v.rooms[room]; st != nil {
		return st.relay
	}
	return ""
}
