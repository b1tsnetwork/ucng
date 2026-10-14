// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamLimits(t *testing.T) {
	ts, _ := testServerWith(t, func(c *Config) { c.MaxStreams = 3; c.MaxStreamsIP = 2 })
	open := func() *http.Response {
		res, err := http.Get(ts.URL + "/api/rooms/lobby/events")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a, b := open(), open()
	if a.StatusCode != 200 || b.StatusCode != 200 {
		t.Fatalf("%d %d", a.StatusCode, b.StatusCode)
	}
	c := open()
	if c.StatusCode != 429 || c.Header.Get("Retry-After") == "" {
		t.Fatalf("dritte Verbindung: %d", c.StatusCode)
	}
	c.Body.Close()
	a.Body.Close() // Platz wird frei
	deadline := time.Now().Add(2 * time.Second)
	for {
		d := open()
		if d.StatusCode == 200 {
			d.Body.Close()
			break
		}
		d.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("Platz nach dem Schließen nicht frei geworden")
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.Body.Close()
}

func TestStreamLimiterGlobal(t *testing.T) {
	l := newStreamLimiter(2, 5)
	r1, ok1 := l.acquire("a")
	_, ok2 := l.acquire("b")
	_, ok3 := l.acquire("c")
	if !ok1 || !ok2 || ok3 {
		t.Fatal("globale Grenze")
	}
	r1()
	r1() // doppelt freigeben schadet nicht
	if _, ok := l.acquire("c"); !ok || l.Open() != 2 {
		t.Fatalf("nach Freigabe: %d", l.Open())
	}
}

// Direktnachricht-Hinweise gehen nur an die betroffene Person, nicht an alle Verbindungen.
func TestNotifyUserOnlyTarget(t *testing.T) {
	ts, _ := testServer(t)
	a, b, c := claim(t, ts.URL, "Anna"), claim(t, ts.URL, "Ben"), claim(t, ts.URL, "Carla")
	bs, cs := sess(t, ts.URL, b), sess(t, ts.URL, c)
	listen := func(viewer string) (chan string, func()) {
		res, err := http.Get(ts.URL + "/api/rooms/lobby/events?viewer=" + viewer)
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan string, 64)
		go func() {
			sc := bufio.NewScanner(res.Body)
			for sc.Scan() {
				ch <- sc.Text()
			}
			close(ch)
		}()
		return ch, func() { res.Body.Close() }
	}
	bch, stopB := listen(bs.Viewer)
	defer stopB()
	cch, stopC := listen(cs.Viewer)
	defer stopC()
	time.Sleep(100 * time.Millisecond)
	dm := decode[DMInfo](t, expect(t, post(t, ts.URL+"/api/dms", a, `{"userId":"`+bs.ID+`"}`), 200, "DM"))
	expect(t, post(t, ts.URL+"/api/rooms/"+dm.Room+"/messages", a, `{"text":"hallo"}`), 201, "schreiben").Body.Close()
	got := func(ch chan string) bool {
		timeout := time.After(800 * time.Millisecond)
		for {
			select {
			case l, ok := <-ch:
				if !ok {
					return false
				}
				if strings.Contains(l, `"type":"dm"`) {
					return true
				}
			case <-timeout:
				return false
			}
		}
	}
	if !got(bch) {
		t.Fatal("Ben bekommt keinen Hinweis")
	}
	if got(cch) {
		t.Fatal("Carla sieht den DM-Hinweis für Ben")
	}
}
