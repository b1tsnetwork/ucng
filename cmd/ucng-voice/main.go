// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// ucng-voice ist ein eigenständiger Sprach-Relay für UCNG. Er verteilt die Last der
// Sprachkanäle auf weitere Server: Der Chat-Server bleibt für Rechte und Zutritt zuständig, der
// Agent leitet nur Sprache weiter. Beliebig viele Agenten können sich bei einem Chat-Server
// melden; neue Sprachkanäle landen auf dem am wenigsten ausgelasteten.
//
//	CHAT_URL=http://chat:8080 AGENT_TOKEN=… PUBLIC_URL=wss://voice1.example.org ucng-voice
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ucng/internal/clientip"
	"ucng/internal/voicerelay"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type config struct {
	Addr, ChatURL, Token, PublicURL, ControlURL, ID string
	MaxParticipants, MaxPerIP                       int
	MaxMbit                                         float64
	Origins                                         string
	Role                                            string // "" = alles, "media" = nur Bildschirm und Kameras
	TrustProxy                                      bool
	TrustedProxies                                  string
}

func loadConfig() config {
	var c config
	host, _ := os.Hostname()
	maxP, _ := strconv.Atoi(env("MAX_PARTICIPANTS", "200"))
	perIP, _ := strconv.Atoi(env("MAX_PER_IP", "40")) // je Person bis zu drei Verbindungen (Sprache, Bildschirm, Kameras); an Firmen-NAT denken
	maxM, _ := strconv.ParseFloat(env("MAX_MBIT", "50"), 64)
	flag.StringVar(&c.Addr, "addr", env("ADDR", ":8090"), "Listen-Adresse")
	flag.StringVar(&c.ChatURL, "chat-url", env("CHAT_URL", ""), "Adresse des Chat-Servers (intern erreichbar), z. B. http://chat:8080")
	flag.StringVar(&c.Token, "token", env("AGENT_TOKEN", ""), "Agenten-Schlüssel (VOICE_AGENT_TOKEN des Chat-Servers)")
	flag.StringVar(&c.PublicURL, "public-url", env("PUBLIC_URL", ""), "Öffentliche Adresse dieses Agenten für Browser, z. B. wss://voice1.example.org")
	flag.StringVar(&c.ControlURL, "control-url", env("CONTROL_URL", ""), "Adresse, unter der der Chat-Server diesen Agenten erreicht (Standard: http://<hostname><addr>/control)")
	flag.StringVar(&c.ID, "id", env("AGENT_ID", host), "Name dieses Agenten")
	flag.IntVar(&c.MaxParticipants, "max-participants", maxP, "Teilnehmer insgesamt")
	flag.IntVar(&c.MaxPerIP, "max-per-ip", perIP, "Sprachverbindungen je IP-Adresse")
	flag.Float64Var(&c.MaxMbit, "max-mbit", maxM, "Höchstens so viel ausgehende Bandbreite (Mbit/s, geschätzt)")
	flag.StringVar(&c.Origins, "origins", env("ALLOWED_ORIGINS", ""), "Erlaubte Herkunft der Browser (kommagetrennt; Standard: PUBLIC_URL des Chat-Servers)")
	flag.BoolVar(&c.TrustProxy, "trust-proxy", env("TRUST_PROXY", "") == "1", "X-Forwarded-For von vertrauenswürdigen Proxys verwenden")
	flag.StringVar(&c.TrustedProxies, "trusted-proxies", env("TRUSTED_PROXIES", ""), "Vertrauenswürdige Proxys (leer = Loopback und private Netze)")
	flag.StringVar(&c.Role, "role", env("ROLE", ""), "Aufgaben: leer = Sprache, Bildschirm und Kameras; \"media\" = nur Bildschirm und Kameras (ausgelagert)")
	flag.Parse()
	if c.Role != "" && c.Role != "media" {
		log.Fatal("ROLE: leer oder \"media\"")
	}
	c.ChatURL = strings.TrimRight(c.ChatURL, "/")
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.ControlURL == "" {
		port := strings.TrimPrefix(c.Addr, ":")
		if _, p, err := net.SplitHostPort(c.Addr); err == nil {
			port = p
		}
		c.ControlURL = "http://" + host + ":" + port + "/control"
	}
	return c
}

