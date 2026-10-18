// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ucng/internal/fedsig"
)

// Föderation (Modus „hosted“): Direktnachrichten an name@andere-instanz.
//
//   - Jede Instanz hat einen Ed25519-Serverschlüssel. Er steht unter /.well-known/ucng.
//   - Nachrichten gehen als signierter POST an /api/fed/inbox der Ziel-Instanz. Signiert werden
//     Absender-Domain, Zeitpunkt, Methode, Ziel (Host + Pfad) und ein Hash des Inhalts.
//   - Beim ersten Kontakt wird der Schlüssel einer Instanz gemerkt (TOFU). Ändert er sich später,
//     werden ihre Nachrichten abgelehnt, bis ein Admin den alten Schlüssel vergisst.
//   - Föderiert werden nur Ende-zu-Ende-verschlüsselte Direktnachrichten (neu, bearbeitet, gelöscht).
//     Kanäle bleiben je Instanz. Antworten, Reaktionen, Bilder und Umfragen gibt es dabei nicht.
//   - Nicht zustellbare Nachrichten werden bis zu 24 Stunden lang erneut versucht.

const (
	fedPrefix      = "fed:"
	fedSkew        = 5 * time.Minute
	fedMaxBody     = 64 << 10
	fedGiveUp      = 24 * time.Hour
	fedUserTTL     = time.Hour // so lange gilt ein abgefragter Nutzerschlüssel als frisch
	fedSigContext  = "gochat-fed-v1"
	fedWellKnown   = fedsig.WellKnown
	fedInboxPath   = "/api/fed/inbox"
	fedUserPath    = "/api/fed/users/"
	fedStatePath   = "federation.json"
	fedMaxQueueLen = 5000
)

var (
	ErrFedOff       = errors.New("Föderation ist auf dieser Instanz ausgeschaltet")
	ErrFedAddress   = errors.New("Adresse bitte im Format name@instanz.example")
	ErrFedBlocked   = errors.New("Mit dieser Instanz ist kein Austausch erlaubt")
	ErrFedNoUser    = errors.New("Dort gibt es kein Konto mit diesem Namen (oder es nimmt keine Nachrichten von anderen Instanzen an)")
	ErrFedKeyChange = errors.New("Der Serverschlüssel dieser Instanz hat sich geändert – ein Admin muss ihn prüfen und den alten vergessen")
	ErrFedRemote    = errors.New("Die andere Instanz ist nicht erreichbar oder antwortet nicht wie erwartet")
	ErrFedSig       = errors.New("Signatur ungültig")
	ErrFedPrivate   = errors.New("Adressen im lokalen Netz sind für die Föderation gesperrt")
	ErrFedPort      = errors.New("Instanzen mit Port in der Adresse gibt es nur im Testbetrieb (FED_INSECURE)")
)

var fedAddrRe = regexp.MustCompile(`^@?([A-Za-z0-9][A-Za-z0-9_.-]{0,30}[A-Za-z0-9_])@([a-z0-9.-]+(?::\d{1,5})?)$`)

func isFedID(id string) bool { return strings.HasPrefix(id, fedPrefix) }

// fedHost: Domain ohne Port.
func fedHost(domain string) string {
	if i := strings.LastIndex(domain, ":"); i >= 0 {
		return domain[:i]
	}
	return domain
}

// parseFedAddr zerlegt name@domain (Domain klein geschrieben).
func parseFedAddr(a string) (name, domain string, err error) {
	a = strings.TrimSpace(a)
	if at := strings.LastIndex(a, "@"); at > 0 {
		a = a[:at] + "@" + strings.ToLower(a[at+1:])
	}
	m := fedAddrRe.FindStringSubmatch(a)
	if m == nil || strings.Contains(m[2], "..") || strings.HasPrefix(m[2], ".") || strings.HasPrefix(m[2], "-") ||
		strings.HasSuffix(fedHost(m[2]), ".") || strings.HasSuffix(fedHost(m[2]), "-") {
		return "", "", ErrFedAddress
	}
	return m[1], m[2], nil
}

// --- gespeicherter Zustand (federation.json) ---

