// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package wsock

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func echoServer(t *testing.T, max int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, func(o string) bool { return o == "" || o == "http://ok.example" }, max)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			op, p, err := c.Read()
			if err != nil {
				return
			}
			if op == OpText {
				c.WriteText(p)
			} else {
				c.WriteBinary(p)
			}
		}
	}))
}

func TestEcho(t *testing.T) {
	ts := echoServer(t, 70000)
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	c, err := Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, n := range []int{0, 10, 125, 126, 300, 65535, 65536} {
		p := bytes.Repeat([]byte{byte(n)}, n)
		c.WriteBinary(p)
		op, got, err := c.Read(2 * time.Second)
		if err != nil || op != OpBinary || !bytes.Equal(got, p) {
			t.Fatalf("Länge %d: op %d err %v len %d", n, op, err, len(got))
		}
	}
	c.WriteText([]byte("hallo"))
	if op, got, _ := c.Read(time.Second); op != OpText || string(got) != "hallo" {
		t.Fatal("Text")
	}
}

func TestLimitsAndOrigin(t *testing.T) {
	ts := echoServer(t, 1000)
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	if _, err := Dial(url, http.Header{"Origin": {"http://evil.example"}}); err == nil {
		t.Fatal("fremde Herkunft angenommen")
	}
	c, err := Dial(url, http.Header{"Origin": {"http://ok.example"}})
	if err != nil {
		t.Fatal(err)
	}
	c.WriteBinary(make([]byte, 2000))
	if _, _, err := c.Read(time.Second); err != io.EOF {
		t.Fatalf("zu große Nachricht: %v", err)
	}
}
