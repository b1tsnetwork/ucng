// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"strings"
)

// Kleiner QR-Code-Erzeuger (Byte-Modus, Fehlerkorrektur M, Version 1–15) für den
// 2FA-Einrichtungscode. Ausgabe als SVG – ohne externe Bibliotheken.

type qrVersion struct {
	ec     int       // Fehlerkorrektur-Codewörter je Block
	groups [2][2]int // {Anzahl Blöcke, Daten-Codewörter je Block}
	align  []int
	rem    int // Restbits
}

var qrVersions = [...]qrVersion{
	1:  {10, [2][2]int{{1, 16}, {0, 0}}, nil, 0},
	2:  {16, [2][2]int{{1, 28}, {0, 0}}, []int{6, 18}, 7},
	3:  {26, [2][2]int{{1, 44}, {0, 0}}, []int{6, 22}, 7},
	4:  {18, [2][2]int{{2, 32}, {0, 0}}, []int{6, 26}, 7},
	5:  {24, [2][2]int{{2, 43}, {0, 0}}, []int{6, 30}, 7},
	6:  {16, [2][2]int{{4, 27}, {0, 0}}, []int{6, 34}, 7},
	7:  {18, [2][2]int{{4, 31}, {0, 0}}, []int{6, 22, 38}, 0},
	8:  {22, [2][2]int{{2, 38}, {2, 39}}, []int{6, 24, 42}, 0},
	9:  {22, [2][2]int{{3, 36}, {2, 37}}, []int{6, 26, 46}, 0},
	10: {26, [2][2]int{{4, 43}, {1, 44}}, []int{6, 28, 50}, 0},
	11: {30, [2][2]int{{1, 50}, {4, 51}}, []int{6, 30, 54}, 0},
	12: {22, [2][2]int{{6, 36}, {2, 37}}, []int{6, 32, 58}, 0},
	13: {22, [2][2]int{{8, 37}, {1, 38}}, []int{6, 34, 62}, 0},
	14: {24, [2][2]int{{4, 40}, {5, 41}}, []int{6, 26, 46, 66}, 3},
	15: {24, [2][2]int{{5, 41}, {5, 42}}, []int{6, 26, 48, 70}, 3},
}

func (v qrVersion) dataCodewords() int {
	return v.groups[0][0]*v.groups[0][1] + v.groups[1][0]*v.groups[1][1]
}

var errQRTooLong = errors.New("Text zu lang für den QR-Code")

// --- Galois-Feld GF(256), Polynom 0x11D ---

var gfExp, gfLog [512]int

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

func rsGenerator(n int) []int {
	g := []int{1}
	for i := 0; i < n; i++ {
		ng := make([]int, len(g)+1)
		for j, c := range g {
			ng[j] ^= c
			ng[j+1] ^= gfMul(c, gfExp[i])
		}
		g = ng
	}
	return g
}

func rsRemainder(data []int, n int) []int {
	gen := rsGenerator(n)
	rem := make([]int, n)
	for _, d := range data {
		factor := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for i := 0; i < n; i++ {
			rem[i] ^= gfMul(gen[i+1], factor)
		}
	}
	return rem
}

// --- Kodierung ---

type bitBuf struct{ bits []bool }

func (b *bitBuf) put(v, n int) {
	for i := n - 1; i >= 0; i-- {
		b.bits = append(b.bits, (v>>i)&1 == 1)
	}
}

// qrCodewords erzeugt die verschachtelten Codewörter für eine Version.
func qrCodewords(data []byte, ver int) ([]int, error) {
	v := qrVersions[ver]
	capBits := v.dataCodewords() * 8
	var b bitBuf
	b.put(0b0100, 4)
	if ver <= 9 {
		b.put(len(data), 8)
	} else {
		b.put(len(data), 16)
	}
	for _, c := range data {
		b.put(int(c), 8)
	}
	if len(b.bits) > capBits {
		return nil, errQRTooLong
	}
	for i := 0; i < 4 && len(b.bits) < capBits; i++ {
		b.bits = append(b.bits, false)
	}
	for len(b.bits)%8 != 0 {
		b.bits = append(b.bits, false)
	}
	pads := []int{0xEC, 0x11}
	for i := 0; len(b.bits) < capBits; i++ {
		b.put(pads[i%2], 8)
	}
	cw := make([]int, capBits/8)
	for i := range cw {
		for j := 0; j < 8; j++ {
			if b.bits[i*8+j] {
				cw[i] |= 1 << (7 - j)
			}
		}
	}
	// in Blöcke aufteilen
	var blocks, ecs [][]int
	pos := 0
	for _, g := range v.groups {
		for i := 0; i < g[0]; i++ {
			blk := cw[pos : pos+g[1]]
			pos += g[1]
			blocks = append(blocks, blk)
			ecs = append(ecs, rsRemainder(blk, v.ec))
		}
	}
	var out []int
	maxLen := v.groups[0][1]
	if v.groups[1][1] > maxLen {
		maxLen = v.groups[1][1]
	}
	for i := 0; i < maxLen; i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	for i := 0; i < v.ec; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out, nil
}

// QRMatrix ist das fertige Modulraster (true = dunkel).
type QRMatrix struct {
	Size    int
	Version int
	Mask    int
	mod     [][]bool
	fn      [][]bool // Funktionsmuster (nicht maskieren)
}

