// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package imgscale

import (
	"bytes"
	"errors"
	"image"
)

// Vorabprüfung ohne Decodieren: Eine kleine Datei kann beim Entpacken sehr viel Speicher
// belegen (z. B. ein GIF mit Hunderten großflächiger, stark komprimierter Einzelbilder).
// Check liest nur die Kopfdaten (bei GIFs alle Bildbeschreibungen, ohne die LZW-Daten zu
// entpacken) und lehnt ab, bevor Speicher für Pixel angelegt wird.

// ErrImageBroken: Datei ist beschädigt oder unvollständig.
var ErrImageBroken = errors.New("Die Bilddatei ist beschädigt")

// Check prüft Format und Maße eines Bildes vor dem Decodieren.
func Check(data []byte) (image.Config, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return cfg, "", ErrImageFormat
	}
	if cfg.Width > MaxImageSide || cfg.Height > MaxImageSide || cfg.Width < 1 || cfg.Height < 1 {
		return cfg, format, ErrImageTooBig
	}
	if format == "gif" {
		if _, err := ScanGIF(data); err != nil {
			return cfg, format, err
		}
	}
	return cfg, format, nil
}

// GIFInfo fasst die Struktur eines GIFs zusammen.
type GIFInfo struct {
	W, H   int // logische Bildfläche
	Frames int
	Pixels int // Summe der Flächen aller Einzelbilder
}

// ScanGIF geht die Blöcke eines GIFs durch (ohne LZW-Daten zu entpacken) und prüft
// Bildanzahl, Maße jedes Einzelbilds und den Gesamtaufwand. Aufwand: ein Durchlauf über
// die Datei, keine Speicheranforderung abhängig vom Inhalt.
func ScanGIF(data []byte) (GIFInfo, error) {
	var in GIFInfo
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return in, ErrImageFormat
	}
	in.W, in.H = int(data[6])|int(data[7])<<8, int(data[8])|int(data[9])<<8
	if in.W < 1 || in.H < 1 || in.W > MaxImageSide || in.H > MaxImageSide {
		return in, ErrImageTooBig
	}
	p := 13
	if data[10]&0x80 != 0 { // globale Farbtabelle
		p += 3 << (int(data[10]&7) + 1)
	}
	// skipSub überspringt Unterblöcke bis zum Endblock (Länge 0).
	skipSub := func() bool {
		for {
			if p >= len(data) {
				return false
			}
			n := int(data[p])
			p++
			if n == 0 {
				return true
			}
			p += n
		}
	}
	for {
		if p >= len(data) {
			// Ohne Abschlusszeichen: wie der Decoder die vorhandenen Bilder gelten lassen
			if in.Frames == 0 {
				return in, ErrImageBroken
			}
			return in, nil
		}
		switch data[p] {
		case 0x3B: // Ende
			if in.Frames == 0 {
				return in, ErrImageBroken
			}
			return in, nil
		case 0x21: // Erweiterung: Kennung + Unterblöcke
			p += 2
			if !skipSub() {
				return in, ErrImageBroken
			}
		case 0x2C: // Bildbeschreibung
			if p+10 > len(data) {
				return in, ErrImageBroken
			}
			d := data[p+1 : p+10]
			left, top := int(d[0])|int(d[1])<<8, int(d[2])|int(d[3])<<8
			w, h := int(d[4])|int(d[5])<<8, int(d[6])|int(d[7])<<8
			if w < 1 || h < 1 || left+w > in.W || top+h > in.H {
				return in, ErrImageTooBig // Einzelbild größer als die Bildfläche
			}
			in.Frames++
			in.Pixels += w * h
			if in.Frames > maxGIFFrames || in.Pixels > maxGIFWork || in.W*in.H*in.Frames > maxGIFWork {
				return in, ErrImageTooBig
			}
			p += 10
			if d[8]&0x80 != 0 { // lokale Farbtabelle
				p += 3 << (int(d[8]&7) + 1)
			}
			p++ // LZW-Mindestcodelänge
			if !skipSub() {
				return in, ErrImageBroken
			}
		default:
			return in, ErrImageBroken
		}
	}
}
