// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Ende-zu-Ende-verschlüsselte Direktnachrichten (Modus „hosted“).
//
// Jedes Konto hat ein ECDH-Schlüsselpaar (P-256). Der private Schlüssel liegt nur verschlüsselt
// auf dem Server (mit einem Schlüssel, den allein der Browser aus dem Passwort ableitet). Für eine
// Unterhaltung leiten beide Seiten per ECDH + HKDF denselben AES-GCM-Schlüssel ab. Der Server
// speichert und verteilt nur den Geheimtext:
//
//	{"v":1,"iv":"…","ct":"…","spk":"<öffentlicher Schlüssel Absender>","rpk":"<… Empfänger>"}
//
// Die mitgeschickten öffentlichen Schlüssel erlauben es, ältere Nachrichten auch nach einem
// Schlüsselwechsel der Gegenseite zu lesen, und machen einen Wechsel sichtbar.
// In verschlüsselten Unterhaltungen gibt es nur Text: keine Bilder, GIFs oder Umfragen
// (die lägen sonst unverschlüsselt auf dem Server). Automod und Wortfilter greifen nicht.

const (
	e2ePlaceholder = "🔒 Verschlüsselte Nachricht"
	maxE2ELen      = 12_000
)

var (
	ErrE2E          = errors.New("Ungültige verschlüsselte Nachricht")
	ErrE2ERequired  = errors.New("Diese Unterhaltung ist Ende-zu-Ende-verschlüsselt – bitte die Seite neu laden")
	ErrE2ENotHere   = errors.New("Verschlüsselte Nachrichten gibt es nur in Direktnachrichten zwischen Konten")
	ErrE2EMedia     = errors.New("In verschlüsselten Unterhaltungen gibt es nur Text (keine Bilder, GIFs oder Umfragen)")
	ErrFedFeature   = errors.New("Das geht in Unterhaltungen mit anderen Instanzen nicht")
	ErrE2EKeyAbsent = errors.New("Die andere Person hat noch keinen Schlüssel – sie muss sich einmal anmelden")
)

type e2ePayload struct {
	V   int    `json:"v"`
	IV  string `json:"iv"`
	CT  string `json:"ct"`
	SPK string `json:"spk"`
	RPK string `json:"rpk"`
}

func validE2E(s string) error {
	if s == "" || len(s) > maxE2ELen {
		return ErrE2E
	}
	var p e2ePayload
	if err := json.Unmarshal([]byte(s), &p); err != nil || (p.V != 1 && p.V != 2) {
		return ErrE2E
	}
	iv, err1 := base64.StdEncoding.DecodeString(p.IV)
	ct, err2 := base64.StdEncoding.DecodeString(p.CT)
	spk, err3 := base64.StdEncoding.DecodeString(p.SPK)
	rpk, err4 := base64.StdEncoding.DecodeString(p.RPK)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || len(iv) != 12 || len(ct) < 17 || len(spk) != 65 || len(rpk) != 65 {
		return ErrE2E
	}
	return nil
}

// e2eSender liefert den öffentlichen Absenderschlüssel aus der Nutzlast.
func e2eSender(s string) string {
	var p e2ePayload
	json.Unmarshal([]byte(s), &p)
	return p.SPK
}

// FedMeta: Nachricht in einer Unterhaltung mit einer anderen Instanz.
type FedMeta struct {
	Ref   int64  `json:"ref,omitempty"`   // ID der Nachricht auf der anderen Instanz (eingehend)
	State string `json:"state,omitempty"` // ausgehend: pending, sent, failed
}

// DMPeer liefert das Gegenüber in einer Direktnachricht.
func (h *Hub) DMPeer(roomName, me string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomName]
	if !ok || r.kind != RoomDM || !contains(r.members, me) {
		return "", false
	}
	for _, m := range r.members {
		if m != me {
			return m, true
		}
	}
	return "", false
}

// e2eMode: Ist eine Unterhaltung verschlüsselt? peer = Gegenüber, fed = andere Instanz.
func (s *Server) e2eMode(room string, me Identity) (enc bool, peer string, fed bool) {
	if !s.hosted() || !strings.HasPrefix(room, "dm-") {
		return false, "", false
	}
	peer, ok := s.hub.DMPeer(room, me.ID)
	if !ok {
		return false, "", false
	}
	if isFedID(peer) {
		return true, peer, true
	}
	if !me.Member {
		return false, peer, false
	}
	_, hasKey := s.users.PubKeyOf(peer)
	_, mine := s.users.PubKeyOf(me.ID)
	return hasKey && mine, peer, false
}

// EditOwnE2E ersetzt den Geheimtext einer eigenen verschlüsselten Nachricht.
func (h *Hub) EditOwnE2E(roomName string, id int64, p Poster, e2e string) (Message, error) {
	if err := validE2E(e2e); err != nil {
		return Message{}, err
	}
	if err := h.checkSanction(p); err != nil {
		return Message{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, m, err := h.findMsgLocked(roomName, id)
	if err != nil {
		return Message{}, err
	}
	if m.UserID == "" || m.UserID != p.ID || m.Bot {
		return Message{}, ErrNotOwn
	}
	if m.Deleted {
		return Message{}, ErrMsgDeleted
	}
	if m.E2E == "" {
		return Message{}, ErrE2E
	}
	if time.Since(m.Time) > EditWindow {
		return Message{}, ErrEditWindow
	}
	now := time.Now().UTC()
	m.E2E, m.EditedAt = e2e, &now
	h.afterChangeLocked(r, m)
	return h.out(r, *m, false), nil
}

// handleDMKey: GET /api/dms/{room}/key – öffentlicher Schlüssel des Gegenübers.
func (s *Server) handleDMKey(w http.ResponseWriter, r *http.Request) {
	me, _, ok := s.me(w, r)
	if !ok {
		return
	}
	room := NormRoom(r.PathValue("room"))
	enc, peer, fed := s.e2eMode(room, me)
	if peer == "" {
		fail(w, ErrNoRoom)
		return
	}
	out := map[string]any{"e2e": enc, "peer": peer, "fed": fed}
	if fed {
		addr := strings.TrimPrefix(peer, fedPrefix)
		out["address"] = addr
		if pk, err := s.fedLookupKey(r.Context(), addr); err == nil {
			out["pubKey"] = pk
		} else {
			out["error"] = err.Error()
		}
	} else if pk, ok := s.users.PubKeyOf(peer); ok {
		out["pubKey"] = pk
	}
	writeJSON(w, http.StatusOK, out)
}
