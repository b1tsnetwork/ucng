// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build loadtest

// Last- und Speichermessungen (nicht Teil der normalen Tests):
//
//	go test -tags loadtest -run Load -v -timeout 30m .
//	LOAD_N=1000000 LOAD_CLIENTS=2000 go test -tags loadtest -run Load -v -timeout 60m .
//
// Gemessen werden: Arbeitsspeicher je Nachricht, Kaltstart (Dateispeicher), Dauer von Suche
// und Statistik, Latenz beim Senden während paralleler Suchen und die Verteilung einer
// Nachricht an viele Live-Verbindungen.
package main

import (
	"bufio"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func loadN(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

var loadWords = strings.Fields("hallo heute morgen danke super prima genau vielleicht später termin projekt " +
	"frage antwort kaffee meeting server update fehler idee bild woche wochenende chat kanal nachricht " +
	"bitte gerne alles klar okay ja nein warum wann wo wie gut schlecht schnell langsam neu alt")

func loadText(r *rand.Rand) string {
	n := 6 + r.Intn(14)
	w := make([]string, n)
	for i := range w {
		w[i] = loadWords[r.Intn(len(loadWords))]
	}
	return strings.Join(w, " ")
}

// fillHub legt n Nachrichten in rooms Kanälen an (über 90 Tage verteilt).
func fillHub(h *Hub, n, rooms int) {
	r := rand.New(rand.NewSource(1))
	start := time.Now().Add(-90 * 24 * time.Hour)
	step := 90 * 24 * time.Hour / time.Duration(n)
	per := make([][]Message, rooms)
	for i := 0; i < n; i++ {
		u := r.Intn(500)
		m := Message{ID: int64(i + 1), UserID: fmt.Sprintf("u%05d", u), Name: fmt.Sprintf("Person %d", u),
			Text: loadText(r), Time: start.Add(time.Duration(i) * step)}
		k := r.Intn(rooms)
		per[k] = append(per[k], m)
	}
	for k := 0; k < rooms; k++ {
		h.putRoom(roomMeta{Name: fmt.Sprintf("kanal-%02d", k), Created: start}, per[k])
	}
	h.nextID.Store(int64(n))
}

func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapInuse) / (1 << 20)
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}

func TestLoadMemoryAndLocks(t *testing.T) {
	n := loadN("LOAD_N", 200000)
	base := heapMB()
	h := NewHub(0)
	t0 := time.Now()
	fillHub(h, n, 20)
	t.Logf("%d Nachrichten angelegt in %v", n, time.Since(t0).Round(time.Millisecond))
	used := heapMB() - base
	t.Logf("Arbeitsspeicher: %.0f MB (%.0f Byte je Nachricht)", used, used*(1<<20)/float64(n))

	t0 = time.Now()
	hits, _ := h.Search("", "gibtesnichtxyz", true, 100)
	t.Logf("Admin-Suche über alle Kanäle (kein Treffer): %v (%d)", time.Since(t0).Round(time.Millisecond), len(hits))
	t0 = time.Now()
	h.Search("kanal-01", "gibtesnichtxyz", false, 50)
	t.Logf("Suche in einem Kanal: %v", time.Since(t0).Round(time.Millisecond))
	t0 = time.Now()
	h.Stats(30, time.Now())
	t.Logf("Statistik 30 Tage: %v", time.Since(t0).Round(time.Millisecond))

	// Senden, während parallel gesucht wird
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.Search("", "gibtesnichtxyz", true, 100)
				}
			}
		}()
	}
	var lat []time.Duration
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 100; i++ {
		p := Poster{ID: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("Sender %d", i), Team: true}
		t1 := time.Now()
		if _, err := h.SendFull("kanal-03", p, SendInput{Text: loadText(r)}); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(t1))
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	t.Logf("Senden während 4 paralleler Admin-Suchen: p50 %v, p95 %v, p99 %v, max %v",
		pct(lat, .5), pct(lat, .95), pct(lat, .99), pct(lat, 1))
}

