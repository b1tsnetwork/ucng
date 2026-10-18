// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ucng/internal/fedsig"
)

func testBeacon(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{AdminPassword: "geheim-passwort", GifBox: 240}, st)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return ts, s
}

// fakeInstance veröffentlicht einen Serverschlüssel wie eine Instanz von vor der Umbenennung
// (/.well-known/gochat, software "gochat") – prüft zugleich, dass ältere Instanzen weiter angenommen werden.
func fakeInstance(t *testing.T) (*httptest.Server, string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var domain string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/gochat" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"software": "gochat", "domain": domain, "key": base64.StdEncoding.EncodeToString(pub)})
	}))
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	domain = u.Host
	return ts, domain, priv
}

func register(t *testing.T, beacon, domain string, priv ed25519.PrivateKey, payload map[string]any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(payload)
	u, _ := url.Parse(beacon)
	ts, sig := fedsig.Sign(priv, RegisterContext, domain, "POST", u.Host+"/api/v1/register", body, time.Now())
	req, _ := http.NewRequest("POST", beacon+"/api/v1/register", bytes.NewReader(body))
	req.Header.Set(fedsig.HeaderDomain, domain)
	req.Header.Set(fedsig.HeaderDate, ts)
	req.Header.Set(fedsig.HeaderSignature, sig)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func instances(t *testing.T, base, query string) []Instance {
	t.Helper()
	res, err := http.Get(base + "/api/v1/instances" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Instances []Instance `json:"instances"`
	}
	b := new(bytes.Buffer)
	b.ReadFrom(res.Body)
	if err := json.Unmarshal(b.Bytes(), &out); err != nil || out.Instances == nil {
		var list []Instance
		if json.Unmarshal(b.Bytes(), &list) == nil {
			return list
		}
	}
	return out.Instances
}

func TestRegisterAndList(t *testing.T) {
	bts, _ := testBeacon(t)
	_, domain, priv := fakeInstance(t)
	payload := map[string]any{"url": "http://" + domain, "name": "Testchat", "users": 42, "language": "de", "country": "AT",
		"mode": "hosted", "federation": true, "listed": true, "source": "https://git.example.org/team/ucng-fork", "version": "1.2.3-team"}
	res := register(t, bts.URL, domain, priv, payload)
	if res.StatusCode != 200 {
		t.Fatalf("Anmeldung: %d", res.StatusCode)
	}
	res.Body.Close()
	list := instances(t, bts.URL, "")
	if len(list) != 1 || list[0].Users != 42 || list[0].Language != "de" || list[0].Country != "AT" || list[0].Name != "Testchat" ||
		list[0].Source != "https://git.example.org/team/ucng-fork" || list[0].Version != "1.2.3-team" {
		t.Fatalf("Liste: %+v", list)
	}
	if l := instances(t, bts.URL, "?country=DE"); len(l) != 0 {
		t.Fatalf("Filter Land: %+v", l)
	}
	if l := instances(t, bts.URL, "?language=de&q=test"); len(l) != 1 {
		t.Fatalf("Filter Sprache/Suche: %+v", l)
	}

	// Aktualisierung
	payload["users"] = 50
	res = register(t, bts.URL, domain, priv, payload)
	res.Body.Close()
	if l := instances(t, bts.URL, ""); len(l) != 1 || l[0].Users != 50 {
		t.Fatalf("Aktualisierung: %+v", l)
	}

	// falscher Schlüssel
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	res = register(t, bts.URL, domain, priv2, payload)
	if res.StatusCode != 401 {
		t.Fatalf("falscher Schlüssel: %d", res.StatusCode)
	}
	res.Body.Close()

	// fremde URL
	bad := map[string]any{"url": "https://anders.example.org", "listed": true}
	res = register(t, bts.URL, domain, priv, bad)
	if res.StatusCode != 400 {
		t.Fatalf("fremde URL: %d", res.StatusCode)
	}
	res.Body.Close()

	// abmelden
	res = register(t, bts.URL, domain, priv, map[string]any{"listed": false})
	res.Body.Close()
	if l := instances(t, bts.URL, ""); len(l) != 0 {
		t.Fatalf("Abmeldung: %+v", l)
	}
}

func TestRegisterUnsigned(t *testing.T) {
	bts, _ := testBeacon(t)
	res, _ := http.Post(bts.URL+"/api/v1/register", "application/json", strings.NewReader(`{"listed":true}`))
	if res.StatusCode != 401 {
		t.Fatalf("ohne Signatur: %d", res.StatusCode)
	}
	res.Body.Close()
}

func testGIFData() []byte {
	pal := color.Palette{color.Black, color.White, color.RGBA{255, 0, 0, 255}}
	g := &gif.GIF{}
	for i := 0; i < 3; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 100, 80), pal)
		for x := 0; x < 100; x++ {
			img.SetColorIndex(x, (x+i*10)%80, uint8(1+i%2))
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	gif.EncodeAll(&buf, g)
	return buf.Bytes()
}

