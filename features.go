// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Funktionen für Personen im Chat (reagieren, bearbeiten, selbst löschen, suchen,
// ungelesene Nachrichten) und für den Datenschutz (endgültig löschen, Aufbewahrungsfrist).

const EditWindow = 15 * time.Minute

var (
	ErrNotOwn       = errors.New("Das ist nicht deine Nachricht")
	ErrEditWindow   = errors.New("Nachrichten können nur in den ersten 15 Minuten bearbeitet werden")
	ErrMsgDeleted   = errors.New("Diese Nachricht wurde gelöscht")
	ErrBadReaction  = errors.New("Diese Reaktion gibt es nicht")
	ErrManyReacts   = errors.New("Diese Nachricht hat schon zu viele verschiedene Reaktionen")
	ErrSearchQuery  = errors.New("Suchbegriff: 2–100 Zeichen")
	ErrUnchanged    = errors.New("Der Text ist unverändert")
	ErrAwaitsReview = errors.New("Diese Nachricht wird gerade geprüft")
)

// maxReactionKinds begrenzt die verschiedenen Reaktionen an einer Nachricht.
const maxReactionKinds = 20

// findMsgLocked liefert Kanal und Nachricht. h.mu muss gehalten werden.
func (h *Hub) findMsgLocked(roomName string, id int64) (*room, *Message, error) {
	r, ok := h.rooms[roomName]
	if !ok {
		return nil, nil, ErrNoRoom
	}
	i, ok := r.find(id)
	if !ok {
		return nil, nil, ErrNoMessage
	}
	return r, &r.history[i], nil
}

// React setzt oder entfernt eine Reaktion (Umschalten).
func (h *Hub) React(roomName string, id int64, p Poster, emoji string) (Message, error) {
	valid := h.media.ValidReaction(emoji)
	order := map[string]int{}
	for i, e := range h.media.Reactions().Reactions {
		order[e] = i + 1
	}
	if err := h.checkSanction(p); err != nil {
		return Message{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	if !r.canRead(accessOf(p)) { // private Kanäle und fremde Direktnachrichten: wie nicht vorhanden
		return Message{}, ErrNoMessage
	}
	if r.dmBlocked() {
		return Message{}, ErrDMBlocked
	}
	if m.Deleted {
		return Message{}, ErrMsgDeleted
	}
	var out []Reaction
	found := false
	for _, re := range m.Reactions {
		users := make([]ReactUser, 0, len(re.Users))
		for _, u := range re.Users {
			if re.Emoji == emoji && u.ID == p.ID {
				found = true
				continue
			}
			users = append(users, u)
		}
		if re.Emoji == emoji && !found {
			users = append(users, ReactUser{ID: p.ID, Name: p.Name})
			found = true
		}
		if len(users) > 0 {
			out = append(out, Reaction{Emoji: re.Emoji, Users: users})
		}
	}
	if !found {
		// neue Reaktion: muss erlaubt sein (eigene, nicht mehr erlaubte lassen sich trotzdem zurücknehmen)
		if !valid {
			return Message{}, ErrBadReaction
		}
		if len(out) >= maxReactionKinds {
			return Message{}, ErrManyReacts
		}
		out = append(out, Reaction{Emoji: emoji, Users: []ReactUser{{ID: p.ID, Name: p.Name}}})
	} else if !valid && !wasMine(m.Reactions, emoji, p.ID) {
		return Message{}, ErrBadReaction
	}
	// Reihenfolge: zuerst wie in der Schnellauswahl, danach in der Reihenfolge des Hinzufügens
	rank := func(e string) int {
		if o, ok := order[e]; ok {
			return o
		}
		return len(order) + 1
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Emoji) < rank(out[j].Emoji) })
	m.Reactions = out
	h.afterChangeLocked(r, m)
	return h.out(r, *m, false), nil
}

func wasMine(rs []Reaction, emoji, userID string) bool {
	for _, re := range rs {
		if re.Emoji == emoji {
			for _, u := range re.Users {
				if u.ID == userID {
					return true
				}
			}
		}
	}
	return false
}

