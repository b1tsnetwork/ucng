// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ucng/internal/i18n"
)

// Übersetzungen des Beacons: offizielle Sprachdateien in web/i18n/*.json (eingebettet),
// Änderungen aus der Verwaltung in DATA_DIR/i18n.json – sie bleiben bei Updates erhalten.

var beaconScopes = map[string][]string{
	"beacon": {"beacon.", "i18ned.", "common."},
}

func officialCatalog() (*i18n.Catalog, error) {
	static, _ := fs.Sub(webFiles, "web")
	return i18n.New(static, "i18n")
}

// openCatalog lädt die Sprachdateien und die gespeicherten Änderungen; jede Änderung wird sofort gesichert.
func openCatalog(dir string) (*i18n.Catalog, error) {
	cat, err := officialCatalog()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "i18n.json")
	var st i18n.State
	if err := readJSON(path, &st); err != nil {
		// Kaputte Datei beiseitelegen, damit die nächste Änderung sie nicht überschreibt.
		bad := path + ".kaputt-" + time.Now().Format("20060102-150405")
		os.Rename(path, bad)
		log.Printf("Sprachen: %v – es gelten die offiziellen Texte, die alte Datei liegt unter %s", err, bad)
		st = i18n.State{}
	}
	if st.Default != "" || len(st.Texts) > 0 || len(st.Custom) > 0 {
		cat.Restore(st)
	}
	var mu sync.Mutex
	cat.OnChange(func() {
		mu.Lock()
		defer mu.Unlock()
		b, _ := json.MarshalIndent(cat.Export(), "", " ")
		if err := writeFileAtomic(path, b); err != nil {
			log.Printf("Sprachen: %s nicht gespeichert: %v", path, err)
		}
	})
	return cat, nil
}

func (s *Server) i18nAPI() *i18n.API {
	return &i18n.API{
		Cat:       func() *i18n.Catalog { return s.i18n },
		App:       "UCNG Beacon",
		Scopes:    beaconScopes,
		Order:     []string{"beacon"},
		WriteJSON: writeJSON,
		WriteErr:  writeErr,
		Decode:    decodeJSON,
	}
}

// withLang: Fehlermeldungen in der Sprache der Anfrage (X-Lang, Cookie, Accept-Language).
func (s *Server) withLang(next http.Handler) http.Handler {
	return i18n.Middleware(func() *i18n.Catalog { return s.i18n })(next)
}
