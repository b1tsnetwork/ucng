// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- Öffentlich: Medien und Befehle für Chat und Widget ---

// GET /api/media – Emotes und ob GIFs (und die Web-Suche) verfügbar sind.
func (s *Server) handleMediaPublic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, s.hub.Media().Public())
}

// GET /api/commands?room=… – Befehle für die Vervollständigung im Chat.
func (s *Server) handleCommandsPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Commands().Public(NormRoom(r.URL.Query().Get("room"))))
}

// GET /api/gifs?q=… – GIF-Bibliothek durchsuchen.
func (s *Server) handleGIFLibrary(w http.ResponseWriter, r *http.Request) {
	if !s.hub.Media().Config().GifsEnabled {
		fail(w, ErrGifsOff)
		return
	}
	writeJSON(w, http.StatusOK, s.hub.Media().GIFs(r.URL.Query().Get("q")))
}

// GET /api/gifs/web?q=… – Web-Suche über GIPHY bzw. Tenor (läuft über den Server).
func (s *Server) handleGIFWeb(w http.ResponseWriter, r *http.Request) {
	if !s.gifRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Suchen – bitte kurz warten.")
		return
	}
	list, err := s.hub.Media().SearchWeb(r.URL.Query().Get("q"))
	if err != nil {
		if err == ErrNoWebSearch {
			fail(w, err)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /api/gifs/web/preview/{id} – Vorschaubild eines Suchtreffers über den Server laden,
// damit Besucher keine Verbindung zum Anbieter aufbauen.
func (s *Server) handleGIFPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, ok := s.hub.Media().previewURL(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "Vorschau abgelaufen – bitte neu suchen")
		return
	}
	if it, ok := s.previews.get(id); ok {
		w.Header().Set("Content-Type", it.ct)
		w.Header().Set("Cache-Control", "private, max-age=1800")
		w.Write(it.data)
		return
	}
	if !s.previewRL.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Vorschauen – bitte kurz warten.")
		return
	}
	release, ok := s.previews.acquire()
	if !ok {
		fail(w, ErrBusy)
		return
	}
	defer release()
	// Ältere Beacons liefern keine kleine Vorschau, dann ist es das GIF selbst (bis 20 MB).
	limit := int64(maxPreview)
	if strings.HasPrefix(id, "beacon:") {
		limit = maxGIFUpload
	}
	data, err := s.hub.Media().fetch(src, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("Vorschau nicht verfügbar: %s", err.Error())) //i18n
		return
	}
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		writeErr(w, http.StatusBadGateway, "Vorschau nicht verfügbar")
		return
	}
	s.previews.put(id, data, ct)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=1800")
	w.Write(data)
}

// GET /media/{kind}/{file} – skalierte Emotes und GIFs aus dem Datenordner.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	kind, file := r.PathValue("kind"), r.PathValue("file")
	if (kind != "emotes" && kind != "gifs" && kind != "web" && kind != "uploads" && kind != "avatars") || !mediaFileRe.MatchString(file) || strings.HasPrefix(file, ".") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.hub.Media().Dir(), kind, file)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	if kind == "uploads" && strings.HasPrefix(file, privateUploadPrefix) {
		// Bild aus einem privaten Kanal: nur mit gültiger, befristeter Signatur
		q := r.URL.Query()
		if !s.hub.checkMediaSig(file, q.Get("exp"), q.Get("sig")) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		http.ServeFile(w, r, path)
		return
	}
	// Dateinamen enthalten Zufall bzw. Größe – sie ändern sich nie.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	http.ServeFile(w, r, path)
}

// runCommand lässt den eingebauten Bot auf eine Nachricht antworten.
func (s *Server) runCommand(room string, msg Message, me Identity) {
	if msg.Deleted || msg.Text == "" {
		return
	}
	cmds := s.hub.Commands()
	res := cmds.Run(msg.Text, CmdContext{
		Room: room, User: me.Name, Team: me.Team, Now: time.Now().In(s.hub.Location()),
		Online: func() Event { ev, _ := s.hub.Presence(room); return ev },
		Tr:     s.publicTr(),
	})
	if !res.Matched || res.Text == "" {
		return
	}
	var replyTo int64
	if res.Reply {
		replyTo = msg.ID
	}
	bot := Poster{ID: CmdBotID, Name: cmds.BotName(), Bot: true, NoAutomod: true, System: true}
	s.hub.SendFull(room, bot, SendInput{Text: res.Text, ReplyTo: replyTo, Thread: msg.Thread})
}

// EphemeralReply ist eine Antwort des Befehls-Bots, die nur die fragende Person sieht.
// Sie wird nicht gespeichert und an niemanden sonst verschickt.
type EphemeralReply struct {
	Name    string    `json:"name"`    // Name des Bots
	Text    string    `json:"text"`    // Antwort (Formatierung wie bei Nachrichten)
	Command string    `json:"command"` // was die Person geschrieben hat
	Time    time.Time `json:"time"`
	Notice  bool      `json:"notice,omitempty"` // Hinweis statt Antwort (z. B. Abklingzeit)
}

