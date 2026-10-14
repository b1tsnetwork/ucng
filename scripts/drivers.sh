#!/bin/sh
# SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Stellt die Datenbank-Treiber für die angegebenen Build-Tags bereit:
#   sh scripts/drivers.sh sqlite mysql
#
# Stehen die Treiber in go.mod (festgeschrieben, Prüfsummen in go.sum), wird genau diese
# Version verwendet – reproduzierbar und gegen die Prüfsummen-Datenbank geprüft. Sonst wird
# die neueste Version geholt und eine Warnung ausgegeben; festschreiben lässt sich das mit
# dem Workflow „Treiber festschreiben“ (.github/workflows/deps.yml).
set -eu
for t in "$@"; do
  case "$t" in
    sqlite) mod=modernc.org/sqlite ;;
    mysql) mod=github.com/go-sql-driver/mysql ;;
    *) continue ;;
  esac
  if grep -Eq "^[[:space:]]*(require[[:space:]]+)?$mod[[:space:]]+v" go.mod; then
    echo "Treiber $mod: Version aus go.mod"
    go mod download "$mod"
  else
    echo "WARNUNG: $mod ist nicht in go.mod festgeschrieben – es wird die neueste Version verwendet." >&2
    echo "         Workflow „Treiber festschreiben“ ausführen, damit Builds reproduzierbar sind." >&2
    go get "$mod"
  fi
done
