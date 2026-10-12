// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Beispiel-Bot mit Live-Verbindung: liest alle Nachrichten in seinen Kanälen mit und
// beantwortet Befehle wie !hilfe, !ping, !zeit und !würfel.
//
//	UCNG_URL=http://localhost:8080 UCNG_BOT_TOKEN=gcb_… go run ./examples/bot-stream
//
// Der Bot verbindet sich selbst mit dem Chat – er braucht keine öffentliche Adresse und
// funktioniert auch hinter einer Firewall oder auf dem eigenen Rechner.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type message struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
	Bot  bool   `json:"bot"`
}

type event struct {
	Event   string   `json:"event"` // ready, message, update, revoked
	Room    string   `json:"room"`
	Message *message `json:"message"`
	Bot     *struct {
		Name string `json:"name"`
	} `json:"bot"`
}

var (
	base  = strings.TrimRight(env("UCNG_URL", "http://localhost:8080"), "/")
	token = os.Getenv("UCNG_BOT_TOKEN")
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if token == "" {
		log.Fatal("Bitte UCNG_BOT_TOKEN setzen (API-Schlüssel aus dem Admin-Bereich unter „Bots“).")
	}
	wait := time.Second
	for {
		err := listen()
		if err == errRevoked {
			log.Fatal("Der Bot wurde im Admin-Bereich deaktiviert oder gelöscht.")
		}
		log.Printf("Verbindung getrennt (%v) – neuer Versuch in %v", err, wait)
		time.Sleep(wait)
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

var errRevoked = fmt.Errorf("Zugang entzogen")

// listen hält die Live-Verbindung offen und verarbeitet eingehende Ereignisse.
func listen() error {
	req, _ := http.NewRequest("GET", base+"/api/bot/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return errRevoked
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Status %d", res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // Kommentare (": ping") und event:-Zeilen überspringen
		}
		var ev event
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "ready":
			log.Printf("Verbunden als %s", ev.Bot.Name)
		case "revoked":
			return errRevoked
		case "message":
			if ev.Message != nil && !ev.Message.Bot {
				if reply := answer(ev.Message); reply != "" {
					if err := say(ev.Room, reply); err != nil {
						log.Printf("Antwort in #%s fehlgeschlagen: %v", ev.Room, err)
					}
				}
			}
		}
	}
	return sc.Err()
}

// answer entscheidet, ob und was der Bot antwortet.
func answer(m *message) string {
	cmd := strings.ToLower(strings.Fields(m.Text + " ")[0])
	switch cmd {
	case "!hilfe", "!help":
		return "Ich kann: !ping, !zeit, !würfel"
	case "!ping":
		return "pong 🏓"
	case "!zeit":
		loc, err := time.LoadLocation("Europe/Berlin")
		if err != nil {
			loc = time.Local
		}
		return "Es ist " + time.Now().In(loc).Format("15:04") + " Uhr."
	case "!würfel", "!wuerfel":
		return fmt.Sprintf("%s würfelt eine %d 🎲", m.Name, rand.Intn(6)+1)
	}
	return ""
}

// say schreibt eine Nachricht in einen Kanal.
func say(room, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	req, _ := http.NewRequest("POST", base+"/api/bot/rooms/"+url.PathEscape(room)+"/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		var e struct{ Error string }
		json.NewDecoder(res.Body).Decode(&e)
		return fmt.Errorf("Status %d: %s", res.StatusCode, e.Error)
	}
	return nil
}
