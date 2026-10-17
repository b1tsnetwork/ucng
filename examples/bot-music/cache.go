// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Zwischenspeicher für YouTube & Co.: Jedes Stück wird beim ersten Abspielen nebenbei fertig
// in Ogg-Opus umgewandelt und unter CACHE_DIR abgelegt („Titel [ID].opus“). Beim nächsten Mal
// geht es ohne Anfrage bei YouTube und ohne ffmpeg direkt aus der Datei. Schlüssel ist die
// Video-ID (nicht die URL – dieselbe ID steckt in watch?v=, youtu.be/, Shorts, Playlists …);
// Suchbegriffe merkt sich der Bot ebenfalls. Kein Ablauf; optional eine Größengrenze
// (CACHE_MAX_GB), dann fliegt zuerst, was am längsten nicht gespielt wurde.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	cacheDir   = env("CACHE_DIR", "") // leer = aus
	cacheMaxGB = atoiEnv("CACHE_MAX_GB", 0)
)

type cacheEntry struct {
	Title  string    `json:"title"`
	URL    string    `json:"url"`
	File   string    `json:"file"` // Dateiname in CACHE_DIR
	Bytes  int64     `json:"bytes"`
	Kbps   int       `json:"kbps"`
	Added  time.Time `json:"added"`
	Played time.Time `json:"played"`
}

type cacheIndex struct {
	Tracks   map[string]*cacheEntry `json:"tracks"`   // Schlüssel: „youtube-<id>“
	Searches map[string]string      `json:"searches"` // Suchbegriff (klein) → Schlüssel
}

var (
	cacheMu   sync.Mutex
	cache     *cacheIndex
	cacheBusy = map[string]bool{} // wird gerade angelegt
)

func cacheOn() bool { return cacheDir != "" }

func cacheIndexPath() string { return filepath.Join(cacheDir, "index.json") }

// cacheLoad liest den Index (einmal beim Start); Dateien, die fehlen, fallen heraus.
func cacheLoad() {
	if !cacheOn() {
		return
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, ".tmp"), 0o755); err != nil {
		log.Printf("Zwischenspeicher %s nicht beschreibbar (%v) – ausgeschaltet. Im Container läuft der Bot als Nutzer 10001: Musik-Ordner ohne „:ro“ einbinden und z. B. „chown 10001 music/youtube“.", cacheDir, err)
		cacheDir = ""
		return
	}
	os.RemoveAll(filepath.Join(cacheDir, ".tmp")) // Reste eines Absturzes
	os.MkdirAll(filepath.Join(cacheDir, ".tmp"), 0o755)
	idx := &cacheIndex{Tracks: map[string]*cacheEntry{}, Searches: map[string]string{}}
	if b, err := os.ReadFile(cacheIndexPath()); err == nil {
		if err := json.Unmarshal(b, idx); err != nil {
			log.Printf("Zwischenspeicher: index.json unlesbar (%v) – fange neu an", err)
		}
	}
	if idx.Tracks == nil {
		idx.Tracks = map[string]*cacheEntry{}
	}
	if idx.Searches == nil {
		idx.Searches = map[string]string{}
	}
	for k, e := range idx.Tracks {
		if _, err := os.Stat(filepath.Join(cacheDir, e.File)); err != nil {
			delete(idx.Tracks, k)
		}
	}
	cacheMu.Lock()
	cache = idx
	n, size := len(idx.Tracks), cacheSizeLocked()
	cacheSaveLocked()
	cacheMu.Unlock()
	log.Printf("Zwischenspeicher %s: %d Stücke, %s", cacheDir, n, human(size))
}

func cacheSaveLocked() {
	b, _ := json.MarshalIndent(cache, "", " ")
	tmp := cacheIndexPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, cacheIndexPath())
	}
}

func cacheSizeLocked() int64 {
	var n int64
	for _, e := range cache.Tracks {
		n += e.Bytes
	}
	return n
}

