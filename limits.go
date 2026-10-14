// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// Live-Verbindungen (Server-Sent Events) bleiben lange offen und belegen je Verbindung Speicher
// und einen Platz beim Verteilen von Nachrichten. Begrenzt wird insgesamt und je Schlüssel
// (IP-Adresse; bei Bots die Bot-ID).

var ErrTooManyStreams = errors.New("Zu viele gleichzeitige Verbindungen – bitte einige Tabs schließen und gleich noch einmal versuchen")

type streamLimiter struct {
	mu     sync.Mutex
	max    int
	perKey int
	total  int
	byKey  map[string]int
}

func newStreamLimiter(max, perKey int) *streamLimiter {
	return &streamLimiter{max: max, perKey: perKey, byKey: map[string]int{}}
}

// acquire reserviert eine Verbindung; release muss danach genau einmal aufgerufen werden.
func (l *streamLimiter) acquire(key string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.max || l.byKey[key] >= l.perKey {
		return nil, false
	}
	l.total++
	l.byKey[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.total--
			if l.byKey[key]--; l.byKey[key] <= 0 {
				delete(l.byKey, key)
			}
			l.mu.Unlock()
		})
	}, true
}

// Open liefert die Zahl offener Verbindungen (für die Statistik).
func (l *streamLimiter) Open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// streamSlot reserviert einen Platz für einen Live-Stream oder antwortet mit 429.
func (s *Server) streamSlot(w http.ResponseWriter, key string) (func(), bool) {
	release, ok := s.streams.acquire(key)
	if !ok {
		w.Header().Set("Retry-After", "30")
		writeErr(w, http.StatusTooManyRequests, ErrTooManyStreams.Error())
		return nil, false
	}
	// Live-Streams laufen lange: die Lesefrist des Servers (gegen langsame Uploads) gilt hier nicht.
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	return release, true
}