type fedPeer struct {
	Key   string    `json:"key"` // Ed25519, Base64
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

type fedUser struct {
	PubKey  string    `json:"pubKey"`
	Fetched time.Time `json:"fetched"`
}

type fedJob struct {
	ID      string          `json:"id"`
	Domain  string          `json:"domain"`
	Body    json.RawMessage `json:"body"`
	Room    string          `json:"room,omitempty"`
	MsgID   int64           `json:"msgId,omitempty"` // lokale Nachricht (für den Zustellstatus)
	Path    string          `json:"path,omitempty"`  // Kanal-Föderation: Ziel (sonst der Eingang für Direktnachrichten)
	Chan    string          `json:"chan,omitempty"`  // Kanal-Föderation: freigegebener Kanal (für den Zustellstatus)
	Tries   int             `json:"tries"`
	Next    time.Time       `json:"next"`
	Created time.Time       `json:"created"`
}

type fedState struct {
	Seed  string             `json:"seed"`
	Peers map[string]fedPeer `json:"peers"`
	Users map[string]fedUser `json:"users"`
	Queue []fedJob           `json:"queue"`
}

// FedStore hält Schlüssel, bekannte Instanzen und die Warteschlange.
type FedStore struct {
	mu      sync.Mutex
	priv    ed25519.PrivateKey
	peers   map[string]fedPeer
	users   map[string]fedUser
	queue   []fedJob
	replay  map[string]time.Time
	changed func()
	wake    chan struct{}
}

func NewFedStore() *FedStore {
	f := &FedStore{peers: map[string]fedPeer{}, users: map[string]fedUser{}, replay: map[string]time.Time{}, wake: make(chan struct{}, 1)}
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	f.priv = ed25519.NewKeyFromSeed(seed)
	return f
}

func (f *FedStore) touch() {
	if f.changed != nil {
		f.changed()
	}
}

func (f *FedStore) state() fedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := fedState{Seed: base64.StdEncoding.EncodeToString(f.priv.Seed()), Peers: map[string]fedPeer{}, Users: map[string]fedUser{}, Queue: append([]fedJob{}, f.queue...)}
	for k, v := range f.peers {
		st.Peers[k] = v
	}
	for k, v := range f.users {
		st.Users[k] = v
	}
	return st
}

func (f *FedStore) restore(st fedState) error {
	seed, err := base64.StdEncoding.DecodeString(st.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("Serverschlüssel ungültig")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.priv = ed25519.NewKeyFromSeed(seed)
	if st.Peers != nil {
		f.peers = st.Peers
	}
	if st.Users != nil {
		f.users = st.Users
	}
	f.queue = st.Queue
	for i := range f.queue {
		if f.queue[i].ID == "" {
			f.queue[i].ID = randomHex(8)
		}
	}
	return nil
}

func (f *FedStore) PublicKey() string {
	return base64.StdEncoding.EncodeToString(f.priv.Public().(ed25519.PublicKey))
}

func (f *FedStore) Fingerprint() string {
	sum := sha256.Sum256(f.priv.Public().(ed25519.PublicKey))
	return fingerprint(sum[:])
}

func fingerprint(b []byte) string {
	h := hex.EncodeToString(b[:16])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, " ")
}

// --- Signaturen ---

func fedSignString(domain, ts, method, target string, body []byte) []byte {
	return fedsig.String(fedSigContext, domain, ts, method, target, body)
}

func (f *FedStore) sign(domain, method, target string, body []byte, now time.Time) (ts, sig string) {
	return fedsig.Sign(f.priv, fedSigContext, domain, method, target, body, now)
}

// signFor signiert mit einem anderen Kontext (z. B. für den Beacon).
func (f *FedStore) signFor(context, domain, method, target string, body []byte, now time.Time) (ts, sig string) {
	return fedsig.Sign(f.priv, context, domain, method, target, body, now)
}

func fedVerify(key, domain, ts, method, target string, body []byte, sig string) bool {
	return fedsig.Verify(key, fedSigContext, domain, ts, method, target, body, sig)
}

