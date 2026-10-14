// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestIP(t *testing.T) {
	def, _ := New(true, "")
	off, _ := New(false, "")
	cf, _ := New(true, "127.0.0.1, 203.0.113.0/24")
	cases := []struct {
		name   string
		r      *Resolver
		remote string
		xff    []string
		real   string
		want   string
	}{
		{"ohne Vertrauen", off, "198.51.100.7:1", []string{"1.1.1.1"}, "", "198.51.100.7"},
		{"direkt verbunden, gefälscht", def, "198.51.100.7:1", []string{"1.1.1.1"}, "", "198.51.100.7"},
		{"Proxy hängt an", def, "127.0.0.1:1", []string{"1.1.1.1, 198.51.100.7"}, "", "198.51.100.7"},
		{"Proxy ersetzt", def, "127.0.0.1:1", []string{"198.51.100.7"}, "", "198.51.100.7"},
		{"mehrere Kopfzeilen", def, "172.17.0.1:1", []string{"6.6.6.6", "198.51.100.7, 10.0.0.2"}, "", "198.51.100.7"},
		{"X-Real-IP", def, "::1", nil, "198.51.100.7", "198.51.100.7"},
		{"nur interne Stationen", def, "127.0.0.1:1", []string{"192.168.1.20"}, "", "192.168.1.20"},
		{"CDN davor", cf, "127.0.0.1:1", []string{"6.6.6.6, 198.51.100.7, 203.0.113.5"}, "", "198.51.100.7"},
		{"Unsinn", def, "127.0.0.1:1", []string{"evil, 198.51.100.7"}, "", "198.51.100.7"},
		{"nur Unsinn", def, "127.0.0.1:1", []string{"evil"}, "", "127.0.0.1"},
		{"mit Port", def, "127.0.0.1:1", []string{"198.51.100.7:4711"}, "", "198.51.100.7"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		for _, x := range c.xff {
			req.Header.Add("X-Forwarded-For", x)
		}
		if c.real != "" {
			req.Header.Set("X-Real-IP", c.real)
		}
		if got := c.r.IP(req); got != c.want {
			t.Errorf("%s: %s, erwartet %s", c.name, got, c.want)
		}
	}
	if _, err := New(true, "kein-netz"); err == nil {
		t.Error("ungültige Liste angenommen")
	}
}
