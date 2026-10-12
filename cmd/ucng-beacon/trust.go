// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Vertrauensstufen: Das Team des Beacons stuft Instanzen ein.
//
//	official     Offiziell – vom Projekt bzw. vom Betreiber des Beacons selbst betrieben
//	recommended  Empfohlen – geprüft und für gut befunden
//	standard     Standard – vom Team gesichtet, unauffällig; keine Empfehlung, aber auch keine Warnung
//	(leer)       Community – nicht geprüft; jede Instanz, die sich anmeldet
//
// Die Stufe hängt an der Domain und am Serverschlüssel: Wechselt der Schlüssel (z. B. weil die
// Domain jemand anderem gehört), gilt die Instanz wieder als Community, bis das Team sie neu
// bestätigt. Abmelden und erneutes Anmelden behält die Stufe.

const (
	TrustOfficial    = "official"
	TrustRecommended = "recommended"
	TrustStandard    = "standard"
	TrustCommunity   = ""
)

type TrustEntry struct {
	Level string    `json:"level"`
	Key   string    `json:"key,omitempty"`  // Fingerabdruck, für den die Stufe gilt (leer = beim nächsten Melden binden)
	Note  string    `json:"note,omitempty"` // kurzer Hinweis, z. B. „Betrieben vom UCNG-Team“
	Set   time.Time `json:"set"`
}

func validTrust(l string) bool {
	return l == TrustOfficial || l == TrustRecommended || l == TrustStandard || l == TrustCommunity
}

func trustRank(l string) int {
	switch l {
	case TrustOfficial:
		return 0
	case TrustRecommended:
		return 1
	case TrustStandard:
		return 2
	}
	return 3
}

func (s *Store) loadTrust() error {
	s.trust = map[string]TrustEntry{}
	return readJSON(filepath.Join(s.dir, "trust.json"), &s.trust)
}

// applyTrustLocked setzt Trust/KeyChanged einer Instanz (Kopie). s.mu muss gehalten werden.
func (s *Store) applyTrustLocked(x *Instance) {
	x.Trust, x.TrustNote, x.KeyChanged = TrustCommunity, "", false
	t, ok := s.trust[x.Domain]
	if !ok || t.Level == TrustCommunity {
		return
	}
	if t.Key != "" && x.Key != "" && t.Key != x.Key {
		x.KeyChanged = true
		return
	}
	x.Trust, x.TrustNote = t.Level, t.Note
}

// bindTrustLocked merkt beim ersten Melden nach dem Einstufen den Schlüssel.
func (s *Store) bindTrustLocked(domain, key string) {
	if t, ok := s.trust[domain]; ok && t.Key == "" && key != "" {
		t.Key = key
		s.trust[domain] = t
		s.saveLocked("trust.json")
	}
}

// SetTrust stuft eine Domain ein. Die Stufe gilt für den aktuell bekannten Schlüssel.
func (s *Store) SetTrust(domain, level, note string) (TrustEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if level == TrustCommunity {
		delete(s.trust, domain)
		s.saveLocked("trust.json")
		return TrustEntry{}, true
	}
	key := ""
	if x, ok := s.instances[domain]; ok {
		key = x.Key
	}
	t := TrustEntry{Level: level, Key: key, Note: cleanText(note, 80), Set: time.Now().UTC()}
	s.trust[domain] = t
	s.saveLocked("trust.json")
	return t, true
}

// TrustList: alle Einstufungen (auch für Domains, die gerade nicht gemeldet sind).
func (s *Store) TrustList() map[string]TrustEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]TrustEntry, len(s.trust))
	for k, v := range s.trust {
		out[k] = v
	}
	return out
}

// --- HTTP ---

// PUT /api/admin/trust/{domain} {"level":"official|recommended|","note":"…"}
// Geht auch für Domains, die sich noch nicht gemeldet haben (z. B. eigene Instanzen vorab).
func (s *Server) handleTrustPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Level string `json:"level"`
		Note  string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	domain := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	if !domainRe.MatchString(domain) {
		writeErr(w, http.StatusBadRequest, "Ungültige Domain")
		return
	}
	if !validTrust(in.Level) {
		writeErr(w, http.StatusBadRequest, "Stufe: official, recommended, standard oder leer (Community)")
		return
	}
	t, _ := s.store.SetTrust(domain, in.Level, in.Note)
	writeJSON(w, http.StatusOK, t)
}

// GET /api/admin/trust – Einstufungen ohne gemeldete Instanz (zum Vorab-Eintragen sichtbar machen)
func (s *Server) handleTrustList(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Domain string `json:"domain"`
		TrustEntry
	}
	out := []row{}
	for d, t := range s.store.TrustList() {
		out = append(out, row{d, t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	writeJSON(w, http.StatusOK, out)
}
