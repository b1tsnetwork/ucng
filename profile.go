// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Profil: eigenes Profilbild und Status („abwesend“, „beschäftigt“ + kurzer Text).

var (
	ErrStatus     = errors.New("Status: leer, away oder busy")
	ErrStatusText = errors.New("Statustext: höchstens 60 Zeichen")
	ErrStatusBad  = errors.New("Dieser Statustext ist nicht erlaubt")
)

// GET /api/profiles – Profilbilder und Status aller Personen, die etwas gesetzt haben.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.hub.Media().Uploads().Avatars {
		out := map[string]Profile{}
		for id, p := range s.users.ProfileMap() {
			p.Avatar = ""
			if p.Status != "" || p.Text != "" {
				out[id] = p
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, s.users.ProfileMap())
}

// POST /api/profile/avatar (multipart, Feld "file")
func (s *Server) handleAvatarSet(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	if !s.hub.Media().Uploads().Avatars {
		fail(w, ErrAvatarsOff)
		return
	}
	if err := s.hub.checkSanction(s.posterOf(me, ip)); err != nil {
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
	img, err := ProcessAvatar(data, avatarSize)
	<-ml.work
	if err != nil {
		fail(w, err)
		return
	}
	file := me.ID + "-" + randomHex(4) + "." + img.Ext
	if err := writeFileAtomic(filepath.Join(ml.Dir(), "avatars", file), img.Data); err != nil {
		fail(w, err)
		return
	}
	old, err := s.users.SetAvatar(me.ID, file)
	if err != nil {
		fail(w, err)
		return
	}
	s.removeAvatarFile(old)
	s.hub.Notify("profiles")
	writeJSON(w, http.StatusOK, map[string]string{"avatar": avatarURL(file)})
}

func (s *Server) removeAvatarFile(file string) {
	if file != "" && mediaFileRe.MatchString(file) && !strings.HasPrefix(file, ".") {
		os.Remove(filepath.Join(s.hub.Media().Dir(), "avatars", file))
	}
}

// DELETE /api/profile/avatar
func (s *Server) handleAvatarDelete(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	old, err := s.users.SetAvatar(me.ID, "")
	if err != nil {
		fail(w, err)
		return
	}
	s.removeAvatarFile(old)
	s.hub.Notify("profiles")
	w.WriteHeader(http.StatusNoContent)
}

// PUT /api/profile/status {"status": "away", "text": "Bin gleich zurück"}
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	me, ip, ok := s.me(w, r)
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
		Text   string `json:"text"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Status != "" && in.Status != "away" && in.Status != "busy" {
		fail(w, ErrStatus)
		return
	}
	text := strings.Join(strings.Fields(in.Text), " ")
	if len([]rune(text)) > 60 {
		fail(w, ErrStatusText)
		return
	}
	if text != "" {
		if err := s.hub.checkSanction(s.posterOf(me, ip)); err != nil {
			fail(w, err)
			return
		}
		if _, hit := s.hub.checkAutomod("", text); hit {
			fail(w, ErrStatusBad)
			return
		}
		text, _ = s.hub.applyFilter(text)
	}
	u, err := s.users.SetStatus(me.ID, in.Status, text)
	if err != nil {
		fail(w, err)
		return
	}
	s.hub.Notify("profiles")
	writeJSON(w, http.StatusOK, Profile{Avatar: u.Avatar, Status: u.Status, Text: u.StatusText})
}

// DELETE /api/admin/users/{id}/avatar – Profilbild entfernen (Moderation).
func (s *Server) handleAdminAvatarDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, ok := s.users.Get(id)
	if !ok {
		fail(w, ErrNoUser)
		return
	}
	old, _ := s.users.SetAvatar(id, "")
	s.removeAvatarFile(old)
	s.users.SetStatus(id, u.Status, "")
	s.hub.Notify("profiles")
	s.log(r, AuditEntry{Action: "user.avatar.remove", UserID: id, UserName: u.Name})
	w.WriteHeader(http.StatusNoContent)
}

// GET/PUT /api/admin/uploads – Regeln für Bilder und Profilbilder
func (s *Server) handleAdminUploadsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.hub.Media().Uploads())
}

func (s *Server) handleAdminUploadsSet(w http.ResponseWriter, r *http.Request) {
	var in UploadSettings
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := s.hub.Media().SetUploads(in)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "media.uploads", Detail: out.Mode})
	s.hub.Notify("profiles")
	writeJSON(w, http.StatusOK, out)
}
