// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Medien: eigene Emotes, eine GIF-Bibliothek und (optional) die GIF-Suche im Web über
// GIPHY oder Tenor. Alle Bilder werden beim Hochladen bzw. beim ersten Senden auf eine
// feste Größe gebracht und vom eigenen Server ausgeliefert – Besucher laden nichts von
// Drittanbietern (Ausnahme: die kleinen Vorschaubilder der Web-Suche laufen ebenfalls
// über den Server, siehe handleWebPreview).
//
//	data/media/emotes/<name>-<zufall>.png|gif   skalierte Emotes
//	data/media/gifs/<id>-<breite>.gif           skalierte Bibliotheks-GIFs
//	data/media/web/<anbieter>_<id>-<breite>.gif aus der Web-Suche gesendete GIFs
//	data/media/originals/…                       Originale (zum erneuten Skalieren)

type Emote struct {
	Name     string    `json:"name"`
	File     string    `json:"file"`
	Original string    `json:"original,omitempty"`
	Size     int       `json:"size"` // 32 oder 64 (feste Kantenlänge)
	Animated bool      `json:"animated,omitempty"`
	Created  time.Time `json:"created"`
}

type LibGIF struct {
	ID       string    `json:"id"`
	File     string    `json:"file"`
	Original string    `json:"original,omitempty"`
	Title    string    `json:"title"`
	Tags     []string  `json:"tags"`
	W        int       `json:"w"`
	H        int       `json:"h"`
	Created  time.Time `json:"created"`
}

// MediaConfig sind die Einstellungen (media.json). Der API-Schlüssel verlässt den Server nie.
type MediaConfig struct {
	EmoteSize   int    `json:"emoteSize"`   // Standardgröße neuer Emotes: 32 oder 64
	GifsEnabled bool   `json:"gifsEnabled"` // GIFs im Chat senden
	GifWidth    int    `json:"gifWidth"`    // GIFs passen in einen Rahmen dieser Breite (und Höhe)
	Provider    string `json:"provider"`    // "", "giphy" oder "tenor"
	APIKey      string `json:"apiKey,omitempty"`
	Rating      string `json:"rating"` // g, pg, pg-13, r
	// Reaktionen: Schnellauswahl (Emojis oder :emote:) und ob alle Emotes erlaubt sind
	Reactions        []string `json:"reactions,omitempty"`
	NoEmoteReactions bool     `json:"noEmoteReactions,omitempty"`
	// Bilder und Profilbilder
	Uploads    string `json:"uploads,omitempty"`    // off, all, known (Standard), roles
	UploadSize int    `json:"uploadSize,omitempty"` // Rahmen in Pixeln (Standard 480)
	NoAvatars  bool   `json:"noAvatars,omitempty"`
}

// DefaultReactions ist die Schnellauswahl, solange nichts eingestellt ist.
var DefaultReactions = []string{"👍", "❤️", "😂", "😮", "😢", "🙏"}

const maxReactionChoices = 12

// Media ist ein Bild an einer Nachricht (derzeit nur GIFs).
type Media struct {
	Kind   string `json:"kind"` // "gif"
	Src    string `json:"src"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Title  string `json:"title,omitempty"`
	Source string `json:"source,omitempty"` // library, giphy, tenor, beacon
	Credit string `json:"credit,omitempty"` // Quellenangabe, z. B. „via Beacon Alpenraum“ (Beacon)
}

var (
	EmoteSizes = []int{32, 64}
	GifWidths  = []int{200, 250, 300, 400}

	emoteNameRe = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)
	mediaFileRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,120}$`)
	webIDRe     = regexp.MustCompile(`^((giphy|tenor):[A-Za-z0-9_-]{1,64}|beacon:[0-9a-f]{8}:b_[0-9a-f]{10})$`)

	ErrEmoteName     = errors.New("Emote-Name: 2–32 Zeichen, nur a–z, 0–9 und _")
	ErrEmoteExists   = errors.New("Ein Emote mit diesem Namen gibt es schon")
	ErrNoEmote       = errors.New("Dieses Emote gibt es nicht")
	ErrEmoteSize     = errors.New("Emote-Größe muss 32 oder 64 sein")
	ErrNoGIF         = errors.New("Dieses GIF gibt es nicht")
	ErrGifsOff       = errors.New("GIFs sind ausgeschaltet")
	ErrGifWidth      = errors.New("GIF-Größe muss 200, 250, 300 oder 400 sein")
	ErrGifProvider   = errors.New("Anbieter muss leer, giphy oder tenor sein")
	ErrGifRating     = errors.New("Jugendschutz muss g, pg, pg-13 oder r sein")
	ErrReactionList  = errors.New("Die Schnellauswahl braucht 1 bis 12 verschiedene Reaktionen")
	ErrReactionItem  = errors.New("Reaktionen sind einzelne Emojis oder vorhandene Emotes (:name:)")
	ErrNoWebSearch   = errors.New("Die GIF-Suche im Web ist nicht eingerichtet")
	ErrWebGIF        = errors.New("Dieses GIF konnte nicht geladen werden")
	ErrMediaTooLarge = errors.New("Die Datei ist zu groß")
	ErrTitle         = errors.New("Titel: höchstens 80 Zeichen")
)

