// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"log"
	"time"
)

// saver schreibt Änderungen zeitnah auf die Platte. Mehrere Änderungen kurz
// hintereinander (z. B. viele Nachrichten) werden zu einem Schreibvorgang gebündelt.
type saver struct {
	store    Backend // nil = Speicherung aus
	hub      *Hub
	users    *Users
	debounce time.Duration
	sig      chan struct{}
}

func newSaver(store Backend, hub *Hub, users *Users, debounce time.Duration) *saver {
	s := &saver{store: store, hub: hub, users: users, debounce: debounce, sig: make(chan struct{}, 1)}
	hub.OnChange(s.notify)
	users.OnChange(s.notify)
	return s
}

// notify blockiert nie – es wird höchstens ein Signal vorgemerkt.
func (s *saver) notify() {
	select {
	case s.sig <- struct{}{}:
	default:
	}
}

func (s *saver) save() {
	if s.store == nil {
		return
	}
	if err := s.store.Flush(s.hub, s.users); err != nil {
		log.Printf("Speichern fehlgeschlagen: %v – neuer Versuch in Kürze", err)
		go func() { time.Sleep(5 * time.Second); s.notify() }()
	}
}

// run läuft bis ctx endet und speichert danach ein letztes Mal.
func (s *saver) run(ctx context.Context) {
	defer func() {
		s.save()
		if s.store != nil {
			s.store.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.sig:
			select { // kurz sammeln
			case <-ctx.Done():
				return
			case <-time.After(s.debounce):
			}
			s.save()
		}
	}
}
