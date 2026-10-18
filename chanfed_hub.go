// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Kanal-Föderation, Teil im Hub: Freigabe eines Kanals (Ursprung) und Spiegelkanäle
// (abonnierte Kanäle anderer Instanzen). Das Protokoll steht in chanfed.go.

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ShareRead  = "read"  // andere Instanzen lesen mit
	ShareWrite = "write" // … und schreiben mit
)

var (
	ErrShareMode       = errors.New("Freigabe muss leer, read oder write sein")
	ErrShareRestricted = errors.New("Freigeben lassen sich nur öffentliche Kanäle (ohne Leserollen, Passwort und nicht temporär)")
	ErrShareMirror     = errors.New("Ein abonnierter Kanal einer anderen Instanz lässt sich nicht weiter freigeben")
	ErrNotMirror       = errors.New("Das ist kein abonnierter Kanal einer anderen Instanz")
	ErrMirrorReadOnly  = errors.New("In diesem abonnierten Kanal kann man nur mitlesen")
	ErrMirrorEnded     = errors.New("Die andere Instanz hat diesen Kanal nicht mehr freigegeben")
	ErrMirrorFeature   = errors.New("Das geht in abonnierten Kanälen anderer Instanzen nicht")
)

// RemoteChan: Spiegelkanal – abonnierter Kanal einer anderen Instanz.
type RemoteChan struct {
	Domain  string    `json:"domain"`
	Channel string    `json:"channel"`         // Name beim Ursprung
	Mode    string    `json:"mode"`            // read oder write (meldet der Ursprung)
	Title   string    `json:"title,omitempty"` // Name der Ursprungs-Instanz
	Public  bool      `json:"public,omitempty"`
	Subs    []string  `json:"subs,omitempty"` // persönliche Abos (Nutzer-IDs)
	Ended   bool      `json:"ended,omitempty"`
	Since   time.Time `json:"since"`
	LastAt  time.Time `json:"lastAt,omitempty"` // letzte Lieferung vom Ursprung
}

// RemoteInfo: öffentliche Sicht auf einen Spiegelkanal.
type RemoteInfo struct {
	Address    string    `json:"address"` // #kanal@instanz
	Domain     string    `json:"domain"`
	Channel    string    `json:"channel"`
	Mode       string    `json:"mode"`
	Title      string    `json:"title,omitempty"`
	Public     bool      `json:"public,omitempty"`
	Subs       int       `json:"subs,omitempty"`
	Ended      bool      `json:"ended,omitempty"`
	Subscribed bool      `json:"subscribed,omitempty"` // nur in RoomsFor: persönlich abonniert
	LastAt     time.Time `json:"lastAt,omitempty"`
}

func (rc *RemoteChan) clone() *RemoteChan {
	if rc == nil {
		return nil
	}
	c := *rc
	c.Subs = append([]string(nil), rc.Subs...)
	return &c
}

func (rc *RemoteChan) info() *RemoteInfo {
	if rc == nil {
		return nil
	}
	return &RemoteInfo{Address: "#" + rc.Channel + "@" + rc.Domain, Domain: rc.Domain, Channel: rc.Channel, Mode: rc.Mode,
		Title: rc.Title, Public: rc.Public, Subs: len(rc.Subs), Ended: rc.Ended, LastAt: rc.LastAt}
}

func (rc *RemoteChan) writable() bool { return rc != nil && rc.Mode == ShareWrite && !rc.Ended }

// listedFor: Spiegelkanäle, die nur einzelne Personen abonniert haben, stehen nur bei ihnen in der Liste.
func (r *room) listedFor(a *Access) bool {
	if r.remote == nil || r.remote.Public {
		return true
	}
	return a != nil && contains(r.remote.Subs, a.ID)
}

// shareable: Darf der Kanal für andere Instanzen freigegeben sein?
func (r *room) shareable() bool {
	return r.kind == "" && r.remote == nil && r.temp == nil && r.passHash == "" && len(r.readRoles) == 0
}

// shareMode: tatsächliche Freigabe ("" = keine). Schreiben nur Rollen, gilt für andere Instanzen „lesen“.
func (r *room) shareMode() string {
	if r.share == "" || !r.shareable() {
		return ""
	}
	if r.share == ShareWrite && len(r.writeRoles) > 0 {
		return ShareRead
	}
	return r.share
}

