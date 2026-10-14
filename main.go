// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"ucng/internal/clientip"
	"ucng/internal/fedsig"
	"ucng/internal/webstatic"
)

//go:embed web
var webFiles embed.FS

// version wird beim Bauen gesetzt: go build -ldflags "-X main.version=1.2.3"
var version = "dev"

// sourceURL: öffentliche Adresse des Quellcodes (AGPL-3.0 § 13: Wer eine geänderte Fassung im Netz
// betreibt, muss den Nutzern den Quellcode anbieten). Offizielle Builds setzen sie beim Bauen
// (-ldflags "-X main.sourceURL=https://github.com/…/ucng"); Betreiber überschreiben sie mit SOURCE_URL.
var sourceURL = ""

const licenseID = "AGPL-3.0-or-later"

// Config enthält alle Einstellungen (Flags oder Umgebungsvariablen).
type Config struct {
	Addr           string
	AllowedOrigins []string // "*" = alle
	DataDir        string   // Ordner für alle Dateien; leer = nichts speichern
	MaxRooms       int      // 0 = unbegrenzt
	TrustProxy     bool
	TrustedProxies string        // vertrauenswürdige Proxys (IPs/Netze, kommagetrennt; leer = Loopback und private Netze)
	MaxStreams     int           // gleichzeitige Live-Verbindungen insgesamt
	MaxStreamsIP   int           // gleichzeitige Live-Verbindungen je IP-Adresse
	RateLimit      int           // Nachrichten pro Fenster und IP
	RateWindow     time.Duration // Fensterlänge
	NameTTL        time.Duration // ungenutzte Namen werden danach wieder frei (0 = nie)
	MaxUsers       int
	ClaimLimit     int    // Namensreservierungen pro Minute und IP
	BotRate        int    // Nachrichten pro 10 Sekunden und Bot
	AdminPassword  string // Passwort für /admin
	BackupDir      string // Ordner für Backups (Standard: neben dem Datenordner)
	Mode           string // "simple" (Namen ohne Konto) oder "hosted" (Nutzerkonten, 2FA, E2E, Föderation)
	Storage        string // "files", "sqlite" oder "mysql"
	DBDSN          string // Verbindung zur Datenbank (sqlite: Dateipfad, mysql: user:pass@tcp(host:3306)/db)
	PublicURL      string // öffentliche Adresse, z. B. https://chat.example.org (Links in E-Mails, Föderation)
	SourceURL      string // Adresse des Quellcodes dieser Fassung (AGPL), z. B. https://github.com/…/ucng
	FedInsecure    bool   // Föderation auch über http:// (nur für Tests!)
	BackupKeep     int    // Anzahl aufbewahrter Backups (0 = keine automatischen Backups)
	// Sprachkanäle (voice.go)
	VoiceOff             bool    // Sprachkanäle ganz aus
	VoiceNoBuiltin       bool    // kein eingebauter Relay (nur Agenten, cmd/ucng-voice)
	VoiceMaxParticipants int     // eingebauter Relay: Teilnehmer insgesamt
	VoiceMaxMbit         float64 // eingebauter Relay: geschätzte ausgehende Bandbreite
	VoiceAgentToken      string  // Schlüssel, mit dem sich Agenten anmelden (leer = keine Agenten)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

func loadConfig() Config {
	var c Config
	var origins string
	flag.StringVar(&c.Addr, "addr", env("ADDR", ":8080"), "Listen-Adresse")
	flag.StringVar(&origins, "origins", env("ALLOWED_ORIGINS", ""), "Zusätzlich erlaubte Origins für das Embed-Widget (kommagetrennt, * = alle). Sonst gilt die Einstellung „Einbetten“ im Admin-Bereich")
	defDir := "data"
	if f := os.Getenv("DATA_FILE"); f != "" { // ältere Konfiguration: Ordner der Datei verwenden
		defDir = filepath.Dir(f)
	}
	flag.StringVar(&c.DataDir, "data", env("DATA_DIR", defDir), `Ordner für Kanäle, Nachrichten, Einstellungen, Automod und Namen ("none" = nichts speichern)`)
	flag.IntVar(&c.MaxRooms, "max-rooms", envInt("MAX_ROOMS", 0), "Maximale Anzahl Kanäle (0 = unbegrenzt)")
	flag.BoolVar(&c.TrustProxy, "trust-proxy", env("TRUST_PROXY", "") == "1", "X-Forwarded-For für Client-IP verwenden (nur von vertrauenswürdigen Proxys)")
	flag.StringVar(&c.TrustedProxies, "trusted-proxies", env("TRUSTED_PROXIES", ""), "Vertrauenswürdige Proxys: IP-Adressen oder Netze, kommagetrennt (leer = Loopback und private Netze)")
	flag.IntVar(&c.MaxStreams, "max-streams", envInt("MAX_STREAMS", 10000), "Gleichzeitige Live-Verbindungen insgesamt")
	flag.IntVar(&c.MaxStreamsIP, "max-streams-ip", envInt("MAX_STREAMS_PER_IP", 50), "Gleichzeitige Live-Verbindungen je IP-Adresse")
	flag.IntVar(&c.RateLimit, "rate", envInt("RATE_LIMIT", 10), "Nachrichten pro 10 Sekunden und IP")
	nameDays := flag.Int("name-ttl", envInt("NAME_TTL_DAYS", 30), "Tage, nach denen ungenutzte Namen wieder frei werden (0 = nie)")
	flag.IntVar(&c.MaxUsers, "max-users", envInt("MAX_USERS", 10000), "Maximale Anzahl reservierter Namen")
	flag.IntVar(&c.ClaimLimit, "claim-rate", envInt("CLAIM_RATE", 5), "Namensreservierungen pro Minute und IP")
	flag.IntVar(&c.BotRate, "bot-rate", envInt("BOT_RATE", 30), "Nachrichten pro 10 Sekunden und Bot")
	flag.StringVar(&c.AdminPassword, "admin-password", env("ADMIN_PASSWORD", ""), "Passwort für den Admin-Bereich /admin (leer = bei jedem Start zufällig erzeugt)")
	flag.StringVar(&c.BackupDir, "backup-dir", env("BACKUP_DIR", ""), "Ordner für Backups (Standard: <Datenordner>-backups)")
	flag.IntVar(&c.BackupKeep, "backup-keep", envInt("BACKUP_KEEP", 7), "Anzahl aufbewahrter Backups, täglich um 3 Uhr (0 = aus)")
	flag.StringVar(&c.SourceURL, "source-url", env("SOURCE_URL", sourceURL), "Adresse des Quellcodes dieser Fassung (AGPL – bei eigenen Änderungen auf dein Repository setzen)")
	flag.StringVar(&c.Mode, "mode", env("MODE", "simple"), "Betriebsart: simple (Namen ohne Konto) oder hosted (Nutzerkonten, 2FA, verschlüsselte Direktnachrichten, Föderation)")
	flag.StringVar(&c.Storage, "storage", env("STORAGE", "files"), "Speicher: files, sqlite oder mysql (sqlite/mysql nur mit Build-Tag)")
	flag.StringVar(&c.DBDSN, "db", env("DB_DSN", ""), "Datenbank-Verbindung (sqlite: Pfad, Standard <Datenordner>/ucng.db; mysql: user:pass@tcp(host:3306)/ucng)")
	flag.StringVar(&c.PublicURL, "public-url", env("PUBLIC_URL", ""), "Öffentliche Adresse (z. B. https://chat.example.org) – für Links in E-Mails und die Föderation")
	flag.BoolVar(&c.FedInsecure, "fed-insecure", env("FED_INSECURE", "") == "1", "Föderation auch ohne HTTPS erlauben (nur für Tests)")
	flag.BoolVar(&c.VoiceOff, "voice-off", env("VOICE", "1") == "0", "Sprachkanäle ausschalten")
	flag.BoolVar(&c.VoiceNoBuiltin, "voice-no-builtin", env("VOICE_BUILTIN", "1") == "0", "Keinen eingebauten Sprach-Relay starten (nur Agenten, cmd/ucng-voice)")
	flag.IntVar(&c.VoiceMaxParticipants, "voice-max-participants", envInt("VOICE_MAX_PARTICIPANTS", 200), "Eingebauter Sprach-Relay: Teilnehmer insgesamt")
	flag.Float64Var(&c.VoiceMaxMbit, "voice-max-mbit", envFloat("VOICE_MAX_MBIT", 50), "Eingebauter Sprach-Relay: höchstens so viel ausgehende Bandbreite (Mbit/s, geschätzt)")
	flag.StringVar(&c.VoiceAgentToken, "voice-agent-token", env("VOICE_AGENT_TOKEN", ""), "Schlüssel für Sprach-Agenten (cmd/ucng-voice); leer = keine Agenten")
	flag.Parse()
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode != ModeHosted {
		c.Mode = ModeSimple
	}
	c.Storage = strings.ToLower(strings.TrimSpace(c.Storage))
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	c.SourceURL = cleanSourceURL(c.SourceURL)
	if c.DataDir == "none" {
		c.DataDir = ""
	}
	if c.BackupDir == "" && c.DataDir != "" {
		c.BackupDir = filepath.Clean(c.DataDir) + "-backups"
	}
	c.NameTTL = time.Duration(*nameDays) * 24 * time.Hour
	c.RateWindow = 10 * time.Second
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, strings.TrimRight(o, "/"))
		}
	}
	return c
}

