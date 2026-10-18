// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Musik-Bot für UCNG: spielt MP3s (und andere Formate) aus einem Ordner oder YouTube-Videos bzw.
// -Playlists in einen Sprachkanal. Gesteuert wird im Chat:
//
//	!play <YouTube-Link | Suchbegriff>   Video, Playlist oder Suche in die Warteschlange
//	!folder [Unterordner]                alle Dateien aus dem Musik-Ordner (zufällig gemischt)
//	!songs [Begriff]                     Unterordner bzw. passende Dateien anzeigen
//	!skip  !stop  !queue  !np  !vol 25  !shuffle  !loop  !clear  !cache  !music
//
// Mit CACHE_DIR legt der Bot YouTube-Stücke dauerhaft ab (cache.go) – beim nächsten Mal ohne
// Anfrage bei YouTube. Mit YOUTUBE=0 gibt es gar kein YouTube: !play <Begriff> sucht dann im
// Musik-Ordner (z. B. für lizenzfreie Musik).
//
// Der Bot selbst braucht nur die Go-Standardbibliothek; zum Umwandeln ruft er ffmpeg auf, für
// YouTube yt-dlp (beides im Docker-Image, siehe Dockerfile). Die Musik geht im Streaming-Modus an
// den Chat-Server (POST /api/bot/voice/{room}/play?stream=1) – sie startet sofort, ohne
// Längengrenze; die Anfrage endet, wenn das Stück zu Ende ist.
//
//	UCNG_URL=http://ucng:8080 UCNG_BOT_TOKEN=gcb_… MUSIC_DIR=/music go run ./examples/bot-music
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	base     = strings.TrimRight(env("UCNG_URL", "http://localhost:8080"), "/")
	token    = os.Getenv("UCNG_BOT_TOKEN")
	musicDir = env("MUSIC_DIR", "/music")
	fixRoom  = env("MUSIC_ROOM", "") // leer = der Kanal, in dem der Befehl geschrieben wurde
	prefix   = env("PREFIX", "!")
	teamOnly = env("TEAM_ONLY", "") == "1" // nur das Team darf steuern
	volume   = atoiEnv("VOLUME", 20)       // Voreinstellung für alle (1–100)
	kbps     = atoiEnv("BITRATE", 128)     // Opus-Bitrate
	maxQueue = atoiEnv("MAX_QUEUE", 500)
	tmpDir   = env("TMP_DIR", os.TempDir())
	// YOUTUBE=0: keine Links und keine YouTube-Suche – nur Musik aus dem Musik-Ordner (z. B.
	// lizenzfreie Stücke). !play <Begriff> sucht dann in den Dateinamen.
	youtubeOn = env("YOUTUBE", "1") != "0"
	client    = &http.Client{} // ohne Zeitlimit: ein Stück dauert so lange, wie es dauert
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func atoiEnv(k string, d int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return n
	}
	return d
}

// item: ein Stück in der Warteschlange.
type item struct {
	Title  string
	File   string // lokale Datei
	URL    string // YouTube (oder andere von yt-dlp unterstützte Seite)
	Key    string // Schlüssel im Zwischenspeicher (z. B. youtube-<Video-ID>)
	Live   bool   // Livestream (läuft, bis !skip oder !stop)
	By     string
	Credit string // „ – Künstler · Lizenz“ aus den Tags der Datei (beim Abspielen gelesen)
}

// player: Warteschlange und Wiedergabe eines Sprachkanals.
type player struct {
	room   string // Sprachkanal
	chat   string // Kanal für Meldungen
	mu     sync.Mutex
	queue  []item
	cur    *item
	cancel context.CancelFunc
	loop   bool
	vol    int
	wake   chan struct{}
}

var (
	playersMu sync.Mutex
	players   = map[string]*player{}
)

