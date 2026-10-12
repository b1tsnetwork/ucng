// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Jede Quelldatei trägt die SPDX-Kopfzeilen (Lizenz auch bei herauskopierten Dateien eindeutig).
func TestSPDXHeaders(t *testing.T) {
	filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "data", "data-backups", "beacon-data", "flags", "node_modules", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(p) {
		case ".go", ".js", ".css", ".html":
		default:
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		head := string(b[:min(len(b), 400)])
		if !strings.Contains(head, "SPDX-License-Identifier: AGPL-3.0-or-later") {
			t.Errorf("%s: SPDX-Kopfzeilen fehlen (siehe CONTRIBUTING.md)", p)
		}
		return nil
	})
}

// LICENSE ist der unveränderte Text der GNU AGPL v3 (Prüfsumme der FSF-Datei agpl-3.0.txt).
func TestLicenseText(t *testing.T) {
	b, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != "8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef" {
		t.Fatalf("LICENSE wurde verändert: %s", got)
	}
}

// Die Quellcode-Adresse (AGPL § 13) erscheint in der Oberfläche und für andere Instanzen.
func TestSourceURLPublished(t *testing.T) {
	ts, _ := testServerWith(t, func(c *Config) {
		c.PublicURL = "https://chat.example.org"
		c.SourceURL = cleanSourceURL("https://git.example.org/ich/ucng")
	})
	st := jmap(t, do(t, "GET", ts.URL+"/api/settings", "", ""))
	about, _ := st["about"].(map[string]any)
	if about["source"] != "https://git.example.org/ich/ucng" || about["license"] != "AGPL-3.0-or-later" || about["name"] != "UCNG" {
		t.Fatalf("about: %v", about)
	}
	wk := jmap(t, do(t, "GET", ts.URL+"/.well-known/ucng", "", ""))
	if wk["source"] != "https://git.example.org/ich/ucng" {
		t.Fatalf("well-known: %v", wk)
	}
	if cleanSourceURL("javascript:alert(1)") != "" || cleanSourceURL("ftp://x") != "" {
		t.Fatal("ungültige Quellcode-Adressen angenommen")
	}
}
