// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"ucng/internal/fedsig"
)

// Anmeldung einer Instanz (POST /api/v1/register), signiert mit ihrem Serverschlüssel:
// Der Beacon lädt https://<domain>/.well-known/ucng, prüft die Signatur mit dem dort
// veröffentlichten Schlüssel und speichert die Angaben. Wer die Domain kontrolliert, kann also
// Angaben für sie machen – sonst niemand.

const RegisterContext = "gochat-beacon-v1" // Teil der Signatur – bleibt trotz Umbenennung gleich

var (
	langRe    = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)
	countryRe = regexp.MustCompile(`^[A-Z]{2}$`)
	domainRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:\d{1,5})?$`)
)

type registration struct {
	URL          string `json:"url"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Users        int    `json:"users"`
	Language     string `json:"language"`
	Country      string `json:"country"`
	Mode         string `json:"mode"`
	Federation   bool   `json:"federation"`
	Registration bool   `json:"registration"`
	Guests       string `json:"guests"`
	Version      string `json:"version"`
	Source       string `json:"source"` // Quellcode dieser Fassung (AGPL)
	Listed       bool   `json:"listed"` // false = aus dem Verzeichnis entfernen
	// freigegebene Kanäle (Kanal-Föderation)
	Channels []SharedChannel `json:"channels"`
}

// SharedChannel: ein Kanal, den eine Instanz für andere Instanzen freigibt.
type SharedChannel struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Mode        string    `json:"mode"` // read, write
	Messages    int       `json:"messages,omitempty"`
	LastAt      time.Time `json:"lastAt,omitempty"`
}