// --- Rate Limiting ---

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

func (rl *rateLimiter) Allow(key string) bool { return rl.AllowN(key, rl.limit) }

// AllowN wie Allow, aber mit eigenem Limit für diesen Schlüssel (z. B. je Bot).
func (rl *rateLimiter) AllowN(key string, limit int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cut := now.Add(-rl.window)
	h := rl.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= limit {
		rl.hits[key] = h
		return false
	}
	rl.hits[key] = append(h, now)
	return true
}

func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cut := time.Now().Add(-rl.window)
	for k, h := range rl.hits {
		if len(h) == 0 || h[len(h)-1].Before(cut) {
			delete(rl.hits, k)
		}
	}
}

// --- Server ---

type Server struct {
	cfg         Config
	ipr         *clientip.Resolver
	streams     *streamLimiter // gleichzeitige Live-Verbindungen
	templates   TemplateStore  // geprüfte Vorlagen (templates.go)
	tplMu       sync.Mutex
	hub         *Hub
	users       *Users
	rl          *rateLimiter // Nachrichten
	claimRL     *rateLimiter // Namensreservierungen
	loginRL     *rateLimiter // Admin-Anmeldungen
	botRL       *rateLimiter // Nachrichten je Bot
	gifRL       *rateLimiter
	previewRL   *rateLimiter  // Vorschaubilder der GIF-Suche je IP
	previews    *previewCache // … und ihr Zwischenspeicher
	reportRL    *rateLimiter  // Meldungen je Person
	uploadRL    *rateLimiter  // Bilder je Person
	dmRL        *rateLimiter  // neue Direktnachrichten je Person // GIF-Suchen im Web je IP
	acctRL      *rateLimiter  // Anmeldung/Registrierung je IP (Modus „hosted“)
	mailRL      *rateLimiter  // E-Mails je IP
	storageKind string        // files, sqlite, mysql (Anzeige im Admin-Bereich)
	fedRL       *rateLimiter  // Föderation: Eingang je Domain, Abfragen je Person
	fedOnce     sync.Once
	fedHTTP     *http.Client
	accounts    *Accounts
	backups     *Backups
	webhooks    *webhooks
	voice       *voiceMgr     // Sprachkanäle (nil = aus)
	done        chan struct{} // wird beim Herunterfahren geschlossen, beendet SSE-Streams
}

