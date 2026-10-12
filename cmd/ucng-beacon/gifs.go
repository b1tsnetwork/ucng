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
	"strconv"
	"strings"
	"time"

	"ucng/internal/imgscale"
)

// GIF-Hub: Sammlungen von GIFs, die Instanzen abonnieren können.
//
// Öffentlich:  GET /api/v1/collections, GET /api/v1/gifs?q=&collection=&limit=&offset=,
//              GET /api/v1/gifs/{id}, GET /media/{datei}
// Eine Instanz holt sich die Treffer, lädt das GIF beim Senden einmal herunter und skaliert es
// auf ihre eigene Größe – Besucher der Instanz verbinden sich nicht mit dem Beacon.

const (
	maxGIFUpload = 20 << 20
	previewBox   = 200 // Vorschauen im GIF-Fenster der Chats – klein und schnell geladen
)

var (
	fileRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,120}$`)
	webIDRe   = regexp.MustCompile(`^(giphy|tenor):[A-Za-z0-9_-]{1,64}$`)
	errNoGIF  = errors.New("Dieses GIF gibt es nicht")
	errNoColl = errors.New("Diese Sammlung gibt es nicht")
)

type GIFView struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Collections []string `json:"collections"`
	W           int      `json:"w"`
	H           int      `json:"h"`
	URL         string   `json:"url"`
	Preview     string   `json:"preview,omitempty"` // kleine Fassung (max. 200 px) für Vorschauen
	Credit      string   `json:"credit"`            // Quellenangabe für Chats, die das GIF senden
}

func (s *Server) view(r *http.Request, g GIF) GIFView {
	v := GIFView{ID: g.ID, Title: g.Title, Tags: g.Tags, Collections: g.Collections, W: g.W, H: g.H, URL: s.baseURL(r) + "/media/" + g.File, Credit: s.credit()}
	if g.Preview != "" {
		v.Preview = s.baseURL(r) + "/media/" + g.Preview
	}
	return v
}

// credit: Quellenangabe an GIFs im Chat (in der Verwaltung einstellbar).
func (s *Server) credit() string {
	c := s.store.Config()
	if c.Credit != "" {
		return c.Credit
	}
	return "via " + c.Name
}

type CollectionView struct {
	Collection
	Count   int    `json:"count"`
	Preview string `json:"preview,omitempty"`
}

// GET /api/v1/collections
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	gifs := s.store.GIFList()
	out := []CollectionView{}
	for _, c := range s.store.Collections() {
		v := CollectionView{Collection: c}
		for _, g := range gifs {
			if contains(g.Collections, c.ID) {
				v.Count++
				if v.Preview == "" {
					v.Preview = s.baseURL(r) + "/media/" + g.File
				}
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// GET /api/v1/gifs?q=&collection=a,b&limit=&offset=
func (s *Server) handleGIFs(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	var colls []string
	for _, c := range strings.Split(r.URL.Query().Get("collection"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			colls = append(colls, c)
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	if offset < 0 {
		offset = 0
	}
	type hit struct {
		g     GIF
		score int
	}
	var hits []hit
	for _, g := range s.store.GIFList() {
		if len(colls) > 0 {
			in := false
			for _, c := range colls {
				if contains(g.Collections, c) {
					in = true
				}
			}
			if !in {
				continue
			}
		}
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
	// neueste zuerst (Liste ist schon so sortiert), bessere Treffer davor
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].score > hits[j-1].score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	total := len(hits)
	out := []GIFView{}
	for i := offset; i < len(hits) && len(out) < limit; i++ {
		out = append(out, s.view(r, hits[i].g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"gifs": out, "total": total})
}

// GET /api/v1/gifs/{id}
func (s *Server) handleGIF(w http.ResponseWriter, r *http.Request) {
	g, ok := s.store.GetGIF(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errNoGIF.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(r, g))
}

// GET /media/{file}
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !fileRe.MatchString(file) || strings.HasPrefix(file, ".") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.store.dir, "media", file)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	http.ServeFile(w, r, path)
}

// --- Verwaltung: Sammlungen ---

func (s *Server) handleCollectionCreate(w http.ResponseWriter, r *http.Request) {
	var in Collection
	if !decodeJSON(w, r, &in) {
		return
	}
	name := cleanText(in.Name, 50)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "Name fehlt")
		return
	}
	c := Collection{ID: "c_" + randomHex(4), Name: name, Description: cleanText(in.Description, 200), Created: time.Now().UTC()}
	s.store.mu.Lock()
	s.store.collections = append(s.store.collections, c)
	err := s.store.saveLocked("gifs.json")
	s.store.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleCollectionUpdate(w http.ResponseWriter, r *http.Request) {
	var in Collection
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	s.store.mu.Lock()
	var out *Collection
	for i := range s.store.collections {
		c := &s.store.collections[i]
		if c.ID == id {
			if n := cleanText(in.Name, 50); n != "" {
				c.Name = n
			}
			c.Description = cleanText(in.Description, 200)
			s.store.saveLocked("gifs.json")
			cp := *c
			out = &cp
			break
		}
	}
	s.store.mu.Unlock()
	if out == nil {
		writeErr(w, http.StatusNotFound, errNoColl.Error())
		return
	}
	writeJSON(w, http.StatusOK, *out)
}

func (s *Server) handleCollectionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.store.deleteCollection(id) {
		writeErr(w, http.StatusNotFound, errNoColl.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteCollection entfernt eine Sammlung; die GIFs bleiben und verlieren nur die Zuordnung.
func (st *Store) deleteCollection(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	var keep []Collection
	found := false
	for _, c := range st.collections {
		if c.ID == id {
			found = true
			continue
		}
		keep = append(keep, c)
	}
	if !found {
		return false
	}
	st.collections = keep
	for _, g := range st.gifs {
		rest := []string{}
		for _, c := range g.Collections {
			if c != id {
				rest = append(rest, c)
			}
		}
		g.Collections = rest
	}
	st.saveLocked("gifs.json")
	return true
}

// --- Verwaltung: GIFs ---

func cleanTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.ToLower(cleanText(t, 32))
		if t != "" && !seen[t] && len(out) < 20 {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// addGIF skaliert ein GIF und legt es an.
func (s *Server) addGIF(data []byte, title string, tags, colls []string, source string) (GIF, error) {
	if len(data) > maxGIFUpload {
		return GIF{}, errors.New("Die Datei ist zu groß (höchstens 20 MB)")
	}
	img, err := imgscale.ProcessImage(data, s.cfg.GifBox, s.cfg.GifBox, false, true)
	if err != nil {
		return GIF{}, err
	}
	id := "b_" + randomHex(5)
	g := GIF{ID: id, File: fmt.Sprintf("%s-%d.gif", id, s.cfg.GifBox), Original: id + ".bin", Title: cleanText(title, 80),
		Tags: cleanTags(tags), W: img.W, H: img.H, Source: source, Created: time.Now().UTC()}
	if err := writeFileAtomic(filepath.Join(s.store.dir, "media", g.File), img.Data); err != nil {
		return GIF{}, err
	}
	if writeFileAtomic(filepath.Join(s.store.dir, "originals", g.Original), data) != nil {
		g.Original = ""
	}
	g.Preview = s.makePreview(id, img.W, img.H, img.Data)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	g.Collections = []string{}
	for _, c := range colls {
		if s.store.collectionExistsLocked(c) && !contains(g.Collections, c) {
			g.Collections = append(g.Collections, c)
		}
	}
	s.store.gifs[id] = &g
	return g, s.store.saveLocked("gifs.json")
}

// POST /api/admin/gifs – multipart (file, title, tags, collections) oder JSON {url | web, title, tags, collections}
func (s *Server) handleGIFAdd(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var title, source string
	var tags, colls []string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, maxGIFUpload+1<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "Die Datei ist zu groß (höchstens 20 MB)")
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Datei fehlt")
			return
		}
		data, err = io.ReadAll(io.LimitReader(f, maxGIFUpload+1))
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		title, source = r.FormValue("title"), "upload"
		tags = strings.Split(r.FormValue("tags"), ",")
		colls = strings.Split(r.FormValue("collections"), ",")
	} else {
		var in struct {
			URL         string   `json:"url"`
			Web         string   `json:"web"` // giphy:<id> oder tenor:<id>
			Title       string   `json:"title"`
			Tags        []string `json:"tags"`
			Collections []string `json:"collections"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		title, tags, colls = in.Title, in.Tags, in.Collections
		var err error
		switch {
		case in.Web != "":
			var e webEntry
			if e, err = s.webEntry(in.Web); err == nil {
				if title == "" {
					title = e.Title
				}
				data, err = s.fetch(e.Full, maxGIFUpload)
				source = strings.Split(in.Web, ":")[0]
			}
		case in.URL != "":
			data, err = s.fetch(in.URL, maxGIFUpload)
			source = "url"
		default:
			err = errors.New("Datei, Adresse oder Suchtreffer fehlt")
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	g, err := s.addGIF(data, title, tags, colls, source)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.view(r, g))
}