var channelNameRe = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{1,32}$`)

const maxSharedChannels = 100

type wellKnown struct {
	Software string `json:"software"`
	Domain   string `json:"domain"`
	Key      string `json:"key"`
}

// regScheme: Protokoll aus der gemeldeten Adresse (PUBLIC_URL der Instanz). http:// ist erlaubt,
// außer der Beacon läuft mit HTTPS_ONLY.
func (s *Server) regScheme(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err == nil && u.Scheme == "http" {
		if s.cfg.HTTPSOnly {
			return "", errors.New("Dieser Beacon nimmt nur Instanzen mit https:// an (PUBLIC_URL der Instanz beginnt mit http://)")
		}
		return "http", nil
	}
	return "https", nil
}

// instanceKey lädt den Serverschlüssel einer Instanz (/.well-known/ucng, bei älteren
// Instanzen /.well-known/gochat).
func (s *Server) instanceKey(ctx context.Context, scheme, domain string) (string, error) {
	key, err := s.instanceKeyAt(ctx, scheme, domain, fedsig.WellKnown)
	if err != nil && !errors.Is(err, errPrivate) {
		if k, err2 := s.instanceKeyAt(ctx, scheme, domain, fedsig.LegacyWellKnown); err2 == nil {
			return k, nil
		}
	}
	return key, err
}

func (s *Server) instanceKeyAt(ctx context.Context, scheme, domain, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	where := scheme + "://" + domain + path
	req, _ := http.NewRequestWithContext(ctx, "GET", where, nil)
	res, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, errPrivate) {
			return "", errPrivate
		}
		return "", fmt.Errorf("Der Beacon erreicht die Instanz nicht unter %s (%s). Ist PUBLIC_URL der Instanz vom Beacon aus erreichbar?", where, netReason(err))
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("%s antwortet mit Status %d – ist PUBLIC_URL bei der Instanz gesetzt und zeigt auf sie?", where, res.StatusCode)
	}
	var wk wellKnown
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&wk); err != nil || !fedsig.IsInstance(wk.Software) || wk.Key == "" {
		return "", fmt.Errorf("Unter %s antwortet keine UCNG-Instanz", where)
	}
	if !strings.EqualFold(wk.Domain, domain) {
		return "", fmt.Errorf("Unter %s meldet sich die Instanz %q statt %q – PUBLIC_URL prüfen", where, wk.Domain, domain)
	}
	return wk.Key, nil
}

// netReason übersetzt häufige Verbindungsfehler.
func netReason(err error) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	m := err.Error()
	switch {
	case errors.As(err, &dnsErr):
		return "Name nicht auflösbar"
	case errors.As(err, &certErr), strings.Contains(m, "x509"), strings.Contains(m, "certificate"):
		return "TLS-Zertifikat ungültig"
	case strings.Contains(m, "server gave HTTP response to HTTPS client"):
		return "die Instanz spricht nur http://, nicht https://"
	case strings.Contains(m, "first record does not look like a TLS handshake"), strings.Contains(m, "tls:"):
		return "TLS-Fehler – spricht die Instanz https://?"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(m, "timeout"):
		return "Zeitüberschreitung"
	case strings.Contains(m, "refused"):
		return "Verbindung abgelehnt – läuft die Instanz auf diesem Port?"
	case errors.As(err, &opErr):
		return "Netzwerkfehler"
	}
	return "Verbindungsfehler"
}

func (s *Server) seenSig(sig string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, t := range s.seen {
		if now.Sub(t) > 2*fedsig.MaxSkew {
			delete(s.seen, k)
		}
	}
	key := sig
	if raw, err := base64.StdEncoding.DecodeString(sig); err == nil {
		key = string(raw)
	}
	if _, ok := s.seen[key]; ok {
		return true
	}
	s.seen[key] = now
	return false
}

// POST /api/v1/register
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	domain, ts, sig := fedsig.FromRequest(r)
	domain = strings.ToLower(domain)
	if !domainRe.MatchString(domain) || ts == "" || sig == "" {
		writeErr(w, http.StatusUnauthorized, "Signatur fehlt")
		return
	}
	if strings.Contains(domain, ":") && s.cfg.NoPorts {
		writeErr(w, http.StatusBadRequest, "Dieser Beacon listet keine Instanzen mit Port in der Adresse")
		return
	}
	if !s.regRL.allow(domain) || !s.regRL.allow("ip:"+s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Anmeldungen – bitte später erneut")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10+1))
	if err != nil || len(body) > 16<<10 {
		writeErr(w, http.StatusRequestEntityTooLarge, "Zu groß")
		return
	}
	now := time.Now()
	if !fedsig.Fresh(ts, now) {
		writeErr(w, http.StatusUnauthorized, "Zeitstempel ungültig (Uhrzeit des Servers prüfen)")
		return
	}
	u, _ := url.Parse(s.baseURL(r))
	target := u.Host + "/api/v1/register"
	var peek struct {
		URL string `json:"url"`
	}
	json.Unmarshal(body, &peek)
	if pu, err := url.Parse(strings.TrimSpace(peek.URL)); peek.URL != "" && (err != nil || !strings.EqualFold(pu.Host, domain)) {
		writeErr(w, http.StatusBadRequest, "url muss die Startseite der angemeldeten Domain sein (PUBLIC_URL prüfen)")
		return
	}
	scheme, err := s.regScheme(peek.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := s.instanceKey(r.Context(), scheme, domain)
	if err != nil && peek.URL == "" && !s.cfg.HTTPSOnly { // ohne Adresse (z. B. Abmeldung): auch http:// versuchen
		key, err = s.instanceKey(r.Context(), "http", domain)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !fedsig.Verify(key, RegisterContext, domain, ts, "POST", target, body, sig) {
		writeErr(w, http.StatusUnauthorized, "Signatur ungültig")
		return
	}
	if s.seenSig(sig) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var in registration
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
		return
	}
	if !in.Listed {
		s.store.RemoveInstance(domain)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "listed": false})
		return
	}
	x, err := cleanRegistration(in, domain)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, _ := base64.StdEncoding.DecodeString(key)
	x.Key = fedsig.Fingerprint(raw)
	if err := s.store.PutInstance(x, now); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "listed": true, "url": s.baseURL(r) + "/#instanzen", "trust": s.store.TrustOf(domain)})
}

func cleanRegistration(in registration, domain string) (Instance, error) {
	u, err := url.Parse(strings.TrimRight(in.URL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || strings.ToLower(u.Host) != domain || (u.Path != "" && u.Path != "/") {
		return Instance{}, errors.New("url muss die Startseite der angemeldeten Domain sein")
	}
	x := Instance{Domain: domain, URL: u.Scheme + "://" + domain, Name: cleanText(in.Name, 60), Description: cleanText(in.Description, 280),
		Users: in.Users, Federation: in.Federation, Registration: in.Registration, Version: cleanText(in.Version, 20),
		Source: cleanSourceURL(in.Source)}
	if x.Name == "" {
		x.Name = domain
	}
	if x.Users < 0 {
		x.Users = 0
	}
	if x.Users > 10_000_000 {
		x.Users = 10_000_000
	}
	if in.Language != "" {
		if !langRe.MatchString(in.Language) {
			return Instance{}, errors.New("Sprache als Kürzel, z. B. de oder en")
		}
		x.Language = in.Language
	}
	if in.Country != "" {
		if !countryRe.MatchString(in.Country) {
			return Instance{}, errors.New("Land als Kürzel, z. B. DE oder AT")
		}
		x.Country = in.Country
	}
	switch in.Mode {
	case "simple", "hosted":
		x.Mode = in.Mode
	}
	switch in.Guests {
	case "none", "read", "write":
		x.Guests = in.Guests
	}
	seen := map[string]bool{}
	for _, c := range in.Channels {
		if !channelNameRe.MatchString(c.Name) || seen[c.Name] || (c.Mode != "read" && c.Mode != "write") {
			continue
		}
		seen[c.Name] = true
		if c.Messages < 0 {
			c.Messages = 0
		}
		if c.LastAt.After(time.Now().Add(time.Hour)) {
			c.LastAt = time.Time{}
		}
		x.Channels = append(x.Channels, SharedChannel{Name: c.Name, Description: cleanText(c.Description, 140), Mode: c.Mode, Messages: c.Messages, LastAt: c.LastAt.UTC()})
		if len(x.Channels) >= maxSharedChannels {
			break
		}
	}
	return x, nil
}

// ChannelListing: ein freigegebener Kanal im Verzeichnis.
type ChannelListing struct {
	Address     string    `json:"address"` // #kanal@instanz
	Domain      string    `json:"domain"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Mode        string    `json:"mode"`
	Title       string    `json:"title"` // Name der Instanz
	Messages    int       `json:"messages,omitempty"`
	LastAt      time.Time `json:"lastAt,omitempty"`
	Trust       string    `json:"trust"`
	Language    string    `json:"language,omitempty"`
	URL         string    `json:"url"`
}

