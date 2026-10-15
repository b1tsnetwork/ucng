// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ucng/internal/i18n"
)

// Vorlagen: Teile der Grundkonfiguration als .zip exportieren und in diese oder eine andere
// Instanz importieren. Nachrichten, Personen, Konten, Schlüssel und Geheimnisse (Bot-Schlüssel,
// Webhook-Geheimnisse, API-Schlüssel, SMTP) sind nie enthalten.
//
//	manifest.json        Format, Version, Herkunft, enthaltene Teile, Anzahl
//	parts/<teil>.json    Daten je Teil
//	files/emotes/…       Emote-Bilder (Original, sonst skaliert)
//	files/gifs/…         GIFs der Bibliothek

const (
	templateFormat   = "ucng-template"
	legacyTplFormat  = "gochat-template" // vor der Umbenennung exportierte Vorlagen
	templateVersion  = 1
	maxTemplateZip   = 64 << 20
	maxTemplateTotal = 256 << 20 // entpackt
	maxTemplateFiles = 4000
	templateTTL      = 30 * time.Minute
	maxPendingTpl    = 4
)

// TemplateParts in der Reihenfolge, in der sie importiert werden (Abhängigkeiten zuerst).
var TemplateParts = []string{"roles", "channels", "automod", "commands", "bots", "widgets", "emotes", "gifs", "media", "i18n", "settings"}

var (
	ErrTplFormat  = errors.New("Das ist keine UCNG-Vorlage")
	ErrTplVersion = errors.New("Diese Vorlage stammt von einer neueren UCNG-Version")
	ErrTplToken   = errors.New("Die geprüfte Vorlage ist abgelaufen – bitte erneut hochladen")
	ErrTplParts   = errors.New("Bitte mindestens einen Teil auswählen")
	ErrTplTooBig  = errors.New("Die Vorlage ist zu groß")
	ErrTplMode    = errors.New("Vorgehen muss merge oder replace sein")
	tplFileRe     = regexp.MustCompile(`^files/(emotes|gifs)/[A-Za-z0-9_.-]{1,80}$`)
)

type TemplateManifest struct {
	Format  string         `json:"format"`
	Version int            `json:"version"`
	Created time.Time      `json:"created"`
	Source  string         `json:"source"` // Titel der Instanz
	Parts   []string       `json:"parts"`
	Counts  map[string]int `json:"counts"`
}

// --- Inhalte der Teile ---

type tplRoom struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	SlowMode    int          `json:"slowMode,omitempty"`
	Links       string       `json:"links,omitempty"`
	BotSlowMode int          `json:"botSlowMode,omitempty"`
	BotLinks    string       `json:"botLinks,omitempty"`
	ReadRoles   []string     `json:"readRoles,omitempty"` // Rollen-IDs der Vorlage (siehe roles)
	WriteRoles  []string     `json:"writeRoles,omitempty"`
	Automod     *RoomAutomod `json:"automod,omitempty"`
	Voice       *VoiceConfig `json:"voice,omitempty"` // Sprachkanal (stageRoles = Rollen-IDs der Vorlage)
}

type tplChannels struct {
	Rooms     []tplRoom  `json:"rooms"`
	Groups    []tplGroup `json:"groups"`
	Ungrouped []string   `json:"ungrouped"`
	// Rollennamen der Vorlage, damit Kanal-Rechte auch ohne den Teil „roles“ zugeordnet werden können.
	RoleNames map[string]string `json:"roleNames,omitempty"`
}

type tplGroup struct {
	Name  string   `json:"name"`
	Rooms []string `json:"rooms"`
}

type tplAutomod struct {
	Automod AutomodConfig `json:"automod"`
	Filter  FilterConfig  `json:"filter"`
}

type tplBot struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	AllRooms    bool     `json:"allRooms"`
	Rooms       []string `json:"rooms,omitempty"`
	WebhookURL  string   `json:"webhookUrl,omitempty"`
	ReceiveBots bool     `json:"receiveBots"`
	Perms       BotPerms `json:"perms"`
	Enabled     bool     `json:"enabled"`
}

type tplEmote struct {
	Name string `json:"name"`
	Size int    `json:"size"`
	File string `json:"file"`
}

type tplGIF struct {
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	File  string   `json:"file"`
	Hash  string   `json:"sha256"`
}

type tplGIFs struct {
	Library []tplGIF    `json:"library"`
	Beacons []BeaconSub `json:"beacons"` // abonnierte Beacon-Sammlungen
}