func playerFor(room, chat string) *player {
	playersMu.Lock()
	defer playersMu.Unlock()
	p := players[room]
	if p == nil {
		p = &player{room: room, vol: volume, wake: make(chan struct{}, 1)}
		players[room] = p
		go p.run()
	}
	p.mu.Lock()
	p.chat = chat
	p.mu.Unlock()
	return p
}

func (p *player) add(items ...item) int {
	p.mu.Lock()
	n := min(len(items), maxQueue-len(p.queue))
	if n > 0 {
		p.queue = append(p.queue, items[:n]...)
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return max(n, 0)
}

// run spielt die Warteschlange ab – ein Stück nach dem anderen.
func (p *player) run() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.cur = nil
			p.mu.Unlock()
			<-p.wake
			continue
		}
		it := p.queue[0]
		p.queue = p.queue[1:]
		if p.loop {
			p.queue = append(p.queue, it)
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.cur, p.cancel = &it, cancel
		vol, chat := p.vol, p.chat
		p.mu.Unlock()
		if it.File != "" {
			it.Credit = credit(ctx, it.File, it.Title) // Künstler und Lizenz aus den Tags (Namensnennung bei CC BY)
			p.mu.Lock()
			if p.cur != nil {
				p.cur.Credit = it.Credit
			}
			p.mu.Unlock()
		}
		announce := func() { say(chat, fmt.Sprintf("▶️ **%s**%s%s%s", it.Title, it.Credit, liveTag(it), byLine(it))) }
		if it.URL != "" && it.Key != "" {
			if path, _ := cacheGet(it.Key); path != "" {
				it.URL = "" // aus dem Zwischenspeicher: sofort da
			}
		}
		if it.URL != "" {
			say(chat, fmt.Sprintf("⏳ Lade **%s** …", it.Title)) // ▶️ folgt, sobald Ton fließt
		} else {
			announce()
		}
		err := play(ctx, p.room, it, vol, announce)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			say(chat, "⚠️ "+it.Title+": "+err.Error()+ytHint(err))
			time.Sleep(time.Second)
		}
	}
}

func liveTag(it item) string {
	if it.Live {
		return " 🔴 live"
	}
	return ""
}

func byLine(it item) string {
	if it.By == "" {
		return ""
	}
	return " (gewünscht von " + it.By + ")"
}

func (p *player) skip() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil || p.cur == nil {
		return false
	}
	stopRemote(p.room) // sofort still (sonst spielt der Server noch, was schon unterwegs ist)
	p.cancel()
	return true
}

func (p *player) stop() {
	p.mu.Lock()
	p.queue = nil
	p.loop = false
	if p.cancel != nil {
		stopRemote(p.room)
		p.cancel()
	}
	p.mu.Unlock()
}

func stopRemote(room string) {
	req, _ := http.NewRequest("POST", base+"/api/bot/voice/"+url.PathEscape(room)+"/stop", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
	}
}

// --- Abspielen ---

// play wandelt ein Stück mit ffmpeg in Ogg-Opus um und streamt es zum Chat-Server. started:
// sobald die Wiedergabe wirklich beginnt.
func play(ctx context.Context, room string, it item, vol int, started func()) error {
	if path, _ := cacheGet(it.Key); path != "" { // schon da: ohne YouTube und ohne ffmpeg
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			cacheTouch(it.Key)
			return stream(ctx, room, it, vol, f, nil)
		}
	}
	if it.URL != "" {
		return playURL(ctx, room, it, vol, started)
	}
	ff := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs(it.File)...)
	out, err := ff.StdoutPipe()
	if err != nil {
		return err
	}
	var errBuf bytes.Buffer
	ff.Stderr = &errBuf
	if err := ff.Start(); err != nil {
		return fmt.Errorf("ffmpeg fehlt? %w", err)
	}
	defer ff.Wait()
	return stream(ctx, room, it, vol, out, func() {
		ff.Process.Kill()
		if errBuf.Len() > 0 {
			log.Printf("ffmpeg: %s", strings.TrimSpace(errBuf.String()))
		}
	})
}