// seenSig: Schutz gegen Wiederholung derselben signierten Anfrage.
func (f *FedStore) seenSig(sig string, now time.Time) bool {
	// Schlüssel sind die dekodierten Bytes – andere Schreibweisen derselben Signatur zählen mit.
	if raw, err := base64.StdEncoding.DecodeString(sig); err == nil {
		sig = hex.EncodeToString(raw)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, t := range f.replay {
		if now.Sub(t) > 2*fedSkew {
			delete(f.replay, k)
		}
	}
	if _, ok := f.replay[sig]; ok {
		return true
	}
	f.replay[sig] = now
	return false
}

// --- Server: Einstellungen ---

func (s *Server) fedDomain() string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	u, err := url.Parse(s.cfg.PublicURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func (s *Server) fedEnabled() bool {
	return s.hosted() && s.fedDomain() != "" && s.hub.Hosted().Config().Fed.Enabled
}

func (s *Server) fedScheme() string {
	if s.cfg.FedInsecure {
		return "http"
	}
	return "https"
}

// fedClient: HTTP-Client für andere Instanzen. Ohne FED_INSECURE werden Ziele im lokalen Netz
// (Loopback, private Adressen) abgelehnt – sonst ließen sich über Adressen interne Dienste abtasten.
func (s *Server) fedClient() *http.Client {
	s.fedOnce.Do(func() {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		if !s.cfg.FedInsecure {
			dialer.Control = func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || inBlockedNet(ip) {
					return ErrFedPrivate
				}
				return nil
			}
		}
		tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}
		s.fedHTTP = &http.Client{Transport: tr, Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	})
	return s.fedHTTP
}

// Weitere Bereiche, die nicht ins öffentliche Internet gehören.
var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func inBlockedNet(ip net.IP) bool {
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) fedGetJSON(ctx context.Context, rawURL string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.fedClient().Do(req)
	if err != nil {
		if errors.Is(err, ErrFedPrivate) {
			return ErrFedPrivate
		}
		return ErrFedRemote
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return ErrFedNoUser
	}
	if res.StatusCode != http.StatusOK {
		return ErrFedRemote
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, fedMaxBody)).Decode(v); err != nil {
		return ErrFedRemote
	}
	return nil
}

// WellKnown beschreibt eine Instanz.
type WellKnown struct {
	Software string `json:"software"`
	Version  int    `json:"version"`
	Domain   string `json:"domain"`
	Key      string `json:"key"`
	Inbox    string `json:"inbox,omitempty"`
	Users    string `json:"users,omitempty"`
	// Föderation an? (Der Schlüssel steht auch ohne Föderation bereit – für den Beacon.)
	Federation bool `json:"federation"`
	// Kanal-Föderation an (freigegebene Kanäle unter /api/fed/channels)?
	Channels bool `json:"channels,omitempty"`
	// Fassung und Quellcode dieser Instanz (AGPL: Änderungen sind offenzulegen)
	Build  string `json:"build,omitempty"`
	Source string `json:"source,omitempty"`
}

// GET /.well-known/ucng (auch unter dem früheren Namen /.well-known/gochat)
func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	// Der Serverschlüssel steht immer bereit, sobald PUBLIC_URL gesetzt ist – der Beacon prüft
	// damit Anmeldungen. Ob die Föderation an ist, sagt "federation".
	if s.fedDomain() == "" {
		writeErr(w, http.StatusNotFound, "PUBLIC_URL ist nicht gesetzt")
		return
	}
	base := s.baseURL()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	software := fedsig.Software
	if r.URL.Path == fedsig.LegacyWellKnown { // ältere Beacons/Instanzen erwarten dort die frühere Kennung
		software = fedsig.LegacySoftware
	}
	wk := WellKnown{Build: version, Source: s.cfg.SourceURL, Software: software, Version: 1, Domain: s.fedDomain(), Key: s.hub.Fed().PublicKey(), Federation: s.fedEnabled(), Channels: s.chanFedOn()}
	if wk.Federation {
		wk.Inbox, wk.Users = base+fedInboxPath, base+fedUserPath
	}
	writeJSON(w, http.StatusOK, wk)
}

