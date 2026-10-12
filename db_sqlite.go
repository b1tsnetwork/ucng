// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build sqlite

package main

// SQLite ohne cgo (reines Go). Bauen mit:
//
//	go get modernc.org/sqlite
//	go build -tags sqlite .
import _ "modernc.org/sqlite"

func init() { sqlDrivers["sqlite"] = "sqlite" }
