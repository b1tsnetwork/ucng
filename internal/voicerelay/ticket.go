// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicerelay

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Ticket: die Erlaubnis des Chat-Servers, einem Sprachkanal beizutreten. Es wird mit dem
// Sprach-Schlüssel des Chat-Servers signiert; der Relay (eingebaut oder als eigener Agent)
// prüft es mit dem öffentlichen Schlüssel. Gültig 60 Sekunden, nur einmal verwendbar.
type Ticket struct {
	Channel     string `json:"c"`
	User        string `json:"u"`
	Name        string `json:"n"`
	Speak       bool   `json:"s"`             // darf sprechen
	Muted       bool   `json:"m"`             // vom Team stummgeschaltet
	Hand        bool   `json:"h"`             // hat sich gemeldet (Bühne)
	Bitrate     int    `json:"b"`             // Opus-Bitrate der Voreinstellung (bit/s)
	Stereo      bool   `json:"st"`            // Voreinstellung „Musik“
	MaxSpeakers int    `json:"ms"`            // gleichzeitig weitergeleitete Sprecher
	MaxPeers    int    `json:"mp"`            // Teilnehmer im Kanal
	Screen      bool   `json:"sc,omitempty"`  // darf den Bildschirm teilen
	ScreenKbps  int    `json:"sk,omitempty"`  // Bitrate einer Bildschirmfreigabe (kbit/s)
	MaxScreens  int    `json:"msc,omitempty"` // gleichzeitige Freigaben im Kanal
	Video       bool   `json:"vd,omitempty"`  // darf die Kamera nutzen (Konferenz)
	VideoKbps   int    `json:"vk,omitempty"`  // Bitrate des großen Bildes (kbit/s)
	ThumbKbps   int    `json:"tk,omitempty"`  // Bitrate der Miniansicht (kbit/s)
	Featured    int    `json:"fe,omitempty"`  // so viele Sprecher groß
	Music       int    `json:"mu,omitempty"`  // Musik eines Bots: Lautstärke in Prozent (1–100); eigener Datenstrom
	ModUser     bool   `json:"mo,omitempty"`  // darf im Kanal moderieren (nur Anzeige)
	Exp         int64  `json:"e"`             // Unix-Sekunden
	Nonce       string `json:"x"`
}

const ticketContext = "ucng-voice-ticket-v1\x00"

var (
	ErrTicket        = errors.New("Ungültige oder abgelaufene Zutrittskarte – bitte erneut beitreten")
	ErrTicketExpired = errors.New("Die Zutrittskarte ist abgelaufen – bitte erneut beitreten")
)

// TicketTTL: Gültigkeit einer Zutrittskarte.
const TicketTTL = 60 * time.Second

// SignTicket erzeugt eine signierte Zutrittskarte.
func SignTicket(priv ed25519.PrivateKey, t Ticket, now time.Time) string {
	if t.Exp == 0 {
		t.Exp = now.Add(TicketTTL).Unix()
	}
	if t.Nonce == "" {
		var b [12]byte
		rand.Read(b[:])
		t.Nonce = hex.EncodeToString(b[:])
	}
	raw, _ := json.Marshal(t)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(ticketContext+payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyTicket prüft Signatur und Ablauf (nicht die Einmaligkeit – das macht der Relay).
func VerifyTicket(pub ed25519.PublicKey, token string, now time.Time) (Ticket, error) {
	var t Ticket
	payload, sigS, ok := strings.Cut(token, ".")
	if !ok || len(pub) != ed25519.PublicKeySize || len(token) > 4096 {
		return t, ErrTicket
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigS)
	if err != nil || !ed25519.Verify(pub, []byte(ticketContext+payload), sig) {
		return t, ErrTicket
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &t) != nil {
		return t, ErrTicket
	}
	if t.Exp < now.Unix() {
		return t, ErrTicketExpired
	}
	if t.Exp > now.Add(10*time.Minute).Unix() || t.Channel == "" || t.User == "" || t.Nonce == "" {
		return t, ErrTicket
	}
	return t, nil
}

// EstimateKbps: Bandbreite je Zuhörer (Server → Browser) in kbit/s bei der Bitrate einer
// Voreinstellung und so vielen gleichzeitig weitergeleiteten Sprechern. 50 Takte je Sekunde;
// je Takt rund 72 B Overhead (TCP/IP, TLS, WebSocket, Bündelkopf), je Sprecher 6 B + Opus.
func EstimateKbps(bitrate, speakers int) float64 {
	payload := float64(bitrate) / 8 / 50
	return 50 * 8 * (72 + float64(speakers)*(6+payload)) / 1000
}
