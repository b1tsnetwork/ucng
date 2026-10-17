// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestYoutubeKey(t *testing.T) {
	for link, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":                  "youtube-dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?si=abc&t=42":                     "youtube-dQw4w9WgXcQ",
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ&feature=share":      "youtube-dQw4w9WgXcQ",
		"https://music.youtube.com/watch?v=dQw4w9WgXcQ":                "youtube-dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":                   "youtube-dQw4w9WgXcQ",
		"https://www.youtube.com/playlist?list=PL123":                  "",
		"https://www.youtube.com/watch?v=kurz":                         "",
		"https://example.org/watch?v=dQw4w9WgXcQ":                      "",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL1&index=2": "youtube-dQw4w9WgXcQ",
	} {
		if got := youtubeKey(link); got != want {
			t.Errorf("%s: %q, erwartet %q", link, got, want)
		}
	}
	if keyOf("Youtube", "dQw4w9WgXcQ") != youtubeKey("https://youtu.be/dQw4w9WgXcQ") {
		t.Error("Schlüssel aus yt-dlp und aus dem Link passen nicht zusammen")
	}
	if n := safeName(`AC/DC: "Back" <in> Black?`); n != "AC DC Back in Black" {
		t.Errorf("Dateiname: %q", n)
	}
}

func TestCacheEvict(t *testing.T) {
	dir := t.TempDir()
	cacheDir, cacheMaxGB = dir, 1
	defer func() { cacheDir, cacheMaxGB = "", 0 }()
	cacheLoad()
	now := time.Now()
	for i, k := range []string{"youtube-a", "youtube-b", "youtube-c"} {
		f := k + ".opus"
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
		cache.Tracks[k] = &cacheEntry{Title: k, File: f, Bytes: 400 << 20, Played: now.Add(time.Duration(i) * time.Minute)}
	}
	cache.Tracks["youtube-a"].Played = now.Add(time.Hour) // a lief zuletzt
	cache.Searches["lied b"] = "youtube-b"
	cacheMu.Lock()
	cacheEvictLocked("youtube-c")
	cacheMu.Unlock()
	if len(cache.Tracks) != 2 || cache.Tracks["youtube-b"] != nil { // 1,2 GB > 1 GB: b lief am längsten nicht
		t.Fatalf("Verdrängung: %v", keys(cache.Tracks))
	}
	if _, err := os.Stat(filepath.Join(dir, "youtube-b.opus")); err == nil || cache.Searches["lied b"] != "" {
		t.Fatal("Datei bzw. Suchbegriff des verdrängten Stücks noch da")
	}
}

func keys(m map[string]*cacheEntry) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
