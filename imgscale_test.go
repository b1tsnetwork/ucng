// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

func testPNG(w, h int, c color.Color) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// testGIF erzeugt eine Animation: ein Quadrat wandert über einen transparenten Hintergrund.
func testGIF(w, h, frames int) []byte {
	pal := color.Palette{color.RGBA{}, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < frames; i++ {
		f := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := 0; y < h/2; y++ {
			for x := 0; x < w/2; x++ {
				f.SetColorIndex((x+i*w/frames)%w, y, uint8(1+i%2))
			}
		}
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 8)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	var buf bytes.Buffer
	gif.EncodeAll(&buf, g)
	return buf.Bytes()
}

func TestEmoteScaledToFixedSquare(t *testing.T) {
	for _, size := range []int{32, 64} {
		// breites PNG → genau size×size, mit transparentem Rand oben/unten
		out, err := ProcessImage(testPNG(300, 100, color.RGBA{0, 200, 0, 255}), size, size, true, false)
		if err != nil {
			t.Fatal(err)
		}
		img, _ := png.Decode(bytes.NewReader(out.Data))
		if out.Ext != "png" || img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("%d: %v %v", size, out.Ext, img.Bounds())
		}
		if _, _, _, a := img.At(size/2, 0).RGBA(); a != 0 {
			t.Fatal("Rand nicht transparent")
		}
		if r, g, _, a := img.At(size/2, size/2).RGBA(); a == 0 || g < r {
			t.Fatal("Mitte falsch")
		}
	}
	// kleines Bild wird für Emotes vergrößert
	out, _ := ProcessImage(testPNG(8, 8, color.RGBA{255, 0, 0, 255}), 64, 64, true, false)
	if out.W != 64 || out.H != 64 {
		t.Fatalf("%dx%d", out.W, out.H)
	}
	// JPEG wird angenommen
	var jb bytes.Buffer
	jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 50, 50)), nil)
	if _, err := ProcessImage(jb.Bytes(), 32, 32, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessImage([]byte("kein bild"), 32, 32, true, false); err != ErrImageFormat {
		t.Fatal(err)
	}
}

func TestAnimatedGIFStaysAnimated(t *testing.T) {
	start := time.Now()
	out, err := ProcessImage(testGIF(480, 270, 20), 300, 300, false, true)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(out.Data))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Animated || len(g.Image) != 20 || g.Config.Width != 300 || g.Config.Height != 169 || g.Delay[0] != 8 {
		t.Fatalf("frames=%d %dx%d delay=%v", len(g.Image), g.Config.Width, g.Config.Height, g.Delay[:1])
	}
	if !raceEnabled && time.Since(start) > 3*time.Second {
		t.Fatalf("zu langsam: %v", time.Since(start))
	}
	// kleine GIFs werden nicht vergrößert
	out, _ = ProcessImage(testGIF(100, 80, 3), 300, 300, false, true)
	if out.W != 100 || out.H != 80 {
		t.Fatalf("%dx%d", out.W, out.H)
	}
	// animiertes Emote: quadratisch und animiert
	out, _ = ProcessImage(testGIF(120, 60, 4), 32, 32, true, false)
	if out.Ext != "gif" || !out.Animated || out.W != 32 || out.H != 32 {
		t.Fatalf("%+v", out.Ext)
	}
	if _, err := ProcessImage(testPNG(10, 10, color.White), 300, 300, false, true); err != ErrNotGIF {
		t.Fatal(err)
	}
}