func (q *QRMatrix) Dark(x, y int) bool { return q.mod[y][x] }

func newQRMatrix(ver int) *QRMatrix {
	n := 17 + 4*ver
	q := &QRMatrix{Size: n, Version: ver}
	q.mod = make([][]bool, n)
	q.fn = make([][]bool, n)
	for i := range q.mod {
		q.mod[i] = make([]bool, n)
		q.fn[i] = make([]bool, n)
	}
	return q
}

func (q *QRMatrix) set(x, y int, dark bool) {
	q.mod[y][x] = dark
	q.fn[y][x] = true
}

func (q *QRMatrix) drawFunctionPatterns() {
	n := q.Size
	for i := 0; i < n; i++ { // Taktmuster
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	finder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= n || y >= n {
					continue
				}
				d := max(abs(dx), abs(dy))
				q.set(x, y, d != 2 && d != 4)
			}
		}
	}
	finder(3, 3)
	finder(n-4, 3)
	finder(3, n-4)
	al := qrVersions[q.Version].align
	for i, ax := range al {
		for j, ay := range al {
			if (i == 0 && j == 0) || (i == 0 && j == len(al)-1) || (i == len(al)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.set(ax+dx, ay+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reservieren
	if q.Version >= 7 {
		q.drawVersion()
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// drawFormat schreibt die Formatinformation (Fehlerkorrektur M = 00, Maske).
func (q *QRMatrix) drawFormat(mask int) {
	data := 0<<3 | mask // M = 0b00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>i)&1 == 1 }
	n := q.Size
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, n-15+i, bit(i))
	}
	q.set(8, n-8, true) // dunkles Modul
}

func (q *QRMatrix) drawVersion() {
	rem := q.Version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := q.Version<<12 | rem
	n := q.Size
	for i := 0; i < 18; i++ {
		b := (bits>>i)&1 == 1
		a, c := n-11+i%3, i/3
		q.set(a, c, b)
		q.set(c, a, b)
	}
}

func (q *QRMatrix) drawCodewords(cw []int) {
	n := q.Size
	i := 0
	total := len(cw) * 8
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := ((right + 1) & 2) == 0
				y := vert
				if upward {
					y = n - 1 - vert
				}
				if q.fn[y][x] {
					continue
				}
				if i < total {
					q.mod[y][x] = (cw[i>>3]>>(7-(i&7)))&1 == 1
					i++
				}
			}
		}
	}
}

func qrMaskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

func (q *QRMatrix) applyMask(mask int) {
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if !q.fn[y][x] && qrMaskBit(mask, x, y) {
				q.mod[y][x] = !q.mod[y][x]
			}
		}
	}
}

// penalty bewertet ein Raster nach den Regeln des Standards (kleiner = besser).
func (q *QRMatrix) penalty() int {
	n := q.Size
	p := 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i < n; i++ {
			if get(i) == get(i-1) {
				run++
				if run == 5 {
					p += 3
				} else if run > 5 {
					p++
				}
			} else {
				run = 1
			}
		}
		// Muster 1:1:3:1:1 mit 4 hellen Modulen davor oder danach
		for i := 0; i+11 <= n; i++ {
			a := []bool{true, false, true, true, true, false, true, false, false, false, false}
			b := []bool{false, false, false, false, true, false, true, true, true, false, true}
			ma, mb := true, true
			for k := 0; k < 11; k++ {
				if get(i+k) != a[k] {
					ma = false
				}
				if get(i+k) != b[k] {
					mb = false
				}
			}
			if ma {
				p += 40
			}
			if mb {
				p += 40
			}
		}
	}
	for y := 0; y < n; y++ {
		line(func(i int) bool { return q.mod[y][i] })
	}
	for x := 0; x < n; x++ {
		line(func(i int) bool { return q.mod[i][x] })
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.mod[y][x] {
				dark++
			}
			if x < n-1 && y < n-1 {
				c := q.mod[y][x]
				if c == q.mod[y][x+1] && c == q.mod[y+1][x] && c == q.mod[y+1][x+1] {
					p += 3
				}
			}
		}
	}
	p += abs(dark*20-n*n*10) / (n * n) * 10
	return p
}

// EncodeQR erzeugt einen QR-Code; mask < 0 = beste Maske wählen.
func EncodeQR(text string, mask int) (*QRMatrix, error) {
	data := []byte(text)
	for ver := 1; ver < len(qrVersions); ver++ {
		cw, err := qrCodewords(data, ver)
		if err != nil {
			continue
		}
		best, bestP := (*QRMatrix)(nil), 0
		for m := 0; m < 8; m++ {
			if mask >= 0 && m != mask {
				continue
			}
			q := newQRMatrix(ver)
			q.drawFunctionPatterns()
			q.drawCodewords(cw)
			q.applyMask(m)
			q.drawFormat(m)
			q.Mask = m
			if p := q.penalty(); best == nil || p < bestP {
				best, bestP = q, p
			}
		}
		return best, nil
	}
	return nil, errQRTooLong
}

// SVG liefert den Code als SVG (mit 4 Modulen Rand).
func (q *QRMatrix) SVG() string {
	var b strings.Builder
	n := q.Size + 8
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if q.mod[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
