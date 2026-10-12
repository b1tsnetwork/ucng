// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ucng/internal/i18n"
)

// Sprachpakete für Chat-Server: Das Beacon-Team stellt Übersetzungen der Chat-Oberfläche bereit,
// Instanzen installieren sie unter Admin → Sprachen. Jeder kann ein Übersetzungspaket (Anleitung,
// deutsche Ausgangstexte, Platzhalter) herunterladen – etwa um es von einer KI übersetzen zu lassen.
//
// Die Ausgangstexte des Chats liegen als Kopie in chati18n/ (= web/i18n/ des Chats, ein Test prüft das).

//go:embed chati18n
var chatI18nFiles embed.FS

var (
	chatCatOnce sync.Once
	chatCat     *i18n.Catalog
	chatCatErr  error

	errNoPack   = errors.New("Dieses Sprachpaket gibt es nicht")
	errPackCode = errors.New("Ungültiges Sprachkürzel (z. B. fr oder pt-BR)")
	errPackName = errors.New("Name der Sprache fehlt (höchstens 40 Zeichen)")
	errPackSrc  = errors.New("Die Ausgangssprache (Deutsch) kann kein Sprachpaket sein")
	errPackNone = errors.New("Die Datei enthält keinen gültigen Text")
)

// chatCatalog: offizielle Texte des Chats (ohne Änderungen; für Prüfung und Pakete).
func chatCatalog() *i18n.Catalog {
	chatCatOnce.Do(func() { chatCat, chatCatErr = i18n.New(chatI18nFiles, "chati18n") })
	if chatCatErr != nil {
		panic("Chat-Sprachdateien: " + chatCatErr.Error())
	}
	return chatCat.Fresh()
}

// LangPack ist eine Übersetzung der Chat-Oberfläche.
type LangPack struct {
	Code    string            `json:"code"`
	Meta    i18n.Meta         `json:"meta"`
	Note    string            `json:"note,omitempty"`
	Updated time.Time         `json:"updated"`
	Texts   map[string]string `json:"texts"`
	Bases   map[string]string `json:"bases"` // Bezug je Text beim Hochladen (für „veraltet“)
}

// LangPackView für Listen.
type LangPackView struct {
	Code     string    `json:"code"`
	Name     string    `json:"name"`
	Locale   string    `json:"locale,omitempty"`
	Dir      string    `json:"dir,omitempty"`
	Note     string    `json:"note,omitempty"`
	Updated  time.Time `json:"updated"`
	Texts    int       `json:"texts"`    // Texte, die es im aktuellen Chat noch gibt
	Outdated int       `json:"outdated"` // deutscher Text hat sich seit der Übersetzung geändert
	Total    int       `json:"total"`
	Complete float64   `json:"complete"`
	Official bool      `json:"official"` // im Chat bereits eingebaut (Paket ergänzt/ändert Texte)
}

type PackStore struct {
	mu    sync.RWMutex
	dir   string
	packs map[string]*LangPack
}

func openPacks(dir string) (*PackStore, error) {
	ps := &PackStore{dir: filepath.Join(dir, "langpacks"), packs: map[string]*LangPack{}}
	if err := os.MkdirAll(ps.dir, 0o700); err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(ps.dir, "*.json"))
	for _, f := range files {
		var p LangPack
		if err := readJSON(f, &p); err != nil {
			return nil, err
		}
		if i18n.ValidCode(p.Code) {
			ps.packs[p.Code] = &p
		}
	}
	return ps, nil
}

func (ps *PackStore) saveLocked(p *LangPack) error {
	b, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(ps.dir, p.Code+".json"), b)
}

func (ps *PackStore) Get(code string) (LangPack, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	p, ok := ps.packs[code]
	if !ok {
		return LangPack{}, false
	}
	return *p, true
}

