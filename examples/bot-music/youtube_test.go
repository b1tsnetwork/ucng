// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// followReader liest eine wachsende Datei mit, bis der Schreiber fertig ist.
func TestFollowReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.media")
	done := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond) // Datei gibt es anfangs noch nicht
		f, _ := os.Create(path)
		for i := 0; i < 5; i++ {
			f.Write([]byte("abc"))
			time.Sleep(60 * time.Millisecond)
		}
		f.Close()
		close(done)
	}()
	r := &followReader{ctx: context.Background(), path: path, done: done}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != strings.Repeat("abc", 5) {
		t.Fatalf("%q %v", b, err)
	}
	// abgebrochen: kein Warten mehr
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r2 := &followReader{ctx: ctx, path: filepath.Join(t.TempDir(), "fehlt"), done: make(chan struct{})}
	if _, err := r2.Read(make([]byte, 4)); !errors.Is(err, context.Canceled) {
		t.Fatalf("abgebrochen: %v", err)
	}
}

func TestYTReason(t *testing.T) {
	b := &tailBuffer{}
	b.Write([]byte("[youtube] abc: Downloading webpage\nERROR: [youtube] abc: Sign in to confirm you’re not a bot\nnoch etwas\n"))
	if r := b.reason(); r != "[youtube] abc: Sign in to confirm you’re not a bot" {
		t.Fatalf("Grund: %q", r)
	}
	if ytHint(errors.New("YouTube: "+b.reason())) == "" {
		t.Fatal("kein Tipp zu Cookies")
	}
}

func TestCreditFrom(t *testing.T) {
	j := []byte(`{"format":{"tags":{"ARTIST":"Kevin MacLeod","title":"Wallpaper"}},"streams":[{"tags":{"LICENSE":"CC BY 4.0"}}]}`)
	if got := creditFrom(j, "Wallpaper"); got != " – Kevin MacLeod · CC BY 4.0" {
		t.Fatalf("got %q", got)
	}
	if got := creditFrom(j, "Kevin MacLeod - Wallpaper"); got != " – CC BY 4.0" {
		t.Fatalf("got %q", got)
	}
	if got := creditFrom([]byte(`{"format":{}}`), "x"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestFindLocal(t *testing.T) {
	dir := t.TempDir()
	old := musicDir
	musicDir = dir
	defer func() { musicDir = old }()
	for _, f := range []string{"lofi/Chill_Beat.mp3", "lofi/Rain.ogg", "rock/Loud Song.flac", "notes.txt"} {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	if n := len(findLocal("chill beat", "")); n != 1 {
		t.Fatalf("chill beat: %d", n)
	}
	if n := len(findLocal("lofi", "")); n != 2 {
		t.Fatalf("lofi: %d", n)
	}
	if n := len(findLocal("", "")); n != 3 {
		t.Fatalf("alle: %d", n)
	}
	if s := songList(""); !strings.Contains(s, "3 Stücke") || !strings.Contains(s, "lofi (2)") {
		t.Fatalf("songList: %s", s)
	}
}