// GET /api/v1/channels?q=&language=&trust=&mode= – freigegebene Kanäle aller Instanzen
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	lang, mode, trust := r.URL.Query().Get("language"), r.URL.Query().Get("mode"), r.URL.Query().Get("trust")
	if trust == "community" {
		trust = TrustCommunity
	} else if !validTrust(trust) || trust == "" {
		trust = "*"
	}
	out := []ChannelListing{}
	for _, x := range s.store.Instances(time.Now(), false) {
		if lang != "" && x.Language != lang && !strings.HasPrefix(x.Language, lang+"-") {
			continue
		}
		if trust != "*" && x.Trust != trust {
			continue
		}
		for _, c := range x.Channels {
			if mode != "" && c.Mode != mode {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(c.Name+" "+c.Description+" "+x.Name+" "+x.Domain), q) {
				continue
			}
			out = append(out, ChannelListing{Address: "#" + c.Name + "@" + x.Domain, Domain: x.Domain, Name: c.Name, Description: c.Description,
				Mode: c.Mode, Title: x.Name, Messages: c.Messages, LastAt: c.LastAt, Trust: x.Trust, Language: x.Language, URL: x.URL})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := trustRank(a.Trust), trustRank(b.Trust); ra != rb {
			return ra < rb
		}
		if !a.LastAt.Equal(b.LastAt) {
			return a.LastAt.After(b.LastAt)
		}
		return a.Address < b.Address
	})
	total := len(out)
	if len(out) > 500 {
		out = out[:500]
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "total": total})
}