// EditOwn ändert den Text einer eigenen Nachricht (innerhalb von 15 Minuten). Die Automod
// prüft den neuen Text; frühere Fassungen sieht nur der Admin-Bereich.
func (h *Hub) EditOwn(roomName string, id int64, p Poster, text string) (Message, error) {
	text, ok := cleanText(text, maxTextLen)
	if !ok {
		return Message{}, ErrInvalidText
	}
	if err := h.checkSanction(p); err != nil {
		return Message{}, err
	}
	var pattern string
	var hit bool
	if !p.NoAutomod {
		pattern, hit = h.checkAutomod(roomName, text)
	}
	unfiltered := ""
	if !hit && !p.Bot {
		if t, changed := h.applyFilter(text); changed {
			unfiltered, text = text, t
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	if m.UserID == "" || m.UserID != p.ID || m.Bot != p.Bot {
		return Message{}, ErrNotOwn
	}
	if m.AwaitingReview() {
		return Message{}, ErrAwaitsReview
	}
	if m.Deleted {
		return Message{}, ErrMsgDeleted
	}
	if !p.Bot && time.Since(m.Time) > EditWindow { // Bots dürfen z. B. Statusmeldungen später aktualisieren
		return Message{}, ErrEditWindow
	}
	if m.E2E != "" {
		return Message{}, ErrE2ERequired
	}
	if m.Text == text {
		return Message{}, ErrUnchanged
	}
	if !linkRe.MatchString(m.Text) {
		if err := h.checkRoomRules(r, p, text, true); err != nil {
			return Message{}, err
		}
	}
	now := time.Now().UTC()
	m.Edits = append(m.Edits, m.Text)
	m.EditedAt = &now
	if hit {
		m.Original, m.Text, m.Deleted, m.DeletedBy, m.DeletedAt, m.Match, m.Reviewed = text, "", true, "automod", &now, pattern, false
	} else {
		m.Text, m.Unfiltered = text, unfiltered
	}
	h.afterChangeLocked(r, m)
	return h.out(r, *m, false), nil
}

// DeleteOwn löscht eine eigene Nachricht. Der Text wird dabei wirklich entfernt
// (auch für Admins) – im Verlauf bleibt nur „Nachricht gelöscht“.
func (h *Hub) DeleteOwn(roomName string, id int64, userID string) (Message, error) {
	return h.deleteOwn(roomName, id, userID, false)
}

// DeleteOwnBot löscht eine eigene Nachricht eines Bots.
func (h *Hub) DeleteOwnBot(roomName string, id int64, botID string) (Message, error) {
	return h.deleteOwn(roomName, id, botID, true)
}

func (h *Hub) deleteOwn(roomName string, id int64, userID string, bot bool) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	if m.UserID == "" || m.UserID != userID || m.Bot != bot {
		return Message{}, ErrNotOwn
	}
	if m.Deleted && m.DeletedBy != "automod" {
		return Message{}, ErrMsgDeleted
	}
	now := time.Now().UTC()
	m.Text, m.Original, m.Edits, m.Match, m.Reactions, m.Media, m.Poll, m.Unfiltered, m.E2E = "", "", nil, "", nil, nil, nil, "", ""
	m.Deleted, m.DeletedBy, m.DeletedAt, m.Reviewed, m.ModBy = true, "self", &now, true, ""
	h.afterChangeLocked(r, m)
	h.record(storeOp{kind: opRoomRewrite, room: r.name}) // frühere Fassungen auch aus der Datei entfernen
	return h.out(r, *m, false), nil
}

// removeLocked entfernt Nachrichten endgültig aus einem Kanal und schreibt die Datei neu.
func (h *Hub) removeLocked(r *room, drop func(*Message) bool) []int64 {
	var removed []int64
	keep := r.history[:0]
	for i := range r.history {
		m := r.history[i]
		if drop(&m) {
			removed = append(removed, m.ID)
			delete(h.pending, m.ID)
			continue
		}
		keep = append(keep, m)
	}
	// Rest der alten Slice freigeben (Texte nicht im Speicher halten)
	for i := len(keep); i < len(r.history); i++ {
		r.history[i] = Message{}
	}
	r.history = keep
	if len(removed) > 0 {
		r.rebuildThreads()
	}
	if len(removed) > 0 && len(r.pins) > 0 {
		h.prunePinsLocked(r)
		h.record(storeOp{kind: opRoomMeta, room: r.name})
	}
	if len(removed) > 0 {
		if n := len(r.history); n > 0 {
			r.last = r.history[n-1].Time
		}
		h.record(storeOp{kind: opRoomRewrite, room: r.name})
	}
	return removed
}

// PurgeMessage löscht eine Nachricht endgültig – auch aus der Moderation und der Datei.
func (h *Hub) PurgeMessage(roomName string, id int64) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	gone := *m
	h.removeLocked(r, func(x *Message) bool { return x.ID == id })
	h.broadcast(r, Event{Type: "removed", ID: id, Online: len(r.clients)})
	h.emit(BusEvent{Type: "removed", Room: roomName, Message: &Message{ID: id, Room: roomName}})
	// Textauszüge dieser Nachricht auch aus dem Protokoll entfernen (Nachrichten-IDs sind eindeutig).
	h.audit.Scrub(func(e *AuditEntry) bool { return e.Message == id && e.Detail != "" },
		func(e *AuditEntry) { e.Detail = "" })
	return gone, nil
}