func (s *Server) handleGIFUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title       string   `json:"title"`
		Tags        []string `json:"tags"`
		Collections []string `json:"collections"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	// Unter der Sperre nur ändern und speichern; die Antwort (view → credit → Config) braucht
	// die Sperre selbst wieder und wird deshalb erst danach gebaut.
	s.store.mu.Lock()
	g, ok := s.store.gifs[r.PathValue("id")]
	var out GIF
	if ok {
		g.Title, g.Tags = cleanText(in.Title, 80), cleanTags(in.Tags)
		g.Collections = []string{}
		for _, c := range in.Collections {
			if s.store.collectionExistsLocked(c) && !contains(g.Collections, c) {
				g.Collections = append(g.Collections, c)
			}
		}
		s.store.saveLocked("gifs.json")
		out = *g
	}
	s.store.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, errNoGIF.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.view(r, out))
}

func (s *Server) handleGIFDelete(w http.ResponseWriter, r *http.Request) {
	s.store.mu.Lock()
	g, ok := s.store.gifs[r.PathValue("id")]
	if ok {
		delete(s.store.gifs, g.ID)
		s.store.saveLocked("gifs.json")
	}
	s.store.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, errNoGIF.Error())
		return
	}
	os.Remove(filepath.Join(s.store.dir, "media", g.File))
	if g.Preview != "" {
		os.Remove(filepath.Join(s.store.dir, "media", g.Preview))
	}
	if g.Original != "" {
		os.Remove(filepath.Join(s.store.dir, "originals", g.Original))
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Laden aus dem Web ---

func (s *Server) fetch(raw string, limit int64) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("Bitte eine vollständige Adresse mit https:// angeben")
	}
	req, _ := http.NewRequest("GET", u.String(), nil)
	req.Header.Set("User-Agent", "UCNG-Beacon/1")
	res, err := s.mediaClient().Do(req)
	if err != nil {
		if errors.Is(err, errPrivate) {
			return nil, errPrivate
		}
		return nil, errors.New("Download fehlgeschlagen")
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
		return nil, errors.New("Die Datei ist zu groß")
	}
	return data, nil
}

// mediaClient folgt (anders als der Client für Instanzen) bis zu drei Weiterleitungen.
func (s *Server) mediaClient() *http.Client {
	c := *s.client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("zu viele Weiterleitungen")
		}
		return nil
	}
	return &c
}

// webEntry: ein Treffer bei GIPHY oder Tenor.
type webEntry struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Preview string `json:"preview"`
	Full    string `json:"-"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

