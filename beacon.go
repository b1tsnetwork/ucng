// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"ucng/internal/fedsig"
)

// Beacon (siehe cmd/ucng-beacon): Die Instanz meldet sich im Verzeichnis eines Beacons an
// und aktualisiert ihre Angaben alle 6 Stunden. Außerdem kann sie GIF-Sammlungen von Beacons
// abonnieren – die GIFs erscheinen im Chat im GIF-Fenster unter „Sammlungen“.
//
// Die Anmeldung ist mit dem Serverschlüssel der Instanz signiert (wie die Föderation); der
// Beacon prüft sie über /.well-known/ucng. Dafür muss PUBLIC_URL gesetzt sein.

const (
	beaconContext  = "gochat-beacon-v1"
	beaconInterval = 6 * time.Hour
	maxBeaconSubs  = 10
)

var (
	ErrBeaconURL    = errors.New("Bitte die Adresse des Beacons angeben, z. B. https://beacon.example.org")
	ErrBeaconLang   = errors.New("Sprache als Kürzel, z. B. de oder en")
	ErrBeaconLand   = errors.New("Land als Kürzel, z. B. DE oder AT")
	ErrBeaconNoURL  = errors.New("Für das Verzeichnis braucht die Instanz PUBLIC_URL (öffentliche Adresse)")
	ErrBeaconSubs   = errors.New("Höchstens 10 abonnierte Beacons")
	ErrBeaconRemote = errors.New("Der Beacon ist nicht erreichbar oder antwortet nicht wie erwartet")

	beaconLangRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)
	beaconCCRe   = regexp.MustCompile(`^[A-Z]{2}$`)
	beaconCollRe = regexp.MustCompile(`^c_[0-9a-f]{8}$`)
)

// BeaconSub: abonnierte GIF-Sammlungen eines Beacons (leere Liste = alle).
type BeaconSub struct {
	URL         string   `json:"url"`
	Name        string   `json:"name"`
	Collections []string `json:"collections"`
}

// Key: kurze, stabile Kennung des Beacons (Teil der GIF-IDs "beacon:<key>:<id>").
func (b BeaconSub) Key() string {
	sum := sha256.Sum256([]byte(b.URL))
	return hex.EncodeToString(sum[:4])
}

type BeaconSettings struct {
	URL         string      `json:"url"`    // Beacon für das Verzeichnis
	Listed      bool        `json:"listed"` // im Verzeichnis anzeigen
	Language    string      `json:"language"`
	Country     string      `json:"country"`
	Description string      `json:"description"`
	Subs        []BeaconSub `json:"subscriptions"`
	// Status der letzten Meldung (nur Anzeige)
	LastOK    time.Time `json:"lastOk,omitempty"`
	LastErr   string    `json:"lastError,omitempty"`
	WasListed string    `json:"wasListed,omitempty"` // Beacon, bei dem die Instanz zuletzt gelistet war
	Trust     string    `json:"trust,omitempty"`     // Einstufung durch den Beacon: official, recommended, standard, leer = Community
}

type BeaconStore struct {
	mu      sync.Mutex
	cfg     BeaconSettings
	changed func()
	wake    chan struct{}
}

func NewBeaconStore() *BeaconStore { return &BeaconStore{wake: make(chan struct{}, 1)} }

func (b *BeaconStore) Get() BeaconSettings {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.cfg
	c.Subs = append([]BeaconSub{}, b.cfg.Subs...)
	return c
}

func (b *BeaconStore) Subs() []BeaconSub { return b.Get().Subs }

// Poke meldet die Instanz bald neu (z. B. weil sich freigegebene Kanäle geändert haben).
func (b *BeaconStore) Poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *BeaconStore) touch() {
	if b.changed != nil {
		b.changed()
	}
}

func (b *BeaconStore) restore(c BeaconSettings) {
	b.mu.Lock()
	b.cfg = c
	b.mu.Unlock()
}

func normBeaconURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", ErrBeaconURL
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// Set übernimmt die Einstellungen für das Verzeichnis (die Abos bleiben).
func (b *BeaconStore) Set(in BeaconSettings) (BeaconSettings, error) {
	u, err := normBeaconURL(in.URL)
	if err != nil {
		return BeaconSettings{}, err
	}
	if in.Language != "" && !beaconLangRe.MatchString(in.Language) {
		return BeaconSettings{}, ErrBeaconLang
	}
	in.Country = strings.ToUpper(in.Country)
	if in.Country != "" && !beaconCCRe.MatchString(in.Country) {
		return BeaconSettings{}, ErrBeaconLand
	}
	b.mu.Lock()
	b.cfg.URL, b.cfg.Listed, b.cfg.Language, b.cfg.Country = u, in.Listed && u != "", in.Language, in.Country
	b.cfg.Description = strings.Join(strings.Fields(in.Description), " ")
	if r := []rune(b.cfg.Description); len(r) > 280 {
		b.cfg.Description = string(r[:280])
	}
	b.mu.Unlock()
	b.touch()
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return b.Get(), nil
}

func (b *BeaconStore) setTrust(t string) {
	b.mu.Lock()
	changed := b.cfg.Trust != t
	b.cfg.Trust = t
	b.mu.Unlock()
	if changed {
		b.touch()
	}
}

func (b *BeaconStore) setStatus(ok bool, msg, listedAt string) {
	b.mu.Lock()
	if ok {
		b.cfg.LastOK, b.cfg.LastErr = time.Now().UTC(), ""
		b.cfg.WasListed = listedAt
	} else {
		b.cfg.LastErr = msg
	}
	b.mu.Unlock()
	b.touch()
}