// stream schickt Ogg-Opus im Streaming-Modus zum Chat-Server; die Anfrage endet mit dem Stück.
// abort (optional) beendet die Quelle, wenn der Server ablehnt.
func stream(ctx context.Context, room string, it item, vol int, body io.Reader, abort func()) error {
	if abort == nil {
		abort = func() {}
	}
	q := url.Values{"stream": {"1"}, "volume": {strconv.Itoa(vol)}, "title": {it.Title}, "kbps": {strconv.Itoa(kbps)}}
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/bot/voice/"+url.PathEscape(room)+"/play?"+q.Encode(), io.NopCloser(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "audio/ogg")
	res, err := client.Do(req)
	if err != nil {
		abort()
		if ctx.Err() != nil {
			return context.Canceled
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		abort()
		var e struct{ Error string }
		json.NewDecoder(res.Body).Decode(&e)
		return fmt.Errorf("%d: %s", res.StatusCode, e.Error)
	}
	var r struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(res.Body).Decode(&r)
	switch r.Reason {
	case "stopped": // vom Chat aus angehalten (oder ein anderes Stück gestartet)
		return context.Canceled
	case "gone":
		if ctx.Err() != nil {
			return context.Canceled
		}
		return errors.New("Verbindung zum Sprachkanal verloren (Sprach-Server neu gestartet?)")
	case "error":
		return errors.New("Wiedergabe abgebrochen (fehlerhafter Datenstrom)")
	}
	return nil
}

func firstLine(s string, err error) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return s
}

