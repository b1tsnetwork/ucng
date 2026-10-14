// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package imgscale

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"runtime"
	"testing"
)

// bombGIF: großflächige, einfarbige Einzelbilder – winzig als Datei, riesig entpackt.
func bombGIF(t *testing.T, side, frames int) []byte {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	g := &gif.GIF{Config: image.Config{Width: side, Height: side, ColorModel: pal}}
	img := image.NewPaletted(image.Rect(0, 0, side, side), pal)
	for i := 0; i < frames; i++ {
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 1)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestScanGIFRejectsBombBeforeDecoding(t *testing.T) {
	data := bombGIF(t, 4000, 4) // 64 Mio. Pixel entpackt
	t.Logf("Dateigröße: %d KB", len(data)/1024)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := ProcessImage(data, 480, 480, false, true)
	runtime.ReadMemStats(&after)
	if err != ErrImageTooBig {
		t.Fatalf("erwartet ErrImageTooBig, bekam %v", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("Prüfung hat %d MB angefordert", grew>>20)
	}
	// Zum Vergleich: vollständiges Decodieren derselben Datei
	runtime.ReadMemStats(&before)
	gif.DecodeAll(bytes.NewReader(data))
	runtime.ReadMemStats(&after)
	t.Logf("Vorabprüfung: <8 MB, DecodeAll hätte %d MB angefordert", (after.TotalAlloc-before.TotalAlloc)>>20)
}

func TestScanGIFNormal(t *testing.T) {
	data := bombGIF(t, 200, 20)
	in, err := ScanGIF(data)
	if err != nil || in.Frames != 20 || in.W != 200 || in.Pixels != 200*200*20 {
		t.Fatalf("%+v %v", in, err)
	}
	if _, err := ProcessImage(data, 100, 100, false, true); err != nil {
		t.Fatal(err)
	}
}

func TestScanGIFBroken(t *testing.T) {
	data := bombGIF(t, 50, 2)
	if _, err := ScanGIF(data[:20]); err == nil {
		t.Fatal("abgeschnittene Datei angenommen")
	}
	// Einzelbild größer als die Bildfläche
	bad := append([]byte(nil), data...)
	bad[6], bad[8] = 10, 10 // Bildfläche 10×10, Einzelbilder 50×50
	if _, err := ScanGIF(bad); err != ErrImageTooBig {
		t.Fatalf("zu großes Einzelbild: %v", err)
	}
	if _, err := ScanGIF([]byte("GIF89a")); err == nil {
		t.Fatal("zu kurz")
	}
}