type tplMedia struct {
	EmoteSize   int              `json:"emoteSize"`
	GifsEnabled bool             `json:"gifsEnabled"`
	GifWidth    int              `json:"gifWidth"`
	Provider    string           `json:"provider"`
	Rating      string           `json:"rating"`
	Reactions   ReactionSettings `json:"reactions"`
	Uploads     UploadSettings   `json:"uploads"`
}

// --- Export ---

func validParts(in []string) []string {
	var out []string
	for _, p := range TemplateParts {
		if contains(in, p) {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) exportTemplate(w io.Writer, parts []string) error {
	h := s.hub
	ml := h.Media()
	zw := zip.NewWriter(w)
	man := TemplateManifest{Format: templateFormat, Version: templateVersion, Created: time.Now().UTC(),
		Source: h.Settings().Title, Parts: parts, Counts: map[string]int{}}
	files := map[string][]byte{}
	put := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		files[name] = b
		return nil
	}
	for _, p := range parts {
		var err error
		switch p {
		case "settings":
			st := h.Settings()
			err = put("parts/settings.json", st)
			man.Counts[p] = 1
		case "roles":
			list := h.Roles().List()
			err = put("parts/roles.json", list)
			man.Counts[p] = len(list)
		case "channels":
			var c tplChannels
			c.RoleNames = map[string]string{}
			for _, r := range h.Roles().List() {
				c.RoleNames[r.ID] = r.Name
			}
			for _, ri := range h.Rooms() {
				if ri.Temp != nil { // temporäre Kanäle gehören nicht in eine Vorlage
					continue
				}
				tr := tplRoom{Name: ri.Name, Description: ri.Description, SlowMode: ri.SlowMode, Links: ri.Links,
					BotSlowMode: ri.BotSlowMode, BotLinks: ri.BotLinks, ReadRoles: ri.ReadRoles, WriteRoles: ri.WriteRoles, Voice: ri.Voice}
				if am, e := h.RoomAutomodConfig(ri.Name); e == nil && (am.Mode != AutomodGlobal || len(am.Patterns) > 0) {
					tr.Automod = &am
				}
				c.Rooms = append(c.Rooms, tr)
			}
			l := h.Layout()
			for _, g := range l.Groups {
				c.Groups = append(c.Groups, tplGroup{Name: g.Name, Rooms: g.Rooms})
			}
			c.Ungrouped = l.Ungrouped
			err = put("parts/channels.json", c)
			man.Counts[p] = len(c.Rooms)
		case "automod":
			a := tplAutomod{Automod: h.Automod().Config(), Filter: h.Filter()}
			err = put("parts/automod.json", a)
			man.Counts[p] = len(a.Automod.Patterns) + len(a.Filter.Patterns)
		case "commands":
			c := h.Commands().Config()
			err = put("parts/commands.json", c)
			man.Counts[p] = len(c.Commands)
		case "bots":
			var list []tplBot
			for _, b := range h.Bots().List() {
				list = append(list, tplBot{Name: b.Name, Description: b.Description, AllRooms: b.AllRooms, Rooms: b.Rooms,
					WebhookURL: b.WebhookURL, ReceiveBots: b.ReceiveBots, Perms: b.Perms, Enabled: b.Enabled})
			}
			err = put("parts/bots.json", list)
			man.Counts[p] = len(list)
		case "widgets":
			list := h.Widgets().List()
			for i := range list {
				list[i].ID, list[i].Created = "", time.Time{}
			}
			err = put("parts/widgets.json", list)
			man.Counts[p] = len(list)
		case "emotes":
			var list []tplEmote
			for _, e := range ml.exportEmotes() {
				data := ml.readMediaFile("originals", e.Original)
				if data == nil {
					data = ml.readMediaFile("emotes", e.File)
				}
				if data == nil {
					continue
				}
				f := "files/emotes/" + e.Name + imageExt(data)
				files[f] = data
				list = append(list, tplEmote{Name: e.Name, Size: e.Size, File: f})
			}
			err = put("parts/emotes.json", list)
			man.Counts[p] = len(list)
		case "gifs":
			g := tplGIFs{Library: []tplGIF{}, Beacons: h.Beacon().Subs()}
			for _, x := range ml.exportGIFs() {
				data := ml.readMediaFile("originals", x.Original)
				if data == nil {
					data = ml.readMediaFile("gifs", x.File)
				}
				if data == nil {
					continue
				}
				f := "files/gifs/" + x.ID + imageExt(data)
				files[f] = data
				g.Library = append(g.Library, tplGIF{Title: x.Title, Tags: x.Tags, File: f, Hash: sha256Hex(data)})
			}
			err = put("parts/gifs.json", g)
			man.Counts[p] = len(g.Library) + len(g.Beacons)
		case "media":
			c := ml.Config()
			m := tplMedia{EmoteSize: c.EmoteSize, GifsEnabled: c.GifsEnabled, GifWidth: c.GifWidth, Provider: c.Provider,
				Rating: c.Rating, Reactions: ml.Reactions(), Uploads: ml.Uploads()}
			err = put("parts/media.json", m)
			man.Counts[p] = 1
		case "i18n":
			st := h.I18n().Export()
			st.Version = 0
			err = put("parts/i18n.json", st)
			man.Counts[p] = h.I18n().Overrides() + len(st.Custom)
		}
		if err != nil {
			return err
		}
	}
	if err := put("manifest.json", man); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { // Manifest zuerst
		if (names[i] == "manifest.json") != (names[j] == "manifest.json") {
			return names[i] == "manifest.json"
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		method := zip.Deflate
		if strings.HasPrefix(n, "files/") {
			method = zip.Store // Bilder sind schon komprimiert
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: method, Modified: man.Created})
		if err != nil {
			return err
		}
		if _, err := fw.Write(files[n]); err != nil {
			return err
		}
	}
	return zw.Close()
}

func (ml *MediaLib) readMediaFile(sub, name string) []byte {
	if name == "" || !mediaFileRe.MatchString(name) {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(ml.Dir(), sub, name))
	if err != nil {
		return nil
	}
	return b
}

func imageExt(b []byte) string {
	switch http.DetectContentType(b) {
	case "image/gif":
		return ".gif"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ".bin"
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// --- Import: einlesen und prüfen ---

type parsedTemplate struct {
	Manifest TemplateManifest
	parts    map[string][]byte
	files    map[string][]byte
	at       time.Time
}

func parseTemplate(data []byte) (*parsedTemplate, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrTplFormat
	}
	if len(zr.File) > maxTemplateFiles {
		return nil, ErrTplTooBig
	}
	t := &parsedTemplate{parts: map[string][]byte{}, files: map[string][]byte{}, at: time.Now()}
	var total int64
	var man []byte
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		limit := int64(maxGIFUpload)
		if !strings.HasPrefix(f.Name, "files/") {
			limit = 16 << 20
		}
		if f.UncompressedSize64 > uint64(limit) {
			return nil, ErrTplTooBig
		}
		rc, err := f.Open()
		if err != nil {
			return nil, ErrTplFormat
		}
		b, err := io.ReadAll(io.LimitReader(rc, limit+1))
		rc.Close()
		if err != nil {
			return nil, ErrTplFormat
		}
		if int64(len(b)) > limit {
			return nil, ErrTplTooBig
		}
		total += int64(len(b))
		if total > maxTemplateTotal {
			return nil, ErrTplTooBig
		}
		switch {
		case f.Name == "manifest.json":
			man = b
		case strings.HasPrefix(f.Name, "parts/") && strings.HasSuffix(f.Name, ".json"):
			p := strings.TrimSuffix(strings.TrimPrefix(f.Name, "parts/"), ".json")
			if contains(TemplateParts, p) {
				t.parts[p] = b
			}
		case tplFileRe.MatchString(f.Name):
			t.files[f.Name] = b
		}
	}
	if man == nil || json.Unmarshal(man, &t.Manifest) != nil || (t.Manifest.Format != templateFormat && t.Manifest.Format != legacyTplFormat) {
		return nil, ErrTplFormat
	}
	if t.Manifest.Version > templateVersion {
		return nil, ErrTplVersion
	}
	var present []string
	for _, p := range TemplateParts {
		if _, ok := t.parts[p]; ok {
			present = append(present, p)
		}
	}
	t.Manifest.Parts = present
	if t.Manifest.Counts == nil {
		t.Manifest.Counts = map[string]int{}
	}
	if len(t.Manifest.Source) > 80 {
		t.Manifest.Source = t.Manifest.Source[:80]
	}
	return t, nil
}

// TemplateStore hält geprüfte Vorlagen kurz im Speicher, bis sie angewendet werden.
type TemplateStore struct {
	mu   sync.Mutex
	list map[string]*parsedTemplate
}

func (ts *TemplateStore) put(t *parsedTemplate) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.list == nil {
		ts.list = map[string]*parsedTemplate{}
	}
	ts.gcLocked()
	for len(ts.list) >= maxPendingTpl { // älteste verwerfen
		var oldest string
		for k, v := range ts.list {
			if oldest == "" || v.at.Before(ts.list[oldest].at) {
				oldest = k
			}
		}
		delete(ts.list, oldest)
	}
	tok := randomHex(16)
	ts.list[tok] = t
	return tok
}