// resolve: YouTube-Link, Playlist oder Suche → Stücke (yt-dlp --flat-playlist -J).
func resolve(ctx context.Context, q, by string) ([]item, error) {
	target, search := q, !strings.HasPrefix(q, "http://") && !strings.HasPrefix(q, "https://")
	// Schon im Zwischenspeicher? Dann gar nicht erst bei YouTube fragen.
	key := cacheSearch(q)
	if !search {
		key = ""
		if u, err := url.Parse(q); err == nil && u.Query().Get("list") == "" {
			key = youtubeKey(q)
		}
	}
	if _, e := cacheGet(key); e != nil {
		return []item{{Title: e.Title, URL: e.URL, Key: key, By: by}}, nil
	}
	if search {
		target = "ytsearch1:" + q
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := append([]string{"--quiet", "--no-warnings", "--flat-playlist", "-J"}, ytdlpArgs...)
	cmd := exec.CommandContext(ctx, "yt-dlp", append(args, "--", target)...)
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("YouTube antwortet nicht (60 s)")
		}
		why := stderr.reason()
		if why == "" {
			why = err.Error()
		}
		return nil, errors.New("YouTube: " + why)
	}
	var info struct {
		ID           string `json:"id"`
		ExtractorKey string `json:"extractor_key"`
		Title        string `json:"title"`
		LiveStatus   string `json:"live_status"` // is_live, was_live, not_live, is_upcoming …
		IsLive       bool   `json:"is_live"`
		WebpageURL   string `json:"webpage_url"`
		OriginalURL  string `json:"original_url"`
		Entries      []struct {
			Title      string `json:"title"`
			URL        string `json:"url"`
			ID         string `json:"id"`
			IEKey      string `json:"ie_key"`
			LiveStatus string `json:"live_status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, err
	}
	var items []item
	if len(info.Entries) == 0 {
		u := info.WebpageURL
		if u == "" {
			u = q
		}
		if info.LiveStatus == "is_upcoming" {
			return nil, errors.New("der Livestream hat noch nicht begonnen")
		}
		live := info.IsLive || info.LiveStatus == "is_live"
		key := keyOf(info.ExtractorKey, info.ID)
		if live {
			key = "" // Livestreams nicht zwischenspeichern
		}
		return []item{{Title: orDefault(info.Title, u), URL: u, Key: key, Live: live, By: by}}, nil
	}
	for _, e := range info.Entries {
		u := e.URL
		if !strings.HasPrefix(u, "http") && e.ID != "" {
			u = "https://www.youtube.com/watch?v=" + e.ID
		}
		if u != "" {
			k := keyOf(orDefault(e.IEKey, "youtube"), e.ID)
			if c, ce := cacheGet(k); c != "" { // Titel aus dem Zwischenspeicher
				e.Title = orDefault(e.Title, ce.Title)
			}
			live := e.LiveStatus == "is_live"
			if live {
				k = ""
			}
			items = append(items, item{Title: orDefault(e.Title, u), URL: u, Key: k, Live: live, By: by})
		}
	}
	if search && len(items) > 0 {
		cacheRememberSearch(q, items[0].Key)
	}
	return items, nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

var audioExt = map[string]bool{".mp3": true, ".ogg": true, ".opus": true, ".flac": true, ".m4a": true, ".aac": true, ".wav": true}

var idSuffix = regexp.MustCompile(` \[[A-Za-z0-9_-]{6,}\]$`)

// folder: alle Musikdateien eines (Unter-)Ordners, zufällig gemischt.
func folder(sub, by string) ([]item, error) {
	root := filepath.Clean(musicDir)
	dir := filepath.Join(root, filepath.Clean("/"+sub)) // kein Ausbruch aus dem Ordner
	var items []item
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != dir && (strings.HasPrefix(d.Name(), ".") || // z. B. .tmp des Zwischenspeichers
			!youtubeOn && isCacheDir(path)) { // YouTube aus: alte Downloads nicht spielen
			return filepath.SkipDir
		}
		if !d.IsDir() && audioExt[strings.ToLower(filepath.Ext(path))] {
			name := idSuffix.ReplaceAllString(strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())), "") // „Titel [ID]“ → „Titel“
			items = append(items, item{Title: name, File: path, By: by})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	return items, nil
}

// --- Befehle ---

type message struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Bot  bool   `json:"bot"`
	Team bool   `json:"team"`
}

type event struct {
	Event   string   `json:"event"`
	Room    string   `json:"room"`
	Message *message `json:"message"`
	Bot     *struct {
		Name string `json:"name"`
	} `json:"bot"`
}

func command(chat string, m *message) {
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, prefix) {
		return
	}
	fields := strings.Fields(strings.TrimPrefix(text, prefix))
	if len(fields) == 0 {
		return
	}
	cmd, arg := strings.ToLower(fields[0]), strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, prefix), fields[0]))
	known := map[string]bool{"play": true, "p": true, "folder": true, "ordner": true, "skip": true, "next": true, "stop": true, "queue": true, "q": true,
		"np": true, "vol": true, "volume": true, "shuffle": true, "loop": true, "clear": true, "music": true, "musik": true, "cache": true,
		"songs": true, "list": true, "lieder": true}
	if !known[cmd] {
		return
	}
	if teamOnly && !m.Team {
		say(chat, "🎵 Nur das Team darf die Musik steuern.")
		return
	}
	room := fixRoom
	if room == "" {
		room = chat
	}
	p := playerFor(room, chat)
	switch cmd {
	case "music", "musik":
		rest := prefix + "folder [Unterordner] · " + prefix + "songs [Begriff] · " + prefix + "skip · " + prefix + "stop · " + prefix + "queue · " + prefix + "np · " + prefix + "vol 1–100 · " + prefix + "shuffle · " + prefix + "loop · " + prefix + "clear"
		if youtubeOn {
			say(chat, "🎵 "+prefix+"play <YouTube-Link/Playlist/Suche> · "+rest+" · "+prefix+"cache")
		} else {
			say(chat, "🎵 "+prefix+"play [Begriff] (sucht im Musik-Ordner) · "+rest+" – YouTube und Links sind abgeschaltet.")
		}
	case "play", "p":
		if arg == "" {
			items, err := folder("", m.Name)
			if err != nil || len(items) == 0 {
				say(chat, "🎵 Im Musik-Ordner liegt nichts.")
				return
			}
			say(chat, fmt.Sprintf("🎵 %d Stücke aus dem Ordner in der Warteschlange.", p.add(items...)))
			return
		}
		if !youtubeOn {
			if strings.Contains(arg, "://") {
				say(chat, "🚫 Links sind abgeschaltet – hier läuft nur Musik aus dem Musik-Ordner. Suchen: "+prefix+"play <Begriff>, anzeigen: "+prefix+"songs")
				return
			}
			items := findLocal(arg, m.Name)
			switch {
			case len(items) == 0:
				say(chat, "🎵 Im Musik-Ordner passt nichts zu „"+arg+"“ ("+prefix+"songs zeigt, was da ist).")
				return
			case len(items) == 1:
				say(chat, "➕ "+items[0].Title)
			default:
				say(chat, fmt.Sprintf("➕ %d Stücke zu „%s“", min(len(items), maxQueue), arg))
			}
			p.add(items...)
			return
		}
		go func() {
			items, err := resolve(context.Background(), arg, m.Name)
			if err != nil || len(items) == 0 {
				say(chat, "⚠️ Nichts gefunden: "+errText(err)+ytHint(err))
				return
			}
			p.mu.Lock()
			free := maxQueue - len(p.queue)
			p.mu.Unlock()
			switch n := min(len(items), free); { // erst melden, dann anstellen (Reihenfolge im Chat)
			case n <= 0:
				say(chat, "⚠️ Die Warteschlange ist voll.")
				return
			case n == 1:
				say(chat, "➕ "+items[0].Title+liveTag(items[0]))
			default:
				say(chat, fmt.Sprintf("➕ %d Stücke", n))
			}
			p.add(items...)
		}()
	case "folder", "ordner":
		items, err := folder(arg, m.Name)
		if err != nil || len(items) == 0 {
			say(chat, "🎵 Dort liegt keine Musik.")
			return
		}
		say(chat, fmt.Sprintf("🎵 %d Stücke in der Warteschlange.", p.add(items...)))
	case "songs", "list", "lieder":
		say(chat, songList(arg))
	case "cache":
		if !youtubeOn {
			say(chat, "💾 YouTube ist abgeschaltet – kein Zwischenspeicher.")
			return
		}
		f := strings.Fields(arg)
		if len(f) == 2 && (f[0] == "forget" || f[0] == "vergessen") {
			if !m.Team {
				say(chat, "💾 Nur das Team darf Stücke aus dem Zwischenspeicher löschen.")
			} else if k := youtubeKey(f[1]); k != "" && cacheForget(k) {
				say(chat, "💾 Aus dem Zwischenspeicher gelöscht.")
			} else {
				say(chat, "💾 Nicht im Zwischenspeicher.")
			}
			return
		}
		say(chat, cacheStats())
	case "skip", "next":
		if !p.skip() {
			say(chat, "Gerade läuft nichts.")
		}
	case "stop":
		p.stop()
		say(chat, "⏹️ Angehalten, Warteschlange geleert.")
	case "clear":
		p.mu.Lock()
		p.queue = nil
		p.mu.Unlock()
		say(chat, "🧹 Warteschlange geleert.")
	case "shuffle":
		p.mu.Lock()
		rand.Shuffle(len(p.queue), func(i, j int) { p.queue[i], p.queue[j] = p.queue[j], p.queue[i] })
		p.mu.Unlock()
		say(chat, "🔀 Gemischt.")
	case "loop":
		p.mu.Lock()
		p.loop = !p.loop
		on := p.loop
		p.mu.Unlock()
		say(chat, map[bool]string{true: "🔁 Wiederholen an.", false: "🔁 Wiederholen aus."}[on])
	case "np":
		p.mu.Lock()
		cur := p.cur
		p.mu.Unlock()
		if cur == nil {
			say(chat, "Gerade läuft nichts.")
		} else {
			say(chat, "🎶 "+cur.Title+cur.Credit+byLine(*cur))
		}
	case "queue", "q":
		p.mu.Lock()
		var b strings.Builder
		if p.cur != nil {
			b.WriteString("🎶 " + p.cur.Title + "\n")
		}
		for i, it := range p.queue {
			if i == 10 {
				fmt.Fprintf(&b, "… und %d weitere", len(p.queue)-10)
				break
			}
			fmt.Fprintf(&b, "%d. %s\n", i+1, it.Title)
		}
		p.mu.Unlock()
		if b.Len() == 0 {
			b.WriteString("Die Warteschlange ist leer.")
		}
		say(chat, strings.TrimSpace(b.String()))
	case "vol", "volume":
		n, err := strconv.Atoi(strings.TrimSuffix(arg, "%"))
		if err != nil || n < 1 || n > 100 {
			say(chat, "Lautstärke: "+prefix+"vol 1–100")
			return
		}
		p.mu.Lock()
		p.vol = n
		p.mu.Unlock()
		setVolume(room, n)
		say(chat, fmt.Sprintf("🔊 %d %% (jede Person kann die Musik zusätzlich selbst leiser stellen)", n))
	}
}

func errText(err error) string {
	if err == nil {
		return "keine Treffer"
	}
	return err.Error()
}

func setVolume(room string, n int) {
	req, _ := http.NewRequest("POST", base+"/api/bot/voice/"+url.PathEscape(room)+"/volume?volume="+strconv.Itoa(n), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if res, err := client.Do(req); err == nil {
		res.Body.Close()
	}
}

// --- Chat ---

func say(room, text string) {
	if room == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, _ := http.NewRequest("POST", base+"/api/bot/rooms/"+url.PathEscape(room)+"/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Nachricht in #%s: %v", room, err)
		return
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
}

var errRevoked = errors.New("Zugang entzogen")

func listen() error {
	req, _ := http.NewRequest("GET", base+"/api/bot/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return errRevoked
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Status %d", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev event
		if json.Unmarshal([]byte(line[6:]), &ev) != nil {
			continue
		}
		switch ev.Event {
		case "ready":
			log.Printf("Verbunden als %s – Musik aus %s, Befehle mit %smusic", ev.Bot.Name, musicDir, prefix)
		case "revoked":
			return errRevoked
		case "message":
			if ev.Message != nil && !ev.Message.Bot {
				command(ev.Room, ev.Message)
			}
		}
	}
	return sc.Err()
}

// version wird beim Bauen gesetzt (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("ucng-bot-music", version)
		return
	}
	log.Printf("UCNG Musik-Bot %s", version)
	if token == "" {
		log.Fatal("Bitte UCNG_BOT_TOKEN setzen (Admin-Bereich → Bots; Rechte: Schreiben und Musik).")
	}
	tools := []string{"ffmpeg"}
	if youtubeOn {
		cacheLoad()
		setupCookies()
		tools = append(tools, "yt-dlp")
	} else {
		cacheDir = "" // nichts von YouTube – auch nichts aus dem alten Zwischenspeicher
		log.Printf("YouTube abgeschaltet (YOUTUBE=0): nur Musik aus %s", musicDir)
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			log.Printf("Hinweis: %s nicht gefunden – %s", tool, map[string]string{"ffmpeg": "ohne ffmpeg geht gar nichts", "yt-dlp": "YouTube geht dann nicht"}[tool])
		}
	}
	wait := time.Second
	for {
		err := listen()
		if errors.Is(err, errRevoked) {
			log.Fatal("Der Bot wurde im Admin-Bereich deaktiviert oder gelöscht.")
		}
		log.Printf("Verbindung getrennt (%v) – neuer Versuch in %v", err, wait)
		time.Sleep(wait)
		wait = min(wait*2, 30*time.Second)
	}
}
