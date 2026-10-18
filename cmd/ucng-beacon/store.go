// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Speicherung: drei JSON-Dateien im Datenordner (atomar ersetzt) und die GIF-Dateien.
//
//	beacon-data/
//	  config.json        Name, Beschreibung, GIPHY/Tenor-Zugang
//	  instances.json     gemeldete Instanzen
//	  gifs.json          Sammlungen und GIFs
//	  media/             skalierte GIFs
//	  originals/         Originale

const (
	staleAfter  = 3 * 24 * time.Hour  // so lange ohne Meldung: nicht mehr gelistet
	removeAfter = 30 * 24 * time.Hour // danach ganz entfernt
)

type BeaconConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Provider    string `json:"provider"` // "", giphy, tenor (nur für die Verwaltung: GIFs übernehmen)
	APIKey      string `json:"apiKey,omitempty"`
	Rating      string `json:"rating"`
	Credit      string `json:"credit"` // Quellenangabe an GIFs im Chat, z. B. „via Beacon Alpenraum“ (leer = „via <Name>“)
}

type Instance struct {
	Domain       string    `json:"domain"`
	URL          string    `json:"url"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Users        int       `json:"users"`
	Language     string    `json:"language,omitempty"`
	Country      string    `json:"country,omitempty"`
	Mode         string    `json:"mode,omitempty"` // simple, hosted
	Federation   bool      `json:"federation"`
	Registration bool      `json:"registration"` // neue Konten möglich (hosted) bzw. Namen frei wählbar (simple)
	Guests       string    `json:"guests,omitempty"`
	Version      string    `json:"version,omitempty"`
	Source       string    `json:"source,omitempty"` // Quellcode der Fassung (AGPL)
	Key          string    `json:"key"`              // Fingerabdruck des Serverschlüssels
	First        time.Time `json:"first"`
	Last         time.Time `json:"last"`
	Hidden       bool      `json:"hidden,omitempty"` // von der Beacon-Verwaltung ausgeblendet
	// Kanäle, die die Instanz für andere Instanzen freigibt (Kanal-Föderation)
	Channels []SharedChannel `json:"channels,omitempty"`
	// berechnet (siehe trust.go), nicht gespeichert
	Trust      string `json:"trust"`                // official, recommended oder leer (Community)
	TrustNote  string `json:"trustNote,omitempty"`  // Hinweis des Beacon-Teams
	KeyChanged bool   `json:"keyChanged,omitempty"` // Schlüssel passt nicht mehr zur Einstufung
	Port       bool   `json:"port,omitempty"`       // Adresse mit eigenem Port
}

type Collection struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Created     time.Time `json:"created"`
}

type GIF struct {
	ID          string    `json:"id"`
	File        string    `json:"file"`
	Original    string    `json:"original,omitempty"`
	Title       string    `json:"title"`
	Tags        []string  `json:"tags"`
	Collections []string  `json:"collections"`
	W           int       `json:"w"`
	H           int       `json:"h"`
	Preview     string    `json:"preview,omitempty"` // kleine Fassung für Vorschauen (Datei in media/)
	Source      string    `json:"source,omitempty"`  // upload, url, giphy, tenor
	Created     time.Time `json:"created"`
}

type gifDoc struct {
	Collections []Collection `json:"collections"`
	GIFs        []GIF        `json:"gifs"`
}

type Store struct {
	dir         string
	mu          sync.Mutex
	cfg         BeaconConfig
	instances   map[string]*Instance
	collections []Collection
	gifs        map[string]*GIF
	trust       map[string]TrustEntry
}

func OpenStore(dir string) (*Store, error) {
	s := &Store{dir: dir, instances: map[string]*Instance{}, gifs: map[string]*GIF{},
		cfg: BeaconConfig{Name: "UCNG Beacon", Rating: "g"}}
	for _, sub := range []string{"media", "originals"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if err := readJSON(filepath.Join(dir, "config.json"), &s.cfg); err != nil {
		return nil, err
	}
	var list []Instance
	if err := readJSON(filepath.Join(dir, "instances.json"), &list); err != nil {
		return nil, err
	}
	for i := range list {
		x := list[i]
		s.instances[x.Domain] = &x
	}
	if err := s.loadTrust(); err != nil {
		return nil, err
	}
	var doc gifDoc
	if err := readJSON(filepath.Join(dir, "gifs.json"), &doc); err != nil {
		return nil, err
	}
	s.collections = doc.Collections
	for i := range doc.GIFs {
		g := doc.GIFs[i]
		s.gifs[g.ID] = &g
	}
	return s, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s ist beschädigt: %v", path, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Store) saveLocked(name string) error {
	var v any
	switch name {
	case "config.json":
		v = s.cfg
	case "trust.json":
		v = s.trust
	case "instances.json":
		list := make([]Instance, 0, len(s.instances))
		for _, x := range s.instances {
			c := *x
			c.Trust, c.TrustNote, c.KeyChanged, c.Port = "", "", false, false
			list = append(list, c)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Domain < list[j].Domain })
		v = list
	case "gifs.json":
		doc := gifDoc{Collections: s.collections, GIFs: []GIF{}}
		for _, g := range s.gifs {
			doc.GIFs = append(doc.GIFs, *g)
		}
		sort.Slice(doc.GIFs, func(i, j int) bool { return doc.GIFs[i].Created.Before(doc.GIFs[j].Created) })
		if doc.Collections == nil {
			doc.Collections = []Collection{}
		}
		v = doc
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, name), append(data, '\n'))
}

func (s *Store) Config() BeaconConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Store) SetConfig(c BeaconConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = c
	return s.saveLocked("config.json")
}

// --- Instanzen ---

func (s *Store) PutInstance(x Instance, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.instances[x.Domain]; ok {
		x.First, x.Hidden = old.First, old.Hidden
	} else {
		x.First = now
	}
	x.Last = now
	s.instances[x.Domain] = &x
	s.bindTrustLocked(x.Domain, x.Key)
	return s.saveLocked("instances.json")
}

// TrustOf liefert die wirksame Stufe einer gemeldeten Instanz.
func (s *Store) TrustOf(domain string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.instances[domain]
	if !ok {
		return TrustCommunity
	}
	c := *x
	s.applyTrustLocked(&c)
	return c.Trust
}

func (s *Store) RemoveInstance(domain string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[domain]; !ok {
		return false
	}
	delete(s.instances, domain)
	s.saveLocked("instances.json")
	return true
}

func (s *Store) SetHidden(domain string, hidden bool) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.instances[domain]
	if !ok {
		return Instance{}, false
	}
	x.Hidden = hidden
	s.saveLocked("instances.json")
	c := *x
	s.applyTrustLocked(&c)
	return c, true
}

// Instances liefert Instanzen (admin = auch ausgeblendete und lange nicht gemeldete).
func (s *Store) Instances(now time.Time, admin bool) []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Instance{}
	for _, x := range s.instances {
		if !admin && (x.Hidden || now.Sub(x.Last) > staleAfter) {
			continue
		}
		c := *x
		s.applyTrustLocked(&c)
		c.Port = strings.Contains(c.Domain, ":")
		out = append(out, c)
	}
	return out
}

func (s *Store) PruneInstances(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for d, x := range s.instances {
		if now.Sub(x.Last) > removeAfter {
			delete(s.instances, d)
			changed = true
		}
	}
	if changed {
		s.saveLocked("instances.json")
	}
}

// --- Sammlungen und GIFs ---

func (s *Store) Collections() []Collection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Collection{}, s.collections...)
}

func (s *Store) collectionExistsLocked(id string) bool {
	for _, c := range s.collections {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (s *Store) GIFList() []GIF {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]GIF, 0, len(s.gifs))
	for _, g := range s.gifs {
		c := *g
		c.Tags = append([]string{}, g.Tags...)
		c.Collections = append([]string{}, g.Collections...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (s *Store) GetGIF(id string) (GIF, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gifs[id]
	if !ok {
		return GIF{}, false
	}
	return *g, true
}

func cleanText(t string, max int) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > max {
		t = string(r[:max])
	}
	return t
}
