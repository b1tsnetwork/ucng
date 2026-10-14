// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package wsock

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client ist eine einfache WebSocket-Gegenstelle (für Tests und Lastmessungen; ohne TLS).
type Client struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

// Dial verbindet sich mit ws://host/pfad.
func Dial(rawURL string, header http.Header) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "ws" {
		return nil, errors.New("wsock: nur ws:// wird unterstützt")
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var k [16]byte
	rand.Read(k[:])
	key := base64.StdEncoding.EncodeToString(k[:])
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", u.RequestURI(), u.Host, key)
	for name, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := c.Write([]byte(b.String())); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		c.Close()
		return nil, fmt.Errorf("wsock: %d %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return &Client{c: c, r: r}, nil
}

func (c *Client) send(op int, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var h [14]byte
	h[0] = 0x80 | byte(op)
	n := len(p)
	hl := 2
	switch {
	case n < 126:
		h[1] = 0x80 | byte(n)
	case n <= 0xffff:
		h[1] = 0x80 | 126
		binary.BigEndian.PutUint16(h[2:], uint16(n))
		hl = 4
	default:
		h[1] = 0x80 | 127
		binary.BigEndian.PutUint64(h[2:], uint64(n))
		hl = 10
	}
	var mask [4]byte
	rand.Read(mask[:])
	copy(h[hl:], mask[:])
	hl += 4
	buf := append([]byte{}, h[:hl]...)
	for i, x := range p {
		buf = append(buf, x^mask[i&3])
	}
	_, err := c.c.Write(buf)
	return err
}

func (c *Client) WriteText(p []byte) error   { return c.send(OpText, p) }
func (c *Client) WriteBinary(p []byte) error { return c.send(OpBinary, p) }
func (c *Client) Close() error {
	c.send(opClose, []byte{0x03, 0xE8})
	return c.c.Close()
}

// Read liefert die nächste Daten-Nachricht (Ping wird beantwortet).
func (c *Client) Read(timeout time.Duration) (int, []byte, error) {
	for {
		c.c.SetReadDeadline(time.Now().Add(timeout))
		var h [2]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return 0, nil, err
		}
		op := int(h[0] & 0x0f)
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 16<<20 {
			return 0, nil, ErrTooLarge
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.r, p); err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			c.send(opPong, p)
		case opPong:
		case opClose:
			return 0, p, io.EOF
		default:
			return op, p, nil
		}
	}
}
