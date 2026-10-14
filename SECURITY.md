# Sicherheit / Security

## Schwachstelle melden

Bitte melde Sicherheitslücken **nicht** als öffentliches Issue, sondern vertraulich:

- über **GitHub → Security → „Report a vulnerability“** (Private Vulnerability Reporting) oder
- an die Kontaktadresse in [`NOTICE`](NOTICE).

Hilfreich sind: betroffene Version (steht im Admin-Bereich unten und im Log beim Start), Betriebsart (`simple`/`hosted`), Schritte zum Nachvollziehen und die mögliche Auswirkung. Wir bestätigen den Eingang in der Regel innerhalb weniger Tage und stimmen die Veröffentlichung mit dir ab.

## Unterstützte Versionen

Sicherheitskorrekturen gibt es für die jeweils neueste Version. Betreiber:innen sollten zeitnah aktualisieren (Docker: `docker compose pull && docker compose up -d`; erst die Chats, dann den Beacon).

## Betrieb

Hinweise zu Reverse-Proxy, Client-IP, Grenzen und Kopfzeilen stehen in der README unter „Hinweise für den Produktivbetrieb“.

---

**English:** Please report vulnerabilities privately via GitHub's “Report a vulnerability” or the contact address in `NOTICE` – not as a public issue. Security fixes are provided for the latest release.
