// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Musik aus dem Ordner: Suche in den Dateinamen (!play <Begriff> bei YOUTUBE=0), Übersicht
// (!songs) und Namensnennung aus den Tags der Datei (Künstler, Lizenz – wichtig bei CC BY).

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// isCacheDir: der (frühere) Zwischenspeicher für YouTube – bei YOUTUBE=0 wird er übersprungen.
func isCacheDir(path string) bool {
	d := env("CACHE_DIR", "")
	return d != "" && filepath.Clean(path) == filepath.Clean(d)
}

// findLocal: alle Dateien, deren Pfad (ohne Groß/klein) jedes Wort des Begriffs enthält –
// in Ordner-Reihenfolge. Ein leerer Begriff passt auf alles (gemischt).
func findLocal(q, by string) []item {
	all, _ := folder("", by)
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return all
	}
	root := filepath.Clean(musicDir)
	var hits []item
	for _, it := range all {
		rel, _ := filepath.Rel(root, it.File)
		hay := strings.ToLower(strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(rel))
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) && !strings.Contains(strings.ToLower(rel), w) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, it)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].File < hits[j].File })
	return hits
}

// songList: ohne Begriff die Unterordner (für !folder), mit Begriff die passenden Stücke.
func songList(q string) string {
	if strings.TrimSpace(q) != "" {
		hits := findLocal(q, "")
		if len(hits) == 0 {
			return "🎵 Nichts gefunden zu „" + q + "“."
		}
		var b strings.Builder
		fmt.Fprintf(&b, "🎵 %d Treffer:\n", len(hits))
		for i, it := range hits {
			if i == 15 {
				fmt.Fprintf(&b, "… und %d weitere", len(hits)-15)
				break
			}
			b.WriteString("• " + it.Title + "\n")
		}
		return strings.TrimSpace(b.String())
	}
	all, _ := folder("", "")
	if len(all) == 0 {
		return "🎵 Im Musik-Ordner liegt nichts."
	}
	root := filepath.Clean(musicDir)
	count := map[string]int{}
	for _, it := range all {
		rel, _ := filepath.Rel(root, it.File)
		top := ""
		if i := strings.IndexRune(rel, filepath.Separator); i > 0 {
			top = rel[:i]
		}
		count[top]++
	}
	var dirs []string
	for d := range count {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	var b strings.Builder
	fmt.Fprintf(&b, "🎵 %d Stücke im Musik-Ordner", len(all))
	if n := count[""]; n > 0 && len(dirs) > 0 {
		fmt.Fprintf(&b, " (%d direkt darin)", n)
	}
	for i, d := range dirs {
		if i == 25 {
			fmt.Fprintf(&b, "\n… und %d weitere Ordner", len(dirs)-25)
			break
		}
		fmt.Fprintf(&b, "\n📁 %s (%d) – %sfolder %s", d, count[d], prefix, d)
	}
	b.WriteString("\nSuchen: " + prefix + "songs <Begriff>, abspielen: " + prefix + "play <Begriff>")
	return b.String()
}

// credit liest Künstler und Lizenz aus den Tags (ffprobe) → „ – Künstler · CC BY 4.0“.
// Ohne ffprobe oder Tags: leer.
func credit(ctx context.Context, file, title string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "--", file).Output()
	if err != nil {
		return ""
	}
	return creditFrom(out, title)
}

func creditFrom(out []byte, title string) string {
	var info struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Tags map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &info) != nil {
		return ""
	}
	tags := map[string]string{}
	add := func(m map[string]string) {
		for k, v := range m {
			k = strings.ToLower(k)
			if v = strings.TrimSpace(v); v != "" && tags[k] == "" {
				tags[k] = v
			}
		}
	}
	add(info.Format.Tags)
	for _, s := range info.Streams { // Ogg/Opus: Tags am Datenstrom
		add(s.Tags)
	}
	var parts []string
	for _, k := range []string{"artist", "album_artist", "composer"} {
		if v := tags[k]; v != "" {
			if !strings.Contains(strings.ToLower(title), strings.ToLower(v)) { // steht schon im Dateinamen
				parts = append(parts, v)
			}
			break
		}
	}
	for _, k := range []string{"license", "copyright", "tcop", "wcop"} {
		if v := tags[k]; v != "" {
			parts = append(parts, shorten(v))
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " – " + strings.Join(parts, " · ")
}
