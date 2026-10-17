// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Untertitel: Der Browser schickt kurze Sprach-Abschnitte (WAV, 16 kHz, mono – nur, solange die
// Person spricht) an den Chat-Server. Der reicht sie an die eigene Spracherkennung weiter
// (STT_URL, OpenAI-kompatibel: POST multipart „file“, „model“ → {"text": …}, z. B. ein selbst
// betriebenes Whisper wie faster-whisper-server oder whisper.cpp server) und verteilt den Text als
// Untertitel an alle im Sprachkanal. Ton geht nie an Dritte; ohne STT_URL gibt es keine
// Untertitel. In Ende-zu-Ende-verschlüsselten Kanälen sind sie aus.

var (
	ErrSTTOff    = errors.New("Untertitel sind hier nicht eingeschaltet")
	ErrSTTBusy   = errors.New("Die Spracherkennung ist gerade ausgelastet")
	ErrSTTFormat = errors.New("Bitte WAV schicken (16 kHz, mono)")
)

const maxSTTBody = 1 << 20 // ≈ 30 s bei 16 kHz/16 bit

type sttClient struct {
	url, token, model, lang string
	client                  *http.Client
	sem                     chan struct{}
	rl                      *rateLimiter
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
	return &sttClient{url: cfg.STTURL, token: cfg.STTToken, model: model, lang: cfg.STTLanguage,
		client: &http.Client{Timeout: 45 * time.Second}, sem: make(chan struct{}, n), rl: newRateLimiter(40, time.Minute)}
}

// transcribe schickt einen Abschnitt an die Spracherkennung.
func (c *sttClient) transcribe(ctx context.Context, wav []byte, lang string) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	fw.Write(wav)
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
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
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

func isWAV(b []byte) bool {
	return len(b) > 44 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE"
}

// POST /api/voice/{room}/stt?lang=de – WAV-Abschnitt der eigenen Stimme → Untertitel für alle
func (s *Server) handleVoiceSTT(w http.ResponseWriter, r *http.Request) {
	v := s.voice
	if v == nil || v.stt == nil {
		writeErr(w, http.StatusNotFound, ErrSTTOff.Error())
		return
	}
	me, ok := s.users.Lookup(bearer(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, ErrNoSession.Error())
		return
	}
	room := NormRoom(r.PathValue("room"))
	acc := s.hub.voiceAccessFull(room, s.voiceWhoOf(me, "").access())
	if acc.Cfg == nil || !acc.Cfg.Captions || acc.Cfg.E2EE {
		writeErr(w, http.StatusNotFound, ErrSTTOff.Error())
		return
	}
	speaking := false
	for _, p := range v.Peers(room) { // nur, wer im Kanal ist und sprechen darf
		speaking = speaking || p.User == me.ID && p.Speak && !p.Muted
	}
	if !speaking {
		writeErr(w, http.StatusConflict, ErrMediaNoVoice.Error())
		return
	}
	if !v.stt.rl.Allow(me.ID) {
		writeErr(w, http.StatusTooManyRequests, "Zu viele Abschnitte – bitte kurz warten.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSTTBody))
	if err != nil || !isWAV(body) {
		writeErr(w, http.StatusBadRequest, ErrSTTFormat.Error())
		return
	}
	select { // nicht stauen: lieber einen Abschnitt auslassen
	case v.stt.sem <- struct{}{}:
	default:
		writeErr(w, http.StatusServiceUnavailable, ErrSTTBusy.Error())
		return
	}
	defer func() { <-v.stt.sem }()
	lang := r.URL.Query().Get("lang")
	if len(lang) > 8 || strings.ContainsAny(lang, " /&") {
		lang = ""
	}
	text, err := v.stt.transcribe(r.Context(), body, lang)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if text != "" {
		v.control(room, me.ID, "cap", true, text)
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}
