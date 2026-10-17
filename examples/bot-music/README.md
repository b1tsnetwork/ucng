# Musik-Bot

Spielt MP3s (auch OGG/Opus, FLAC, M4A, AAC, WAV) aus einem Ordner **oder** YouTube-Videos und
-Playlists in einen Sprachkanal. Gesteuert wird im Chat des Kanals.

Der Bot ist reines Go (nur Standardbibliothek, ~600 Zeilen in `main.go`). Zum Umwandeln ruft er
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
cp .env.example .env            # UCNG_BOT_TOKEN und MUSIC_PATH eintragen
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
| `!play <Suchbegriff>` | erstes YouTube-Suchergebnis |
| `!folder [Unterordner]` | alle Dateien aus dem Musik-Ordner, zufällig gemischt (`!ordner`) |
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
| `YTDLP_UPDATE` | `1` (Image) | beim Start neueste yt-dlp-Fassung holen (YouTube ändert sich oft) |
| `CACHE_DIR` | `/music/youtube` (Image), sonst leer | Zwischenspeicher für YouTube (siehe unten); leer = aus |
| `CACHE_MAX_GB` | `0` | Größengrenze des Zwischenspeichers; `0` = ohne Grenze |
| `TMP_DIR` | `/tmp` (Image) | für Downloads von YouTube (ein Stück liegt dort, solange es läuft) |

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

Der Bot schickt die Musik im **Streaming-Modus**: `POST /api/bot/voice/{room}/play?stream=1` mit
dem Ogg/Opus-Strom als Body. Die Wiedergabe startet sofort, ohne Längengrenze; die Anfrage endet
mit `{"ended":true,"reason":"eof|stopped|gone|error"}`. `POST …/volume?volume=30` ändert die
Lautstärke live, `POST …/stop` beendet. Details stehen im Haupt-README unter „Bot-API“.

**Wichtig:** Den Bot direkt mit dem Chat-Server verbinden (wie in `deploy/music` im Docker-Netz).
Läuft er über einen Reverse-Proxy, muss der den Request-Body ungepuffert weiterreichen
(nginx: `proxy_request_buffering off;`), sonst kommt die Musik erst nach dem Ende des Stücks an.

## Rechtliches

Spiel nur Musik ab, die du öffentlich (im Kanal) wiedergeben darfst. Das Herunterladen von
YouTube kann gegen die Nutzungsbedingungen von YouTube verstoßen und ist je nach Land und Inhalt
urheberrechtlich heikel – die Verantwortung liegt beim Betreiber. Das gilt erst recht für den
Zwischenspeicher, der Kopien dauerhaft aufbewahrt; wer das nicht will, setzt `CACHE_DIR=` leer.
