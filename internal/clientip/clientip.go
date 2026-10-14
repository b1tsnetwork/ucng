// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clientip ermittelt die Adresse der anfragenden Person – auch hinter Reverse-Proxys,
// ohne dass sich jemand per gefälschtem X-Forwarded-For eine andere Adresse geben kann.
//
// Regeln:
//   - Ohne Proxy-Vertrauen zählt nur die Adresse der TCP-Verbindung.
//   - Mit Proxy-Vertrauen werden Weiterleitungs-Kopfzeilen nur beachtet, wenn die Verbindung von
//     einem vertrauenswürdigen Proxy kommt. X-Forwarded-For wird von rechts gelesen: Die erste
//     Adresse, die kein vertrauenswürdiger Proxy ist, ist die Person. Was ein Client selbst links
//     voranstellt, wird so nie verwendet.
//   - Vertrauenswürdig sind standardmäßig Loopback und private Netze (typisch für einen Proxy auf
//     demselben Server oder im Docker-Netz), sonst die Liste aus TRUSTED_PROXIES.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Standard: Loopback, private Netze (RFC 1918, ULA), Link-Local und Carrier-NAT.
var defaultTrusted = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"fc00::/7", "169.254.0.0/16", "fe80::/10", "100.64.0.0/10"}

type Resolver struct {
	trust bool
	nets  []*net.IPNet
}

// New: trust = Weiterleitungs-Kopfzeilen beachten; list = vertrauenswürdige Proxys (IP-Adressen
// oder Netze, kommagetrennt; leer = Standard).
func New(trust bool, list string) (*Resolver, error) {
	r := &Resolver{trust: trust}
	entries := defaultTrusted
	if strings.TrimSpace(list) != "" {
		entries = strings.Split(list, ",")
	}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, fmt.Errorf("TRUSTED_PROXIES: %q ist keine IP-Adresse", e)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			e = fmt.Sprintf("%s/%d", e, bits)
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: %q ist kein gültiges Netz", e)
		}
		r.nets = append(r.nets, n)
	}
	return r, nil
}

func (r *Resolver) trusted(ip net.IP) bool {
	for _, n := range r.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func parse(s string) net.IP {
	s = strings.TrimSpace(s)
	if h, _, err := net.SplitHostPort(s); err == nil { // "1.2.3.4:5678" oder "[::1]:80"
		s = h
	}
	return net.ParseIP(strings.Trim(s, "[]"))
}

// IP liefert die Adresse der anfragenden Person.
func (r *Resolver) IP(req *http.Request) string {
	remote := remoteIP(req)
	if r == nil || !r.trust {
		return remote
	}
	rip := net.ParseIP(remote)
	if rip == nil || !r.trusted(rip) {
		return remote // direkt verbunden – Kopfzeilen sind dann frei erfunden
	}
	var hops []string
	for _, h := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	if len(hops) == 0 {
		if ip := parse(req.Header.Get("X-Real-Ip")); ip != nil {
			return ip.String()
		}
		return remote
	}
	first := ""
	for i := len(hops) - 1; i >= 0; i-- {
		ip := parse(hops[i])
		if ip == nil {
			break // unlesbar: nicht weiter nach links vertrauen
		}
		if !r.trusted(ip) {
			return ip.String()
		}
		first = ip.String()
	}
	if first != "" {
		return first // alle Stationen sind eigene Proxys (z. B. Test im lokalen Netz)
	}
	return remote
}