var apiBase = map[string]string{"giphy": "https://api.giphy.com", "tenor": "https://tenor.googleapis.com"}

func (s *Server) apiGet(provider, path string, q url.Values, v any) error {
	data, err := s.fetch(apiBase[provider]+path+"?"+q.Encode(), 4<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errors.New("Unerwartete Antwort des GIF-Anbieters")
	}
	return nil
}

type giphyItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Images map[string]struct {
		URL    string `json:"url"`
		Width  string `json:"width"`
		Height string `json:"height"`
	} `json:"images"`
}

func (it giphyItem) entry() webEntry {
	e := webEntry{ID: "giphy:" + it.ID, Title: it.Title}
	for _, k := range []string{"downsized", "fixed_width", "original"} {
		if im, ok := it.Images[k]; ok && im.URL != "" {
			e.Full = im.URL
			break
		}
	}
	for _, k := range []string{"fixed_width_small", "fixed_width"} {
		if im, ok := it.Images[k]; ok && im.URL != "" {
			e.Preview = im.URL
			e.W, _ = strconv.Atoi(im.Width)
			e.H, _ = strconv.Atoi(im.Height)
			break
		}
	}
	return e
}

type tenorItem struct {
	ID           string `json:"id"`
	Description  string `json:"content_description"`
	MediaFormats map[string]struct {
		URL  string `json:"url"`
		Dims []int  `json:"dims"`
	} `json:"media_formats"`
}

func (it tenorItem) entry() webEntry {
	e := webEntry{ID: "tenor:" + it.ID, Title: it.Description}
	if f, ok := it.MediaFormats["gif"]; ok {
		e.Full = f.URL
	}
	if f, ok := it.MediaFormats["tinygif"]; ok {
		e.Preview = f.URL
		if len(f.Dims) == 2 {
			e.W, e.H = f.Dims[0], f.Dims[1]
		}
	}
	if e.Preview == "" {
		e.Preview = e.Full
	}
	return e
}

var tenorFilter = map[string]string{"g": "high", "pg": "medium", "pg-13": "low", "r": "off"}