// PurgeUser löscht alle Nachrichten und Reaktionen einer Person endgültig.
// Liefert die Anzahl der gelöschten Nachrichten.
func (h *Hub) PurgeUser(userID string) int {
	if userID == "" {
		return 0
	}
	// Name und Textauszüge der Person auch aus dem Protokoll entfernen (die ID bleibt als Beleg).
	defer h.audit.Scrub(func(e *AuditEntry) bool { return e.UserID == userID && (e.Detail != "" || e.UserName != "") },
		func(e *AuditEntry) {
			e.UserName = ""
			if strings.HasPrefix(e.Action, "message.") {
				e.Detail = ""
			}
		})
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, r := range h.rooms {
		removed := h.removeLocked(r, func(m *Message) bool { return m.UserID == userID })
		total += len(removed)
		reacted := false
		for i := range r.history {
			m := &r.history[i]
			var out []Reaction
			changed := false
			for _, re := range m.Reactions {
				users := re.Users[:0:0]
				for _, u := range re.Users {
					if u.ID == userID {
						changed = true
						continue
					}
					users = append(users, u)
				}
				if len(users) > 0 {
					out = append(out, Reaction{Emoji: re.Emoji, Users: users})
				}
			}
			if changed {
				m.Reactions = out
				reacted = true
			}
		}
		if reacted && len(removed) == 0 {
			h.record(storeOp{kind: opRoomRewrite, room: r.name})
		}
		if len(removed) > 0 || reacted {
			h.broadcast(r, Event{Type: "user_updated", User: userID, Online: len(r.clients)})
		}
	}
	return total
}

