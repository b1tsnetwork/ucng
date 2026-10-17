// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

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
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Untertitel und Live-Transkription: Der Relay schneidet die Sprache an Sprechpausen in
// Abschnitte (Ogg-Opus, internal/voicerelay/tap.go); der Chat-Server reicht sie an die eigene
// Spracherkennung weiter (STT_URL, OpenAI-kompatibel: POST multipart „file“, „model“ →
// {"text": …}, z. B. ein selbst betriebenes Whisper wie speaches/faster-whisper) und verteilt
// den Text als Untertitel an alle im Sprachkanal – bei laufender Live-Transkription auch ins
// Transkript (transcripts.go). Ton geht nie an Dritte; ohne STT_URL gibt es beides nicht. In
// Ende-zu-Ende-verschlüsselten Kanälen ist beides aus.

type sttClient struct {
	url, token, model, lang string
	client                  *http.Client
	sem                     chan struct{}
	rl                      *rateLimiter
	queue                   chan sttJob  // Abschnitte vom Relay (transcripts.go)
	lastWarn                atomic.Int64 // Unix-Sekunden der letzten Warnung im Log
}

// warn: Fehler der Spracherkennung ins Log – höchstens alle 5 Minuten (sonst je Satz eine Zeile).
func (c *sttClient) warn(format string, args ...any) {
	now := time.Now().Unix()
	if last := c.lastWarn.Load(); now-last < 300 || !c.lastWarn.CompareAndSwap(last, now) {
		return
	}
	log.Printf("Untertitel: "+format, args...)
}

func newSTT(cfg Config) *sttClient {
	if cfg.STTURL == "" {
		return nil
	}
	n := cfg.STTConcurrency
	if n <= 0 {
		n = 4
	}
	model := cfg.STTModel
	if model == "" {
		model = "whisper-1"
	}
	log.Printf("Untertitel: Spracherkennung %s (Modell %s)", cfg.STTURL, model)
	return &sttClient{url: cfg.STTURL, token: cfg.STTToken, model: model, lang: cfg.STTLanguage,
		client: &http.Client{Timeout: 45 * time.Second}, sem: make(chan struct{}, n), rl: newRateLimiter(40, time.Minute),
		queue: make(chan sttJob, 1024)}
}

// transcribeFile schickt einen Abschnitt (Ogg-Opus vom Relay) an die Spracherkennung.
func (c *sttClient) transcribeFile(ctx context.Context, audio []byte, filename, ctype, lang string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	hdr.Set("Content-Type", ctype)
	fw, _ := mw.CreatePart(hdr)
	fw.Write(audio)
	mw.WriteField("model", c.model)
	mw.WriteField("response_format", "json")
	mw.WriteField("temperature", "0")
	if c.lang != "" {
		lang = c.lang
	}
	if lang != "" {
		mw.WriteField("language", lang)
	}
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.warn("%s nicht erreichbar: %v", c.url, err)
		}
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		// ins Log, damit Fehler bei der Einrichtung auffallen (z. B. Modell nicht installiert)
		c.warn("%s antwortet HTTP %d: %s (STT_MODEL=%s)", c.url, res.StatusCode, strings.TrimSpace(string(msg)), c.model)
		return "", fmt.Errorf("Spracherkennung: HTTP %d", res.StatusCode)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		return "", err
	}
	return cleanCaption(out.Text), nil
}

// Typische „Halluzinationen“ von Whisper bei Stille oder Rauschen.
var sttNoise = []string{"untertitel im auftrag des zdf", "untertitelung des zdf", "vielen dank fürs zuschauen", "untertitel von stephanie geiges",
	"thank you for watching", "thanks for watching", "subtitles by the amara.org community", "copyright wdr"}

func cleanCaption(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	low := strings.ToLower(strings.Trim(s, " .!?"))
	if low == "" {
		return ""
	}
	for _, n := range sttNoise {
		if strings.Contains(low, n) {
			return ""
		}
	}
	if utf8.RuneCountInString(s) > 500 {
		s = string([]rune(s)[:500])
	}
	return s
}
