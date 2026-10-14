// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package webstatic

import (
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	js := strings.Repeat("console.log('hallo');\n", 100)
	fsys := fstest.MapFS{
		"index.html":     {Data: []byte(`<script src="/app.js"></script><link href="/x.css"><img src="/fehlt.png"><a href="https://example.org/a.js">`)},
		"app.js":         {Data: []byte(js)},
		"x.css":          {Data: []byte("a{}")},
		"sub/index.html": {Data: []byte("sub")},
	}
	h, err := New(fsys)
	if err != nil {
		t.Fatal(err)
	}
	get := func(url string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", url, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	idx := get("/", nil)
	body := idx.Body.String()
	v := h.Version("app.js")
	if !strings.Contains(body, `src="/app.js?v=`+v+`"`) || !strings.Contains(body, `href="/x.css?v=`) || !strings.Contains(body, `src="/fehlt.png"`) || !strings.Contains(body, `https://example.org/a.js"`) {
		t.Fatalf("Verweise: %s", body)
	}
	if idx.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("HTML-Cache: %s", idx.Header().Get("Cache-Control"))
	}
	r := get("/app.js?v="+v, map[string]string{"Accept-Encoding": "gzip, br"})
	if r.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || r.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("versioniert: %v", r.Header())
	}
	zr, _ := gzip.NewReader(r.Body)
	if b, _ := io.ReadAll(zr); string(b) != js {
		t.Fatal("gzip-Inhalt")
	}
	if r2 := get("/app.js", map[string]string{"If-None-Match": r.Header().Get("ETag")}); r2.Code != 304 {
		t.Fatalf("304: %d", r2.Code)
	}
	if r3 := get("/app.js?v=alt", nil); r3.Header().Get("Cache-Control") != "no-cache" || r3.Body.String() != js {
		t.Fatal("alte Version")
	}
	if get("/gibtsnicht.js", nil).Code != 404 || get("/../x.css", nil).Code != 200 {
		t.Fatal("Pfade")
	}
	if get("/sub/", nil).Body.String() != "sub" || get("/sub/index.html", nil).Code != 301 {
		t.Fatal("Verzeichnis")
	}
}
