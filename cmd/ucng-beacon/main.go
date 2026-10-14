// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// UCNG Beacon: Verzeichnis für UCNG-Instanzen (Unified Chat Network Gateway) und gemeinsamer GIF-Hub.
//
//   - Instanzen melden sich signiert an (Ed25519-Serverschlüssel aus /.well-known/ucng) und
//     aktualisieren ihre Angaben regelmäßig: Adresse, Name, Anzahl der Nutzer, Sprache, Land …
//     Die Weboberfläche listet sie durchsuchbar und filterbar.
//   - GIF-Hub: Das Beacon-Team pflegt GIF-Sammlungen (Hochladen, aus dem Web laden, aus GIPHY/Tenor
//     übernehmen). Jede Instanz kann Sammlungen abonnieren; ihre Nutzer finden die GIFs dann im Chat.
//
// Start:  ADMIN_PASSWORD='…' PUBLIC_URL=https://beacon.example.org go run ./cmd/ucng-beacon
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"ucng/internal/clientip"
	"ucng/internal/webstatic"

	"ucng/internal/i18n"
)

//go:embed web
var webFiles embed.FS

// version wird beim Bauen gesetzt: go build -ldflags "-X main.version=1.2.3"
var version = "dev"

// sourceURL: Adresse des Quellcodes (AGPL-3.0 § 13), beim Bauen gesetzt oder per SOURCE_URL.
var sourceURL = ""

const licenseID = "AGPL-3.0-or-later"

type Config struct {
	Addr          string
	DataDir       string
	AdminPassword string
	PublicURL     string
	HTTPSOnly     bool // nur Instanzen mit https:// annehmen
	PublicOnly    bool // keine Verbindungen zu Adressen im lokalen Netz
	TrustProxy    bool
	TrustedProxy  string // vertrauenswürdige Proxys (leer = Loopback und private Netze)
	GifBox        int    // GIFs passen in einen Rahmen dieser Größe
	NoPorts       bool   // Instanzen mit Port in der Adresse ablehnen
	SourceURL     string // Quellcode dieser Fassung (AGPL)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	var c Config
	flag.StringVar(&c.Addr, "addr", env("ADDR", ":8090"), "Listen-Adresse")
	flag.StringVar(&c.DataDir, "data", env("DATA_DIR", "beacon-data"), "Datenordner")
	flag.StringVar(&c.AdminPassword, "admin-password", env("ADMIN_PASSWORD", ""), "Passwort für die Verwaltung (leer = zufällig, steht im Log)")
	flag.StringVar(&c.PublicURL, "public-url", env("PUBLIC_URL", ""), "Öffentliche Adresse des Beacons, z. B. https://beacon.example.org")
	flag.BoolVar(&c.HTTPSOnly, "https-only", env("HTTPS_ONLY", "") == "1", "nur Instanzen mit https:// annehmen")
	flag.BoolVar(&c.PublicOnly, "public-only", env("PUBLIC_ONLY", "") == "1", "nur Instanzen mit öffentlicher IP-Adresse annehmen (keine im lokalen Netz)")
	flag.BoolVar(&c.TrustProxy, "trust-proxy", env("TRUST_PROXY", "") == "1", "Client-IP aus X-Forwarded-For (nur von vertrauenswürdigen Proxys)")
	flag.StringVar(&c.TrustedProxy, "trusted-proxies", env("TRUSTED_PROXIES", ""), "Vertrauenswürdige Proxys: IP-Adressen oder Netze, kommagetrennt (leer = Loopback und private Netze)")
	flag.BoolVar(&c.NoPorts, "no-ports", env("NO_PORTS", "") == "1", "Instanzen mit Port in der Adresse (chat.example.org:8443) ablehnen")
	flag.StringVar(&c.SourceURL, "source-url", env("SOURCE_URL", sourceURL), "Adresse des Quellcodes dieser Fassung (AGPL – bei eigenen Änderungen auf dein Repository setzen)")
	box, _ := strconv.Atoi(env("GIF_SIZE", "480"))
	flag.IntVar(&c.GifBox, "gif-size", box, "GIFs werden in diesen Rahmen eingepasst (Pixel)")
	flag.Parse()
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	c.SourceURL = cleanSourceURL(c.SourceURL)
	if c.GifBox < 100 || c.GifBox > 1000 {
		c.GifBox = 480
	}
	return c
}

type Server struct {
	ipr    *clientip.Resolver
	cfg    Config
	store  *Store
	admin  *adminAuth
	regRL  *limiter
	readRL *limiter
	client *http.Client
	i18n   *i18n.Catalog
	packs  *PackStore // Sprachpakete für Chat-Server
	mu     sync.Mutex
	seen   map[string]time.Time // Signaturen (gegen Wiederholung)
}

func NewServer(cfg Config, store *Store) *Server {
	s := &Server{cfg: cfg, store: store, admin: newAdminAuth(cfg.AdminPassword), seen: map[string]time.Time{},
		regRL: newLimiter(20, time.Hour), readRL: newLimiter(240, time.Minute)}
	s.client = newClient(cfg.PublicOnly)
	ipr, err := clientip.New(cfg.TrustProxy, cfg.TrustedProxy)
	if err != nil {
		log.Printf("%v – es gelten die Standard-Proxys (Loopback und private Netze)", err)
		ipr, _ = clientip.New(cfg.TrustProxy, "")
	}
	s.ipr = ipr
	cat, err := openCatalog(store.dir)
	if err != nil {
		panic("Sprachdateien: " + err.Error()) // eingebettet – nur bei kaputtem Build
	}
	s.i18n = cat
	packs, err := openPacks(store.dir)
	if err != nil {
		log.Printf("Sprachpakete: %v", err)
		packs = &PackStore{dir: filepath.Join(store.dir, "langpacks"), packs: map[string]*LangPack{}}
	}
	s.packs = packs
	return s
}

