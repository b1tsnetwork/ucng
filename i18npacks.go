// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ucng/internal/fedsig"
	"ucng/internal/i18n"
)

// Sprachpakete von einem Beacon: Liste abrufen und eine Sprache installieren bzw. aktualisieren.
// Installieren legt eine eigene Sprache an (falls nötig) und übernimmt die Texte wie ein Import;
// die Herkunft wird gemerkt, damit neuere Pakete als „Update verfügbar“ erscheinen.

var (
	ErrNoLangPacks = errors.New("Dieser Beacon bietet keine Sprachpakete an (ältere Version?)")
	ErrPackCode    = errors.New("Ungültiges Sprachkürzel")
)

type beaconLangPack struct {
	Code     string    `json:"code"`
	Name     string    `json:"name"`
	Locale   string    `json:"locale,omitempty"`
	Dir      string    `json:"dir,omitempty"`
	Note     string    `json:"note,omitempty"`
	Updated  time.Time `json:"updated"`
	Texts    int       `json:"texts"`
	Outdated int       `json:"outdated"`
	Total    int       `json:"total"`
	Complete float64   `json:"complete"`
	Official bool      `json:"official"`
	// aus Sicht dieser Instanz
	Installed bool      `json:"installed"`          // Sprache gibt es hier
	FromHere  bool      `json:"fromHere,omitempty"` // aus diesem Beacon installiert
	Update    bool      `json:"update,omitempty"`   // Paket ist neuer als die installierte Fassung
	InstAt    time.Time `json:"installedAt,omitempty"`
}

// Beacons, die diese Instanz kennt (Verzeichnis und GIF-Abos) – als Vorschläge.
func (s *Server) knownBeacons() []string {
	cfg := s.hub.Beacon().Get()
	var out []string
	add := func(u string) {
		if u == "" {
			return
		}
		for _, x := range out {
			if x == u {
				return
			}
		}
		out = append(out, u)
	}
	add(cfg.URL)
	for _, sub := range cfg.Subs {
		add(sub.URL)
	}
	return out
}

// GET /api/admin/i18n/beacon?url=… – Sprachpakete eines Beacons (ohne url: der eingetragene Beacon)
func (s *Server) handleI18nBeaconList(w http.ResponseWriter, r *http.Request) {
	known := s.knownBeacons()
	base, err := normBeaconURL(r.URL.Query().Get("url"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if base == "" {
		if len(known) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"url": "", "suggestions": []string{}, "packs": []beaconLangPack{}})
			return
		}
		base = known[0]
	}
	ml := s.hub.Media()
	var info beaconInfo
	if err := ml.beaconGet(base, "/api/v1/info", &info); err != nil || !fedsig.IsBeacon(info.Software) {
		writeErr(w, http.StatusBadGateway, ErrBeaconRemote.Error())
		return
	}
	var list struct {
		Packs []beaconLangPack `json:"packs"`
	}
	if err := ml.beaconGet(base, "/api/v1/languages", &list); err != nil {
		writeErr(w, http.StatusBadGateway, ErrNoLangPacks.Error())
		return
	}
	cat := s.hub.I18n()
	src := cat.PackSources()
	packs := []beaconLangPack{}
	for _, p := range list.Packs {
		if !i18n.ValidCode(p.Code) {
			continue
		}
		p.Installed = cat.Has(p.Code)
		if ps, ok := src[p.Code]; ok && p.Installed && ps.URL == base {
			p.FromHere, p.InstAt = true, ps.Installed
			p.Update = p.Updated.After(ps.Updated)
		}
		packs = append(packs, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": base, "name": info.Name, "suggestions": known, "packs": packs})
}

// POST /api/admin/i18n/beacon/install {url, code, replace} – Sprachpaket übernehmen
func (s *Server) handleI18nBeaconInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL     string    `json:"url"`
		Code    string    `json:"code"`
		Replace bool      `json:"replace"` // bisherige Änderungen dieser Sprache verwerfen
		Updated time.Time `json:"updated"` // Stand des Pakets laut Liste
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	base, err := normBeaconURL(in.URL)
	if err != nil || base == "" {
		writeErr(w, http.StatusBadRequest, ErrBeaconURL.Error())
		return
	}
	if !i18n.ValidCode(in.Code) || in.Code == i18n.Source {
		writeErr(w, http.StatusBadRequest, ErrPackCode.Error())
		return
	}
	data, err := s.hub.Media().fetch(base+"/api/v1/languages/"+in.Code, 8<<20)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var head struct {
		Meta i18n.Meta `json:"_meta"`
	}
	if json.Unmarshal(data, &head) != nil {
		writeErr(w, http.StatusBadGateway, ErrBeaconRemote.Error())
		return
	}
	cat := s.hub.I18n()
	if !cat.Has(in.Code) {
		if err := cat.AddLanguage(in.Code, head.Meta.Name, head.Meta.Locale); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if head.Meta.Dir == "rtl" {
			m := cat.Meta(in.Code)
			m.Dir = "rtl"
			cat.SetMeta(in.Code, m)
		}
	}
	res, err := cat.Import(in.Code, data, in.Replace)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Updated.IsZero() {
		in.Updated = time.Now().UTC()
	}
	cat.SetPackSource(in.Code, i18n.PackSource{URL: base, Updated: in.Updated, Installed: time.Now().UTC()})
	for i, e := range res.Errors {
		res.Errors[i] = translateImportErr(w, e)
	}
	s.log(r, AuditEntry{Action: "i18n.pack", Detail: fmt.Sprintf("%s von %s: %d", in.Code, base, res.Applied)})
	writeJSON(w, http.StatusOK, res)
}

func translateImportErr(w http.ResponseWriter, e string) string {
	for i := 0; i < len(e)-1; i++ {
		if e[i] == ':' && e[i+1] == ' ' {
			return e[:i] + ": " + tr(w, e[i+2:])
		}
	}
	return tr(w, e)
}