// SetShare gibt einen Kanal für andere Instanzen frei (mode "" = nicht mehr).
func (h *Hub) SetShare(name, mode string) (RoomInfo, error) {
	switch mode {
	case "", ShareRead, ShareWrite:
	default:
		return RoomInfo{}, ErrShareMode
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.kind != "" {
		return RoomInfo{}, ErrNoRoom
	}
	if mode != "" && r.remote != nil {
		return RoomInfo{}, ErrShareMirror
	}
	if mode != "" && !r.shareable() {
		return RoomInfo{}, ErrShareRestricted
	}
	if r.share == mode {
		return r.info(), nil
	}
	r.share = mode
	h.record(storeOp{kind: opRoomMeta, room: name})
	info := r.info()
	h.broadcast(r, Event{Type: "room", RoomInfo: &info, Online: len(r.clients)})
	h.roomChangedLocked(name, name)
	return info, nil
}

// SharedRoom: Freigabe und Angaben eines Kanals für andere Instanzen.
type SharedRoom struct {
	Name        string
	Description string
	Mode        string
	Messages    int
	LastAt      time.Time
}

// Shared liefert einen freigegebenen Kanal (ok = false: gibt es nicht oder nicht freigegeben).
func (h *Hub) Shared(name string) (SharedRoom, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok {
		return SharedRoom{}, false
	}
	m := r.shareMode()
	if m == "" {
		return SharedRoom{}, false
	}
	return SharedRoom{Name: r.name, Description: r.description, Mode: m, Messages: len(r.history), LastAt: r.last}, true
}

// SharedRooms: alle freigegebenen Kanäle (nach Name).
func (h *Hub) SharedRooms() []SharedRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []SharedRoom
	for _, r := range h.rooms {
		if m := r.shareMode(); m != "" {
			out = append(out, SharedRoom{Name: r.name, Description: r.description, Mode: m, Messages: len(r.history), LastAt: r.last})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SharedHistory: die letzten n öffentlichen Nachrichten eines freigegebenen Kanals.
func (h *Hub) SharedHistory(name string, n int) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.shareMode() == "" {
		return nil
	}
	msgs, _ := r.page(0, n)
	return h.outAll(r, msgs, false)
}

// --- Spiegelkanäle ---

// Mirror: Spiegelkanal zu domain/channel ("" = keiner).
func (h *Hub) Mirror(domain, channel string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mirrorLocked(domain, channel)
}

func (h *Hub) mirrorLocked(domain, channel string) string {
	for n, r := range h.rooms {
		if r.remote != nil && r.remote.Domain == domain && r.remote.Channel == channel {
			return n
		}
	}
	return ""
}

// Remote liefert die Angaben eines Spiegelkanals.
func (h *Hub) Remote(name string) (RemoteChan, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return RemoteChan{}, false
	}
	return *r.remote.clone(), true
}

// Mirrors: alle Spiegelkanäle (lokaler Name → Angaben).
func (h *Hub) Mirrors() map[string]RemoteChan {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]RemoteChan{}
	for n, r := range h.rooms {
		if r.remote != nil {
			out[n] = *r.remote.clone()
		}
	}
	return out
}

var mirrorSlugRe = regexp.MustCompile(`[^\p{Ll}\p{Lo}\p{N}_-]+`)

// mirrorNameLocked findet einen freien Namen für einen Spiegelkanal: wunsch, sonst
// kanal-instanz, sonst mit Zähler.
func (h *Hub) mirrorNameLocked(wish, channel, domain string) (string, error) {
	if wish != "" {
		wish = NormRoom(wish)
		if !ValidRoom(wish) || isDMName(wish) {
			return "", ErrInvalidRoom
		}
		if _, ok := h.rooms[wish]; ok {
			return "", ErrRoomExists
		}
		return wish, nil
	}
	cands := []string{channel}
	slug := strings.Trim(mirrorSlugRe.ReplaceAllString(strings.ToLower(fedHost(domain)), "-"), "-")
	if c := channel + "-" + slug; len([]rune(c)) <= 32 {
		cands = append(cands, c)
	} else if c := channel + "-" + strings.SplitN(slug, "-", 2)[0]; len([]rune(c)) <= 32 {
		cands = append(cands, c)
	}
	for _, c := range cands {
		if _, ok := h.rooms[c]; !ok && ValidRoom(c) && !isDMName(c) {
			return c, nil
		}
	}
	base := []rune(cands[len(cands)-1])
	if len(base) > 28 {
		base = base[:28]
	}
	for i := 2; i < 1000; i++ {
		c := string(base) + "-" + strconv.Itoa(i)
		if _, ok := h.rooms[c]; !ok && ValidRoom(c) {
			return c, nil
		}
	}
	return "", ErrRoomExists
}