// staticFiles: die eingebettete Oberfläche – einmal je Prozess eingelesen und gepackt.
var staticFiles = sync.OnceValue(func() *webstatic.Handler {
	static, _ := fs.Sub(webFiles, "web")
	h, err := webstatic.New(static)
	if err != nil {
		panic("Oberfläche: " + err.Error())
	}
	return h
})

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", staticFiles()) // gzip, ETag, versionierte Adressen

	// öffentlich
	mux.HandleFunc("GET /api/v1/info", s.cors(s.handleInfo))
	mux.HandleFunc("GET /api/v1/instances", s.cors(s.handleInstances))
	mux.HandleFunc("POST /api/v1/register", s.handleRegister)
	mux.HandleFunc("GET /api/v1/collections", s.cors(s.handleCollections))
	mux.HandleFunc("GET /api/v1/gifs", s.cors(s.handleGIFs))
	mux.HandleFunc("GET /api/v1/gifs/{id}", s.cors(s.handleGIF))
	mux.HandleFunc("GET /media/{file}", s.handleMedia)
	mux.HandleFunc("GET /api/v1/languages", s.cors(s.handleLangPacks))
	mux.HandleFunc("GET /api/v1/languages/{code}", s.cors(s.handleLangPackFile))
	mux.HandleFunc("GET /api/v1/languages/{code}/package", s.cors(s.handleLangPackPackage))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	// Verwaltung
	a := s.admin.require
	mux.HandleFunc("POST /api/admin/login", s.handleLogin)
	mux.HandleFunc("POST /api/admin/logout", a(s.handleLogout))
	mux.HandleFunc("GET /api/admin/config", a(s.handleConfigGet))
	mux.HandleFunc("PUT /api/admin/config", a(s.handleConfigPut))
	mux.HandleFunc("GET /api/admin/instances", a(s.handleAdminInstances))
	mux.HandleFunc("PATCH /api/admin/instances/{domain}", a(s.handleInstancePatch))
	mux.HandleFunc("DELETE /api/admin/instances/{domain}", a(s.handleInstanceDelete))
	mux.HandleFunc("GET /api/admin/trust", a(s.handleTrustList))
	mux.HandleFunc("PUT /api/admin/trust/{domain}", a(s.handleTrustPut))
	mux.HandleFunc("POST /api/admin/collections", a(s.handleCollectionCreate))
	mux.HandleFunc("PATCH /api/admin/collections/{id}", a(s.handleCollectionUpdate))
	mux.HandleFunc("DELETE /api/admin/collections/{id}", a(s.handleCollectionDelete))
	mux.HandleFunc("POST /api/admin/gifs", a(s.handleGIFAdd))
	mux.HandleFunc("PATCH /api/admin/gifs/{id}", a(s.handleGIFUpdate))
	mux.HandleFunc("DELETE /api/admin/gifs/{id}", a(s.handleGIFDelete))
	mux.HandleFunc("GET /api/admin/web", a(s.handleWebSearch))
	mux.HandleFunc("POST /api/admin/langpacks/{code}", a(s.handleLangPackUpload))
	mux.HandleFunc("PATCH /api/admin/langpacks/{code}", a(s.handleLangPackMeta))
	mux.HandleFunc("DELETE /api/admin/langpacks/{code}", a(s.handleLangPackDelete))
	s.i18nAPI().Register(mux, a, s.cors) // Texte der Oberfläche und Sprach-Editor
	return securityHeaders(s.withLang(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/media/") {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'")
		}
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		next.ServeHTTP(w, r)
	})
}

// cors: öffentliche Lese-Schnittstellen dürfen von überall abgefragt werden.
func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if !s.readRL.allow(s.clientIP(r)) {
			writeErr(w, http.StatusTooManyRequests, "Zu viele Anfragen – bitte kurz warten.")
			return
		}
		next(w, r)
	}
}

func (s *Server) clientIP(r *http.Request) string { return s.ipr.IP(r) }

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func main() {
	cfg := loadConfig()
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = randomHex(8)
		log.Printf("Kein ADMIN_PASSWORD gesetzt. Temporäres Passwort für die Verwaltung: %s", cfg.AdminPassword)
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	srv := NewServer(cfg, store)
	go srv.backfillPreviews()
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 2 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			store.PruneInstances(time.Now())
			srv.regRL.cleanup()
			srv.readRL.cleanup()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	go func() {
		log.Printf("UCNG Beacon %s läuft auf %s – Daten in %s", version, cfg.Addr, filepath.Clean(cfg.DataDir))
		if cfg.SourceURL == "" {
			log.Printf("Hinweis: SOURCE_URL ist nicht gesetzt. Wer eine geänderte Fassung öffentlich betreibt, muss nach der AGPL den Quellcode anbieten.")
		}
		if cfg.PublicURL == "" {
			log.Printf("Hinweis: PUBLIC_URL ist nicht gesetzt – Instanzen signieren für die Adresse, unter der sie den Beacon aufrufen.")
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(sctx)
}