func NewServer(cfg Config, hub *Hub, users *Users) *Server {
	if cfg.BotRate <= 0 {
		cfg.BotRate = 30
	}
	if cfg.Mode != ModeHosted {
		cfg.Mode = ModeSimple
	}
	ipr, err := clientip.New(cfg.TrustProxy, cfg.TrustedProxies)
	if err != nil {
		log.Printf("%v – es gelten die Standard-Proxys (Loopback und private Netze)", err)
		ipr, _ = clientip.New(cfg.TrustProxy, "")
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 10000
	}
	if cfg.MaxStreamsIP <= 0 {
		cfg.MaxStreamsIP = 50
	}
	s := &Server{
		cfg: cfg, hub: hub, users: users, ipr: ipr, streams: newStreamLimiter(cfg.MaxStreams, cfg.MaxStreamsIP),
		rl:          newRateLimiter(cfg.RateLimit, cfg.RateWindow),
		claimRL:     newRateLimiter(cfg.ClaimLimit, time.Minute),
		loginRL:     newRateLimiter(5, time.Minute),
		botRL:       newRateLimiter(cfg.BotRate, 10*time.Second),
		gifRL:       newRateLimiter(30, time.Minute),
		previewRL:   newRateLimiter(240, time.Minute),
		previews:    newPreviewCache(),
		reportRL:    newRateLimiter(10, 10*time.Minute),
		uploadRL:    newRateLimiter(15, 10*time.Minute),
		dmRL:        newRateLimiter(10, time.Hour),
		acctRL:      newRateLimiter(10, time.Minute),
		mailRL:      newRateLimiter(5, time.Hour),
		fedRL:       newRateLimiter(120, time.Minute),
		storageKind: cfg.Storage,
		accounts:    hub.Accounts(),
		done:        make(chan struct{}),
	}
	// Namen von Bots und Personen schließen sich gegenseitig aus.
	hub.Bots().users = users
	users.SetReserved(func(name string, team bool) error {
		if hub.Bots().HasNameKey(nameKey(name)) || nameKey(name) == nameKey(hub.Commands().BotName()) {
			return ErrNameTaken
		}
		if !team && hub.Mod().Protected(name) {
			return ErrNameProtected
		}
		return nil
	})
	users.SetPrivileged(func(roles []string) bool { return hub.Roles().PermsOf(roles, false).Protected })
	users.OnSessionsEnded(hub.DropViewer)
	if cfg.AdminPassword != "" {
		s.accounts.SetSuperPassword(cfg.AdminPassword)
	}
	if hub.Media().Dir() == "" { // ohne Datenordner (Tests, -data none): vorübergehender Ordner
		if dir, err := os.MkdirTemp("", "ucng-media-"); err == nil {
			hub.Media().SetDir(dir)
		}
	}
	if cfg.DataDir != "" && cfg.BackupKeep > 0 {
		s.backups = NewBackups(cfg.DataDir, cfg.BackupDir, cfg.BackupKeep)
	}
	s.webhooks = newWebhooks(hub, s.botSay)
	if !cfg.VoiceOff {
		if s.cfg.VoiceMaxParticipants <= 0 {
			s.cfg.VoiceMaxParticipants = 200
		}
		if s.cfg.VoiceMaxMbit <= 0 {
			s.cfg.VoiceMaxMbit = 50
		}
		s.voice = newVoiceMgr(s)
	}
	return s
}