// wsURL: öffentliche WebSocket-Adresse (https:// wird zu wss://).
func wsURL(public string) (string, error) {
	u, err := url.Parse(public)
	if err != nil || u.Host == "" {
		return "", errors.New("PUBLIC_URL fehlt oder ist ungültig (z. B. wss://voice1.example.org)")
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", errors.New("PUBLIC_URL muss mit wss://, ws://, https:// oder http:// beginnen")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	return u.String(), nil
}

type agent struct {
	cfg     config
	ws      string
	relay   *voicerelay.Relay
	pub     atomic.Pointer[ed25519.PublicKey]
	origins atomic.Pointer[[]string]
	kick    chan struct{} // sofort melden
	client  *http.Client
	ipr     *clientip.Resolver
	ipMu    sync.Mutex
	perIP   map[string]int
}

func main() {
	cfg := loadConfig()
	if cfg.ChatURL == "" || cfg.Token == "" {
		log.Fatal("CHAT_URL und AGENT_TOKEN müssen gesetzt sein")
	}
	ws, err := wsURL(cfg.PublicURL)
	if err != nil {
		log.Fatal(err)
	}
	ipr, err := clientip.New(cfg.TrustProxy, cfg.TrustedProxies)
	if err != nil {
		log.Fatal(err)
	}
	a := &agent{cfg: cfg, ws: ws, kick: make(chan struct{}, 1), client: &http.Client{Timeout: 10 * time.Second},
		ipr: ipr, perIP: map[string]int{}}
	if cfg.Origins != "" {
		var list []string
		for _, o := range strings.Split(cfg.Origins, ",") {
			if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
				list = append(list, o)
			}
		}
		a.origins.Store(&list)
	}
	a.relay = voicerelay.New(voicerelay.Config{
		PublicKey: func() ed25519.PublicKey {
			if k := a.pub.Load(); k != nil {
				return *k
			}
			return nil
		},
		MaxParticipants: cfg.MaxParticipants,
		MaxMbit:         cfg.MaxMbit,
		OriginOK:        a.originOK,
		OnChange:        func(string, []voicerelay.PeerInfo) { a.soon() },
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", a.handleWS)
	mux.HandleFunc("POST /control", a.handleControl)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if a.pub.Load() == nil {
			http.Error(w, "noch nicht beim Chat-Server angemeldet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.reportLoop(ctx)
	go func() {
		log.Printf("ucng-voice %s läuft auf %s – öffentlich %s, Chat-Server %s", version, cfg.Addr, ws, cfg.ChatURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Println("Fahre herunter …")
	a.relay.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	a.report(context.Background()) // leere Kanäle melden
}

func (a *agent) originOK(origin string) bool {
	list := a.origins.Load()
	if list == nil {
		return false
	}
	for _, o := range *list {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (a *agent) soon() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *agent) handleWS(w http.ResponseWriter, r *http.Request) {
	if a.pub.Load() == nil {
		http.Error(w, "noch nicht beim Chat-Server angemeldet", http.StatusServiceUnavailable)
		return
	}
	ip := a.ipr.IP(r)
	a.ipMu.Lock()
	if a.perIP[ip] >= a.cfg.MaxPerIP {
		a.ipMu.Unlock()
		http.Error(w, "Zu viele Sprachverbindungen von dieser Adresse", http.StatusTooManyRequests)
		return
	}
	a.perIP[ip]++
	a.ipMu.Unlock()
	defer func() {
		a.ipMu.Lock()
		if a.perIP[ip]--; a.perIP[ip] <= 0 {
			delete(a.perIP, ip)
		}
		a.ipMu.Unlock()
	}()
	a.relay.ServeHTTP(w, r)
}

// POST /control – Steuerbefehl des Chat-Servers.
func (a *agent) handleControl(w http.ResponseWriter, r *http.Request) {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(t) != len(a.cfg.Token) || subtle.ConstantTimeCompare([]byte(t), []byte(a.cfg.Token)) != 1 {
		http.Error(w, "nicht berechtigt", http.StatusUnauthorized)
		return
	}
	var in struct {
		Channel, User, Action, Reason string
		Value                         bool
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		http.Error(w, "ungültiges JSON", http.StatusBadRequest)
		return
	}
	var ok bool
	switch in.Action {
	case "speak", "mute", "hand", "kick", "close", "screen", "video", "mod", "move", "mediakick", "remedia", "forcemute", "askunmute", "camsoff", "rec", "cap":
		ok = a.relay.Control(in.Channel, in.User, in.Action, in.Value, in.Reason)
	default:
		http.Error(w, "unbekannte Aktion", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":%t}`, ok)
}

// reportLoop meldet Auslastung und Teilnehmer alle 3 Sekunden – bei Änderungen sofort
// (höchstens alle 150 ms).
func (a *agent) reportLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	fails := 0
	for {
		if err := a.report(ctx); err != nil {
			if fails++; fails == 1 || fails%20 == 0 {
				log.Printf("Meldung an den Chat-Server fehlgeschlagen: %v", err)
			}
		} else if fails > 0 {
			log.Printf("Wieder mit dem Chat-Server verbunden")
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
			time.Sleep(150 * time.Millisecond)
		}
	}
}

func (a *agent) report(ctx context.Context) error {
	st := a.relay.Stats()
	rooms := map[string][]voicerelay.PeerInfo{}
	for ch := range st.Channels {
		rooms[ch] = a.relay.Peers(ch)
	}
	body, _ := json.Marshal(map[string]any{"id": a.cfg.ID, "wsURL": a.ws, "controlURL": a.cfg.ControlURL,
		"version": version, "stats": st, "rooms": rooms, "role": a.cfg.Role})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ChatURL+"/api/voice/agents/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Antwort %d (AGENT_TOKEN und VOICE_AGENT_TOKEN prüfen)", res.StatusCode)
	}
	var out struct {
		PublicKey string   `json:"publicKey"`
		Origins   []string `json:"origins"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(out.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("ungültiger Sprach-Schlüssel vom Chat-Server")
	}
	pk := ed25519.PublicKey(key)
	if old := a.pub.Load(); old == nil || !old.Equal(pk) {
		a.pub.Store(&pk)
		log.Printf("Beim Chat-Server angemeldet")
	}
	if a.cfg.Origins == "" {
		list := out.Origins
		a.origins.Store(&list)
	}
	return nil
}
