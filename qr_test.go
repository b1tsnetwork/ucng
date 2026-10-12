// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQRSizes(t *testing.T) {
	for _, n := range []int{1, 14, 15, 60, 120, 200, 330} {
		q, err := EncodeQR(strings.Repeat("a", n), -1)
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if q.Size != 17+4*q.Version {
			t.Fatal("Größe")
		}
	}
	if _, err := EncodeQR(strings.Repeat("a", 500), -1); err == nil {
		t.Fatal("zu lang erwartet")
	}
	q, _ := EncodeQR("otpauth://totp/GoChat:anna?secret=ABC&issuer=GoChat", -1)
	if svg := q.SVG(); !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "M") {
		t.Fatal(svg)
	}
}

// QR_OUT=<ordner> go test -run TestQRExport schreibt PBM-Bilder zum Prüfen mit einem Decoder.
func TestQRExport(t *testing.T) {
	dir := os.Getenv("QR_OUT")
	if dir == "" {
		t.Skip()
	}
	texts := []string{"HELLO", "otpauth://totp/GoChat:anna?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=GoChat",
		strings.Repeat("Größe-äöü ", 20), strings.Repeat("x", 300)}
	for i, txt := range texts {
		for m := 0; m < 8; m++ {
			q, err := EncodeQR(txt, m)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			sc := 8
			n := (q.Size + 8) * sc
			fmt.Fprintf(&b, "P1\n%d %d\n", n, n)
			for y := 0; y < n; y++ {
				for x := 0; x < n; x++ {
					mx, my := x/sc-4, y/sc-4
					d := mx >= 0 && my >= 0 && mx < q.Size && my < q.Size && q.Dark(mx, my)
					if d {
						b.WriteString("1 ")
					} else {
						b.WriteString("0 ")
					}
				}
				b.WriteString("\n")
			}
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("q%d_v%d_m%d.pbm", i, q.Version, m)), []byte(b.String()), 0o644)
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("q%d.txt", i)), []byte(txt), 0o644)
		}
	}
}