// staticFiles: die eingebettete Oberfläche – einmal je Prozess eingelesen und gepackt.
var staticFiles = sync.OnceValue(func() *webstatic.Handler {
	static, _ := fs.Sub(webFiles, "web")
	h, err := webstatic.New(static) // gzip, ETag, versionierte Adressen
	if err != nil {
		panic("Oberfläche: " + err.Error()) // eingebettet – nur bei kaputtem Build
	}
	h.CacheFor = func(name string) string {
		if strings.HasPrefix(name, "embed/") { // Widget-Lader auf fremden Seiten: ohne Version, kurz cachen
			return "public, max-age=300"
		}
		return ""
	}
	return h
})

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	fileServer := staticFiles()
	mod, adm := s.requireMod, s.requireAdmin

	// Chat (auch für das Widget auf fremden Webseiten)
	mux.HandleFunc("GET /api/settings", s.handleSettings)
	mux.HandleFunc("GET /theme.css", s.handleThemeCSS)
	mux.HandleFunc("GET /api/hours", s.handleHours)
	mux.HandleFunc("GET /api/media", s.handleMediaPublic)
	mux.HandleFunc("GET /api/roles", s.handleRolesPublic)
	mux.HandleFunc("GET /api/groups", s.handleGroups)
	mux.HandleFunc("PUT /api/profile/pins", s.handlePinsPut)
	mux.HandleFunc("GET /api/commands", s.handleCommandsPublic)
	mux.HandleFunc("GET /api/gifs", s.handleGIFLibrary)
	mux.HandleFunc("GET /api/gifs/web", s.handleGIFWeb)
	mux.HandleFunc("GET /api/gifs/beacon", s.handleGIFBeacon)
	mux.HandleFunc("GET /api/gifs/web/preview/{id}", s.handleGIFPreview)
	mux.HandleFunc("GET /media/{kind}/{file}", s.handleMediaFile)
	mux.HandleFunc("GET /api/widgets/{id}", s.handleWidgetPublic)
	// Nutzerkonten (Modus „hosted“)
	ho := s.hostedOnly
	mux.HandleFunc("POST /api/account/prelogin", ho(s.handlePrelogin))
	mux.HandleFunc("POST /api/account/register", ho(s.handleRegister))
	mux.HandleFunc("POST /api/account/login", ho(s.handleAccountLogin))
	mux.HandleFunc("GET /api/account/verify", ho(s.handleVerify))
	mux.HandleFunc("POST /api/account/resend", ho(s.handleResend))
	mux.HandleFunc("POST /api/account/forgot", ho(s.handleForgot))
	mux.HandleFunc("GET /api/account/reset", ho(s.handleResetInfo))
	mux.HandleFunc("POST /api/account/reset", ho(s.handleReset))
	mux.HandleFunc("GET /api/account/invite", ho(s.handleInviteCheck))
	mux.HandleFunc("GET /api/account", ho(s.handleAccountGet))
	mux.HandleFunc("POST /api/account/password", ho(s.handleAccountPassword))
	mux.HandleFunc("POST /api/account/email", ho(s.handleAccountEmail))
	mux.HandleFunc("POST /api/account/2fa/begin", ho(s.handle2FABegin))
	mux.HandleFunc("POST /api/account/2fa/enable", ho(s.handle2FAEnable))
	mux.HandleFunc("POST /api/account/2fa/disable", ho(s.handle2FADisable))
	mux.HandleFunc("POST /api/account/recovery", ho(s.handleRecoveryNew))
	mux.HandleFunc("DELETE /api/account/sessions/{id}", ho(s.handleSessionEnd))
	mux.HandleFunc("POST /api/account/keys", ho(s.handleAccountKeys))
	mux.HandleFunc("POST /api/account/federation", ho(s.handleAccountFed))
	mux.HandleFunc("POST /api/account/delete", ho(s.handleMemberDelete))
	// Föderation
	mux.HandleFunc("GET "+fedWellKnown, s.handleWellKnown)
	mux.HandleFunc("GET "+fedsig.LegacyWellKnown, s.handleWellKnown) // früherer Name, für ältere Beacons und Instanzen
	mux.HandleFunc("GET /api/fed/users/{name}", s.handleFedUser)
	mux.HandleFunc("POST "+fedInboxPath, s.handleFedInbox)
	mux.HandleFunc("GET /api/fed/lookup", ho(s.handleFedLookup))
	mux.HandleFunc("GET /api/dms/{room}/key", ho(s.handleDMKey))
	mux.HandleFunc("GET /api/session", s.handleSessionGet)
	mux.HandleFunc("POST /api/session", s.handleSessionClaim)
	mux.HandleFunc("DELETE /api/session", s.handleSessionRelease)
	mux.HandleFunc("POST /api/unread", s.readGate(s.handleUnread))
	mux.HandleFunc("GET /api/rooms", s.readGate(s.handleRooms))
	mux.HandleFunc("GET /api/rooms/{room}/messages", s.readGate(s.handleHistory))
	mux.HandleFunc("POST /api/rooms/{room}/messages", s.handlePost)
	mux.HandleFunc("PATCH /api/rooms/{room}/messages/{id}", s.handleEditOwn)
	mux.HandleFunc("DELETE /api/rooms/{room}/messages/{id}", s.handleDeleteOwn)
	mux.HandleFunc("POST /api/rooms/{room}/messages/{id}/react", s.handleReact)
	mux.HandleFunc("POST /api/rooms/{room}/messages/{id}/pin", s.handlePin)
	mux.HandleFunc("POST /api/rooms/{room}/messages/{id}/vote", s.handleVote)
	mux.HandleFunc("GET /api/rooms/{room}/threads/{id}", s.readGate(s.handleThread))
	mux.HandleFunc("POST /api/rooms/{room}/messages/{id}/report", s.handleReport)
	mux.HandleFunc("DELETE /api/rooms/{room}/messages/{id}/mod", s.handleChatModDelete)
	mux.HandleFunc("POST /api/users/{id}/mute", s.handleChatMute)
	mux.HandleFunc("POST /api/rooms/{room}/upload", s.handleUpload)
	mux.HandleFunc("GET /api/profiles", s.readGate(s.handleProfiles))
	mux.HandleFunc("GET /api/dms", s.handleDMs)
	mux.HandleFunc("POST /api/dms", s.handleDMOpen)
	mux.HandleFunc("DELETE /api/dms/{room}", s.handleDMClear)
	mux.HandleFunc("GET /api/blocks", s.handleBlocks)
	mux.HandleFunc("POST /api/blocks", s.handleBlockSet)
	mux.HandleFunc("POST /api/profile/avatar", s.handleAvatarSet)
	mux.HandleFunc("DELETE /api/profile/avatar", s.handleAvatarDelete)
	mux.HandleFunc("PUT /api/profile/status", s.handleStatus)
	mux.HandleFunc("GET /api/rooms/{room}/search", s.readGate(s.handleSearch))
	mux.HandleFunc("GET /api/rooms/{room}/presence", s.readGate(s.handlePresence))
	mux.HandleFunc("GET /api/rooms/{room}/events", s.readGate(s.handleEvents))
	s.voiceRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	// Admin-Bereich (nur gleiche Origin, keine CORS-Freigabe).
	// mod = Moderatoren und Admins, adm = nur Admins.
	mux.HandleFunc("POST /api/admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /api/admin/logout", s.handleAdminLogout)
	mux.HandleFunc("GET /api/admin/me", mod(s.handleAdminMe))
	mux.HandleFunc("GET /api/admin/settings", mod(s.handleAdminSettingsGet))
	mux.HandleFunc("PUT /api/admin/settings", adm(s.handleAdminSettings))
	mux.HandleFunc("POST /api/admin/rooms", adm(s.handleAdminCreateRoom))
	mux.HandleFunc("PATCH /api/admin/rooms/{room}", adm(s.handleAdminUpdateRoom))
	mux.HandleFunc("DELETE /api/admin/rooms/{room}", adm(s.handleAdminDeleteRoom))
	mux.HandleFunc("GET /api/admin/rooms/{room}/messages", mod(s.handleAdminHistory))
	mux.HandleFunc("GET /api/admin/rooms", mod(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.hub.Rooms()) }))
	mux.HandleFunc("DELETE /api/admin/rooms/{room}/messages/{id}", mod(s.handleAdminDeleteMessage))
	mux.HandleFunc("POST /api/admin/rooms/{room}/messages/{id}/restore", mod(s.handleAdminRestoreMessage))
	mux.HandleFunc("POST /api/admin/rooms/{room}/messages/{id}/pin", mod(s.handleAdminPin))
	mux.HandleFunc("GET /api/admin/rooms/{room}/events", mod(s.handleAdminEvents)) // Token nur als Kopfzeile, nie in der Adresse
	mux.HandleFunc("GET /api/admin/rooms/{room}/pins", mod(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.hub.PinnedAll(NormRoom(r.PathValue("room")))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.HandleFunc("GET /api/admin/reports", mod(s.handleAdminReports))
	mux.HandleFunc("DELETE /api/admin/users/{id}/avatar", mod(s.handleAdminAvatarDelete))
	mux.HandleFunc("GET /api/admin/uploads", adm(s.handleAdminUploadsGet))
	mux.HandleFunc("PUT /api/admin/uploads", adm(s.handleAdminUploadsSet))
	mux.HandleFunc("GET /api/admin/schedules", adm(s.handleAdminSchedules))
	mux.HandleFunc("POST /api/admin/schedules", adm(s.handleAdminSchedulePut))
	mux.HandleFunc("PUT /api/admin/schedules/{id}", adm(s.handleAdminSchedulePut))
	mux.HandleFunc("DELETE /api/admin/schedules/{id}", adm(s.handleAdminScheduleDelete))
	mux.HandleFunc("POST /api/admin/schedules/{id}/run", adm(s.handleAdminScheduleRun))
	mux.HandleFunc("GET /api/admin/reports/count", mod(s.handleAdminReportCount))
	mux.HandleFunc("POST /api/admin/reports/{id}", mod(s.handleAdminReportClose))
	mux.HandleFunc("POST /api/admin/rooms/{room}/messages/{id}/confirm", mod(s.handleAdminConfirmMessage))
	mux.HandleFunc("POST /api/admin/rooms/{room}/messages/{id}/purge", adm(s.handleAdminPurgeMessage))
	mux.HandleFunc("GET /api/admin/search", mod(s.handleAdminSearch))
	mux.HandleFunc("GET /api/admin/automod", mod(s.handleAutomodGet))
	mux.HandleFunc("PUT /api/admin/automod", mod(s.handleAutomodSet))
	mux.HandleFunc("GET /api/admin/rooms/{room}/automod", mod(s.handleRoomAutomodGet))
	mux.HandleFunc("PUT /api/admin/rooms/{room}/automod", mod(s.handleRoomAutomodSet))
	mux.HandleFunc("POST /api/admin/automod/test", mod(s.handleAutomodTest))
	mux.HandleFunc("GET /api/admin/automod/queue", mod(s.handleAutomodQueue))
	mux.HandleFunc("GET /api/admin/filter", mod(s.handleFilterGet))
	mux.HandleFunc("PUT /api/admin/filter", mod(s.handleFilterSet))
	mux.HandleFunc("GET /api/admin/users", mod(s.handleAdminUsers))
	mux.HandleFunc("GET /api/admin/users/{id}", mod(s.handleAdminUser))
	mux.HandleFunc("POST /api/admin/users/{id}/sanction", mod(s.handleAdminSanction))
	mux.HandleFunc("DELETE /api/admin/users/{id}/sanction", mod(s.handleAdminLift))
	mux.HandleFunc("POST /api/admin/users/{id}/team", adm(s.handleAdminTeam))
	mux.HandleFunc("POST /api/admin/users/{id}/roles", adm(s.handleAdminUserRoles))
	mux.HandleFunc("GET /api/admin/roles", mod(s.handleAdminRoles))
	mux.HandleFunc("PUT /api/admin/roles", adm(s.handleAdminRolesSet))
	mux.HandleFunc("POST /api/admin/users/{id}/purge", adm(s.handleAdminPurgeUser))
	mux.HandleFunc("GET /api/admin/sanctions", mod(s.handleAdminSanctions))
	mux.HandleFunc("GET /api/admin/protected-names", mod(s.handleProtectedGet))
	mux.HandleFunc("PUT /api/admin/protected-names", adm(s.handleProtectedSet))
	mux.HandleFunc("GET /api/admin/audit", mod(s.handleAudit))
	mux.HandleFunc("GET /api/admin/stats", mod(s.handleStats))
	mux.HandleFunc("GET /api/admin/accounts", adm(s.handleAccounts))
	mux.HandleFunc("POST /api/admin/accounts", adm(s.handleAccountCreate))
	mux.HandleFunc("PATCH /api/admin/accounts/{name}", adm(s.handleAccountUpdate))
	mux.HandleFunc("DELETE /api/admin/accounts/{name}", adm(s.handleAccountDelete))
	mux.HandleFunc("GET /api/admin/hosted", adm(s.handleAdminHostedGet))
	mux.HandleFunc("PUT /api/admin/hosted", adm(ho(s.handleAdminHostedPut)))
	mux.HandleFunc("POST /api/admin/hosted/testmail", adm(ho(s.handleAdminTestMail)))
	mux.HandleFunc("POST /api/admin/invites", adm(ho(s.handleAdminInvite)))
	mux.HandleFunc("POST /api/admin/users/{id}/account", adm(ho(s.handleAdminMember)))
	mux.HandleFunc("GET /api/admin/layout", mod(s.handleLayoutGet))
	mux.HandleFunc("PUT /api/admin/layout", adm(s.handleLayoutPut))
	mux.HandleFunc("GET /api/admin/beacon", adm(s.handleBeaconGet))
	mux.HandleFunc("PUT /api/admin/beacon", adm(s.handleBeaconPut))
	mux.HandleFunc("POST /api/admin/beacon/sync", adm(s.handleBeaconSync))
	mux.HandleFunc("GET /api/admin/beacon/collections", adm(s.handleBeaconCollections))
	mux.HandleFunc("PUT /api/admin/beacon/subscriptions", adm(s.handleBeaconSubs))
	mux.HandleFunc("GET /api/admin/template", adm(s.handleTemplateExport))
	mux.HandleFunc("POST /api/admin/template/inspect", adm(s.handleTemplateInspect))
	mux.HandleFunc("POST /api/admin/template/apply", adm(s.handleTemplateApply))
	mux.HandleFunc("GET /api/admin/federation", adm(s.handleAdminFed))
	mux.HandleFunc("DELETE /api/admin/federation/peers/{domain}", adm(s.handleAdminFedForget))
	mux.HandleFunc("GET /api/admin/backups", adm(s.handleBackups))
	mux.HandleFunc("POST /api/admin/backups", adm(s.handleBackupCreate))
	mux.HandleFunc("GET /api/admin/backups/{name}", adm(s.handleBackupDownload))
	mux.HandleFunc("GET /api/admin/media", adm(s.handleAdminMedia))
	mux.HandleFunc("PUT /api/admin/media/settings", adm(s.handleAdminMediaSettings))
	mux.HandleFunc("PUT /api/admin/reactions", adm(s.handleAdminReactions))
	mux.HandleFunc("POST /api/admin/emotes", adm(s.handleAdminEmoteAdd))
	mux.HandleFunc("PATCH /api/admin/emotes/{name}", adm(s.handleAdminEmoteUpdate))
	mux.HandleFunc("DELETE /api/admin/emotes/{name}", adm(s.handleAdminEmoteDelete))
	mux.HandleFunc("POST /api/admin/gifs", adm(s.handleAdminGIFAdd))
	mux.HandleFunc("PATCH /api/admin/gifs/{id}", adm(s.handleAdminGIFUpdate))
	mux.HandleFunc("DELETE /api/admin/gifs/{id}", adm(s.handleAdminGIFDelete))
	mux.HandleFunc("GET /api/admin/commands", adm(s.handleAdminCommands))
	mux.HandleFunc("PUT /api/admin/commands", adm(s.handleAdminCommandsSet))
	mux.HandleFunc("POST /api/admin/commands/test", adm(s.handleAdminCommandsTest))
	mux.HandleFunc("GET /api/admin/widgets", adm(s.handleWidgets))
	mux.HandleFunc("POST /api/admin/widgets", adm(s.handleWidgetCreate))
	mux.HandleFunc("PUT /api/admin/widgets/{id}", adm(s.handleWidgetUpdate))
	mux.HandleFunc("DELETE /api/admin/widgets/{id}", adm(s.handleWidgetDelete))
	mux.HandleFunc("GET /api/admin/bots", adm(s.handleAdminBots))
	mux.HandleFunc("POST /api/admin/bots", adm(s.handleAdminCreateBot))
	mux.HandleFunc("PATCH /api/admin/bots/{id}", adm(s.handleAdminUpdateBot))
	mux.HandleFunc("DELETE /api/admin/bots/{id}", adm(s.handleAdminDeleteBot))
	mux.HandleFunc("POST /api/admin/bots/{id}/renew", adm(s.handleAdminRenewBot))
	mux.HandleFunc("POST /api/admin/bots/{id}/test", adm(s.handleAdminTestBot))

	// Bot-API (nur für Programme – keine CORS-Freigabe)
	mux.HandleFunc("GET /api/bot/me", s.requireBot(s.handleBotMe))
	mux.HandleFunc("GET /api/bot/rooms", s.requireBot(s.handleBotRooms))
	mux.HandleFunc("GET /api/bot/rooms/{room}/messages", s.requireBot(s.handleBotHistory))
	mux.HandleFunc("POST /api/bot/rooms/{room}/messages", s.requireBot(s.handleBotPost))
	mux.HandleFunc("PATCH /api/bot/rooms/{room}/messages/{id}", s.requireBot(s.handleBotEdit))
	mux.HandleFunc("DELETE /api/bot/rooms/{room}/messages/{id}", s.requireBot(s.handleBotDelete))
	mux.HandleFunc("POST /api/bot/rooms/{room}/messages/{id}/react", s.requireBot(s.handleBotReact))
	mux.HandleFunc("GET /api/bot/rooms/{room}/presence", s.requireBot(s.handleBotPresence))
	mux.HandleFunc("POST /api/bot/users/{id}/mute", s.requireBot(s.handleBotMute))
	mux.HandleFunc("POST /api/bot/users/{id}/ban", s.requireBot(s.handleBotBan))
	mux.HandleFunc("DELETE /api/bot/users/{id}/sanction", s.requireBot(s.handleBotLift))
	mux.HandleFunc("GET /api/bot/events", s.requireBot(s.handleBotEvents))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		fileServer.Serve(w, r, "admin.html")
	})
	mux.HandleFunc("GET /admin.html", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	})

	mux.Handle("GET /", fileServer)

	s.i18nAPI().Register(mux, adm, nil) // Texte der Oberfläche und Sprach-Editor
	mux.HandleFunc("GET /api/admin/i18n/beacon", adm(s.handleI18nBeaconList))
	mux.HandleFunc("POST /api/admin/i18n/beacon/install", adm(s.handleI18nBeaconInstall))
	return s.withLang(s.withCORS(s.withHeaders(mux)))
}

