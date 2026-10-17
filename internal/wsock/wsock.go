// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wsock ist ein kleiner WebSocket-Server (RFC 6455) nur mit der Standardbibliothek –
// genau so viel, wie die Sprachkanäle brauchen: Text- und Binärnachrichten, Ping/Pong,
// geordnetes Schließen, Fragmente, Größenlimit und Origin-Prüfung. Keine Erweiterungen
// (keine Kompression – Opus ist schon komprimiert).
package wsock

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	OpText   = 1
	OpBinary = 2
	opClose  = 8
	opPing   = 9
	opPong   = 10
	opCont   = 0
)

var (
	ErrTooLarge = errors.New("websocket: Nachricht zu groß")
	ErrProtocol = errors.New("websocket: Protokollfehler")
	ErrClosed   = errors.New("websocket: geschlossen")
)

// Conn ist eine WebSocket-Verbindung (Server-Seite).
type Conn struct {
	c        net.Conn
	r        *bufio.Reader
	wmu      sync.Mutex
	maxMsg   int
	closed   bool
	closeErr sync.Once
	idle     time.Duration // > 0: jedes Frame (auch Pong) verlängert die Lesefrist
}

// Upgrade nimmt eine WebSocket-Verbindung an. originOK prüft die Origin-Kopfzeile (nil = jede).
// maxMsg begrenzt die Größe einer Nachricht (auch zusammengesetzt aus Fragmenten).
func Upgrade(w http.ResponseWriter, r *http.Request, originOK func(string) bool, maxMsg int) (*Conn, error) {
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!hasToken(r.Header.Get("Connection"), "upgrade") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "WebSocket erwartet", http.StatusBadRequest)
		return nil, ErrProtocol
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		http.Error(w, "ungültiger Schlüssel", http.StatusBadRequest)
		return nil, ErrProtocol
	}
	if originOK != nil && !originOK(r.Header.Get("Origin")) {
		http.Error(w, "Herkunft nicht erlaubt", http.StatusForbidden)
		return nil, ErrProtocol
	}
	// ResponseController folgt Unwrap() – auch hinter Middleware, die den Writer einpackt.
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "WebSocket wird hier nicht unterstützt", http.StatusInternalServerError)
		return nil, err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	c.SetDeadline(time.Time{})
	if _, err := rw.WriteString(resp); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	if maxMsg <= 0 {
		maxMsg = 64 << 10
	}
	return &Conn{c: c, r: rw.Reader, maxMsg: maxMsg}, nil
}

// NewServerConn macht aus einer bestehenden Verbindung (z. B. net.Pipe) die Server-Seite eines
// WebSockets – ohne HTTP-Handshake (für Verbindungen im selben Prozess).
func NewServerConn(c net.Conn, maxMsg int) *Conn {
	if maxMsg <= 0 {
		maxMsg = 64 << 10
	}
	return &Conn{c: c, r: bufio.NewReader(c), maxMsg: maxMsg}
}

func hasToken(v, t string) bool {
	for _, x := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(x), t) {
			return true
		}
	}
	return false
}

// RemoteAddr der zugrunde liegenden Verbindung.
func (c *Conn) RemoteAddr() string { return c.c.RemoteAddr().String() }

// SetReadDeadline setzt die Frist für das nächste Read.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.c.SetReadDeadline(t) }

// SetIdleTimeout: Die Verbindung gilt als tot, wenn so lange gar nichts ankommt – auch keine
// Antwort (Pong) auf unsere Pings. Ersetzt SetReadDeadline in einer Lese-Schleife.
func (c *Conn) SetIdleTimeout(d time.Duration) { c.idle = d }

// Read liefert die nächste Daten-Nachricht (Text oder Binär). Ping wird beantwortet, Pong
// ignoriert, Close beendet die Verbindung (io.EOF).
func (c *Conn) Read() (op int, data []byte, err error) {
	var msg []byte
	msgOp := -1
	for {
		if c.idle > 0 {
			c.c.SetReadDeadline(time.Now().Add(c.idle))
		}
		fin, fop, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code := []byte{0x03, 0xE8} // 1000
			if len(payload) >= 2 {
				code = payload[:2]
			}
			c.write(opClose, code)
			c.Close()
			return 0, nil, io.EOF
		case OpText, OpBinary:
			if msgOp != -1 {
				return 0, nil, c.fail(1002)
			}
			msgOp = fop
		case opCont:
			if msgOp == -1 {
				return 0, nil, c.fail(1002)
			}
		default:
			return 0, nil, c.fail(1002)
		}
		if len(msg)+len(payload) > c.maxMsg {
			return 0, nil, c.fail(1009)
		}
		msg = append(msg, payload...)
		if fin {
			return msgOp, msg, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op int, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.r, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 { // keine Erweiterungen ausgehandelt
		return false, 0, nil, c.fail(1002)
	}
	op = int(h[0] & 0x0f)
	if h[1]&0x80 == 0 { // Clients müssen maskieren
		return false, 0, nil, c.fail(1002)
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= 8 && (n > 125 || !fin) { // Steuer-Rahmen: kurz und nicht fragmentiert
		return false, 0, nil, c.fail(1002)
	}
	if n > uint64(c.maxMsg) {
		return false, 0, nil, c.fail(1009)
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.r, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return fin, op, payload, nil
}

// fail schließt mit einem Fehlercode.
func (c *Conn) fail(code int) error {
	c.write(opClose, []byte{byte(code >> 8), byte(code)})
	c.Close()
	if code == 1009 {
		return ErrTooLarge
	}
	return ErrProtocol
}

// WriteText / WriteBinary schicken eine Nachricht (ein Rahmen, unmaskiert).
func (c *Conn) WriteText(p []byte) error   { return c.write(OpText, p) }
func (c *Conn) WriteBinary(p []byte) error { return c.write(OpBinary, p) }

// Ping schickt ein Lebenszeichen.
func (c *Conn) Ping() error { return c.write(opPing, nil) }

func (c *Conn) write(op int, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return ErrClosed
	}
	var h [10]byte
	h[0] = 0x80 | byte(op)
	n := len(p)
	hl := 2
	switch {
	case n < 126:
		h[1] = byte(n)
	case n <= 0xffff:
		h[1] = 126
		binary.BigEndian.PutUint16(h[2:], uint16(n))
		hl = 4
	default:
		h[1] = 127
		binary.BigEndian.PutUint64(h[2:], uint64(n))
		hl = 10
	}
	c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 0, hl+n) // ein Schreibvorgang je Nachricht
	buf = append(buf, h[:hl]...)
	buf = append(buf, p...)
	_, err := c.c.Write(buf)
	return err
}

// CloseWith schließt geordnet mit Code und Grund (für den Client sichtbar).
func (c *Conn) CloseWith(code int, reason string) {
	if len(reason) > 120 {
		reason = reason[:120]
	}
	c.write(opClose, append([]byte{byte(code >> 8), byte(code)}, reason...))
	c.Close()
}

// Close beendet die Verbindung sofort.
func (c *Conn) Close() error {
	c.wmu.Lock()
	c.closed = true
	c.wmu.Unlock()
	return c.c.Close()
}
