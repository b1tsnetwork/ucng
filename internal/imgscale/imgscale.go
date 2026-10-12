// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package imgscale skaliert Bilder (auch animierte GIFs) nur mit der Standardbibliothek.
// Benutzt von UCNG und vom Beacon.
package imgscale

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"sort"
)

// Bildverarbeitung nur mit der Standardbibliothek:
//   - Emotes werden auf ein festes Quadrat (z. B. 32×32 oder 64×64) skaliert, mittig mit
//     transparentem Rand, falls das Bild nicht quadratisch ist.
//   - GIFs werden in einen Rahmen (z. B. höchstens 300×300) eingepasst – nie vergrößert.
//   - Animierte GIFs bleiben animiert: jedes Bild wird vollständig zusammengesetzt,
//     skaliert und mit einer eigenen Farbtabelle neu kodiert.

var (
	ErrImageFormat = errors.New("Nur PNG, JPEG oder GIF werden unterstützt")
	ErrImageTooBig = errors.New("Das Bild ist zu groß (höchstens 4000×4000 Pixel, bei GIFs höchstens 400 Einzelbilder)")
	ErrNotGIF      = errors.New("Bitte eine GIF-Datei wählen")
)

const (
	MaxImageSide   = 4000
	maxGIFFrames   = 400
	maxGIFWork     = 120_000_000 // Summe aus Breite × Höhe aller Einzelbilder
	gifTransparent = 0           // Index der Transparenz in neu erzeugten Farbtabellen
)

// ScaledImage ist das Ergebnis der Verarbeitung.
type ScaledImage struct {
	Data     []byte
	Ext      string // "png" oder "gif"
	W, H     int
	Animated bool
}

// --- Skalieren ---

// toNRGBA wandelt in ein vormultipliziertes RGBA-Bild (für korrektes Mitteln mit Transparenz).
func ToRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// weights berechnet für jede Zielspalte die beteiligten Quellpixel mit Gewichten
// (Dreiecksfilter, beim Verkleinern so breit wie das Verkleinerungsverhältnis).
type contrib struct {
	start int
	w     []float64
}

func weights(srcN, dstN int) []contrib {
	scale := float64(srcN) / float64(dstN)
	support := math.Max(1, scale)
	out := make([]contrib, dstN)
	for i := 0; i < dstN; i++ {
		center := (float64(i)+0.5)*scale - 0.5
		lo := int(math.Floor(center - support))
		hi := int(math.Ceil(center + support))
		if lo < 0 {
			lo = 0
		}
		if hi > srcN-1 {
			hi = srcN - 1
		}
		ws := make([]float64, 0, hi-lo+1)
		sum := 0.0
		for j := lo; j <= hi; j++ {
			w := 1 - math.Abs(float64(j)-center)/support
			if w < 0 {
				w = 0
			}
			ws = append(ws, w)
			sum += w
		}
		if sum == 0 {
			ws = []float64{1}
			lo = int(math.Max(0, math.Min(float64(srcN-1), math.Round(center))))
			sum = 1
		}
		for k := range ws {
			ws[k] /= sum
		}
		out[i] = contrib{start: lo, w: ws}
	}
	return out
}

// resize skaliert ein vormultipliziertes RGBA-Bild auf w×h (zwei Durchgänge).
func Resize(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if sw == w && sh == h {
		return src
	}
	// horizontal
	cx := weights(sw, w)
	tmp := make([]float64, w*sh*4)
	for y := 0; y < sh; y++ {
		row := src.Pix[y*src.Stride:]
		for x := 0; x < w; x++ {
			c := cx[x]
			var r, g, b, a float64
			for k, wt := range c.w {
				p := (c.start + k) * 4
				r += wt * float64(row[p])
				g += wt * float64(row[p+1])
				b += wt * float64(row[p+2])
				a += wt * float64(row[p+3])
			}
			o := (y*w + x) * 4
			tmp[o], tmp[o+1], tmp[o+2], tmp[o+3] = r, g, b, a
		}
	}
	// vertikal
	cy := weights(sh, h)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	clamp := func(v float64) uint8 {
		if v <= 0 {
			return 0
		}
		if v >= 255 {
			return 255
		}
		return uint8(v + 0.5)
	}
	for y := 0; y < h; y++ {
		c := cy[y]
		for x := 0; x < w; x++ {
			var r, g, b, a float64
			for k, wt := range c.w {
				o := ((c.start+k)*w + x) * 4
				r += wt * tmp[o]
				g += wt * tmp[o+1]
				b += wt * tmp[o+2]
				a += wt * tmp[o+3]
			}
			p := y*dst.Stride + x*4
			A := clamp(a)
			// vormultipliziert: Farbe darf nicht größer als Alpha sein
			dst.Pix[p], dst.Pix[p+1], dst.Pix[p+2], dst.Pix[p+3] = minU8(clamp(r), A), minU8(clamp(g), A), minU8(clamp(b), A), A
		}
	}
	return dst
}

