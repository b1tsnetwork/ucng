// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"log"
	"time"

	"ucng/internal/stt"
)

// Untertitel und Live-Transkription: Der Relay schneidet die Sprache an Sprechpausen in
// Abschnitte (Ogg-Opus, internal/voicerelay/tap.go). Verschriftlicht wird entweder hier (STT_URL
// beim Chat-Server, internal/stt) oder schon auf dem Sprach-Server, wenn der eine eigene
// Spracherkennung hat (STT_URL beim Agenten – dann kommt nur Text an). Der Text geht als
// Untertitel an alle im Sprachkanal und bei laufender Live-Transkription ins Transkript
// (transcripts.go). Ton geht nie an Dritte. In Ende-zu-Ende-verschlüsselten Kanälen ist beides aus.

type sttClient struct {
	*stt.Client
}

func newSTT(cfg Config) *sttClient {
	c := stt.New(stt.Config{URL: cfg.STTURL, Token: cfg.STTToken, Model: cfg.STTModel, Lang: cfg.STTLanguage,
		Concurrency: cfg.STTConcurrency, Timeout: 60 * time.Second})
	if c == nil {
		return nil
	}
	log.Printf("Untertitel: Spracherkennung %s (Modell %s)", c.URL(), c.Model())
	return &sttClient{Client: c}
}

func cleanCaption(s string) string { return stt.Clean(s) }
