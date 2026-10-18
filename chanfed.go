// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Kanal-Föderation: Kanäle für andere Instanzen freigeben und Kanäle anderer Instanzen abonnieren.
//
//   - Ursprung: Der Admin gibt einen öffentlichen Kanal frei – „lesen“ oder „mitschreiben“
//     (SetShare, chanfed_hub.go). GET /api/fed/channels listet die freigegebenen Kanäle.
//   - Abonnent: abonniert #kanal@ursprung.example – der Admin für alle oder jede Person für sich
//     (wenn erlaubt). Die Instanz legt dafür einen Spiegelkanal an, einen je abonniertem Kanal.
//   - Anmelden/Abmelden, Schreiben, Bearbeiten und Löschen schickt der Abonnent signiert an
//     POST /api/fed/channels/{kanal}/{subscribe|unsubscribe|post|edit|delete} des Ursprungs.
//     Geschrieben wird beim Ursprung: Dort gelten Freigabe, Langsam-Modus, Automod und Sperren.
//   - Der Ursprung verteilt jede neue/geänderte Nachricht signiert an POST /api/fed/channels/inbox
//     jedes Abonnenten – über die Warteschlange der Föderation (mit Wiederholungen, je Instanz in
//     der richtigen Reihenfolge). Antwortet ein Abonnent 410, ist das Abo dort beendet.
//
// Schlüssel und Signaturen wie bei der Föderation (Ed25519, TOFU, federation.go). Geht in beiden
// Betriebsarten; nötig sind PUBLIC_URL und der Schalter im Admin-Bereich.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ucng/internal/fedsig"
)

const (
	chanInboxPath      = "/api/fed/channels/inbox"
	chanAPIPath        = "/api/fed/channels/"
	chanFedDoc         = "channelfed.json"
	chanHistoryLen     = 50  // so viele Nachrichten bekommt ein neuer Abonnent
	maxChanMirrors     = 200 // abonnierte Kanäle je Instanz
	maxChanSubscribers = 500 // abonnierende Instanzen je Kanal
)

var (
	ErrChanFedOff   = errors.New("Kanal-Föderation ist auf dieser Instanz ausgeschaltet")
	ErrChanAddr     = errors.New("Adresse bitte im Format #kanal@instanz.example")
	ErrChanOwn      = errors.New("Das ist ein Kanal dieser Instanz")
	ErrChanNotFound = errors.New("Diesen Kanal gibt es dort nicht, oder er ist nicht für andere Instanzen freigegeben")
	ErrChanMirrors  = errors.New("Höchstzahl abonnierter Kanäle erreicht")
	ErrChanUserSubs = errors.New("Kanäle anderer Instanzen abonniert auf dieser Instanz nur der Admin")
	ErrChanMember   = errors.New("Dafür brauchst du ein Konto")
	ErrChanNotSub   = errors.New("Diese Instanz hat den Kanal nicht abonniert")
	ErrChanFull     = errors.New("Diesen Kanal haben schon zu viele Instanzen abonniert")
)

// --- Einstellungen und Abonnenten (channelfed.json) ---

type ChanFedConfig struct {
	Enabled  bool     `json:"enabled"`
	UserSubs bool     `json:"userSubs"` // Personen dürfen selbst abonnieren
	Allow    []string `json:"allow"`    // leer = alle (außer Deny)
	Deny     []string `json:"deny"`
}

func (c ChanFedConfig) Allowed(domain string) bool {
	return FedConfig{Allow: c.Allow, Deny: c.Deny}.Allowed(domain)
}

type chanSub struct {
	Since time.Time `json:"since"`
	Last  time.Time `json:"last,omitempty"`  // letzte erfolgreiche Zustellung
	Err   string    `json:"error,omitempty"` // letzter Fehler
}

type chanFedState struct {
	Config ChanFedConfig                  `json:"config"`
	Subs   map[string]map[string]*chanSub `json:"subscribers"` // Kanal → Instanz → Abo
}

// ChanFed hält Einstellungen und Abonnenten freigegebener Kanäle.
type ChanFed struct {
	mu      sync.Mutex
	cfg     ChanFedConfig
	subs    map[string]map[string]*chanSub
	pushed  map[int64]uint64 // zuletzt verteilte Fassung je Nachricht (keine doppelten Lieferungen)
	changed func()
}

func NewChanFed() *ChanFed {
	return &ChanFed{subs: map[string]map[string]*chanSub{}, pushed: map[int64]uint64{}}
}

func (c *ChanFed) touch() {
	if c.changed != nil {
		c.changed()
	}
}

func (c *ChanFed) state() chanFedState {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := chanFedState{Config: c.cfg, Subs: map[string]map[string]*chanSub{}}
	for ch, m := range c.subs {
		st.Subs[ch] = map[string]*chanSub{}
		for d, s := range m {
			cp := *s
			st.Subs[ch][d] = &cp
		}
	}
	return st
}

func (c *ChanFed) restore(st chanFedState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = st.Config
	c.subs = map[string]map[string]*chanSub{}
	for ch, m := range st.Subs {
		for d, s := range m {
			if s == nil {
				continue
			}
			if c.subs[ch] == nil {
				c.subs[ch] = map[string]*chanSub{}
			}
			c.subs[ch][d] = s
		}
	}
}

func (c *ChanFed) Config() ChanFedConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.cfg
	out.Allow = append([]string{}, c.cfg.Allow...)
	out.Deny = append([]string{}, c.cfg.Deny...)
	return out
}

func (c *ChanFed) SetConfig(in ChanFedConfig) (ChanFedConfig, error) {
	var err error
	if in.Allow, err = cleanDomainList(in.Allow); err != nil {
		return ChanFedConfig{}, err
	}
	if in.Deny, err = cleanDomainList(in.Deny); err != nil {
		return ChanFedConfig{}, err
	}
	c.mu.Lock()
	c.cfg = in
	c.mu.Unlock()
	c.touch()
	return c.Config(), nil
}

