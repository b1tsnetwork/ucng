// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"sync"
	"time"
)

// Protokoll: Wer hat wann was im Admin-Bereich getan? Jede Aktion wird als eine Zeile an
// data/audit.jsonl angehängt. Bei gesetzter Aufbewahrungsfrist werden alte Einträge mit gelöscht.

type AuditEntry struct {
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor"`          // Anmeldename oder "bot:<Name>" / "automod" / "system"
	Display  string    `json:"display"`        // lesbarer Name
	Action   string    `json:"action"`         // z. B. message.delete, user.mute, room.create
	Room     string    `json:"room,omitempty"` // betroffener Kanal
	Message  int64     `json:"message,omitempty"`
	UserID   string    `json:"userId,omitempty"` // betroffene Person
	UserName string    `json:"userName,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

type Audit struct {
	mu      sync.Mutex
	entries []AuditEntry // älteste zuerst
	record  func(AuditEntry)
	rewrite func()
}

func NewAudit() *Audit { return &Audit{} }

func (a *Audit) Add(e AuditEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	a.mu.Lock()
	a.entries = append(a.entries, e)
	a.mu.Unlock()
	if a.record != nil {
		a.record(e)
	}
}

// Query liefert Einträge, neueste zuerst. q durchsucht Aktion, Person, Kanal und Details.
// before ist ein Index-Cursor (0 = neueste).
func (a *Audit) Query(q string, before, limit int) ([]AuditEntry, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q = strings.ToLower(strings.TrimSpace(q))
	end := len(a.entries)
	if before > 0 && before < end {
		end = before
	}
	out := []AuditEntry{}
	i := end - 1
	for ; i >= 0 && len(out) < limit; i-- {
		e := a.entries[i]
		if q == "" || strings.Contains(strings.ToLower(e.Action+" "+e.Display+" "+e.Actor+" "+e.Room+" "+e.UserName+" "+e.Detail), q) {
			out = append(out, e)
		}
	}
	next := 0
	if i >= 0 {
		next = i + 1
	}
	return out, next
}

// Scrub entfernt personenbezogene Angaben aus passenden Einträgen (nach endgültigem Löschen).
func (a *Audit) Scrub(match func(*AuditEntry) bool, clean func(*AuditEntry)) {
	a.mu.Lock()
	n := 0
	for i := range a.entries {
		if match(&a.entries[i]) {
			clean(&a.entries[i])
			n++
		}
	}
	a.mu.Unlock()
	if n > 0 && a.rewrite != nil {
		a.rewrite()
	}
}

// Trim entfernt Einträge, die älter als cutoff sind.
func (a *Audit) Trim(cutoff time.Time) bool {
	a.mu.Lock()
	n := 0
	for n < len(a.entries) && a.entries[n].Time.Before(cutoff) {
		n++
	}
	if n > 0 {
		a.entries = append([]AuditEntry(nil), a.entries[n:]...)
	}
	a.mu.Unlock()
	if n > 0 && a.rewrite != nil {
		a.rewrite()
	}
	return n > 0
}

func (a *Audit) all() []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEntry(nil), a.entries...)
}

func (a *Audit) restore(list []AuditEntry) {
	a.mu.Lock()
	a.entries = list
	a.mu.Unlock()
}
