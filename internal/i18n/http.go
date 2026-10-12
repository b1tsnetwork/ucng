// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package i18n

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// --- Sprache je Anfrage ---

// Writer merkt sich Anfrage und Katalog, damit Antworten (Fehlermeldungen) übersetzt werden können.
type Writer struct {
	http.ResponseWriter
	r    *http.Request
	cat  *Catalog
	lang string
}

func (lw *Writer) Flush() {
	if f, ok := lw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (lw *Writer) Unwrap() http.ResponseWriter { return lw.ResponseWriter }

// Language: Sprache der Anfrage (wird beim ersten Zugriff bestimmt).
func (lw *Writer) Language() string {
	if lw.lang == "" {
		lw.lang = lw.cat.Resolve(lw.r)
	}
	return lw.lang
}

// Catalog des Writers.
func (lw *Writer) Catalog() *Catalog { return lw.cat }

// Middleware legt um jede Antwort einen Writer (äußerste Schicht, vor CORS usw.).
func Middleware(cat func() *Catalog) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&Writer{ResponseWriter: w, r: r, cat: cat()}, r)
		})
	}
}

// Tr übersetzt eine deutsche Server-Meldung in die Sprache der Anfrage (w aus Middleware).
func Tr(w http.ResponseWriter, msg string) string {
	if lw, ok := w.(*Writer); ok {
		return lw.cat.Message(lw.Language(), msg)
	}
	return msg
}

// TranslateError übersetzt "error" in einer einfachen Fehlerantwort.
func TranslateError(w http.ResponseWriter, v any) any {
	if m, ok := v.(map[string]string); ok {
		if e, ok := m["error"]; ok {
			out := make(map[string]string, len(m))
			for k, x := range m {
				out[k] = x
			}
			out["error"] = Tr(w, e)
			return out
		}
	}
	return v
}

// --- HTTP-Schnittstelle (Oberfläche und Verwaltung) ---

// API stellt die Texte für die Oberfläche bereit und den Editor für die Verwaltung.
// Chat und Beacon nutzen sie gleichermaßen; nur Ausgabe, Prüfung und Protokoll kommen vom Programm.
type API struct {
	Cat       func() *Catalog
	App       string              // Name im Übersetzungspaket, z. B. "UCNG"
	Scopes    map[string][]string // Bereich → Schlüssel-Präfixe; der erste Eintrag in Order ist der Standard
	Order     []string
	WriteJSON func(w http.ResponseWriter, status int, v any)
	WriteErr  func(w http.ResponseWriter, status int, msg string)
	Decode    func(w http.ResponseWriter, r *http.Request, v any) bool
	Audit     func(r *http.Request, action, detail string) // optional
}

// Register hängt die Routen ein. admin schützt die Verwaltung, public (optional) z. B. CORS.
//
//	GET  /api/i18n/boot.js?scope=…         window.I18N für die Seite
//	GET  /api/i18n/dict?scope=…&lang=…    dasselbe als JSON (Widget auf fremden Seiten)
//	GET  /api/i18n/languages              sichtbare Sprachen
//	…    /api/admin/i18n/…                Editor
func (a *API) Register(mux *http.ServeMux, admin, public func(http.HandlerFunc) http.HandlerFunc) {
	if public == nil {
		public = func(h http.HandlerFunc) http.HandlerFunc { return h }
	}
	mux.HandleFunc("GET /api/i18n/boot.js", a.handleBoot)
	mux.HandleFunc("GET /api/i18n/dict", public(a.handleDict))
	mux.HandleFunc("GET /api/i18n/languages", public(a.handleLanguages))
	mux.HandleFunc("GET /api/admin/i18n", admin(a.handleAdmin))
	mux.HandleFunc("PUT /api/admin/i18n/settings", admin(a.handleSettings))
	mux.HandleFunc("POST /api/admin/i18n/languages", admin(a.handleAdd))
	mux.HandleFunc("PATCH /api/admin/i18n/languages/{code}", admin(a.handleMeta))
	mux.HandleFunc("DELETE /api/admin/i18n/languages/{code}", admin(a.handleDelete))
	mux.HandleFunc("GET /api/admin/i18n/texts/{code}", admin(a.handleEntries))
	mux.HandleFunc("PUT /api/admin/i18n/texts/{code}", admin(a.handleSet))
	mux.HandleFunc("GET /api/admin/i18n/file/{code}", admin(a.handleFile))
	mux.HandleFunc("POST /api/admin/i18n/file/{code}", admin(a.handleImport))
	mux.HandleFunc("GET /api/admin/i18n/package/{code}", admin(a.handlePackage))
}

