# Musik-Bot

Spielt MP3s (auch OGG/Opus, FLAC, M4A, AAC, WAV) aus einem Ordner **oder** YouTube-Videos und
-Playlists in einen Sprachkanal. Gesteuert wird im Chat des Kanals. Mit `YOUTUBE=0` gibt es
kein YouTube – dann läuft nur Musik aus dem Ordner, z. B. lizenzfreie Stücke
([Nur Musik aus dem Ordner](#nur-musik-aus-dem-ordner-youtube0)).

Der Bot ist reines Go (nur Standardbibliothek, gut 1000 Zeilen). Zum Umwandeln ruft er
`ffmpeg` auf, für YouTube `yt-dlp` – beides steckt im offiziellen Docker-Image.

## 1. Im Admin-Bereich vorbereiten

1. **Bots → Neuer Bot**, z. B. „DJ“, mit den Rechten **Schreiben** und
   **Musik in Sprachkanäle einspielen**. Den Token (`gcb_…`) kopieren.
2. Beim gewünschten Kanal unter **Sprachkanal** das Häkchen **Musik von Bots** setzen.
   (Bei Ende-zu-Ende-verschlüsselten Sprachkanälen geht Musik nicht – der Server kann nichts einspielen.)

## 2. Starten mit Docker

Es gibt ein offizielles Image für amd64 und arm64: `ghcr.io/<owner>/ucng-bot-music` (Tags wie
beim Chat-Server: `latest`, `1.2.3`, `1.2`). Fertige compose-Datei: [`deploy/music`](../../deploy/music/compose.yml).
Der Chat-Server läuft dabei wie in `deploy/chat/compose.yml` (Docker-Netz `ucng_default`, Dienst `ucng`).

```sh
cd deploy/music
cp .env.example .env            # UCNG_BOT_TOKEN und MUSIC_PATH eintragen (alle Optionen stehen darin)
docker compose up -d
docker logs -f ucng-bot-music
```

Unter Windows (PowerShell) genauso, z. B. mit `MUSIC_PATH=D:/Musik` in der `.env`.

Ohne compose:

```sh
docker run -d --name ucng-bot-music --network ucng_default --read-only --tmpfs /tmp:size=1g,uid=10001,gid=10001 \
  -e UCNG_URL=http://ucng:8080 -e UCNG_BOT_TOKEN=gcb_… \
  -v /pfad/zur/musik:/music ghcr.io/<owner>/ucng-bot-music:latest
```

Selbst bauen (aus dem Wurzelverzeichnis des Repos): `docker build -f Dockerfile.music -t ucng-bot-music .`
– mit `--build-arg YTDLP_VERSION=…` bzw. `DENO_VERSION=…` lassen sich yt-dlp und deno festlegen
(Standard: neueste Fassung). Im Image: ffmpeg (Debian), yt-dlp und deno (JavaScript-Laufzeit, die
yt-dlp für YouTube braucht). Der Container läuft als Nutzer 10001 und verträgt `read_only`;
Downloads und das yt-dlp-Update beim Start landen in `/tmp`.

Ohne Docker geht es auch (ffmpeg und ggf. yt-dlp im `PATH`):

```sh
UCNG_URL=http://localhost:8080 UCNG_BOT_TOKEN=gcb_… MUSIC_DIR=~/Musik go run ./examples/bot-music
```

## 3. Befehle im Chat

| Befehl | Wirkung |
|---|---|
| `!play <YouTube-Link>` | Video in die Warteschlange; bei einer Playlist alle Videos (`!p` geht auch) |
| `!play <Suchbegriff>` | erstes YouTube-Suchergebnis; mit `YOUTUBE=0`: alle Dateien im Musik-Ordner, deren Name (oder Ordner) alle Wörter enthält |
| `!play` | ohne Angabe: alles aus dem Musik-Ordner, gemischt |
| `!folder [Unterordner]` | alle Dateien aus dem Musik-Ordner, zufällig gemischt (`!ordner`) |
| `!songs [Begriff]` | Unterordner mit Anzahl der Stücke; mit Begriff die passenden Stücke (`!list`, `!lieder`) |
| `!skip` | nächstes Stück (`!next`) |
| `!stop` | Musik aus, Warteschlange leeren, Bot verlässt den Kanal |
| `!queue` | Warteschlange anzeigen (`!q`) |
| `!np` | was läuft gerade |
| `!vol 30` | Lautstärke 0–100 – ändert sich sofort, auch mitten im Stück |
| `!shuffle` / `!loop` / `!clear` | mischen / Warteschlange wiederholen / leeren |
| `!cache` | Größe des Zwischenspeichers; `!cache forget <Link>` löscht ein Stück (nur Team) |
| `!music` | Hilfe (`!musik`) |

Jede Person kann die Musik zusätzlich für sich selbst lauter/leiser stellen oder stummschalten.

## Einstellungen (Umgebungsvariablen)

| Variable | Standard | Bedeutung |
|---|---|---|
| `UCNG_URL` | `http://ucng:8080` (Image) | Adresse des Chat-Servers |
| `UCNG_BOT_TOKEN` | – | Token aus dem Admin-Bereich (Pflicht) |
| `MUSIC_DIR` | `/music` | Musik-Ordner (im Container); der Bot verlässt ihn nie |
| `MUSIC_ROOM` | leer | fester Sprachkanal; leer = Kanal, in dem der Befehl kommt |
| `PREFIX` | `!` | Befehlszeichen |
| `TEAM_ONLY` | `0` | `1` = nur das Team darf steuern |
| `VOLUME` | `20` | Start-Lautstärke |
| `BITRATE` | `128` | Opus-Bitrate in kbit/s (16–256) |
| `MAX_QUEUE` | `500` | maximale Länge der Warteschlange |
| `YOUTUBE` | `1` | `0` = kein YouTube und keine Links: nur Musik aus dem Musik-Ordner, `!play <Begriff>` sucht dort; alle `YTDLP_*`- und `CACHE_*`-Einstellungen sind dann ohne Wirkung |
| `YTDLP_UPDATE` | `1` (Image) | beim Start neueste yt-dlp-Fassung holen (YouTube ändert sich oft) |
| `YTDLP_COOKIES` | leer | Cookies eines YouTube-Kontos (`cookies.txt`, Netscape-Format) – bei „Sign in to confirm you’re not a bot“ (siehe unten); darf schreibgeschützt eingebunden sein |
| `YTDLP_ARGS` | leer | weitere Optionen für yt-dlp, z. B. `--extractor-args youtube:player_client=web` |
| `START_TIMEOUT` | `90` | so viele Sekunden auf den ersten Ton warten, dann Fehlermeldung mit dem Grund von YouTube |
| `CACHE_DIR` | `/music/youtube` (Image), sonst leer | Zwischenspeicher für YouTube (siehe unten); leer = aus |
| `CACHE_MAX_GB` | `0` | Größengrenze des Zwischenspeichers; `0` = ohne Grenze |
| `TMP_DIR` | `/tmp` (Image) | für Downloads von YouTube (ein Stück liegt dort, solange es läuft) |

Nur in `deploy/music` (compose): `MUSIC_PATH` (Ordner auf dem Rechner, wird nach `/music` eingebunden), `TMP_SIZE` (Platz für Downloads im Arbeitsspeicher, Standard `1g`), `UCNG_VERSION` und `TZ`. Alle Werte mit Erklärung: [`deploy/music/.env.example`](../../deploy/music/.env.example).

## Zwischenspeicher für YouTube

Jedes YouTube-Stück wird beim ersten Abspielen nebenbei fertig umgewandelt und dauerhaft abgelegt –
im Image unter `/music/youtube` als `Titel [Video-ID].opus` (Ogg-Opus, mit Titel und Link in den
Metadaten). Beim nächsten `!play` desselben Videos fragt der Bot **gar nicht mehr bei YouTube**
und braucht auch kein ffmpeg: Die Datei geht direkt an den Chat-Server – Start sofort, kaum CPU.

- **Schlüssel ist die Video-ID**, nicht die URL: `watch?v=…`, `youtu.be/…`, Shorts, `music.youtube.com`
  und Links mit `&t=`/`?si=` treffen dasselbe Stück. Suchbegriffe (`!play lofi beats`) merkt sich
  der Bot ebenfalls.
- **Playlists** werden weiterhin bei YouTube aufgelöst (sie ändern sich), aber jedes schon
  gespeicherte Video daraus kommt aus dem Zwischenspeicher.
- **Kein Ablauf.** Mit `CACHE_MAX_GB` gibt es eine Grenze; dann wird zuerst gelöscht, was am längsten
  nicht lief. Bei 128 kbit/s braucht eine Stunde Musik rund 55 MB.
- Weil der Ordner im Musik-Ordner liegt, spielt **`!folder youtube`** alles Gespeicherte, gemischt.
  Von Hand löschen geht auch (Datei weg = wird beim nächsten Mal neu geholt); `index.json` hält
  die Zuordnung Video-ID → Datei.
- Dafür muss der Musik-Ordner **beschreibbar** eingebunden sein (ohne `:ro`). Der Bot schreibt nur in
  `youtube/`. Unter Linux läuft der Container als Nutzer 10001: einmal
  `mkdir -p music/youtube && sudo chown 10001 music/youtube`. Ist der Ordner nicht beschreibbar,
  schreibt der Bot das ins Log und spielt einfach ohne Zwischenspeicher.
- Lieber getrennt? `CACHE_DIR=/cache` und ein eigenes Volume nach `/cache`; dann bleibt `/music` `:ro`.

## So funktioniert es

```
Datei / YouTube ──yt-dlp──▶ ffmpeg (Opus/Ogg, 48 kHz) ──POST …/play?stream=1──▶ UCNG ──▶ Sprachkanal
```

- **YouTube-Videos:** yt-dlp lädt mit voller Geschwindigkeit in eine Datei, ffmpeg liest sie schon
  während des Downloads mit – die Wiedergabe beginnt nach wenigen Sekunden, auch bei langen Videos.
  Im Chat steht erst „⏳ Lade …“, „▶️“ erst, wenn wirklich Ton fließt. Der fertige Download kommt in
  den Zwischenspeicher (auch, wenn das Stück übersprungen wurde).
- **Livestreams** (Radio-Streams, Premieren nach dem Start) erkennt der Bot: Sie laufen, bis jemand
  `!skip` oder `!stop` schreibt, und werden nicht gespeichert. Angekündigte, noch nicht gestartete
  Streams lehnt er mit einer Meldung ab.
- **Nie still hängen:** Kommt nach `START_TIMEOUT` Sekunden kein Ton, bricht der Bot ab und schreibt
  den Grund in den Chat. Lehnt YouTube ab oder bricht ein Download ab, steht die Meldung von yt-dlp
  im Chat (z. B. „Sign in to confirm you’re not a bot“, „Video unavailable“, „HTTP Error 403“).

Der Bot schickt die Musik im **Streaming-Modus**: `POST /api/bot/voice/{room}/play?stream=1` mit
dem Ogg/Opus-Strom als Body. Die Wiedergabe startet sofort, ohne Längengrenze; die Anfrage endet
mit `{"ended":true,"reason":"eof|stopped|gone|error"}`. `POST …/volume?volume=30` ändert die
Lautstärke live, `POST …/stop` beendet. Details stehen im Haupt-README unter „Bot-API“.

**Wichtig:** Den Bot direkt mit dem Chat-Server verbinden (wie in `deploy/music` im Docker-Netz).
Läuft er über einen Reverse-Proxy, muss der den Request-Body ungepuffert weiterreichen
(nginx: `proxy_request_buffering off;`), sonst kommt die Musik erst nach dem Ende des Stücks an.

## Wenn YouTube nicht abspielt

| Meldung im Chat | Ursache und Abhilfe |
|---|---|
| „Sign in to confirm you’re not a bot“ / „confirm your age“ | YouTube verlangt eine Anmeldung (oft bei Rechenzentrums-IPs). Cookies eines YouTube-Kontos hinterlegen – Schritt für Schritt unter [YouTube-Cookies exportieren](#youtube-cookies-exportieren). Am besten ein eigenes Konto nur dafür. |
| „HTTP Error 403“, „Requested format is not available“, „Unable to extract …“ | yt-dlp ist zu alt für eine Änderung bei YouTube. `YTDLP_UPDATE=1` (Standard) holt beim Neustart die neueste Fassung: `docker compose restart`. |
| „YouTube liefert nach 90 s keinen Ton“ | Sehr langsame Verbindung zu YouTube oder yt-dlp hängt. Im Log steht mehr (`docker logs ucng-bot-music`); `START_TIMEOUT` erhöhen. |
| „Video unavailable“ / „Private video“ / Ländersperre | Das Video ist für den Server nicht abrufbar. |
| „Verbindung zum Sprachkanal verloren“ | Der Sprach-Server wurde neu gestartet oder der Kanal gelöscht – einfach erneut `!play`. |

Die Musik-Bots älterer Fassungen haben in diesen Fällen still nichts abgespielt (oder bei Livestreams
endlos geladen); jetzt kommt immer eine Meldung.

### YouTube-Cookies exportieren

YouTube tauscht die Cookies offener Tabs regelmäßig aus – exportierte Cookies aus einem normalen
Fenster werden deshalb schnell ungültig. So bleiben sie gültig (Empfehlung der yt-dlp-Entwickler):

1. **Erweiterung installieren:** Chrome/Edge „Get cookies.txt LOCALLY“, Firefox „cookies.txt“.
   (Nicht „Get cookies.txt“ ohne „LOCALLY“ – die wurde als Schadsoftware entfernt.) In Chrome die
   Erweiterung unter *Details* für *Inkognito zulassen*, in Firefox für *private Fenster*.
2. **Privates Fenster** öffnen und dort bei YouTube anmelden – am besten mit einem eigenen Konto nur
   für den Bot (YouTube kann Konten sperren, die von Programmen genutzt werden).
3. Im **selben Tab** `https://www.youtube.com/robots.txt` aufrufen (kein anderer privater Tab offen).
4. Über die Erweiterung die Cookies **für youtube.com exportieren** → Datei `cookies.txt`.
5. Das **private Fenster schließen** und dieses Konto im Browser nicht mehr öffnen.
6. `cookies.txt` neben `deploy/music/compose.yml` legen, dort `- ./cookies.txt:/cookies.txt:ro`
   einkommentieren, in der `.env` `YTDLP_COOKIES=/cookies.txt` setzen, `docker compose up -d`.
   Im Log steht dann „YouTube: Cookies aus /cookies.txt“.

Die Datei muss mit `# Netscape HTTP Cookie File` (oder `# HTTP Cookie File`) beginnen. Unter
Windows gespeicherte Zeilenenden wandelt der Bot selbst um. Behandle die Datei wie ein Passwort.
Laufen die Cookies irgendwann ab (wieder „Sign in to confirm …“), die Schritte wiederholen.

## Nur Musik aus dem Ordner (`YOUTUBE=0`)

Wer YouTube nicht nutzen will (Nutzungsbedingungen, Urheberrecht, Bot-Sperren), setzt in der
`.env` `YOUTUBE=0` und legt Musik in den Musik-Ordner – zum Beispiel lizenzfreie Stücke. Dann:

- `!play <Link>` antwortet „Links sind abgeschaltet“, `!play <Begriff>` sucht in den Datei- und
  Ordnernamen (`!play lofi` spielt alles aus dem Ordner `lofi` oder mit „lofi“ im Namen),
  `!songs` zeigt die Ordner, `!songs <Begriff>` die Treffer.
- yt-dlp wird nicht gestartet und beim Start nicht aktualisiert, Cookies werden nicht gelesen.
- Früher gespeicherte YouTube-Stücke (`/music/youtube`) spielt der Bot nicht mehr – auch nicht
  über `!folder`. Löschen kannst du sie selbst; der Ordner darf dann wieder `:ro` eingebunden werden.

**Namensnennung:** Viele freie Lizenzen (z. B. CC BY) verlangen, dass Künstler und Lizenz genannt
werden. Der Bot liest beim Abspielen die Tags der Datei (`artist`, `copyright` bzw. `license`) und
zeigt sie in „▶️“ und `!np` an, z. B. „▶️ **Wallpaper** – Kevin MacLeod · CC BY 4.0“. Fehlen sie,
lassen sie sich mit jedem Tag-Editor (z. B. [Mp3tag](https://www.mp3tag.de), [Kid3](https://kid3.kde.org)) oder mit ffmpeg nachtragen:

```sh
ffmpeg -i in.mp3 -c copy -metadata artist="Kevin MacLeod" -metadata copyright="CC BY 4.0 – incompetech.com" out.mp3
```

**Ordnen:** Unterordner nach Stimmung (`lofi/`, `chill/`, `games/` …) machen `!folder lofi` und
`!play <Begriff>` nützlich. Dateinamen wie „Künstler – Titel.mp3“ helfen der Suche.

Quellen für freie Musik (Lizenz **je Stück** prüfen – nicht jede erlaubt die öffentliche Wiedergabe;
NC = nicht kommerziell, ND = keine Bearbeitung):

| Seite | Lizenzen | Hinweis |
|---|---|---|
| [Free Music Archive](https://freemusicarchive.org) | Creative Commons / gemeinfrei, je Stück | große Auswahl, nach Lizenz filterbar |
| [Incompetech](https://incompetech.com/music/royalty-free/) (Kevin MacLeod) | CC BY 4.0 | Namensnennung Pflicht |
| [ccMixter](https://ccmixter.org) | Creative Commons, je Stück | Remixe, Instrumentals, Lofi |
| [Musopen](https://musopen.org) | gemeinfrei / CC | Klassik-Aufnahmen |
| [Internet Archive – Netlabels](https://archive.org/details/netlabels), [Wikimedia Commons](https://commons.wikimedia.org/wiki/Category:Audio_files) | CC / gemeinfrei, je Datei | sehr gemischt |
| [Pixabay Music](https://pixabay.com/music/) | eigene Pixabay-Lizenz | Weitergabe „als Datei, unverändert“ ist verboten – das reine Abspielen als Radio ist eine Grauzone |
| [Jamendo](https://www.jamendo.com) | CC zum privaten Hören | öffentliche Nutzung nur mit kostenpflichtiger Lizenz von Jamendo Licensing |

In Deutschland vermutet die GEMA bei öffentlicher Musikwiedergabe zunächst ihr Repertoire. Wer
nur freie Musik spielt, bewahrt am besten auf, woher jedes Stück stammt und unter welcher Lizenz es
steht (Tags, eine Liste oder die Lizenz-Seite) – das ist auch der Nachweis gegenüber der GEMA.
Keine Rechtsberatung.

## Rechtliches

Spiel nur Musik ab, die du öffentlich (im Kanal) wiedergeben darfst. Das Herunterladen von
YouTube kann gegen die Nutzungsbedingungen von YouTube verstoßen und ist je nach Land und Inhalt
urheberrechtlich heikel – die Verantwortung liegt beim Betreiber. Mit `YOUTUBE=0` fällt das weg. Das gilt erst recht für den
Zwischenspeicher, der Kopien dauerhaft aufbewahrt; wer das nicht will, setzt `CACHE_DIR=` leer.
