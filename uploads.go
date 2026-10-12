// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Bilder hochladen (Fotos, Screenshots) und Profilbilder. Wie bei Emotes und GIFs werden
// Bilder auf eine feste Größe gebracht; Fotos werden als JPEG gespeichert, Bilder mit
// Transparenz als PNG, animierte GIFs bleiben animiert.

const (
	UploadsOff   = "off"
	UploadsAll   = "all"
	UploadsKnown = "known" // Name älter als 24 Stunden (Standard)
	UploadsRoles = "roles" // nur Team und Rollen mit Recht „Bilder hochladen“

	maxUpload       = 8 << 20
	avatarSize      = 128
	defaultUpload   = 480
	uploadKeepAfter = time.Hour // nicht mehr benutzte Dateien nach dieser Zeit löschen
)

var (
	ErrUploadsOff   = errors.New("Bilder hochladen ist hier nicht erlaubt")
	ErrUploadKnown  = errors.New("Bilder kannst du hochladen, sobald dein Name einen Tag alt ist")
	ErrUploadRole   = errors.New("Bilder hochladen dürfen hier nur bestimmte Rollen")
	ErrAvatarsOff   = errors.New("Profilbilder sind ausgeschaltet")
	ErrUploadMode   = errors.New("Bilder: off, all, known oder roles")
	ErrUploadSize   = errors.New("Bildgröße: 320, 480, 640 oder 800")
	UploadSizes     = []int{320, 480, 640, 800}
	uploadFileRe    = mediaFileRe
	errNoUploadFile = errors.New("Bitte eine Bilddatei mitschicken (Feld „file“)")
)

// UploadSettings: im Admin-Bereich unter „Emotes & GIFs“.
type UploadSettings struct {
	Mode    string `json:"mode"`
	Size    int    `json:"size"`
	Avatars bool   `json:"avatars"`
}

func (c MediaConfig) uploads() UploadSettings {
	u := UploadSettings{Mode: c.Uploads, Size: c.UploadSize, Avatars: !c.NoAvatars}
	if u.Mode == "" {
		u.Mode = UploadsKnown
	}
	if u.Size == 0 {
		u.Size = defaultUpload
	}
	return u
}

func (ml *MediaLib) Uploads() UploadSettings { return ml.Config().uploads() }

func (ml *MediaLib) SetUploads(in UploadSettings) (UploadSettings, error) {
	switch in.Mode {
	case UploadsOff, UploadsAll, UploadsKnown, UploadsRoles:
	default:
		return UploadSettings{}, ErrUploadMode
	}
	ok := false
	for _, s := range UploadSizes {
		ok = ok || s == in.Size
	}
	if !ok {
		return UploadSettings{}, ErrUploadSize
	}
	ml.mu.Lock()
	ml.cfg.Uploads, ml.cfg.UploadSize, ml.cfg.NoAvatars = in.Mode, in.Size, !in.Avatars
	out := ml.cfg.uploads()
	ml.mu.Unlock()
	ml.touch("media.json")
	ml.notify()
	return out, nil
}

// ProcessPhoto bringt ein Foto in einen Rahmen von box×box (nie vergrößert).
func ProcessPhoto(data []byte, box int) (ScaledImage, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ScaledImage{}, ErrImageFormat
	}
	if cfg.Width > maxImageSide || cfg.Height > maxImageSide || cfg.Width < 1 || cfg.Height < 1 {
		return ScaledImage{}, ErrImageTooBig
	}
	if format == "gif" {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return ScaledImage{}, ErrImageFormat
		}
		if len(g.Image) > 1 {
			return processGIF(g, box, box, false)
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ScaledImage{}, ErrImageFormat
	}
	src := toRGBA(img)
	w, h := fit(src.Bounds().Dx(), src.Bounds().Dy(), box, box, false)
	return encodePhoto(resize(src, w, h))
}

// ProcessAvatar schneidet die Mitte quadratisch aus und skaliert auf size×size (Standbild).
func ProcessAvatar(data []byte, size int) (ScaledImage, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ScaledImage{}, ErrImageFormat
	}
	if cfg.Width > maxImageSide || cfg.Height > maxImageSide || cfg.Width < 1 || cfg.Height < 1 {
		return ScaledImage{}, ErrImageTooBig
	}
	img, _, err := image.Decode(bytes.NewReader(data)) // bei GIFs das erste Bild
	if err != nil {
		return ScaledImage{}, ErrImageFormat
	}
	src := toRGBA(img)
	b := src.Bounds()
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	x0, y0 := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2
	crop := toRGBA(src.SubImage(image.Rect(x0, y0, x0+side, y0+side)))
	return encodePhoto(resize(crop, size, size))
}

