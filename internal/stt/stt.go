// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package stt spricht mit der eigenen Spracherkennung (OpenAI-kompatibel: POST multipart
// „file“, „model“, „language“ → {"text": …}, z. B. speaches/faster-whisper). Genutzt vom
// Chat-Server und – wenn dort STT_URL gesetzt ist – von den Sprach-Servern (ucng-voice), die
// dann selbst verschriftlichen und nur noch Text weitergeben.
//
// Abschnitte gehen durch eine Warteschlange mit Concurrency Arbeitern. Untertitel werden bei
// Überlast ausgelassen (lieber gar nicht als zu spät), Transkript-Abschnitte warten.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Config der Spracherkennung.
type Config struct {
	URL         string // z. B. http://ucng-stt:8000/v1/audio/transcriptions
	Token       string // Bearer-Schlüssel (optional)
	Model       string // Standard whisper-1
	Lang        string // Sprache fest vorgeben (sonst je Abschnitt)
	Concurrency int    // gleichzeitige Anfragen (Standard 4)
	Timeout     time.Duration
}

// Job: ein Abschnitt (Ogg-Opus) und was mit dem Text geschehen soll.
type Job struct {
	Audio    []byte
	Lang     string
	Priority bool                         // Live-Transkription: nicht auslassen
	Done     func(text string, err error) // in einem Arbeiter aufgerufen
}

// Stats für die Übersicht im Admin-Bereich.
type Stats struct {
	Host        string `json:"host"`  // nur Rechner und Port – keine Schlüssel
	Model       string `json:"model"` // Modell
	Lang        string `json:"lang,omitempty"`
	Concurrency int    `json:"concurrency"`
	Queue       int    `json:"queue"`   // wartende Abschnitte
	Busy        int    `json:"busy"`    // gerade in Arbeit
	Done        int64  `json:"done"`    // verschriftlicht
	Errors      int64  `json:"errors"`  // fehlgeschlagen
	Dropped     int64  `json:"dropped"` // ausgelassen (Überlast)
	AvgMs       int64  `json:"avgMs"`   // mittlere Dauer einer Anfrage (gleitend)
	LastError   string `json:"lastError,omitempty"`
	LastErrorAt int64  `json:"lastErrorAt,omitempty"` // Unix-Millisekunden
	LastOKAt    int64  `json:"lastOkAt,omitempty"`
}

// Client: Verbindung zur Spracherkennung samt Warteschlange.
type Client struct {
	cfg      Config
	http     *http.Client
	queue    chan Job
	stop     chan struct{}
	once     sync.Once
	busy     atomic.Int32
	done     atomic.Int64
	errs     atomic.Int64
	dropped  atomic.Int64
	avgMs    atomic.Int64
	lastOK   atomic.Int64
	lastWarn atomic.Int64
	mu       sync.Mutex
	lastErr  string
	lastErrT int64
}

// New startet die Arbeiter (nil, wenn keine URL gesetzt ist).
func New(cfg Config) *Client {
	if cfg.URL == "" {
		return nil
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Model == "" {
		cfg.Model = "whisper-1"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	c := &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}, queue: make(chan Job, 1024), stop: make(chan struct{})}
	for i := 0; i < cfg.Concurrency; i++ {
		go c.worker()
	}
	return c
}

// Close beendet die Arbeiter (wartende Abschnitte entfallen).
func (c *Client) Close() {
	if c != nil {
		c.once.Do(func() { close(c.stop) })
	}
}

// Model und URL (für Meldungen).
func (c *Client) Model() string { return c.cfg.Model }
func (c *Client) URL() string   { return c.cfg.URL }

// Submit stellt einen Abschnitt an. false = ausgelassen.
func (c *Client) Submit(j Job) bool {
	if !j.Priority && len(c.queue) > c.cfg.Concurrency*3 {
		c.dropped.Add(1)
		return false
	}
	select {
	case c.queue <- j:
		return true
	default:
		c.dropped.Add(1)
		c.Warn("Warteschlange voll – Abschnitt ausgelassen (mehr Arbeiter über STT_CONCURRENCY?)")
		return false
	}
}