func (c *ChanFed) addSub(ch, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.subs[ch]
	if m == nil {
		m = map[string]*chanSub{}
		c.subs[ch] = m
	}
	if _, ok := m[domain]; !ok {
		if len(m) >= maxChanSubscribers {
			return ErrChanFull
		}
		m[domain] = &chanSub{Since: time.Now().UTC()}
		defer c.touch()
	}
	return nil
}

func (c *ChanFed) removeSub(ch, domain string) bool {
	c.mu.Lock()
	m := c.subs[ch]
	_, ok := m[domain]
	delete(m, domain)
	if len(m) == 0 {
		delete(c.subs, ch)
	}
	c.mu.Unlock()
	if ok {
		c.touch()
	}
	return ok
}

func (c *ChanFed) hasSub(ch, domain string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.subs[ch][domain]
	return ok
}

func (c *ChanFed) domains(ch string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.subs[ch]))
	for d := range c.subs[ch] {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// dropChannel entfernt alle Abonnenten eines Kanals und liefert sie.
func (c *ChanFed) dropChannel(ch string) []string {
	out := c.domains(ch)
	c.mu.Lock()
	delete(c.subs, ch)
	c.mu.Unlock()
	if len(out) > 0 {
		c.touch()
	}
	return out
}

func (c *ChanFed) renameChannel(from, to string) {
	c.mu.Lock()
	if m, ok := c.subs[from]; ok {
		delete(c.subs, from)
		c.subs[to] = m
	}
	c.mu.Unlock()
	c.touch()
}

// note merkt Erfolg oder Fehler der letzten Zustellung (gespeichert nur, wenn sich der Fehler ändert).
func (c *ChanFed) note(ch, domain string, err error) {
	c.mu.Lock()
	s := c.subs[ch][domain]
	if s == nil {
		c.mu.Unlock()
		return
	}
	msg := ""
	if err != nil {
		msg = clip200(err.Error())
	} else {
		s.Last = time.Now().UTC()
	}
	changed := s.Err != msg
	s.Err = msg
	c.mu.Unlock()
	if changed {
		c.touch()
	}
}

func clip200(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// SubInfo: eine abonnierende Instanz (Admin-Bereich).
type SubInfo struct {
	Domain string    `json:"domain"`
	Since  time.Time `json:"since"`
	Last   time.Time `json:"last,omitempty"`
	Error  string    `json:"error,omitempty"`
}

func (c *ChanFed) subsOf(ch string) []SubInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []SubInfo{}
	for d, s := range c.subs[ch] {
		out = append(out, SubInfo{Domain: d, Since: s.Since, Last: s.Last, Error: s.Err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// fresh: Hat sich die Nachricht seit der letzten Lieferung geändert? (Reaktionen z. B. nicht.)
func (c *ChanFed) fresh(cm chanMsg) bool {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%v\x00%v\x00", cm.Text, cm.Deleted, cm.EditedAt)
	if cm.Media != nil {
		h.Write([]byte(cm.Media.Src))
	}
	sum := h.Sum64()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pushed[cm.ID] == sum {
		return false
	}
	if len(c.pushed) > 20000 {
		c.pushed = map[int64]uint64{}
	}
	c.pushed[cm.ID] = sum
	return true
}

// --- Datenformat zwischen Instanzen ---

// chanMsg: eine Nachricht eines freigegebenen Kanals.
type chanMsg struct {
	ID       int64      `json:"id"`
	User     string     `json:"user,omitempty"` // Kennung beim Ursprung; Gäste anderer Instanzen: fed:<instanz>/<id>
	Name     string     `json:"name"`
	From     string     `json:"from"` // Instanz der Person
	Bot      bool       `json:"bot,omitempty"`
	Text     string     `json:"text"`
	Time     time.Time  `json:"time"`
	ReplyTo  int64      `json:"replyTo,omitempty"`
	Thread   int64      `json:"thread,omitempty"`
	EditedAt *time.Time `json:"editedAt,omitempty"`
	Deleted  bool       `json:"deleted,omitempty"`
	Media    *Media     `json:"media,omitempty"`
}

// chanInfo: Angaben zu einem freigegebenen Kanal.
type chanInfo struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Mode        string    `json:"mode"`
	Title       string    `json:"title,omitempty"` // Name der Instanz
	Messages    int       `json:"messages,omitempty"`
	LastAt      time.Time `json:"lastAt,omitempty"`
}

// chanEvent: eine Lieferung an den Eingang eines Abonnenten.
type chanEvent struct {
	Type    string    `json:"type"`    // message, update, removed, info, end
	Channel string    `json:"channel"` // Name beim Ursprung (bei info nach Umbenennen: der alte)
	Msg     *chanMsg  `json:"msg,omitempty"`
	ID      int64     `json:"id,omitempty"` // removed
	Info    *chanInfo `json:"info,omitempty"`
}

type chanSubscribeResp struct {
	Info     chanInfo  `json:"info"`
	Messages []chanMsg `json:"messages"`
}

type chanWriteReq struct {
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Text    string `json:"text,omitempty"`
	ID      int64  `json:"id,omitempty"` // edit, delete
	ReplyTo int64  `json:"replyTo,omitempty"`
	Thread  int64  `json:"thread,omitempty"`
}

var chanUserRe = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,80}$`)

// --- Grundlagen ---

func (s *Server) chanFedOn() bool {
	return s.fedDomain() != "" && s.hub.chanFed.Config().Enabled
}

func (s *Server) chanCheckDomain(domain string) error {
	if !s.chanFedOn() {
		return ErrChanFedOff
	}
	if strings.Contains(domain, ":") && !s.cfg.FedInsecure {
		return ErrFedPort
	}
	if !s.hub.chanFed.Config().Allowed(domain) {
		return ErrFedBlocked
	}
	return nil
}

// parseChanAddr zerlegt #kanal@instanz.example.
func parseChanAddr(a string) (channel, domain string, err error) {
	a = strings.TrimSpace(a)
	if u, e := url.Parse(a); e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Fragment != "" {
		a = u.Fragment + "@" + u.Host // https://instanz.example/#kanal
	}
	a = strings.TrimPrefix(a, "#")
	at := strings.LastIndex(a, "@")
	if at <= 0 {
		return "", "", ErrChanAddr
	}
	channel = NormRoom(a[:at])
	if !ValidRoom(channel) || isDMName(channel) {
		return "", "", ErrChanAddr
	}
	if _, domain, err = parseFedAddr("xx@" + a[at+1:]); err != nil {
		return "", "", ErrChanAddr
	}
	return channel, domain, nil
}

type chanRemoteErr struct {
	Status int
	Msg    string
}

func (e *chanRemoteErr) Error() string { return e.Msg }

// chanCall schickt eine signierte Anfrage an eine andere Instanz und liest die Antwort.
func (s *Server) chanCall(ctx context.Context, domain, path string, in, out any) error {
	if _, err := s.peerKey(ctx, domain, false); err != nil {
		return err
	}
	body, _ := json.Marshal(in)
	target := domain + path
	ts, sig := s.hub.Fed().sign(s.fedDomain(), "POST", target, body, time.Now())
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", s.fedScheme()+"://"+target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	fedsig.SetHeaders(req.Header, s.fedDomain(), ts, sig)
	res, err := s.fedClient().Do(req)
	if err != nil {
		if errors.Is(err, ErrFedPrivate) {
			return ErrFedPrivate
		}
		return ErrFedRemote
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("%s: Status %d", domain, res.StatusCode)
		} else {
			e.Error = clip200(e.Error) + " (" + domain + ")"
		}
		return &chanRemoteErr{Status: res.StatusCode, Msg: e.Error}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return ErrFedRemote
		}
	}
	return nil
}

// chanSigned prüft eine signierte Anfrage einer anderen Instanz.
func (s *Server) chanSigned(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	if !s.chanFedOn() {
		writeErr(w, http.StatusNotFound, ErrChanFedOff.Error())
		return "", nil, false
	}
	domain, ts, sig := fedsig.FromRequest(r)
	domain = strings.ToLower(domain)
	if domain == "" || ts == "" || sig == "" {
		writeErr(w, http.StatusUnauthorized, ErrFedSig.Error())
		return "", nil, false
	}
	if _, d, err := parseFedAddr("xx@" + domain); err != nil || d != domain {
		writeErr(w, http.StatusBadRequest, "Ungültige Domain")
		return "", nil, false
	}
	if err := s.chanCheckDomain(domain); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return "", nil, false
	}
	if !s.chanRL.Allow("d:" + domain) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anfragen")
		return "", nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, fedMaxBody+1))
	if err != nil || len(body) > fedMaxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "Zu groß")
		return "", nil, false
	}
	now := time.Now()
	if !fedsig.Fresh(ts, now) {
		writeErr(w, http.StatusUnauthorized, "Zeitstempel ungültig (Uhrzeit prüfen)")
		return "", nil, false
	}
	key, err := s.peerKey(r.Context(), domain, false)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return "", nil, false
	}
	if !fedVerify(key, domain, ts, "POST", s.fedDomain()+r.URL.Path, body, sig) {
		if _, err := s.peerKey(r.Context(), domain, true); err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return "", nil, false
		}
		writeErr(w, http.StatusUnauthorized, ErrFedSig.Error())
		return "", nil, false
	}
	if s.hub.Fed().seenSig(sig, now) {
		writeErr(w, http.StatusConflict, "Anfrage schon verarbeitet")
		return "", nil, false
	}
	return domain, body, true
}

// chanQueue stellt eine Lieferung an eine andere Instanz in die Warteschlange der Föderation.
func (s *Server) chanQueue(domain, path string, payload any, channel string) {
	body, _ := json.Marshal(payload)
	f := s.hub.Fed()
	now := time.Now().UTC()
	f.mu.Lock()
	if len(f.queue) >= fedMaxQueueLen {
		f.mu.Unlock()
		log.Printf("Kanal-Föderation: Warteschlange voll – Lieferung an %s verworfen", domain)
		return
	}
	f.queue = append(f.queue, fedJob{ID: randomHex(8), Domain: domain, Path: path, Chan: channel, Body: body, Next: now, Created: now})
	f.mu.Unlock()
	f.touch()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// --- Ursprung ---

// chanMsgOf übersetzt eine Nachricht eines freigegebenen Kanals für andere Instanzen.
func (s *Server) chanMsgOf(m Message) chanMsg {
	cm := chanMsg{ID: m.ID, User: m.UserID, Name: m.Name, From: s.fedDomain(), Bot: m.Bot, Text: m.Text, Time: m.Time,
		ReplyTo: m.ReplyTo, Thread: m.Thread, EditedAt: m.EditedAt, Deleted: m.Deleted}
	if m.Fed != nil && m.Fed.From != "" && strings.HasPrefix(m.UserID, fedPrefix) {
		cm.From = m.Fed.From
	}
	if m.Deleted || m.E2E != "" {
		cm.Text, cm.Deleted = "", true
		return cm
	}
	if m.Poll != nil {
		cm.Text = strings.TrimSpace(cm.Text + "\n📊 " + m.Poll.Question)
	}
	if md := m.Media; md != nil && !strings.Contains(md.Src, privateUploadPrefix) {
		src := md.Src
		if strings.HasPrefix(src, "/") {
			src = s.baseURL() + src
		}
		if strings.HasPrefix(src, "https://") || (s.cfg.FedInsecure && strings.HasPrefix(src, "http://")) {
			c := *md
			c.Src = src
			cm.Media = &c
		}
	}
	return cm
}

func (s *Server) chanInfoOf(sh SharedRoom) chanInfo {
	return chanInfo{Name: sh.Name, Description: sh.Description, Mode: sh.Mode, Title: s.hub.Settings().Title, Messages: sh.Messages, LastAt: sh.LastAt}
}

// runChanFed verteilt Nachrichten freigegebener Kanäle an die abonnierenden Instanzen.
func (s *Server) runChanFed(ctx context.Context) {
	bus := s.hub.Listen(4096)
	defer s.hub.Unlisten(bus)
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-bus:
			s.chanOnBus(ev)
		}
	}
}

func (s *Server) chanOnBus(ev BusEvent) {
	if ev.Message == nil || !s.chanFedOn() {
		return
	}
	subs := s.hub.chanFed.domains(ev.Room)
	if len(subs) == 0 {
		return
	}
	if _, ok := s.hub.Shared(ev.Room); !ok {
		return
	}
	e := chanEvent{Type: ev.Type, Channel: ev.Room}
	switch ev.Type {
	case "message", "update":
		cm := s.chanMsgOf(*ev.Message)
		if !s.hub.chanFed.fresh(cm) {
			return
		}
		e.Msg = &cm
	case "removed":
		e.ID = ev.Message.ID
	default:
		return
	}
	for _, d := range subs {
		s.chanQueue(d, chanInboxPath, e, ev.Room)
	}
}

// chanRoomChanged: Kanal geändert, umbenannt oder gelöscht – Abonnenten Bescheid geben.
func (s *Server) chanRoomChanged(oldName, newName string) {
	if oldName != newName || newName == "" {
		s.hub.Beacon().Poke()
	}
	subs := s.hub.chanFed.domains(oldName)
	if len(subs) == 0 {
		if newName != "" {
			if _, ok := s.hub.Shared(newName); ok || oldName == newName {
				s.hub.Beacon().Poke() // Freigabe geändert: Verzeichnis aktualisieren
			}
		}
		return
	}
	end := func() {
		for _, d := range s.hub.chanFed.dropChannel(oldName) {
			s.chanQueue(d, chanInboxPath, chanEvent{Type: "end", Channel: oldName}, "")
		}
		s.hub.Beacon().Poke()
	}
	if newName == "" {
		end()
		return
	}
	sh, ok := s.hub.Shared(newName)
	if !ok {
		end()
		return
	}
	if oldName != newName {
		s.hub.chanFed.renameChannel(oldName, newName)
	}
	info := s.chanInfoOf(sh)
	for _, d := range subs {
		s.chanQueue(d, chanInboxPath, chanEvent{Type: "info", Channel: oldName, Info: &info}, newName)
	}
	s.hub.Beacon().Poke()
}

// GET /api/fed/channels – freigegebene Kanäle dieser Instanz (öffentlich)
func (s *Server) handleChanList(w http.ResponseWriter, r *http.Request) {
	if !s.chanFedOn() {
		writeErr(w, http.StatusNotFound, ErrChanFedOff.Error())
		return
	}
	if !s.fedRL.Allow("cl:" + s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anfragen")
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	list := []chanInfo{}
	for _, sh := range s.hub.SharedRooms() {
		list = append(list, s.chanInfoOf(sh))
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": s.fedDomain(), "title": s.hub.Settings().Title, "channels": list})
}

// POST /api/fed/channels/{channel}/{action} – signiert von der abonnierenden Instanz
func (s *Server) handleChanAction(w http.ResponseWriter, r *http.Request) {
	channel, action := NormRoom(r.PathValue("channel")), r.PathValue("action")
	domain, body, ok := s.chanSigned(w, r)
	if !ok {
		return
	}
	sh, shared := s.hub.Shared(channel)
	if action == "unsubscribe" {
		s.hub.chanFed.removeSub(channel, domain)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !shared {
		writeErr(w, http.StatusNotFound, ErrChanNotFound.Error())
		return
	}
	if action == "subscribe" {
		if err := s.hub.chanFed.addSub(channel, domain); err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		out := chanSubscribeResp{Info: s.chanInfoOf(sh), Messages: []chanMsg{}}
		for _, m := range s.hub.SharedHistory(channel, chanHistoryLen) {
			cm := s.chanMsgOf(m)
			s.hub.chanFed.fresh(cm)
			out.Messages = append(out.Messages, cm)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if !s.hub.chanFed.hasSub(channel, domain) {
		writeErr(w, http.StatusForbidden, ErrChanNotSub.Error())
		return
	}
	var in chanWriteReq
	if err := json.Unmarshal(body, &in); err != nil || !chanUserRe.MatchString(in.User) {
		writeErr(w, http.StatusBadRequest, "Ungültige Anfrage")
		return
	}
	poster := Poster{ID: fedPrefix + domain + "/" + in.User, Name: in.Name}
	if !s.chanRL.Allow("u:" + poster.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Nachrichten – bitte kurz warten.")
		return
	}
	var msg Message
	var err error
	switch action {
	case "post":
		if sh.Mode != ShareWrite {
			writeErr(w, http.StatusForbidden, ErrMirrorReadOnly.Error())
			return
		}
		msg, err = s.hub.SendFull(channel, poster, SendInput{Text: in.Text, ReplyTo: in.ReplyTo, Thread: in.Thread, Fed: &FedMeta{From: domain}})
	case "edit":
		if sh.Mode != ShareWrite {
			writeErr(w, http.StatusForbidden, ErrMirrorReadOnly.Error())
			return
		}
		msg, err = s.hub.EditOwn(channel, in.ID, poster, in.Text)
	case "delete":
		msg, err = s.hub.DeleteOwn(channel, in.ID, poster.ID)
	default:
		writeErr(w, http.StatusNotFound, "Unbekannte Aktion")
		return
	}
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.chanMsgOf(msg))
}

// --- Abonnent ---

// mirrorOf überträgt eine Nachricht des Ursprungs auf diese Instanz. Personen bekommen die
// Kennung fed:<instanz>/<id> – auch eigene (die Instanz des Ursprungs könnte sonst Nachrichten
// im Namen von Personen dieser Instanz unterschieben).
//
// own: Die Nachricht hat diese Instanz selbst geschrieben (Antwort auf ihr eigenes post/edit).
// Sonst gilt eine Nachricht, die „von dieser Instanz“ sein will, als Angabe des Ursprungs: Sie
// bekommt eine eigene Kennung und das Kennzeichen des Ursprungs – niemand kann sie bearbeiten,
// und sie erscheint nicht als eigene Nachricht.
func (s *Server) mirrorOf(cm chanMsg, origin string, own bool) mirrorMsg {
	from := strings.ToLower(cm.From)
	if from == "" {
		from = origin
	}
	uid := cm.User
	if !strings.HasPrefix(uid, fedPrefix) {
		uid = fedPrefix + origin + "/" + uid
	}
	if from == origin && !strings.HasPrefix(uid, fedPrefix+origin+"/") {
		from = strings.SplitN(strings.TrimPrefix(uid, fedPrefix), "/", 2)[0]
	}
	self := s.fedDomain()
	if from == self && !own {
		uid, from = fedPrefix+origin+"/"+strings.TrimPrefix(uid, fedPrefix), origin
	}
	if from != self && !strings.HasPrefix(uid, fedPrefix+from+"/") && !strings.HasPrefix(uid, fedPrefix+origin+"/") {
		uid = fedPrefix + origin + "/" + strings.TrimPrefix(uid, fedPrefix) // Kennung passt nicht zur Instanz
	}
	mm := mirrorMsg{Ref: cm.ID, UserID: uid, Name: cm.Name, From: from, Bot: cm.Bot, Text: cm.Text, Time: cm.Time,
		ReplyTo: cm.ReplyTo, Thread: cm.Thread, EditedAt: cm.EditedAt, Deleted: cm.Deleted, Own: own}
	if md := cm.Media; md != nil && (strings.HasPrefix(md.Src, "https://") || (s.cfg.FedInsecure && strings.HasPrefix(md.Src, "http://"))) {
		mm.Media = &Media{Kind: md.Kind, Src: md.Src, W: md.W, H: md.H, Title: clip200(md.Title), Source: "remote", Credit: "via " + origin}
		if mm.Media.Kind != "gif" {
			mm.Media.Kind = "image"
		}
	}
	return mm
}

// mirrorPut übernimmt eine Nachricht – mit der eigenen Automoderation.
func (s *Server) mirrorPut(room string, cm chanMsg, origin string, own bool) {
	mm := s.mirrorOf(cm, origin, own)
	if !mm.Deleted && mm.Text != "" {
		if p, hit := s.hub.checkAutomod(room, mm.Text); hit {
			mm.Hidden = p
		}
	}
	s.hub.MirrorPut(room, mm)
}

// POST /api/fed/channels/inbox – Lieferungen eines Ursprungs
func (s *Server) handleChanInbox(w http.ResponseWriter, r *http.Request) {
	domain, body, ok := s.chanSigned(w, r)
	if !ok {
		return
	}
	var e chanEvent
	if err := json.Unmarshal(body, &e); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	room := s.hub.Mirror(domain, NormRoom(e.Channel))
	if room == "" {
		writeErr(w, http.StatusGone, ErrChanNotSub.Error()) // der Ursprung beendet das Abo
		return
	}
	switch e.Type {
	case "message", "update":
		if e.Msg == nil || e.Msg.ID <= 0 {
			writeErr(w, http.StatusBadRequest, "Nachricht fehlt")
			return
		}
		s.mirrorPut(room, *e.Msg, domain, false)
	case "removed":
		s.hub.MirrorRemove(room, e.ID)
	case "info":
		if e.Info == nil {
			writeErr(w, http.StatusBadRequest, "Angaben fehlen")
			return
		}
		s.hub.MirrorState(room, func(rc *RemoteChan, desc *string) {
			if e.Info.Mode == ShareRead || e.Info.Mode == ShareWrite {
				rc.Mode = e.Info.Mode
			}
			if n := NormRoom(e.Info.Name); ValidRoom(n) && !isDMName(n) {
				rc.Channel = n
			}
			if t := strings.TrimSpace(e.Info.Title); t != "" {
				rc.Title = clip200(t)
			}
			*desc = e.Info.Description
			rc.Ended = false
		})
	case "end":
		s.hub.MirrorState(room, func(rc *RemoteChan, _ *string) { rc.Ended = true })
	default:
		writeErr(w, http.StatusBadRequest, "Unbekannter Typ")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// chanSubscribe abonniert #kanal@instanz: public = für alle (Admin), userID = für diese Person.
func (s *Server) chanSubscribe(ctx context.Context, address, wish string, public bool, userID string) (string, error) {
	if !s.chanFedOn() {
		return "", ErrChanFedOff
	}
	channel, domain, err := parseChanAddr(address)
	if err != nil {
		return "", err
	}
	if domain == s.fedDomain() {
		return "", ErrChanOwn
	}
	if err := s.chanCheckDomain(domain); err != nil {
		return "", err
	}
	chanSubMu.Lock()
	defer chanSubMu.Unlock()
	var pub *bool
	if public {
		pub = &public
	}
	if name := s.hub.Mirror(domain, channel); name != "" {
		if rc, _ := s.hub.Remote(name); rc.Ended { // vorher beendet: neu anmelden
			if err := s.chanResubscribe(ctx, name, rc); err != nil {
				return "", err
			}
		}
		_, err := s.hub.MirrorSubs(name, pub, userID, "")
		return name, err
	}
	if len(s.hub.Mirrors()) >= maxChanMirrors {
		return "", ErrChanMirrors
	}
	var resp chanSubscribeResp
	if err := s.chanCall(ctx, domain, chanAPIPath+channel+"/subscribe", map[string]string{"title": s.hub.Settings().Title}, &resp); err != nil {
		var re *chanRemoteErr
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			return "", ErrChanNotFound
		}
		return "", err
	}
	rc := RemoteChan{Domain: domain, Channel: channel, Mode: resp.Info.Mode, Title: clip200(resp.Info.Title), Public: public}
	if rc.Mode != ShareWrite {
		rc.Mode = ShareRead
	}
	if userID != "" {
		rc.Subs = []string{userID}
	}
	name, err := s.hub.CreateMirror(wish, rc, resp.Info.Description)
	if err != nil {
		s.chanQueue(domain, chanAPIPath+channel+"/unsubscribe", struct{}{}, "")
		return "", err
	}
	for _, m := range resp.Messages {
		s.mirrorPut(name, m, domain, false)
	}
	return name, nil
}

func (s *Server) chanResubscribe(ctx context.Context, name string, rc RemoteChan) error {
	var resp chanSubscribeResp
	if err := s.chanCall(ctx, rc.Domain, chanAPIPath+rc.Channel+"/subscribe", map[string]string{"title": s.hub.Settings().Title}, &resp); err != nil {
		return err
	}
	s.hub.MirrorState(name, func(x *RemoteChan, desc *string) {
		x.Ended, x.Mode = false, resp.Info.Mode
		if x.Mode != ShareWrite {
			x.Mode = ShareRead
		}
		*desc = resp.Info.Description
	})
	for _, m := range resp.Messages {
		s.mirrorPut(name, m, rc.Domain, false)
	}
	return nil
}

// chanMirrorGone: Spiegelkanal gelöscht – beim Ursprung abmelden.
func (s *Server) chanMirrorGone(rc RemoteChan) {
	if !rc.Ended {
		s.chanQueue(rc.Domain, chanAPIPath+rc.Channel+"/unsubscribe", struct{}{}, "")
	}
}

// chanUnsubscribeUser: persönliches Abo beenden; ohne weitere Abos verschwindet der Spiegel.
func (s *Server) chanUnsubscribeUser(name, userID string) error {
	chanSubMu.Lock()
	defer chanSubMu.Unlock()
	still, err := s.hub.MirrorSubs(name, nil, "", userID)
	if err != nil {
		return err
	}
	if !still {
		return s.hub.DeleteRoom(name)
	}
	return nil
}

// mirrorWrite: schreiben, bearbeiten oder löschen in einem Spiegelkanal (geht an den Ursprung).
func (s *Server) mirrorWrite(ctx context.Context, room string, rc RemoteChan, action string, me Identity, in chanWriteReq) (Message, error) {
	if rc.Ended {
		return Message{}, ErrMirrorEnded
	}
	if action != "delete" && rc.Mode != ShareWrite {
		return Message{}, ErrMirrorReadOnly
	}
	if s.hosted() && !me.Member {
		return Message{}, ErrChanMember
	}
	in.User, in.Name = me.ID, me.Name
	var cm chanMsg
	if err := s.chanCall(ctx, rc.Domain, chanAPIPath+rc.Channel+"/"+action, in, &cm); err != nil {
		var re *chanRemoteErr
		if errors.As(err, &re) && re.Status == http.StatusNotFound {
			return Message{}, ErrChanNotFound
		}
		return Message{}, err
	}
	s.mirrorPut(room, cm, rc.Domain, true) // sofort zeigen; die Lieferung des Ursprungs folgt (ohne doppelt)
	if id := s.hub.MirrorLocal(room, cm.ID); id > 0 {
		if m, ok := s.hub.Message(room, id); ok {
			return m.public(), nil
		}
	}
	return Message{}, nil
}

// mirrorOwnMsg: eigene Nachricht im Spiegelkanal → ID beim Ursprung.
func (s *Server) mirrorOwnMsg(room string, id int64, me Identity) (int64, error) {
	m, ok := s.hub.Message(room, id)
	if !ok || m.Fed == nil || m.Fed.Ref == 0 {
		return 0, ErrNoMessage
	}
	if m.UserID != fedPrefix+s.fedDomain()+"/"+me.ID {
		return 0, ErrNotOwn
	}
	return m.Fed.Ref, nil
}

// --- Abos von Personen ---

// POST /api/channels/remote {"address": "#kanal@instanz"} – für sich abonnieren
func (s *Server) handleChanUserSubscribe(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	if !s.hub.chanFed.Config().UserSubs {
		writeErr(w, http.StatusForbidden, ErrChanUserSubs.Error())
		return
	}
	if s.hosted() && !me.Member {
		fail(w, ErrChanMember)
		return
	}
	var in struct {
		Address string `json:"address"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.fedRL.Allow("cs:" + me.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anfragen – bitte kurz warten.")
		return
	}
	name, err := s.chanSubscribe(r.Context(), in.Address, "", false, me.ID)
	if err != nil {
		writeErr(w, chanStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"room": name})
}

// DELETE /api/channels/remote/{room} – persönliches Abo beenden
func (s *Server) handleChanUserUnsubscribe(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	if err := s.chanUnsubscribeUser(NormRoom(r.PathValue("room")), me.ID); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/channels/remote/browse?domain=… – freigegebene Kanäle einer Instanz
func (s *Server) handleChanBrowse(w http.ResponseWriter, r *http.Request) {
	if !s.chanBrowseAllowed(w, r) {
		return
	}
	s.chanBrowse(w, r)
}

// GET /api/channels/remote/directory?q=… – freigegebene Kanäle aus dem Beacon-Verzeichnis
func (s *Server) handleChanDirectory(w http.ResponseWriter, r *http.Request) {
	if !s.chanBrowseAllowed(w, r) {
		return
	}
	s.chanDirectory(w, r)
}

func (s *Server) chanBrowseAllowed(w http.ResponseWriter, r *http.Request) bool {
	me, _, ok := s.me(w, r)
	if !ok {
		return false
	}
	if !s.hub.chanFed.Config().UserSubs {
		writeErr(w, http.StatusForbidden, ErrChanUserSubs.Error())
		return false
	}
	if !s.fedRL.Allow("cb:" + me.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anfragen – bitte kurz warten.")
		return false
	}
	return true
}

type chanListing struct {
	Address    string    `json:"address"`
	Domain     string    `json:"domain"`
	Name       string    `json:"name"`
	Desc       string    `json:"description"`
	Mode       string    `json:"mode"`
	Title      string    `json:"title,omitempty"`
	Messages   int       `json:"messages,omitempty"`
	LastAt     time.Time `json:"lastAt,omitempty"`
	Trust      string    `json:"trust,omitempty"`    // Einstufung der Instanz durch den Beacon
	Language   string    `json:"language,omitempty"` // Sprache der Instanz
	Subscribed string    `json:"room,omitempty"`     // schon abonniert: lokaler Kanal
}

func (s *Server) chanBrowse(w http.ResponseWriter, r *http.Request) {
	if !s.chanFedOn() {
		fail(w, ErrChanFedOff)
		return
	}
	_, domain, err := parseFedAddr("xx@" + strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(r.URL.Query().Get("domain"), "https://"), "http://")))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Bitte die Adresse der Instanz angeben, z. B. chat.example.org")
		return
	}
	domain = strings.TrimSuffix(domain, "/")
	if err := s.chanCheckDomain(domain); err != nil {
		writeErr(w, chanStatus(err), err.Error())
		return
	}
	var out struct {
		Title    string     `json:"title"`
		Channels []chanInfo `json:"channels"`
	}
	if err := s.fedGetJSON(r.Context(), s.fedScheme()+"://"+domain+"/api/fed/channels", &out); err != nil {
		if errors.Is(err, ErrFedNoUser) {
			err = errors.New("Diese Instanz gibt keine Kanäle frei (oder ist keine UCNG-Instanz)")
		}
		writeErr(w, chanStatus(err), err.Error())
		return
	}
	list := []chanListing{}
	for _, c := range out.Channels {
		n := NormRoom(c.Name)
		if !ValidRoom(n) {
			continue
		}
		list = append(list, chanListing{Address: "#" + n + "@" + domain, Domain: domain, Name: n, Desc: clip200(c.Description),
			Mode: c.Mode, Title: clip200(out.Title), Messages: c.Messages, LastAt: c.LastAt, Subscribed: s.hub.Mirror(domain, n)})
		if len(list) >= 200 {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": domain, "title": out.Title, "channels": list})
}

func (s *Server) chanDirectory(w http.ResponseWriter, r *http.Request) {
	if !s.chanFedOn() {
		fail(w, ErrChanFedOff)
		return
	}
	cfg := s.hub.Beacon().Get()
	var beacons []string
	seen := map[string]bool{}
	for _, u := range append([]string{cfg.URL}, beaconURLs(cfg.Subs)...) {
		if u != "" && !seen[u] {
			seen[u] = true
			beacons = append(beacons, u)
		}
	}
	if len(beacons) == 0 {
		writeErr(w, http.StatusNotFound, "Kein Beacon eingerichtet (Admin → Beacon)")
		return
	}
	q := url.Values{"q": {strings.TrimSpace(r.URL.Query().Get("q"))}}
	list := []chanListing{}
	var lastErr error
	for _, b := range beacons {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", b+"/api/v1/channels?"+q.Encode(), nil)
		res, err := beaconClient.Do(req)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("Beacon %s nicht erreichbar", b)
			continue
		}
		var out struct {
			Channels []chanListing `json:"channels"`
		}
		if res.StatusCode == http.StatusOK {
			json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&out)
		} else {
			lastErr = fmt.Errorf("Beacon %s: Status %d (zu alt für das Kanal-Verzeichnis?)", b, res.StatusCode)
		}
		res.Body.Close()
		cancel()
		for _, c := range out.Channels {
			ch, d, err := parseChanAddr(c.Address)
			if err != nil || d == s.fedDomain() || !s.hub.chanFed.Config().Allowed(d) {
				continue
			}
			c.Address, c.Domain, c.Name = "#"+ch+"@"+d, d, ch
			c.Desc, c.Title = clip200(c.Desc), clip200(c.Title)
			c.Subscribed = s.hub.Mirror(d, ch)
			list = append(list, c)
		}
	}
	if len(list) == 0 && lastErr != nil {
		writeErr(w, http.StatusBadGateway, lastErr.Error())
		return
	}
	// doppelte (mehrere Beacons) entfernen
	uniq := list[:0]
	dup := map[string]bool{}
	for _, c := range list {
		if !dup[c.Address] {
			dup[c.Address] = true
			uniq = append(uniq, c)
		}
	}
	if len(uniq) > 200 {
		uniq = uniq[:200]
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": uniq})
}

func beaconURLs(subs []BeaconSub) []string {
	var out []string
	for _, b := range subs {
		out = append(out, b.URL)
	}
	return out
}

func chanStatus(err error) int {
	var re *chanRemoteErr
	switch {
	case errors.As(err, &re):
		if re.Status >= 400 && re.Status < 500 {
			return re.Status
		}
		return http.StatusBadGateway
	case errors.Is(err, ErrChanAddr), errors.Is(err, ErrChanOwn), errors.Is(err, ErrInvalidRoom):
		return http.StatusBadRequest
	case errors.Is(err, ErrChanNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrChanFedOff), errors.Is(err, ErrChanUserSubs), errors.Is(err, ErrChanMember), errors.Is(err, ErrChanMirrors), errors.Is(err, ErrRoomExists), errors.Is(err, ErrTooMany):
		return http.StatusConflict
	}
	for _, e := range []error{ErrFedRemote, ErrFedKeyChange, ErrFedPrivate, ErrFedBlocked, ErrFedNoUser, ErrFedAddress, ErrFedPort, ErrFedSig} {
		if errors.Is(err, e) {
			return fedStatus(err)
		}
	}
	return statusFor(err)
}

// --- Admin ---

type sharedAdmin struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Mode        string    `json:"mode"`
	Subscribers []SubInfo `json:"subscribers"`
}