// beaconClient folgt keinen Weiterleitungen (die Signatur gilt nur für das eine Ziel).
var beaconClient = &http.Client{Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func beaconNetErr(err error) string {
	m := err.Error()
	switch {
	case strings.Contains(m, "Client.Timeout"), strings.Contains(m, "deadline exceeded"):
		return "Zeitüberschreitung"
	case strings.Contains(m, "refused"):
		return "Verbindung abgelehnt – läuft der Beacon auf diesem Port?"
	case strings.Contains(m, "no such host"):
		return "Name nicht auflösbar"
	case strings.Contains(m, "x509"), strings.Contains(m, "certificate"):
		return "TLS-Zertifikat ungültig"
	case strings.Contains(m, "HTTP response to HTTPS client"):
		return "der Beacon spricht nur http://"
	}
	return "Verbindungsfehler"
}

// --- Anmeldung ---

func (s *Server) beaconPayload(listed bool) map[string]any {
	st := s.hub.Settings()
	cfg := s.hub.Beacon().Get()
	mode, guests, reg := s.cfg.Mode, "", st.AllowUserRooms
	if s.hosted() {
		h := s.hub.Hosted().Config()
		guests, reg = h.Guests, h.Registration
	} else {
		reg = true // Namen sind frei wählbar
	}
	return map[string]any{"url": s.baseURL(), "name": st.Title, "description": cfg.Description, "users": s.users.Count(),
		"language": cfg.Language, "country": cfg.Country, "mode": mode, "federation": s.fedEnabled(), "registration": reg,
		"guests": guests, "version": version, "listed": listed, "source": s.cfg.SourceURL, "channels": s.beaconChannels()}
}

// beaconRegister meldet die Instanz (listed=false: aus dem Verzeichnis entfernen).
func (s *Server) beaconRegister(ctx context.Context, beaconURL string, listed bool) error {
	_, err := s.beaconRegisterTrust(ctx, beaconURL, listed)
	return err
}

// beaconRegisterTrust meldet und liefert die Einstufung, die der Beacon zurückgibt.
func (s *Server) beaconRegisterTrust(ctx context.Context, beaconURL string, listed bool) (string, error) {
	if s.fedDomain() == "" {
		return "", ErrBeaconNoURL
	}
	u, err := url.Parse(beaconURL)
	if err != nil || u.Host == "" {
		return "", ErrBeaconURL
	}
	body, _ := json.Marshal(s.beaconPayload(listed))
	target := u.Host + "/api/v1/register"
	ts, sig := s.hub.Fed().signFor(beaconContext, s.fedDomain(), "POST", target, body, time.Now())
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", beaconURL+"/api/v1/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	fedsig.SetHeaders(req.Header, s.fedDomain(), ts, sig)
	def, _ := s.hub.I18n().Settings()
	req.Header.Set("X-Lang", def) // Meldungen des Beacons in der Standardsprache der Instanz
	// Die Adresse des Beacons trägt der Admin selbst ein – daher auch http:// und lokales Netz.
	res, err := beaconClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Beacon unter %s nicht erreichbar (%s)", beaconURL, beaconNetErr(err))
	}
	defer res.Body.Close()
	var out struct {
		Error string `json:"error"`
		Trust string `json:"trust"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 8<<10)).Decode(&out)
	if res.StatusCode != 200 {
		if out.Error != "" {
			return "", errors.New("Beacon: " + out.Error)
		}
		return "", fmt.Errorf("Beacon: Status %d", res.StatusCode)
	}
	switch out.Trust {
	case "official", "recommended", "standard":
	default:
		out.Trust = ""
	}
	return out.Trust, nil
}

// beaconSync meldet die Instanz an bzw. ab, je nach Einstellung.
func (s *Server) beaconSync(ctx context.Context) {
	cfg := s.hub.Beacon().Get()
	if cfg.WasListed != "" && (!cfg.Listed || cfg.WasListed != cfg.URL) { // abmelden (aus oder anderer Beacon)
		if err := s.beaconRegister(ctx, cfg.WasListed, false); err == nil || !cfg.Listed {
			s.hub.Beacon().setStatus(true, "", "")
			s.hub.Beacon().setTrust("")
		}
	}
	if !cfg.Listed || cfg.URL == "" {
		return
	}
	trust, err := s.beaconRegisterTrust(ctx, cfg.URL, true)
	if err != nil {
		log.Printf("Beacon %s: %v", cfg.URL, err)
		s.hub.Beacon().setStatus(false, err.Error(), cfg.WasListed)
		return
	}
	s.hub.Beacon().setStatus(true, "", cfg.URL)
	s.hub.Beacon().setTrust(trust)
}

// runBeacon meldet regelmäßig (und sofort nach Änderungen).
func (s *Server) runBeacon(ctx context.Context) {
	t := time.NewTicker(beaconInterval)
	defer t.Stop()
	b := s.hub.Beacon()
	s.beaconSync(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.wake:
		}
		s.beaconSync(ctx)
	}
}

// --- GIF-Sammlungen abonnieren ---

type beaconInfo struct {
	Software string `json:"software"`
	Name     string `json:"name"`
	URL      string `json:"url"`
}

type beaconCollection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Count       int    `json:"count"`
	Preview     string `json:"preview"`
}

type beaconGIF struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	URL     string `json:"url"`
	Preview string `json:"preview"` // kleine Fassung (neuere Beacons); sonst das GIF selbst
	Credit  string `json:"credit"`  // Quellenangabe, die der Beacon festlegt
}

// beaconCredit: Quellenangabe für ein GIF (vom Beacon festgelegt, sonst Name des Abos).
func beaconCredit(g beaconGIF, sub BeaconSub) string {
	c := strings.Join(strings.Fields(g.Credit), " ")
	if c == "" && sub.Name != "" {
		c = "via " + strings.Join(strings.Fields(sub.Name), " ")
	}
	if r := []rune(c); len(r) > 40 {
		c = string(r[:40])
	}
	return c
}

var beaconGIFIDRe = regexp.MustCompile(`^b_[0-9a-f]{10}$`)

func (ml *MediaLib) beaconGet(base, path string, v any) error {
	data, err := ml.fetch(base+path, 4<<20)
	if err != nil {
		return ErrBeaconRemote
	}
	if err := json.Unmarshal(data, v); err != nil {
		return ErrBeaconRemote
	}
	return nil
}

// BeaconCollections liefert Name und Sammlungen eines Beacons.
func (ml *MediaLib) BeaconCollections(base string) (string, []beaconCollection, error) {
	var info beaconInfo
	if err := ml.beaconGet(base, "/api/v1/info", &info); err != nil || !fedsig.IsBeacon(info.Software) {
		return "", nil, ErrBeaconRemote
	}
	var list []beaconCollection
	if err := ml.beaconGet(base, "/api/v1/collections", &list); err != nil {
		return "", nil, err
	}
	if list == nil {
		list = []beaconCollection{}
	}
	return info.Name, list, nil
}

// SearchBeacons durchsucht alle abonnierten Sammlungen.
func (ml *MediaLib) SearchBeacons(q string) ([]WebGIF, error) {
	if !ml.Config().GifsEnabled {
		return nil, ErrGifsOff
	}
	subs := ml.beaconSubs()
	if len(subs) == 0 {
		return nil, errors.New("Keine GIF-Sammlungen abonniert")
	}
	q = strings.TrimSpace(q)
	if len([]rune(q)) > 60 {
		q = string([]rune(q)[:60])
	}
	type result struct {
		sub  BeaconSub
		gifs []beaconGIF
	}
	results := make([]result, len(subs))
	var wg sync.WaitGroup
	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub BeaconSub) {
			defer wg.Done()
			p := url.Values{"q": {q}, "limit": {"30"}, "collection": {strings.Join(sub.Collections, ",")}}
			var res struct {
				GIFs []beaconGIF `json:"gifs"`
			}
			if ml.beaconGet(sub.URL, "/api/v1/gifs?"+p.Encode(), &res) == nil {
				results[i] = result{sub, res.GIFs}
			}
		}(i, sub)
	}
	wg.Wait()
	out := []WebGIF{}
	ml.webMu.Lock()
	defer ml.webMu.Unlock()
	for k, e := range ml.web {
		if time.Since(e.at) > webCacheTTL {
			delete(ml.web, k)
		}
	}
	// abwechselnd aus allen Beacons
	for i := 0; i < 30; i++ {
		for _, r := range results {
			if i >= len(r.gifs) {
				continue
			}
			g := r.gifs[i]
			if !beaconGIFIDRe.MatchString(g.ID) || !sameOrigin(g.URL, r.sub.URL) {
				continue
			}
			id := "beacon:" + r.sub.Key() + ":" + g.ID
			preview := g.URL
			if g.Preview != "" && sameOrigin(g.Preview, r.sub.URL) {
				preview = g.Preview
			}
			ml.web[id] = webEntry{id: id, title: g.Title, full: g.URL, preview: preview, w: g.W, h: g.H, pw: g.W, ph: g.H, at: time.Now(), credit: beaconCredit(g, r.sub)}
			out = append(out, WebGIF{ID: id, Title: g.Title, W: g.W, H: g.H, Preview: "/api/gifs/web/preview/" + id})
		}
	}
	return out, nil
}

// sameOrigin: GIF-Dateien nur vom Beacon selbst laden.
func sameOrigin(raw, base string) bool {
	u, err := url.Parse(raw)
	b, err2 := url.Parse(base)
	return err == nil && err2 == nil && u.Scheme == b.Scheme && strings.EqualFold(u.Host, b.Host)
}

// beaconEntry lädt einen Treffer neu beim Beacon (wenn er nicht mehr im Zwischenspeicher ist).
func (ml *MediaLib) beaconEntry(id string) (webEntry, error) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || !beaconGIFIDRe.MatchString(parts[2]) {
		return webEntry{}, ErrWebGIF
	}
	for _, sub := range ml.beaconSubs() {
		if sub.Key() != parts[1] {
			continue
		}
		var g beaconGIF
		if err := ml.beaconGet(sub.URL, "/api/v1/gifs/"+parts[2], &g); err != nil || g.ID != parts[2] || !sameOrigin(g.URL, sub.URL) {
			return webEntry{}, ErrWebGIF
		}
		e := webEntry{id: id, title: g.Title, full: g.URL, preview: g.URL, w: g.W, h: g.H, pw: g.W, ph: g.H, at: time.Now(), credit: beaconCredit(g, sub)}
		ml.webMu.Lock()
		ml.web[id] = e
		ml.webMu.Unlock()
		return e, nil
	}
	return webEntry{}, ErrWebGIF
}

// --- HTTP ---

// GET /api/gifs/beacon?q=
func (s *Server) handleGIFBeacon(w http.ResponseWriter, r *http.Request) {
	if !s.gifRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Suchen – bitte kurz warten.")
		return
	}
	list, err := s.hub.Media().SearchBeacons(r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GET/PUT /api/admin/beacon
func (s *Server) handleBeaconGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": beaconView(w, s.hub.Beacon().Get()), "publicUrl": s.cfg.PublicURL,
		"domain": s.fedDomain(), "preview": s.beaconPayload(true)})
}

func (s *Server) handleBeaconPut(w http.ResponseWriter, r *http.Request) {
	var in BeaconSettings
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Listed && s.fedDomain() == "" {
		writeErr(w, http.StatusConflict, ErrBeaconNoURL.Error())
		return
	}
	out, err := s.hub.Beacon().Set(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "beacon.update", Detail: fmt.Sprintf("%s (Verzeichnis: %v)", out.URL, out.Listed)})
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/beacon/sync – sofort melden und das Ergebnis abwarten
func (s *Server) handleBeaconSync(w http.ResponseWriter, r *http.Request) {
	s.beaconSync(r.Context())
	writeJSON(w, http.StatusOK, beaconView(w, s.hub.Beacon().Get()))
}

// beaconView: letzte Fehlermeldung in der Sprache der Anfrage (soweit bekannt).
func beaconView(w http.ResponseWriter, b BeaconSettings) BeaconSettings {
	if b.LastErr != "" {
		b.LastErr = tr(w, b.LastErr)
	}
	return b
}

// GET /api/admin/beacon/collections?url=… – Sammlungen eines Beacons (zum Abonnieren)
func (s *Server) handleBeaconCollections(w http.ResponseWriter, r *http.Request) {
	base, coll := splitSubAddr(r.URL.Query().Get("url"))
	if base == "" {
		writeErr(w, http.StatusBadRequest, ErrBeaconURL.Error())
		return
	}
	name, list, err := s.hub.Media().BeaconCollections(base)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": base, "name": name, "collections": list, "selected": coll})
}

// splitSubAddr zerlegt "https://beacon.example.org#c_1234abcd" in Adresse und Sammlung.
func splitSubAddr(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	frag := ""
	if i := strings.Index(raw, "#"); i >= 0 {
		raw, frag = raw[:i], raw[i+1:]
	}
	base, err := normBeaconURL(raw)
	if err != nil {
		return "", ""
	}
	if !beaconCollRe.MatchString(frag) {
		frag = ""
	}
	return base, frag
}

// PUT /api/admin/beacon/subscriptions [{url, collections}]
func (s *Server) handleBeaconSubs(w http.ResponseWriter, r *http.Request) {
	var in []BeaconSub
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in) > maxBeaconSubs {
		writeErr(w, http.StatusBadRequest, ErrBeaconSubs.Error())
		return
	}
	var out []BeaconSub
	seen := map[string]bool{}
	for _, sub := range in {
		base, _ := splitSubAddr(sub.URL)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		var colls []string
		for _, c := range sub.Collections {
			if beaconCollRe.MatchString(c) && !contains(colls, c) {
				colls = append(colls, c)
			}
		}
		if colls == nil {
			colls = []string{}
		}
		name := strings.Join(strings.Fields(sub.Name), " ")
		if len([]rune(name)) > 60 {
			name = string([]rune(name)[:60])
		}
		out = append(out, BeaconSub{URL: base, Name: name, Collections: colls})
	}
	if out == nil {
		out = []BeaconSub{}
	}
	b := s.hub.Beacon()
	b.mu.Lock()
	b.cfg.Subs = out
	b.mu.Unlock()
	b.touch()
	s.hub.Media().notify()
	s.log(r, AuditEntry{Action: "beacon.subscriptions", Detail: fmt.Sprintf("%d Beacons", len(out))})
	writeJSON(w, http.StatusOK, out)
}