// ApplyRetention löscht alle Nachrichten, die älter als days Tage sind, endgültig.
func (h *Hub) ApplyRetention(days int, now time.Time) int {
	if days <= 0 {
		return 0
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	h.mu.Lock()
	total := 0
	for _, r := range h.rooms {
		if len(r.history) == 0 || !r.history[0].Time.Before(cutoff) {
			continue
		}
		total += len(h.removeLocked(r, func(m *Message) bool { return m.Time.Before(cutoff) }))
	}
	h.mu.Unlock()
	h.audit.Trim(cutoff)
	return total
}

// HasPostedIn: Hat die Person in einem der erlaubten Kanäle geschrieben?
func (h *Hub) HasPostedIn(userID string, allowed func(string) bool) bool {
	if userID == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, r := range h.rooms {
		if !allowed(name) {
			continue
		}
		for i := len(r.history) - 1; i >= 0; i-- {
			if r.history[i].UserID == userID {
				return true
			}
		}
	}
	return false
}

// --- Suche ---

// SearchHit ist ein Suchtreffer.
type SearchHit struct {
	Room    string  `json:"room"`
	Message Message `json:"message"`
}

func normSearch(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Search durchsucht einen Kanal (roomName = "" nur für Admins: alle Kanäle), neueste zuerst.
// admin = true durchsucht auch Originaltexte gelöschter Nachrichten und Namen.
func (h *Hub) Search(roomName, q string, admin bool, limit int) ([]SearchHit, error) {
	q = normSearch(q)
	if n := len([]rune(q)); n < 2 || n > 100 {
		return nil, ErrSearchQuery
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var rooms []*room
	if roomName != "" {
		r, ok := h.rooms[roomName]
		if !ok {
			return nil, ErrNoRoom
		}
		rooms = []*room{r}
	} else if admin {
		for _, r := range h.rooms {
			rooms = append(rooms, r)
		}
	} else {
		return nil, ErrNoRoom
	}
	hits := []SearchHit{}
	for _, r := range rooms {
		n := 0
		for i := len(r.history) - 1; i >= 0 && n < limit; i-- {
			m := r.history[i]
			hay := m.Text
			if admin {
				hay += "\n" + m.Original + "\n" + m.Name
			} else if m.Deleted {
				continue
			}
			if strings.Contains(normSearch(hay), q) {
				hits = append(hits, SearchHit{Room: r.name, Message: h.out(r, m, admin)})
				n++
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Message.ID > hits[j].Message.ID })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// --- Ungelesen ---

type UnreadInfo struct {
	Unread   int `json:"unread"`
	Mentions int `json:"mentions"`
}

// Mentions: Kommt @name im Text vor (ohne Groß-/Kleinschreibung, als ganzes Wort)?
func Mentions(text, name string) bool {
	if name == "" {
		return false
	}
	lt, needle := strings.ToLower(text), "@"+strings.ToLower(name)
	for from := 0; ; {
		i := strings.Index(lt[from:], needle)
		if i < 0 {
			return false
		}
		end := from + i + len(needle)
		if end >= len(lt) {
			return true
		}
		r := []rune(lt[end:])[0]
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return true
		}
		from = end
	}
}

// MentionsAny: wird einer der Namen erwähnt (z. B. @Rolle)?
func MentionsAny(text string, names []string) bool {
	for _, n := range names {
		if Mentions(text, n) {
			return true
		}
	}
	return false
}

// Unread zählt je Kanal Nachrichten nach der zuletzt gelesenen ID (ohne eigene und gelöschte).
// Kanäle ohne Eintrag in since werden nicht gezählt.
func (h *Hub) Unread(since map[string]int64, a *Access, userID, name string, roleNames []string) map[string]UnreadInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]UnreadInfo{}
	for rn, last := range since {
		r, ok := h.rooms[NormRoom(rn)]
		if !ok || !r.canRead(a) {
			continue
		}
		var info UnreadInfo
		if f := r.dmFloor(userID); f > last {
			last = f
		}
		start, _ := r.find(last + 1)
		for i := start; i < len(r.history); i++ {
			m := r.history[i]
			if m.Deleted || (userID != "" && m.UserID == userID) {
				continue
			}
			if userID != "" && m.UserID != "" && h.blocks.Has(userID, m.UserID) {
				continue // blockierte Personen zählen nicht
			}
			info.Unread++
			if Mentions(m.Text, name) || MentionsAny(m.Text, roleNames) {
				info.Mentions++
			}
		}
		if r.kind == RoomDM { // Direktnachrichten zählen wie Erwähnungen
			info.Mentions = info.Unread
		}
		out[r.name] = info
	}
	return out
}

// --- Statistik ---

type DayStat struct {
	Day      string `json:"day"` // JJJJ-MM-TT in der eingestellten Zeitzone
	Messages int    `json:"messages"`
	Users    int    `json:"users"`   // verschiedene Personen, die geschrieben haben
	Automod  int    `json:"automod"` // Automod-Treffer
}

type RoomStat struct {
	Room     string `json:"room"`
	Messages int    `json:"messages"`
}

type Stats struct {
	Days       []DayStat  `json:"days"`
	TopRooms   []RoomStat `json:"topRooms"`
	Rooms      int        `json:"rooms"`
	Messages   int        `json:"messages"` // insgesamt gespeichert
	Online     int        `json:"online"`
	Pending    int        `json:"pending"`
	Period     int        `json:"period"`     // Nachrichten im Zeitraum
	ModDeleted int        `json:"modDeleted"` // im Zeitraum moderativ gelöscht
}

func (h *Hub) Stats(days int, now time.Time) Stats {
	if days <= 0 || days > 365 {
		days = 30
	}
	loc := h.Location()
	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	first := today.AddDate(0, 0, -(days - 1))
	st := Stats{Days: make([]DayStat, days)}
	idx := map[string]int{}
	users := make([]map[string]bool, days)
	for i := 0; i < days; i++ {
		d := first.AddDate(0, 0, i).Format("2006-01-02")
		st.Days[i] = DayStat{Day: d}
		idx[d] = i
		users[i] = map[string]bool{}
	}
	h.mu.Lock()
	st.Rooms = len(h.rooms)
	st.Pending = len(h.pending)
	for _, r := range h.rooms {
		st.Messages += len(r.history)
		st.Online += len(r.clients)
		rs := RoomStat{Room: r.name}
		start := sort.Search(len(r.history), func(i int) bool { return !r.history[i].Time.Before(first) })
		for _, m := range r.history[start:] {
			i, ok := idx[m.Time.In(loc).Format("2006-01-02")]
			if !ok {
				continue
			}
			st.Days[i].Messages++
			rs.Messages++
			if m.Match != "" {
				st.Days[i].Automod++
			}
			if m.Deleted && (m.DeletedBy == "mod" || m.DeletedBy == "bot") {
				st.ModDeleted++
			}
			if !m.Bot {
				key := m.UserID
				if key == "" {
					key = "n:" + strings.ToLower(m.Name)
				}
				users[i][key] = true
			}
		}
		if rs.Messages > 0 {
			if r.kind != RoomDM { // Direktnachrichten nicht namentlich in der Übersicht
				st.TopRooms = append(st.TopRooms, rs)
			}
		}
	}
	h.mu.Unlock()
	for i := range st.Days {
		st.Days[i].Users = len(users[i])
		st.Period += st.Days[i].Messages
	}
	sort.Slice(st.TopRooms, func(i, j int) bool {
		if st.TopRooms[i].Messages != st.TopRooms[j].Messages {
			return st.TopRooms[i].Messages > st.TopRooms[j].Messages
		}
		return st.TopRooms[i].Room < st.TopRooms[j].Room
	})
	if len(st.TopRooms) > 8 {
		st.TopRooms = st.TopRooms[:8]
	}
	if st.TopRooms == nil {
		st.TopRooms = []RoomStat{}
	}
	return st
}