type mirrorAdmin struct {
	Room string `json:"room"`
	RemoteInfo
	Users []string `json:"users,omitempty"` // Namen der Personen mit persönlichem Abo
}

// GET /api/admin/channelfed
func (s *Server) handleAdminChanFed(w http.ResponseWriter, r *http.Request) {
	shared := []sharedAdmin{}
	for _, sh := range s.hub.SharedRooms() {
		shared = append(shared, sharedAdmin{Name: sh.Name, Description: sh.Description, Mode: sh.Mode, Subscribers: s.hub.chanFed.subsOf(sh.Name)})
	}
	mirrors := []mirrorAdmin{}
	for name, rc := range s.hub.Mirrors() {
		ma := mirrorAdmin{Room: name, RemoteInfo: *rc.info()}
		for _, id := range rc.Subs {
			if u, ok := s.users.Get(id); ok {
				ma.Users = append(ma.Users, u.Name)
			}
		}
		mirrors = append(mirrors, ma)
	}
	sort.Slice(mirrors, func(i, j int) bool { return mirrors[i].Room < mirrors[j].Room })
	queued := 0
	f := s.hub.Fed()
	f.mu.Lock()
	for _, j := range f.queue {
		if j.Path != "" {
			queued++
		}
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"config": s.hub.chanFed.Config(), "on": s.chanFedOn(), "domain": s.fedDomain(),
		"publicUrl": s.cfg.PublicURL, "shared": shared, "mirrors": mirrors, "queued": queued})
}