func (a *API) audit(r *http.Request, action, detail string) {
	if a.Audit != nil {
		a.Audit(r, action, detail)
	}
}

type langItem struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Payload: Texte eines Bereichs in einer Sprache (für boot.js und dict).
func (a *API) Payload(lang, scope string) map[string]any {
	cat := a.Cat()
	m := cat.Meta(lang)
	langs := []langItem{}
	for _, l := range cat.Languages(false) {
		langs = append(langs, langItem{l.Code, l.Name})
	}
	def, _ := cat.Settings()
	return map[string]any{"lang": lang, "locale": m.Locale, "dir": m.Dir, "langs": langs, "default": def,
		"dict": cat.Dict(lang, a.Scopes[scope]...)}
}

func (a *API) scopeOf(r *http.Request) string {
	sc := r.URL.Query().Get("scope")
	if _, ok := a.Scopes[sc]; !ok && len(a.Order) > 0 {
		sc = a.Order[0]
	}
	return sc
}

// GET /api/i18n/boot.js?scope=… – setzt window.I18N vor den Skripten der Seite.
func (a *API) handleBoot(w http.ResponseWriter, r *http.Request) {
	cat := a.Cat()
	lang := cat.Resolve(r)
	scope := a.scopeOf(r)
	etag := `"` + cat.Version(lang) + "-" + scope + `"`
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Cookie, Accept-Language")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, _ := json.Marshal(a.Payload(lang, scope))
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write([]byte("window.I18N=" + string(b) + ";\n"))
}

// GET /api/i18n/dict?scope=…&lang=…
func (a *API) handleDict(w http.ResponseWriter, r *http.Request) {
	cat := a.Cat()
	lang := ""
	if l := r.URL.Query().Get("lang"); l != "" && cat.Has(l) {
		lang = l
	}
	if lang == "" {
		lang = cat.Resolve(r)
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	a.WriteJSON(w, http.StatusOK, a.Payload(lang, a.scopeOf(r)))
}

// GET /api/i18n/languages
func (a *API) handleLanguages(w http.ResponseWriter, r *http.Request) {
	a.WriteJSON(w, http.StatusOK, a.Cat().Languages(false))
}

// GET /api/admin/i18n
func (a *API) handleAdmin(w http.ResponseWriter, r *http.Request) {
	cat := a.Cat()
	def, auto := cat.Settings()
	a.WriteJSON(w, http.StatusOK, map[string]any{"languages": cat.Languages(true), "default": def, "auto": auto, "source": Source,
		"packs": cat.PackSources(), "total": cat.Total()})
}

// PUT /api/admin/i18n/settings {default, auto, hidden}
func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Default string   `json:"default"`
		Auto    bool     `json:"auto"`
		Hidden  []string `json:"hidden"`
	}
	if !a.Decode(w, r, &in) {
		return
	}
	if err := a.Cat().SetSettings(in.Default, in.Auto, in.Hidden); err != nil {
		a.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.audit(r, "i18n.settings", in.Default)
	a.handleAdmin(w, r)
}

// POST /api/admin/i18n/languages {code, name, locale, dir}
func (a *API) handleAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code   string `json:"code"`
		Name   string `json:"name"`
		Locale string `json:"locale"`
		Dir    string `json:"dir"`
	}
	if !a.Decode(w, r, &in) {
		return
	}
	cat := a.Cat()
	if err := cat.AddLanguage(in.Code, in.Name, in.Locale); err != nil {
		a.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Dir != "" {
		cat.SetMeta(in.Code, Meta{Name: in.Name, Locale: in.Locale, Dir: in.Dir})
	}
	a.audit(r, "i18n.language", in.Code+" "+in.Name)
	a.handleAdmin(w, r)
}

