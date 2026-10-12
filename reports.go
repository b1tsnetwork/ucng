// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Meldungen: Personen melden eine Nachricht, sie landet in der Prüfliste der Moderation.

type Report struct {
	ID           string `json:"id"`
	Room         string `json:"room"`
	MessageID    int64  `json:"messageId"`
	ReporterID   string `json:"reporterId"`
	ReporterName string `json:"reporterName"`
	Reason       string `json:"reason,omitempty"`
	// Verschlüsselte Nachricht: der Text, den der Browser der meldenden Person entschlüsselt hat
	// (vom Server nicht überprüfbar).
	Plaintext string     `json:"plaintext,omitempty"`
	Created   time.Time  `json:"created"`
	Status    string     `json:"status"`           // open, done
	Action    string     `json:"action,omitempty"` // dismiss, delete
	By        string     `json:"by,omitempty"`
	Closed    *time.Time `json:"closed,omitempty"`
}

var (
	ErrReportOwn   = errors.New("Eigene Nachrichten kannst du nicht melden")
	ErrReported    = errors.New("Du hast diese Nachricht schon gemeldet")
	ErrNoReport    = errors.New("Diese Meldung gibt es nicht")
	ErrReportText  = errors.New("Grund: höchstens 300 Zeichen")
	maxDoneReports = 500
)

type Reports struct {
	mu      sync.Mutex
	list    []*Report
	changed func()
}

func NewReports() *Reports { return &Reports{} }

func (rp *Reports) touch() {
	if rp.changed != nil {
		rp.changed()
	}
}

// Add legt eine Meldung an (eine offene je Person und Nachricht).
func (rp *Reports) Add(room string, msgID int64, reporterID, reporterName, reason string) (Report, error) {
	return rp.AddE2E(room, msgID, reporterID, reporterName, reason, "")
}

// AddE2E wie Add, mit dem entschlüsselten Text einer verschlüsselten Nachricht.
func (rp *Reports) AddE2E(room string, msgID int64, reporterID, reporterName, reason, plaintext string) (Report, error) {
	if len([]rune(reason)) > 300 {
		return Report{}, ErrReportText
	}
	rp.mu.Lock()
	for _, x := range rp.list {
		if x.MessageID == msgID && x.ReporterID == reporterID && x.Status == "open" {
			rp.mu.Unlock()
			return Report{}, ErrReported
		}
	}
	r := &Report{ID: "rep_" + randomHex(5), Room: room, MessageID: msgID, ReporterID: reporterID,
		ReporterName: reporterName, Reason: reason, Plaintext: plaintext, Created: time.Now().UTC(), Status: "open"}
	rp.list = append(rp.list, r)
	rp.mu.Unlock()
	rp.touch()
	return *r, nil
}

// List liefert offene (open = true) oder erledigte Meldungen, neueste zuerst.
func (rp *Reports) List(open bool) []Report {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := []Report{}
	for _, x := range rp.list {
		if (x.Status == "open") == open {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (rp *Reports) OpenCount() int {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	n := 0
	for _, x := range rp.list {
		if x.Status == "open" {
			n++
		}
	}
	return n
}

func (rp *Reports) Get(id string) (Report, bool) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	for _, x := range rp.list {
		if x.ID == id {
			return *x, true
		}
	}
	return Report{}, false
}

// CloseMessage schließt alle offenen Meldungen zu einer Nachricht.
func (rp *Reports) CloseMessage(room string, msgID int64, action, by string) int {
	rp.mu.Lock()
	n := 0
	now := time.Now().UTC()
	for _, x := range rp.list {
		if x.MessageID == msgID && x.Room == room && x.Status == "open" {
			x.Status, x.Action, x.By, x.Closed = "done", action, by, &now
			n++
		}
	}
	rp.trimLocked()
	rp.mu.Unlock()
	if n > 0 {
		rp.touch()
	}
	return n
}

// Close schließt eine Meldung (und alle anderen zur selben Nachricht).
func (rp *Reports) Close(id, action, by string) (Report, error) {
	r, ok := rp.Get(id)
	if !ok {
		return Report{}, ErrNoReport
	}
	rp.CloseMessage(r.Room, r.MessageID, action, by)
	r, _ = rp.Get(id)
	return r, nil
}

// renameRoom / removeRoom halten Meldungen beim Umbenennen und Löschen eines Kanals aktuell.
func (rp *Reports) renameRoom(old, name string) {
	rp.mu.Lock()
	for _, x := range rp.list {
		if x.Room == old {
			x.Room = name
		}
	}
	rp.mu.Unlock()
	rp.touch()
}

// ScrubUser entfernt eine Person aus den Meldungen (endgültig löschen / DSGVO).
func (rp *Reports) ScrubUser(userID string) {
	rp.mu.Lock()
	keep := rp.list[:0]
	for _, x := range rp.list {
		if x.ReporterID == userID {
			continue
		}
		keep = append(keep, x)
	}
	rp.list = keep
	rp.mu.Unlock()
	rp.touch()
}

func (rp *Reports) trimLocked() {
	done := 0
	for i := len(rp.list) - 1; i >= 0; i-- {
		if rp.list[i].Status != "open" {
			done++
			if done > maxDoneReports {
				rp.list = append(rp.list[:i], rp.list[i+1:]...)
			}
		}
	}
}

func (rp *Reports) export() []Report {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := make([]Report, len(rp.list))
	for i, x := range rp.list {
		out[i] = *x
	}
	return out
}

func (rp *Reports) restore(list []Report) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.list = nil
	for i := range list {
		x := list[i]
		if x.ID != "" && x.MessageID > 0 {
			rp.list = append(rp.list, &x)
		}
	}
}