// PUT /api/admin/channelfed – Einstellungen
func (s *Server) handleAdminChanFedSet(w http.ResponseWriter, r *http.Request) {
	var in ChanFedConfig
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Enabled && s.fedDomain() == "" {
		writeErr(w, http.StatusBadRequest, "Dafür braucht die Instanz PUBLIC_URL (öffentliche Adresse)")
		return
	}
	out, err := s.hub.chanFed.SetConfig(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "channelfed.settings", Detail: fmt.Sprintf("an=%v, Personen=%v", out.Enabled, out.UserSubs)})
	s.hub.Beacon().Poke()
	writeJSON(w, http.StatusOK, out)
}

// PUT /api/admin/rooms/{room}/share {"mode": "read"|"write"|""}
func (s *Server) handleAdminRoomShare(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	room := NormRoom(r.PathValue("room"))
	info, err := s.hub.SetShare(room, in.Mode)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "channelfed.share", Room: room, Detail: orDash(in.Mode)})
	writeJSON(w, http.StatusOK, info)
}

func orDash(s string) string {
	if s == "" {
		return "aus"
	}
	return s
}

// DELETE /api/admin/channelfed/shared/{room}/{domain} – Abo einer Instanz beenden
func (s *Server) handleAdminChanKick(w http.ResponseWriter, r *http.Request) {
	room, domain := NormRoom(r.PathValue("room")), strings.ToLower(r.PathValue("domain"))
	if !s.hub.chanFed.removeSub(room, domain) {
		writeErr(w, http.StatusNotFound, ErrChanNotSub.Error())
		return
	}
	s.chanQueue(domain, chanInboxPath, chanEvent{Type: "end", Channel: room}, "")
	s.log(r, AuditEntry{Action: "channelfed.kick", Room: room, Detail: domain})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/admin/channelfed/subscribe {"address", "name", "public"}
func (s *Server) handleAdminChanSubscribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Public  *bool  `json:"public"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	public := in.Public == nil || *in.Public
	name, err := s.chanSubscribe(r.Context(), in.Address, in.Name, public, "")
	if err != nil {
		writeErr(w, chanStatus(err), err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "channelfed.subscribe", Room: name, Detail: in.Address})
	writeJSON(w, http.StatusOK, map[string]string{"room": name})
}

// PATCH /api/admin/channelfed/mirrors/{room} {"public": bool}
func (s *Server) handleAdminMirrorUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Public bool `json:"public"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	room := NormRoom(r.PathValue("room"))
	if _, err := s.hub.MirrorSubs(room, &in.Public, "", ""); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	s.hub.Notify("rooms")
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/admin/channelfed/browse, /directory – wie für Personen
func (s *Server) handleAdminChanBrowse(w http.ResponseWriter, r *http.Request) { s.chanBrowse(w, r) }
func (s *Server) handleAdminChanDir(w http.ResponseWriter, r *http.Request)    { s.chanDirectory(w, r) }

// beaconChannels: freigegebene Kanäle für das Verzeichnis des Beacons.
func (s *Server) beaconChannels() []map[string]any {
	if !s.chanFedOn() {
		return nil
	}
	var out []map[string]any
	for _, sh := range s.hub.SharedRooms() {
		out = append(out, map[string]any{"name": sh.Name, "description": sh.Description, "mode": sh.Mode, "messages": sh.Messages, "lastAt": sh.LastAt})
		if len(out) >= 100 {
			break
		}
	}
	return out
}

// PublicChanFed: was die Oberfläche über die Kanal-Föderation wissen muss.
type PublicChanFed struct {
	Domain   string `json:"domain"`
	UserSubs bool   `json:"userSubs"`
}

func (s *Server) publicChanFed() *PublicChanFed {
	if !s.chanFedOn() {
		return nil
	}
	return &PublicChanFed{Domain: s.fedDomain(), UserSubs: s.hub.chanFed.Config().UserSubs}
}

// postMirror: neue Nachricht in einem abonnierten Kanal – geht an den Ursprung. Private Befehle
// (z. B. Hilfe) bleiben hier.
func (s *Server) postMirror(w http.ResponseWriter, r *http.Request, room string, rc RemoteChan, me Identity, ip, text, gif string, replyTo, thread int64) {
	if gif != "" {
		fail(w, ErrMirrorFeature)
		return
	}
	if s.hub.Commands().IsEphemeral(text, room, me.Team) {
		s.runEphemeral(w, room, text, me, ip)
		return
	}
	if err := s.hub.checkSanction(s.posterOf(me, ip)); err != nil { // auch hier gesperrt: auch dort nicht
		fail(w, err)
		return
	}
	if !s.hub.CanWrite(room, &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}) && rc.writable() {
		fail(w, s.writeDenied(room))
		return
	}
	t, ok := cleanText(text, maxTextLen)
	if !ok {
		fail(w, ErrInvalidText)
		return
	}
	req := chanWriteReq{Text: t}
	if replyTo > 0 {
		if req.ReplyTo = s.hub.MirrorRef(room, replyTo); req.ReplyTo == 0 {
			fail(w, ErrNoReplyMsg)
			return
		}
	}
	if thread > 0 {
		if req.Thread = s.hub.MirrorRef(room, thread); req.Thread == 0 {
			fail(w, ErrNoReplyMsg)
			return
		}
	}
	msg, err := s.mirrorWrite(r.Context(), room, rc, "post", me, req)
	if err != nil {
		writeErr(w, chanStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}
