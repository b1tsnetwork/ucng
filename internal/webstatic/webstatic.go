// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webstatic liefert die eingebetteten Dateien der Oberfläche aus – komprimiert,
// mit ETag und mit versionierten Adressen:
//
//   - Beim Start wird jede Datei einmal gelesen, ihr Inhalt gehasht (ETag) und Text einmal
//     mit gzip gepackt. Danach kostet eine Auslieferung keine Rechenzeit mehr.
//   - In HTML-Seiten bekommen Verweise auf eigene Dateien „?v=<Hash>“. Solche Adressen ändern
//     sich mit jedem Inhalt und dürfen deshalb ein Jahr lang im Browser bleiben (immutable).
//   - Alles andere wird bei jedem Aufruf kurz nachgefragt (ETag → 304 ohne Inhalt).
package webstatic

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
)

type file struct {
	data, gz []byte
	etag     string
	ctype    string
	hash     string
}

// Handler liefert die Dateien eines Dateisystems aus.
type Handler struct {
	files map[string]*file
	// Cache für Dateien ohne Version (Standard: bei jedem Aufruf nachfragen).
	CacheFor func(name string) string
}

var refRe = regexp.MustCompile(`((?:src|href)=")(/[^"?#]+)(")`)

func compressible(ct string) bool {
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "javascript") || strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg") || strings.Contains(ct, "xml")
}

// New liest alle Dateien aus fsys ein.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{files: map[string]*file{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = http.DetectContentType(b)
		}
		h.files[p] = &file{data: b, ctype: ct}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, f := range h.files {
		f.hash = hashOf(f.data)
	}
	// HTML: Verweise auf eigene Dateien versionieren (danach erst den eigenen Hash bilden).
	for name, f := range h.files {
		if strings.HasSuffix(name, ".html") {
			f.data = refRe.ReplaceAllFunc(f.data, func(m []byte) []byte {
				sub := refRe.FindSubmatch(m)
				if t, ok := h.files[strings.TrimPrefix(string(sub[2]), "/")]; ok {
					return []byte(string(sub[1]) + string(sub[2]) + "?v=" + t.hash + string(sub[3]))
				}
				return m
			})
			f.hash = hashOf(f.data)
		}
	}
	for _, f := range h.files {
		f.etag = `"` + f.hash + `"`
		if compressible(f.ctype) && len(f.data) > 512 {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(f.data)
			zw.Close()
			if buf.Len() < len(f.data) {
				f.gz = buf.Bytes()
			}
		}
	}
	return h, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Version liefert den Hash einer Datei (für Verweise aus dynamischen Antworten).
func (h *Handler) Version(name string) string {
	if f, ok := h.files[name]; ok {
		return f.hash
	}
	return ""
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || strings.HasSuffix(r.URL.Path, "/") {
		name = path.Join(name, "index.html")
	}
	if strings.HasSuffix(name, "/index.html") || name == "index.html" {
		if strings.HasSuffix(r.URL.Path, "/index.html") { // wie http.FileServer: auf das Verzeichnis umleiten
			http.Redirect(w, r, strings.TrimSuffix(r.URL.Path, "index.html"), http.StatusMovedPermanently)
			return
		}
	}
	h.Serve(w, r, name)
}

// Serve liefert eine bestimmte Datei aus (z. B. admin.html unter /admin).
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, name string) {
	f, ok := h.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", f.ctype)
	hd.Set("ETag", f.etag)
	hd.Add("Vary", "Accept-Encoding")
	switch {
	case r.URL.Query().Get("v") == f.hash:
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	case h.CacheFor != nil && h.CacheFor(name) != "":
		hd.Set("Cache-Control", h.CacheFor(name))
	case hd.Get("Cache-Control") == "":
		hd.Set("Cache-Control", "no-cache") // speichern, aber vor Gebrauch nachfragen (ETag)
	}
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatch(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.data
	if f.gz != nil && acceptsGzip(r) {
		hd.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	if r.Method == http.MethodHead {
		hd.Set("Content-Length", itoa(len(body)))
		return
	}
	hd.Set("Content-Length", itoa(len(body)))
	w.Write(body)
}

func etagMatch(header, etag string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "W/"))
		if p == etag || p == "*" {
			return true
		}
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, p := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(p), ";")
		if strings.TrimSpace(enc) == "gzip" && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