// runEphemeral beantwortet einen privaten Befehl direkt in der HTTP-Antwort (200 statt 201).
func (s *Server) runEphemeral(w http.ResponseWriter, room, text string, me Identity, ip string) {
	p := s.posterOf(me, ip)
	if err := s.hub.checkSanction(p); err != nil {
		fail(w, err)
		return
	}
	if _, err := s.hub.Presence(room); err != nil {
		fail(w, err)
		return
	}
	text, ok := cleanText(text, maxTextLen)
	if !ok {
		fail(w, ErrInvalidText)
		return
	}
	cmds := s.hub.Commands()
	now := time.Now().In(s.hub.Location())
	res := cmds.Run(text, CmdContext{
		Room: room, User: me.Name, UserID: me.ID, Team: me.Team, Now: now,
		Online: func() Event { ev, _ := s.hub.Presence(room); return ev },
		Tr:     func(msg string) string { return tr(w, msg) },
	})
	out := EphemeralReply{Name: cmds.BotName(), Text: res.Text, Command: text, Time: now.UTC()}
	if !res.Matched || res.Text == "" {
		out.Text, out.Notice = res.Note, true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ephemeral": out})
}

// --- Admin: Medien ---

type adminMedia struct {
	Settings  MediaSettingsView `json:"settings"`
	Emotes    []EmoteInfo       `json:"emotes"`
	Gifs      []GIFInfo         `json:"gifs"`
	Reactions ReactionSettings  `json:"reactions"`
}

// GET /api/admin/media
func (s *Server) handleAdminMedia(w http.ResponseWriter, r *http.Request) {
	ml := s.hub.Media()
	writeJSON(w, http.StatusOK, adminMedia{Settings: ml.Config().view(), Emotes: ml.Emotes(), Gifs: ml.GIFs(""), Reactions: ml.Reactions()})
}

// PUT /api/admin/media/settings
func (s *Server) handleAdminMediaSettings(w http.ResponseWriter, r *http.Request) {
	var in MediaSettingsInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, rescale, err := s.hub.Media().SetConfig(in)
	if err != nil {
		fail(w, err)
		return
	}
	detail := fmt.Sprintf("Emotes %d px, GIFs %s %d px", v.EmoteSize, map[bool]string{true: "an", false: "aus"}[v.GifsEnabled], v.GifWidth)
	if v.Provider != "" {
		detail += ", Web-Suche: " + v.Provider
	}
	if in.APIKey != nil {
		detail += ", API-Schlüssel geändert"
	}
	s.log(r, AuditEntry{Action: "media.settings", Detail: detail})
	if rescale {
		go s.hub.Media().RescaleGIFs()
	}
	writeJSON(w, http.StatusOK, v)
}

// PUT /api/admin/reactions {"reactions":["👍",":party:"],"emoteReactions":true}
func (s *Server) handleAdminReactions(w http.ResponseWriter, r *http.Request) {
	var in ReactionSettings
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.hub.Media().SetReactions(in)
	if err != nil {
		fail(w, err)
		return
	}
	detail := strings.Join(out.Reactions, " ")
	if out.EmoteReactions {
		detail += " + alle Emotes"
	}
	s.log(r, AuditEntry{Action: "reactions.update", Detail: detail})
	writeJSON(w, http.StatusOK, out)
}

// readUpload liest eine hochgeladene Datei (multipart, Feld "file") oder – bei JSON – eine
// Adresse zum Import. Liefert Felder des Formulars bzw. des JSON-Objekts.
type uploadInput struct {
	data   []byte
	url    string
	fields map[string]string
	tags   []string
}

func readUpload(w http.ResponseWriter, r *http.Request, limit int64) (uploadInput, bool) {
	in := uploadInput{fields: map[string]string{}}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	if mt == "application/json" {
		var j struct {
			URL   string   `json:"url"`
			Name  string   `json:"name"`
			Title string   `json:"title"`
			Size  int      `json:"size"`
			Tags  []string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
			writeErr(w, http.StatusBadRequest, "Ungültiges JSON")
			return in, false
		}
		in.url, in.tags = j.URL, j.Tags
		in.fields["name"], in.fields["title"] = j.Name, j.Title
		if j.Size != 0 {
			in.fields["size"] = fmt.Sprint(j.Size)
		}
		return in, true
	}
	if err := r.ParseMultipartForm(limit + 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, "Upload fehlgeschlagen – ist die Datei zu groß?")
		return in, false
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Bitte eine Datei auswählen")
		return in, false
	}
	defer f.Close()
	if hdr.Size > limit {
		fail(w, ErrMediaTooLarge)
		return in, false
	}
	in.data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(in.data)) > limit {
		fail(w, ErrMediaTooLarge)
		return in, false
	}
	for _, k := range []string{"name", "title", "size"} {
		in.fields[k] = r.FormValue(k)
	}
	if in.fields["name"] == "" {
		in.fields["name"] = hdr.Filename
	}
	if t := r.FormValue("tags"); t != "" {
		in.tags = strings.Split(t, ",")
	}
	return in, true
}