// GET /api/v1/instances?q=&language=&country=&trust=official|recommended|community&sort=users|name|new|active
func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	trust := r.URL.Query().Get("trust")
	if trust == "community" {
		trust = TrustCommunity
	} else if !validTrust(trust) {
		trust = "*"
	}
	if r.URL.Query().Get("trust") == "" {
		trust = "*"
	}
	lang, country := r.URL.Query().Get("language"), strings.ToUpper(r.URL.Query().Get("country"))
	list := s.store.Instances(time.Now(), false)
	out := list[:0]
	for _, x := range list {
		if q != "" && !strings.Contains(strings.ToLower(x.Name+" "+x.Domain+" "+x.Description), q) {
			continue
		}
		if lang != "" && x.Language != lang && !strings.HasPrefix(x.Language, lang+"-") {
			continue
		}
		if country != "" && x.Country != country {
			continue
		}
		if trust != "*" && x.Trust != trust {
			continue
		}
		out = append(out, x)
	}
	sortInstances(out, r.URL.Query().Get("sort"))
	writeJSON(w, http.StatusOK, map[string]any{"instances": out, "total": len(out)})
}

func sortInstances(list []Instance, by string) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := trustRank(a.Trust), trustRank(b.Trust); ra != rb { // Offiziell, Empfohlen, Community
			return ra < rb
		}
		switch by {
		case "name":
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		case "new":
			return a.First.After(b.First)
		case "active":
			return a.Last.After(b.Last)
		}
		if a.Users != b.Users {
			return a.Users > b.Users
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// GET /api/v1/info
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	c := s.store.Config()
	inst := s.store.Instances(time.Now(), false)
	users := 0
	langs := map[string]int{}
	countries := map[string]int{}
	trust := map[string]int{"official": 0, "recommended": 0, "standard": 0, "community": 0}
	for _, x := range inst {
		if x.Trust == TrustCommunity {
			trust["community"]++
		} else {
			trust[x.Trust]++
		}
		users += x.Users
		if x.Language != "" {
			langs[strings.Split(x.Language, "-")[0]]++
		}
		if x.Country != "" {
			countries[x.Country]++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"software": fedsig.BeaconSoftware, "version": 1, "build": version, "name": c.Name, "description": c.Description,
		"url": s.baseURL(r), "instances": len(inst), "users": users, "gifs": len(s.store.GIFList()), "collections": len(s.store.Collections()),
		"languages": langs, "countries": countries, "trust": trust, "credit": s.credit(),
		"license": licenseID, "source": s.cfg.SourceURL})
}

// --- Verwaltung ---

func (s *Server) handleAdminInstances(w http.ResponseWriter, r *http.Request) {
	list := s.store.Instances(time.Now(), true)
	sortInstances(list, "active")
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleInstancePatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hidden bool `json:"hidden"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	x, ok := s.store.SetHidden(strings.ToLower(r.PathValue("domain")), in.Hidden)
	if !ok {
		writeErr(w, http.StatusNotFound, "Diese Instanz ist nicht gemeldet")
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.RemoveInstance(strings.ToLower(r.PathValue("domain"))) {
		writeErr(w, http.StatusNotFound, "Diese Instanz ist nicht gemeldet")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	c := s.store.Config()
	if c.APIKey != "" {
		c.APIKey = keepSecret
	}
	writeJSON(w, http.StatusOK, c)
}

const keepSecret = "••••••••"

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var in BeaconConfig
	if !decodeJSON(w, r, &in) {
		return
	}
	old := s.store.Config()
	in.Name, in.Description, in.Credit = cleanText(in.Name, 60), cleanText(in.Description, 400), cleanText(in.Credit, 40)
	if in.Name == "" {
		in.Name = "UCNG Beacon"
	}
	if in.Provider != "" && in.Provider != "giphy" && in.Provider != "tenor" {
		writeErr(w, http.StatusBadRequest, "Anbieter: leer, giphy oder tenor")
		return
	}
	switch in.Rating {
	case "g", "pg", "pg-13", "r":
	default:
		in.Rating = "g"
	}
	if in.APIKey == keepSecret {
		in.APIKey = old.APIKey
	}
	if err := s.store.SetConfig(in); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.handleConfigGet(w, r)
}
