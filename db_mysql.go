// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build mysql

package main

// MySQL/MariaDB (reines Go). Bauen mit:
//
//	go get github.com/go-sql-driver/mysql
//	go build -tags mysql .
import _ "github.com/go-sql-driver/mysql"

func init() { sqlDrivers["mysql"] = "mysql" }