// originAllowed: Darf eine fremde Webseite (Origin) diesen Pfad abrufen?
//   - ALLOWED_ORIGINS enthält die Origin (oder "*")            → ja
//   - Einstellung „Einbetten: überall“ und kein ALLOWED_ORIGINS → ja
//   - eine Domain aus einem gespeicherten Widget passt           → ja (bei Kanal-Pfaden nur
//     für Widgets dieses Kanals)
func (s *Server) originAllowed(origin, path string) bool {
	for _, o := range s.cfg.AllowedOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	if len(s.cfg.AllowedOrigins) == 0 && s.hub.Settings().EmbedPolicy != EmbedListed {
		return true
	}
	room := ""
	if rest, ok := strings.CutPrefix(path, "/api/rooms/"); ok {
		room = NormRoom(strings.SplitN(rest, "/", 2)[0])
	}
	return s.hub.Widgets().OriginAllowed(origin, room)
}

// withCORS erlaubt Zugriffe vom Embed-Widget auf fremden Webseiten.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		isAPI := strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/admin/") && !strings.HasPrefix(r.URL.Path, "/api/bot/")
		if origin != "" && isAPI && s.originAllowed(origin, r.URL.Path) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Lang")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.Header().Set("Access-Control-Expose-Headers", "X-Has-More")
		}
		if isAPI {
			w.Header().Add("Vary", "Origin")
		}
		if strings.HasPrefix(r.URL.Path, "/embed/") {
			// Widget-Dateien dürfen von überall geladen werden.
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		mic := "()"
		if s.voice != nil {
			mic = "(self)" // Sprachkanäle
		}
		hd.Set("Permissions-Policy", "camera=(), microphone="+mic+", geolocation=(), payment=(), usb=()")
		// Content Security Policy: nur eigene Skripte (keine eingebetteten), damit eine
		// eingeschleuste Zeichenkette nie als Code läuft. Bilder auch von https (GIF-Quellen).
		// Die Beispielseite /demo.html enthält absichtlich Inline-Code und ist ausgenommen.
		if r.URL.Path != "/demo.html" {
			hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob: https:; media-src 'self' blob: https:; font-src 'self' data:; connect-src 'self' "+s.voiceConnectSrc(r)+"; "+
				"frame-src 'self'; worker-src 'self' blob:; object-src 'none'; base-uri 'self'; form-action 'self'")
		}
		if strings.HasPrefix(s.cfg.PublicURL, "https://") {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP: Adresse der anfragenden Person (Weiterleitungs-Kopfzeilen nur von vertrauenswürdigen
// Proxys, siehe internal/clientip).
func (s *Server) clientIP(r *http.Request) string { return s.ipr.IP(r) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	v = translatePayload(w, v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func statusFor(err error) int {
	var se *SanctionError
	var sm *SlowModeError
	switch {
	case errors.As(err, &se):
		return http.StatusForbidden
	case errors.As(err, &sm), errors.Is(err, ErrRepeat), errors.Is(err, errBotRate):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrTooMany), errors.Is(err, ErrTooManyUsers), errors.Is(err, ErrBusy):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrMediaTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrGifsOff), errors.Is(err, ErrNoWebSearch):
		return http.StatusForbidden
	case errors.Is(err, ErrWebGIF):
		return http.StatusBadGateway
	case errors.Is(err, ErrNameTaken), errors.Is(err, ErrRoomExists), errors.Is(err, ErrNameProtected), errors.Is(err, ErrEmoteExists),
		errors.Is(err, ErrAccountExists), errors.Is(err, ErrReported):
		return http.StatusConflict
	case errors.Is(err, ErrBotRoom), errors.Is(err, ErrLinksKnown), errors.Is(err, ErrLinksTeam),
		errors.Is(err, ErrNotOwn), errors.Is(err, ErrEditWindow), errors.Is(err, ErrSuperAccount),
		errors.Is(err, ErrLastAdmin), errors.Is(err, errBotNoMod), errors.Is(err, errBotNoPerm),
		errors.Is(err, errBotNoWrite), errors.Is(err, errBotOwnSanct), errors.Is(err, errBotOutOfReach),
		errors.Is(err, ErrBotLinks), errors.Is(err, ErrReadOnly), errors.Is(err, ErrNoPerm), errors.Is(err, ErrReportOwn),
		errors.Is(err, ErrUploadsOff), errors.Is(err, ErrUploadKnown), errors.Is(err, ErrUploadRole), errors.Is(err, ErrAvatarsOff), errors.Is(err, ErrDMsOff),
		errors.Is(err, ErrDMBlocked):
		return http.StatusForbidden
	case errors.Is(err, ErrNoRoom), errors.Is(err, ErrNoMessage), errors.Is(err, ErrNoBot),
		errors.Is(err, ErrNoReplyMsg), errors.Is(err, ErrNoUser), errors.Is(err, ErrNoSanction),
		errors.Is(err, ErrNoAccount), errors.Is(err, ErrNoWidget), errors.Is(err, ErrNoBackup),
		errors.Is(err, ErrNoEmote), errors.Is(err, ErrNoGIF), errors.Is(err, ErrNoReport), errors.Is(err, ErrNoSchedule):
		return http.StatusNotFound
	case errors.Is(err, ErrNoSession):
		return http.StatusUnauthorized
	default:
		return http.StatusBadRequest
	}
}

func main() {
	cfg := loadConfig()
	hub := NewHub(cfg.MaxRooms)
	users := NewUsers(cfg.NameTTL, cfg.MaxUsers)
	var store Backend
	if cfg.DataDir == "" && cfg.Storage != "" && cfg.Storage != "files" {
		log.Fatalf("STORAGE=%s braucht einen Datenordner (für hochgeladene Bilder)", cfg.Storage)
	}
	if cfg.DataDir != "" {
		hub.Media().SetDir(filepath.Join(cfg.DataDir, "media"))
		var err error
		if store, err = OpenBackend(cfg.Storage, cfg.DataDir, cfg.DBDSN); err != nil {
			log.Fatal(err)
		}
		start := time.Now()
		if err := store.Load(hub, users); err != nil {
			log.Fatalf("Daten konnten nicht geladen werden: %v", err)
		}
		total := 0
		rooms := hub.Rooms()
		for _, r := range rooms {
			total += r.Messages
		}
		where := cfg.DataDir
		if store.Kind() != "files" {
			where = store.Kind() + "-Datenbank"
		}
		log.Printf("Daten aus %s geladen: %d Kanäle, %d Nachrichten (%v)", where, len(rooms), total, time.Since(start).Round(time.Millisecond))
		hub.EnablePersistence()
		hub.touchDoc("config.json") // config.json und automod.json immer vorhanden
		hub.touchDoc("automod.json")
		if cfg.Mode == ModeHosted {
			hub.touchDoc("hosted.json") // internes Geheimnis sofort sichern
		}
		if cfg.PublicURL != "" {
			hub.touchDoc(fedStatePath) // Serverschlüssel sichern (Föderation, Beacon)
		}
	} else {
		log.Printf("Achtung: Speicherung ist aus (-data none). Alles geht beim Neustart verloren.")
	}
	hub.EnsureDefaultRoom()
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = randomPassword()
		log.Printf("Kein ADMIN_PASSWORD gesetzt. Temporäres Admin-Passwort (gilt bis zum Neustart): %s", cfg.AdminPassword)
	} else if len(cfg.AdminPassword) < 10 {
		log.Printf("Warnung: Das Admin-Passwort ist kürzer als 10 Zeichen.")
	}
	srv := NewServer(cfg, hub, users)
	if cfg.Mode == ModeHosted {
		log.Printf("Modus: hosted (Nutzerkonten, 2FA, verschlüsselte Direktnachrichten). Öffentliche Adresse: %s", srv.baseURL())
		if cfg.PublicURL == "" {
			log.Printf("Hinweis: PUBLIC_URL ist nicht gesetzt – Links in E-Mails zeigen auf %s, Föderation ist aus.", srv.baseURL())
		}
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // ganze Anfrage inkl. Upload (langsame Clients halten sonst ewig)
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Änderungen zeitnah speichern (gebündelt, spätestens nach ~0,5 s).
	sv := newSaver(store, hub, users, 500*time.Millisecond)
	saverDone := make(chan struct{})
	go func() { sv.run(ctx); close(saverDone) }()
	go srv.webhooks.run(ctx) // Nachrichten an Bots mit Webhook verteilen
	if cfg.Mode == ModeHosted {
		go srv.runFed(ctx) // Föderation: Warteschlange abarbeiten
	}
	go srv.runBeacon(ctx) // Verzeichnis: regelmäßig beim Beacon melden
	sv.notify()           // Startzustand sofort sichern (z. B. neu angelegte Lobby)
	if srv.backups != nil {
		srv.backups.store, srv.backups.flush = store, sv.save
		if store != nil && store.Kind() != "files" {
			srv.backups.export = func(dir string) error { return ExportFiles(dir, hub, users) }
		}
	}

	// Hintergrundaufgaben: abgelaufene Namen und Sperren freigeben, Rate-Limiter aufräumen,
	// Aufbewahrungsfrist anwenden (stündlich), Backups (täglich um 3 Uhr).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		var lastRetention time.Time
		run := func() {
			users.Expire()
			hub.Mod().Cleanup()
			srv.rl.cleanup()
			srv.claimRL.cleanup()
			srv.loginRL.cleanup()
			srv.botRL.cleanup()
			srv.gifRL.cleanup()
			srv.previewRL.cleanup()
			srv.reportRL.cleanup()
			srv.uploadRL.cleanup()
			srv.dmRL.cleanup()
			srv.acctRL.cleanup()
			srv.mailRL.cleanup()
			srv.fedRL.cleanup()
			if time.Since(lastRetention) >= time.Hour {
				lastRetention = time.Now()
				srv.applyRetention()
				srv.cleanupMedia()
			}
			srv.runSchedules(time.Now())
			if srv.backups != nil {
				srv.backups.Tick(time.Now().In(hub.Location()), func() { sv.save() })
			}
		}
		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	}()

	go func() {
		log.Printf("UCNG %s läuft auf http://localhost%s  – Admin-Bereich: /admin", version, displayAddr(cfg.Addr))
		if cfg.SourceURL == "" {
			log.Printf("Hinweis: SOURCE_URL ist nicht gesetzt. Wer eine geänderte Fassung öffentlich betreibt, muss nach der AGPL den Quellcode anbieten – SOURCE_URL auf das eigene Repository setzen.")
		}
		if srv.backups != nil {
			log.Printf("Backups: täglich um 3 Uhr nach %s (die letzten %d bleiben erhalten)", cfg.BackupDir, cfg.BackupKeep)
		}
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("Fahre herunter …")
	close(srv.done)
	if srv.voice != nil {
		srv.voice.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
	<-saverDone // letzter Speichervorgang
	log.Println("Daten gespeichert. Bis bald!")
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return ":" + port
	}
	return addr
}

// cleanSourceURL: nur vollständige http(s)-Adressen, sonst leer.
func cleanSourceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(raw) > 300 {
		log.Printf("SOURCE_URL %q ist keine gültige Adresse – wird ignoriert", raw)
		return ""
	}
	return raw
}

// AboutInfo: Name, Version, Lizenz und Quellcode – für die Oberfläche und andere Instanzen.
type AboutInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	License string `json:"license"`
	Source  string `json:"source,omitempty"`
}

func (s *Server) about() AboutInfo {
	return AboutInfo{Name: "UCNG", Version: version, License: licenseID, Source: s.cfg.SourceURL}
}
