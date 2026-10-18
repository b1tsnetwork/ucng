// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// YouTube & Co. abspielen – ohne auf den kompletten Download zu warten:
//
//   - Normale Videos: yt-dlp lädt mit voller Geschwindigkeit in eine Datei; ffmpeg liest die
//     wachsende Datei sofort mit (followReader). Die Wiedergabe startet nach wenigen Sekunden,
//     der fertige Download wird danach für den Zwischenspeicher umgewandelt (auch nach !skip).
//   - Livestreams (z. B. Lofi-Radio): laufen nie zu Ende – yt-dlp schreibt direkt in ffmpeg,
//     nichts wird gespeichert.
//
// Kommt nach startTimeout kein Ton, gibt es eine Fehlermeldung mit dem Grund von yt-dlp (statt
// still zu warten); bricht yt-dlp mitten im Stück ab, ebenso.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ytdlpArgs    = strings.Fields(env("YTDLP_ARGS", ""))                     // zusätzliche Optionen, z. B. --cookies /cookies.txt
	startTimeout = time.Duration(atoiEnv("START_TIMEOUT", 90)) * time.Second // so lange auf den ersten Ton warten
)

// setupCookies: YTDLP_COOKIES (eine cookies.txt, oft schreibgeschützt eingebunden) nach TMP_DIR
// kopieren – yt-dlp schreibt die Datei am Ende zurück – und als --cookies anhängen.
func setupCookies() {
	src := env("YTDLP_COOKIES", "")
	if src == "" {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil {
		log.Printf("YTDLP_COOKIES %s nicht lesbar: %v – ohne Cookies", src, err)
		return
	}
	// Unter Windows exportiert: CRLF → LF (sonst „HTTP Error 400“ bei YouTube)
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(bytes.TrimLeft(b, "\ufeff"), []byte("# ")) || !bytes.Contains(b, []byte("youtube.com")) {
		log.Printf("YTDLP_COOKIES %s: sieht nicht nach einer cookies.txt (Netscape-Format) mit YouTube-Cookies aus", src)
	}
	dst := filepath.Join(tmpDir, "ucng-cookies.txt")
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		log.Printf("Cookies nicht kopiert: %v", err)
		return
	}
	ytdlpArgs = append(ytdlpArgs, "--cookies", dst)
	log.Printf("YouTube: Cookies aus %s", src)
}

// ytHint: Tipp, wenn YouTube eine Anmeldung verlangt (Bot-Erkennung, Altersgrenze).
func ytHint(err error) string {
	if err == nil {
		return ""
	}
	e := strings.ToLower(err.Error())
	if strings.Contains(e, "sign in") || strings.Contains(e, "cookies") || strings.Contains(e, "confirm your age") {
		return " – Tipp für den Betreiber: Cookies hinterlegen (YTDLP_COOKIES, siehe README des Musik-Bots)"
	}
	return ""
}

// tailBuffer behält die letzten Zeilen einer Ausgabe (für Fehlermeldungen).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 8<<10 {
		t.buf = t.buf[len(t.buf)-8<<10:]
	}
	t.mu.Unlock()
	return len(p), nil
}

// reason: die aussagekräftigste Zeile – bevorzugt „ERROR: …“ von yt-dlp.
func (t *tailBuffer) reason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "ERROR:") {
			return shorten(strings.TrimSpace(strings.TrimPrefix(l, "ERROR:")))
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return shorten(l)
		}
	}
	return ""
}

func shorten(s string) string {
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + " …"
	}
	return s
}

// firstBytes meldet einmal, sobald Daten fließen (dann beginnt die Wiedergabe wirklich).
type firstBytes struct {
	r    io.Reader
	once sync.Once
	fn   func()
}

func (f *firstBytes) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.once.Do(f.fn)
	}
	return n, err
}

// followReader liest eine Datei, während sie noch geschrieben wird (wie tail -f), bis der
// Schreiber fertig ist (done geschlossen) und alles gelesen wurde.
type followReader struct {
	ctx  context.Context
	path string
	done <-chan struct{}
	f    *os.File
}

