# Mitmachen

Danke, dass du UCNG verbessern möchtest!

## Vor dem ersten Pull Request

- **Lizenz:** UCNG steht unter der [AGPL-3.0-or-later](LICENSE). Beiträge nehmen wir unter dem [Contributor License Agreement](CLA.md) an – der CLA-Assistent fragt beim ersten Pull Request danach; unterschrieben wird mit einem Kommentar.
- **Größere Änderungen** (neue Funktionen, Protokoll, Speicherformat) bitte zuerst in einem Issue besprechen.

## Prüfen vor dem Einreichen

```bash
gofmt -l .                       # muss leer sein
go vet ./...
go test -race ./...
```

- Neue Quelldateien tragen oben die SPDX-Kopfzeilen (ein Test prüft das):
  `// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)` und
  `// SPDX-License-Identifier: AGPL-3.0-or-later`
- **Texte** gehören in die Sprachdateien (`web/i18n/de.json` = Ausgangssprache, `en.json`), nicht direkt in den Code – siehe „Sprachen und Übersetzungen“ im README. Danach `UPDATE_I18N=1 go test -run TestWidgetFallback .` und die Kopien für den Beacon aktualisieren (die Tests melden Abweichungen).
- **Protokoll und Föderation:** Änderungen müssen mit der vorherigen Version zusammenarbeiten (siehe „Name und frühere Bezeichnungen“ im README).

## Übersetzungen

Neue Sprachen am einfachsten über das Übersetzungspaket (Admin → Sprachen bzw. Beacon → Sprachen) – ausfüllen, im Chat prüfen und als Datei unter `web/i18n/<code>.json` einreichen.
