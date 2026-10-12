// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fedsig: Signaturen für Anfragen zwischen UCNG-Servern (Föderation) und an den Beacon.
//
// Signiert wird mit dem Ed25519-Serverschlüssel der Instanz (siehe /.well-known/ucng):
//
//	<kontext>\n<absender-domain>\n<unix-zeit>\n<methode>\n<ziel-host+pfad>\n<sha256(inhalt) hex>
//
// Übertragen in den Kopfzeilen X-UCNG-Domain, X-UCNG-Date und X-UCNG-Signature (frühere Namen
// X-GoChat-* werden zusätzlich gesetzt und angenommen). Die signierten Kontexte („gochat-fed-v1“ …)
// bleiben unverändert – sie sind Teil der Signatur, nicht des Namens.
package fedsig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderDomain    = "X-UCNG-Domain"
	HeaderDate      = "X-UCNG-Date"
	HeaderSignature = "X-UCNG-Signature"
	MaxSkew         = 5 * time.Minute

	// Öffentliche Angaben einer Instanz (Serverschlüssel) und Kennungen der Programme.
	WellKnown      = "/.well-known/ucng"
	Software       = "ucng"
	BeaconSoftware = "ucng-beacon"

	// Frühere Namen (GoChat): werden weiter angenommen bzw. mitgeschickt, damit Instanzen
	// verschiedener Versionen sich während eines Updates verstehen.
	LegacyWellKnown      = "/.well-known/gochat"
	LegacySoftware       = "gochat"
	LegacyBeaconSoftware = "gochat-beacon"
	legacyDomain         = "X-GoChat-Domain"
	legacyDate           = "X-GoChat-Date"
	legacySignature      = "X-GoChat-Signature"
)

// SetHeaders setzt die Signatur-Kopfzeilen (neue und frühere Namen).
func SetHeaders(h http.Header, domain, ts, sig string) {
	h.Set(HeaderDomain, domain)
	h.Set(HeaderDate, ts)
	h.Set(HeaderSignature, sig)
	h.Set(legacyDomain, domain)
	h.Set(legacyDate, ts)
	h.Set(legacySignature, sig)
}

// FromRequest liest die Signatur-Kopfzeilen (neue Namen zuerst).
func FromRequest(r *http.Request) (domain, ts, sig string) {
	get := func(name, legacy string) string {
		if v := r.Header.Get(name); v != "" {
			return v
		}
		return r.Header.Get(legacy)
	}
	return get(HeaderDomain, legacyDomain), get(HeaderDate, legacyDate), get(HeaderSignature, legacySignature)
}

// IsInstance: Kennung einer Chat-Instanz in /.well-known (auch frühere Versionen).
func IsInstance(software string) bool { return software == Software || software == LegacySoftware }

// IsBeacon: Kennung eines Beacons in /api/v1/info (auch frühere Versionen).
func IsBeacon(software string) bool {
	return software == BeaconSoftware || software == LegacyBeaconSoftware
}

// String ist der signierte Text.
func String(context, domain, ts, method, target string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{context, domain, ts, method, target, hex.EncodeToString(sum[:])}, "\n"))
}

// Sign liefert Zeitstempel und Signatur (Base64).
func Sign(priv ed25519.PrivateKey, context, domain, method, target string, body []byte, now time.Time) (ts, sig string) {
	ts = strconv.FormatInt(now.Unix(), 10)
	return ts, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, String(context, domain, ts, method, target, body)))
}

// Verify prüft eine Signatur mit dem öffentlichen Schlüssel (Base64).
func Verify(key, context, domain, ts, method, target string, body []byte, sig string) bool {
	pk, err := base64.StdEncoding.DecodeString(key)
	s, err2 := base64.StdEncoding.Strict().DecodeString(sig)
	if err != nil || err2 != nil || len(pk) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(pk, String(context, domain, ts, method, target, body), s)
}

// Fresh: liegt der Zeitstempel innerhalb der erlaubten Abweichung?
func Fresh(ts string, now time.Time) bool {
	sec, err := strconv.ParseInt(ts, 10, 64)
	return err == nil && now.Sub(time.Unix(sec, 0)).Abs() <= MaxSkew
}

// Fingerprint: kurze, lesbare Kennung eines Schlüssels.
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	h := hex.EncodeToString(sum[:16])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, " ")
}