func (c *Client) worker() {
	for {
		var j Job
		select {
		case <-c.stop:
			return
		case j = <-c.queue:
		}
		c.busy.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
		text, err := c.Transcribe(ctx, j.Audio, "audio.ogg", "audio/ogg", j.Lang)
		cancel()
		c.busy.Add(-1)
		if j.Done != nil {
			j.Done(text, err)
		}
	}
}

// Transcribe schickt eine Audiodatei an die Spracherkennung und gibt den bereinigten Text zurück.
func (c *Client) Transcribe(ctx context.Context, audio []byte, filename, ctype, lang string) (string, error) {
	start := time.Now()
	text, err := c.transcribe(ctx, audio, filename, ctype, lang)
	if err != nil {
		c.errs.Add(1)
		c.mu.Lock()
		c.lastErr, c.lastErrT = err.Error(), time.Now().UnixMilli()
		c.mu.Unlock()
		return "", err
	}
	ms := time.Since(start).Milliseconds()
	if old := c.avgMs.Load(); old == 0 {
		c.avgMs.Store(ms)
	} else {
		c.avgMs.Store((old*7 + ms) / 8)
	}
	c.done.Add(1)
	c.lastOK.Store(time.Now().UnixMilli())
	return text, nil
}

func (c *Client) transcribe(ctx context.Context, audio []byte, filename, ctype, lang string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	hdr.Set("Content-Type", ctype)
	fw, _ := mw.CreatePart(hdr)
	fw.Write(audio)
	mw.WriteField("model", c.cfg.Model)
	mw.WriteField("response_format", "json")
	mw.WriteField("temperature", "0")
	if c.cfg.Lang != "" {
		lang = c.cfg.Lang
	}
	if lang != "" {
		mw.WriteField("language", lang)
	}
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.Warn("%s nicht erreichbar: %v", c.cfg.URL, err)
		}
		return "", fmt.Errorf("nicht erreichbar: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		// ins Log, damit Fehler bei der Einrichtung auffallen (z. B. Modell nicht installiert)
		c.Warn("%s antwortet HTTP %d: %s (STT_MODEL=%s)", c.cfg.URL, res.StatusCode, strings.TrimSpace(string(msg)), c.cfg.Model)
		return "", fmt.Errorf("HTTP %d: %s", res.StatusCode, firstLine(string(msg)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	return Clean(out.Text), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// Warn: Fehler ins Log – höchstens alle 5 Minuten (sonst je Satz eine Zeile).
func (c *Client) Warn(format string, args ...any) {
	now := time.Now().Unix()
	if last := c.lastWarn.Load(); now-last < 300 || !c.lastWarn.CompareAndSwap(last, now) {
		return
	}
	log.Printf("Spracherkennung: "+format, args...)
}

// Stats: aktueller Stand.
func (c *Client) Stats() Stats {
	host := c.cfg.URL
	if u, err := url.Parse(c.cfg.URL); err == nil && u.Host != "" {
		host = u.Host
	}
	c.mu.Lock()
	le, lt := c.lastErr, c.lastErrT
	c.mu.Unlock()
	return Stats{Host: host, Model: c.cfg.Model, Lang: c.cfg.Lang, Concurrency: c.cfg.Concurrency, Queue: len(c.queue),
		Busy: int(c.busy.Load()), Done: c.done.Load(), Errors: c.errs.Load(), Dropped: c.dropped.Load(), AvgMs: c.avgMs.Load(),
		LastError: le, LastErrorAt: lt, LastOKAt: c.lastOK.Load()}
}

// Typische „Halluzinationen“ von Whisper bei Stille oder Rauschen.
var noise = []string{"untertitel im auftrag des zdf", "untertitelung des zdf", "vielen dank fürs zuschauen", "untertitel von stephanie geiges",
	"thank you for watching", "thanks for watching", "subtitles by the amara.org community", "copyright wdr"}

// Clean: Leerraum glätten, Halluzinationen verwerfen, Länge begrenzen.
func Clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	low := strings.ToLower(strings.Trim(s, " .!?"))
	if low == "" {
		return ""
	}
	for _, n := range noise {
		if strings.Contains(low, n) {
			return ""
		}
	}
	if utf8.RuneCountInString(s) > 500 {
		s = string([]rune(s)[:500])
	}
	return s
}