func (ps *PackStore) List() []LangPack {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]LangPack, 0, len(ps.packs))
	for _, p := range ps.packs {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

func cleanMeta(code string, m i18n.Meta) (i18n.Meta, error) {
	m.Name = strings.Join(strings.Fields(m.Name), " ")
	if m.Name == "" || len([]rune(m.Name)) > 40 {
		return m, errPackName
	}
	if m.Locale == "" || !i18n.ValidCode(m.Locale) {
		m.Locale = code
	}
	if m.Dir != "rtl" {
		m.Dir = ""
	}
	return m, nil
}

// Put übernimmt geprüfte Texte (replace: bisherige Texte verwerfen). Unveränderte Texte behalten ihren Bezug.
func (ps *PackStore) Put(code string, meta i18n.Meta, note string, texts map[string]string, replace bool, cat *i18n.Catalog) (LangPack, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.packs[code]
	if !ok {
		p = &LangPack{Code: code, Texts: map[string]string{}, Bases: map[string]string{}}
	}
	np := *p
	np.Meta, np.Note, np.Updated = meta, note, time.Now().UTC()
	np.Texts, np.Bases = map[string]string{}, map[string]string{}
	if !replace {
		for k, v := range p.Texts {
			np.Texts[k], np.Bases[k] = v, p.Bases[k]
		}
	}
	for k, v := range texts {
		if old, had := p.Texts[k]; had && old == v && !replace {
			continue
		}
		np.Texts[k], np.Bases[k] = v, cat.Base(code, k)
	}
	if len(np.Texts) == 0 {
		return LangPack{}, errPackNone
	}
	if err := ps.saveLocked(&np); err != nil {
		return LangPack{}, err
	}
	ps.packs[code] = &np
	return np, nil
}

func (ps *PackStore) SetMeta(code string, meta i18n.Meta, note string) (LangPack, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.packs[code]
	if !ok {
		return LangPack{}, errNoPack
	}
	np := *p
	np.Meta, np.Note = meta, note
	if err := ps.saveLocked(&np); err != nil {
		return LangPack{}, err
	}
	ps.packs[code] = &np
	return np, nil
}

func (ps *PackStore) Delete(code string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, ok := ps.packs[code]; !ok {
		return errNoPack
	}
	delete(ps.packs, code)
	if err := os.Remove(filepath.Join(ps.dir, code+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// view: Stand eines Pakets gegenüber den aktuellen Ausgangstexten.
func packView(p LangPack, cat *i18n.Catalog) LangPackView {
	v := LangPackView{Code: p.Code, Name: p.Meta.Name, Locale: p.Meta.Locale, Dir: p.Meta.Dir, Note: p.Note,
		Updated: p.Updated, Total: cat.Total(), Official: cat.Has(p.Code)}
	src := cat.Dict(i18n.Source)
	for k := range p.Texts {
		if _, ok := src[k]; !ok {
			continue
		}
		v.Texts++
		if p.Bases[k] != cat.Base(p.Code, k) {
			v.Outdated++
		}
	}
	if v.Total > 0 {
		v.Complete = float64(v.Texts) / float64(v.Total)
	}
	return v
}

// packCatalog: Chat-Katalog mit dem Paket als eigener Sprache bzw. als Änderungen (für Übersetzungspakete).
func packCatalog(code string, p *LangPack, meta i18n.Meta) (*i18n.Catalog, error) {
	cat := chatCatalog()
	st := i18n.State{Default: i18n.Source, Auto: true}
	if !cat.Has(code) {
		st.Custom = map[string]i18n.Meta{code: meta}
	}
	if p != nil {
		st.Texts = map[string]map[string]i18n.Override{code: {}}
		for k, v := range p.Texts {
			st.Texts[code][k] = i18n.Override{Text: v, Base: p.Bases[k], At: p.Updated}
		}
	}
	res := cat.ApplyState(st, false)
	if len(st.Custom) > 0 && !cat.Has(code) {
		if len(res.Errors) > 0 {
			return nil, errors.New(res.Errors[0])
		}
		return nil, errPackCode
	}
	return cat, nil
}

// --- HTTP ---

// GET /api/v1/languages – Sprachpakete für Chat-Server
func (s *Server) handleLangPacks(w http.ResponseWriter, r *http.Request) {
	cat := chatCatalog()
	out := []LangPackView{}
	for _, p := range s.packs.List() {
		out = append(out, packView(p, cat))
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": i18n.Source, "total": cat.Total(), "packs": out,
		"official": officialChatLangs(cat)})
}

func officialChatLangs(cat *i18n.Catalog) []string {
	var out []string
	for _, l := range cat.Languages(true) {
		if l.Official {
			out = append(out, l.Code)
		}
	}
	return out
}

// GET /api/v1/languages/{code} – Sprachdatei (_meta + key → Text) zum Installieren
func (s *Server) handleLangPackFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.packs.Get(r.PathValue("code"))
	if !ok {
		writeErr(w, http.StatusNotFound, errNoPack.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Last-Modified", p.Updated.Format(http.TimeFormat))
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+p.Code+`.json"`)
	}
	w.Write(i18n.LangFile(p.Meta, p.Texts))
}

// GET /api/v1/languages/{code}/package?open=1&name=…&locale=… – Übersetzungspaket (auch für neue Sprachen)
func (s *Server) handleLangPackPackage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !i18n.ValidCode(code) {
		writeErr(w, http.StatusBadRequest, errPackCode.Error())
		return
	}
	if code == i18n.Source {
		writeErr(w, http.StatusBadRequest, errPackSrc.Error())
		return
	}
	q := r.URL.Query()
	var pp *LangPack
	meta := i18n.Meta{Name: q.Get("name"), Locale: q.Get("locale"), Dir: q.Get("dir")}
	if p, ok := s.packs.Get(code); ok {
		pp = &p
		meta = p.Meta
	}
	if strings.TrimSpace(meta.Name) == "" {
		meta.Name = code
	}
	meta, err := cleanMeta(code, meta)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cat, err := packCatalog(code, pp, meta)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	open := q.Get("open") == "1"
	b, err := cat.Package(code, "UCNG", open)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := "ucng-" + code + "-translation.json"
	if open {
		name = "ucng-" + code + "-translation-open.json"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(b)
}

// POST /api/admin/langpacks/{code}?replace=1&name=…&locale=…&dir=…&note=… – Datei oder ausgefülltes Paket hochladen
func (s *Server) handleLangPackUpload(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !i18n.ValidCode(code) {
		writeErr(w, http.StatusBadRequest, errPackCode.Error())
		return
	}
	if code == i18n.Source {
		writeErr(w, http.StatusBadRequest, errPackSrc.Error())
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
	if err != nil || len(data) > 8<<20 {
		writeErr(w, http.StatusRequestEntityTooLarge, "Die Datei ist zu groß (höchstens 8 MB)")
		return
	}
	cat := chatCatalog()
	texts, fm, res, err := cat.Validate(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	meta := fm
	old, had := s.packs.Get(code)
	if had && meta.Name == "" {
		meta = old.Meta
	}
	if v := q.Get("name"); v != "" {
		meta.Name = v
	}
	if v := q.Get("locale"); v != "" {
		meta.Locale = v
	}
	if v := q.Get("dir"); v != "" {
		meta.Dir = v
	}
	if meta.Name == "" {
		if m := cat.Meta(code); cat.Has(code) {
			meta = m
		}
	}
	meta, err = cleanMeta(code, meta)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	note := old.Note
	if q.Has("note") {
		note = strings.TrimSpace(q.Get("note"))
	}
	if len([]rune(note)) > 200 {
		note = string([]rune(note)[:200])
	}
	p, err := s.packs.Put(code, meta, note, texts, q.Get("replace") == "1", cat)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for i, e := range res.Errors {
		k, msg, _ := strings.Cut(e, ": ")
		res.Errors[i] = k + ": " + i18n.Tr(w, msg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "pack": packView(p, cat)})
}

// PATCH /api/admin/langpacks/{code} {name, locale, dir, note}
func (s *Server) handleLangPackMeta(w http.ResponseWriter, r *http.Request) {
	var in struct {
		i18n.Meta
		Note string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	code := r.PathValue("code")
	meta, err := cleanMeta(code, in.Meta)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	note := strings.TrimSpace(in.Note)
	if len([]rune(note)) > 200 {
		note = string([]rune(note)[:200])
	}
	p, err := s.packs.SetMeta(code, meta, note)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, packView(p, chatCatalog()))
}

// DELETE /api/admin/langpacks/{code}
func (s *Server) handleLangPackDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.packs.Delete(r.PathValue("code")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