// CreateMirror legt einen Spiegelkanal an.
func (h *Hub) CreateMirror(wish string, rc RemoteChan, desc string) (string, error) {
	desc, _ = cleanDesc(desc)
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := h.mirrorLocked(rc.Domain, rc.Channel); n != "" {
		return n, nil
	}
	name, err := h.mirrorNameLocked(wish, rc.Channel, rc.Domain)
	if err != nil {
		return "", err
	}
	r, err := h.newRoomLocked(name, desc)
	if err != nil {
		return "", err
	}
	rc.Since = time.Now().UTC()
	r.remote = rc.clone()
	h.record(storeOp{kind: opRoomMeta, room: name})
	return name, nil
}

// MirrorSubs ändert die Abos eines Spiegelkanals: public (nil = unverändert), Person hinzu/weg.
// Liefert, ob noch jemand abonniert hat (sonst kann der Spiegel weg).
func (h *Hub) MirrorSubs(name string, public *bool, addUser, removeUser string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return false, ErrNotMirror
	}
	rc := r.remote
	if public != nil {
		rc.Public = *public
	}
	if addUser != "" && !contains(rc.Subs, addUser) {
		rc.Subs = append(rc.Subs, addUser)
	}
	if removeUser != "" {
		keep := rc.Subs[:0]
		for _, u := range rc.Subs {
			if u != removeUser {
				keep = append(keep, u)
			}
		}
		rc.Subs = keep
	}
	h.record(storeOp{kind: opRoomMeta, room: name})
	info := r.info()
	h.broadcast(r, Event{Type: "room", RoomInfo: &info, Online: len(r.clients)})
	return rc.Public || len(rc.Subs) > 0, nil
}

// MirrorState setzt Freigabe-Art, Beschreibung, Namen beim Ursprung oder „beendet“.
func (h *Hub) MirrorState(name string, change func(rc *RemoteChan, desc *string)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return ErrNotMirror
	}
	desc := r.description
	change(r.remote, &desc)
	if d, err := cleanDesc(desc); err == nil {
		r.description = d
	}
	h.record(storeOp{kind: opRoomMeta, room: name})
	info := r.info()
	d := r.description
	h.broadcast(r, Event{Type: "room", Description: &d, RoomInfo: &info, Online: len(r.clients)})
	return nil
}

// refIndex: ID beim Ursprung → eigene ID. h.mu muss gehalten werden.
func (r *room) refIndex() map[int64]int64 {
	if r.refs == nil {
		r.refs = map[int64]int64{}
		for _, m := range r.history {
			if m.Fed != nil && m.Fed.Ref > 0 {
				r.refs[m.Fed.Ref] = m.ID
			}
		}
	}
	return r.refs
}

// MirrorLocal: ID beim Ursprung → eigene Nachrichten-ID (0 = unbekannt).
func (h *Hub) MirrorLocal(name string, ref int64) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return 0
	}
	return r.refIndex()[ref]
}

// MirrorRef: eigene Nachrichten-ID → ID beim Ursprung (0 = unbekannt).
func (h *Hub) MirrorRef(name string, id int64) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return 0
	}
	if i, ok := r.find(id); ok && r.history[i].Fed != nil {
		return r.history[i].Fed.Ref
	}
	return 0
}

// mirrorMsg: eine Nachricht vom Ursprung, schon auf diese Instanz übertragen.
type mirrorMsg struct {
	Ref      int64
	UserID   string
	Name     string
	From     string // Instanz der Person ("" = diese)
	Bot      bool
	Text     string
	Time     time.Time
	ReplyTo  int64 // beim Ursprung
	Thread   int64 // beim Ursprung
	EditedAt *time.Time
	Deleted  bool
	Media    *Media
	Hidden   string // eigene Automod hat angeschlagen (Muster)
	Own      bool   // von dieser Instanz geschrieben (bestätigt) – Kennung darf übernommen werden
}

