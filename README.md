<p align="center"><img src="assets/logo-wordmark.svg" alt="UCNG – Unified Chat Network Gateway" width="470"></p>

# UCNG – Unified Chat Network Gateway

Echtzeit-Chat in Go – mit vollständiger Weboberfläche und einem einbettbaren Widget (JS + CSS) für beliebige andere Webseiten. Instanzen verbinden sich über Föderation und Beacons zu einem Netz („Unified Chat Network“).

- **Zwei Betriebsarten in einem Programm** (`MODE=simple|hosted`): *simple* für Intranet und private Nutzung (Namen ohne Konto), *hosted* für öffentlich gehostete Chats mit **Nutzerkonten, Zwei-Faktor-Anmeldung, Ende-zu-Ende-verschlüsselten Direktnachrichten und Föderation** (`name@andere-instanz`) – siehe [Modus „hosted“](#modus-hosted)
- **Speicher wählbar:** Dateien (Standard), **SQLite** oder **MySQL/MariaDB** – beide Treiber in reinem Go, ohne cgo
- **Keine externen Abhängigkeiten** im Standard-Build – nur die Go-Standardbibliothek (Go ≥ 1.24); Datenbank-Treiber nur per Build-Option
- **Eine einzige Binärdatei** – HTML, CSS und JS sind per `go:embed` eingebaut
- **Echtzeit per Server-Sent Events** (funktioniert durch Proxys, automatischer Reconnect)
- Kanäle, Online-Anzeige, Verlauf – **alles wird dauerhaft gespeichert** (Kanäle, Einstellungen, Nachrichten, Namen)
- **Namensreservierung ohne Registrierung** – niemand kann unter deinem Namen schreiben
- **Community-Funktionen:** Threads, Umfragen, angepinnte Nachrichten, Bilder hochladen, Direktnachrichten, Profilbilder und Status, `@Rolle`-Erwähnungen, Melden-Knopf, geplante Ankündigungen
- **Rollen mit Rechten** (Links, kein Langsam-Modus, anpinnen, im Chat löschen/stummschalten …) und **private Kanäle** bzw. Kanäle, in denen nur bestimmte Rollen schreiben
- **Chatten wie gewohnt:** Antworten mit Zitat, Reaktionen, eigene Nachrichten bearbeiten (15 Min.) und löschen, `@Erwähnungen` mit Vervollständigung, **fett**/*kursiv*/`Code`, Online-Liste, Suche, Ungelesen-Zähler, Ton und Browser-Benachrichtigungen
- **Admin-Bereich** unter `/admin` mit **eigenen Konten für Moderatoren** und **Protokoll** aller Aktionen: Übersicht mit Statistik, Moderation mit Suche, Automoderation, Personen sperren/stummschalten, Kanäle, Farben, Widgets, Bots, Backups
- **Schutz vor Missbrauch:** Stummschalten und Ausschließen (zeitlich oder dauerhaft, optional mit Verbindung), geschützte Namen („Admin“, „Support“ …), Team-Abzeichen und frei definierbare Rollen-Abzeichen, Langsam-Modus und Link-Regeln pro Kanal, Wiederholungs-Bremse, Moderations-Bots
- **Datenschutz:** Aufbewahrungsfrist mit automatischem Löschen, Nachrichten oder alle Daten einer Person endgültig löschen, Links zu Datenschutz und Impressum, IP-Adressen werden nie gespeichert
- **Profil mitnehmen:** Name per Datei oder Code auf andere Geräte und ins Widget übertragen
- **Bot-Schnittstelle:** Bots per Live-Verbindung oder Webhook andocken – mit Beispiel-Bots
- **Eingebauter Befehls-Bot:** `!hilfe`, `!würfel`, `!wähle`, `!online` … und eigene Befehle – alles im Admin-Bereich einstellbar
- **Eigene Emotes** (`:name:`) und **GIFs** aus einer eigenen Bibliothek oder per GIPHY/Tenor – fest auf 32 × 32 / 64 × 64 px bzw. eine feste GIF-Größe skaliert, Animationen bleiben erhalten
- **Automoderation** mit Wildcards (`arsch*`, `*loch`, `arsc*och`) und Erkennung von Tarnungen (0=o, 3=e, 4=a …)
- **Widgets** zentral verwalten: Begrüßung, Öffnungszeiten, erlaubte Domains je Widget
- **Tägliche Backups** mit Download im Admin-Bereich
- Rate-Limiting pro IP, Eingabevalidierung, CORS nur für das Widget
- **Mehrsprachig:** Deutsch und Englisch eingebaut, Sprache in Chat, Widget, Verwaltung und Beacon umschaltbar; Admins bearbeiten Texte direkt im Admin-Bereich und legen eigene Sprachen an – ihre Änderungen überstehen jedes Update (siehe [Sprachen und Übersetzungen](#sprachen-und-übersetzungen))
- Hell/Dunkel-Modus, mobil nutzbar

## Schnellstart

```bash
ADMIN_PASSWORD='ein-langes-passwort' go run .
# → http://localhost:8080           Weboberfläche
# → http://localhost:8080/admin     Verwaltung
# → http://localhost:8080/demo.html Widget-Demo auf einer "fremden" Seite
```

Ohne `ADMIN_PASSWORD` erzeugt der Server bei jedem Start ein zufälliges Passwort und schreibt es ins Log.

Alle Daten landen automatisch im Ordner `data/` (wird angelegt) – siehe [Speicherung](#speicherung).

Bauen und Tests:

```bash
go build -o ucng .
go test ./...
```

Mit Docker:

```bash
docker build -t ucng .
docker run -p 8080:8080 -v ucng-data:/data -e ADMIN_PASSWORD='ein-langes-passwort' ucng
```

Hosting-Version mit Nutzerkonten, 2FA, verschlüsselten Direktnachrichten und SQLite:

```bash
go get modernc.org/sqlite && CGO_ENABLED=0 go build -tags sqlite -o ucng .
MODE=hosted STORAGE=sqlite PUBLIC_URL=https://chat.example.org ADMIN_PASSWORD='…' ./ucng
```

## Konfiguration

Jede Option geht als Flag oder als Umgebungsvariable:

| Flag           | Umgebungsvariable | Standard | Bedeutung |
|----------------|-------------------|----------|-----------|
| `-addr`        | `ADDR`            | `:8080`  | Listen-Adresse |
| `-admin-password` | `ADMIN_PASSWORD` | zufällig | Passwort für `/admin` (mind. 10 Zeichen empfohlen) |
| `-origins`     | `ALLOWED_ORIGINS` | leer     | Zusätzlich erlaubte Webseiten für das Widget, kommagetrennt (`https://shop.de,https://blog.de`); `*` = alle. Leer = es gilt die Einstellung unter **Widgets** im Admin-Bereich |
| `-data`        | `DATA_DIR`        | `data`   | Ordner für alle Daten; `none` = nichts speichern (nur zum Testen) |
| `-max-rooms`   | `MAX_ROOMS`       | `0`      | Maximale Anzahl Kanäle; `0` = unbegrenzt |
| `-rate`        | `RATE_LIMIT`      | `10`     | Nachrichten pro 10 Sekunden und IP |
| `-trust-proxy` | `TRUST_PROXY=1`   | aus      | Client-IP aus `X-Forwarded-For` lesen (nur hinter eigenem Reverse-Proxy!) |
| `-name-ttl`    | `NAME_TTL_DAYS`   | `30`     | Tage ohne Aktivität, nach denen ein Name wieder frei wird (`0` = nie) |
| `-max-users`   | `MAX_USERS`       | `10000`  | Maximale Anzahl reservierter Namen |
| `-claim-rate`  | `CLAIM_RATE`      | `5`      | Namensreservierungen pro Minute und IP |
| `-bot-rate`    | `BOT_RATE`        | `30`     | Nachrichten pro 10 Sekunden und Bot |
| `-backup-dir`  | `BACKUP_DIR`      | `<data>-backups` | Ordner für die täglichen Backups |
| `-backup-keep` | `BACKUP_KEEP`     | `7`      | Anzahl aufbewahrter Backups; `0` = keine automatischen Backups |
| `-mode`        | `MODE`            | `simple` | `simple` (Namen ohne Konto) oder `hosted` (Nutzerkonten, 2FA, E2E, Föderation) |
| `-storage`     | `STORAGE`         | `files`  | `files`, `sqlite` oder `mysql` (die beiden letzten nur mit Build-Tag, siehe unten) |
| `-db`          | `DB_DSN`          | `<data>/ucng.db` (eine vorhandene `gochat.db` wird weiter genutzt) | Datenbank: bei SQLite ein Dateipfad, bei MySQL z. B. `ucng:passwort@tcp(db:3306)/ucng` |
| `-public-url`  | `PUBLIC_URL`      | leer     | Öffentliche Adresse, z. B. `https://chat.example.org` – für Links in E-Mails und die Föderation |
| `-source-url`  | `SOURCE_URL`      | bei Releases das GitHub-Repository | Adresse des Quellcodes dieser Fassung – Pflicht nach der AGPL, wenn du eine **geänderte** Fassung öffentlich betreibst; erscheint im Chat, in der Verwaltung, in `/.well-known/ucng` und im Beacon |
| `-fed-insecure`| `FED_INSECURE=1`  | aus      | Föderation auch über `http://` und zu Adressen im lokalen Netz (nur zum Testen!) |

Beispiel für den Produktivbetrieb:

```bash
ADMIN_PASSWORD='…' ./ucng -addr :8080 -data /var/lib/ucng -backup-dir /var/backups/ucng -trust-proxy
```

## Speicherung

Alles wird dauerhaft im Ordner `data/` gespeichert – jeder Bereich in seiner eigenen Datei:

```
data/
  config.json                  Einstellungen: Titel, Farben, Datenschutz, Zeitzone, Öffnungszeiten
  automod.json                 Automod: an/aus und alle Muster (inkl. Kommentarzeilen)
  users.json                   reservierte Namen (nur Hashes der Tokens), Team-Abzeichen, zugewiesene Rollen
  roles.json                   Rollen (Name, Farbe, Reihenfolge, Rechte)
  groups.json                  Kanal-Gruppen (Gruppe und Position stehen in rooms/<kanal>.json)
  reports.json                 Meldungen (offen und die letzten 500 erledigten)
  schedules.json               geplante Ankündigungen
  filter.json                  Wortfilter (ersetzen statt ausblenden)
  bots.json                    Bots (API-Schlüssel nur als Hash, Webhook-Geheimnis im Klartext)
  accounts.json                Konten für Admins und Moderatoren (Passwörter als PBKDF2-Hash)
  moderation.json              Sperren, geschützte Namen, geheimer Schlüssel für Verbindungs-Hashes
  widgets.json                 gespeicherte Widgets
  emotes.json                  Emotes (Name, Datei, Größe)
  gifs.json                    GIF-Bibliothek (Titel, Schlagwörter, Größe)
  media.json                   Einstellungen für Emotes, GIFs und Reaktionen (inkl. GIPHY-/Tenor-Schlüssel)
  commands.json                Befehls-Bot: an/aus, Name, Zeichen, alle Befehle
  media/
    emotes/                    skalierte Emotes (z. B. smile-32.png)
    gifs/                      skalierte GIFs der Bibliothek
    originals/                 Originale – zum späteren Neu-Skalieren
    web/                       Zwischenspeicher für gesendete GIPHY-/Tenor-GIFs
    uploads/                   hochgeladene Bilder (verkleinert)
    avatars/                   Profilbilder (128 × 128)
  audit.jsonl                  Protokoll, ein Eintrag pro Zeile
  rooms/
    lobby.json                 Kanal: Name, Beschreibung, Erstellungsdatum
    lobby.messages.jsonl       alle Nachrichten des Kanals
    support.json
    support.messages.jsonl
    …
```

- **Kein Limit:** Der Verlauf jedes Kanals wird vollständig aufbewahrt, ohne Obergrenze. Auch die Zahl der Kanäle ist nicht begrenzt (optional mit `MAX_ROOMS` einschränkbar, z. B. wenn Besucher selbst Kanäle anlegen dürfen).
- **Nachrichtendateien** (`.jsonl`, „JSON Lines“) enthalten eine JSON-Nachricht pro Zeile. Neue Nachrichten werden nur angehängt, die Datei wird also nicht jedes Mal neu geschrieben – das bleibt auch bei Hunderttausenden Nachrichten schnell (getestet: 200.000 Nachrichten laden in unter einer Sekunde). Wird eine Nachricht moderiert oder benennt sich ihr Autor um, wird die neue Fassung angehängt; beim Laden gilt die letzte. Überholte Zeilen räumt der Server beim Start automatisch auf.
- **Alle anderen Dateien** sind normales, eingerücktes JSON und werden bei Änderungen in einem Schritt ersetzt.
- **Sofort:** Änderungen werden nach spätestens etwa einer halben Sekunde geschrieben und auf die Platte gezwungen (fsync); beim normalen Beenden wird ein letztes Mal gespeichert.
- **Absturzsicher:** Bei Stromausfall oder `kill -9` fehlt höchstens die letzte halbe Sekunde. Eine halb geschriebene letzte Zeile wird beim nächsten Start erkannt und entfernt. Ist eine Datei darüber hinaus beschädigt, startet der Server nicht und überschreibt nichts, bis du sie geprüft oder aus einem Backup zurückgespielt hast.
- **Umbenennen und Löschen von Kanälen** benennt bzw. löscht die zugehörigen Dateien mit.
- **Umstieg von älteren Versionen:** Liegt im Datenordner noch eine `gochat.json` (oder `chat.json`) im alten Format, wird sie beim ersten Start automatisch in die einzelnen Dateien übernommen und danach in `gochat.json.migrated` umbenannt.
- **Backups:** Täglich um 3 Uhr (in der eingestellten Zeitzone) sichert der Server den kompletten Datenordner als `.tar.gz` nach `BACKUP_DIR` und behält die letzten `BACKUP_KEEP`. Unter **Backups** im Admin-Bereich siehst du alle Sicherungen, legst sofort eine an und lädst sie herunter. Wiederherstellen: Server stoppen, Datenordner leeren, Archiv darin entpacken (`tar -xzf ucng-….tar.gz -C data`), Server starten.
- **Docker:** Der Ordner liegt unter `/data`; mit `-v ucng-data:/data` bleibt er über Container-Neustarts und Updates hinweg erhalten. Die Backups liegen im Container unter `/data/backups` (werden beim Sichern selbst übersprungen); für eine Kopie außerhalb des Volumes `-v /pfad/zu/backups:/data/backups` einhängen.
- **Nicht gespeichert:** Admin-Sitzungen – nach einem Neustart meldest du dich im Admin-Bereich neu an.

### Datenbank: SQLite oder MySQL

Statt einzelner Dateien kann UCNG in **SQLite** (eine Datei, kein Server nötig) oder **MySQL/MariaDB** speichern. Beide Treiber sind reines Go – es braucht **kein cgo** und keinen C-Compiler. Sie sind nur enthalten, wenn du mit dem passenden Build-Tag baust; der Standard-Build bleibt ohne fremde Pakete:

```bash
go get modernc.org/sqlite                 # SQLite (reines Go)
go get github.com/go-sql-driver/mysql     # MySQL/MariaDB (reines Go)
CGO_ENABLED=0 go build -tags "sqlite mysql" -o ucng .

STORAGE=sqlite ./ucng                                        # → data/ucng.db
STORAGE=mysql DB_DSN='ucng:passwort@tcp(db:3306)/ucng' ./ucng
```

Mit Docker: `docker build --build-arg TAGS="sqlite mysql" -t ucng .`

- **Tabellen** legt UCNG selbst an: `gochat_docs` (Einstellungen, Namen, Konten …), `gochat_rooms`, `gochat_messages` (eine Zeile je Nachricht) und `gochat_audit`. Jeder Speichervorgang läuft in einer Transaktion. MySQL-Tabellen nutzen `utf8mb4` (Emojis).
- **Umstieg:** Ist die Datenbank beim ersten Start leer und liegen im Datenordner noch Daten im Dateiformat, werden sie automatisch übernommen. Die Dateien bleiben unverändert liegen (als Rückfallebene), werden aber nicht mehr benutzt – außer `media/` (Bilder, Emotes, GIFs liegen weiterhin im Datenordner).
- **Backups** schreiben im Datenbank-Betrieb den aktuellen Stand im **Dateiformat** plus `media/` ins Archiv. Ein solches Archiv startest du direkt mit `STORAGE=files` – oder du entpackst es in einen Datenordner und startest mit leerer Datenbank, dann wird es übernommen. Für MySQL empfiehlt sich zusätzlich das übliche `mysqldump`.
- **Grenze:** Wie im Dateibetrieb hält der Server den kompletten Stand im Arbeitsspeicher; die Datenbank ist der dauerhafte Speicher, nicht die Abfragequelle. Mehrere UCNG-Prozesse auf derselben Datenbank sind deshalb **nicht** möglich.
- **Hinweis zur Entwicklung:** Die SQL-Schicht ist gegen echtes SQLite getestet (`go test -tags sqlite ./...` führt die Datenbank-Tests aus, ohne Tag werden sie übersprungen). Der MySQL-Dialekt (`ON DUPLICATE KEY UPDATE`, `LONGTEXT`, `VARCHAR(191)`-Schlüssel) ist kompiliert, aber im Projekt nicht gegen einen laufenden MySQL-Server getestet – bitte vor dem Produktivbetrieb einmal ausprobieren.

**Laden im Browser:** Chat, Widget und Moderation zeigen zuerst die neuesten 100 Nachrichten. Beim Hochscrollen werden ältere automatisch nachgeladen (in der Moderation per Knopf am Listenende), ohne dass die Ansicht springt.

## Modus „hosted“

Für öffentlich erreichbare Chats: `MODE=hosted PUBLIC_URL=https://chat.example.org ./ucng`. Alles aus dem Modus „simple“ bleibt erhalten; dazu kommen:

### Nutzerkonten

- **Registrieren und Anmelden** mit Name (zugleich die Adresse für die Föderation: 2–32 Zeichen `a–z 0–9 . _ -`), optional E-Mail-Adresse, Passwort. Anmelden geht mit Name oder E-Mail-Adresse.
- **Das Passwort verlässt den Browser nie.** Der Browser leitet daraus per PBKDF2-SHA-256 (300.000 Runden, zufälliges Salz) 512 Bit ab: Die erste Hälfte geht als Anmeldeschlüssel an den Server (dort noch einmal gesalzen gehasht), die zweite verschlüsselt den privaten Schlüssel für die Direktnachrichten und bleibt im Browser.
- **Zwei-Faktor-Anmeldung (TOTP)** mit jeder Authenticator-App. Den QR-Code erzeugt der Server selbst (ohne externe Dienste). Ist sie aktiv, verlangen auch Passwort- und E-Mail-Wechsel, neue Wiederherstellungscodes, eine neue 2FA-Einrichtung und das Löschen des Kontos einen aktuellen Code.
- **Wiederherstellungscodes:** Bei der Registrierung und beim Einschalten von 2FA gibt es 10 Einmal-Codes. Sie ersetzen den 2FA-Code und erlauben ein neues Passwort, wenn kein E-Mail-Versand eingerichtet ist.
- **E-Mail per SMTP** (Admin-Bereich → **Hosting**): Bestätigung der Adresse bei der Registrierung (abschaltbar) und „Passwort vergessen“ per Link (1 Stunde gültig). Mit Test-E-Mail-Knopf. Eine geänderte Adresse gilt erst nach Bestätigung – bis dahin bleibt die alte aktiv.
- **Angemeldete Geräte** sehen und einzeln oder alle anderen abmelden; Konten laufen nicht ab, ungenutzte Sitzungen nach 30 Tagen.
- **Gäste** (Admin → Hosting): *nichts sehen* (nur für Mitglieder), *mitlesen* oder *mit Gastnamen schreiben* (wie im Modus „simple“). Bei *nichts sehen* sind Kanäle, Verlauf, Suche, Online-Liste und Live-Stream nur mit Konto erreichbar; Bild-Dateien unter `/media/` haben zufällige Namen, sind aber – damit sie im Browser ohne Anmeldekopf laden – nicht geschützt.
- **Registrierung geschlossen?** Einladungslinks (7 Tage, einmalig) im Admin-Bereich.
- **Admin:** Konten sperren/entsperren, 2FA zurücksetzen, E-Mail als bestätigt markieren, Reset-Link senden, überall abmelden (Personen → Verwalten). Das dürfen nur Admins; Moderatoren sehen Konten, aber keine E-Mail-Adressen. Gesperrte oder abgemeldete Sitzungen verlieren sofort auch ihre Live-Verbindungen.
- **Widget:** Dürfen Gäste nicht schreiben, zeigt das eingebettete Widget eine Anmeldung (Name + Passwort + ggf. Code). Das braucht eine Seite mit `https://`.

**Wichtig:** Wer das Passwort zurücksetzt (per E-Mail-Link oder Wiederherstellungscode), bekommt ein neues Schlüsselpaar – ältere verschlüsselte Direktnachrichten sind danach nicht mehr lesbar. Ein normaler Passwortwechsel (mit altem Passwort) behält den Schlüssel.

### Ende-zu-Ende-verschlüsselte Direktnachrichten

Direktnachrichten zwischen zwei Konten sind automatisch verschlüsselt:

- Jedes Konto hat ein **ECDH-Schlüsselpaar (P-256)**, erzeugt im Browser (WebCrypto). Auf dem Server liegt der private Schlüssel nur verschlüsselt (AES-GCM mit dem aus dem Passwort abgeleiteten Schlüssel). Im Browser wird er nicht exportierbar in IndexedDB abgelegt – nach einem Neuladen muss man das Passwort nicht erneut eingeben. Auf einem neuen Gerät entsperrt die Anmeldung ihn.
- Für jede Unterhaltung leiten beide Seiten per **ECDH + HKDF-SHA-256** denselben **AES-256-GCM**-Schlüssel ab. Absender- und Empfängerschlüssel sind mit authentifiziert. Der Server speichert und verteilt nur Geheimtext; Moderation und Backups sehen „🔒 Verschlüsselte Nachricht“.
- Der Browser prüft bei jeder Nachricht, ob sie mit dem Schlüssel der angezeigten Person geschrieben wurde. Passt er nicht (älterer Schlüssel nach einem Passwort-Zurücksetzen – oder eine untergeschobene Nachricht), steht „⚠ Schlüssel passt nicht“ daran.
- **Sicherheitsnummer** (60 Ziffern) im Kopf der Unterhaltung: vergleicht sie persönlich oder am Telefon, dann kann auch der Server nicht unbemerkt dazwischen sitzen. Ändert sich der Schlüssel des Gegenübers, warnt der Browser.
- In verschlüsselten Unterhaltungen gibt es **nur Text** (inkl. Emotes, Formatierung, Antworten, Bearbeiten, Löschen, Reaktionen) – keine Bilder, GIFs, Umfragen oder Threads, weil diese sonst unverschlüsselt auf dem Server lägen. Automod und Wortfilter greifen nicht.
- **Melden** geht trotzdem: Der Browser der meldenden Person schickt den entschlüsselten Text mit; der Admin-Bereich zeigt ihn als „laut meldender Person“ (vom Server nicht überprüfbar).
- Bewusste Grenzen: kein *Forward Secrecy* (ein später bekannt gewordenes Passwort öffnet auch ältere Nachrichten), Metadaten (wer wann mit wem) sieht der Server. Gäste ohne Konto schreiben unverschlüsselte Direktnachrichten.

### Föderation (name@instanz)

Mitglieder können Mitgliedern auf **anderen UCNG-Instanzen** verschlüsselte Direktnachrichten schreiben: in der Seitenleiste bei „Direktnachrichten“ auf **+** und `name@chat.example.org` eingeben. Kanäle bleiben je Instanz.

- Einschalten unter Admin → **Hosting** (braucht `PUBLIC_URL` mit `https://`). Optional nur bestimmte Instanzen erlauben (`chat.example.org`, `*.example.net`) oder einzelne sperren.
- Jede Instanz hat einen **Ed25519-Serverschlüssel** (`/.well-known/ucng`). Nachrichten gehen als **signierter** POST an `/api/fed/inbox` der anderen Instanz (Absender-Domain, Zeit, Ziel und Hash des Inhalts sind signiert; Wiederholungen und veraltete Anfragen werden abgelehnt).
- **TOFU:** Beim ersten Kontakt wird der Serverschlüssel der anderen Instanz gemerkt. Ändert er sich, nimmt UCNG nichts mehr von ihr an, bis ein Admin den alten Schlüssel unter **Hosting → Bekannte Instanzen** vergisst.
- Übertragen werden neue, bearbeitete und gelöschte Nachrichten. Nicht erreichbare Instanzen werden bis zu 24 Stunden lang erneut versucht; der Status steht an der Nachricht („wird zugestellt“ / „nicht zugestellt“). Antworten mit Zitat und Reaktionen gibt es instanzübergreifend nicht.
- Jede:r kann in „Konto & Sicherheit“ Nachrichten von anderen Instanzen abschalten.
- Ohne `FED_INSECURE=1` verbindet sich UCNG nie zu Adressen im lokalen Netz (Schutz davor, über Föderations-Adressen interne Dienste abzutasten), und Instanzen mit Port in der Adresse (`chat.example.org:8443`) sind gesperrt. Erlaubt-/Sperrlisten gelten unabhängig von Port und abschließendem Punkt.

### Was die Konten-Daten enthalten

`users.json` (bzw. die Datenbank) enthält je Konto: Name, E-Mail, Hash des Anmeldeschlüssels, Salz, TOTP-Geheimnis, Hashes der Wiederherstellungscodes, öffentlichen Schlüssel, verschlüsselten privaten Schlüssel und Hashes der Sitzungs-Tokens. `federation.json` enthält den privaten Serverschlüssel der Instanz und die gemerkten Schlüssel anderer Instanzen. **Backups enthalten beides – behandle sie entsprechend vertraulich.**

## Admin-Bereich

Unter `/admin` meldest du dich an – als **Haupt-Admin** (Anmeldename leer lassen oder `admin`, Passwort aus `ADMIN_PASSWORD`) oder mit einem **eigenen Konto**. Sitzungen gelten 12 Stunden; nach einem Neustart meldest du dich neu an. Fünf Fehlversuche pro Minute und IP sind erlaubt.

### Rollen

| Bereich | Moderator | Admin |
|---------|:---------:|:-----:|
| Übersicht (Statistik), Moderation, Automod, Wortfilter, Meldungen, Personen sperren, Protokoll | ✓ | ✓ |
| Endgültig löschen, Team-Abzeichen, Rollen, geschützte Namen ändern | | ✓ |
| Kanäle, Allgemein, Emotes & GIFs, Befehle, Ankündigungen, Widgets, Bots, Team-Zugänge, Hosting, Beacon, Vorlagen, Backups | | ✓ |

Zugänge für das Team legst du unter **Team-Zugänge** an (Passwort mindestens 10 Zeichen; „Sicheres Passwort erzeugen“ hilft). Ein Konto lässt sich deaktivieren – offene Sitzungen enden dann sofort, ebenso nach Passwort- oder Rollenwechsel. Passwörter werden mit PBKDF2-HMAC-SHA256 (210.000 Runden, eigenes Salz) gespeichert. Der Haupt-Admin wird nur über `ADMIN_PASSWORD` verwaltet und kann nicht gelöscht werden.

### Bereiche

- **Übersicht:** Kennzahlen (Nachrichten, aktive Personen, online, offene Prüfungen, Sperren, Namen) und ein Diagramm der letzten 30 Tage – umschaltbar zwischen Nachrichten, schreibenden Personen und Automod-Treffern – sowie die aktivsten Kanäle.
- **Moderation:** Live-Ansicht eines Kanals, neueste Nachrichten oben. „Löschen“ (zweimal klicken) ersetzt die Nachricht bei allen durch „Nachricht wurde von einem Moderator gelöscht“; in der Moderation bleibt der Originaltext sichtbar, mit wer und wann. „Wiederherstellen“ macht das rückgängig. Bearbeitete Nachrichten zeigen ihre früheren Fassungen. Über **Person …** schaltest du stumm oder schließt aus. Admins können eine Nachricht außerdem **endgültig löschen** – sie verschwindet dann vollständig, auch aus der Datei. Das Suchfeld durchsucht alle Kanäle, auch Originaltexte gelöschter Nachrichten und Namen. Bist du im selben Browser angemeldet, erscheint im Chat an jeder fremden Nachricht ein Moderations-Knopf.
- **Automod:** siehe unten – dort auch der **Wortfilter**.
- **Meldungen:** Nachrichten, die Personen über „Mehr → Melden …“ gemeldet haben – mit Grund, gemeldet von wem, Anzahl der Meldungen. „Nachricht löschen“, „Verwerfen“, „Person …“ oder „Im Kanal zeigen“. Die Zahl offener Meldungen steht in der Navigation.
- **Personen:** aktive Sperren, alle reservierten Namen (durchsuchbar), Team-Abzeichen, Rollen, geschützte Namen – siehe [Moderation und Schutz](#moderation-und-schutz).
- **Protokoll:** Wer hat wann was getan – Anmeldungen, Löschungen, Sperren, Änderungen an Einstellungen, Kanälen, Konten, Widgets und Backups, auch Aktionen von Moderations-Bots. Filterbar; bei endgültigen Löschungen landet der Text bewusst nicht im Protokoll.
- **Kanäle:** anlegen, umbenennen, Beschreibung, **wer lesen und wer schreiben darf** (Rollen, siehe unten), **Langsam-Modus** und **Link-Regel** – getrennt für Personen und für Bots –, löschen. Aus dem Kanal-Dialog geht es direkt zur Automod des Kanals. Beim Umbenennen wandern Verlauf, Nutzer, Bot-Rechte, Widgets, Gruppe und Position mit. **Gruppen und Reihenfolge:** „Gruppe anlegen“, dann Kanäle per Ziehen (oder mit ↑/↓ und dem Gruppen-Auswahlfeld, auch am Handy) einsortieren; Gruppen lassen sich umbenennen, verschieben und löschen (die Kanäle bleiben). Kanäle ohne Gruppe stehen im Chat oben. Änderungen erscheinen sofort bei allen.
- **Allgemein:** Titel, Farbgestaltung (siehe unten), ob Besucher eigene Kanäle erstellen dürfen, ob Direktnachrichten erlaubt sind, Datenschutz (Links, Aufbewahrungsfrist), Zeitzone und Öffnungszeiten.
- **Widgets:** siehe [Widget einbetten](#widget-in-eine-andere-webseite-einbetten).
- **Emotes & GIFs:** siehe [Emotes und GIFs](#emotes-und-gifs).
- **Befehle:** siehe [Befehls-Bot](#befehls-bot).
- **Ankündigungen:** siehe [Geplante Ankündigungen](#geplante-ankündigungen).
- **Bots:** siehe [Bots](#bots).
- **Team-Zugänge** und **Backups:** siehe oben.
- **Hosting:** Betriebsart und Speicher auf einen Blick; im Modus „hosted“ Gäste-Regel, Registrierung, E-Mail-Versand (SMTP mit Test-E-Mail), Einladungslinks, Föderation (erlaubte/gesperrte Instanzen, bekannte Instanzen mit Schlüssel-Fingerabdruck) – siehe [Modus „hosted“](#modus-hosted).
- **Beacon:** im Instanz-Verzeichnis eintragen und GIF-Sammlungen abonnieren – siehe [Beacon](#beacon-instanz-verzeichnis-und-gif-hub).
- **Vorlagen:** Grundkonfiguration exportieren und importieren – siehe [Vorlagen](#vorlagen-grundkonfiguration-exportieren-und-importieren).
- **Sprachen:** Standardsprache, Browsersprache, Sprachen ein-/ausblenden, Texte bearbeiten, eigene Sprachen – siehe [Sprachen und Übersetzungen](#sprachen-und-übersetzungen).

## Beacon: Instanz-Verzeichnis und GIF-Hub

`ucng-beacon` ist ein **eigenes kleines Programm** (gleiches Repository, keine Abhängigkeiten) mit Weboberfläche:

- **Instanzen:** UCNG-Instanzen tragen sich selbst ein. Besucher finden sie mit Name, Adresse, Beschreibung, **Anzahl der Nutzer**, **bevorzugter Sprache** und **Land** (Flagge), Betriebsart, ob Registrierung offen ist und ob Föderation an ist – durchsuchbar, filterbar nach Sprache und Land, sortierbar (meiste Nutzer, zuletzt gemeldet, neueste, Name).
- **GIFs:** wie die GIF-Bibliothek im Chat – hochladen, per URL laden oder bei GIPHY/Tenor suchen und übernehmen (mit eigenem API-Schlüssel des Beacons). GIFs liegen in **Sammlungen** (ein GIF kann in mehreren stehen). Die öffentliche **GIF-Ansicht** zeigt alle Sammlungen; zu jeder gibt es eine **Abo-Adresse** (`https://beacon.example.org#c_12ab34cd`) zum Kopieren.
- **Verwaltung** (Reiter „Verwaltung“, Passwort aus `ADMIN_PASSWORD`): Name und Beschreibung, **Quellenangabe an GIFs** (steht im Chat unter jedem GIF aus diesem Beacon, z. B. „GIFs vom Alpenraum“; leer = „via“ + Name des Beacons), GIF-Anbieter, Sammlungen, GIFs bearbeiten/löschen, gemeldete Instanzen ausblenden oder entfernen, **Sprachen** (derselbe Editor wie im Chat; Änderungen in `DATA_DIR/i18n.json`).
- **Sprache:** oben rechts umschaltbar (Deutsch, Englisch und eigene Sprachen); Fehlermeldungen des Beacons kommen in der Sprache der Anfrage – auch die, die eine Instanz bei der Anmeldung zurückbekommt (in der Standardsprache der Instanz).
- **Sprachpakete für Chat-Server** (Reiter „Sprachen“): Das Team stellt Übersetzungen der Chat-Oberfläche bereit; Instanzen installieren sie unter Admin → Sprachen. Jeder kann je Sprache die Sprachdatei und das **Übersetzungspaket** (alle oder nur offene Texte) herunterladen – auch für eine neue Sprache (Kürzel und Name eingeben) – und es z. B. von einer KI ausfüllen lassen. In der Verwaltung lädt das Team Sprachdateien oder ausgefüllte Pakete hoch (ergänzen oder ersetzen, mit Hinweis wie „von der Community geprüft“); die Texte werden gegen die deutschen Ausgangstexte des Chats geprüft, der Beacon bringt sie mit (`cmd/ucng-beacon/chati18n/`, Kopie von `web/i18n/`). Gespeichert in `DATA_DIR/langpacks/`; zu jedem Text wird das Original gemerkt, damit veraltete Übersetzungen erkannt werden.

```bash
go build ./cmd/ucng-beacon
ADMIN_PASSWORD=langes-passwort PUBLIC_URL=https://beacon.example.org ./ucng-beacon
# → http://localhost:8090
```

| Variable | Standard | Bedeutung |
|----------|----------|-----------|
| `ADDR` | `:8090` | Listen-Adresse |
| `DATA_DIR` | `beacon-data` | Datenordner (JSON-Dateien, `media/`, `originals/`) |
| `ADMIN_PASSWORD` | zufällig (steht im Log) | Passwort für die Verwaltung |
| `PUBLIC_URL` | – | eigene Adresse; wird in Abo-Adressen und GIF-Links verwendet |
| `TRUST_PROXY` | – | `1`: Client-IP aus `X-Forwarded-For` (hinter einem Reverse-Proxy) |
| `GIF_SIZE` | `480` | GIFs werden in diesen Rahmen eingepasst (100–1000) |
| `NO_PORTS` | – | `1`: Instanzen mit Port in der Adresse (`chat.example.org:8443`) ablehnen |
| `HTTPS_ONLY` | – | `1`: nur Instanzen mit `https://` annehmen (sonst auch `http://`, mit Abzeichen „ohne HTTPS“) |
| `PUBLIC_ONLY` | – | `1`: nur Instanzen mit öffentlicher IP-Adresse annehmen (keine im lokalen Netz) |
| `SOURCE_URL` | bei Releases das GitHub-Repository | Adresse des Quellcodes dieser Fassung (AGPL), steht im Fuß der Seite |

**Vertrauensstufen:** Jede Instanz trägt ein Abzeichen, nach dem die Liste auch gefiltert werden kann:

| Stufe | Bedeutung |
|-------|-----------|
| **Offiziell** | vom Team des Beacons bzw. vom Projekt selbst betrieben |
| **Empfohlen** | vom Team geprüft und für gut befunden |
| **Standard** | vom Team gesichtet und unauffällig – keine besondere Empfehlung, aber auch kein Warnhinweis |
| **Community** | Standard für jede Instanz, die sich selbst einträgt – niemand hat sie geprüft; Besucher werden um Vorsicht gebeten |

Die Liste ist nach Stufe geordnet (Offiziell, Empfohlen, Standard, Community), innerhalb der Stufe nach der gewählten Sortierung. Die Stufe vergibt das Team in der Verwaltung (mit optionalem Hinweis, z. B. „Betrieben vom UCNG-Projekt“), auch vorab für Domains, die sich noch nicht gemeldet haben. Sie gilt für den **Serverschlüssel** der Instanz: Meldet sich die Domain mit einem anderen Schlüssel (etwa nach einem Besitzerwechsel), fällt sie automatisch auf „Community“ zurück und die Verwaltung zeigt einen Hinweis. Ab- und wieder Anmelden behält die Stufe. Die Chat-Verwaltung zeigt unter „Beacon“, welche Stufe die eigene Instanz hat. Instanzen mit Port in der Adresse werden angenommen (Abzeichen „eigener Port“); mit `NO_PORTS=1` lehnt der Beacon sie ab.

**Erreichbarkeit:** Der Beacon ruft zur Prüfung `<PUBLIC_URL der Instanz>/.well-known/ucng` auf – mit dem Protokoll aus `PUBLIC_URL` (`http://` oder `https://`). Diese Adresse muss also **vom Rechner des Beacons aus** erreichbar sein; `http://localhost:8080` funktioniert nur, wenn Beacon und Chat auf demselben Rechner laufen. Schlägt das fehl, nennt die Fehlermeldung im Chat-Admin die versuchte Adresse und den Grund (Verbindung abgelehnt, Name nicht auflösbar, Zeitüberschreitung, TLS-Fehler …). Der Chat selbst erreicht den Beacon unter jeder eingetragenen Adresse, auch `http://` und im lokalen Netz – `FED_INSECURE` ist dafür nicht nötig.

**Anmeldung einer Instanz:** Im Chat unter Admin → **Beacon** die Adresse des Beacons eintragen, Sprache, Land und Kurzbeschreibung wählen und „Im Verzeichnis anzeigen“ einschalten. Die Instanz meldet sich sofort und dann **alle 6 Stunden** mit aktuellen Zahlen; die Vorschau zeigt genau, was übertragen wird. Voraussetzung ist `PUBLIC_URL`. Die Anmeldung ist mit dem **Serverschlüssel** der Instanz signiert (wie bei der Föderation); der Beacon lädt den Schlüssel von `https://<domain>/.well-known/ucng` und prüft die Signatur – Angaben für eine Domain kann also nur machen, wer sie betreibt. Ausschalten (oder Wechsel zu einem anderen Beacon) trägt die Instanz wieder aus. Instanzen, die sich 3 Tage nicht melden, verschwinden aus der Liste; nach 30 Tagen werden sie gelöscht.

**GIF-Sammlungen abonnieren:** Im Chat unter Admin → Beacon die Adresse eines Beacons oder die Abo-Adresse einer Sammlung einfügen, Sammlungen anhaken (nichts angehakt = alle, auch künftige). Bis zu 10 Beacons. Im Chat erscheint im GIF-Fenster der Reiter **„Sammlungen“**. Vorschaubilder laufen über den eigenen Server, und beim Senden lädt der Server das GIF einmal, skaliert und speichert es – Besucher verbinden sich nie mit dem Beacon, und Nachrichten zeigen das GIF auch dann noch, wenn es beim Beacon gelöscht wird.

Öffentliche API des Beacons (CORS offen, Lese-Limit 240/min pro IP):

| Methode | Pfad | Beschreibung |
|---------|------|--------------|
| `GET` | `/api/v1/info` | Name, Beschreibung, Zahlen, Sprachen und Länder |
| `GET` | `/api/v1/instances?q=&language=&country=&trust=official\|recommended\|standard\|community&sort=users\|active\|new\|name` | gelistete Instanzen (mit `trust`, `trustNote`, `port`) |
| `POST` | `/api/v1/register` | Anmeldung (signiert, siehe oben); `{"listed":false}` trägt aus |
| `GET` | `/api/v1/collections` | Sammlungen mit Anzahl und Vorschaubild |
| `GET` | `/api/v1/gifs?q=&collection=c_…,c_…&limit=&offset=` | GIFs, Antwort `{"gifs":[…],"total":n}` |
| `GET` | `/api/v1/gifs/{id}` | ein GIF |
| `GET` | `/api/v1/languages` | Sprachpakete für Chat-Server (Name, Format, Vollständigkeit, veraltete Texte, Stand) |
| `GET` | `/api/v1/languages/{code}` | Sprachdatei eines Pakets (`?download=1` als Datei) |
| `GET` | `/api/v1/languages/{code}/package?open=1&name=…&locale=…` | Übersetzungspaket – auch für Sprachen ohne Paket |

Mit Docker: `docker build -f Dockerfile.beacon -t ucng-beacon .` und `docker run -p 8090:8090 -v beacon:/data -e ADMIN_PASSWORD=… ucng-beacon`. Für den öffentlichen Betrieb hinter einen Reverse-Proxy mit HTTPS stellen.

## Vorlagen: Grundkonfiguration exportieren und importieren

Admin → **Vorlagen** packt ausgewählte Teile der Einrichtung in eine `.zip` – zum Sichern, Weitergeben oder um eine neue Instanz schnell aufzusetzen. Wählbar sind einzeln:

| Teil | Inhalt |
|------|--------|
| Rollen | Namen, Farben, Rechte |
| Kanäle & Gruppen | Beschreibung, Langsam-Modus, Link-Regeln, Lese-/Schreibrechte (Rollen werden über ihren Namen zugeordnet), Automod pro Kanal, Gruppen und Reihenfolge – **keine Nachrichten** |
| Automod & Wortfilter | alle Muster |
| Befehle | eigene und fertige Befehle, Präfix, Bot-Name |
| Bots | Name, Beschreibung, Kanäle, Rechte, Webhook-URL – **ohne Schlüssel und Webhook-Geheimnis** |
| Widgets | Aussehen, Kanal, erlaubte Domains |
| Emotes | mit Bildern (Original) |
| GIFs | Bibliothek mit Bildern und die abonnierten Beacon-Sammlungen |
| Medien-Einstellungen | Emote-/GIF-Größe, Anbieter, Jugendschutz, Reaktionen, Bilder-Regel – **ohne API-Schlüssel** |
| Sprachen | eigene Übersetzungen (mit dem Original, auf dem sie beruhen) und eigene Sprachen; beim Ersetzen auch Standardsprache und ausgeblendete Sprachen |
| Allgemein & Design | Titel, Farben, Links, Zeitzone, Öffnungszeiten, Einbettungs-Regel |

Nie enthalten: Nachrichten, Personen, Konten, Passwörter, Server- und Bot-Schlüssel, SMTP-Daten.

**Import:** Datei wählen → „Prüfen“ zeigt Herkunft, Datum und was enthalten ist → Teile anhaken → Vorgehen wählen:

- **ergänzen:** Neues kommt hinzu, Gleichnamiges bleibt unverändert (gleiche GIFs werden am Inhalt erkannt). Mehrfaches Importieren legt nichts doppelt an.
- **ersetzen:** Gleichnamiges wird überschrieben, was nicht in der Vorlage steht, wird entfernt (Rollen, Bots, Widgets, Emotes, GIFs, Befehle, Automod). **Kanäle werden nie gelöscht.** Gleichnamige Bots und Widgets behalten Schlüssel bzw. Einbettungscode.

Neu angelegte Bots bekommen neue Schlüssel; sie werden im Ergebnis **einmalig** angezeigt. Vorlagen bis 64 MB; eine geprüfte Vorlage wird 30 Minuten bereitgehalten.

## Sprachen und Übersetzungen

Alle Texte – Chat, Widget, Konten-Dialoge, Verwaltung, E-Mails und Server-Meldungen (Fehler, Hinweise des Befehls-Bots) – stehen in **Sprachdateien**. Eingebaut sind **Deutsch** (Ausgangssprache) und **Englisch**.

**Welche Sprache jemand sieht:** zuerst die selbst gewählte (Cookie `gochat_lang`), sonst – wenn „Browsersprache verwenden“ an ist – die erste passende Sprache des Browsers, sonst die **Standardsprache** der Instanz. Umschalten:

- **Chat:** unten in der Seitenleiste, im Profil-Dialog und im „Wie möchtest du heißen?“-Dialog
- **Verwaltung:** unten in der Seitenleiste und auf der Anmeldeseite
- **Widget:** `data-lang="en"` bzw. `UCNG.init({ lang: "en" })`; sonst Browsersprache bzw. Standardsprache
- **Beacon:** oben rechts

Die Auswahl erscheint nur, wenn mehr als eine Sprache angeboten wird. Fehlermeldungen der API (`"error"`) kommen ebenfalls in dieser Sprache; Bots und andere Programme können sie mit dem Header `X-Lang: en` wählen. Antworten des Befehls-Bots, die alle im Kanal sehen (z. B. „3 online: Anna und 2 Gäste“), stehen in der **Standardsprache**; private Antworten und Hinweise in der Sprache der Person. Was Admins selbst eintragen – Kanalnamen, Befehle und ihre Antworten, Begrüßungen, Rollen –, ist Inhalt und wird nicht übersetzt.

### Texte bearbeiten (Admin → Sprachen)

- **Einstellungen:** Standardsprache, „Browsersprache verwenden“
- **Sprachen:** Vollständigkeit, Zahl eigener Änderungen und veralteter Änderungen; ein- und ausblenden; **eigene Sprache anlegen** (Kürzel nach BCP 47, z. B. `fr` oder `pt-BR`, Name, Datums-/Zahlenformat, Schreibrichtung). Eigene Sprachen zeigen bis zur Übersetzung die deutschen Texte.
- **Editor:** Suche nach Text oder Schlüssel, Filter nach Bereich (Chat, Konto, Widget, Verwaltung, Server-Meldungen, E-Mails …) und Zustand (**fehlend**, **geändert**, **veraltet**). Zu jedem Text stehen die deutsche Ausgangsfassung und die offizielle Übersetzung. Speichern mit dem Knopf oder `Strg`+`Enter`, „Zurücksetzen“ stellt das Original wieder her. Änderungen gelten sofort, ohne Neustart.
- **Export/Import:** je Sprache die vollständige Sprachdatei, nur die eigenen Änderungen oder das **Übersetzungspaket** herunterladen; Sprachdateien und ausgefüllte Pakete importieren (ergänzen oder alle Änderungen ersetzen). Texte, die vom Original abweichen, werden als eigene Änderungen übernommen; leere Texte lassen Vorhandenes unberührt.
- **Sprachpakete vom Beacon:** Adresse eines Beacons eintragen (eingetragene Beacons werden vorgeschlagen) → angebotene Sprachen mit Vollständigkeit und Stand → **Installieren**. Die Sprache wird angelegt und die Texte wie bei einem Import übernommen; ist beim Beacon eine neuere Fassung da, erscheint „Update verfügbar“.

### Mit KI übersetzen: das Übersetzungspaket

Für eine automatische Übersetzung braucht eine KI das Format und die Ausgangstexte – beides steckt im **Übersetzungspaket** (Admin → Sprachen → Sprache → „Übersetzungspaket herunterladen“, im Beacon unter „Sprachen“ auch für noch nicht vorhandene Sprachen). Es ist eine JSON-Datei mit:

- `instructions`: Anleitung für die KI (Englisch) – nur `text` ausfüllen, Platzhalter, Mehrzahl, Markup, Tonfall, was nicht übersetzt wird
- `meta`: Name, Format (`locale`) und Schreibrichtung der Zielsprache
- `texts`: je Schlüssel der deutsche Ausgangstext (`source`), die englische Fassung zum Vergleich (`reference`), die Übersetzung (`text` – auszufüllen), der Zustand (`missing`, `outdated` mit dem früheren Ausgangstext in `previousSource`, `translated`), die Platzhalter sowie Hinweise `plural` und `html`

```json
"i18ned.treffer": {
  "source": "{n, plural, one {# Text} other {# Texte}} von {total}",
  "reference": "{n, plural, one {# text} other {# texts}} of {total}",
  "text": "",
  "status": "missing",
  "placeholders": ["{n}", "{total}"],
  "plural": true
}
```

Mit „nur fehlende und veraltete Texte“ enthält das Paket nur, was noch zu tun ist. Ablauf: Paket herunterladen → einer KI geben („Fülle die Felder `text` aus und gib das vollständige JSON zurück“) → Ergebnis importieren. Beim Import werden Platzhalter und Klammern geprüft; fehlerhafte Texte werden mit Grund gemeldet und übersprungen, leere bleiben offen. Auch in eigenen Sprachen werden Übersetzungen als **veraltet** markiert, wenn sich der deutsche Ausgangstext ändert – das nächste Paket enthält sie dann wieder.

**Updates:** Die offiziellen Dateien sind in das Programm eingebaut; deine Änderungen liegen getrennt davon im Speicher der Instanz (Dokument `i18n.json` im Datenordner bzw. in der Datenbank, also auch in Backups und – auf Wunsch – in [Vorlagen](#vorlagen-grundkonfiguration-exportieren-und-importieren)). Ein Update ersetzt nur die offiziellen Texte. Zu jeder Änderung wird das Original gespeichert, auf dem sie beruht: Ändert ein Update dieses Original, bleibt deine Fassung aktiv, wird aber als **veraltet** markiert und zeigt das alte und das neue Original nebeneinander. „Passt so“ bestätigt sie, „Zurücksetzen“ übernimmt das neue Original. Neue Texte eines Updates erscheinen in eigenen Sprachen unter „fehlend“.

### Platzhalter

Werte stehen in geschweiften Klammern und werden beim Anzeigen eingesetzt. Der Editor zeigt die erlaubten Platzhalter als Knöpfe (Klick fügt an der Cursorposition ein), dazu eine Vorschau mit Beispielwerten.

| Schreibweise | Bedeutung |
|--------------|-----------|
| `Hallo {name}!` | Wert einsetzen |
| `{n, plural, =0 {keine Nachricht} one {# Nachricht} other {# Nachrichten}}` | Mehrzahl; `#` ist die Zahl. Im Browser gelten die Regeln der jeweiligen Sprache (`Intl.PluralRules`, z. B. `few`/`many` für Polnisch), auf dem Server vereinfachte Regeln (`one`/`other`) |
| `{{` und `}}` | wörtliche Klammern |
| `{1}`, `{2}` | Werte in Server-Meldungen, in der Reihenfolge des deutschen Originals |
| `<strong>`, `<em>`, `<code>`, `<br>`, `<a href>` | nur in Texten mit Formatierung; alles andere wird als Text angezeigt |

Beim Speichern wird geprüft: Platzhalter, die das Original nicht kennt, und unpaarige Klammern werden **abgelehnt**; fehlt ein Platzhalter, gibt es nur eine **Warnung** (manchmal ist das gewollt, z. B. „ein“ statt „1“).

### Eine Sprache zum Projekt beitragen

1. `web/i18n/en.json` nach `web/i18n/<code>.json` kopieren, `_meta` anpassen (`{"name": "Français", "locale": "fr-FR"}`, bei Bedarf `"dir": "rtl"`) und die Texte übersetzen. Für den Beacon dasselbe in `cmd/ucng-beacon/web/i18n/`.
2. `go test ./...` prüft alle Dateien: unbekannte Schlüssel, Platzhalter, Klammern und fehlende Texte.
3. Neue Texte im Code: in JavaScript `_t("bereich.schluessel", { wert })`, in HTML `data-i18n="…"` (auch `data-i18n-html`, `-placeholder`, `-title`, `-aria-label`); in Go werden Fehlermeldungen über ihren deutschen Text gefunden (Schlüssel `srv.<Text oder Format>`). Danach `UPDATE_I18N=1 go test -run TestWidgetFallback .` – das schreibt die deutschen Rückfalltexte ins Widget (für Seiten, auf denen der Server nicht erreichbar ist). Geänderte Chat-Sprachdateien außerdem nach `cmd/ucng-beacon/chati18n/` kopieren und `web/i18n.js` sowie `web/i18n-editor.*` nach `cmd/ucng-beacon/web/` – `go test ./...` meldet jede Abweichung.

## Moderation und Schutz

- **Stummschalten** (10 Minuten bis 30 Tage): Die Person liest weiter, kann aber nicht schreiben, reagieren oder bearbeiten. Im Chat sieht sie sofort einen Hinweis mit Ende und Grund; danach geht es automatisch weiter.
- **Ausschließen** (zeitlich oder dauerhaft): wie stumm, zusätzlich optional **„auch die Verbindung sperren“** – dann kann sich von dort niemand einen neuen Namen holen. Gespeichert wird dafür nie die IP-Adresse, sondern nur ein Schlüssel-Hash (HMAC mit einem geheimen Server-Schlüssel), der sich nicht zurückrechnen lässt.
- **Team-Abzeichen:** Admins vergeben es unter Personen. Es erscheint an allen Nachrichten der Person („Team“) und in der Online-Liste. Team-Mitglieder dürfen geschützte Namen tragen (z. B. „Support Anna“), sind von Langsam-Modus, Link-Regel und Wiederholungs-Bremse ausgenommen und können nicht gesperrt werden.
- **Rollen:** Neben dem Team-Abzeichen legst du unter Personen → Rollen beliebig viele Rollen an – z. B. „Moderator“, „VIP“, „Redaktion“, „Stammgast“: Name, Farbe (13 Vorgaben oder frei wählbar), auf Wunsch „Namen in dieser Farbe zeigen“ und **„per @Rollenname erwähnbar“** (`@Moderator` benachrichtigt alle mit der Rolle). Vergeben werden sie unter **Verwalten** bei der Person, eine Person kann mehrere tragen. Die Abzeichen erscheinen sofort an allen Nachrichten, in der Online-Liste und im Widget – in der Reihenfolge der Rollenliste (per Pfeil sortierbar). „Team“ und „Bot“ sind als Rollennamen gesperrt. Wird ein Name freigegeben oder läuft ab, verfallen auch seine Rollen.
- **Rechte einer Rolle** (frei kombinierbar; das Team hat immer alle):

  | Recht | Wirkung |
  |-------|---------|
  | Links immer erlaubt | Link-Regel des Kanals gilt nicht |
  | Kein Langsam-Modus | weder Langsam-Modus noch Wiederholungs-Bremse |
  | Geschützte Namen tragen | darf z. B. „Support Anna“ heißen |
  | Nachrichten anpinnen | über „Mehr → Anpinnen“ |
  | Bilder hochladen | wenn Bilder auf Rollen beschränkt sind |
  | Nachrichten im Chat löschen | „Mehr → Löschen (Moderation)“ – wie im Admin-Bereich, wird protokolliert |
  | Im Chat stummschalten | 5 Min., 15 Min. oder 1 Stunde; Team und andere Moderierende sind ausgenommen |

- **Private Kanäle und Nur-lesen-Kanäle:** Am Kanal stellst du ein, welche Rollen **lesen** dürfen (leer = alle) und welche **schreiben** (leer = alle, die lesen). Private Kanäle tauchen für andere gar nicht auf – weder in der Liste noch über Links, Suche, Live-Stream oder Ungelesen-Zähler. In Nur-lesen-Kanälen (z. B. `#ankündigungen`) lesen und reagieren alle, schreiben dürfen nur die gewählten Rollen. „Nur Team“ ist ebenfalls wählbar. Verliert jemand eine Rolle, endet seine Verbindung zum Kanal sofort; wird die letzte Rolle eines privaten Kanals gelöscht, bleibt er dem Team vorbehalten (er wird nie plötzlich öffentlich).
- **Melden:** Jede Person kann fremde Nachrichten über „Mehr → Melden …“ mit Grund melden (je Nachricht einmal, höchstens 10 Meldungen in 10 Minuten). Die Moderation bearbeitet sie unter **Meldungen**; wer gemeldet hat, erfährt die betroffene Person nicht.
- **Wortfilter** (Automod → unten): Wörter auf dieser Liste werden durch Sternchen ersetzt (`Mist` → `****`), statt die Nachricht auszublenden. Gleiche Muster und Tarnungs-Erkennung wie bei der Automod, geprüft Wort für Wort. Das Original sehen nur Admins und Moderatoren.
- **Geschützte Namen:** Muster wie bei der Automod (`admin*`, `support*`, `*ucng*` …) – mit denselben Tarnungs-Erkennungen, also auch „Adm1n“ oder „Supp0rt“. Niemand ohne Team-Abzeichen kann solche Namen wählen. Voreingestellt sind gängige Rollen-Namen.
- **Langsam-Modus** pro Kanal: Mindestabstand zwischen zwei Nachrichten einer Person (5 Sekunden bis 15 Minuten).
- **Link-Regel** pro Kanal: Links für alle, erst wenn der Name älter als 24 Stunden ist, oder nur für das Team.
- **Wiederholungs-Bremse:** Dieselbe Nachricht ab 30 Zeichen wird innerhalb von 2 Minuten nicht zweimal angenommen, kürzere („ok“) höchstens zweimal.
- **Für Bots** stellst du pro Kanal einen eigenen Langsam-Modus (Abstand zwischen zwei Bot-Nachrichten) und ob Bots Links posten dürfen ein; die Regeln für Personen gelten für Bots nicht. Die Wiederholungs-Bremse gilt für Bots nicht.
- **Moderations-Bots:** Bots können Rechte zum Löschen, Stummschalten und zeitlichen Ausschließen bekommen – mit einstellbarer Höchstdauer (siehe [Bots](#bots)).

## Datenschutz

- **Aufbewahrungsfrist** (Allgemein → Datenschutz): Nachrichten älter als X Tage werden stündlich **endgültig** gelöscht – auch Originaltexte in der Moderation und ältere Protokolleinträge. Wird die Frist verkürzt, fragt der Admin-Bereich vorher nach.
- **Eigene Nachrichten löschen:** Löscht jemand selbst, wird der Text wirklich entfernt – auch für Admins bleibt nur „Nachricht wurde gelöscht“.
- **Endgültig löschen:** Admins entfernen einzelne Nachrichten vollständig oder unter Personen → „Alle Nachrichten endgültig löschen“ alles einer Person (Nachrichten und Reaktionen, auf Wunsch auch den Namen) – z. B. für Löschanfragen nach DSGVO. Die Dateien werden dabei neu geschrieben, und Name bzw. Textauszüge verschwinden auch aus dem Protokoll (dort bleibt nur, dass gelöscht wurde).
- **Links zu Datenschutzerklärung und Impressum** erscheinen in der Seitenleiste des Chats.
- **Direktnachrichten** sind privat zwischen zwei Personen, im Modus „simple“ aber **nicht Ende-zu-Ende-verschlüsselt**: Sie liegen wie alle Nachrichten im Datenordner, und Admins sehen gemeldete Direktnachrichten. Im Modus „hosted“ sind Direktnachrichten zwischen Konten verschlüsselt (siehe [Modus „hosted“](#modus-hosted)). Bots und Webhooks erhalten sie nie. Unter Allgemein lassen sie sich abschalten.
- **Bilder und Profilbilder:** Gelöschte Nachrichten und abgelaufene Namen lassen ihre Bilder verwaist zurück – der Server entfernt solche Dateien stündlich (nach einer Stunde Schonfrist). „Alle Nachrichten endgültig löschen“ entfernt auch Profilbild und Status. Unpassende Profilbilder entfernst du unter Personen → Verwalten.
- Beachte: Backups enthalten den Stand ihres Zeitpunkts. Bei einer Löschanfrage verschwinden die Daten aus den Backups, sobald diese rotiert sind (standardmäßig nach 7 Tagen).

## Farbgestaltung

Unter **Allgemein → Farbgestaltung** stellst du ein:

- **Akzentfarbe** – für Knöpfe, eigene Nachrichten, den aktiven Kanal und Links. Acht Vorschläge oder eine beliebige eigene Farbe.
- **Seitenleiste** – Standard (dunkles Blau), Schwarz, Schiefer, Hellgrau, Weiß oder eine eigene Farbe.
- **Darstellung** – wie das Betriebssystem (hell oder dunkel), immer hell oder immer dunkel.

Jede Änderung siehst du im Admin-Bereich sofort als Vorschau; erst „Änderungen speichern“ macht sie für alle gültig – offene Chats wechseln dann live. Der Server erzeugt daraus `/theme.css` und rechnet alle abgeleiteten Töne selbst aus: Hover-Farben, zarte Hintergründe, und vor allem die Schriftfarbe auf Akzent und Seitenleiste (weiß, wenn gut lesbar, sonst dunkel – nach WCAG-Kontrast). Links in der Akzentfarbe werden bei Bedarf so lange abgedunkelt bzw. aufgehellt, bis sie gut lesbar sind. Auch bei Gelb oder Weiß bleibt also alles lesbar.

Das eingebettete Widget übernimmt die Akzentfarbe der Anwendung automatisch, sofern auf der Webseite kein eigenes `data-color` angegeben ist.

## Automoderation

Unter **Automod** im Admin-Bereich pflegst du gesperrte Wörter und Muster – eins pro Zeile, Zeilen mit `#` sind Kommentare. Die Anzahl ist nicht begrenzt: Auch Listen mit Zehntausenden Einträgen bleiben schnell (getestet mit über 50.000 Mustern: eine Nachricht wird in wenigen Millisekunden geprüft), weil ganze Wörter, `wort*`, `*wort`, `*wort*` und `anfang*ende` über Tabellen nachgeschlagen werden. Nur seltenere Formen wie `a*b*c` oder Wortfolgen laufen über reguläre Ausdrücke. Doppelte Muster – auch in anderer Schreibweise wie `Arsch*` und `4rsch*` – werden beim Speichern zusammengefasst.

| Muster      | trifft |
|-------------|--------|
| `arschloch` | genau dieses Wort |
| `arsch*`    | Wörter, die so beginnen: Arsch, Arschloch, ARSCHGEIGE |
| `*loch`     | Wörter, die so enden: Arschloch, Loch |
| `arsc*och`  | Anfang und Ende stimmen: Arschloch |
| `*idiot*`   | irgendwo im Wort: Vollidiot, Idioten |
| `du opfer`  | mehrere Wörter hintereinander |

Muster und Nachrichten werden vor dem Vergleich vereinheitlicht, damit einfache Tarnungen nicht durchrutschen:

- Groß-/Kleinschreibung egal
- Ziffern und Zeichen als Buchstaben: `0`=o, `1`=i oder l, `3`=e, `4`=a, `5`=s, `7`=t, `8`=b, `@`=a, `$`=s, `€`=e, `!`/`|` im Wort = i
- Umlaute und Akzente: ä=a, ö=o, ü=u, ß=ss, é=e …
- Wiederholte Buchstaben zählen einfach: „Arrrrsch“ = „Arsch“
- Trennzeichen im Wort werden ignoriert: „A.r.s.c.h“, „Arsch-loch“, „Arsch_loch“; einzeln gesetzte Buchstaben werden zusammengezogen: „a r s c h“

Muster gelten pro Wort, nicht als beliebiger Textausschnitt: `arsch*` trifft deshalb nicht „Barschfilet“ oder „Marsch“. Willst du ein Wort auch mitten in anderen Wörtern finden, nutze `*wort*` – das erhöht aber das Risiko von Fehlalarmen.

**Was passiert bei einem Treffer?** Die Nachricht wird sofort ausgeblendet. Andere sehen „Nachricht wurde automatisch ausgeblendet“, der Absender sieht „Deine Nachricht wurde automatisch zurückgehalten und wird geprüft“. Im Admin-Bereich erscheint sie in der Liste **Zur Prüfung** mit Originaltext, Kanal und dem Muster, das angeschlagen hat; die Navigation zeigt die Anzahl offener Treffer. Dort kannst du:

- **Freischalten** – Fehlalarm, die Nachricht erscheint bei allen.
- **Ausgeblendet lassen** – die Nachricht bleibt verborgen und verschwindet aus der Prüfliste. In der Moderationsansicht bleibt sie mit Kennzeichnung sichtbar und kann später noch freigeschaltet werden.

### Pro Kanal

Oben im Automod-Bereich wählst du unter **Regeln für** entweder „Alle Kanäle – globale Regeln“ oder einen Kanal. Für jeden Kanal gibt es vier Möglichkeiten:

| Einstellung | Wirkung |
|-------------|---------|
| Globale Regeln | Standard – es gelten nur die globalen Muster |
| Globale und eigene Regeln | zusätzlich eigene Muster, z. B. strengere für einen Kinder- oder Support-Kanal |
| Nur eigene Regeln | die globalen gelten hier nicht, nur die eigenen |
| Aus | keine Automoderation in diesem Kanal |

Eigene Muster verwenden dieselbe Schreibweise und Tarnungs-Erkennung. Treffer erscheinen in der Prüfliste mit dem Kanal am Muster (z. B. `doof* (#kinder)`). Die Kanalliste zeigt, wo etwas anderes als global gilt; die Einstellungen wandern beim Umbenennen mit.

Mit dem Feld **Regeln testen** probierst du Beispieltexte gegen die gespeicherten Regeln aus und siehst, wie der Text vereinheitlicht wurde. Regeln und Prüfliste werden dauerhaft gespeichert.

Die Admin-API ist absichtlich nicht per CORS freigegeben: Sie funktioniert nur von der UCNG-Domain selbst.

## Chat-Funktionen

- **Kanalliste:** Kanäle stehen in den Gruppen, die das Team angelegt hat – Gruppen lassen sich auf- und zuklappen (der Browser merkt es sich; ein zugeklappter Gruppenkopf zeigt Ungelesenes gesammelt). Mit dem Pfeil-Knopf neben „Kanäle“ sortierst du nach **Reihenfolge des Teams**, **alphabetisch** oder **zuletzt aktiv**. Über die Stecknadel (beim Darüberfahren) **pinnst du Kanäle an** – sie stehen dann ganz oben unter „Angepinnt“ (mit Namen auf dem Server gespeichert, sonst im Browser).
- **Threads in der Seitenleiste:** Unter jedem Kanal stehen seine aktiven Threads (Antwort in den letzten 7 Tagen, die neuesten zuerst); ein Klick öffnet Kanal und Thread.
- **Darstellung:** Nachrichten stehen untereinander als schlichte Blöcke mit einer farbigen Kante links – wie das Antwort-Zitat. Eigene Nachrichten tragen die Akzentfarbe und ein „du“, Nachrichten mit einer Erwähnung von dir sind gelb markiert. Im Widget sieht es genauso aus.
- **Antworten:** Fahre über eine Nachricht (am Handy: antippen) und wähle „Antworten“. Die Antwort zeigt ein Zitat; ein Klick darauf springt zur Ursprungsnachricht.
- **Reaktionen:** Schnellauswahl (Standard 👍 ❤️ 😂 😮 😢 🙏, im Admin-Bereich frei wählbar – auch eigene Emotes), über „+“ zusätzlich alle Emotes. Klick auf eine vorhandene Reaktion macht mit oder nimmt sie zurück.
- **Bearbeiten:** eigene Nachrichten in den ersten 15 Minuten (Taste ↑ im leeren Eingabefeld bearbeitet die letzte). Sie tragen dann „bearbeitet“; die Automod prüft den neuen Text.
- **Löschen:** eigene Nachrichten jederzeit.
- **Erwähnungen:** `@` tippen zeigt passende Namen (online und aus dem Verlauf) und erwähnbare Rollen. Wer erwähnt wird – direkt oder über `@Rolle` –, sieht die Nachricht hervorgehoben und bekommt Ton/Hinweis wie bei einer Erwähnung.
- **Threads:** „Im Thread antworten“ an einer Nachricht öffnet rechts (am Handy bildschirmfüllend) einen eigenen Verlauf. Im Kanal steht an der Ausgangsnachricht nur „3 Antworten · Anna, Ben · zuletzt 14:05“ – lange Diskussionen fluten den Kanal nicht. Befehle im Thread beantwortet der Bot im Thread; die Suche findet auch Thread-Antworten.
- **Umfragen:** `!umfrage Wann treffen wir uns? | Freitag | Samstag` (2–10 Antworten; mit `mehrfach:` vor der Frage sind mehrere Antworten erlaubt). Abstimmen per Klick, Ergebnis live mit Balken; wer die Umfrage gestartet hat (oder löschen darf), kann sie beenden und wieder öffnen. Im Widget wird mit abgestimmt.
- **Angepinnt:** Oben im Kanal steht die zuletzt angepinnte Nachricht, ein Klick zeigt alle (höchstens 10) und springt hin. Anpinnen dürfen Team, Rollen mit dem Recht und Admins/Moderatoren in der Moderation.
- **Bilder:** Bild-Knopf, Einfügen aus der Zwischenablage oder Hineinziehen. Bilder werden auf eine feste Größe verkleinert (Admin-Bereich: 320–800 px), Fotos als JPEG, animierte GIFs bleiben animiert; Text im Eingabefeld wird zur Bildunterschrift. Wer hochladen darf, stellst du unter Emotes & GIFs ein (niemand / Namen älter als 24 h / alle / nur Rollen). Bis zu 8 MB, 15 Bilder in 10 Minuten je Person.
- **Direktnachrichten:** Über den Briefumschlag in der Online-Liste oder „Mehr → Direktnachricht“ an einer Nachricht. Unterhaltungen stehen links unter „Direktnachrichten“, neue zählen wie Erwähnungen. Nur die beiden Beteiligten sehen sie; Bots und Webhooks bekommen nichts davon mit. Abschaltbar unter Allgemein.
- **Profilbild und Status** (im Profil): eigenes Bild (mittig auf 128 × 128 zugeschnitten) statt des Buchstabens, Status „da“ / „abwesend“ / „beschäftigt“ mit kurzem Text („Im Urlaub“). Der Status erscheint als Punkt am Bild und in der Online-Liste; die Automod prüft den Text.
- **Melden:** „Mehr → Melden …“ an fremden Nachrichten.
- **Formatierung:** `**fett**`, `*kursiv*` oder `_kursiv_`, `~~durchgestrichen~~`, `` `Code` ``, ` ```Codeblock``` `, Links werden klickbar. Der Knopf „Aa“ im Eingabefeld zeigt die Übersicht.
- **Online-Liste:** Die Avatare oben rechts zeigen, wer gerade im Kanal ist (mit Team- und Rollen-Abzeichen), dazu die Zahl der Gäste ohne Namen.
- **Suche:** Lupe oben rechts oder `Strg+K` – durchsucht den Kanal, ein Klick springt zur Nachricht (ältere werden dafür nachgeladen).
- **Ungelesen:** Neben jedem Kanal steht die Zahl ungelesener Nachrichten, bei Erwähnungen ein rotes `@`. Beim Öffnen markiert ein Trenner „Neue Nachrichten“ die Stelle. Der Tab-Titel zeigt die Anzahl.
- **Emotes:** `:name:` tippen (mit Vervollständigung ab `:` + Buchstabe) oder den Smiley-Knopf nutzen. Im Text erscheinen Emotes in Zeilenhöhe, eine Nachricht nur aus Emotes in voller Größe.
- **GIFs:** Knopf „GIF“ – aus der eigenen Bibliothek oder (falls eingerichtet) per Websuche. Steht schon Text im Eingabefeld, wird er zur Bildunterschrift; eine laufende Antwort bleibt erhalten.
- **Befehle:** `!` am Anfang zeigt die verfügbaren Befehle mit Beschreibung.
- **Benachrichtigungen** (im Profil): Ton und Browser-Hinweis jeweils aus, nur bei Erwähnungen oder bei jeder Nachricht. Die Einstellung wandert mit Profil-Datei und Code mit.

## Emotes und GIFs

Verwaltet im Admin-Bereich unter **Emotes & GIFs** (nur Admins).

**Emotes**
- PNG, JPEG oder GIF hochladen (auch mehrere auf einmal) oder per Adresse importieren. Der Name kommt aus dem Feld oder dem Dateinamen (`a–z`, `0–9`, `_`, 2–32 Zeichen).
- Jedes Emote wird **fest auf 32 × 32 oder 64 × 64 Pixel** skaliert – Seitenverhältnis bleibt, der Rest ist transparent. Die Standardgröße stellst du oben ein, je Emote lässt sie sich umschalten. Weil das Original aufbewahrt wird, wird dabei immer neu aus dem Original gerechnet (keine Qualitätsverluste).
- Animierte GIFs bleiben animiert.
- Umbenennen und Löschen: alte Nachrichten zeigen dann nur noch den Text `:name:`.

**GIF-Bibliothek**
- Eigene GIFs hochladen oder per Adresse importieren, mit Titel und Schlagwörtern für die Suche.
- Alle GIFs werden auf eine **feste Größe** gebracht (in ein Quadrat von 200, 250, 300 oder 400 px, nie vergrößert). Änderst du die Größe, rechnet der Server die Bibliothek im Hintergrund neu.

**GIFs aus dem Web (GIPHY oder Tenor)** – optional
- Anbieter wählen, API-Schlüssel eintragen (kostenlos bei [developers.giphy.com](https://developers.giphy.com) bzw. in der Google Cloud Console für Tenor), Altersfreigabe festlegen.
- **Datenschutz:** Browser sprechen nie direkt mit GIPHY/Tenor. Suche und Vorschaubilder laufen über deinen Server, Clients schicken nur eine ID (`giphy:abc123`). Der Server lädt das GIF selbst, skaliert es auf die eingestellte Größe und speichert es zwischen. Der Schlüssel wird nie an Browser ausgeliefert.
- Websuche ist pro IP begrenzt (30 Anfragen/Minute).

**Reaktionen**
- Die **Schnellauswahl** beim Reagieren ist frei einstellbar: 1–12 Emojis und/oder Emotes, per Ziehen oder Pfeil sortierbar. Vorschläge und alle Emotes stehen zum Anklicken bereit; eigene Emojis lassen sich einfügen.
- **Alle Emotes als Reaktion erlauben** (Standard an): Im Chat öffnet „+“ alle übrigen Emotes (mit Suche ab 17 Emotes). Aus: nur die Schnellauswahl.
- An einer Nachricht stehen die Reaktionen in der Reihenfolge der Schnellauswahl, weitere dahinter. Höchstens 20 verschiedene je Nachricht.
- Nimmst du eine Reaktion aus der Auswahl, bleiben vorhandene stehen; wer sie gesetzt hat, kann sie noch zurücknehmen. Wird ein Emote umbenannt, zieht die Schnellauswahl mit.

Grenzen: Uploads bis 4 MB (Emotes) bzw. 20 MB (GIFs), höchstens 4000 px Kantenlänge und 400 Einzelbilder. WebP wird nicht unterstützt (die Go-Standardbibliothek kann es nicht lesen).

Im Widget werden Emotes, GIFs und hochgeladene Bilder angezeigt; Auswahl-Knöpfe gibt es dort bewusst nicht, `:name:` funktioniert aber auch beim Tippen. Umfragen lassen sich im Widget mit abstimmen; Thread-Antworten erscheinen dort nur als Hinweis „3 Antworten im Chat ansehen“ (Link zur vollen Chat-Seite). Nur-lesen-Kanäle sperren auch im Widget das Eingabefeld.

## Geplante Ankündigungen

Unter **Ankündigungen** (nur Admins) planst du Nachrichten, die der Befehls-Bot selbst schreibt:

- **Einmalig** (Datum und Uhrzeit), **täglich** oder **wöchentlich** an ausgewählten Wochentagen – in der eingestellten Zeitzone.
- Text mit Formatierung, Links, Emotes und den Platzhaltern `{date}` und `{time}`.
- **Anpinnen:** Die neue Ankündigung wird angepinnt, die vorige desselben Plans gelöst – praktisch für „Nächstes Treffen: …“.
- „Jetzt senden“ schickt sie sofort (zum Testen); die Liste zeigt den nächsten Termin, den letzten Versand und Fehler (z. B. gelöschter Kanal).
- Verpasste Termine (Server war aus) werden bis zu 10 Minuten nachgeholt, danach übersprungen.

## Befehls-Bot

Ein eingebauter Bot, der im Server mitläuft – kein API-Schlüssel, keine externe Verbindung. Einstellbar unter **Befehle**:

- **An/aus, Name** (Standard „Chat-Bot“, reserviert) und **Zeichen** für Befehle (`!`, `/`, `.`, `?`, `$`, `#`).
- **Fertige Befehle:** `!hilfe` (Liste), `!regeln`, `!würfel [Seiten]`, `!münze`, `!zeit`, `!online`, `!wähle A | B | C`, `!umfrage Frage? | A | B` (startet eine Umfrage im Namen der Person) – jeweils mit Aliassen (`!help`, `!dice` …). Alle lassen sich ändern oder abschalten.
- **Eigene Befehle** antworten mit einem Text deiner Wahl – mit Formatierung, Links und Emotes. Platzhalter: `{user}`, `{room}`, `{args}` (alles nach dem Befehl), `{result}` (Ergebnis bei Würfel, Münze …), `{time}`, `{date}`, `{online}`.
- Je Befehl: Aliasse, Beschreibung (für `!hilfe` und die Vervollständigung), **Pause** gegen Spam, **nur für das Team**, nur in **bestimmten Kanälen**, Antwort **mit Zitat**.
- **Nur für die Person sichtbar** (wie „ephemeral“ bei Discord): Befehl und Antwort sieht nur, wer den Befehl eingibt. Im Kanal erscheint nichts, gespeichert wird nichts, andere Personen und Bots bekommen nichts mit. Die Antwort steht mit „Nur für dich sichtbar“ im eigenen Verlauf, bis man sie ausblendet oder die Seite neu lädt – auch im Widget. Die Pause gilt dann je Person statt je Kanal; wer warten muss, bekommt einen privaten Hinweis. Standardmäßig privat: `!hilfe`, `!regeln`, `!zeit`.
- **Ausprobieren:** Im Admin-Bereich siehst du die Antwort vorab, als Person oder als Team, für einen bestimmten Kanal.

Öffentliche Antworten des Bots sind normale Nachrichten (mit „Bot“-Abzeichen). Langsam-Modus und Link-Regeln des Kanals gelten für ihn nicht; die Befehls-Nachricht selbst bleibt stehen. Hinweis: Private Befehle erreichen auch angeschlossene Bots nicht – gibt es einen eigenen Bot, der z. B. auf `!hilfe` reagieren soll, den eingebauten Befehl abschalten oder öffentlich machen.

## Namen (ohne Registrierung)

Es gibt keine Konten und keine Passwörter. Stattdessen:

1. Beim ersten Besuch wählt man einen Namen. Ist er frei, reserviert der Server ihn und gibt ein zufälliges, geheimes **Token** zurück.
2. Der Browser speichert das Token im `localStorage` und schickt es bei jeder Nachricht als `Authorization: Bearer …` mit.
3. Der Server ermittelt den Namen **aus dem Token** – ein im Request mitgeschickter Name wird ignoriert. Wer das Token nicht hat, kann nicht unter dem Namen schreiben.
4. Namen sind unabhängig von Groß-/Kleinschreibung und Leerzeichen eindeutig (`Anna` = `anna` = ` ANNA `).
5. Wird ein Name `NAME_TTL_DAYS` Tage lang nicht benutzt, wird er wieder frei.

Der Server speichert nur einen SHA-256-Hash des Tokens, nie das Token selbst.

**Umbenennen:** Jede Person hat zusätzlich eine feste, öffentliche ID, die an jeder ihrer Nachrichten hängt. Ändert jemand seinen Namen, erscheinen auch alle bisherigen Nachrichten unter dem neuen Namen – bei allen Lesern sofort. Der alte Name wird frei; nimmt ihn später jemand anderes, werden dessen Nachrichten nicht mit den alten verwechselt.

### Profil exportieren und importieren

Ein Klick auf den eigenen Namen unten links öffnet das **Profil**. Dort kannst du:

- **Namen ändern** – alle bisherigen Nachrichten erscheinen dann unter dem neuen Namen.
- **Profil-Datei speichern** – eine kleine JSON-Datei (`ucng-profil-<name>.json`).
- **Code kopieren** – derselbe Inhalt als Text (`UCNG1:…`), praktisch fürs Handy.
- **Profil importieren** – Datei auswählen oder Code einfügen. Das geht auch gleich beim ersten Besuch („Vorhandenes Profil übernehmen“), ohne vorher einen Namen zu wählen.
- **Abmelden und Namen freigeben** – der Name wird für andere frei.

Im **eingebetteten Widget** gibt es beim Namensschritt „Profil-Code einfügen“: So nutzt man auf der eigenen Webseite denselben Namen wie in der Weboberfläche.

Datei und Code enthalten das geheime Token und gelten nur für den Server, von dem sie stammen. Wer sie hat, kann unter deinem Namen schreiben – sie sind wie ein Passwort zu behandeln. Ist der Name inzwischen abgelaufen oder freigegeben, meldet der Import das verständlich.

**Zu beachten:**

- Das Token liegt im Browser. Wer Browserdaten löscht, ohne vorher das Profil zu speichern, verliert den Namen (er wird nach Ablauf der Frist wieder frei).
- Ein Token ist kein Passwortschutz im strengen Sinn: Es schützt vor Namensklau im Chat, nicht vor jemandem, der Zugriff auf den Browser hat.

## Widget in eine andere Webseite einbetten

### Variante 1: schwebender Chat-Button (eine Zeile)

```html
<script src="https://chat.example.com/embed/chat.js"
        data-room="lobby"
        data-title="Fragen? Schreib uns!"
        defer></script>
```

Das Script lädt `chat.css` und `format.js` automatisch vom selben Server nach. Das Widget verwendet bewusst kein `<form>`-Element, damit es auch auf Seiten funktioniert, die Formular-Absendungen seitenweit abfangen (viele CMS, Shop-Systeme, Turbo).

### Empfohlen: gespeicherte Widgets

Unter **Widgets** im Admin-Bereich legst du Widgets an: Bezeichnung, Kanal, Überschrift, Darstellung, Farbe, **Begrüßung**, **Öffnungszeiten** und **erlaubte Domains** – mit Live-Vorschau. Der Code für die Webseite ist dann nur noch:

```html
<script src="https://chat.example.com/embed/chat.js" data-widget="w_3fa9c01b2e" defer></script>
```

Alle Einstellungen kommen vom Server; spätere Änderungen wirken sofort auf allen Seiten, ohne den Code neu einzubauen. Zusätzliche `data-`Attribute überschreiben einzelne Einstellungen.

- **Begrüßung:** erscheint als erste Sprechblase im Chat und – beim schwebenden Button – einmal pro Besuch als kleiner Hinweis neben dem Button. Formatierung ist erlaubt.
- **Öffnungszeiten:** Unter Allgemein stellst du Zeiten je Wochentag, Zeitzone und einen Hinweis ein. Das Widget zeigt dann oben „Jetzt erreichbar · heute 09:00–18:00 Uhr“ oder „Gerade nicht besetzt · wieder ab morgen 09:00 Uhr“ mit deinem Hinweis. Schreiben bleibt immer möglich.
- **Wo darf eingebettet werden?** Unter Widgets wählst du „Auf jeder Webseite“ (Standard) oder **„Nur auf eingetragenen Domains“**. Dann funktioniert jedes Widget nur auf seinen Domains (`shop.example.com`, `*.example.com` für alle Subdomains, auch mit Port wie `localhost:3000`) und nur für seinen Kanal – auf anderen Seiten erscheint es gar nicht. Technisch geschieht das über CORS; `ALLOWED_ORIGINS` erlaubt zusätzlich Origins für alle Kanäle.

### Variante 2: inline in einem Element

```html
<div id="mein-chat" style="height: 500px"></div>

<script src="https://chat.example.com/embed/chat.js" data-manual="true"></script>
<script>
  UCNG.init({ room: "support", title: "Support", target: "#mein-chat", color: "#e4572e" });
</script>
```

### Variante 3: CSS selbst einbinden / anpassen

```html
<link rel="stylesheet" href="https://chat.example.com/embed/chat.css">
<script src="https://chat.example.com/embed/chat.js" data-room="lobby" data-css="false" defer></script>

<style>
  .gochat-root {
    --gochat-accent: #0a7d5a;
    --gochat-radius: 4px;
    --gochat-font: "Inter", sans-serif;
  }
</style>
```

### Optionen

| data-Attribut   | `init()`-Option | Bedeutung |
|-----------------|-----------------|-----------|
| `data-room`     | `room`          | Kanalname (Kleinbuchstaben inkl. Umlaute, Ziffern, `_`, `-`, max. 32 Zeichen) |
| `data-title`    | `title`         | Überschrift |
| `data-color`    | `color`         | Akzentfarbe |
| `data-position` | `position`      | `right` (Standard) oder `left` |
| `data-theme`    | `theme`         | `light` (Standard), `dark` oder `auto` (folgt dem Betriebssystem) |
| `data-target`   | `target`        | CSS-Selektor oder Element → Inline-Modus |
| `data-name`     | `name`          | Namen vorschlagen – ist er frei, wird er automatisch reserviert |
| `data-open`     | `open`          | `true` = Fenster sofort öffnen |
| `data-css`      | `css`           | `false` = CSS nicht automatisch laden |
| `data-server`   | `server`        | Server-URL, falls abweichend von der Script-Quelle |
| `data-manual`   | –               | `true` = nicht automatisch starten |
| `data-widget`   | –               | ID eines gespeicherten Widgets (`w_…`) |
| `data-greeting` | `greeting`      | Begrüßungstext |
| `data-hours`    | `hours`         | `true` = Öffnungszeiten anzeigen |
| `data-lang`     | `lang`          | Sprache des Widgets, z. B. `en` (Standard: Sprache des Browsers bzw. Standardsprache der Instanz) |

`UCNG.init()` gibt ein Objekt mit `open()`, `close()`, `toggle()`, `setName(name)` (reserviert bzw. benennt um) und `destroy()` zurück. Die per `data-`Attributen gestartete Instanz liegt in `UCNG.instance`.

CSS-Variablen auf `.gochat-root`: `--gochat-accent`, `--gochat-on-accent`, `--gochat-surface`, `--gochat-canvas`, `--gochat-text`, `--gochat-muted`, `--gochat-line`, `--gochat-bubble`, `--gochat-radius`, `--gochat-font`, `--gochat-z`. Alle Klassen beginnen mit `gochat-`, damit es keine Konflikte mit dem CSS der Host-Seite gibt. Die Grundstile des Widgets haben die Spezifität 0 (`:where()`), sodass deine eigenen Regeln sie problemlos überschreiben.

## Eigene Schrift

Weboberfläche und Widget verwenden die Systemschrift des Geräts – das lädt nichts von Drittservern nach (wichtig wegen DSGVO; Google Fonts direkt einzubinden gilt in Deutschland als problematisch). Für eine eigene Schrift:

1. `.woff2`-Datei nach `web/fonts/` legen (wird automatisch mit eingebettet).
2. Am Anfang von `web/app.css` ergänzen:
   ```css
   @font-face { font-family: "MeineSchrift"; src: url("/fonts/MeineSchrift.woff2") format("woff2"); font-weight: 100 900; font-display: swap; }
   :root { --font: "MeineSchrift", system-ui, sans-serif; --font-display: var(--font); }
   ```
3. Für das Widget dasselbe `@font-face` in `web/embed/chat.css` (mit absoluter URL deines Servers) und `--gochat-font` setzen.
4. Neu bauen (`go build`) – die Web-Dateien stecken in der Binärdatei.

## Bots

Bots sind Programme, die mitlesen und schreiben – z. B. für Begrüßungen, Hilfe-Befehle, FAQ-Antworten oder Benachrichtigungen aus anderen Systemen. Du legst sie im Admin-Bereich unter **Bots** an:

- **Name** – erscheint an jeder Nachricht mit dem Abzeichen „Bot“. Der Name ist reserviert: Keine Person kann ihn wählen, und ein Bot kann keinen Namen bekommen, den eine Person schon nutzt.
- **Kanäle** – „Alle Kanäle“ (auch künftige) oder eine Auswahl. Ein Bot liest und schreibt nur dort. Wird ein Kanal umbenannt, wandert die Berechtigung mit; wird er gelöscht, fällt sie weg. Bots legen keine Kanäle an.
- **Webhook-URL** (optional) – siehe unten.
- **Nachrichten anderer Bots empfangen** – standardmäßig aus, damit sich zwei Bots nicht endlos gegenseitig antworten.
- **Rechte** – fein einstellbar in drei Gruppen:

  | Gruppe | Recht | Bedeutung |
  |--------|-------|-----------|
  | Empfangen | Alle Nachrichten / Nur Befehle und Ansprache | Mit „Nur Befehle“ bekommt der Bot nur Nachrichten, die mit `!` oder `/` beginnen, ihn mit `@Name` erwähnen oder ihm antworten – über Live-Verbindung und Webhook. Schont Datenschutz und Datenverkehr. |
  | | Auch Nachrichten anderer Bots | standardmäßig aus (verhindert Bot-Schleifen) |
  | Lesen und schreiben | Nachrichten schreiben | aus = reiner Lese-Bot (z. B. für Archiv oder Statistik) |
  | | Reagieren | Reaktionen setzen |
  | | Eigene Nachrichten bearbeiten und löschen | ohne 15-Minuten-Grenze, z. B. für Statusmeldungen |
  | | Von der Automoderation ausgenommen | für vertrauenswürdige Bots, deren Texte sonst Fehlalarme auslösen |
  | | Verlauf abrufen / Online-Liste abrufen | Zugriff auf ältere Nachrichten bzw. wer gerade da ist |
  | | Höchstens … Nachrichten pro 10 Sekunden | eigenes Limit; 0 = `BOT_RATE` |
  | Moderieren | Nachrichten moderativ löschen | wie ein Moderator, Original bleibt für Admins |
  | | Stummschalten / zeitlich ausschließen | bis zur eingestellten Höchstdauer (10 Minuten bis 7 Tage) |

  Bots können Team-Mitglieder nicht sperren, keine dauerhaften Sperren verhängen, eine strengere bestehende Sperre nicht verkürzen und nur Sperren aufheben, die sie selbst verhängt haben. Ein Bot, der nicht alle Kanäle sieht, sperrt nur Personen, die in seinen Kanälen geschrieben haben. Jede Moderationsaktion steht im Protokoll. Bots aus älteren Versionen behalten ihre bisherigen Rechte.
- **Aktiv** – ein deaktivierter Bot kann sich nicht anmelden; offene Verbindungen werden innerhalb weniger Sekunden getrennt.

Beim Anlegen erscheint der **API-Schlüssel** (`gcb_…`) genau einmal, zusammen mit einem fertigen `curl`-Befehl zum Ausprobieren. „Neuen API-Schlüssel erzeugen“ macht den alten Schlüssel und das alte Webhook-Geheimnis sofort ungültig. Die Bot-Liste zeigt live, ob ein Bot verbunden ist, wann er zuletzt aktiv war und wie der letzte Webhook-Aufruf ausging.

Jeder Bot darf standardmäßig 30 Nachrichten pro 10 Sekunden schreiben (`BOT_RATE`). Bot-Nachrichten durchlaufen wie alle anderen die Automoderation.

### Bot-API

Alle Aufrufe mit `Authorization: Bearer gcb_…`:

| Methode | Pfad                                   | Beschreibung |
|---------|----------------------------------------|--------------|
| `GET`   | `/api/bot/me`                          | eigener Bot: `{"id","name","allRooms","rooms","perms"}` |
| `GET`   | `/api/bot/rooms`                       | erlaubte Kanäle mit Beschreibung, Online-Zahl und Regeln |
| `GET`   | `/api/bot/rooms/{room}/messages`       | **Verlauf abrufen:** seitenweise wie bei Nutzern (`?before=&limit=`, Header `X-Has-More`) |
| `POST`  | `/api/bot/rooms/{room}/messages`       | **Schreiben:** `{"text":"…","replyTo":123,"thread":456,"gif":"lib:g_…"}` → `201` mit der Nachricht (`gif`, `thread` optional; antwortet der Bot auf eine Thread-Nachricht, landet die Antwort automatisch im Thread). Direktnachrichten sind für Bots gesperrt. |
| `PATCH` | `/api/bot/rooms/{room}/messages/{id}`  | **Eigene bearbeiten:** `{"text":"…"}` |
| `DELETE`| `/api/bot/rooms/{room}/messages/{id}`  | eigene Nachricht löschen (**Eigene bearbeiten**) oder fremde moderativ löschen (**Löschen**) |
| `POST`  | `/api/bot/rooms/{room}/messages/{id}/react` | **Reagieren:** `{"emoji":"👍"}` (umschalten) |
| `GET`   | `/api/bot/rooms/{room}/presence`       | **Online-Liste:** `{"users":[…],"guests":2,"online":5}` |
| `POST`  | `/api/bot/users/{userId}/mute`         | **Stummschalten:** `{"minutes":10,"reason":"…"}` – die `userId` steht an jeder Nachricht |
| `POST`  | `/api/bot/users/{userId}/ban`          | **Ausschließen:** wie oben, zeitlich begrenzt |
| `DELETE`| `/api/bot/users/{userId}/sanction`     | eigene Sperre aufheben |
| `GET`   | `/api/bot/events`                      | Live-Stream (Server-Sent Events), siehe unten |

Fehler: `401` falscher oder deaktivierter Schlüssel, `403` Kanal nicht freigegeben, fehlendes Recht oder Link-Regel, `404` Kanal oder Nachricht gibt es nicht, `429` zu viele Nachrichten oder Langsam-Modus.

### Weg 1: Live-Verbindung (empfohlen)

Der Bot öffnet `GET /api/bot/events` und hält die Verbindung offen. Er braucht dafür keine öffentliche Adresse – das funktioniert auch auf dem eigenen Rechner oder hinter einer Firewall.

```
event: ready
data: {"event":"ready","bot":{"id":"bot_…","name":"Helferlein","allRooms":true,"rooms":[]}}

event: message
data: {"event":"message","room":"lobby","message":{"id":42,"room":"lobby","userId":"…","name":"Anna","text":"!ping","time":"…"}}
```

- `message` – neue Nachricht in einem erlaubten Kanal. Die eigenen Nachrichten des Bots kommen nicht an (und die anderer Bots nur, wenn eingeschaltet).
- `update` – eine Nachricht wurde geändert, z. B. moderiert (`deleted: true`), bearbeitet oder hat neue Reaktionen.
- `removed` – eine Nachricht wurde endgültig gelöscht (`message.id`).
- `revoked` – der Bot wurde deaktiviert oder gelöscht; danach endet der Stream.
- Alle 25 Sekunden kommt ein Kommentar (`: ping`), damit Proxys die Verbindung offen halten. Bricht sie ab, einfach neu verbinden.

### Weg 2: Webhook

Trägst du eine Webhook-URL ein, schickt der Chat jede neue Nachricht aus den erlaubten Kanälen per `POST` dorthin – im selben JSON-Format wie oben. Antwortet der Bot mit `Content-Type: application/json` und

```json
{"text": "Wir sind Mo–Fr von 9 bis 18 Uhr für dich da."}
```

erscheint das als Bot-Nachricht im selben Kanal (optional mit `"room": "anderer-kanal"` oder `"replyTo": <id>` als Antwort). Dafür braucht der Bot das Recht „Nachrichten schreiben“. Eine leere Antwort oder `204` bedeutet: nichts sagen.

Jeder Aufruf ist **signiert**, damit der Bot prüfen kann, dass er wirklich vom Chat kommt:

```
X-UCNG-Event:     message        (bzw. ping beim Testknopf)
X-UCNG-Timestamp: 1760018400     (Unix-Sekunden)
X-UCNG-Signature: sha256=<hex(HMAC-SHA256(Webhook-Geheimnis, Timestamp + "." + Body))>
```

Der Bot sollte die Signatur mit konstanter Laufzeit vergleichen und Aufrufe mit einem Zeitstempel älter als ein paar Minuten ablehnen. Nachrichten an einen Bot werden der Reihe nach zugestellt; bei Netzwerkfehlern oder `5xx` gibt es einen zweiten Versuch, Zeitlimit sind 8 Sekunden. Mit **Webhook testen** im Admin-Bereich schickst du ein `ping` und siehst sofort, ob der Bot erreichbar ist.

### Beispiel-Bots

Im Ordner `examples/` liegen zwei lauffähige Bots (nur Go-Standardbibliothek):

```bash
# Live-Verbindung: beantwortet !hilfe, !ping, !zeit und !würfel
UCNG_URL=http://localhost:8080 UCNG_BOT_TOKEN=gcb_… go run ./examples/bot-stream

# Webhook: beantwortet !ping und Fragen nach Öffnungszeiten
UCNG_WEBHOOK_SECRET=… PORT=9000 go run ./examples/bot-webhook
# im Admin als Webhook-URL eintragen: http://<rechner>:9000/hook
```

Schnelltest ohne Programm:

```bash
curl -N http://localhost:8080/api/bot/events -H "Authorization: Bearer gcb_…"      # mitlesen
curl -X POST http://localhost:8080/api/bot/rooms/lobby/messages \
     -H "Authorization: Bearer gcb_…" -H "Content-Type: application/json" \
     -d '{"text":"Hallo, ich bin ein Bot!"}'                                        # schreiben
```

## HTTP-API

| Methode | Pfad                                | Beschreibung |
|---------|-------------------------------------|--------------|
| `GET`   | `/api/settings`                     | Einstellungen (Titel, Farben, Links, Aufbewahrung …) plus `reactions` und `editMinutes` |
| `GET`   | `/theme.css`                        | Farben als CSS-Variablen (für eine Vorschau mit `?accent=&sidebar=&mode=`) |
| `GET`   | `/api/hours`                        | Öffnungszeiten jetzt: `{"enabled","open","today","next","message"}` |
| `GET`   | `/api/widgets/{id}`                 | gespeichertes Widget (ohne Domains) |
| `POST`  | `/api/session`                      | Namen reservieren: `{"name":"Anna"}` → `{"token","name","id","viewer","team","sanction"}`; mit Token = umbenennen; `409` vergeben oder geschützt |
| `GET`   | `/api/session`                      | Token prüfen → wie oben ohne `token`, oder `401` |
| `DELETE`| `/api/session`                      | Namen freigeben |
| `POST`  | `/api/unread`                       | `{"since":{"lobby":120}}` → `{"lobby":{"unread":3,"mentions":1}}` (mit Token ohne eigene Nachrichten, mit Erwähnungen) |
| `GET`   | `/api/rooms`                        | Kanäle mit Beschreibung, Online-Zahl, Nachrichtenanzahl, `lastId`, `slowMode`, `links` |
| `GET`   | `/api/rooms/{room}/messages?before=<id>&limit=<n>` | Verlauf seitenweise (Standard 100, max. 500), älteste zuerst, Header `X-Has-More` |
| `POST`  | `/api/rooms/{room}/messages`        | Senden: `{"text":"Hallo","replyTo":123,"thread":456,"gif":"lib:g_…"}` (`thread`: Antwort im Thread; `gif` optional: `lib:<id>`, `giphy:<id>`, `tenor:<id>`, `beacon:<…>` aus `/api/gifs/beacon`; mit GIF darf `text` leer sein). `201` mit der Nachricht – bei einem privaten Befehl stattdessen `200` mit `{"ephemeral":{"name","text","command","time","notice"}}` mit `Authorization: Bearer <token>`; `403` gesperrt/Link-Regel/Nur-lesen-Kanal, `404` privater Kanal ohne Zugang, `429` Langsam-Modus/Wiederholung. Mit `Authorization` sehen Verlauf, Suche, Online-Liste und `/api/rooms` auch private Kanäle der eigenen Rollen. |
| `PATCH` | `/api/rooms/{room}/messages/{id}`   | eigene Nachricht bearbeiten: `{"text":"…"}` (15 Minuten) |
| `DELETE`| `/api/rooms/{room}/messages/{id}`   | eigene Nachricht löschen |
| `POST`  | `/api/rooms/{room}/messages/{id}/react` | Reaktion umschalten: `{"emoji":"👍"}` oder ein Emote `{"emoji":":party:"}` |
| `POST`  | `/api/rooms/{room}/messages/{id}/vote` | Umfrage: `{"option":1}` (umschalten), `{"close":true}` / `{"reopen":true}` |
| `POST`  | `/api/rooms/{room}/messages/{id}/pin` | `{"pin":true}` – Recht „anpinnen“ |
| `POST`  | `/api/rooms/{room}/messages/{id}/report` | Melden: `{"reason":"…"}` |
| `DELETE`| `/api/rooms/{room}/messages/{id}/mod` | fremde Nachricht löschen – Recht „löschen“ |
| `POST`  | `/api/users/{id}/mute`              | `{"minutes":15,"reason":"…"}` (1–60) – Recht „stummschalten“ |
| `GET`   | `/api/rooms/{room}/threads/{id}`    | Thread: `{"root":{…},"messages":[…]}` |
| `POST`  | `/api/rooms/{room}/upload`          | Bild: `multipart/form-data` mit `file`, optional `text`, `replyTo`, `thread` |
| `GET`/`POST` | `/api/dms`                     | eigene Direktnachrichten `{"enabled","dms":[{"room","with","name","lastId"}]}` / öffnen `{"userId":"…"}` |
| `GET`   | `/api/profiles`                     | Profilbilder und Status: `{"<userId>":{"avatar","status","text"}}` |
| `POST`/`DELETE` | `/api/profile/avatar`       | eigenes Profilbild (`file`) setzen / entfernen |
| `PUT`   | `/api/profile/status`               | `{"status":"away"|"busy"|"","text":"…"}` |
| `GET`   | `/api/rooms/{room}/search?q=…`      | sichtbare Nachrichten durchsuchen (neueste 50) |
| `GET`   | `/api/rooms/{room}/presence`        | Online-Liste: `{"users":[…],"guests":2,"online":5}` |
| `GET`   | `/api/rooms/{room}/events?viewer=…` | SSE-Stream: `history`, `message`, `update`, `removed`, `presence`, `room`, `room_renamed`, `room_deleted`, `settings`, `user_renamed`, `user_updated`, `sanction`, `media` (Emotes/Reaktionen geändert), `roles` (Rollen geändert), `pins` (angepinnte Nachrichten), `profiles` (Profilbild/Status geändert), `dm` (neue Direktnachricht für `user`). `history` enthält zusätzlich `pinned`. Mit `viewer` (aus der Session) erscheint man mit Namen in der Online-Liste |
| `GET`   | `/api/media`                        | Emotes (`name`, `url`, `size`, `animated`), Reaktionen (`reactions`, `emoteReactions`), ob GIFs an sind, Websuche (`web`: `giphy`/`tenor`/leer), GIF-Breite |
| `GET`   | `/api/gifs?q=…`                     | GIF-Bibliothek durchsuchen |
| `GET`   | `/api/gifs/web?q=…`                 | Websuche über den Server (ohne `q`: Trends) → `[{"id":"giphy:…","title","w","h","preview"}]` |
| `GET`   | `/api/groups`                       | Kanal-Gruppen in Reihenfolge `[{"id","name"}]`; `/api/rooms` liefert je Kanal `group`, `pos` und aktive `threads` (`[{"id","title","count","lastAt"}]`) |
| `PUT`   | `/api/profile/pins`                 | angepinnte Kanäle `{"rooms":["lobby",…]}` (stehen auch in `/api/session` als `pinned`) |
| `GET`   | `/api/i18n/boot.js?scope=app\|admin\|widget` | Texte der Oberfläche als `window.I18N` (Sprache aus Cookie, Browser oder Standard) |
| `GET`   | `/api/i18n/dict?scope=widget&lang=en` | dasselbe als JSON (für das Widget, CORS) |
| `GET`   | `/api/i18n/languages`               | angebotene Sprachen |
| `GET`   | `/api/roles`                        | Rollen und wer sie trägt: `{"roles":[{"id","name","color","colorName","mentionable","perms"}],"users":{"<userId>":["r_…"]}}` |
| `GET`   | `/api/commands?room=…`              | verfügbare Befehle (Zeichen, Bot-Name, Auslöser, Beschreibung) |
| `GET`   | `/media/emotes/…`, `/media/gifs/…`  | skalierte Bilder (lange cachebar) |
| `GET`   | `/healthz`                          | Health-Check |

Nachrichten enthalten neben `id`, `name`, `text`, `time` je nach Fall `userId`, `bot`, `team`, `replyTo` und `reply` (Vorschau der beantworteten Nachricht), `editedAt`, `reactions` (`[{"emoji","users":[{"id","name"}]}]`), `deleted` und `deletedBy` (`mod`, `automod`, `bot`, `self`) sowie `media` bei GIFs und Bildern (`{"kind":"gif"|"image","src","w","h","title","source"}`), `poll` (`{"question","options":[{"text","votes":[userIds]}],"multi","closed"}`), `thread` (ID der Ausgangsnachricht) und an Ausgangsnachrichten `threadInfo` (`{"count","lastAt","names"}`).

Im Modus „hosted“ zusätzlich (sonst `404`); `/api/settings` enthält dann `appMode` und `hosted` (`guests`, `registration`, `email`, `requireVerify`, `federation`, `domain`):

| Methode | Pfad                                | Beschreibung |
|---------|-------------------------------------|--------------|
| `POST`  | `/api/account/prelogin`             | `{"login"}` → `{"kdfSalt","kdfIter"}` für die Schlüsselableitung im Browser (für unbekannte Namen gleichbleibend erfunden) |
| `POST`  | `/api/account/register`             | `{"name","email","invite","keys":{"auth","kdfSalt","kdfIter","pubKey","privKey"}}` → `{"recovery":[…],"verify"}` und ohne Bestätigungspflicht `session` |
| `POST`  | `/api/account/login`                | `{"login","auth","code"|"recovery"}` → `{"session":{…,"token"},"pubKey","privKey"}`; `401` mit `need2fa`, `403` mit `unverified` |
| `GET`   | `/api/account/verify?t=…`           | Link aus der Bestätigungs-E-Mail |
| `POST`  | `/api/account/resend`, `/api/account/forgot` | Bestätigung bzw. Reset-Link erneut senden (Antwort immer gleich) |
| `GET`/`POST` | `/api/account/reset`           | Reset-Link prüfen (`?t=`) / neues Passwort: `{"ticket"|"login"+"recovery","code","keys"}` |
| `GET`   | `/api/account`                      | eigenes Konto, Sitzungen, verschlüsselter privater Schlüssel |
| `POST`  | `/api/account/password`, `/email`, `/keys`, `/federation`, `/delete` | Konto ändern (jeweils mit `auth` des aktuellen Passworts) |
| `POST`  | `/api/account/2fa/begin`, `/enable`, `/disable`, `/api/account/recovery` | 2FA einrichten (`{"secret","uri","svg"}`), aktivieren, abschalten; neue Wiederherstellungscodes |
| `DELETE`| `/api/account/sessions/{id}`        | Sitzung beenden (`others` = alle anderen) |
| `GET`   | `/api/dms/{room}/key`               | öffentlicher Schlüssel des Gegenübers (`e2e`, `fed`, `pubKey`) |
| `POST`  | `/api/dms`                          | zusätzlich `{"address":"name@instanz"}` (oder nur `name` auf dieser Instanz) |
| `POST`/`PATCH` | `/api/rooms/{room}/messages[/{id}]` | in verschlüsselten Unterhaltungen `{"e2e":"{\"v\":1,\"iv\",\"ct\",\"spk\",\"rpk\"}"}` statt `text` |
| `GET`   | `/api/fed/lookup?addr=…`            | Schlüssel eines Kontos auf einer anderen Instanz |
| `GET`   | `/.well-known/ucng`, `/api/fed/users/{name}` | für andere Instanzen: Serverschlüssel, öffentlicher Schlüssel eines Kontos |
| `POST`  | `/api/fed/inbox`                    | signierter Eingang von anderen Instanzen (`X-UCNG-Domain`, `X-UCNG-Date`, `X-UCNG-Signature`) |

Admin-Endpunkte (alle außer Login mit `Authorization: Bearer <token>`; **M** = auch Moderatoren):

| Methode | Pfad                                         | Beschreibung |
|---------|----------------------------------------------|--------------|
| `POST`  | `/api/admin/login`                           | `{"username":"mia","password":"…"}` (ohne `username`: Haupt-Admin) → `{"token","actor"}` |
| `POST`  | `/api/admin/logout`                          | Sitzung beenden |
| `GET`   | `/api/admin/me`                              | **M** angemeldetes Konto und Rolle |
| `GET`/`PUT` | `/api/admin/settings`                    | Einstellungen lesen (**M**) / speichern – immer das vollständige Objekt schicken |
| `POST`  | `/api/admin/rooms`                           | Kanal anlegen: `{"name","description","slowMode","links","botSlowMode","botLinks"}` (`botLinks`: `all` oder `none`) |
| `PATCH` | `/api/admin/rooms/{room}`                    | ändern/umbenennen (fehlende Regeln bleiben unverändert) |
| `GET`/`PUT` | `/api/admin/rooms/{room}/automod`        | **M** Automod des Kanals: `{"mode":"global"|"both"|"own"|"off","patterns":[…]}` |
| `DELETE`| `/api/admin/rooms/{room}`                    | Kanal mit Verlauf löschen |
| `GET`   | `/api/admin/rooms/{room}/messages`           | **M** Verlauf inkl. `original`, `modBy`, `edits` |
| `DELETE`| `/api/admin/rooms/{room}/messages/{id}`      | **M** moderativ löschen |
| `POST`  | `/api/admin/rooms/{room}/messages/{id}/restore` | **M** wiederherstellen bzw. freischalten |
| `POST`  | `/api/admin/rooms/{room}/messages/{id}/confirm` | **M** Automod-Treffer ausgeblendet lassen |
| `POST`  | `/api/admin/rooms/{room}/messages/{id}/purge`   | endgültig löschen |
| `GET`   | `/api/admin/search?q=…&room=…`               | **M** Suche in allen Kanälen inkl. Originaltexten |
| `GET`/`PUT` | `/api/admin/automod`                     | **M** Regeln |
| `POST`  | `/api/admin/automod/test`                    | **M** Text testen: `{"text":"…","room":"…"}` (mit `room` so, wie es in diesem Kanal gilt) |
| `GET`   | `/api/admin/automod/queue`                   | **M** ungeprüfte Treffer |
| `GET`   | `/api/admin/users?q=…`                       | **M** reservierte Namen (Header `X-Total`) |
| `GET`   | `/api/admin/users/{id}`                      | **M** Person mit Sperre |
| `POST`  | `/api/admin/users/{id}/sanction`             | **M** `{"kind":"mute"|"ban","minutes":60,"reason":"…","ip":true}` (`minutes: 0` = dauerhaft, nur `ban`) |
| `DELETE`| `/api/admin/users/{id}/sanction`             | **M** Sperre aufheben |
| `GET`   | `/api/admin/sanctions`                       | **M** aktive Sperren |
| `POST`  | `/api/admin/users/{id}/team`                 | `{"team":true}` |
| `POST`  | `/api/admin/users/{id}/roles`                | Rollen setzen: `{"roles":["r_…"]}` |
| `GET`/`PUT` | `/api/admin/roles`                       | Rollen lesen (**M**, mit Anzahl je Rolle) / alle in Reihenfolge speichern `{"roles":[{"id","name","color","colorName"}]}` (ohne `id` = neu) |
| `POST`  | `/api/admin/users/{id}/purge`                | alle Nachrichten endgültig löschen: `{"releaseName":true}` |
| `GET`/`PUT` | `/api/admin/protected-names`             | geschützte Namen (`GET` **M**) |
| `GET`   | `/api/admin/audit?q=…&before=<cursor>`       | **M** Protokoll (100 je Seite, `next` = Cursor) |
| `GET`   | `/api/admin/stats?days=30`                   | **M** Statistik |
| `GET`/`POST` | `/api/admin/accounts`                   | Konten auflisten / anlegen `{"name","display","role","password"}` |
| `PATCH`/`DELETE` | `/api/admin/accounts/{name}`        | Konto ändern (`password` leer = unverändert, `disabled`) / löschen |
| `GET`/`PUT` | `/api/admin/layout`                      | Gruppen und Reihenfolge (**M** lesen): `{"groups":[{"id","name","rooms":[…]}],"ungrouped":[…]}` – Gruppen ohne `id` werden angelegt, fehlende gelöscht; SSE-Ereignis `layout` |
| `GET`/`PUT` | `/api/admin/hosted`                      | Modus „hosted“: Gäste, Registrierung, SMTP, Föderation (`GET` zeigt auch Betriebsart und Speicher) |
| `POST`  | `/api/admin/hosted/testmail`                 | Test-E-Mail `{"to"}` |
| `POST`  | `/api/admin/invites`                         | Einladungslink `{"note"}` → `{"link"}` |
| `POST`  | `/api/admin/users/{id}/account`              | **M** `{"action":"disable"|"enable"|"reset2fa"|"verify"|"logout"|"resetmail"}` |
| `GET`   | `/api/admin/federation`                      | Domain, Schlüssel-Fingerabdruck, bekannte Instanzen, Warteschlange |
| `DELETE`| `/api/admin/federation/peers/{domain}`       | gemerkten Serverschlüssel einer Instanz vergessen |
| `GET`/`POST` | `/api/admin/backups`                    | Backups auflisten / sofort anlegen |
| `GET`   | `/api/admin/backups/{name}`                  | Backup herunterladen |
| `GET`   | `/api/admin/i18n`                            | Sprachen mit Vollständigkeit, Standardsprache, Browsersprache an/aus |
| `PUT`   | `/api/admin/i18n/settings`                   | `{"default":"en","auto":true,"hidden":["fr"]}` |
| `POST`/`PATCH`/`DELETE` | `/api/admin/i18n/languages[/{code}]` | eigene Sprache anlegen `{"code","name","locale","dir"}` / ändern / löschen |
| `GET`/`PUT` | `/api/admin/i18n/texts/{code}`           | alle Texte mit Original, Änderung, Zustand und Platzhaltern / Text setzen `{"key","text"}` (leer = zurücksetzen, `"confirm":true` = veraltete Änderung bestätigen) |
| `GET`/`POST` | `/api/admin/i18n/file/{code}`           | Sprachdatei herunterladen (`?full=0`: nur Änderungen) / importieren (`?replace=1`; auch ausgefüllte Übersetzungspakete) |
| `GET`   | `/api/admin/i18n/package/{code}`             | Übersetzungspaket (`?open=1`: nur fehlende und veraltete Texte) |
| `GET`   | `/api/admin/i18n/beacon?url=…`               | Sprachpakete eines Beacons mit Zustand hier (installiert, Update verfügbar) |
| `POST`  | `/api/admin/i18n/beacon/install`             | `{"url","code","updated"}` – Sprachpaket installieren bzw. aktualisieren |
| `GET`/`POST` | `/api/admin/widgets`                    | Widgets auflisten / anlegen |
| `PUT`/`DELETE` | `/api/admin/widgets/{id}`             | Widget ändern / löschen |
| `GET`/`POST` | `/api/admin/bots`                       | Bots mit Rechten: `{"name",…,"perms":{"receive","write","react","editOwn","automodExempt","history","presence","rateLimit","delete","mute","ban","maxMinutes"}}` |
| `PATCH`/`DELETE` | `/api/admin/bots/{id}`              | Bot ändern / löschen |
| `POST`  | `/api/admin/bots/{id}/renew`                 | neuer API-Schlüssel und Webhook-Geheimnis |
| `POST`  | `/api/admin/bots/{id}/test`                  | `ping` an die Webhook-URL |
| `GET`   | `/api/admin/media`                           | Emotes, GIF-Bibliothek und Einstellungen (Schlüssel nur als `hasKey`) |
| `PUT`   | `/api/admin/reactions`                       | `{"reactions":["👍",":party:"],"emoteReactions":true}` |
| `GET`/`PUT` | `/api/admin/uploads`                     | Bilder: `{"mode":"off"|"known"|"all"|"roles","size":480,"avatars":true}` |
| `GET`/`PUT` | `/api/admin/filter`                      | **M** Wortfilter `{"patterns":[…]}` |
| `GET`   | `/api/admin/reports?status=open|done`        | **M** Meldungen (je Nachricht zusammengefasst, mit Nachricht) |
| `GET`   | `/api/admin/reports/count`                   | **M** `{"open":3}` |
| `POST`  | `/api/admin/reports/{id}`                    | **M** `{"action":"dismiss"|"delete"}` |
| `GET`   | `/api/admin/rooms`                           | **M** alle Kanäle (auch private, ohne Direktnachrichten) |
| `GET`   | `/api/admin/rooms/{room}/events?t=<token>`   | **M** Live-Stream für die Moderation (auch private Kanäle) |
| `GET`   | `/api/admin/rooms/{room}/pins`               | **M** angepinnte Nachrichten |
| `POST`  | `/api/admin/rooms/{room}/messages/{id}/pin`  | **M** `{"pin":true}` |
| `DELETE`| `/api/admin/users/{id}/avatar`               | **M** Profilbild und Statustext entfernen |
| `GET`/`POST` | `/api/admin/schedules`                  | Ankündigungen auflisten / anlegen `{"room","text","mode":"once"|"daily"|"weekly","at":"2026-10-12T20:00","time":"20:00","weekdays":[4],"pin":true,"enabled":true}` |
| `PUT`/`DELETE` | `/api/admin/schedules/{id}`           | ändern / löschen |
| `POST`  | `/api/admin/schedules/{id}/run`              | jetzt senden |
| `PUT`   | `/api/admin/media/settings`                  | `{"emoteSize":32,"gifsEnabled":true,"gifWidth":300,"provider":"giphy","rating":"g","apiKey":"…"}` (`apiKey` weglassen = unverändert) |
| `POST`  | `/api/admin/emotes`                          | Emote hinzufügen: `multipart/form-data` mit `file`, `name`, `size` – oder JSON `{"url","name","size"}` |
| `PATCH`/`DELETE` | `/api/admin/emotes/{name}`          | `{"name":"neu","size":64}` / löschen |
| `POST`  | `/api/admin/gifs`                            | GIF hinzufügen: `file` + `title`, `tags` – oder JSON `{"url","title","tags"}` |
| `PATCH`/`DELETE` | `/api/admin/gifs/{id}`              | `{"title","tags":[…]}` / löschen |
| `GET`/`PUT` | `/api/admin/commands`                    | Befehls-Bot lesen / vollständig speichern |
| `POST`  | `/api/admin/commands/test`                   | `{"text":"!würfel","room":"lobby","team":false}` → `{"matched","text","reply","note"}` |

Beispiel:

```bash
TOKEN=$(curl -s -X POST localhost:8080/api/session -d '{"name":"Bot"}' | sed 's/.*"token":"\([^"]*\)".*/\1/')
curl -X POST localhost:8080/api/rooms/lobby/messages \
     -H "Authorization: Bearer $TOKEN" \
     -d '{"text":"Hallo aus curl"}'
```

## Projektstruktur

```
main.go          HTTP-Server, Routen, CORS, Konfiguration, Hintergrundaufgaben
api.go           Chat-API: Session, Senden, Bearbeiten, Reaktionen, Suche, Ungelesen, SSE
hub.go           Kanäle, Verlauf, Broadcast, Spam-Bremsen beim Senden
features.go      Reaktionen, Bearbeiten, Löschen, endgültiges Löschen, Aufbewahrung, Suche, Statistik
settings.go      Einstellungen, Prüfung, Zeitzone, Öffnungszeiten
users.go         Namensreservierung per Token, feste Nutzer-IDs, Team-Abzeichen, Rollen
roles.go         Rollen-Abzeichen (Anzeige)
rolesapi.go      Endpunkte für Rollen
access.go        Zugriff auf Kanäle (private Kanäle, Nur-lesen, Direktnachrichten)
chatmod.go       Moderation im Chat per Rollen-Recht, Melden, Admin-Endpunkte für Meldungen
reports.go       Meldungen
pins.go          Angepinnte Nachrichten
polls.go         Umfragen
threads.go       Threads
layout.go        Kanal-Gruppen, Reihenfolge, aktive Threads in der Kanalliste, angepinnte Kanäle
schedules.go     Geplante Ankündigungen
filter.go        Wortfilter (ersetzen statt ausblenden)
uploads.go       Bilder hochladen, Profilbilder verarbeiten, Aufräumen
profile.go       Profilbild und Status
dms.go           Direktnachrichten
accounts.go      Admin- und Moderatoren-Konten, Passwörter, Sitzungen, Rollen
admin.go         Admin-Endpunkte (mit Protokolleinträgen)
audit.go         Protokoll
moderation.go    Sperren, geschützte Namen, Langsam-Modus, Wiederholungen
widgets.go       gespeicherte Widgets und erlaubte Domains
backups.go       tägliche Backups, Download
theme.go         Farbgestaltung: Farbrechnung, Kontrast, /theme.css
bots.go          Bot-Verwaltung: Namen, Schlüssel, Kanäle, Rechte
botapi.go        Bot-API und Admin-Endpunkte für Bots
webhook.go       Webhook-Versand mit Signatur, Antworten, Wiederholung
commands.go      Eingebauter Befehls-Bot
media.go         Emotes, GIF-Bibliothek, GIPHY/Tenor über den Server
mediaapi.go      Endpunkte für Medien und Befehle
imgscale.go      Anbindung an internal/imgscale (Bildverarbeitung selbst dort)
beacon.go        Beacon: Anmeldung im Verzeichnis (alle 6 h), GIF-Sammlungen abonnieren
templates.go     Vorlagen: Grundkonfiguration exportieren/importieren
i18n.go          Sprachen: Katalog, Sprache je Anfrage, übersetzte Fehlermeldungen
i18npacks.go     Sprachpakete von einem Beacon abrufen und installieren
automod.go       Automoderation: Muster, Vereinheitlichung, schnelle Prüfung
store.go         Speicherung in einzelnen Dateien, Laden, Aufräumen, Umstieg vom alten Format
backend.go       Speicher-Schnittstelle (Dateien, SQLite, MySQL), Auswahl per STORAGE
storage_sql.go   Speicherung in SQLite/MySQL (database/sql), Übernahme aus Dateien, Export für Backups
db_sqlite.go     SQLite-Treiber modernc.org/sqlite (nur mit -tags sqlite)
db_mysql.go      MySQL-Treiber go-sql-driver/mysql (nur mit -tags mysql)
persist.go       Zeitnahes, gebündeltes Speichern (stößt das Backend an)
mode.go          Betriebsarten simple/hosted
members.go       Nutzerkonten (hosted): Anmeldeschlüssel, Sitzungen, TOTP, Wiederherstellungscodes, Tickets
account_api.go   Endpunkte für Konten, Gäste-Regel, Einladungen, Admin-Aktionen
hosted.go        Einstellungen des Modus hosted (Gäste, Registrierung, SMTP, Föderation)
mailer.go        E-Mail-Versand per SMTP (STARTTLS/TLS)
qr.go            QR-Code-Erzeuger (SVG) für die 2FA-Einrichtung
e2e.go           Ende-zu-Ende-verschlüsselte Direktnachrichten (Server-Seite)
federation.go    Föderation: Serverschlüssel, signierter Eingang, Warteschlange, TOFU
*_test.go        Tests
LICENSE          GNU AGPL v3 (unverändert)
NOTICE           Projektinhaber, Lizenzhinweis, Fremdinhalte
CLA.md           Contributor License Agreement
CONTRIBUTING.md  Hinweise für Beiträge
TRADEMARKS.md    Regeln für Name und Logo
.github/
  workflows/     CI (ci.yml), Veröffentlichung (release.yml), CLA-Assistent (cla.yml)
  pull_request_template.md  Checkliste für Pull Requests
  dependabot.yml Automatische Update-PRs
internal/
  imgscale/      Bilder skalieren (auch animierte GIFs) – nur Standardbibliothek
  fedsig/        Ed25519-Signaturen für Föderation und Beacon
  i18n/          Sprachdateien, eigene Änderungen, Platzhalter/Mehrzahl, Server-Meldungen, Übersetzungspakete, HTTP-Schnittstelle
cmd/
  ucng-beacon/   Beacon-Programm: Verzeichnis, GIF-Hub, Verwaltung (Web-Oberfläche eingebettet)
assets/          Logo, Schriftzug, Social-Preview (für README und GitHub)
examples/
  bot-stream/    Beispiel-Bot mit Live-Verbindung
  bot-webhook/   Beispiel-Bot per Webhook
web/
  index.html     Weboberfläche
  app.css/js
  account.js     Konten, 2FA und Ende-zu-Ende-Verschlüsselung im Browser (WebCrypto)
  admin.html     Verwaltung
  admin.css/js
  demo.html      Demo-Seite für das Widget
  logo.svg       Logo (Favicon)
  i18n/          Sprachdateien (de.json = Ausgangssprache, en.json …)
  i18n.js        Übersetzungen im Browser (_t, data-i18n, Sprachauswahl) – auch im Beacon
  i18n-editor.*  Sprach-Editor der Verwaltung – auch im Beacon
  embed/
    chat.js      Einbettbares Widget
    chat.css     Widget-Styles
    format.js    Formatierung (Chat und Widget)
```

## GitHub: CI, Releases und Dependabot

Im Ordner `.github/` liegt alles für GitHub:

| Datei | Zweck |
|-------|-------|
| `workflows/ci.yml` | Bei jedem Pull Request: `gofmt`, `go vet`, `go test -race`, JavaScript-Syntax, Builds mit allen Speicher-Varianten (Dateien, SQLite, MySQL, beides) samt Rauchtest beider Programme |
| `workflows/release.yml` | Bei jedem Push auf `main` und jedem Tag `v*`: erst die CI, dann Container-Images für **amd64 und arm64** in die GitHub Container Registry und Programmdateien für Linux, Windows und macOS |
| `workflows/cla.yml` | Fragt bei Pull Requests nach der Zustimmung zum [CLA](CLA.md); Unterschriften landen im Branch `cla-signatures` (wird beim ersten Mal angelegt) |
| `dependabot.yml` | Wöchentliche Update-PRs (montags) für GitHub Actions, die Docker-Basis-Images und – sobald vorhanden – Go-Module |

**Container-Images** (das UCNG-Image enthält alle Speicher-Treiber):

| Auslöser | Tags |
|----------|------|
| Push auf `main` | `latest`, `<git describe>`, `sha-<kurz>` |
| Tag `v1.2.3` | `1.2.3`, `1.2`, `<git describe>` |

```bash
docker run -p 8080:8080 -v ucng:/data -e ADMIN_PASSWORD=… ghcr.io/<owner>/ucng:latest
docker run -p 8090:8090 -v beacon:/data -e ADMIN_PASSWORD=… ghcr.io/<owner>/ucng-beacon:latest
```

**Release veröffentlichen:** `git tag v1.0.0 && git push origin v1.0.0`. Der Workflow legt das GitHub-Release an (mit automatisch erzeugten Release-Notes) und hängt Archive für `linux/amd64`, `linux/arm64`, `windows/amd64`, `darwin/arm64` und `darwin/amd64` an – jeweils `ucng` und `ucng-beacon` – plus `SHA256SUMS.txt`. Die Version steht im Log beim Start („UCNG 1.0.0 läuft …“) und wird beim Beacon mitgemeldet.

**Einmalig in GitHub einstellen:**

- *Settings → Actions → General → Workflow permissions*: „Read and write permissions“ ist **nicht** nötig – die Workflows fordern die Rechte selbst an (`packages: write`, `contents: write` nur für das Release).
- Nach dem ersten Lauf unter *Packages* die Images bei Bedarf auf **Public** stellen (neue GHCR-Pakete sind privat).
- Empfohlen: *Settings → Branches* eine Regel für `main` mit dem Pflicht-Check **Tests** (aus `ci`) – und zusätzlich **CLA unterschrieben** (aus `cla`), damit nur Beiträge mit unterschriebenem CLA zusammengeführt werden.
- Vor dem Veröffentlichen in `NOTICE` den Projektinhaber eintragen (zwei Stellen mit „[PROJEKTINHABER …]“) – auf ihn verweist das CLA.
- *Settings → General → Social preview*: `assets/social-preview.png` hochladen.
- Dependabot-Alerts und Security-Updates unter *Settings → Code security* einschalten.

## Hinweise für den Produktivbetrieb

- **HTTPS:** Hinter einen Reverse-Proxy (Caddy, nginx, Traefik) stellen. Bei nginx für `/api/rooms/*/events` `proxy_buffering off;` setzen (der Server sendet zusätzlich `X-Accel-Buffering: no`) und `proxy_read_timeout` > 30 s.
- **Bot-Schlüssel schützen:** API-Schlüssel und Webhook-Geheimnis gehören nur in die Konfiguration des Bots, nicht in Webseiten oder öffentliche Repositories. Die Bot-API ist absichtlich nicht per CORS aus dem Browser erreichbar.
- **Webhooks:** Der Chat-Server ruft die eingetragenen URLs auf. Trage nur Adressen ein, die du kontrollierst.
- **Admin-Passwort setzen:** Ein langes `ADMIN_PASSWORD` verwenden und den Admin-Bereich nur über HTTPS aufrufen.
- **Einbetten einschränken:** Unter Widgets „Nur auf eingetragenen Domains“ wählen und bei jedem Widget die Domains eintragen.
- **Eigene Konten statt geteiltem Passwort:** Jede Person im Team bekommt ein eigenes Konto – so steht im Protokoll, wer was getan hat, und ein Konto lässt sich einzeln sperren.
- **Modus „hosted“:** `PUBLIC_URL` mit `https://` setzen (Links in E-Mails, Föderation, 2FA-Bezeichnung) und `TRUST_PROXY=1` hinter dem Reverse-Proxy. Die Browser-Kryptografie (WebCrypto) funktioniert nur über HTTPS oder auf `localhost`. Der Reverse-Proxy muss `/.well-known/ucng` und `/api/fed/` an UCNG durchreichen, wenn die Föderation an ist.
- **Daten schützen:** Der Datenordner enthält Nachrichten, Originaltexte gelöschter Nachrichten, Token- und Passwort-Hashes (Rechte `0600`). Die Backups ebenso – lege sie auf einen geschützten Datenträger.
- **Hinter einem Proxy** `TRUST_PROXY=1` setzen, sonst sehen Rate-Limit und Verbindungssperre nur die Adresse des Proxys.
- **Skalierung:** Der Zustand liegt im Speicher einer Instanz. Für mehrere Instanzen bräuchte es einen gemeinsamen Pub/Sub (z. B. Redis).
- Die Moderationsansicht und die Einbettungs-Vorschau sind normale Verbindungen und zählen in der Online-Anzeige mit.
- Jeder Browser hält pro Kanal eine offene Verbindung. Bei HTTP/1.1 erlauben Browser nur ~6 Verbindungen pro Domain – mit HTTP/2 (Standard hinter Caddy/nginx mit TLS) entfällt diese Grenze.

## Lizenz

UCNG und der UCNG Beacon stehen unter der **GNU Affero General Public License, Version 3 oder neuer** (`AGPL-3.0-or-later`, siehe [`LICENSE`](LICENSE) und [`NOTICE`](NOTICE)). Kurz gefasst:

- Du darfst UCNG frei nutzen, betreiben, verändern und weitergeben – auch kommerziell.
- **Wer eine geänderte Fassung anderen über das Netz anbietet** (eine Chat-Instanz oder einen Beacon), muss deren Nutzern den vollständigen Quellcode dieser Fassung zugänglich machen – unter derselben Lizenz. In einem föderierten Netz heißt das: Jede Instanz legt offen, was an ihr verändert wurde.
- Dafür gibt es `SOURCE_URL`: Die Adresse steht im Chat neben Datenschutz/Impressum, unten in der Verwaltung, in `/.well-known/ucng` (für andere Instanzen) und im Beacon-Verzeichnis an jeder Instanz. Offizielle Builds (Releases, Container-Images) setzen sie automatisch auf den genauen Stand im Repository; die Verwaltung weist darauf hin, wenn sie fehlt.
- Name und Logo sind von der Lizenz ausgenommen – siehe [`TRADEMARKS.md`](TRADEMARKS.md). Forks sind willkommen, bitte mit eigenem Namen.

**Beiträge** nehmen wir unter dem [Contributor License Agreement](CLA.md) an: Du behältst dein Urheberrecht, räumst dem Projektinhaber aber das Recht ein, Beiträge auch unter anderen Bedingungen zu lizenzieren; alles, was veröffentlicht wurde, bleibt unter der AGPL verfügbar. Der Workflow `.github/workflows/cla.yml` fragt beim ersten Pull Request automatisch nach der Zustimmung. Mehr in [`CONTRIBUTING.md`](CONTRIBUTING.md).

Jede Quelldatei trägt SPDX-Kopfzeilen (`SPDX-License-Identifier: AGPL-3.0-or-later`); ein Test prüft das. Fremdinhalte: siehe [Lizenzen von Drittinhalten](#lizenzen-von-drittinhalten).

## Name und frühere Bezeichnungen

Das Projekt hieß bis Oktober 2026 *GoChat*. Sichtbare Namen, Programme (`ucng`, `ucng-beacon`), Container-Images und Protokoll-Kennungen heißen jetzt UCNG. Damit Instanzen, Beacons, Bots und eingebettete Widgets während eines Updates weiterarbeiten, gilt:

| Bereich | neu | früher (wird weiter angenommen) |
|---------|-----|----------------------------------|
| Serverschlüssel einer Instanz | `/.well-known/ucng`, `"software": "ucng"` | `/.well-known/gochat` (mit `"software": "gochat"`) wird zusätzlich ausgeliefert und bei anderen Instanzen abgefragt |
| Signierte Anfragen (Föderation, Beacon) | `X-UCNG-Domain`, `-Date`, `-Signature` | `X-GoChat-*` werden mitgeschickt und angenommen |
| Webhooks an Bots | `X-UCNG-Signature`, `-Timestamp`, `-Event`, `-Bot` | `X-GoChat-*` werden zusätzlich gesendet |
| Beacon-Kennung | `"software": "ucng-beacon"` | `gochat-beacon` |
| Widget-Schnittstelle | `UCNG.init()`, `UCNG.load()` | `GoChat.init()` usw. (dasselbe Objekt) |
| Profil-Code / -Datei | `UCNG1:…`, `ucng-profile` | `GOCHAT1:…`, `gochat-profile` |
| Vorlagen, Übersetzungspakete | `ucng-template`, `ucng-i18n-package/1` | `gochat-template`, `gochat-i18n-package/1` |
| Backups | `ucng-JJJJMMTT-hhmmss.tar.gz` | `gochat-…` werden weiter angezeigt |
| SQLite-Datei | `ucng.db` | eine vorhandene `gochat.db` wird weiter genutzt |

Bewusst **unverändert** bleiben gespeicherte und eingebettete Bezeichner: Datenbanktabellen (`gochat_docs` …), Signatur- und Verschlüsselungs-Kontexte (`gochat-fed-v1`, `gochat-dm-v1` … – sie sind Teil der Signaturen bzw. der verschlüsselten Direktnachrichten), Schlüssel im Browser-Speicher und das Sprach-Cookie `gochat_lang` (sonst wären Namen, Sitzungen und Spracheinstellung weg) sowie die CSS-Klassen und -Variablen des Widgets (`.gochat-root`, `--gochat-accent` …), damit eigenes Widget-CSS weiter greift. **Reihenfolge beim Update:** zuerst die Chat-Instanzen, dann den Beacon. Neue Chats arbeiten mit alten und neuen Beacons zusammen (Anmeldung, GIF-Sammlungen, Sprachpakete); ein alter Chat kann sich zwar bei einem neuen Beacon anmelden, dessen GIF-Sammlungen aber nicht mehr abonnieren, weil er die Kennung `gochat-beacon` erwartet. Für die Föderation zwischen Instanzen schicken und akzeptieren neue Versionen beide Kopfzeilen-Sätze und fragen den Serverschlüssel bei Bedarf unter dem früheren Pfad ab.

**Logo:** `assets/logo.svg` (Zeichen), `assets/logo-wordmark.svg` (mit Schriftzug, hell/dunkel), `assets/social-preview.png` (1280 × 640, für die GitHub-Einstellung „Social preview“). Das Zeichen – eine Sprechblase mit einem Torbogen aus drei verbundenen Knoten – steht in der Weboberfläche in der Akzentfarbe der jeweiligen Instanz.

## Lizenzen von Drittinhalten

Die Länderflaggen (`web/flags/`, `cmd/ucng-beacon/web/flags/`) stammen aus [flag-icons](https://github.com/lipis/flag-icons) 7.2.3 (MIT-Lizenz, siehe `LICENSE.txt` im jeweiligen Ordner). Sie werden als Bilder ausgeliefert, weil Windows Flaggen-Emojis nur als Buchstaben (DE, AT …) darstellt.