// cacheGet: Datei eines Stücks, falls vorhanden (und merkt sich „zuletzt gespielt“).
func cacheGet(key string) (string, *cacheEntry) {
	if !cacheOn() || key == "" {
		return "", nil
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e := cache.Tracks[key]
	if e == nil {
		return "", nil
	}
	path := filepath.Join(cacheDir, e.File)
	if _, err := os.Stat(path); err != nil { // von Hand gelöscht
		delete(cache.Tracks, key)
		cacheSaveLocked()
		return "", nil
	}
	return path, e
}

func cacheTouch(key string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if e := cache.Tracks[key]; e != nil {
		e.Played = time.Now()
		cacheSaveLocked()
	}
}

func cacheSearch(q string) string {
	if !cacheOn() {
		return ""
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return cache.Searches[strings.ToLower(strings.TrimSpace(q))]
}

func cacheRememberSearch(q, key string) {
	if !cacheOn() || key == "" {
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache.Searches[strings.ToLower(strings.TrimSpace(q))] = key
	cacheSaveLocked()
}

// cacheStore wandelt eine heruntergeladene Datei vollständig um und legt sie ab – im
// Hintergrund, parallel zur Wiedergabe (läuft auch weiter, wenn das Stück übersprungen wird).
// done wird danach aufgerufen (dann darf die Quelle weg).
func cacheStore(key, title, link, src string, done func()) {
	cacheMu.Lock()
	if !cacheOn() || key == "" || cache.Tracks[key] != nil || cacheBusy[key] {
		cacheMu.Unlock()
		done()
		return
	}
	cacheBusy[key] = true
	cacheMu.Unlock()
	go func() {
		defer done()
		defer func() { cacheMu.Lock(); delete(cacheBusy, key); cacheMu.Unlock() }()
		name := safeName(title) + " [" + strings.TrimPrefix(key, "youtube-") + "].opus"
		tmp := filepath.Join(cacheDir, ".tmp", key+".opus")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		ff := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
			"-i", src, "-vn", "-map", "0:a:0", "-c:a", "libopus", "-b:a", strconv.Itoa(kbps)+"k",
			"-ac", "2", "-ar", "48000", "-frame_duration", "20", "-application", "audio",
			"-metadata", "title="+title, "-metadata", "comment="+link, "-f", "ogg", tmp)
		if out, err := ff.CombinedOutput(); err != nil {
			os.Remove(tmp)
			log.Printf("Zwischenspeicher: %s nicht umgewandelt: %s", title, firstLine(string(out), err))
			return
		}
		st, err := os.Stat(tmp)
		if err != nil || os.Rename(tmp, filepath.Join(cacheDir, name)) != nil {
			os.Remove(tmp)
			return
		}
		now := time.Now()
		cacheMu.Lock()
		cache.Tracks[key] = &cacheEntry{Title: title, URL: link, File: name, Bytes: st.Size(), Kbps: kbps, Added: now, Played: now}
		cacheEvictLocked(key)
		cacheSaveLocked()
		cacheMu.Unlock()
	}()
}

// cacheEvictLocked: über CACHE_MAX_GB – zuerst, was am längsten nicht gespielt wurde.
func cacheEvictLocked(keep string) {
	if cacheMaxGB <= 0 {
		return
	}
	limit := int64(cacheMaxGB) << 30
	size := cacheSizeLocked()
	if size <= limit {
		return
	}
	keys := make([]string, 0, len(cache.Tracks))
	for k := range cache.Tracks {
		if k != keep {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return cache.Tracks[keys[i]].Played.Before(cache.Tracks[keys[j]].Played) })
	for _, k := range keys {
		if size <= limit {
			break
		}
		e := cache.Tracks[k]
		os.Remove(filepath.Join(cacheDir, e.File))
		size -= e.Bytes
		delete(cache.Tracks, k)
		for q, v := range cache.Searches {
			if v == k {
				delete(cache.Searches, q)
			}
		}
	}
}

// cacheStats für !cache.
func cacheStats() string {
	if !cacheOn() {
		return "💾 Zwischenspeicher aus (CACHE_DIR setzen)."
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	lim := "ohne Grenze"
	if cacheMaxGB > 0 {
		lim = fmt.Sprintf("Grenze %d GB", cacheMaxGB)
	}
	return fmt.Sprintf("💾 Zwischenspeicher: %d Stücke, %s (%s)", len(cache.Tracks), human(cacheSizeLocked()), lim)
}

// cacheForget entfernt ein Stück (Team).
func cacheForget(key string) bool {
	if !cacheOn() || key == "" {
		return false
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e := cache.Tracks[key]
	if e == nil {
		return false
	}
	os.Remove(filepath.Join(cacheDir, e.File))
	delete(cache.Tracks, key)
	for q, v := range cache.Searches {
		if v == key {
			delete(cache.Searches, q)
		}
	}
	cacheSaveLocked()
	return true
}

var ytID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youtubeKey erkennt die Video-ID in gängigen YouTube-Links – ohne Anfrage bei YouTube.
// Playlists (list= ohne v=) liefern "", die werden aufgelöst.
func youtubeKey(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	var id string
	switch host {
	case "youtu.be":
		id = strings.Trim(u.Path, "/")
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		switch {
		case u.Path == "/watch":
			id = u.Query().Get("v")
		case strings.HasPrefix(u.Path, "/shorts/"), strings.HasPrefix(u.Path, "/live/"), strings.HasPrefix(u.Path, "/embed/"):
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) >= 2 {
				id = parts[1]
			}
		}
	}
	if !ytID.MatchString(id) {
		return ""
	}
	return "youtube-" + id
}

// keyOf: Schlüssel aus den Angaben von yt-dlp (Seite + ID).
func keyOf(extractor, id string) string {
	extractor = strings.ToLower(strings.TrimSpace(extractor))
	if extractor == "" || id == "" {
		return ""
	}
	if extractor == "youtubetab" {
		extractor = "youtube"
	}
	return safeName(extractor + "-" + id)
}

var unsafeChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]+`)

// safeName: Dateiname, der unter Linux und Windows (Freigaben) geht.
func safeName(s string) string {
	s = strings.Join(strings.Fields(unsafeChars.ReplaceAllString(s, " ")), " ")
	s = strings.Trim(s, ". ")
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimSpace(string(r[:120]))
	}
	if s == "" {
		s = "track"
	}
	return s
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}
