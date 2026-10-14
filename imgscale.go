// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"image"
	"image/gif"

	"ucng/internal/imgscale"
)

// Die Bildverarbeitung liegt in internal/imgscale (gemeinsam mit dem Beacon).

type ScaledImage = imgscale.ScaledImage

var (
	ErrImageFormat = imgscale.ErrImageFormat
	ErrImageTooBig = imgscale.ErrImageTooBig
	ErrNotGIF      = imgscale.ErrNotGIF
	ErrImageBroken = imgscale.ErrImageBroken
)

// checkImage prüft Format, Maße und (bei GIFs) Bildanzahl und Aufwand vor dem Decodieren.
func checkImage(data []byte) (image.Config, string, error) { return imgscale.Check(data) }

func ProcessImage(data []byte, boxW, boxH int, emote, requireGIF bool) (ScaledImage, error) {
	return imgscale.ProcessImage(data, boxW, boxH, emote, requireGIF)
}

func ProcessImageFromRGBA(img *image.RGBA) (ScaledImage, error) {
	return imgscale.ProcessImageFromRGBA(img)
}

func toRGBA(img image.Image) *image.RGBA           { return imgscale.ToRGBA(img) }
func resize(src *image.RGBA, w, h int) *image.RGBA { return imgscale.Resize(src, w, h) }
func fit(w, h, boxW, boxH int, upscale bool) (int, int) {
	return imgscale.Fit(w, h, boxW, boxH, upscale)
}
func processGIF(g *gif.GIF, boxW, boxH int, emote bool) (ScaledImage, error) {
	return imgscale.ProcessGIF(g, boxW, boxH, emote)
}
func gifConfig(data []byte) (image.Config, error) { return imgscale.GIFConfig(data) }

const maxImageSide = imgscale.MaxImageSide