// MirrorPut übernimmt eine neue oder geänderte Nachricht vom Ursprung. Lokal Ausgeblendetes
// bleibt ausgeblendet; Reaktionen gibt es in Spiegelkanälen nicht.
func (h *Hub) MirrorPut(name string, in mirrorMsg) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return Message{}, ErrNotMirror
	}
	r.remote.LastAt = time.Now().UTC()
	refs := r.refIndex()
	text := in.Text
	if t, ok := cleanText(text, maxTextLen+200); ok {
		text = t
	} else if in.Media == nil {
		text = "…"
	}
	nm, ok := cleanText(strings.ReplaceAll(in.Name, "\n", " "), maxNameLen)
	if !ok {
		nm = "?"
	}
	if id, ok := refs[in.Ref]; ok { // bekannt: ändern
		i, found := r.find(id)
		if !found {
			delete(refs, in.Ref)
		} else {
			m := &r.history[i]
			localHidden := m.Deleted && (m.DeletedBy == "mod" || m.DeletedBy == "automod")
			ownFix := in.Own && (m.UserID != in.UserID || m.Fed == nil || m.Fed.From != in.From)
			if ownFix { // die Lieferung kam vor der Antwort auf das eigene Schreiben
				m.UserID, m.Fed = in.UserID, &FedMeta{Ref: in.Ref, From: in.From}
			}
			if in.Deleted && !m.Deleted {
				now := time.Now().UTC()
				m.Text, m.Media, m.Deleted, m.DeletedBy, m.DeletedAt = "", nil, true, "self", &now
			} else if !in.Deleted && !localHidden && (m.Deleted || m.Text != text || !sameTime(m.EditedAt, in.EditedAt)) {
				m.Text, m.EditedAt, m.Media = text, in.EditedAt, in.Media
				m.Deleted, m.DeletedBy, m.DeletedAt = false, "", nil // beim Ursprung wiederhergestellt
			} else if !ownFix {
				return h.out(r, *m, false), nil
			}
			h.afterChangeLocked(r, m)
			return h.out(r, *m, false), nil
		}
	}
	if in.Deleted { // gelöscht, bevor sie hier ankam
		return Message{}, nil
	}
	msg := Message{ID: h.nextID.Add(1), Room: name, UserID: in.UserID, Bot: in.Bot, Name: nm, Text: text,
		Time: in.Time, EditedAt: in.EditedAt, Media: in.Media, Fed: &FedMeta{Ref: in.Ref, From: in.From}}
	if msg.Time.IsZero() || msg.Time.After(time.Now().Add(time.Minute)) {
		msg.Time = time.Now().UTC()
	}
	if in.ReplyTo > 0 {
		msg.ReplyTo = refs[in.ReplyTo]
	}
	if in.Thread > 0 {
		if root, ok := refs[in.Thread]; ok {
			if i, ok := r.find(root); ok && r.history[i].Thread == 0 {
				msg.Thread = root
			}
		}
	}
	if in.Hidden != "" {
		t := time.Now().UTC()
		msg.Original, msg.Text, msg.Deleted, msg.DeletedBy, msg.DeletedAt, msg.Match = text, "", true, "automod", &t, in.Hidden
		h.pending[msg.ID] = name
	}
	r.history = append(r.history, msg)
	refs[in.Ref] = msg.ID
	if msg.Time.After(r.last) {
		r.last = msg.Time
	}
	if msg.Thread > 0 {
		r.addToThread(msg)
	}
	h.record(storeOp{kind: opMessage, room: name, msg: msg})
	pub := h.out(r, msg, false)
	h.broadcast(r, Event{Type: "message", Message: &pub, Online: len(r.clients)})
	h.emit(BusEvent{Type: "message", Room: name, Message: &pub})
	if msg.Thread > 0 {
		if i, ok := r.find(msg.Thread); ok {
			rp := h.out(r, r.history[i], false)
			h.broadcast(r, Event{Type: "update", Message: &rp, Online: len(r.clients)})
		}
	}
	return pub, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// MirrorRemove: Nachricht beim Ursprung endgültig gelöscht – hier auch.
func (h *Hub) MirrorRemove(name string, ref int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[name]
	if !ok || r.remote == nil {
		return
	}
	id, ok := r.refIndex()[ref]
	if !ok {
		return
	}
	h.removeLocked(r, func(x *Message) bool { return x.ID == id })
	h.broadcast(r, Event{Type: "removed", ID: id, Online: len(r.clients)})
}

// chanSubMu: Abos nacheinander anlegen und beenden (kein doppelter Spiegel).
var chanSubMu sync.Mutex