const (
	maxEmoteUpload = 4 << 20
	maxGIFUpload   = 20 << 20
	maxPreview     = 4 << 20
	webCacheTTL    = 30 * time.Minute
)

type webEntry struct {
	id, title string
	full      string // Adresse des GIFs in voller Größe
	preview   string // kleines Vorschaubild
	w, h      int
	pw, ph    int
	at        time.Time
	credit    string // Quellenangabe (Beacon)
}

type MediaLib struct {
	mu      sync.Mutex
	dir     string
	emotes  map[string]*Emote
	gifs    map[string]*LibGIF
	cfg     MediaConfig
	changed func(doc string)
	onList  func() // Emotes oder Einstellungen geändert → Clients informieren

	client  *http.Client
	apiBase map[string]string // für Tests austauschbar
	webMu   sync.Mutex
	web     map[string]webEntry // Treffer der letzten Suchen: "giphy:<id>" → Adressen
	work    chan struct{}       // begrenzt gleichzeitige Bildverarbeitung
	fileMu  sync.Mutex
	// abonnierte GIF-Sammlungen von Beacons (siehe beacon.go)
	beaconSubs func() []BeaconSub
}

func NewMediaLib() *MediaLib {
	return &MediaLib{
		emotes: map[string]*Emote{}, gifs: map[string]*LibGIF{},
		cfg:     MediaConfig{EmoteSize: 32, GifsEnabled: true, GifWidth: 300, Rating: "g"},
		client:  &http.Client{Timeout: 20 * time.Second},
		apiBase: map[string]string{"giphy": "https://api.giphy.com", "tenor": "https://tenor.googleapis.com"},
		web:     map[string]webEntry{}, work: make(chan struct{}, 2),
	}
}

// SetDir legt den Ordner für Bilddateien fest (beim Start).
func (ml *MediaLib) SetDir(dir string) {
	ml.mu.Lock()
	ml.dir = dir
	ml.mu.Unlock()
	for _, sub := range []string{"emotes", "gifs", "web", "originals", "uploads", "avatars"} {
		os.MkdirAll(filepath.Join(dir, sub), 0o700)
	}
}

func (ml *MediaLib) Dir() string {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	return ml.dir
}

func (ml *MediaLib) touch(doc string) {
	if ml.changed != nil {
		ml.changed(doc)
	}
}

func (ml *MediaLib) notify() {
	if ml.onList != nil {
		ml.onList()
	}
}

func (ml *MediaLib) Config() MediaConfig {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	return ml.cfg
}

// writeMedia speichert eine Datei atomar im Medienordner.
func (ml *MediaLib) writeMedia(sub, name string, data []byte) error {
	return writeFileAtomic(filepath.Join(ml.Dir(), sub, name), data)
}

func (ml *MediaLib) removeMedia(sub, name string) {
	if name != "" && mediaFileRe.MatchString(name) {
		os.Remove(filepath.Join(ml.Dir(), sub, name))
	}
}

// ErrBusy: Die Bildverarbeitung ist gerade ausgelastet.
var ErrBusy = errors.New("Der Server ist gerade ausgelastet – bitte gleich noch einmal versuchen")

// runImage führt fn aus, sobald die Bildverarbeitung frei ist (höchstens zwei gleichzeitig).
// Wartet höchstens 20 Sekunden – sonst sammeln sich Anfragen samt Dateien im Speicher.
func (ml *MediaLib) runImage(data []byte, fn func() (ScaledImage, error)) (ScaledImage, error) {
	if _, _, err := checkImage(data); err != nil { // billig – vor dem Warten ablehnen
		return ScaledImage{}, err
	}
	t := time.NewTimer(20 * time.Second)
	defer t.Stop()
	select {
	case ml.work <- struct{}{}:
	case <-t.C:
		return ScaledImage{}, ErrBusy
	}
	defer func() { <-ml.work }()
	return fn()
}

func (ml *MediaLib) process(data []byte, box int, emote, requireGIF bool) (ScaledImage, error) {
	return ml.runImage(data, func() (ScaledImage, error) { return ProcessImage(data, box, box, emote, requireGIF) })
}

// --- Emotes ---

// NormEmoteName macht aus einem Dateinamen oder einer Eingabe einen gültigen Namen.
func NormEmoteName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimPrefix(s, ":"), ":")
	if i := strings.LastIndex(s, "."); i > 0 {
		switch s[i+1:] {
		case "png", "gif", "jpg", "jpeg", "webp":
			s = s[:i]
		}
	}
	s = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "-", "_", " ", "_").Replace(s)
	return s
}

type EmoteInfo struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Size     int    `json:"size"`
	Animated bool   `json:"animated,omitempty"`
}

func (e *Emote) info() EmoteInfo {
	return EmoteInfo{Name: e.Name, URL: "/media/emotes/" + e.File, Size: e.Size, Animated: e.Animated}
}

