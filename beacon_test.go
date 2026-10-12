// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"ucng/internal/fedsig"
)

// fakeBeacon prüft Anmeldungen mit dem Schlüssel aus /.well-known/gochat der Instanz und
// liefert eine Sammlung mit einem GIF.
type fakeBeacon struct {
	ts       *httptest.Server
	mu       sync.Mutex
	regs     []map[string]any
	chatBase string
	gif      []byte
}

func newFakeBeacon(t *testing.T) *fakeBeacon {
	fb := &fakeBeacon{gif: testGIF(60, 40, 2)}
	fb.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/register":
			body, _ := io.ReadAll(r.Body)
			res, err := http.Get(fb.chatBase + "/.well-known/gochat")
			if err != nil {
				http.Error(w, `{"error":"nicht erreichbar"}`, 502)
				return
			}
			var wk struct{ Key, Domain string }
			json.NewDecoder(res.Body).Decode(&wk)
			res.Body.Close()
			dom := r.Header.Get(fedsig.HeaderDomain)
			if dom != wk.Domain || !fedsig.Verify(wk.Key, beaconContext, dom, r.Header.Get(fedsig.HeaderDate), "POST", r.Host+"/api/v1/register", body, r.Header.Get(fedsig.HeaderSignature)) {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"Signatur ungültig"}`))
				return
			}
			var m map[string]any
			json.Unmarshal(body, &m)
			fb.mu.Lock()
			fb.regs = append(fb.regs, m)
			fb.mu.Unlock()
			w.Write([]byte(`{"ok":true,"trust":"recommended"}`))
		case r.URL.Path == "/api/v1/info":
			w.Write([]byte(`{"software":"gochat-beacon","name":"Test-Beacon","url":"` + fb.ts.URL + `"}`))
		case r.URL.Path == "/api/v1/collections":
			w.Write([]byte(`[{"id":"c_12ab34cd","name":"Reaktionen","count":1}]`))
		case r.URL.Path == "/api/v1/gifs":
			if !strings.Contains(r.URL.Query().Get("collection"), "c_12ab34cd") {
				w.Write([]byte(`{"gifs":[],"total":0}`))
				return
			}
			w.Write([]byte(`{"gifs":[{"id":"b_0123456789","title":"Hurra","w":60,"h":40,"url":"` + fb.ts.URL + `/media/b_0123456789.gif","credit":"  GIFs   vom Alpenraum "}],"total":1}`))
		case r.URL.Path == "/api/v1/gifs/b_0123456789":
			w.Write([]byte(`{"id":"b_0123456789","title":"Hurra","w":60,"h":40,"url":"` + fb.ts.URL + `/media/b_0123456789.gif"}`))
		case r.URL.Path == "/media/b_0123456789.gif":
			w.Header().Set("Content-Type", "image/gif")
			w.Write(fb.gif)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fb.ts.Close)
	return fb
}

func TestBeaconRegistration(t *testing.T) {
	fb := newFakeBeacon(t)
	// Die Instanz braucht ihre eigene Adresse vor dem Start: Listener zuerst öffnen.
	l := httptest.NewUnstartedServer(nil)
	pub := "http://" + l.Listener.Addr().String()
	ts, srv := testServerWith(t, func(c *Config) { c.PublicURL = pub; c.FedInsecure = true })
	l.Config.Handler = ts.Config.Handler
	l.Start()
	t.Cleanup(l.Close)
	fb.chatBase = pub

	if _, err := srv.hub.Beacon().Set(BeaconSettings{URL: fb.ts.URL, Listed: true, Language: "de", Country: "de", Description: "  Ein   Test "}); err != nil {
		t.Fatal(err)
	}
	srv.beaconSync(context.Background())
	st := srv.hub.Beacon().Get()
	if st.LastErr != "" || st.WasListed != fb.ts.URL || st.Trust != "recommended" {
		t.Fatalf("Status: %+v", st)
	}
	fb.mu.Lock()
	reg := fb.regs[len(fb.regs)-1]
	fb.mu.Unlock()
	if reg["language"] != "de" || reg["country"] != "DE" || reg["listed"] != true || reg["description"] != "Ein Test" || reg["url"] != pub {
		t.Fatalf("Angaben: %+v", reg)
	}

	// Ausschalten meldet ab
	srv.hub.Beacon().Set(BeaconSettings{URL: fb.ts.URL, Listed: false})
	srv.beaconSync(context.Background())
	fb.mu.Lock()
	last := fb.regs[len(fb.regs)-1]
	fb.mu.Unlock()
	if last["listed"] != false || srv.hub.Beacon().Get().WasListed != "" || srv.hub.Beacon().Get().Trust != "" {
		t.Fatalf("Abmeldung: %+v / %+v", last, srv.hub.Beacon().Get())
	}

	// ungültige Angaben
	if _, err := srv.hub.Beacon().Set(BeaconSettings{URL: fb.ts.URL, Language: "deutsch"}); err == nil {
		t.Fatal("Sprache nicht geprüft")
	}
	if _, err := srv.hub.Beacon().Set(BeaconSettings{URL: "ftp://x"}); err == nil {
		t.Fatal("Adresse nicht geprüft")
	}
}

func TestBeaconListedNeedsPublicURL(t *testing.T) {
	ts, _ := testServerWith(t, nil)
	tok := adminLogin(t, ts.URL)
	res := do(t, "PUT", ts.URL+"/api/admin/beacon", tok, `{"url":"https://beacon.example.org","listed":true}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("ohne PUBLIC_URL: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestBeaconSubscriptions(t *testing.T) {
	fb := newFakeBeacon(t)
	ts, srv := testServerWith(t, nil)
	tok := adminLogin(t, ts.URL)

	res := do(t, "GET", ts.URL+"/api/admin/beacon/collections?url="+url.QueryEscape(fb.ts.URL+"#c_12ab34cd"), tok, "")
	cols := decode[struct {
		URL         string             `json:"url"`
		Name        string             `json:"name"`
		Collections []beaconCollection `json:"collections"`
		Selected    string             `json:"selected"`
	}](t, res)
	if cols.Name != "Test-Beacon" || len(cols.Collections) != 1 || cols.Selected != "c_12ab34cd" {
		t.Fatalf("Sammlungen: %+v", cols)
	}
	body, _ := json.Marshal([]BeaconSub{{URL: cols.URL, Name: cols.Name, Collections: []string{"c_12ab34cd", "kaputt"}}})
	res = do(t, "PUT", ts.URL+"/api/admin/beacon/subscriptions", tok, string(body))
	subs := decode[[]BeaconSub](t, res)
	if len(subs) != 1 || len(subs[0].Collections) != 1 {
		t.Fatalf("Abos: %+v", subs)
	}
	if !srv.hub.Media().Public().Beacon {
		t.Fatal("Beacon-Reiter nicht freigeschaltet")
	}

	// Suche im Chat
	res = do(t, "GET", ts.URL+"/api/gifs/beacon?q=hurra", "", "")
	hits := decode[[]WebGIF](t, res)
	if len(hits) != 1 || !strings.HasPrefix(hits[0].ID, "beacon:") {
		t.Fatalf("Treffer: %+v", hits)
	}
	// Auflösen beim Senden: Datei wird lokal gespeichert
	m, err := srv.hub.Media().ResolveGIF(hits[0].ID)
	if err != nil || !strings.HasPrefix(m.Src, "/media/") || m.Source != "beacon" || m.Credit != "GIFs vom Alpenraum" {
		t.Fatalf("Auflösen: %+v %v", m, err)
	}
	// auch ohne Zwischenspeicher (neu beim Beacon nachgeschlagen)
	srv.hub.Media().webMu.Lock()
	srv.hub.Media().web = map[string]webEntry{}
	srv.hub.Media().webMu.Unlock()
	if m, err := srv.hub.Media().ResolveGIF(hits[0].ID); err != nil || m.Credit != "via Test-Beacon" {
		t.Fatalf("Auflösen ohne Cache: %+v %v", m, err) // Einzelabruf ohne credit → Name des Abos
	}
	// fremde ID wird abgelehnt
	if _, err := srv.hub.Media().ResolveGIF("beacon:ffffffff:b_0123456789"); err == nil {
		t.Fatal("unbekannter Beacon angenommen")
	}
}
