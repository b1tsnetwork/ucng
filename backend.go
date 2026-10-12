// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backend ist der Speicher hinter dem Chat. Der komplette Zustand liegt im Arbeitsspeicher;
// das Backend schreibt Änderungen fort (Flush) und lädt beim Start alles (Load).
//
//   - files  – einzelne JSON-/JSONL-Dateien im Datenordner (Standard, ohne Abhängigkeiten)
//   - sqlite – eine SQLite-Datenbank (Build-Option -tags sqlite, reines Go über modernc.org/sqlite)
//   - mysql  – MySQL/MariaDB (Build-Option -tags mysql, github.com/go-sql-driver/mysql)
type Backend interface {
	Load(h *Hub, u *Users) error
	Flush(h *Hub, u *Users) error
	WriteAll(h *Hub, u *Users) error
	Close()
	// Lock/Unlock halten Schreibvorgänge an (für einen konsistenten Stand beim Sichern).
	Lock()
	Unlock()
	// Kind liefert "files", "sqlite" oder "mysql".
	Kind() string
}

func (s *Store) Lock()        { s.mu.Lock() }
func (s *Store) Unlock()      { s.mu.Unlock() }
func (s *Store) Kind() string { return "files" }

// sqlDrivers ordnet Speicherart → Name des database/sql-Treibers zu. Die Einträge kommen aus
// db_sqlite.go bzw. db_mysql.go und sind nur vorhanden, wenn mit dem passenden Build-Tag gebaut wurde.
var sqlDrivers = map[string]string{}

// OpenBackend öffnet den eingestellten Speicher.
func OpenBackend(kind, dataDir, dsn string) (Backend, error) {
	switch kind {
	case "", "files", "file":
		return NewStore(dataDir), nil
	case "sqlite", "mysql":
		driver, ok := sqlDrivers[kind]
		if !ok {
			return nil, fmt.Errorf("STORAGE=%s: Diese Version wurde ohne %s-Unterstützung gebaut. Bauen mit:\n"+
				"  go get %s\n  go build -tags %s .", kind, kind, sqlModule(kind), kind)
		}
		if dsn == "" {
			if kind == "mysql" {
				return nil, fmt.Errorf("STORAGE=mysql braucht DB_DSN, z. B. ucng:passwort@tcp(localhost:3306)/ucng")
			}
			dsn = filepath.Join(dataDir, "ucng.db")
			if _, err := os.Stat(filepath.Join(dataDir, "gochat.db")); err == nil {
				dsn = filepath.Join(dataDir, "gochat.db") // vorhandene Datenbank (vor der Umbenennung)
			}
		}
		return OpenSQL(kind, driver, dsn, dataDir)
	}
	return nil, fmt.Errorf("unbekannte Speicherart %q (files, sqlite oder mysql)", kind)
}

func sqlModule(kind string) string {
	if kind == "mysql" {
		return "github.com/go-sql-driver/mysql"
	}
	return "modernc.org/sqlite"
}

// redactDSN blendet ein Passwort in einer MySQL-DSN aus (für Logs und Admin-Anzeige).
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	cred := dsn[:at]
	if i := strings.Index(cred, ":"); i >= 0 {
		return cred[:i] + ":***" + dsn[at:]
	}
	return dsn
}