func (ml *MediaLib) Emotes() []EmoteInfo {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	out := make([]EmoteInfo, 0, len(ml.emotes))
	for _, e := range ml.emotes {
		out = append(out, e.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func validEmoteSize(size int) bool { return size == 32 || size == 64 }

// AddEmote skaliert und speichert ein neues Emote. size = 0: Standardgröße.
func (ml *MediaLib) AddEmote(name string, size int, data []byte) (EmoteInfo, error) {
	name = NormEmoteName(name)
	if !emoteNameRe.MatchString(name) {
		return EmoteInfo{}, ErrEmoteName
	}
	if size == 0 {
		size = ml.Config().EmoteSize
	}
	if !validEmoteSize(size) {
		return EmoteInfo{}, ErrEmoteSize
	}
	if len(data) > maxEmoteUpload {
		return EmoteInfo{}, ErrMediaTooLarge
	}
	ml.mu.Lock()
	_, exists := ml.emotes[name]
	ml.mu.Unlock()
	if exists {
		return EmoteInfo{}, ErrEmoteExists
	}
	img, err := ml.process(data, size, true, false)
	if err != nil {
		return EmoteInfo{}, err
	}
	stamp := randomHex(4)
	e := &Emote{Name: name, Size: size, Animated: img.Animated, Created: time.Now().UTC(),
		File: name + "-" + stamp + "." + img.Ext, Original: "emote-" + name + "-" + stamp + ".bin"}
	if err := ml.writeMedia("emotes", e.File, img.Data); err != nil {
		return EmoteInfo{}, err
	}
	if err := ml.writeMedia("originals", e.Original, data); err != nil {
		e.Original = ""
	}
	ml.mu.Lock()
	if _, exists := ml.emotes[name]; exists { // gleichzeitig angelegt
		ml.mu.Unlock()
		ml.removeMedia("emotes", e.File)
		ml.removeMedia("originals", e.Original)
		return EmoteInfo{}, ErrEmoteExists
	}
	ml.emotes[name] = e
	ml.mu.Unlock()
	ml.touch("emotes.json")
	ml.notify()
	return e.info(), nil
}

// UpdateEmote benennt um und/oder ändert die Größe (aus dem Original neu skaliert).
func (ml *MediaLib) UpdateEmote(name, newName string, size int) (EmoteInfo, error) {
	newName = NormEmoteName(newName)
	if newName == "" {
		newName = name
	}
	if !emoteNameRe.MatchString(newName) {
		return EmoteInfo{}, ErrEmoteName
	}
	ml.mu.Lock()
	e, ok := ml.emotes[name]
	if !ok {
		ml.mu.Unlock()
		return EmoteInfo{}, ErrNoEmote
	}
	if _, taken := ml.emotes[newName]; taken && newName != name {
		ml.mu.Unlock()
		return EmoteInfo{}, ErrEmoteExists
	}
	cur := *e
	ml.mu.Unlock()
	if size == 0 {
		size = cur.Size
	}
	if !validEmoteSize(size) {
		return EmoteInfo{}, ErrEmoteSize
	}
	next := cur
	next.Name = newName
	if size != cur.Size {
		if cur.Original == "" {
			return EmoteInfo{}, errors.New("Für dieses Emote fehlt das Original – bitte neu hochladen")
		}
		orig, err := os.ReadFile(filepath.Join(ml.Dir(), "originals", cur.Original))
		if err != nil {
			return EmoteInfo{}, errors.New("Das Original konnte nicht gelesen werden – bitte neu hochladen")
		}
		img, err := ml.process(orig, size, true, false)
		if err != nil {
			return EmoteInfo{}, err
		}
		next.Size, next.Animated = size, img.Animated
		next.File = newName + "-" + randomHex(4) + "." + img.Ext
		if err := ml.writeMedia("emotes", next.File, img.Data); err != nil {
			return EmoteInfo{}, err
		}
	} else if newName != name {
		// Datei behält ihren Namen nicht – neue Adresse, damit Caches nichts Altes zeigen
		ext := filepath.Ext(cur.File)
		next.File = newName + "-" + randomHex(4) + ext
		if err := os.Rename(filepath.Join(ml.Dir(), "emotes", cur.File), filepath.Join(ml.Dir(), "emotes", next.File)); err != nil {
			return EmoteInfo{}, err
		}
	}
	ml.mu.Lock()
	delete(ml.emotes, name)
	ml.emotes[newName] = &next
	renamedReaction := false
	if newName != name {
		for i, r := range ml.cfg.Reactions {
			if r == ":"+name+":" {
				ml.cfg.Reactions[i] = ":" + newName + ":"
				renamedReaction = true
			}
		}
	}
	ml.mu.Unlock()
	if next.File != cur.File && size != cur.Size {
		ml.removeMedia("emotes", cur.File)
	}
	if renamedReaction {
		ml.touch("media.json")
	}
	ml.touch("emotes.json")
	ml.notify()
	return next.info(), nil
}

func (ml *MediaLib) DeleteEmote(name string) error {
	ml.mu.Lock()
	e, ok := ml.emotes[name]
	delete(ml.emotes, name)
	ml.mu.Unlock()
	if !ok {
		return ErrNoEmote
	}
	ml.removeMedia("emotes", e.File)
	ml.removeMedia("originals", e.Original)
	ml.touch("emotes.json")
	ml.notify()
	return nil
}

// --- GIF-Bibliothek ---

type GIFInfo struct {
	ID    string   `json:"id"`
	URL   string   `json:"url"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	W     int      `json:"w"`
	H     int      `json:"h"`
}

func (g *LibGIF) info() GIFInfo {
	return GIFInfo{ID: g.ID, URL: "/media/gifs/" + g.File, Title: g.Title, Tags: append([]string{}, g.Tags...), W: g.W, H: g.H}
}

func cleanTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.ToLower(strings.Join(strings.Fields(t), " "))
		if t != "" && len([]rune(t)) <= 32 && !seen[t] && len(out) < 20 {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func cleanTitle(t string) (string, error) {
	t = strings.Join(strings.Fields(t), " ")
	if len([]rune(t)) > 80 {
		return "", ErrTitle
	}
	return t, nil
}

func (ml *MediaLib) AddGIF(title string, tags []string, data []byte) (GIFInfo, error) {
	title, err := cleanTitle(title)
	if err != nil {
		return GIFInfo{}, err
	}
	if len(data) > maxGIFUpload {
		return GIFInfo{}, ErrMediaTooLarge
	}
	width := ml.Config().GifWidth
	img, err := ml.process(data, width, false, true)
	if err != nil {
		return GIFInfo{}, err
	}
	id := "g_" + randomHex(5)
	g := &LibGIF{ID: id, Title: title, Tags: cleanTags(tags), W: img.W, H: img.H, Created: time.Now().UTC(),
		File: fmt.Sprintf("%s-%d.gif", id, width), Original: "gif-" + id + ".bin"}
	if err := ml.writeMedia("gifs", g.File, img.Data); err != nil {
		return GIFInfo{}, err
	}
	if err := ml.writeMedia("originals", g.Original, data); err != nil {
		g.Original = ""
	}
	ml.mu.Lock()
	ml.gifs[id] = g
	ml.mu.Unlock()
	ml.touch("gifs.json")
	return g.info(), nil
}

func (ml *MediaLib) UpdateGIF(id, title string, tags []string) (GIFInfo, error) {
	title, err := cleanTitle(title)
	if err != nil {
		return GIFInfo{}, err
	}
	ml.mu.Lock()
	g, ok := ml.gifs[id]
	if ok {
		g.Title, g.Tags = title, cleanTags(tags)
	}
	var info GIFInfo
	if ok {
		info = g.info()
	}
	ml.mu.Unlock()
	if !ok {
		return GIFInfo{}, ErrNoGIF
	}
	ml.touch("gifs.json")
	return info, nil
}

func (ml *MediaLib) DeleteGIF(id string) error {
	ml.mu.Lock()
	g, ok := ml.gifs[id]
	delete(ml.gifs, id)
	ml.mu.Unlock()
	if !ok {
		return ErrNoGIF
	}
	// Die Datei bleibt, solange alte Nachrichten sie zeigen? Nein: Wer löscht, will es weg haben.
	ml.removeMedia("gifs", g.File)
	ml.removeMedia("originals", g.Original)
	ml.touch("gifs.json")
	return nil
}

// GIFs liefert die Bibliothek, optional gefiltert (Titel und Schlagwörter).
func (ml *MediaLib) GIFs(q string) []GIFInfo {
	q = strings.ToLower(strings.TrimSpace(q))
	ml.mu.Lock()
	defer ml.mu.Unlock()
	type hit struct {
		g     *LibGIF
		score int
	}
	var hits []hit
	for _, g := range ml.gifs {
		score := 1
		if q != "" {
			score = 0
			if strings.Contains(strings.ToLower(g.Title), q) {
				score = 2
			}
			for _, t := range g.Tags {
				if strings.Contains(t, q) {
					score = 3
				}
			}
		}
		if score > 0 {
			hits = append(hits, hit{g, score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].g.Created.After(hits[j].g.Created)
	})
	out := make([]GIFInfo, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.g.info())
	}
	return out
}

func (ml *MediaLib) libMedia(id string) (*Media, error) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	g, ok := ml.gifs[id]
	if !ok {
		return nil, ErrNoGIF
	}
	return &Media{Kind: "gif", Src: "/media/gifs/" + g.File, W: g.W, H: g.H, Title: g.Title, Source: "library"}, nil
}

// RescaleGIFs bringt alle Bibliotheks-GIFs auf die eingestellte Größe (nach einer Änderung).
func (ml *MediaLib) RescaleGIFs() {
	width := ml.Config().GifWidth
	ml.mu.Lock()
	var list []LibGIF
	for _, g := range ml.gifs {
		list = append(list, *g)
	}
	ml.mu.Unlock()
	for _, g := range list {
		if g.Original == "" || strings.HasSuffix(g.File, fmt.Sprintf("-%d.gif", width)) {
			continue
		}
		orig, err := os.ReadFile(filepath.Join(ml.Dir(), "originals", g.Original))
		if err != nil {
			continue
		}
		img, err := ml.process(orig, width, false, true)
		if err != nil {
			log.Printf("GIF %s nicht neu skaliert: %v", g.ID, err)
			continue
		}
		file := fmt.Sprintf("%s-%d.gif", g.ID, width)
		if ml.writeMedia("gifs", file, img.Data) != nil {
			continue
		}
		ml.mu.Lock()
		if cur, ok := ml.gifs[g.ID]; ok {
			cur.File, cur.W, cur.H = file, img.W, img.H
		}
		ml.mu.Unlock()
		// alte Datei bleibt für bereits gesendete Nachrichten erhalten
	}
	ml.touch("gifs.json")
}

// --- Einstellungen ---

type MediaSettingsInput struct {
	EmoteSize   int     `json:"emoteSize"`
	GifsEnabled bool    `json:"gifsEnabled"`
	GifWidth    int     `json:"gifWidth"`
	Provider    string  `json:"provider"`
	APIKey      *string `json:"apiKey"` // fehlt = unverändert, "" = löschen
	Rating      string  `json:"rating"`
}

// MediaSettingsView ist die Sicht im Admin-Bereich (ohne Schlüssel).
type MediaSettingsView struct {
	EmoteSize   int    `json:"emoteSize"`
	GifsEnabled bool   `json:"gifsEnabled"`
	GifWidth    int    `json:"gifWidth"`
	Provider    string `json:"provider"`
	HasKey      bool   `json:"hasKey"`
	Rating      string `json:"rating"`
}

func (c MediaConfig) view() MediaSettingsView {
	return MediaSettingsView{EmoteSize: c.EmoteSize, GifsEnabled: c.GifsEnabled, GifWidth: c.GifWidth,
		Provider: c.Provider, HasKey: c.APIKey != "", Rating: c.Rating}
}

func (ml *MediaLib) SetConfig(in MediaSettingsInput) (MediaSettingsView, bool, error) {
	if !validEmoteSize(in.EmoteSize) {
		return MediaSettingsView{}, false, ErrEmoteSize
	}
	okW := false
	for _, w := range GifWidths {
		okW = okW || w == in.GifWidth
	}
	if !okW {
		return MediaSettingsView{}, false, ErrGifWidth
	}
	if in.Provider != "" && in.Provider != "giphy" && in.Provider != "tenor" {
		return MediaSettingsView{}, false, ErrGifProvider
	}
	switch in.Rating {
	case "":
		in.Rating = "g"
	case "g", "pg", "pg-13", "r":
	default:
		return MediaSettingsView{}, false, ErrGifRating
	}
	ml.mu.Lock()
	rescale := ml.cfg.GifWidth != in.GifWidth
	ml.cfg.EmoteSize, ml.cfg.GifsEnabled, ml.cfg.GifWidth = in.EmoteSize, in.GifsEnabled, in.GifWidth
	ml.cfg.Provider, ml.cfg.Rating = in.Provider, in.Rating
	if in.APIKey != nil {
		ml.cfg.APIKey = strings.TrimSpace(*in.APIKey)
	}
	v := ml.cfg.view()
	ml.mu.Unlock()
	ml.touch("media.json")
	ml.notify()
	return v, rescale, nil
}

// --- Reaktionen ---

// ReactionSettings ist die Einstellung der Reaktionen.
type ReactionSettings struct {
	Reactions      []string `json:"reactions"`      // Schnellauswahl in dieser Reihenfolge
	EmoteReactions bool     `json:"emoteReactions"` // alle Emotes als Reaktion erlaubt
}

// isEmoji prüft grob, ob s ein einzelnes Emoji ist (keine Buchstaben, kein Text).
func isEmoji(s string) bool {
	if s == "" || len(s) > 32 || utf8.RuneCountInString(s) > 8 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x80 && r != '#' && r != '*' && !(r >= '0' && r <= '9') {
			return false
		}
		if unicode.IsLetter(r) || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	// nur Ziffern/#/* (ohne Tastenkappe) sind kein Emoji
	return strings.IndexFunc(s, func(r rune) bool { return r >= 0x80 }) >= 0
}

// emoteRef liefert den Namen bei ":name:".
func emoteRef(s string) (string, bool) {
	if len(s) > 2 && s[0] == ':' && s[len(s)-1] == ':' && emoteNameRe.MatchString(s[1:len(s)-1]) {
		return s[1 : len(s)-1], true
	}
	return "", false
}

// Reactions liefert die aktuelle Einstellung (gelöschte Emotes fallen heraus).
func (ml *MediaLib) Reactions() ReactionSettings {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	return ml.reactionsLocked()
}

func (ml *MediaLib) reactionsLocked() ReactionSettings {
	list := ml.cfg.Reactions
	if len(list) == 0 {
		list = DefaultReactions
	}
	out := make([]string, 0, len(list))
	for _, r := range list {
		if name, ok := emoteRef(r); ok {
			if _, exists := ml.emotes[name]; !exists {
				continue
			}
		}
		out = append(out, r)
	}
	return ReactionSettings{Reactions: out, EmoteReactions: !ml.cfg.NoEmoteReactions}
}

// SetReactions speichert Schnellauswahl und Emote-Reaktionen.
func (ml *MediaLib) SetReactions(in ReactionSettings) (ReactionSettings, error) {
	seen := map[string]bool{}
	list := make([]string, 0, len(in.Reactions))
	ml.mu.Lock()
	for _, r := range in.Reactions {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		if name, ok := emoteRef(strings.ToLower(r)); ok {
			if _, exists := ml.emotes[name]; !exists {
				ml.mu.Unlock()
				return ReactionSettings{}, fmt.Errorf("Das Emote :%s: gibt es nicht", name)
			}
			r = ":" + name + ":"
		} else if !isEmoji(r) {
			ml.mu.Unlock()
			return ReactionSettings{}, ErrReactionItem
		}
		seen[r] = true
		list = append(list, r)
	}
	if len(list) == 0 || len(list) > maxReactionChoices {
		ml.mu.Unlock()
		return ReactionSettings{}, ErrReactionList
	}
	ml.cfg.Reactions = list
	ml.cfg.NoEmoteReactions = !in.EmoteReactions
	out := ml.reactionsLocked()
	ml.mu.Unlock()
	ml.touch("media.json")
	ml.notify()
	return out, nil
}

// ValidReaction: in der Schnellauswahl oder ein vorhandenes Emote (wenn erlaubt).
func (ml *MediaLib) ValidReaction(e string) bool {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	rs := ml.reactionsLocked()
	for _, r := range rs.Reactions {
		if r == e {
			return true
		}
	}
	if name, ok := emoteRef(e); ok && rs.EmoteReactions {
		_, exists := ml.emotes[name]
		return exists
	}
	return false
}

// PublicMedia ist das, was Chat und Widget brauchen.
type PublicMedia struct {
	Emotes         []EmoteInfo `json:"emotes"`
	Gifs           bool        `json:"gifs"`     // GIFs erlaubt
	Web            string      `json:"web"`      // Anbieter der Web-Suche ("" = keine)
	Width          int         `json:"gifWidth"` // Rahmen für GIFs
	Reactions      []string    `json:"reactions"`
	EmoteReactions bool        `json:"emoteReactions"`
	Uploads        string      `json:"uploads"`    // off, all, known, roles
	UploadSize     int         `json:"uploadSize"` // Rahmen für Bilder
	Avatars        bool        `json:"avatars"`    // Profilbilder erlaubt
	Beacon         bool        `json:"beacon"`     // GIF-Sammlungen von Beacons abonniert
}

func (ml *MediaLib) Public() PublicMedia {
	c := ml.Config()
	rs := ml.Reactions()
	up := c.uploads()
	p := PublicMedia{Emotes: ml.Emotes(), Gifs: c.GifsEnabled, Width: c.GifWidth, Reactions: rs.Reactions, EmoteReactions: rs.EmoteReactions,
		Uploads: up.Mode, UploadSize: up.Size, Avatars: up.Avatars}
	if c.GifsEnabled && c.Provider != "" && c.APIKey != "" {
		p.Web = c.Provider
	}
	p.Beacon = c.GifsEnabled && ml.beaconSubs != nil && len(ml.beaconSubs()) > 0
	return p
}

// --- Speichern ---

func (ml *MediaLib) exportEmotes() []Emote {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	out := []Emote{}
	for _, e := range ml.emotes {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (ml *MediaLib) exportGIFs() []LibGIF {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	out := []LibGIF{}
	for _, g := range ml.gifs {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (ml *MediaLib) restore(emotes []Emote, gifs []LibGIF, cfg *MediaConfig) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	for i := range emotes {
		e := emotes[i]
		if emoteNameRe.MatchString(e.Name) && mediaFileRe.MatchString(e.File) {
			ml.emotes[e.Name] = &e
		}
	}
	for i := range gifs {
		g := gifs[i]
		if g.ID != "" && mediaFileRe.MatchString(g.File) {
			if g.Tags == nil {
				g.Tags = []string{}
			}
			ml.gifs[g.ID] = &g
		}
	}
	if cfg != nil {
		c := *cfg
		if !validEmoteSize(c.EmoteSize) {
			c.EmoteSize = 32
		}
		if c.GifWidth == 0 {
			c.GifWidth = 300
		}
		if c.Rating == "" {
			c.Rating = "g"
		}
		ml.cfg = c
	}
}

// --- Laden aus dem Web ---

// fetch lädt eine Datei (höchstens limit Bytes).
func (ml *MediaLib) fetch(rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("Bitte eine vollständige Adresse mit https:// angeben")
	}
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "UCNG/1 (+media)")
	res, err := ml.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Download fehlgeschlagen: %s", shortErr(err))
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Download fehlgeschlagen: Status %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrMediaTooLarge
	}
	return data, nil
}

func (ml *MediaLib) ImportEmote(name string, size int, rawURL string) (EmoteInfo, error) {
	if name == "" {
		if u, err := url.Parse(rawURL); err == nil {
			name = filepath.Base(u.Path)
		}
	}
	data, err := ml.fetch(rawURL, maxEmoteUpload)
	if err != nil {
		return EmoteInfo{}, err
	}
	return ml.AddEmote(name, size, data)
}

func (ml *MediaLib) ImportGIF(title string, tags []string, rawURL string) (GIFInfo, error) {
	data, err := ml.fetch(rawURL, maxGIFUpload)
	if err != nil {
		return GIFInfo{}, err
	}
	return ml.AddGIF(title, tags, data)
}

// --- Web-Suche (GIPHY, Tenor) ---

type WebGIF struct {
	ID      string `json:"id"` // "giphy:<id>"
	Title   string `json:"title"`
	W       int    `json:"w"` // Vorschaugröße
	H       int    `json:"h"`
	Preview string `json:"preview"`
}

type giphyImage struct {
	URL    string `json:"url"`
	Width  string `json:"width"`
	Height string `json:"height"`
}

type giphyItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Images struct {
		Original        giphyImage `json:"original"`
		Downsized       giphyImage `json:"downsized"`
		FixedWidth      giphyImage `json:"fixed_width"`
		FixedWidthSmall giphyImage `json:"fixed_width_small"`
	} `json:"images"`
}

type tenorItem struct {
	ID           string `json:"id"`
	Description  string `json:"content_description"`
	MediaFormats map[string]struct {
		URL  string `json:"url"`
		Dims []int  `json:"dims"`
	} `json:"media_formats"`
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func (e giphyItem) entry() webEntry {
	full := e.Images.Downsized
	if full.URL == "" {
		full = e.Images.FixedWidth
	}
	if full.URL == "" {
		full = e.Images.Original
	}
	prev := e.Images.FixedWidthSmall
	if prev.URL == "" {
		prev = e.Images.FixedWidth
	}
	return webEntry{id: "giphy:" + e.ID, title: e.Title, full: full.URL, preview: prev.URL,
		w: atoiSafe(full.Width), h: atoiSafe(full.Height), pw: atoiSafe(prev.Width), ph: atoiSafe(prev.Height), at: time.Now()}
}

func (e tenorItem) entry() webEntry {
	en := webEntry{id: "tenor:" + e.ID, title: e.Description, at: time.Now()}
	if f, ok := e.MediaFormats["gif"]; ok {
		en.full = f.URL
		if len(f.Dims) == 2 {
			en.w, en.h = f.Dims[0], f.Dims[1]
		}
	}
	if f, ok := e.MediaFormats["tinygif"]; ok {
		en.preview = f.URL
		if len(f.Dims) == 2 {
			en.pw, en.ph = f.Dims[0], f.Dims[1]
		}
	}
	if en.preview == "" {
		en.preview, en.pw, en.ph = en.full, en.w, en.h
	}
	return en
}

var tenorFilter = map[string]string{"g": "high", "pg": "medium", "pg-13": "low", "r": "off"}

// apiGet ruft die API des Anbieters auf und dekodiert die Antwort.
func (ml *MediaLib) apiGet(provider, path string, q url.Values, v any) error {
	ml.mu.Lock()
	base := ml.apiBase[provider]
	ml.mu.Unlock()
	data, err := ml.fetch(base+path+"?"+q.Encode(), 4<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errors.New("Unerwartete Antwort des GIF-Anbieters")
	}
	return nil
}

// SearchWeb sucht GIFs beim eingestellten Anbieter. Leere Suche = Trends.
func (ml *MediaLib) SearchWeb(q string) ([]WebGIF, error) {
	c := ml.Config()
	if !c.GifsEnabled || c.Provider == "" || c.APIKey == "" {
		return nil, ErrNoWebSearch
	}
	q = strings.TrimSpace(q)
	if len([]rune(q)) > 60 {
		q = string([]rune(q)[:60])
	}
	var entries []webEntry
	switch c.Provider {
	case "giphy":
		params := url.Values{"api_key": {c.APIKey}, "limit": {"24"}, "rating": {c.Rating}, "lang": {"de"}}
		path := "/v1/gifs/trending"
		if q != "" {
			params.Set("q", q)
			path = "/v1/gifs/search"
		}
		var res struct {
			Data []giphyItem `json:"data"`
		}
		if err := ml.apiGet("giphy", path, params, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Data {
			entries = append(entries, it.entry())
		}
	case "tenor":
		params := url.Values{"key": {c.APIKey}, "client_key": {"ucng"}, "limit": {"24"},
			"media_filter": {"gif,tinygif"}, "contentfilter": {tenorFilter[c.Rating]}, "locale": {"de_DE"}}
		path := "/v2/featured"
		if q != "" {
			params.Set("q", q)
			path = "/v2/search"
		}
		var res struct {
			Results []tenorItem `json:"results"`
		}
		if err := ml.apiGet("tenor", path, params, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Results {
			entries = append(entries, it.entry())
		}
	}
	out := []WebGIF{}
	ml.webMu.Lock()
	for k, e := range ml.web { // alte Treffer vergessen
		if time.Since(e.at) > webCacheTTL {
			delete(ml.web, k)
		}
	}
	for _, e := range entries {
		if e.full == "" || !webIDRe.MatchString(e.id) {
			continue
		}
		ml.web[e.id] = e
		out = append(out, WebGIF{ID: e.id, Title: e.title, W: e.pw, H: e.ph, Preview: "/api/gifs/web/preview/" + e.id})
	}
	ml.webMu.Unlock()
	return out, nil
}

// webEntryFor liefert die Adressen eines Treffers – aus der letzten Suche oder neu beim Anbieter.
// Clients schicken nur IDs, nie Adressen: Der Server lädt ausschließlich, was der Anbieter nennt.
func (ml *MediaLib) webEntryFor(id string) (webEntry, error) {
	if !webIDRe.MatchString(id) {
		return webEntry{}, ErrWebGIF
	}
	ml.webMu.Lock()
	e, ok := ml.web[id]
	ml.webMu.Unlock()
	if ok {
		return e, nil
	}
	c := ml.Config()
	provider, raw, _ := strings.Cut(id, ":")
	if provider == "beacon" {
		return ml.beaconEntry(id)
	}
	if c.Provider != provider || c.APIKey == "" {
		return webEntry{}, ErrNoWebSearch
	}
	switch provider {
	case "giphy":
		var res struct {
			Data giphyItem `json:"data"`
		}
		if err := ml.apiGet("giphy", "/v1/gifs/"+raw, url.Values{"api_key": {c.APIKey}}, &res); err != nil {
			return webEntry{}, err
		}
		e = res.Data.entry()
	case "tenor":
		var res struct {
			Results []tenorItem `json:"results"`
		}
		if err := ml.apiGet("tenor", "/v2/posts", url.Values{"key": {c.APIKey}, "ids": {raw}, "media_filter": {"gif,tinygif"}}, &res); err != nil {
			return webEntry{}, err
		}
		if len(res.Results) == 0 {
			return webEntry{}, ErrWebGIF
		}
		e = res.Results[0].entry()
	}
	if e.full == "" || e.id != id {
		return webEntry{}, ErrWebGIF
	}
	ml.webMu.Lock()
	ml.web[id] = e
	ml.webMu.Unlock()
	return e, nil
}

// webMedia lädt ein GIF aus der Web-Suche, skaliert es und speichert es (einmal je Größe).
func (ml *MediaLib) webMedia(id string) (*Media, error) {
	c := ml.Config()
	if !c.GifsEnabled {
		return nil, ErrGifsOff
	}
	e, err := ml.webEntryFor(id)
	if err != nil {
		return nil, err
	}
	provider, raw, _ := strings.Cut(id, ":")
	file := fmt.Sprintf("%s_%s-%d.gif", provider, strings.ReplaceAll(raw, ":", "_"), c.GifWidth)
	path := filepath.Join(ml.Dir(), "web", file)
	media := &Media{Kind: "gif", Src: "/media/web/" + file, Title: e.title, Source: provider, Credit: e.credit}

	ml.fileMu.Lock() // dasselbe GIF nicht doppelt verarbeiten
	defer ml.fileMu.Unlock()
	if data, err := os.ReadFile(path); err == nil {
		if cfg, err := gifConfig(data); err == nil {
			media.W, media.H = cfg.Width, cfg.Height
			return media, nil
		}
	}
	data, err := ml.fetch(e.full, maxGIFUpload)
	if err != nil {
		return nil, ErrWebGIF
	}
	img, err := ml.process(data, c.GifWidth, false, true)
	if err != nil {
		return nil, ErrWebGIF
	}
	if err := ml.writeMedia("web", file, img.Data); err != nil {
		return nil, err
	}
	media.W, media.H = img.W, img.H
	return media, nil
}

// ResolveGIF macht aus einer Auswahl im Chat ("lib:g_…" oder "giphy:…") das Bild der Nachricht.
func (ml *MediaLib) ResolveGIF(ref string) (*Media, error) {
	if !ml.Config().GifsEnabled {
		return nil, ErrGifsOff
	}
	if id, ok := strings.CutPrefix(ref, "lib:"); ok {
		return ml.libMedia(id)
	}
	return ml.webMedia(ref)
}

// previewURL liefert die Vorschauadresse eines Treffers der letzten Suche.
func (ml *MediaLib) previewURL(id string) (string, bool) {
	ml.webMu.Lock()
	defer ml.webMu.Unlock()
	e, ok := ml.web[id]
	if !ok || e.preview == "" {
		return "", false
	}
	return e.preview, true
}
