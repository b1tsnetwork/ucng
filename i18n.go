// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"sync"

	"ucng/internal/i18n"
)

// Übersetzungen: offizielle Sprachdateien in web/i18n/*.json (eingebettet), Änderungen der Admins
// im Dokument i18n.json (bleiben bei Updates erhalten). Siehe internal/i18n.

var (
	baseCatalogOnce sync.Once
	baseCatalog     *i18n.Catalog
	baseCatalogErr  error
)

// newCatalog liefert einen Katalog ohne Admin-Änderungen (die offiziellen Dateien werden nur einmal gelesen).
func newCatalog() *i18n.Catalog {
	baseCatalogOnce.Do(func() {
		static, _ := fs.Sub(webFiles, "web")
		baseCatalog, baseCatalogErr = i18n.New(static, "i18n")
	})
	if baseCatalogErr != nil {
		panic("Sprachdateien: " + baseCatalogErr.Error())
	}
	return baseCatalog.Fresh()
}

// Bereiche der Oberfläche → Schlüssel-Präfixe (es wird nur geliefert, was die Seite braucht).
var i18nScopes = map[string][]string{
	"app":    {"app.", "acct.", "fmt.", "common."},
	"admin":  {"admin.", "i18ned.", "fmt.", "common."},
	"widget": {"widget.", "fmt.", "common."},
}

func (s *Server) i18nAPI() *i18n.API {
	return &i18n.API{
		Cat: s.hub.I18n, App: "UCNG", Scopes: i18nScopes, Order: []string{"app", "admin", "widget"},
		WriteJSON: writeJSON, WriteErr: writeErr, Decode: decodeJSON,
		Audit: func(r *http.Request, action, detail string) { s.log(r, AuditEntry{Action: action, Detail: detail}) },
	}
}

// withLang: Sprache je Anfrage, damit writeErr/writeJSON Meldungen übersetzen können.
func (s *Server) withLang(next http.Handler) http.Handler {
	return i18n.Middleware(s.hub.I18n)(next)
}

// tr übersetzt eine Meldung des Servers in die Sprache der Anfrage (w aus withLang).
func tr(w http.ResponseWriter, msg string) string { return i18n.Tr(w, msg) }

// trf: wie tr, aber mit Go-Format (der Schlüssel ist das Format, z. B. "Bitte noch %d s warten").
func trf(w http.ResponseWriter, format string, args ...any) string {
	return tr(w, fmt.Sprintf(format, args...))
}

// langOf: Sprache einer Anfrage (z. B. für E-Mails).
func (s *Server) langOf(r *http.Request) string { return s.hub.I18n().Resolve(r) }

// publicTr übersetzt Texte, die alle sehen (z. B. Antworten des Befehls-Bots im Kanal),
// in die Standardsprache der Instanz.
func (s *Server) publicTr() func(string) string {
	cat := s.hub.I18n()
	def, _ := cat.Settings()
	return func(msg string) string { return cat.Message(def, msg) }
}

// translatePayload übersetzt "error", "problems" und Hinweise (ephemeral notice) in Antworten.
func translatePayload(w http.ResponseWriter, v any) any {
	if _, ok := w.(*i18n.Writer); !ok {
		return v
	}
	switch m := v.(type) {
	case map[string]string:
		return i18n.TranslateError(w, m)
	case map[string]any:
		changed := false
		out := make(map[string]any, len(m))
		for k, x := range m {
			out[k] = x
		}
		if e, ok := m["error"].(string); ok {
			out["error"], changed = tr(w, e), true
		}
		if p, ok := m["problems"].([]string); ok {
			tp := make([]string, len(p))
			for i, x := range p {
				tp[i] = tr(w, x)
			}
			out["problems"], changed = tp, true
		}
		if e, ok := m["ephemeral"].(EphemeralReply); ok && e.Notice {
			e.Text = tr(w, e.Text)
			out["ephemeral"], changed = e, true
		}
		if changed {
			return out
		}
	}
	return v
}
