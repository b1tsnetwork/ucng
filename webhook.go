// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Webhooks: Für Bots mit Webhook-URL schickt der Server jede neue Nachricht aus den
// erlaubten Kanälen per POST an diese URL. Antwortet der Bot mit {"text": "…"}, wird
// das als Bot-Nachricht im selben Kanal veröffentlicht.
//
// Jeder Aufruf ist signiert:
//   X-UCNG-Timestamp: <Unix-Sekunden>          (zusätzlich unter dem früheren Namen X-GoChat-…)
//   X-UCNG-Signature: sha256=<hex(HMAC-SHA256(Geheimnis, Timestamp + "." + Body))>
// So kann der Bot prüfen, dass die Anfrage wirklich vom Chat kommt und nicht wiederholt wurde.

type webhooks struct {
	hub    *Hub
	client *http.Client
	say    func(b Bot, room, text string, replyTo int64) (Message, error)

	mu     sync.Mutex
	queues map[string]chan botEvent // eine Warteschlange pro Bot: Reihenfolge bleibt erhalten
}

func newWebhooks(hub *Hub, say func(Bot, string, string, int64) (Message, error)) *webhooks {
	return &webhooks{
		hub: hub, say: say, queues: map[string]chan botEvent{},
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

// run verteilt Nachrichten an alle Bots mit Webhook, bis ctx endet.
func (wh *webhooks) run(ctx context.Context) {
	ch := wh.hub.Listen(1024)
	defer wh.hub.Unlisten(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			if ev.Type != "message" {
				continue // Webhooks bekommen nur neue Nachrichten
			}
			for _, b := range wh.hub.Bots().Webhooks() {
				if b.wants(ev) {
					wh.enqueue(ctx, b.ID, botEvent{Event: "message", Room: ev.Room, Message: ev.Message})
				}
			}
		}
	}
}

func (wh *webhooks) enqueue(ctx context.Context, botID string, ev botEvent) {
	wh.mu.Lock()
	q, ok := wh.queues[botID]
	if !ok {
		q = make(chan botEvent, 256)
		wh.queues[botID] = q
		go wh.worker(ctx, botID, q)
	}
	wh.mu.Unlock()
	select {
	case q <- ev:
	default:
		log.Printf("Webhook %s: Warteschlange voll – Nachricht übersprungen (Bot antwortet zu langsam?)", botID)
	}
}

// worker arbeitet die Warteschlange eines Bots der Reihe nach ab und beendet sich nach Leerlauf.
func (wh *webhooks) worker(ctx context.Context, botID string, q chan botEvent) {
	idle := time.NewTimer(5 * time.Minute)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			wh.mu.Lock()
			if len(q) == 0 {
				delete(wh.queues, botID)
				wh.mu.Unlock()
				return
			}
			wh.mu.Unlock()
			idle.Reset(5 * time.Minute)
		case ev := <-q:
			b, ok := wh.hub.Bots().Get(botID) // aktuellen Stand nehmen (URL, Geheimnis, Rechte)
			if !ok || !b.Enabled || b.WebhookURL == "" {
				continue
			}
			_, reply := wh.deliverRetry(ctx, b, ev)
			if reply != nil && reply.Text != "" {
				room := ev.Room
				if reply.Room != "" {
					room = NormRoom(reply.Room)
				}
				if _, err := wh.say(b, room, reply.Text, reply.ReplyTo); err != nil {
					log.Printf("Webhook-Antwort von %s nicht veröffentlicht: %v", b.Name, err)
				}
			}
			idle.Reset(5 * time.Minute)
		}
	}
}

// webhookReply ist die optionale Antwort eines Bots.
type webhookReply struct {
	Text    string `json:"text"`
	Room    string `json:"room"`    // optional: in einem anderen (erlaubten) Kanal antworten
	ReplyTo int64  `json:"replyTo"` // optional: als Antwort auf diese Nachricht
}

// deliverRetry versucht es bei Netzwerkfehlern oder 5xx ein zweites Mal.
func (wh *webhooks) deliverRetry(ctx context.Context, b Bot, ev botEvent) (Delivery, *webhookReply) {
	d, reply := wh.deliver(ctx, b, ev)
	if d.Error != "" || d.Status >= 500 {
		select {
		case <-ctx.Done():
			return d, nil
		case <-time.After(2 * time.Second):
		}
		d, reply = wh.deliver(ctx, b, ev)
	}
	return d, reply
}

// Sign berechnet die Signatur – auch für eigene Bots nützlich (siehe examples/).
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (wh *webhooks) deliver(ctx context.Context, b Bot, ev botEvent) (Delivery, *webhookReply) {
	start := time.Now()
	d := Delivery{At: start.UTC(), Event: ev.Event}
	defer func() {
		d.DurationMS = time.Since(start).Milliseconds()
		wh.hub.Bots().recordDelivery(b.ID, d)
	}()

	body, _ := json.Marshal(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.WebhookURL, bytes.NewReader(body))
	if err != nil {
		d.Error = err.Error()
		return d, nil
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "UCNG-Webhook/1")
	sig := Sign(b.WebhookSecret, ts, body)
	for _, p := range []string{"X-UCNG-", "X-GoChat-"} { // frühere Namen für bestehende Bots
		req.Header.Set(p+"Event", ev.Event)
		req.Header.Set(p+"Bot", b.ID)
		req.Header.Set(p+"Timestamp", ts)
		req.Header.Set(p+"Signature", sig)
	}

	res, err := wh.client.Do(req)
	if err != nil {
		d.Error = shortErr(err)
		return d, nil
	}
	defer res.Body.Close()
	d.Status = res.StatusCode
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		d.Error = fmt.Sprintf("Antwort mit Status %d", res.StatusCode)
		return d, nil
	}
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt != "application/json" || len(bytes.TrimSpace(data)) == 0 {
		return d, nil // keine Antwort gewünscht
	}
	var reply webhookReply
	if err := json.Unmarshal(data, &reply); err != nil {
		d.Error = "Antwort ist kein gültiges JSON"
		return d, nil
	}
	return d, &reply
}

// shortErr macht Netzwerkfehler für die Anzeige im Admin-Bereich lesbar.
func shortErr(err error) string {
	s := err.Error()
	switch {
	case bytes.Contains([]byte(s), []byte("Client.Timeout")), bytes.Contains([]byte(s), []byte("deadline exceeded")):
		return "Zeitüberschreitung (keine Antwort innerhalb von 8 Sekunden)"
	case bytes.Contains([]byte(s), []byte("connection refused")):
		return "Verbindung abgelehnt – läuft der Bot?"
	case bytes.Contains([]byte(s), []byte("no such host")):
		return "Adresse nicht gefunden"
	}
	return s
}