func TestLoadColdStart(t *testing.T) {
	n := loadN("LOAD_N", 200000)
	dir := t.TempDir()
	{
		h, u, st := NewHub(0), NewUsers(time.Hour, 1000), NewStore(dir)
		if err := st.Load(h, u); err != nil {
			t.Fatal(err)
		}
		h.EnablePersistence()
		fillHub(h, n, 20)
		for _, ri := range h.Rooms() {
			h.record(storeOp{kind: opRoomMeta, room: ri.Name})
			h.record(storeOp{kind: opRoomRewrite, room: ri.Name})
		}
		if err := st.Flush(h, u); err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	var size int64
	entries, _ := os.ReadDir(dir + "/rooms")
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			size += fi.Size()
		}
	}
	base := heapMB()
	t0 := time.Now()
	h, u, st := NewHub(0), NewUsers(time.Hour, 1000), NewStore(dir)
	if err := st.Load(h, u); err != nil {
		t.Fatal(err)
	}
	t.Logf("Kaltstart mit %d Nachrichten (%.0f MB auf der Platte): %v, danach %.0f MB im Speicher",
		n, float64(size)/(1<<20), time.Since(t0).Round(time.Millisecond), heapMB()-base)
	runtime.KeepAlive(h)
	st.Close()
}

func TestLoadSSEFanout(t *testing.T) {
	clients := loadN("LOAD_CLIENTS", 1000)
	ts, srv := testServerWith(t, func(c *Config) { c.MaxStreams = clients + 10; c.MaxStreamsIP = clients + 10 })
	srv.hub.CreateRoomWith(RoomInput{Name: strp("last")})
	tr := &http.Transport{MaxIdleConnsPerHost: clients + 10, MaxConnsPerHost: 0}
	cl := &http.Client{Transport: tr}
	base := heapMB()
	got := make(chan time.Time, clients*4)
	var ready sync.WaitGroup
	var bodies []interface{ Close() error }
	var mu sync.Mutex
	for i := 0; i < clients; i++ {
		ready.Add(1)
		go func() {
			res, err := cl.Get(ts.URL + "/api/rooms/last/events")
			if err != nil || res.StatusCode != 200 {
				ready.Done()
				return
			}
			mu.Lock()
			bodies = append(bodies, res.Body)
			mu.Unlock()
			sc := bufio.NewScanner(res.Body)
			sc.Buffer(make([]byte, 4<<10), 1<<20)
			first := true
			for sc.Scan() {
				l := sc.Text()
				if first && strings.Contains(l, `"type":"history"`) {
					first = false
					ready.Done()
				}
				if strings.Contains(l, `"type":"message"`) {
					got <- time.Now()
				}
			}
		}()
	}
	ready.Wait()
	time.Sleep(500 * time.Millisecond) // Online-Listen sind gebündelt
	t.Logf("%d offene Live-Verbindungen (Server und Test-Clients im selben Prozess): +%.0f MB", srv.streams.Open(), heapMB()-base)
	var lat []time.Duration
	for k := 0; k < 5; k++ {
		t0 := time.Now()
		if _, err := srv.hub.SendFull("last", Poster{ID: "x", Name: "Ansage", Team: true}, SendInput{Text: fmt.Sprintf("Hallo %d", k)}); err != nil {
			t.Fatal(err)
		}
		var last time.Time
		for i := 0; i < clients; i++ {
			select {
			case at := <-got:
				last = at
			case <-time.After(10 * time.Second):
				t.Fatalf("nur %d von %d Clients haben die Nachricht bekommen", i, clients)
			}
		}
		lat = append(lat, last.Sub(t0))
	}
	t.Logf("Nachricht an alle %d Verbindungen verteilt: Median %v, max %v", clients, pct(lat, .5), pct(lat, 1))
	mu.Lock()
	for _, b := range bodies {
		b.Close()
	}
	mu.Unlock()
}