// PATCH /api/admin/i18n/languages/{code} {name, locale, dir}
func (a *API) handleMeta(w http.ResponseWriter, r *http.Request) {
	var in Meta
	if !a.Decode(w, r, &in) {
		return
	}
	if err := a.Cat().SetMeta(r.PathValue("code"), in); err != nil {
		a.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.handleAdmin(w, r)
}

// DELETE /api/admin/i18n/languages/{code}
func (a *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.Cat().DeleteLanguage(r.PathValue("code")); err != nil {
		a.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.audit(r, "i18n.delete", r.PathValue("code"))
	a.handleAdmin(w, r)
}

// GET /api/admin/i18n/texts/{code}?scope=… – optional nur die Texte eines Bereichs
func (a *API) handleEntries(w http.ResponseWriter, r *http.Request) {
	list, err := a.Cat().Entries(r.PathValue("code"))
	if err != nil {
		a.WriteErr(w, http.StatusNotFound, err.Error())
		return
	}
	a.WriteJSON(w, http.StatusOK, list)
}

// PUT /api/admin/i18n/texts/{code} {key, text, confirm} – text leer = auf das Original zurücksetzen
func (a *API) handleSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key     string `json:"key"`
		Text    string `json:"text"`
		Confirm bool   `json:"confirm"` // veraltete Änderung als geprüft markieren
	}
	if !a.Decode(w, r, &in) {
		return
	}
	cat := a.Cat()
	if in.Confirm {
		if err := cat.Confirm(r.PathValue("code"), in.Key); err != nil {
			a.WriteErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.WriteJSON(w, http.StatusOK, map[string]any{"warnings": []string{}})
		return
	}
	warns, err := cat.SetText(r.PathValue("code"), in.Key, in.Text)
	if err != nil {
		a.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for i, x := range warns {
		warns[i] = Tr(w, x)
	}
	if warns == nil {
		warns = []string{}
	}
	a.WriteJSON(w, http.StatusOK, map[string]any{"warnings": warns})
}

// GET /api/admin/i18n/file/{code}?full=0 – Sprachdatei herunterladen (full=0: nur Änderungen)
func (a *API) handleFile(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	full := r.URL.Query().Get("full") != "0"
	b, err := a.Cat().File(code, full)
	if err != nil {
		a.WriteErr(w, http.StatusNotFound, err.Error())
		return
	}
	name := code + ".json"
	if !full {
		name = code + "-changes.json"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(b)
}

// POST /api/admin/i18n/file/{code}?replace=1 – Sprachdatei als Änderungen übernehmen
func (a *API) handleImport(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		a.WriteErr(w, http.StatusRequestEntityTooLarge, "Die Sprachdatei ist zu groß (höchstens 4 MB)")
		return
	}
	res, err := a.Cat().Import(r.PathValue("code"), data, r.URL.Query().Get("replace") == "1")
	if err != nil {
		a.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for i, e := range res.Errors {
		k, msg, _ := strings.Cut(e, ": ")
		res.Errors[i] = k + ": " + Tr(w, msg)
	}
	a.audit(r, "i18n.import", fmt.Sprintf("%s: %d", r.PathValue("code"), res.Applied))
	a.WriteJSON(w, http.StatusOK, res)
}

// GET /api/admin/i18n/package/{code}?open=1 – Übersetzungspaket (für KI oder Übersetzer);
// open=1: nur fehlende und veraltete Texte.
func (a *API) handlePackage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	open := r.URL.Query().Get("open") == "1"
	b, err := a.Cat().Package(code, a.App, open)
	if err != nil {
		a.WriteErr(w, http.StatusNotFound, err.Error())
		return
	}
	name := code + "-translation.json"
	if open {
		name = code + "-translation-open.json"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(b)
}