func (ts *TemplateStore) get(tok string) (*parsedTemplate, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.gcLocked()
	t, ok := ts.list[tok]
	return t, ok
}

func (ts *TemplateStore) drop(tok string) {
	ts.mu.Lock()
	delete(ts.list, tok)
	ts.mu.Unlock()
}

func (ts *TemplateStore) gcLocked() {
	for k, v := range ts.list {
		if time.Since(v.at) > templateTTL {
			delete(ts.list, k)
		}
	}
}

// --- Import: anwenden ---

// TplResult fasst das Ergebnis je Teil zusammen.
type TplResult struct {
	Part    string   `json:"part"`
	Added   int      `json:"added"`
	Updated int      `json:"updated"`
	Removed int      `json:"removed"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
	Keys    []tplKey `json:"keys,omitempty"` // neue Bot-Schlüssel (nur jetzt sichtbar)
}

type tplKey struct {
	Bot   string `json:"bot"`
	Token string `json:"token"`
}

func (r *TplResult) fail(format string, a ...any) {
	if len(r.Errors) < 20 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
	}
}

func (s *Server) applyTemplate(t *parsedTemplate, parts []string, replace bool) []TplResult {
	h := s.hub
	var out []TplResult
	roleMap := map[string]string{} // Rollen-ID der Vorlage → Rollen-ID hier
	createdRooms := map[string]bool{}
	for _, p := range parts {
		raw, ok := t.parts[p]
		if !ok {
			continue
		}
		res := TplResult{Part: p}
		switch p {
		case "roles":
			s.tplRoles(raw, replace, roleMap, &res)
		case "channels":
			s.tplChannels(raw, replace, roleMap, createdRooms, &res)
		case "automod":
			var a tplAutomod
			if json.Unmarshal(raw, &a) != nil {
				res.fail("Daten fehlerhaft")
				break
			}
			am, f := a.Automod, a.Filter
			if !replace {
				cur := h.Automod().Config()
				am.Enabled = cur.Enabled
				am.Patterns, res.Added = mergeLines(cur.Patterns, a.Automod.Patterns)
				var n int
				f.Patterns, n = mergeLines(h.Filter().Patterns, a.Filter.Patterns)
				res.Added += n
			} else {
				res.Updated = len(am.Patterns) + len(f.Patterns)
			}
			if _, err := h.SetAutomod(am); err != nil {
				res.fail("Automod: %v", err)
			}
			if _, err := h.SetFilter(f); err != nil {
				res.fail("Wortfilter: %v", err)
			}
		case "commands":
			s.tplCommands(raw, replace, &res)
		case "bots":
			s.tplBots(raw, replace, &res)
		case "widgets":
			s.tplWidgets(raw, replace, &res)
		case "emotes":
			s.tplEmotes(t, raw, replace, &res)
		case "gifs":
			s.tplGIFs(t, raw, replace, &res)
		case "media":
			var m tplMedia
			if json.Unmarshal(raw, &m) != nil {
				res.fail("Daten fehlerhaft")
				break
			}
			ml := h.Media()
			cur := ml.Config()
			prov := m.Provider
			if prov != "" && cur.APIKey == "" { // ohne eigenen Schlüssel keine Web-Suche
				prov = cur.Provider
			}
			if _, _, err := ml.SetConfig(MediaSettingsInput{EmoteSize: m.EmoteSize, GifsEnabled: m.GifsEnabled, GifWidth: m.GifWidth,
				Provider: prov, Rating: m.Rating}); err != nil {
				res.fail("Einstellungen: %v", err)
			} else {
				res.Updated++
			}
			var reacts []string
			for _, r := range m.Reactions.Reactions {
				if ml.ValidReaction(r) {
					reacts = append(reacts, r)
				} else {
					res.Skipped++
				}
			}
			if len(reacts) > 0 {
				m.Reactions.Reactions = reacts
				if _, err := ml.SetReactions(m.Reactions); err != nil {
					res.fail("Reaktionen: %v", err)
				} else {
					res.Updated++
				}
			}
			if _, err := ml.SetUploads(m.Uploads); err != nil {
				res.fail("Bilder: %v", err)
			} else {
				res.Updated++
			}
		case "i18n":
			var st i18n.State
			if json.Unmarshal(raw, &st) != nil {
				res.fail("Daten fehlerhaft")
				break
			}
			r := h.I18n().ApplyState(st, replace)
			res.Added, res.Skipped = r.Applied, r.Unchanged+r.Unknown
			for _, e := range r.Errors {
				res.fail("%s", e)
			}
		case "settings":
			var st Settings
			if json.Unmarshal(raw, &st) != nil {
				res.fail("Daten fehlerhaft")
				break
			}
			before := h.Settings()
			if _, err := h.SetSettings(st); err != nil {
				res.fail("%v", err)
			} else {
				res.Updated = 1
				if before.RetentionDays != st.RetentionDays {
					go s.applyRetention()
				}
			}
		}
		out = append(out, res)
	}
	return out
}

// mergeLines hängt neue Zeilen an (ohne Doppelte, Groß-/Kleinschreibung egal).
func mergeLines(cur, add []string) ([]string, int) {
	seen := map[string]bool{}
	out := append([]string{}, cur...)
	for _, l := range cur {
		seen[strings.ToLower(strings.TrimSpace(l))] = true
	}
	n := 0
	for _, l := range add {
		k := strings.ToLower(strings.TrimSpace(l))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
		n++
	}
	return out, n
}

func (s *Server) tplRoles(raw []byte, replace bool, roleMap map[string]string, res *TplResult) {
	var in []Role
	if json.Unmarshal(raw, &in) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	cur := s.hub.Roles().List()
	byName := map[string]Role{}
	for _, r := range cur {
		byName[strings.ToLower(r.Name)] = r
	}
	var next []Role
	if !replace {
		next = append(next, cur...)
	}
	tplIDs := make([]string, 0, len(in))
	for _, r := range in {
		tplID := r.ID
		if old, ok := byName[strings.ToLower(strings.Join(strings.Fields(r.Name), " "))]; ok {
			if !replace {
				roleMap[tplID] = old.ID
				res.Skipped++
				tplIDs = append(tplIDs, "")
				continue
			}
			r.ID = old.ID // gleiche Rolle: Zuweisungen bleiben
			res.Updated++
		} else {
			r.ID = ""
			res.Added++
		}
		tplIDs = append(tplIDs, tplID)
		next = append(next, r)
	}
	if replace {
		for _, r := range cur {
			found := false
			for _, n := range next {
				if n.ID == r.ID {
					found = true
				}
			}
			if !found {
				res.Removed++
			}
		}
	}
	out, err := s.hub.Roles().Set(next)
	if err != nil {
		res.fail("%v", err)
		res.Added, res.Updated, res.Removed = 0, 0, 0
		return
	}
	// neue IDs zuordnen: gleiche Reihenfolge wie next
	byNameOut := map[string]string{}
	for _, r := range out {
		byNameOut[strings.ToLower(r.Name)] = r.ID
	}
	for i, r := range in {
		if tplIDs[i] == "" {
			continue
		}
		if id, ok := byNameOut[strings.ToLower(strings.Join(strings.Fields(r.Name), " "))]; ok {
			roleMap[tplIDs[i]] = id
		}
	}
}

func (s *Server) tplChannels(raw []byte, replace bool, roleMap map[string]string, created map[string]bool, res *TplResult) {
	h := s.hub
	var c tplChannels
	if json.Unmarshal(raw, &c) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	// Rollen der Vorlage über den Namen zuordnen, falls „roles“ nicht importiert wurde.
	localByName := map[string]string{}
	for _, r := range h.Roles().List() {
		localByName[strings.ToLower(r.Name)] = r.ID
	}
	mapRoles := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if id == RoleTeamOnly {
				out = append(out, id)
			} else if m, ok := roleMap[id]; ok {
				out = append(out, m)
			} else if n, ok := c.RoleNames[id]; ok {
				if m, ok := localByName[strings.ToLower(n)]; ok {
					out = append(out, m)
				}
			}
		}
		return out
	}
	exists := map[string]bool{}
	for _, r := range h.Rooms() {
		exists[r.Name] = true
	}
	for _, tr := range c.Rooms {
		name := NormRoom(tr.Name)
		in := RoomInput{Name: &name, Description: tr.Description, SlowMode: tr.SlowMode, Links: tr.Links,
			BotSlowMode: tr.BotSlowMode, BotLinks: tr.BotLinks, ReadRoles: mapRoles(tr.ReadRoles), WriteRoles: mapRoles(tr.WriteRoles)}
		if tr.Voice != nil {
			v := *tr.Voice
			v.StageRoles = mapRoles(v.StageRoles)
			in.Voice = &v
		}
		if in.Links == "" {
			in.Links = LinksAll
		}
		if in.BotLinks == "" {
			in.BotLinks = LinksAll
		}
		if exists[name] {
			if !replace {
				res.Skipped++
				continue
			}
			if _, err := h.UpdateRoomWith(name, in); err != nil {
				res.fail("#%s: %v", name, err)
				continue
			}
			res.Updated++
		} else {
			if _, err := h.CreateRoomWith(in); err != nil {
				res.fail("#%s: %v", name, err)
				continue
			}
			created[name] = true
			exists[name] = true
			res.Added++
		}
		if tr.Automod != nil {
			if _, err := h.SetRoomAutomod(name, *tr.Automod); err != nil {
				res.fail("#%s Automod: %v", name, err)
			}
		}
	}
	// Anordnung: Gruppen gleichen Namens werden zusammengeführt. Beim Ergänzen wandern nur
	// neu angelegte Kanäle in ihre Gruppe, beim Ersetzen alle Kanäle der Vorlage.
	cur := h.Layout()
	var in LayoutInput
	type gi = struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		Rooms []string `json:"rooms"`
	}
	moving := func(n string) bool { n = NormRoom(n); return exists[n] && (replace || created[n]) }
	movedTo := map[string]bool{}
	for _, tg := range c.Groups {
		for _, n := range tg.Rooms {
			if moving(n) {
				movedTo[NormRoom(n)] = true
			}
		}
	}
	strip := func(list []string) []string {
		var out []string
		for _, n := range list {
			if !movedTo[n] {
				out = append(out, n)
			}
		}
		return out
	}
	idxByName := map[string]int{}
	for _, g := range cur.Groups {
		idxByName[strings.ToLower(g.Name)] = len(in.Groups)
		in.Groups = append(in.Groups, gi{ID: g.ID, Name: g.Name, Rooms: strip(g.Rooms)})
	}
	for _, tg := range c.Groups {
		var rooms []string
		for _, n := range tg.Rooms {
			if moving(n) {
				rooms = append(rooms, NormRoom(n))
			}
		}
		key := strings.ToLower(strings.Join(strings.Fields(tg.Name), " "))
		if i, ok := idxByName[key]; ok {
			if replace {
				in.Groups[i].Rooms = append(rooms, in.Groups[i].Rooms...)
			} else {
				in.Groups[i].Rooms = append(in.Groups[i].Rooms, rooms...)
			}
			continue
		}
		if len(rooms) == 0 && !replace {
			continue
		}
		idxByName[key] = len(in.Groups)
		in.Groups = append(in.Groups, gi{Name: tg.Name, Rooms: rooms})
	}
	if replace {
		for _, n := range c.Ungrouped {
			if exists[NormRoom(n)] {
				in.Ungrouped = append(in.Ungrouped, NormRoom(n))
			}
		}
	}
	in.Ungrouped = append(in.Ungrouped, cur.Ungrouped...)
	if _, err := h.SetLayout(in); err != nil {
		res.fail("Anordnung: %v", err)
	}
}

func (s *Server) tplCommands(raw []byte, replace bool, res *TplResult) {
	var c CommandsConfig
	if json.Unmarshal(raw, &c) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	cmds := s.hub.Commands()
	cur := cmds.Config()
	if name, ok := CleanName(c.BotName); ok && nameKey(name) != nameKey(cmds.BotName()) {
		if s.users.Taken(name) || s.hub.Bots().HasNameKey(nameKey(name)) {
			c.BotName = cur.BotName
			res.fail("Der Name %q ist hier schon vergeben – der bisherige Name bleibt", name)
		}
	}
	if replace {
		res.Updated = len(c.Commands)
		res.Removed = max(0, len(cur.Commands)-len(c.Commands))
	} else {
		used := map[string]bool{}
		for _, x := range cur.Commands {
			used[x.Trigger] = true
			for _, a := range x.Aliases {
				used[a] = true
			}
		}
		next := cur
		next.Commands = append([]Command{}, cur.Commands...)
		for _, x := range c.Commands {
			clash := used[x.Trigger]
			for _, a := range x.Aliases {
				clash = clash || used[a]
			}
			if clash {
				res.Skipped++
				continue
			}
			x.ID = ""
			used[x.Trigger] = true
			for _, a := range x.Aliases {
				used[a] = true
			}
			next.Commands = append(next.Commands, x)
			res.Added++
		}
		c = next
	}
	if _, err := cmds.Set(c); err != nil {
		res.fail("%v", err)
		res.Added, res.Updated, res.Removed = 0, 0, 0
	}
}

func (s *Server) tplBots(raw []byte, replace bool, res *TplResult) {
	var in []tplBot
	if json.Unmarshal(raw, &in) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	bots := s.hub.Bots()
	byName := map[string]BotInfo{}
	for _, b := range bots.List() {
		byName[nameKey(b.Name)] = b
	}
	keep := map[string]bool{}
	for _, tb := range in {
		perms := tb.Perms
		enabled := tb.Enabled
		bi := BotInput{Name: tb.Name, Description: tb.Description, AllRooms: tb.AllRooms, Rooms: tb.Rooms,
			WebhookURL: tb.WebhookURL, ReceiveBots: tb.ReceiveBots, Perms: &perms, Enabled: &enabled}
		if old, ok := byName[nameKey(tb.Name)]; ok {
			keep[old.ID] = true
			if !replace {
				res.Skipped++
				continue
			}
			if _, _, err := bots.Update(old.ID, bi); err != nil {
				res.fail("%s: %v", tb.Name, err)
				continue
			}
			res.Updated++ // Schlüssel bleibt gültig
			continue
		}
		info, tok, err := bots.Create(bi)
		if err != nil {
			res.fail("%s: %v", tb.Name, err)
			continue
		}
		keep[info.ID] = true
		res.Added++
		res.Keys = append(res.Keys, tplKey{Bot: info.Name, Token: tok})
	}
	if replace {
		for _, b := range byName {
			if !keep[b.ID] && bots.Delete(b.ID) == nil {
				res.Removed++
			}
		}
	}
}

func (s *Server) tplWidgets(raw []byte, replace bool, res *TplResult) {
	var in []Widget
	if json.Unmarshal(raw, &in) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	ws := s.hub.Widgets()
	byName := map[string]Widget{}
	for _, w := range ws.List() {
		byName[strings.ToLower(w.Name)] = w
	}
	keep := map[string]bool{}
	for _, w := range in {
		if old, ok := byName[strings.ToLower(w.Name)]; ok {
			keep[old.ID] = true
			if !replace {
				res.Skipped++
				continue
			}
			if _, err := ws.Update(old.ID, w); err != nil {
				res.fail("%s: %v", w.Name, err)
				continue
			}
			res.Updated++ // Einbettungscode bleibt gleich
			continue
		}
		nw, err := ws.Create(w)
		if err != nil {
			res.fail("%s: %v", w.Name, err)
			continue
		}
		keep[nw.ID] = true
		res.Added++
	}
	if replace {
		for _, w := range byName {
			if !keep[w.ID] && ws.Delete(w.ID) == nil {
				res.Removed++
			}
		}
	}
}

func (s *Server) tplEmotes(t *parsedTemplate, raw []byte, replace bool, res *TplResult) {
	var in []tplEmote
	if json.Unmarshal(raw, &in) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	ml := s.hub.Media()
	have := map[string]bool{}
	for _, e := range ml.Emotes() {
		have[e.Name] = true
	}
	keep := map[string]bool{}
	for _, e := range in {
		name := NormEmoteName(e.Name)
		data, ok := t.files[e.File]
		if !ok {
			res.fail(":%s: Bild fehlt", name)
			continue
		}
		keep[name] = true
		if have[name] {
			if !replace {
				res.Skipped++
				continue
			}
			ml.DeleteEmote(name)
			if _, err := ml.AddEmote(name, e.Size, data); err != nil {
				res.fail(":%s: %v", name, err)
				continue
			}
			res.Updated++
			continue
		}
		if _, err := ml.AddEmote(name, e.Size, data); err != nil {
			res.fail(":%s: %v", name, err)
			continue
		}
		res.Added++
	}
	if replace {
		for n := range have {
			if !keep[n] && ml.DeleteEmote(n) == nil {
				res.Removed++
			}
		}
	}
}

func (s *Server) tplGIFs(t *parsedTemplate, raw []byte, replace bool, res *TplResult) {
	var in tplGIFs
	if json.Unmarshal(raw, &in) != nil {
		res.fail("Daten fehlerhaft")
		return
	}
	ml := s.hub.Media()
	existing := ml.exportGIFs()
	hashes := map[string]string{} // Prüfsumme → ID
	for _, g := range existing {
		if b := ml.readMediaFile("originals", g.Original); b != nil {
			hashes[sha256Hex(b)] = g.ID
		}
	}
	keep := map[string]bool{}
	for _, g := range in.Library {
		data, ok := t.files[g.File]
		if !ok {
			res.fail("%s: Bild fehlt", g.Title)
			continue
		}
		if id, dup := hashes[sha256Hex(data)]; dup {
			keep[id] = true
			if replace {
				if _, err := ml.UpdateGIF(id, g.Title, g.Tags); err == nil {
					res.Updated++
					continue
				}
			}
			res.Skipped++
			continue
		}
		info, err := ml.AddGIF(g.Title, g.Tags, data)
		if err != nil {
			res.fail("%s: %v", g.Title, err)
			continue
		}
		keep[info.ID] = true
		hashes[sha256Hex(data)] = info.ID
		res.Added++
	}
	if replace {
		for _, g := range existing {
			if !keep[g.ID] && ml.DeleteGIF(g.ID) == nil {
				res.Removed++
			}
		}
	}
	// Beacon-Abos
	b := s.hub.Beacon()
	cur := b.Subs()
	var next []BeaconSub
	if !replace {
		next = cur
	}
	for _, sub := range in.Beacons {
		base, _ := splitSubAddr(sub.URL)
		if base == "" {
			continue
		}
		dup := false
		for _, x := range next {
			dup = dup || x.URL == base
		}
		if dup || len(next) >= maxBeaconSubs {
			res.Skipped++
			continue
		}
		var colls []string
		for _, c := range sub.Collections {
			if beaconCollRe.MatchString(c) {
				colls = append(colls, c)
			}
		}
		if colls == nil {
			colls = []string{}
		}
		next = append(next, BeaconSub{URL: base, Name: sub.Name, Collections: colls})
		res.Added++
	}
	if next == nil {
		next = []BeaconSub{}
	}
	b.mu.Lock()
	b.cfg.Subs = next
	b.mu.Unlock()
	b.touch()
	ml.notify()
}

// --- HTTP ---

// GET /api/admin/template?parts=roles,channels,…
func (s *Server) handleTemplateExport(w http.ResponseWriter, r *http.Request) {
	parts := validParts(strings.Split(r.URL.Query().Get("parts"), ","))
	if len(parts) == 0 {
		writeErr(w, http.StatusBadRequest, ErrTplParts.Error())
		return
	}
	var buf bytes.Buffer
	if err := s.exportTemplate(&buf, parts); err != nil {
		writeErr(w, http.StatusInternalServerError, "Die Vorlage konnte nicht erstellt werden")
		return
	}
	name := "ucng-vorlage-" + time.Now().Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	s.log(r, AuditEntry{Action: "template.export", Detail: strings.Join(parts, ", ")})
	w.Write(buf.Bytes())
}

// POST /api/admin/template/inspect (multipart, Feld „file“)
func (s *Server) handleTemplateInspect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTemplateZip+1<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Bitte eine Vorlage (.zip) auswählen – höchstens 64 MB")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTemplateZip+1))
	if err != nil || len(data) > maxTemplateZip {
		writeErr(w, http.StatusRequestEntityTooLarge, ErrTplTooBig.Error())
		return
	}
	t, err := parseTemplate(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	tok := s.templates.put(t)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "manifest": t.Manifest})
}

// POST /api/admin/template/apply {token, parts, mode}
func (s *Server) handleTemplateApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string   `json:"token"`
		Parts []string `json:"parts"`
		Mode  string   `json:"mode"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Mode != "merge" && in.Mode != "replace" {
		writeErr(w, http.StatusBadRequest, ErrTplMode.Error())
		return
	}
	t, ok := s.templates.get(in.Token)
	if !ok {
		writeErr(w, http.StatusGone, ErrTplToken.Error())
		return
	}
	parts := validParts(in.Parts)
	if len(parts) == 0 {
		writeErr(w, http.StatusBadRequest, ErrTplParts.Error())
		return
	}
	s.templates.drop(in.Token)
	s.tplMu.Lock()
	res := s.applyTemplate(t, parts, in.Mode == "replace")
	s.tplMu.Unlock()
	s.log(r, AuditEntry{Action: "template.import", Detail: fmt.Sprintf("%s (%s) aus „%s“", strings.Join(parts, ", "), in.Mode, t.Manifest.Source)})
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}