// GET /api/admin/web?q= – bei GIPHY/Tenor suchen, um GIFs in Sammlungen zu übernehmen
func (s *Server) handleWebSearch(w http.ResponseWriter, r *http.Request) {
	c := s.store.Config()
	if c.Provider == "" || c.APIKey == "" {
		writeErr(w, http.StatusConflict, "Kein GIF-Anbieter eingerichtet (Einstellungen)")
		return
	}
	q := cleanText(r.URL.Query().Get("q"), 60)
	out := []webEntry{}
	switch c.Provider {
	case "giphy":
		params := url.Values{"api_key": {c.APIKey}, "limit": {"30"}, "rating": {c.Rating}}
		path := "/v1/gifs/trending"
		if q != "" {
			params.Set("q", q)
			path = "/v1/gifs/search"
		}
		var res struct {
			Data []giphyItem `json:"data"`
		}
		if err := s.apiGet("giphy", path, params, &res); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		for _, it := range res.Data {
			if e := it.entry(); e.Full != "" {
				out = append(out, e)
			}
		}
	case "tenor":
		params := url.Values{"key": {c.APIKey}, "client_key": {"ucng-beacon"}, "limit": {"30"}, "media_filter": {"gif,tinygif"}, "contentfilter": {tenorFilter[c.Rating]}}
		path := "/v2/featured"
		if q != "" {
			params.Set("q", q)
			path = "/v2/search"
		}
		var res struct {
			Results []tenorItem `json:"results"`
		}
		if err := s.apiGet("tenor", path, params, &res); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		for _, it := range res.Results {
			if e := it.entry(); e.Full != "" {
				out = append(out, e)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// webEntry holt einen einzelnen Treffer beim Anbieter (Adresse nie vom Browser übernehmen).
func (s *Server) webEntry(id string) (webEntry, error) {
	if !webIDRe.MatchString(id) {
		return webEntry{}, errors.New("Ungültiger Treffer")
	}
	c := s.store.Config()
	provider, raw, _ := strings.Cut(id, ":")
	if provider != c.Provider || c.APIKey == "" {
		return webEntry{}, errors.New("Dieser Anbieter ist nicht eingerichtet")
	}
	if provider == "giphy" {
		var res struct {
			Data giphyItem `json:"data"`
		}
		if err := s.apiGet("giphy", "/v1/gifs/"+raw, url.Values{"api_key": {c.APIKey}}, &res); err != nil {
			return webEntry{}, err
		}
		return res.Data.entry(), nil
	}
	var res struct {
		Results []tenorItem `json:"results"`
	}
	if err := s.apiGet("tenor", "/v2/posts", url.Values{"key": {c.APIKey}, "ids": {raw}, "media_filter": {"gif,tinygif"}}, &res); err != nil {
		return webEntry{}, err
	}
	if len(res.Results) == 0 {
		return webEntry{}, errors.New("Nicht gefunden")
	}
	return res.Results[0].entry(), nil
}

// makePreview legt eine kleine Fassung an (aus der bereits skalierten Datei). Ist das GIF ohnehin
// klein, wird keine eigene Vorschau angelegt (leer = das GIF selbst).
func (s *Server) makePreview(id string, w, h int, scaled []byte) string {
	if w <= previewBox && h <= previewBox {
		return ""
	}
	img, err := imgscale.ProcessImage(scaled, previewBox, previewBox, false, true)
	if err != nil {
		return ""
	}
	name := fmt.Sprintf("%s-p%d.gif", id, previewBox)
	if writeFileAtomic(filepath.Join(s.store.dir, "media", name), img.Data) != nil {
		return ""
	}
	return name
}

// backfillPreviews legt Vorschauen für GIFs an, die vor dieser Version hinzugefügt wurden
// (beim Start, im Hintergrund; die Store-Sperre wird nur kurz zum Eintragen genommen).
func (s *Server) backfillPreviews() {
	n := 0
	for _, g := range s.store.GIFList() {
		if g.Preview != "" || (g.W <= previewBox && g.H <= previewBox) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.store.dir, "media", g.File))
		if err != nil {
			continue
		}
		name := s.makePreview(g.ID, g.W, g.H, data)
		if name == "" {
			continue
		}
		s.store.mu.Lock()
		if x, ok := s.store.gifs[g.ID]; ok {
			x.Preview = name
			n++
		}
		s.store.mu.Unlock()
	}
	if n > 0 {
		s.store.mu.Lock()
		s.store.saveLocked("gifs.json")
		s.store.mu.Unlock()
		log.Printf("Vorschauen für %d GIFs angelegt", n)
	}
}