func minU8(a, b uint8) uint8 {
	if a < b {
		return a
	}
	return b
}

// fit liefert die Größe, mit der w×h in boxW×boxH passt (Seitenverhältnis bleibt).
// upscale = false: nie größer als das Original.
func Fit(w, h, boxW, boxH int, upscale bool) (int, int) {
	s := math.Min(float64(boxW)/float64(w), float64(boxH)/float64(h))
	if !upscale && s > 1 {
		s = 1
	}
	nw, nh := int(math.Round(float64(w)*s)), int(math.Round(float64(h)*s))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

// square setzt ein Bild mittig auf eine transparente Fläche size×size.
func square(img *image.RGBA, size int) *image.RGBA {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if w == size && h == size {
		return img
	}
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	off := image.Pt((size-w)/2, (size-h)/2)
	draw.Draw(dst, image.Rectangle{off, off.Add(image.Pt(w, h))}, img, image.Point{}, draw.Src)
	return dst
}

// --- Farbtabellen für GIF ---

// quantize erzeugt aus einem Bild eine Farbtabelle (höchstens 255 Farben + Transparenz).
// Farben werden auf 5 Bit je Kanal gruppiert; die häufigsten Gruppen bilden die Tabelle.
func quantize(img *image.RGBA) color.Palette {
	type acc struct{ r, g, b, n int }
	buckets := map[uint16]*acc{}
	for i := 0; i < len(img.Pix); i += 4 {
		a := img.Pix[i+3]
		if a < 128 {
			continue
		}
		r, g, b := unpremul(img.Pix[i], a), unpremul(img.Pix[i+1], a), unpremul(img.Pix[i+2], a)
		k := uint16(r>>3)<<10 | uint16(g>>3)<<5 | uint16(b>>3)
		x := buckets[k]
		if x == nil {
			x = &acc{}
			buckets[k] = x
		}
		x.r += int(r)
		x.g += int(g)
		x.b += int(b)
		x.n++
	}
	list := make([]*acc, 0, len(buckets))
	for _, a := range buckets {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	if len(list) > 255 {
		list = list[:255]
	}
	pal := color.Palette{color.RGBA{}} // Index 0 = transparent
	for _, a := range list {
		pal = append(pal, color.RGBA{uint8(a.r / a.n), uint8(a.g / a.n), uint8(a.b / a.n), 255})
	}
	if len(pal) == 1 {
		pal = append(pal, color.RGBA{0, 0, 0, 255})
	}
	return pal
}

func unpremul(c, a uint8) uint8 {
	if a == 0 || a == 255 {
		return c
	}
	v := int(c) * 255 / int(a)
	if v > 255 {
		v = 255
	}
	return uint8(v)
}

// toPaletted bildet ein Bild ohne Dithering auf eine Farbtabelle ab (Dithering würde in
// Animationen flackern). Ein Zwischenspeicher macht die Suche nach der nächsten Farbe schnell.
func toPaletted(img *image.RGBA, pal color.Palette) *image.Paletted {
	b := img.Bounds()
	dst := image.NewPaletted(b, pal)
	cache := map[uint16]uint8{}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			p := y*img.Stride + x*4
			a := img.Pix[p+3]
			if a < 128 {
				dst.Pix[y*dst.Stride+x] = gifTransparent
				continue
			}
			r, g, bl := unpremul(img.Pix[p], a), unpremul(img.Pix[p+1], a), unpremul(img.Pix[p+2], a)
			k := uint16(r>>3)<<10 | uint16(g>>3)<<5 | uint16(bl>>3)
			idx, ok := cache[k]
			if !ok {
				idx = uint8(nearest(pal, r, g, bl))
				cache[k] = idx
			}
			dst.Pix[y*dst.Stride+x] = idx
		}
	}
	return dst
}