func atoiField(s string) int {
	n := 0
	fmt.Sscan(s, &n)
	return n
}

// POST /api/admin/emotes – Datei hochladen (multipart: file, name, size) oder {"url","name","size"}.
func (s *Server) handleAdminEmoteAdd(w http.ResponseWriter, r *http.Request) {
	in, ok := readUpload(w, r, maxEmoteUpload)
	if !ok {
		return
	}
	size := atoiField(in.fields["size"])
	var e EmoteInfo
	var err error
	if in.url != "" {
		e, err = s.hub.Media().ImportEmote(in.fields["name"], size, in.url)
	} else {
		e, err = s.hub.Media().AddEmote(in.fields["name"], size, in.data)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "emote.create", Detail: fmt.Sprintf(":%s: (%d px)", e.Name, e.Size)})
	writeJSON(w, http.StatusCreated, e)
}

// PATCH /api/admin/emotes/{name} {"name": "neu", "size": 64}
func (s *Server) handleAdminEmoteUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Size int    `json:"size"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	old := r.PathValue("name")
	e, err := s.hub.Media().UpdateEmote(old, in.Name, in.Size)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "emote.update", Detail: fmt.Sprintf(":%s: → :%s: (%d px)", old, e.Name, e.Size)})
	writeJSON(w, http.StatusOK, e)
}

// DELETE /api/admin/emotes/{name}
func (s *Server) handleAdminEmoteDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.hub.Media().DeleteEmote(name); err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "emote.delete", Detail: ":" + name + ":"})
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/admin/gifs – GIF hochladen (multipart: file, title, tags) oder {"url","title","tags"}.
func (s *Server) handleAdminGIFAdd(w http.ResponseWriter, r *http.Request) {
	in, ok := readUpload(w, r, maxGIFUpload)
	if !ok {
		return
	}
	title := in.fields["title"]
	if title == "" && in.url == "" {
		title = strings.TrimSuffix(in.fields["name"], filepath.Ext(in.fields["name"]))
	}
	var g GIFInfo
	var err error
	if in.url != "" {
		g, err = s.hub.Media().ImportGIF(title, in.tags, in.url)
	} else {
		g, err = s.hub.Media().AddGIF(title, in.tags, in.data)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "gif.create", Detail: g.Title})
	writeJSON(w, http.StatusCreated, g)
}

// PATCH /api/admin/gifs/{id} {"title","tags"}
func (s *Server) handleAdminGIFUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string   `json:"title"`
		Tags  []string `json:"tags"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	g, err := s.hub.Media().UpdateGIF(r.PathValue("id"), in.Title, in.Tags)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// DELETE /api/admin/gifs/{id}
func (s *Server) handleAdminGIFDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.hub.Media().DeleteGIF(id); err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "gif.delete", Detail: id})
	w.WriteHeader(http.StatusNoContent)
}

// --- Admin: Befehle ---

func (s *Server) handleAdminCommands(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Commands().Config())
}

// PUT /api/admin/commands – vollständige Konfiguration.
func (s *Server) handleAdminCommandsSet(w http.ResponseWriter, r *http.Request) {
	var in CommandsConfig
	if !decodeJSON(w, r, &in) {
		return
	}
	// Der Bot-Name darf keiner Person und keinem anderen Bot gehören.
	if name, ok := CleanName(in.BotName); ok && nameKey(name) != nameKey(s.hub.Commands().BotName()) {
		if s.users.Taken(name) || s.hub.Bots().HasNameKey(nameKey(name)) {
			fail(w, ErrNameTaken)
			return
		}
	}
	out, err := s.hub.Commands().Set(in)
	if err != nil {
		fail(w, err)
		return
	}
	on := "aus"
	if out.Enabled {
		on = "an"
	}
	s.log(r, AuditEntry{Action: "commands.update", Detail: fmt.Sprintf("%s, %d Befehle, Präfix %s", on, len(out.Commands), out.Prefix)})
	writeJSON(w, http.StatusOK, out)
}

// POST /api/admin/commands/test {"text": "!würfel 20", "room": "lobby", "team": false}
func (s *Server) handleAdminCommandsTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Room string `json:"room"`
		Team bool   `json:"team"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	room := NormRoom(in.Room)
	if room == "" {
		room = "lobby"
	}
	res := s.hub.Commands().Run(in.Text, CmdContext{
		Room: room, User: actorFrom(r).label(), Team: in.Team, Now: time.Now().In(s.hub.Location()), DryRun: true,
		Online: func() Event { ev, _ := s.hub.Presence(room); return ev },
		Tr:     s.publicTr(),
	})
	writeJSON(w, http.StatusOK, res)
}