// GET /api/fed/users/{name} – öffentlicher E2E-Schlüssel eines Kontos (für andere Instanzen)
func (s *Server) handleFedUser(w http.ResponseWriter, r *http.Request) {
	if !s.fedEnabled() {
		writeErr(w, http.StatusNotFound, ErrFedOff.Error())
		return
	}
	if !s.fedRL.Allow("u:" + s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anfragen")
		return
	}
	_, display, pub, noFed, ok := s.users.MemberByName(r.PathValue("name"))
	if !ok || noFed || pub == "" {
		writeErr(w, http.StatusNotFound, ErrFedNoUser.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": display, "pubKey": pub})
}

// peerKey liefert den (gemerkten oder neu abgefragten) Serverschlüssel einer Instanz.
func (s *Server) peerKey(ctx context.Context, domain string, refresh bool) (string, error) {
	f := s.hub.Fed()
	f.mu.Lock()
	p, ok := f.peers[domain]
	f.mu.Unlock()
	if ok && !refresh {
		return p.Key, nil
	}
	var wk WellKnown
	err := s.fedGetJSON(ctx, s.fedScheme()+"://"+domain+fedWellKnown, &wk)
	if err != nil { // Instanzen vor der Umbenennung (GoChat)
		if err2 := s.fedGetJSON(ctx, s.fedScheme()+"://"+domain+fedsig.LegacyWellKnown, &wk); err2 == nil {
			err = nil
		}
	}
	if err != nil {
		if errors.Is(err, ErrFedNoUser) {
			return "", ErrFedRemote
		}
		return "", err
	}
	if !fedsig.IsInstance(wk.Software) || !strings.EqualFold(wk.Domain, domain) || !(wk.Federation || wk.Channels) {
		return "", ErrFedRemote
	}
	if k, err := base64.StdEncoding.DecodeString(wk.Key); err != nil || len(k) != ed25519.PublicKeySize {
		return "", ErrFedRemote
	}
	if ok && p.Key != wk.Key {
		log.Printf("Föderation: Serverschlüssel von %s hat sich geändert – Nachrichten werden abgelehnt, bis ein Admin den alten Schlüssel vergisst", domain)
		return "", ErrFedKeyChange
	}
	if !ok {
		now := time.Now().UTC()
		f.mu.Lock()
		f.peers[domain] = fedPeer{Key: wk.Key, First: now, Last: now}
		f.mu.Unlock()
		f.touch()
	}
	return wk.Key, nil
}

func (s *Server) fedCheckDomain(domain string) error {
	if !s.fedEnabled() {
		return ErrFedOff
	}
	if strings.Contains(domain, ":") && !s.cfg.FedInsecure {
		return ErrFedPort
	}
	if !s.hub.Hosted().Config().Fed.Allowed(domain) {
		return ErrFedBlocked
	}
	return nil
}

// fedLookupKey liefert den öffentlichen E2E-Schlüssel eines Kontos auf einer anderen Instanz.
func (s *Server) fedLookupKey(ctx context.Context, addr string) (string, error) {
	name, domain, err := parseFedAddr(addr)
	if err != nil {
		return "", err
	}
	if err := s.fedCheckDomain(domain); err != nil {
		return "", err
	}
	key := strings.ToLower(name) + "@" + domain
	f := s.hub.Fed()
	f.mu.Lock()
	u, ok := f.users[key]
	f.mu.Unlock()
	if ok && time.Since(u.Fetched) < fedUserTTL {
		return u.PubKey, nil
	}
	if _, err := s.peerKey(ctx, domain, false); err != nil {
		return "", err
	}
	var out struct {
		Name   string `json:"name"`
		PubKey string `json:"pubKey"`
	}
	if err := s.fedGetJSON(ctx, s.fedScheme()+"://"+domain+fedUserPath+url.PathEscape(name), &out); err != nil {
		if ok { // Instanz gerade nicht erreichbar: zuletzt bekannten Schlüssel verwenden
			return u.PubKey, nil
		}
		return "", err
	}
	if b, err := base64.StdEncoding.DecodeString(out.PubKey); err != nil || len(b) != 65 || b[0] != 4 {
		return "", ErrFedRemote
	}
	f.mu.Lock()
	f.users[key] = fedUser{PubKey: out.PubKey, Fetched: time.Now().UTC()}
	f.mu.Unlock()
	f.touch()
	return out.PubKey, nil
}

// --- Eingang ---

type fedPayload struct {
	Type    string    `json:"type"` // message, edit, delete
	From    string    `json:"from"` // Name auf der Absender-Instanz
	FromKey string    `json:"fromKey,omitempty"`
	To      string    `json:"to"`
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	E2E     string    `json:"e2e,omitempty"`
}

// POST /api/fed/inbox
func (s *Server) handleFedInbox(w http.ResponseWriter, r *http.Request) {
	if !s.fedEnabled() {
		writeErr(w, http.StatusNotFound, ErrFedOff.Error())
		return
	}
	domain, ts, sig := fedsig.FromRequest(r)
	domain = strings.ToLower(domain)
	if domain == "" || ts == "" || sig == "" {
		writeErr(w, http.StatusUnauthorized, ErrFedSig.Error())
		return
	}
	if _, d, err := parseFedAddr("xx@" + domain); err != nil || d != domain {
		writeErr(w, http.StatusBadRequest, "Ungültige Domain")
		return
	}
	if err := s.fedCheckDomain(domain); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if !s.fedRL.Allow("d:" + domain) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Nachrichten")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, fedMaxBody+1))
	if err != nil || len(body) > fedMaxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "Zu groß")
		return
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	now := time.Now()
	if err != nil || now.Sub(time.Unix(sec, 0)).Abs() > fedSkew {
		writeErr(w, http.StatusUnauthorized, "Zeitstempel ungültig (Uhrzeit prüfen)")
		return
	}
	target := s.fedDomain() + fedInboxPath
	key, err := s.peerKey(r.Context(), domain, false)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if !fedVerify(key, domain, ts, "POST", target, body, sig) {
		// Vielleicht hat die Instanz ihren Schlüssel getauscht – dann bleibt es abgelehnt (TOFU).
		if _, err := s.peerKey(r.Context(), domain, true); err != nil {
			writeErr(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeErr(w, http.StatusUnauthorized, ErrFedSig.Error())
		return
	}
	if s.hub.Fed().seenSig(sig, now) {
		w.WriteHeader(http.StatusAccepted) // schon verarbeitet
		return
	}
	var p fedPayload
	if err := json.Unmarshal(body, &p); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	if !handleRe.MatchString(p.From) || p.ID <= 0 {
		writeErr(w, http.StatusBadRequest, "Ungültiger Absender")
		return
	}
	localID, _, _, noFed, ok := s.users.MemberByName(p.To)
	if !ok || noFed {
		writeErr(w, http.StatusNotFound, ErrFedNoUser.Error())
		return
	}
	addr := strings.ToLower(p.From) + "@" + domain
	// Blockiert: annehmen und verwerfen (die andere Seite erfährt nichts davon).
	if p.Type != "delete" && s.hub.Blocks().Has(localID, fedPrefix+addr) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	f := s.hub.Fed()
	switch p.Type {
	case "message", "edit":
		if err := validE2E(p.E2E); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if p.FromKey == "" || e2eSender(p.E2E) != p.FromKey {
			writeErr(w, http.StatusBadRequest, "Absenderschlüssel passt nicht")
			return
		}
		f.mu.Lock()
		f.users[addr] = fedUser{PubKey: p.FromKey, Fetched: now.UTC()}
		if pe, ok := f.peers[domain]; ok {
			pe.Last = now.UTC()
			f.peers[domain] = pe
		}
		f.mu.Unlock()
		f.touch()
	case "delete":
	default:
		writeErr(w, http.StatusBadRequest, "Unbekannter Typ")
		return
	}
	switch p.Type {
	case "message":
		if s.hub.Settings().NoDMs {
			writeErr(w, http.StatusForbidden, ErrDMsOff.Error())
			return
		}
		if _, err := s.hub.ReceiveFed(localID, addr, p.ID, p.E2E); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.hub.NotifyUser("dm", localID)
	case "edit":
		s.hub.FedChange(localID, addr, p.ID, p.E2E, false)
	case "delete":
		s.hub.FedChange(localID, addr, p.ID, "", true)
	}
	w.WriteHeader(http.StatusAccepted)
}

// ReceiveFed legt eine eingehende Nachricht in der Unterhaltung mit addr ab (doppelte werden erkannt).
func (h *Hub) ReceiveFed(localID, addr string, ref int64, e2e string) (Message, error) {
	room := h.OpenDM(localID, fedPrefix+addr)
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[room]
	if !ok {
		return Message{}, ErrNoRoom
	}
	from := fedPrefix + addr
	for i := len(r.history) - 1; i >= 0 && i >= len(r.history)-200; i-- {
		if m := r.history[i]; m.UserID == from && m.Fed != nil && m.Fed.Ref == ref {
			return h.out(r, m, false), nil
		}
	}
	msg := Message{ID: h.nextID.Add(1), Room: room, UserID: from, Name: addr, Text: e2ePlaceholder, Time: time.Now().UTC(),
		E2E: e2e, Fed: &FedMeta{Ref: ref}}
	r.history = append(r.history, msg)
	r.last = msg.Time
	h.record(storeOp{kind: opMessage, room: room, msg: msg})
	pub := h.out(r, msg, false)
	h.broadcast(r, Event{Type: "message", Message: &pub, Online: len(r.clients)})
	return pub, nil
}

// FedChange bearbeitet oder löscht eine eingegangene Nachricht.
func (h *Hub) FedChange(localID, addr string, ref int64, e2e string, del bool) {
	room := dmRoomName(localID, fedPrefix+addr)
	from := fedPrefix + addr
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[room]
	if !ok {
		return
	}
	for i := len(r.history) - 1; i >= 0; i-- {
		m := &r.history[i]
		if m.UserID != from || m.Fed == nil || m.Fed.Ref != ref {
			continue
		}
		now := time.Now().UTC()
		if del {
			m.Text, m.E2E, m.Deleted, m.DeletedBy, m.Reactions = "", "", true, "self", nil
		} else if !m.Deleted {
			m.E2E, m.EditedAt = e2e, &now
		}
		h.afterChangeLocked(r, m)
		return
	}
}

// SetFedState setzt den Zustellstatus einer ausgehenden Nachricht.
func (h *Hub) SetFedState(room string, id int64, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(room, id)
	if err != nil || m.Fed == nil || m.Fed.State == state {
		return
	}
	m.Fed = &FedMeta{State: state}
	h.afterChangeLocked(r, m)
}

// --- Ausgang ---

// fedSend stellt eine Nachricht an die andere Instanz in die Warteschlange.
func (s *Server) fedSend(peer string, p fedPayload, room string, msgID int64) {
	addr := strings.TrimPrefix(peer, fedPrefix)
	name, domain, err := parseFedAddr(addr)
	if err != nil {
		return
	}
	p.To = name
	body, _ := json.Marshal(p)
	f := s.hub.Fed()
	now := time.Now().UTC()
	f.mu.Lock()
	if len(f.queue) >= fedMaxQueueLen {
		f.mu.Unlock()
		if msgID > 0 {
			s.hub.SetFedState(room, msgID, "failed")
		}
		return
	}
	f.queue = append(f.queue, fedJob{ID: randomHex(8), Domain: domain, Body: body, Room: room, MsgID: msgID, Next: now, Created: now})
	f.mu.Unlock()
	f.touch()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// deliver schickt einen Auftrag an die andere Instanz.
func (s *Server) deliver(ctx context.Context, j fedJob) (retry bool, err error) {
	check := s.fedCheckDomain
	if j.Path != "" {
		check = s.chanCheckDomain
	}
	if err := check(j.Domain); err != nil {
		return false, err
	}
	if _, err := s.peerKey(ctx, j.Domain, false); err != nil {
		return !errors.Is(err, ErrFedKeyChange) && !errors.Is(err, ErrFedPrivate), err
	}
	target := j.Domain + fedInboxPath
	if j.Path != "" {
		target = j.Domain + j.Path
	}
	ts, sig := s.hub.Fed().sign(s.fedDomain(), "POST", target, j.Body, time.Now())
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", s.fedScheme()+"://"+target, bytes.NewReader(j.Body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	fedsig.SetHeaders(req.Header, s.fedDomain(), ts, sig)
	res, err := s.fedClient().Do(req)
	if err != nil {
		return !errors.Is(err, ErrFedPrivate), err
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	switch {
	case res.StatusCode < 300:
		return false, nil
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 || res.StatusCode == http.StatusUnauthorized:
		return true, &chanRemoteErr{Status: res.StatusCode, Msg: fmt.Sprintf("%s: %d %s", j.Domain, res.StatusCode, bytes.TrimSpace(msg))}
	default:
		return false, &chanRemoteErr{Status: res.StatusCode, Msg: fmt.Sprintf("%s: %d %s", j.Domain, res.StatusCode, bytes.TrimSpace(msg))}
	}
}

var fedBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour}

// runFed arbeitet die Warteschlange ab.
func (s *Server) runFed(ctx context.Context) {
	f := s.hub.Fed()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.fedWork(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-f.wake:
		}
	}
}

// fedWork stellt fällige Aufträge zu. Je Instanz in der Reihenfolge, in der sie entstanden sind:
// Wartet ein früherer Auftrag (z. B. die Nachricht selbst), warten die späteren (Bearbeiten,
// Löschen) mit. Aufträge bleiben bis zum Abschluss in der Warteschlange – auch beim Speichern
// mitten in einer Zustellung oder beim Herunterfahren geht nichts verloren.
func (s *Server) fedWork(ctx context.Context) {
	f := s.hub.Fed()
	now := time.Now()
	f.mu.Lock()
	blocked := map[string]bool{}
	var due []fedJob
	for _, j := range f.queue {
		if blocked[j.Domain] {
			continue
		}
		if j.Next.After(now) {
			blocked[j.Domain] = true // früherer Auftrag wartet noch
			continue
		}
		due = append(due, j)
	}
	f.mu.Unlock()
	if len(due) == 0 {
		return
	}
	type result struct {
		done  bool // erledigt oder aufgegeben
		retry time.Time
		tries int
	}
	results := map[string]result{}
	failed := map[string]time.Time{} // Instanz → nächster Versuch (spätere Aufträge warten)
	for _, j := range due {
		if ctx.Err() != nil {
			break // beim Herunterfahren: bleibt in der Warteschlange
		}
		if next, ok := failed[j.Domain]; ok {
			results[j.ID] = result{retry: next, tries: j.Tries}
			continue
		}
		retry, err := s.deliver(ctx, j)
		if j.Chan != "" {
			s.hub.chanFed.note(j.Chan, j.Domain, err)
		}
		if err == nil {
			results[j.ID] = result{done: true}
			if j.MsgID > 0 {
				s.hub.SetFedState(j.Room, j.MsgID, "sent")
			}
			continue
		}
		if ctx.Err() != nil {
			break
		}
		tries := j.Tries + 1
		if retry && now.Sub(j.Created) < fedGiveUp {
			next := now.Add(fedBackoff[min(tries-1, len(fedBackoff)-1)])
			results[j.ID] = result{retry: next, tries: tries}
			failed[j.Domain] = next
			continue
		}
		log.Printf("Föderation: Zustellung an %s aufgegeben: %v", j.Domain, err)
		results[j.ID] = result{done: true}
		var re *chanRemoteErr
		if j.Chan != "" && errors.As(err, &re) && re.Status == http.StatusGone { // dort nicht (mehr) abonniert
			s.hub.chanFed.removeSub(j.Chan, j.Domain)
		}
		if j.MsgID > 0 {
			s.hub.SetFedState(j.Room, j.MsgID, "failed")
		}
	}
	f.mu.Lock()
	var keep []fedJob
	for _, j := range f.queue {
		r, ok := results[j.ID]
		switch {
		case !ok:
			keep = append(keep, j)
		case r.done:
		default:
			j.Next, j.Tries = r.retry, r.tries
			keep = append(keep, j)
		}
	}
	f.queue = keep
	f.mu.Unlock()
	f.touch()
}

// --- Client-API ---

// GET /api/fed/lookup?addr=name@domain – Schlüssel eines Kontos auf einer anderen Instanz
func (s *Server) handleFedLookup(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	if !me.Member {
		fail(w, ErrNoMember)
		return
	}
	addr := r.URL.Query().Get("addr")
	name, domain, err := parseFedAddr(addr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if domain == s.fedDomain() {
		id, display, pub, _, ok := s.users.MemberByName(name)
		if !ok {
			writeErr(w, http.StatusNotFound, ErrFedNoUser.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"local": true, "userId": id, "name": display, "pubKey": pub})
		return
	}
	if !s.fedRL.Allow("l:" + me.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Abfragen – bitte kurz warten.")
		return
	}
	pk, err := s.fedLookupKey(r.Context(), addr)
	if err != nil {
		writeErr(w, fedStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"address": strings.ToLower(name) + "@" + domain, "pubKey": pk})
}

func fedStatus(err error) int {
	switch {
	case errors.Is(err, ErrFedNoUser):
		return http.StatusNotFound
	case errors.Is(err, ErrFedOff), errors.Is(err, ErrFedBlocked), errors.Is(err, ErrFedPrivate):
		return http.StatusForbidden
	case errors.Is(err, ErrFedAddress):
		return http.StatusBadRequest
	case errors.Is(err, ErrFedKeyChange):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

// openFedDM öffnet eine Unterhaltung mit name@domain (POST /api/dms {"address": …}).
func (s *Server) openFedDM(w http.ResponseWriter, r *http.Request, me Identity, address string) {
	if !me.Member {
		fail(w, ErrNoMember)
		return
	}
	address = strings.TrimSpace(address)
	if !strings.Contains(strings.TrimPrefix(address, "@"), "@") { // nur ein Name: Konto auf dieser Instanz
		address = strings.TrimPrefix(address, "@") + "@" + s.fedDomain()
		if s.fedDomain() == "" {
			address += "local"
		}
	}
	name, domain, err := parseFedAddr(address)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if domain == s.fedDomain() || (s.fedDomain() == "" && domain == "local") { // eigene Instanz
		id, display, _, _, ok := s.users.MemberByName(name)
		if !ok {
			writeErr(w, http.StatusNotFound, ErrFedNoUser.Error())
			return
		}
		if id == me.ID {
			fail(w, ErrDMSelf)
			return
		}
		if s.hub.Blocks().Either(me.ID, id) {
			writeErr(w, http.StatusForbidden, ErrDMBlocked.Error())
			return
		}
		room := s.hub.OpenDM(me.ID, id)
		s.hub.NotifyUser("dm", id)
		enc, _, _ := s.e2eMode(room, me)
		writeJSON(w, http.StatusOK, DMInfo{Room: room, With: id, Name: display, E2E: enc})
		return
	}
	if _, ok := s.users.PubKeyOf(me.ID); !ok {
		writeErr(w, http.StatusConflict, "Dein Konto hat noch keinen Schlüssel – bitte einmal ab- und wieder anmelden.")
		return
	}
	if _, err := s.fedLookupKey(r.Context(), address); err != nil {
		writeErr(w, fedStatus(err), err.Error())
		return
	}
	addr := strings.ToLower(name) + "@" + domain
	peer := fedPrefix + addr
	if s.hub.Blocks().Has(me.ID, peer) {
		writeErr(w, http.StatusForbidden, ErrDMBlocked.Error())
		return
	}
	if !s.hub.HasRoom(dmRoomName(me.ID, peer)) && !s.dmRL.Allow(me.ID) {
		fail(w, ErrDMLimit)
		return
	}
	room := s.hub.OpenDM(me.ID, peer)
	writeJSON(w, http.StatusOK, DMInfo{Room: room, With: peer, Name: addr, Fed: true, E2E: true})
}

// --- Admin ---

type FedPeerInfo struct {
	Domain      string    `json:"domain"`
	Fingerprint string    `json:"fingerprint"`
	First       time.Time `json:"first"`
	Last        time.Time `json:"last"`
}

// GET /api/admin/federation
func (s *Server) handleAdminFed(w http.ResponseWriter, r *http.Request) {
	f := s.hub.Fed()
	f.mu.Lock()
	peers := []FedPeerInfo{}
	for d, p := range f.peers {
		k, _ := base64.StdEncoding.DecodeString(p.Key)
		sum := sha256.Sum256(k)
		peers = append(peers, FedPeerInfo{Domain: d, Fingerprint: fingerprint(sum[:]), First: p.First, Last: p.Last})
	}
	queued := len(f.queue)
	f.mu.Unlock()
	sort.Slice(peers, func(i, j int) bool { return peers[i].Last.After(peers[j].Last) })
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.fedEnabled(), "domain": s.fedDomain(), "fingerprint": f.Fingerprint(),
		"peers": peers, "queued": queued, "publicUrl": s.cfg.PublicURL, "hosted": s.hosted()})
}

// DELETE /api/admin/federation/peers/{domain} – gemerkten Serverschlüssel vergessen
func (s *Server) handleAdminFedForget(w http.ResponseWriter, r *http.Request) {
	d := strings.ToLower(r.PathValue("domain"))
	f := s.hub.Fed()
	f.mu.Lock()
	_, ok := f.peers[d]
	delete(f.peers, d)
	for k := range f.users {
		if strings.HasSuffix(k, "@"+d) {
			delete(f.users, k)
		}
	}
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "Diese Instanz ist nicht bekannt")
		return
	}
	f.touch()
	s.log(r, AuditEntry{Action: "federation.forget", Detail: d})
	w.WriteHeader(http.StatusNoContent)
}
