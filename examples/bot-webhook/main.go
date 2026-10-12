// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Beispiel-Bot per Webhook: Der Chat ruft diesen kleinen Server bei jeder neuen Nachricht
// auf. Antwortet er mit {"text": "…"}, erscheint das im selben Kanal.
//
//	UCNG_WEBHOOK_SECRET=… PORT=9000 go run ./examples/bot-webhook
//
// Im Admin-Bereich beim Bot als Webhook-URL eintragen: http://<dieser-rechner>:9000/hook
// Der Chat-Server muss diese Adresse erreichen können.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var secret = os.Getenv("UCNG_WEBHOOK_SECRET")

type event struct {
	Event   string `json:"event"` // message oder ping
	Room    string `json:"room"`
	Message *struct {
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"message"`
}

func main() {
	if secret == "" {
		log.Fatal("Bitte UCNG_WEBHOOK_SECRET setzen (Webhook-Geheimnis aus dem Admin-Bereich unter „Bots“).")
	}
	http.HandleFunc("POST /hook", hook)
	addr := ":" + envOr("PORT", "9000")
	log.Printf("Webhook-Bot wartet auf http://localhost%s/hook", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func hook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Lesefehler", http.StatusBadRequest)
		return
	}
	// 1. Echtheit prüfen: Signatur und Zeitstempel (schützt vor gefälschten und wiederholten Aufrufen).
	ts := r.Header.Get("X-UCNG-Timestamp")
	if !validSignature(ts, body, r.Header.Get("X-UCNG-Signature")) {
		http.Error(w, "Signatur ungültig", http.StatusUnauthorized)
		return
	}
	if sec, err := strconv.ParseInt(ts, 10, 64); err != nil || time.Since(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		http.Error(w, "Zeitstempel zu alt", http.StatusUnauthorized)
		return
	}

	// 2. Ereignis auswerten.
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "Ungültiges JSON", http.StatusBadRequest)
		return
	}
	if ev.Event != "message" || ev.Message == nil {
		w.WriteHeader(http.StatusNoContent) // z. B. "ping" vom Testknopf im Admin-Bereich
		return
	}
	reply := ""
	text := strings.ToLower(strings.TrimSpace(ev.Message.Text))
	switch {
	case text == "!ping":
		reply = "pong 🏓"
	case strings.Contains(text, "öffnungszeiten"):
		reply = "Wir sind Mo–Fr von 9 bis 18 Uhr für dich da."
	}
	if reply == "" {
		w.WriteHeader(http.StatusNoContent) // keine Antwort
		return
	}

	// 3. Antworten: einfach JSON mit "text" zurückgeben.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"text": reply})
}

func validSignature(ts string, body []byte, got string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}