func nearest(pal color.Palette, r, g, b uint8) int {
	best, bestD := 1, math.MaxInt
	for i := 1; i < len(pal); i++ {
		c := pal[i].(color.RGBA)
		dr, dg, db := int(c.R)-int(r), int(c.G)-int(g), int(c.B)-int(b)
		d := 3*dr*dr + 4*dg*dg + 2*db*db
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// --- Verarbeitung ---

// ProcessImage skaliert ein hochgeladenes Bild.
//
//	emote = true:  genau boxW×boxW (quadratisch, vergrößert auch kleine Bilder)
//	emote = false: höchstens boxW×boxH, Seitenverhältnis bleibt, nie vergrößert
//
// Animierte GIFs bleiben animiert; Standbilder werden zu PNG (Emotes) bzw. GIF (GIF-Bibliothek).
func ProcessImage(data []byte, boxW, boxH int, emote, requireGIF bool) (ScaledImage, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ScaledImage{}, ErrImageFormat
	}
	if cfg.Width > MaxImageSide || cfg.Height > MaxImageSide || cfg.Width < 1 || cfg.Height < 1 {
		return ScaledImage{}, ErrImageTooBig
	}
	if requireGIF && format != "gif" {
		return ScaledImage{}, ErrNotGIF
	}
	if format == "gif" {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return ScaledImage{}, ErrImageFormat
		}
		if len(g.Image) > 1 || !emote {
			return ProcessGIF(g, boxW, boxH, emote)
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ScaledImage{}, ErrImageFormat
	}
	src := ToRGBA(img)
	w, h := Fit(src.Bounds().Dx(), src.Bounds().Dy(), boxW, boxH, emote)
	out := Resize(src, w, h)
	if emote {
		out = square(out, boxW)
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, out); err != nil {
		return ScaledImage{}, err
	}
	return ScaledImage{Data: buf.Bytes(), Ext: "png", W: out.Bounds().Dx(), H: out.Bounds().Dy()}, nil
}

// ProcessImageFromRGBA speichert ein bereits skaliertes Bild als PNG.
func ProcessImageFromRGBA(img *image.RGBA) (ScaledImage, error) {
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return ScaledImage{}, err
	}
	return ScaledImage{Data: buf.Bytes(), Ext: "png", W: img.Bounds().Dx(), H: img.Bounds().Dy()}, nil
}

func ProcessGIF(g *gif.GIF, boxW, boxH int, emote bool) (ScaledImage, error) {
	if len(g.Image) == 0 {
		return ScaledImage{}, ErrImageFormat
	}
	if len(g.Image) > maxGIFFrames {
		return ScaledImage{}, ErrImageTooBig
	}
	cw, ch := g.Config.Width, g.Config.Height
	if cw == 0 || ch == 0 {
		for _, f := range g.Image {
			b := f.Bounds()
			if b.Max.X > cw {
				cw = b.Max.X
			}
			if b.Max.Y > ch {
				ch = b.Max.Y
			}
		}
	}
	if cw > MaxImageSide || ch > MaxImageSide || cw*ch*len(g.Image) > maxGIFWork {
		return ScaledImage{}, ErrImageTooBig
	}
	w, h := Fit(cw, ch, boxW, boxH, emote)
	outW, outH := w, h
	if emote {
		outW, outH = boxW, boxW
	}

	canvas := image.NewRGBA(image.Rect(0, 0, cw, ch))
	out := &gif.GIF{LoopCount: g.LoopCount, Config: image.Config{Width: outW, Height: outH}}
	for i, f := range g.Image {
		var backup *image.RGBA
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		if disposal == gif.DisposalPrevious {
			backup = image.NewRGBA(canvas.Bounds())
			copy(backup.Pix, canvas.Pix)
		}
		draw.Draw(canvas, f.Bounds(), f, f.Bounds().Min, draw.Over)

		frame := Resize(canvas, w, h)
		if emote {
			frame = square(frame, boxW)
		}
		pal := quantize(frame)
		out.Image = append(out.Image, toPaletted(frame, pal))
		delay := 10
		if i < len(g.Delay) {
			delay = g.Delay[i]
		}
		out.Delay = append(out.Delay, delay)
		// Jedes Ausgabebild ist vollständig – vor dem nächsten die Fläche leeren.
		out.Disposal = append(out.Disposal, gif.DisposalBackground)

		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(canvas, f.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			copy(canvas.Pix, backup.Pix)
		}
	}
	if len(out.Image) == 1 {
		out.Disposal = nil
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return ScaledImage{}, err
	}
	return ScaledImage{Data: buf.Bytes(), Ext: "gif", W: outW, H: outH, Animated: len(out.Image) > 1}, nil
}

// gifConfig liest Breite und Höhe eines GIFs.
func GIFConfig(data []byte) (image.Config, error) {
	return gif.DecodeConfig(bytes.NewReader(data))
}