// encodePhoto: deckend → JPEG (klein), mit Transparenz → PNG.
func encodePhoto(img *image.RGBA) (ScaledImage, error) {
	opaque := true
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 255 {
			opaque = false
			break
		}
	}
	var buf bytes.Buffer
	if opaque {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
			return ScaledImage{}, err
		}
		return ScaledImage{Data: buf.Bytes(), Ext: "jpg", W: img.Bounds().Dx(), H: img.Bounds().Dy()}, nil
	}
	pngImg, err := ProcessImageFromRGBA(img)
	if err != nil {
		return ScaledImage{}, err
	}
	return pngImg, nil
}

// readImageUpload liest das Feld "file" aus multipart/form-data.
func readImageUpload(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+64<<10)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		if strings.Contains(err.Error(), "too large") {
			return nil, ErrMediaTooLarge
		}
		return nil, errNoUploadFile
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return nil, errNoUploadFile
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUpload {
		return nil, ErrMediaTooLarge
	}
	return data, nil
}

// mayUpload prüft die Upload-Regel für eine Person.
func (s *Server) mayUpload(p Poster) error {
	switch s.hub.Media().Uploads().Mode {
	case UploadsOff:
		return ErrUploadsOff
	case UploadsRoles:
		if !p.Perms.Upload {
			return ErrUploadRole
		}
	case UploadsKnown:
		if !p.Perms.Upload && (p.Since.IsZero() || time.Since(p.Since) < 24*time.Hour) {
			return ErrUploadKnown
		}
	}
	return nil
}

// POST /api/rooms/{room}/upload – multipart: file, text, replyTo, thread
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	room := NormRoom(r.PathValue("room"))
	a := &Access{ID: me.ID, Team: me.Team, Roles: me.Roles}
	if !s.hub.CanRead(room, a) {
		fail(w, ErrNoRoom)
		return
	}
	if !s.hub.CanWrite(room, a) {
		fail(w, ErrReadOnly)
		return
	}
	if enc, _, _ := s.e2eMode(room, me); enc {
		fail(w, ErrE2EMedia)
		return
	}
	p := s.posterOf(me, ip)
	if err := s.hub.checkSanction(p); err != nil {
		fail(w, err)
		return
	}
	if err := s.mayUpload(p); err != nil {
		fail(w, err)
		return
	}
	if !s.uploadRL.Allow(me.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Bilder – bitte etwas warten.")
		return
	}
	data, err := readImageUpload(w, r)
	if err != nil {
		fail(w, err)
		return
	}
	ml := s.hub.Media()
	ml.work <- struct{}{}
	img, err := ProcessPhoto(data, ml.Uploads().Size)
	<-ml.work
	if err != nil {
		fail(w, err)
		return
	}
	file := randomHex(10) + "." + img.Ext
	if err := writeFileAtomic(filepath.Join(ml.Dir(), "uploads", file), img.Data); err != nil {
		fail(w, err)
		return
	}
	replyTo, _ := strconv.ParseInt(r.FormValue("replyTo"), 10, 64)
	thread, _ := strconv.ParseInt(r.FormValue("thread"), 10, 64)
	media := &Media{Kind: "image", Src: "/media/uploads/" + file, W: img.W, H: img.H, Source: "upload"}
	msg, err := s.hub.SendFull(room, p, SendInput{Text: r.FormValue("text"), ReplyTo: replyTo, Thread: thread, Media: media})
	if err != nil {
		os.Remove(filepath.Join(ml.Dir(), "uploads", file))
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// UploadRefs liefert alle Dateien, auf die Nachrichten verweisen (auch ausgeblendete).
func (h *Hub) UploadRefs() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for _, r := range h.rooms {
		for _, m := range r.history {
			if m.Media != nil && strings.HasPrefix(m.Media.Src, "/media/uploads/") {
				out[strings.TrimPrefix(m.Media.Src, "/media/uploads/")] = true
			}
		}
	}
	return out
}

// cleanupMedia löscht hochgeladene Bilder und Profilbilder, die nicht mehr gebraucht werden
// (gelöschte Nachrichten, abgelaufene Namen) – nach einer Schonfrist.
func (s *Server) cleanupMedia() {
	dir := s.hub.Media().Dir()
	if dir == "" {
		return
	}
	refs := s.hub.UploadRefs()
	avatars := s.users.AvatarFiles()
	clean := func(sub string, keep map[string]bool) {
		entries, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, e := range entries {
			if e.IsDir() || keep[e.Name()] {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > uploadKeepAfter {
				os.Remove(filepath.Join(dir, sub, e.Name()))
			}
		}
	}
	clean("uploads", refs)
	clean("avatars", avatars)
}