func (r *followReader) Read(p []byte) (int, error) {
	for {
		if r.f == nil {
			f, err := os.Open(r.path)
			if err != nil {
				select {
				case <-r.done: // fertig, ohne je eine Datei zu schreiben
					return 0, io.EOF
				case <-r.ctx.Done():
					return 0, r.ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			r.f = f
		}
		n, err := r.f.Read(p)
		if n > 0 || (err != nil && err != io.EOF) {
			return n, err
		}
		select { // noch nichts Neues: warten, außer der Download ist fertig
		case <-r.done:
			n, err := r.f.Read(p) // letzte Bytes nach dem Ende
			if n > 0 {
				return n, nil
			}
			if err == nil {
				err = io.EOF
			}
			return 0, err
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *followReader) Close() {
	if r.f != nil {
		r.f.Close()
	}
}

func ffmpegArgs(input string) []string {
	pre := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if input == "pipe:0" { // Strom: nicht 5 s lang analysieren, bevor der erste Ton kommt
		pre = append(pre, "-probesize", "256k", "-analyzeduration", "2000000")
	}
	return append(pre, "-i", input, "-vn", "-map", "0:a:0",
		"-c:a", "libopus", "-b:a", strconv.Itoa(kbps)+"k", "-ac", "2", "-ar", "48000", "-frame_duration", "20",
		"-application", "audio", "-f", "ogg", "pipe:1")
}

// playURL spielt ein Video/einen Livestream über yt-dlp und ffmpeg ab. started wird aufgerufen,
// sobald Ton fließt.
func playURL(ctx context.Context, room string, it item, vol int, started func()) error {
	dir, err := os.MkdirTemp(tmpDir, "ucng-music-")
	if err != nil {
		return err
	}
	media := filepath.Join(dir, "track.media")
	// Der Download läuft weiter, wenn das Stück übersprungen wird – für den Zwischenspeicher.
	// Livestreams und ohne Zwischenspeicher: endet mit der Wiedergabe.
	keep := !it.Live && cacheOn() && it.Key != ""
	dlCtx, dlCancel := context.WithCancel(ctx)
	if keep {
		dlCtx, dlCancel = context.WithTimeout(context.Background(), 30*time.Minute)
	}
	args := []string{"--quiet", "--no-warnings", "--no-playlist", "--no-part", "--fixup", "never",
		"-f", "bestaudio[ext=webm]/bestaudio/best"}
	if it.Live {
		args = append(args, "-o", "-")
	} else {
		args = append(args, "-o", media)
	}
	args = append(args, ytdlpArgs...)
	args = append(args, "--", it.URL)
	yt := exec.CommandContext(dlCtx, "yt-dlp", args...)
	ytErr := &tailBuffer{}
	yt.Stderr = ytErr

	playCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ff := exec.CommandContext(playCtx, "ffmpeg", ffmpegArgs("pipe:0")...)
	ffErr := &tailBuffer{}
	ff.Stderr = ffErr
	ytDone := make(chan struct{})
	var ytWaitErr error
	var follow *followReader
	if it.Live {
		pipe, err := yt.StdoutPipe()
		if err != nil {
			dlCancel()
			os.RemoveAll(dir)
			return err
		}
		ff.Stdin = pipe
	} else {
		follow = &followReader{ctx: playCtx, path: media, done: ytDone}
		ff.Stdin = follow
	}
	out, err := ff.StdoutPipe()
	if err != nil {
		dlCancel()
		os.RemoveAll(dir)
		return err
	}
	if err := yt.Start(); err != nil {
		dlCancel()
		os.RemoveAll(dir)
		return fmt.Errorf("yt-dlp fehlt? %w", err)
	}
	go func() { ytWaitErr = yt.Wait(); close(ytDone) }()
	if err := ff.Start(); err != nil {
		dlCancel()
		<-ytDone
		os.RemoveAll(dir)
		return fmt.Errorf("ffmpeg fehlt? %w", err)
	}
	// Aufräumen: erst wenn Download (samt Umwandeln für den Zwischenspeicher) fertig ist
	go func() {
		<-ytDone
		dlCancel()
		if keep && ytWaitErr == nil {
			if st, err := os.Stat(media); err == nil && st.Size() > 0 {
				cacheStore(it.Key, it.Title, it.URL, media, func() { os.RemoveAll(dir) })
				return
			}
		}
		os.RemoveAll(dir)
	}()

	// Kommt zu lange kein Ton, abbrechen – mit dem Grund von yt-dlp
	var gotSound, timedOut bool
	var mu sync.Mutex
	timer := time.AfterFunc(startTimeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if !gotSound {
			timedOut = true
			cancel()
			dlCancel()
		}
	})
	defer timer.Stop()
	body := &firstBytes{r: out, fn: func() {
		mu.Lock()
		gotSound = true
		mu.Unlock()
		timer.Stop()
		started()
	}}
	err = stream(playCtx, room, it, vol, body, func() { ff.Process.Kill() })
	cancel()
	ff.Wait()
	if follow != nil {
		follow.Close()
	}
	if !keep {
		dlCancel()
	}
	mu.Lock()
	to, sound := timedOut, gotSound
	mu.Unlock()
	why := ytErr.reason()
	switch {
	case to:
		if why == "" {
			why = "keine Antwort"
		}
		return fmt.Errorf("YouTube liefert nach %d s keinen Ton – %s", int(startTimeout.Seconds()), why)
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return context.Canceled
	case !sound: // nie Ton: meist lehnt YouTube ab (Anmeldung, Region, Alter, gelöscht …)
		select {
		case <-ytDone:
		case <-time.After(3 * time.Second):
		}
		if why == "" {
			why = ffErr.reason()
		}
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = "kein Ton erhalten"
		}
		return errors.New("YouTube: " + why)
	case err != nil:
		return err
	}
	// Ton lief – aber endete das Stück, weil yt-dlp abgebrochen ist?
	select {
	case <-ytDone:
		if ytWaitErr != nil && why != "" {
			return errors.New("abgebrochen – " + why)
		}
	default:
	}
	return nil
}