func adminDo(t *testing.T, method, url, tok string, body []byte, ctype string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGIFCollections(t *testing.T) {
	bts, _ := testBeacon(t)
	res := adminDo(t, "POST", bts.URL+"/api/admin/login", "", []byte(`{"password":"falsch"}`), "application/json")
	if res.StatusCode == 200 {
		t.Fatal("falsches Passwort angenommen")
	}
	res.Body.Close()
	res = adminDo(t, "POST", bts.URL+"/api/admin/login", "", []byte(`{"password":"geheim-passwort"}`), "application/json")
	var login struct{ Token string }
	json.NewDecoder(res.Body).Decode(&login)
	res.Body.Close()
	if login.Token == "" {
		t.Fatal("kein Token")
	}
	res = adminDo(t, "POST", bts.URL+"/api/admin/collections", login.Token, []byte(`{"name":"Reaktionen"}`), "application/json")
	var coll Collection
	json.NewDecoder(res.Body).Decode(&coll)
	res.Body.Close()
	if res.StatusCode != 201 || !strings.HasPrefix(coll.ID, "c_") {
		t.Fatalf("Sammlung: %d %+v", res.StatusCode, coll)
	}
	// ohne Anmeldung nicht erlaubt
	res = adminDo(t, "POST", bts.URL+"/api/admin/collections", "", []byte(`{"name":"X"}`), "application/json")
	if res.StatusCode != 401 {
		t.Fatalf("ohne Anmeldung: %d", res.StatusCode)
	}
	res.Body.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.gif")
	fw.Write(testGIFData())
	mw.WriteField("title", "Hurra")
	mw.WriteField("tags", "freude, jubel")
	mw.WriteField("collections", coll.ID)
	mw.Close()
	res = adminDo(t, "POST", bts.URL+"/api/admin/gifs", login.Token, buf.Bytes(), mw.FormDataContentType())
	var g struct {
		ID, URL, Credit string
		W, H            int
	}
	json.NewDecoder(res.Body).Decode(&g)
	res.Body.Close()
	if res.StatusCode != 201 || !strings.HasPrefix(g.ID, "b_") || g.URL == "" || g.Credit != "via UCNG Beacon" {
		t.Fatalf("GIF: %d %+v", res.StatusCode, g)
	}
	// Liste mit Filter
	r2, _ := http.Get(bts.URL + "/api/v1/gifs?collection=" + coll.ID + "&q=jubel")
	var list struct {
		GIFs  []struct{ ID string } `json:"gifs"`
		Total int                   `json:"total"`
	}
	json.NewDecoder(r2.Body).Decode(&list)
	r2.Body.Close()
	if list.Total != 1 || list.GIFs[0].ID != g.ID {
		t.Fatalf("Suche: %+v", list)
	}
	r3, _ := http.Get(bts.URL + "/api/v1/gifs?collection=c_00000000")
	json.NewDecoder(r3.Body).Decode(&list)
	r3.Body.Close()
	if list.Total != 0 {
		t.Fatalf("andere Sammlung: %+v", list)
	}
	// Bild abrufbar
	r4, _ := http.Get(g.URL)
	if r4.StatusCode != 200 || r4.Header.Get("Content-Type") != "image/gif" {
		t.Fatalf("Bild: %d %s", r4.StatusCode, r4.Header.Get("Content-Type"))
	}
	r4.Body.Close()
	// Sammlungen öffentlich mit Anzahl
	r5, _ := http.Get(bts.URL + "/api/v1/collections")
	var colls []struct {
		ID    string
		Count int
	}
	json.NewDecoder(r5.Body).Decode(&colls)
	r5.Body.Close()
	if len(colls) != 1 || colls[0].Count != 1 {
		t.Fatalf("Sammlungen: %+v", colls)
	}
}

func adminToken(t *testing.T, base string) string {
	t.Helper()
	res := adminDo(t, "POST", base+"/api/admin/login", "", []byte(`{"password":"geheim-passwort"}`), "application/json")
	var login struct{ Token string }
	json.NewDecoder(res.Body).Decode(&login)
	res.Body.Close()
	return login.Token
}

func TestTrustLevels(t *testing.T) {
	bts, _ := testBeacon(t)
	tok := adminToken(t, bts.URL)
	// zwei Instanzen; die kleinere wird offiziell und steht dann oben
	_, d1, k1 := fakeInstance(t)
	_, d2, k2 := fakeInstance(t)
	register(t, bts.URL, d1, k1, map[string]any{"url": "http://" + d1, "name": "Groß", "users": 500, "listed": true}).Body.Close()
	register(t, bts.URL, d2, k2, map[string]any{"url": "http://" + d2, "name": "Klein", "users": 3, "listed": true}).Body.Close()
	if l := instances(t, bts.URL, ""); l[0].Domain != d1 || l[0].Trust != "" || !l[0].Port {
		t.Fatalf("vorher: %+v", l)
	}
	res := adminDo(t, "PUT", bts.URL+"/api/admin/trust/"+d2, tok, []byte(`{"level":"official","note":"Vom Projekt"}`), "application/json")
	if res.StatusCode != 200 {
		t.Fatalf("Stufe setzen: %d", res.StatusCode)
	}
	res.Body.Close()
	l := instances(t, bts.URL, "")
	if l[0].Domain != d2 || l[0].Trust != "official" || l[0].TrustNote != "Vom Projekt" {
		t.Fatalf("nachher: %+v", l)
	}
	if f := instances(t, bts.URL, "?trust=community"); len(f) != 1 || f[0].Domain != d1 {
		t.Fatalf("Filter Community: %+v", f)
	}
	if f := instances(t, bts.URL, "?trust=official"); len(f) != 1 || f[0].Domain != d2 {
		t.Fatalf("Filter Offiziell: %+v", f)
	}
	// Antwort der Anmeldung nennt die Stufe
	res = register(t, bts.URL, d2, k2, map[string]any{"url": "http://" + d2, "name": "Klein", "users": 4, "listed": true})
	var out struct{ Trust string }
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if out.Trust != "official" {
		t.Fatalf("Antwort: %+v", out)
	}
	// abmelden und wieder anmelden: Stufe bleibt
	register(t, bts.URL, d2, k2, map[string]any{"listed": false}).Body.Close()
	register(t, bts.URL, d2, k2, map[string]any{"url": "http://" + d2, "name": "Klein", "listed": true}).Body.Close()
	if f := instances(t, bts.URL, "?trust=official"); len(f) != 1 {
		t.Fatalf("nach Neuanmeldung: %+v", f)
	}
	// ohne Anmeldung keine Änderung
	res = adminDo(t, "PUT", bts.URL+"/api/admin/trust/"+d1, "", []byte(`{"level":"official"}`), "application/json")
	if res.StatusCode != 401 {
		t.Fatalf("ohne Anmeldung: %d", res.StatusCode)
	}
	res.Body.Close()
	res = adminDo(t, "PUT", bts.URL+"/api/admin/trust/"+d1, tok, []byte(`{"level":"super"}`), "application/json")
	if res.StatusCode != 400 {
		t.Fatalf("ungültige Stufe: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestTrustKeyChange(t *testing.T) {
	bts, s := testBeacon(t)
	tok := adminToken(t, bts.URL)
	_, d, k := fakeInstance(t)
	// vorab vergeben, bindet sich bei der ersten Meldung
	adminDo(t, "PUT", bts.URL+"/api/admin/trust/"+d, tok, []byte(`{"level":"recommended"}`), "application/json").Body.Close()
	register(t, bts.URL, d, k, map[string]any{"url": "http://" + d, "listed": true}).Body.Close()
	if l := instances(t, bts.URL, ""); l[0].Trust != "recommended" {
		t.Fatalf("vorab: %+v", l)
	}
	if s.store.TrustList()[d].Key == "" {
		t.Fatal("Schlüssel nicht gebunden")
	}
	// neuer Schlüssel unter derselben Domain → Community
	s.store.mu.Lock()
	s.store.instances[d].Key = "anderer"
	s.store.mu.Unlock()
	l := s.store.Instances(time.Now(), true)
	if l[0].Trust != "" || !l[0].KeyChanged {
		t.Fatalf("Schlüsselwechsel: %+v", l[0])
	}
	// neu vergeben bindet an den neuen Schlüssel
	adminDo(t, "PUT", bts.URL+"/api/admin/trust/"+d, tok, []byte(`{"level":"recommended"}`), "application/json").Body.Close()
	if l := s.store.Instances(time.Now(), true); l[0].Trust != "recommended" || l[0].KeyChanged {
		t.Fatalf("neu vergeben: %+v", l[0])
	}
}

func TestNoPorts(t *testing.T) {
	st, _ := OpenStore(t.TempDir())
	s := NewServer(Config{AdminPassword: "x", NoPorts: true, GifBox: 240}, st)
	bts := httptest.NewServer(s.Routes())
	defer bts.Close()
	_, d, k := fakeInstance(t)
	res := register(t, bts.URL, d, k, map[string]any{"url": "http://" + d, "listed": true})
	if res.StatusCode != 400 {
		t.Fatalf("Port trotz NO_PORTS: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestCreditAndPublicOnly(t *testing.T) {
	bts, _ := testBeacon(t)
	tok := adminToken(t, bts.URL)
	res := adminDo(t, "PUT", bts.URL+"/api/admin/config", tok, []byte(`{"name":"Alpen","credit":"GIFs vom Alpen-Beacon"}`), "application/json")
	res.Body.Close()
	r2, _ := http.Get(bts.URL + "/api/v1/info")
	var info struct{ Credit string }
	json.NewDecoder(r2.Body).Decode(&info)
	r2.Body.Close()
	if info.Credit != "GIFs vom Alpen-Beacon" {
		t.Fatalf("credit: %+v", info)
	}
	// PUBLIC_ONLY: Instanzen im lokalen Netz werden abgelehnt
	st, _ := OpenStore(t.TempDir())
	s := NewServer(Config{AdminPassword: "x", PublicOnly: true, GifBox: 240}, st)
	pts := httptest.NewServer(s.Routes())
	defer pts.Close()
	_, d, k := fakeInstance(t)
	res = register(t, pts.URL, d, k, map[string]any{"url": "http://" + d, "listed": true})
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("PUBLIC_ONLY: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestRegisterErrors(t *testing.T) {
	// HTTPS_ONLY lehnt http:// ab
	st, _ := OpenStore(t.TempDir())
	s := NewServer(Config{AdminPassword: "x", HTTPSOnly: true, GifBox: 240}, st)
	hts := httptest.NewServer(s.Routes())
	defer hts.Close()
	_, d, k := fakeInstance(t)
	res := register(t, hts.URL, d, k, map[string]any{"url": "http://" + d, "listed": true})
	b := new(bytes.Buffer)
	b.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != 400 || !strings.Contains(b.String(), "https://") {
		t.Fatalf("HTTPS_ONLY: %d %s", res.StatusCode, b)
	}
	// nicht erreichbare Instanz: Meldung nennt Adresse und Grund
	bts, _ := testBeacon(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	res = register(t, bts.URL, "127.0.0.1:1", priv, map[string]any{"url": "http://127.0.0.1:1", "listed": true})
	b.Reset()
	b.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(b.String(), "http://127.0.0.1:1/.well-known/ucng") || !strings.Contains(b.String(), "abgelehnt") {
		t.Fatalf("unerreichbar: %d %s", res.StatusCode, b)
	}
}

func TestTrustStandardOrder(t *testing.T) {
	list := []Instance{{Name: "c", Users: 900}, {Name: "s", Trust: TrustStandard, Users: 1}, {Name: "r", Trust: TrustRecommended}, {Name: "o", Trust: TrustOfficial}}
	sortInstances(list, "users")
	got := ""
	for _, x := range list {
		got += x.Name
	}
	if got != "orsc" {
		t.Fatalf("Reihenfolge: %s", got)
	}
	if !validTrust("standard") || validTrust("super") {
		t.Fatal("validTrust")
	}
}

// Bearbeiten eines GIFs darf den Beacon nicht blockieren (früher: Store-Sperre doppelt genommen).
func TestGIFUpdateNoDeadlock(t *testing.T) {
	bts, _ := testBeacon(t)
	tok := adminToken(t, bts.URL)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.gif")
	fw.Write(testGIFData())
	mw.WriteField("title", "Hurra")
	mw.Close()
	res := adminDo(t, "POST", bts.URL+"/api/admin/gifs", tok, buf.Bytes(), mw.FormDataContentType())
	var g struct{ ID string }
	json.NewDecoder(res.Body).Decode(&g)
	res.Body.Close()

	done := make(chan int, 1)
	go func() {
		r := adminDo(t, "PATCH", bts.URL+"/api/admin/gifs/"+g.ID, tok, []byte(`{"title":"Juhu","tags":["freude"]}`), "application/json")
		r.Body.Close()
		done <- r.StatusCode
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("Bearbeiten: %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Bearbeiten hängt (Deadlock)")
	}
	// der Beacon antwortet danach weiter
	client := &http.Client{Timeout: 3 * time.Second}
	for _, p := range []string{"/api/v1/info", "/api/v1/gifs/" + g.ID, "/api/v1/collections"} {
		r, err := client.Get(bts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		r.Body.Close()
	}
}

// Größere GIFs bekommen eine kleine Vorschau (für das GIF-Fenster der Chats).
func TestGIFPreview(t *testing.T) {
	bts, s := testBeacon(t) // GifBox 240
	tok := adminToken(t, bts.URL)
	pal := color.Palette{color.Black, color.White, color.RGBA{200, 30, 30, 255}}
	anim := &gif.GIF{}
	for i := 0; i < 3; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 400, 300), pal)
		for x := 0; x < 400; x++ {
			img.SetColorIndex(x, (x*7+i*13)%300, uint8(1+i%2))
		}
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, 10)
	}
	var data bytes.Buffer
	gif.EncodeAll(&data, anim)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "gross.gif")
	fw.Write(data.Bytes())
	mw.Close()
	res := adminDo(t, "POST", bts.URL+"/api/admin/gifs", tok, buf.Bytes(), mw.FormDataContentType())
	var g GIFView
	json.NewDecoder(res.Body).Decode(&g)
	res.Body.Close()
	if res.StatusCode != 201 || g.W != 240 || !strings.Contains(g.Preview, "-p200.gif") {
		t.Fatalf("GIF: %d %+v", res.StatusCode, g)
	}
	r, err := http.Get(g.Preview)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("Vorschau: %v %v", err, r)
	}
	cfg, err := gif.DecodeConfig(r.Body)
	r.Body.Close()
	if err != nil || cfg.Width > 200 || cfg.Height > 200 {
		t.Fatalf("Vorschau zu groß: %+v %v", cfg, err)
	}
	// ältere GIFs ohne Vorschau werden beim Start nachgezogen
	s.store.mu.Lock()
	s.store.gifs[g.ID].Preview = ""
	s.store.mu.Unlock()
	s.backfillPreviews()
	if x, _ := s.store.GetGIF(g.ID); x.Preview == "" {
		t.Fatal("Vorschau nicht nachgezogen")
	}
}

func TestSharedChannels(t *testing.T) {
	bts, _ := testBeacon(t)
	_, d, k := fakeInstance(t)
	res := register(t, bts.URL, d, k, map[string]any{"url": "http://" + d, "name": "Alpen", "listed": true, "channels": []map[string]any{
		{"name": "news", "description": "Neuigkeiten", "mode": "write", "messages": 12},
		{"name": "Ungültig Name", "mode": "read"},
		{"name": "ruhig", "mode": "egal"},
		{"name": "lesen", "mode": "read"},
	}})
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("anmelden: %d", res.StatusCode)
	}
	get := func(q string) []ChannelListing {
		res, err := http.Get(bts.URL + "/api/v1/channels?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct{ Channels []ChannelListing }
		json.NewDecoder(res.Body).Decode(&out)
		return out.Channels
	}
	all := get("")
	if len(all) != 2 || all[0].Address != "#lesen@"+d && all[0].Address != "#news@"+d || all[0].Title != "Alpen" {
		t.Fatalf("Kanäle: %+v", all)
	}
	if w := get("mode=write"); len(w) != 1 || w[0].Name != "news" || w[0].Messages != 12 {
		t.Fatalf("nur mitschreiben: %+v", w)
	}
	if q := get("q=neuigk"); len(q) != 1 || q[0].Name != "news" {
		t.Fatalf("Suche: %+v", q)
	}
}
