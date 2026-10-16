# Weitere Sprach-Server für UCNG einrichten

Diese Anleitung beschreibt, wie du zusätzliche Sprach-Server (`ucng-voice`, im Folgenden **Agent**) neben deinem UCNG-Chat betreibst. Für kleine Installationen brauchst du das nicht – der eingebaute Relay des Chat-Servers reicht für bis zu 200 Teilnehmer bzw. 50 Mbit/s (einstellbar). Agenten lohnen sich, wenn

- mehr Menschen gleichzeitig in Sprachkanälen sind, als ein Server tragen soll,
- die Bandbreite für Sprache vom Chat-Server getrennt werden soll, oder
- Sprache näher an den Nutzern laufen soll (z. B. ein Agent je Region).

---

## 1. So funktioniert es

```
                 ┌────────────────────────────┐
  Browser ──────▶│ Chat-Server (ucng)         │  Zutritt, Rechte, Moderation
   │  HTTPS      │  https://chat.example.org  │  stellt signierte Zutrittskarten aus
   │             └──────▲──────────────┬──────┘
   │                    │ Meldung       │ Steuerbefehle
   │                    │ alle 3 s      │ (stummschalten, entfernen)
   │                    │ CHAT_URL      ▼ CONTROL_URL
   │  WSS        ┌──────┴─────────────────────┐
   └────────────▶│ Agent (ucng-voice)         │  leitet nur Sprache weiter
                 │  wss://voice1.example.org  │
                 └────────────────────────────┘
```

- Der **Chat-Server** entscheidet über alles: wer beitreten, sprechen oder zuhören darf. Er gibt dem Browser eine mit Ed25519 signierte Zutrittskarte (60 s gültig, nur einmal verwendbar) und die Adresse des Agenten.
- Der **Agent** prüft die Karte mit dem öffentlichen Schlüssel des Chat-Servers und leitet die Opus-Rahmen weiter. Er mischt nichts, speichert nichts und kennt keine Konten.
- Jeder Agent **meldet sich alle 3 Sekunden** beim Chat-Server (Auslastung, Teilnehmer). Ein neuer Sprachkanal kommt auf den am wenigsten ausgelasteten Relay und bleibt dort, bis er leer ist.
- Fällt ein Agent aus, gilt er nach 15 Sekunden als ausgefallen. Die Browser verbinden sich automatisch neu und landen auf einem anderen Relay.
- **Bildschirm und Kameras** laufen über eigene Verbindungen. Die Sprache eines Kanals bleibt auf ihrem Relay; Bildschirm (ab 50 % Auslastung, `VOICE_OFFLOAD_SCREEN`) und Kameras (ab 75 %, `VOICE_OFFLOAD_VIDEO`) kann der Chat-Server auf einen anderen Relay legen – bevorzugt auf Agenten mit `ROLE=media`. Ein Browser hat dann bis zu drei Verbindungen, gegebenenfalls zu verschiedenen Agenten.

### Verbindungen im Überblick

| Von | Nach | Adresse | Zweck |
|---|---|---|---|
| Browser | Agent | `PUBLIC_URL` des Agenten (`wss://…/ws`) | Sprache, Bildschirm, Kameras (je eine Verbindung) |
| Agent | Chat-Server | `CHAT_URL` + `/api/voice/agents/report` | Anmeldung und Meldungen |
| Chat-Server | Agent | `CONTROL_URL` (`…/control`) | Steuerbefehle |

Beide Richtungen zwischen Chat-Server und Agent sind mit demselben geheimen Schlüssel geschützt (`VOICE_AGENT_TOKEN` = `AGENT_TOKEN`). Am besten laufen sie über ein internes Netz. Über das Internet nur mit HTTPS.

---

## 2. Voraussetzungen

- Ein laufender UCNG-Chat-Server **mit gesetztem `PUBLIC_URL`** (z. B. `https://chat.example.org`). Der Agent lässt nur Browser von dieser Adresse zu.
- Für jeden Agenten eine eigene (Sub-)Domain mit TLS-Zertifikat, z. B. `voice1.example.org`. Browser verbinden sich per `wss://` – ohne HTTPS geht es nicht.
- Ein Reverse-Proxy (Caddy, nginx, Traefik …), der WebSockets durchreicht.
- Nur **TCP-Port 443** muss von außen erreichbar sein. UDP, STUN oder TURN sind nicht nötig.
- Docker mit Compose-Plugin – oder alternativ die Binärdatei `ucng-voice` aus dem Release (siehe Abschnitt 6).

### Wie groß muss ein Agent sein?

Gemessen auf 2 CPU-Kernen: 200 Teilnehmer mit 99,8 % zugestellten Rahmen bei 20 Mbit/s, 500 Teilnehmer mit 99,9 % bei 51 Mbit/s, Arbeitsspeicher unter 30 MB. Entscheidend ist fast immer die **ausgehende Bandbreite**:

| Klang-Voreinstellung | je Zuhörer (4 hörbare Sprecher) | 25 Teilnehmer | 200 Teilnehmer |
|---|---|---|---|
| Sparsam (16 kbit/s) | ca. 100 kbit/s | 2,5 Mbit/s | 20 Mbit/s |
| Standard (24 kbit/s) | ca. 135 kbit/s | 3,4 Mbit/s | 27 Mbit/s |
| Hoch (40 kbit/s) | ca. 200 kbit/s | 5 Mbit/s | 40 Mbit/s |
| Musik (96 kbit/s, Stereo) | ca. 400 kbit/s | 10 Mbit/s | 80 Mbit/s |

**Bildschirmfreigaben** kommen dazu – aber nur je Person, die tatsächlich zusieht: 3 Mbit/s (Text und Folien), 4,5 Mbit/s (Standard), 8 Mbit/s (Flüssig, 1080p60), 12 Mbit/s (1440p60) oder 16 Mbit/s (4K). Sehen 20 Personen einer Standard-Freigabe zu, sind das 90 Mbit/s. Ist die Grenze erreicht, nimmt der Relay keine weiteren Zuschauer an; die Sprache läuft weiter. Deshalb lohnt sich ein eigener **Medien-Agent** (`ROLE=media`, siehe unten): Der Chat-Server lagert Bildschirm und Kameras dorthin aus, sobald der Sprach-Server zu voll würde.

**Kameras (Konferenzraum)** kommen ebenfalls dazu. Jede Person mit Kamera schickt eine Miniansicht (180 kbit/s) an alle und – solange sie zu den großen Sprechern gehört – das große Bild (bis 0,7 / 1,8 / 3,2 / 5 Mbit/s bei SD / HD / Full HD / Full HD 60). Je Zuschauer ausgehend also etwa `Kameras × 180 kbit/s + große Sprecher × Qualität`: 10 Kameras, 1 Sprecher groß in HD ≈ 3,6 Mbit/s je Person, bei 10 Teilnehmern ≈ 36 Mbit/s. Langsame Zuschauer bekommen automatisch nur Miniansichten. **Musik von Bots** braucht je Zuhörer so viel wie ein Sprecher mit der Klang-Einstellung *Musik* (≈ 100 kbit/s).

Das sind vorsichtige Schätzungen (gemessen wurde etwa ein Viertel weniger). Setze `MAX_MBIT` auf etwa 70–80 % der Upload-Bandbreite des Servers. Der Agent nimmt keine neuen Teilnehmer mehr an, wenn die Schätzung diese Grenze überschreiten würde.

---

## 3. Chat-Server vorbereiten

1. Einen langen, zufälligen Schlüssel erzeugen:

   ```bash
   openssl rand -hex 32
   ```

2. In `deploy/chat/.env` eintragen:

   ```env
   PUBLIC_URL=https://chat.example.org
   VOICE_AGENT_TOKEN=<der Schlüssel>
   ```

   Optional: `VOICE_BUILTIN=0`, wenn Sprache **nur** über Agenten laufen soll (der eingebaute Relay startet dann nicht). Solange er läuft, zählt er bei der Verteilung wie ein weiterer Relay.

3. Chat-Server neu starten:

   ```bash
   cd deploy/chat && docker compose up -d
   ```

4. Prüfen: Admin-Bereich → **Übersicht** → *Sprach-Server*. Dort steht jetzt „Weitere Sprach-Server melden sich mit VOICE_AGENT_TOKEN an …“.

> Der Reverse-Proxy des **Chat-Servers** muss ebenfalls WebSockets durchreichen, wenn der eingebaute Relay aktiv ist (Pfad `/api/voice/ws`). Siehe README → *Sprachkanäle → Reverse-Proxy*.

---

## 4. Variante A: Agent auf demselben Server wie der Chat

Am einfachsten: Der Agent tritt dem Docker-Netz des Chats bei. Chat-Server und Agent sprechen dann intern miteinander, nach außen ist nur die WebSocket-Adresse sichtbar.

1. Vorlage kopieren:

   ```bash
   cd deploy/voice
   cp .env.example .env
   ```

2. `.env` ausfüllen:

   ```env
   AGENT_TOKEN=<derselbe Schlüssel wie VOICE_AGENT_TOKEN>
   PUBLIC_URL=wss://voice1.example.org
   AGENT_ID=ucng-voice
   CHAT_URL=http://ucng:8080
   CONTROL_URL=http://ucng-voice:8090/control
   VOICE_PORT=8091
   MAX_PARTICIPANTS=200
   MAX_MBIT=50
   ```

   - `CHAT_URL`: `ucng` ist der Containername des Chats aus `deploy/chat`.
   - `CONTROL_URL`: der Containername des Agenten (= `AGENT_ID`) plus `:8090/control`.

3. Starten:

   ```bash
   docker compose up -d
   docker compose logs -f
   ```

   Erwartet:

   ```
   ucng-voice … läuft auf :8090 – öffentlich wss://voice1.example.org/ws, Chat-Server http://ucng:8080
   Beim Chat-Server angemeldet
   ```

4. Reverse-Proxy für `voice1.example.org` einrichten (Abschnitt 7).

> Das Compose-Projekt erwartet das Docker-Netz `ucng_default` (Projektname `ucng` aus `deploy/chat`). Heißt dein Chat-Projekt anders, passe am Ende von `deploy/voice/compose.yml` den Namen unter `networks → chat → name` an. Mit `docker network ls` siehst du die vorhandenen Netze.

### Mehrere Agenten auf einem Server

Das bringt nur etwas, wenn der Server mehr CPU-Kerne hat, als ein Agent nutzt, oder wenn du die Grenzen getrennt halten willst. Für jeden weiteren Agenten einen eigenen Ordner mit eigener `.env` anlegen und dabei ändern:

```env
AGENT_ID=ucng-voice-2
CONTROL_URL=http://ucng-voice-2:8090/control
VOICE_PORT=8092
PUBLIC_URL=wss://voice2.example.org
```

Gestartet wird mit einem eigenen Projektnamen: `docker compose -p ucng-voice-2 up -d`.

---

## 5. Variante B: Agent auf einem eigenen Server

Auf dem Agent-Server brauchst du nur `deploy/voice/compose.yml` und `.env`.

1. In `compose.yml` den Block `networks` **beim Dienst und am Ende der Datei entfernen** – es gibt hier kein Docker-Netz des Chats.

2. `.env` ausfüllen:

   ```env
   AGENT_TOKEN=<derselbe Schlüssel wie VOICE_AGENT_TOKEN>
   PUBLIC_URL=wss://voice1.example.org
   AGENT_ID=voice-fra-1
   CHAT_URL=https://chat.example.org
   CONTROL_URL=https://voice1.example.org/control
   VOICE_PORT=8091
   MAX_PARTICIPANTS=300
   MAX_MBIT=150
   ```

   - `CHAT_URL`: öffentliche Adresse des Chats. Gibt es ein privates Netz zwischen beiden Servern (VPN, WireGuard, Hetzner vSwitch …), nimm lieber die interne Adresse.
   - `CONTROL_URL`: Adresse, unter der der **Chat-Server** den Agenten erreicht. Über das Internet nur per `https://`. Im Reverse-Proxy erlaubst du `/control` dann nur von der IP-Adresse des Chat-Servers (Abschnitt 7).

3. Starten und Log prüfen wie in Variante A.

### Reiner Medien-Server

Für viele Bildschirmfreigaben oder große Konferenzen lohnt sich ein Agent nur für Bildschirm und Kameras – mit viel Upload-Bandbreite. Er bekommt keine Sprachkanäle; der Chat-Server lagert die Medien zuerst dorthin aus:

```env
ROLE=media
AGENT_ID=media-fra-1
PUBLIC_URL=wss://media1.example.org
MAX_MBIT=800
```

Sonst wie oben. In der Übersicht (Admin → Übersicht → *Sprach-Server*) steht er als „nur Bildschirm und Kameras“, mit der Zahl laufender Freigaben und Kameras. Die Schwellen stellst du beim Chat-Server ein: `VOICE_OFFLOAD_SCREEN=0` lagert Bildschirme immer aus, `100` nie.

---

## 6. Variante C: ohne Docker (systemd)

Die Binärdatei `ucng-voice` liegt in jedem Release-Archiv (`ucng_<version>_linux_amd64.tar.gz`).

```bash
sudo install -m 0755 ucng-voice /usr/local/bin/ucng-voice
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ucng-voice
sudo install -d -m 0750 /etc/ucng-voice
```

`/etc/ucng-voice/env` (Rechte `0640`, Besitzer `root:ucng-voice`):

```env
ADDR=127.0.0.1:8091
CHAT_URL=https://chat.example.org
AGENT_TOKEN=<Schlüssel>
PUBLIC_URL=wss://voice1.example.org
CONTROL_URL=https://voice1.example.org/control
AGENT_ID=voice-fra-1
MAX_PARTICIPANTS=300
MAX_MBIT=150
TRUST_PROXY=1
```

`/etc/systemd/system/ucng-voice.service`:

```ini
[Unit]
Description=UCNG Sprach-Server
After=network-online.target
Wants=network-online.target

[Service]
User=ucng-voice
EnvironmentFile=/etc/ucng-voice/env
ExecStart=/usr/local/bin/ucng-voice
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
CapabilityBoundingSet=
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ucng-voice
journalctl -u ucng-voice -f
```

> Für viele gleichzeitige Verbindungen ist `LimitNOFILE` wichtig: Jeder Teilnehmer belegt eine Verbindung. Im Docker-Container gilt das Limit des Docker-Daemons, meist ausreichend.

---

## 7. Reverse-Proxy

Der Agent erwartet die WebSocket-Verbindungen unter **`/ws`**. Er setzt selbst kein TLS ein – das übernimmt der Proxy.

### Caddy

```caddy
voice1.example.org {
    # Steuerbefehle nur vom Chat-Server (Variante B) – bei Variante A ganz sperren
    @control {
        path /control
        not remote_ip 203.0.113.10   # IP-Adresse des Chat-Servers
    }
    respond @control 403

    reverse_proxy 127.0.0.1:8091
}
```

Caddy reicht WebSockets ohne weitere Einstellung durch und setzt `X-Forwarded-For` korrekt.

### nginx

```nginx
server {
    listen 443 ssl http2;
    server_name voice1.example.org;
    # ssl_certificate …; ssl_certificate_key …;

    location /ws {
        proxy_pass http://127.0.0.1:8091;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
        proxy_buffering off;
    }

    # Steuerbefehle nur vom Chat-Server (Variante B) – bei Variante A: deny all;
    location /control {
        allow 203.0.113.10;   # IP-Adresse des Chat-Servers
        deny all;
        proxy_pass http://127.0.0.1:8091;
    }

    location = /healthz {
        proxy_pass http://127.0.0.1:8091;
    }

    location / {
        return 404;
    }
}
```

### Firewall

Von außen muss nur 443/tcp offen sein (und 80/tcp für die Zertifikatserneuerung). Der Port des Agenten (`8091`) bleibt lokal – `deploy/voice/compose.yml` bindet ihn ohnehin nur an `127.0.0.1`.

---

## 8. Prüfen

1. **Gesundheit:**

   ```bash
   curl -s https://voice1.example.org/healthz
   ```

   `ok` heißt: angemeldet. `503 noch nicht beim Chat-Server angemeldet` heißt: Die Meldung an den Chat-Server klappt nicht – siehe Fehlersuche.

2. **Admin-Bereich** → *Übersicht* → *Sprach-Server*: Der Agent steht mit seinem Namen und seiner Auslastung in der Liste.

3. **Ausprobieren:** einen Sprachkanal mit zwei Browsern betreten. Läuft der eingebaute Relay noch, landet ein neuer Kanal auf dem weniger ausgelasteten Relay. Um den Agenten gezielt zu testen, vorübergehend `VOICE_BUILTIN=0` beim Chat setzen.

4. **Im Browser** (Entwicklerwerkzeuge → Netzwerk → WS): Es sollte eine Verbindung zu `wss://voice1.example.org/ws` mit Status `101` geben.

---

## 9. Fehlersuche

| Symptom | Ursache und Abhilfe |
|---|---|
| Log: `Antwort 401 (AGENT_TOKEN und VOICE_AGENT_TOKEN prüfen)` | Die Schlüssel stimmen nicht überein, oder beim Chat-Server ist `VOICE_AGENT_TOKEN` leer. |
| Log: `Antwort 404` oder `Sprachkanäle sind auf diesem Server ausgeschaltet` | Beim Chat-Server ist `VOICE=0` gesetzt oder `CHAT_URL` zeigt auf die falsche Adresse. |
| Log: `connection refused` / `no such host` | `CHAT_URL` ist vom Agenten aus nicht erreichbar. In Variante A: Ist der Agent im Netz `ucng_default`? (`docker network inspect ucng_default`) |
| Agent steht in der Übersicht als „meldet sich nicht“ | Seit über 15 Sekunden keine Meldung. Log des Agenten ansehen; nach 2 Minuten verschwindet er aus der Liste. |
| Browser: WebSocket `403 Herkunft nicht erlaubt` | Beim Chat-Server fehlt `PUBLIC_URL`, oder sie stimmt nicht mit der Adresse im Browser überein (z. B. `www.` oder ein anderer Port). Notfalls beim Agenten `ALLOWED_ORIGINS=https://chat.example.org` setzen. |
| Browser: WebSocket schlägt sofort fehl, kein Status `101` | Der Reverse-Proxy reicht die Upgrade-Kopfzeilen nicht durch (nginx: `proxy_http_version 1.1` und `Upgrade`/`Connection`). |
| Verbindung bricht nach genau 60 s ab | `proxy_read_timeout` im nginx fehlt (Standard 60 s). |
| Browser-Konsole: `Refused to connect … Content Security Policy` | Der Chat-Server kennt den Agenten noch nicht (erst nach der ersten Meldung). Seite neu laden. |
| „Der Sprach-Server ist gerade ausgelastet“ | Alle Relays haben `MAX_PARTICIPANTS` oder `MAX_MBIT` erreicht. Grenzen erhöhen oder einen weiteren Agenten starten. |
| Stummschalten/Entfernen wirkt nicht | Der Chat-Server erreicht `CONTROL_URL` nicht. Log des **Chat-Servers**: `Sprache: Agent … nicht erreichbar`. Adresse und Proxy-Regel für `/control` prüfen. |
| `429 Zu viele Sprachverbindungen von dieser Adresse` | Mehr als `MAX_PER_IP` (Standard 12; je Person bis zu drei: Sprache, Bildschirm, Kameras) Verbindungen von einer IP. Stehen Agent und Browser hinter einem Proxy, `TRUST_PROXY=1` setzen; sonst sehen alle Verbindungen wie die des Proxys aus. |

---

## 10. Betrieb

**Updates:** Agent und Chat-Server sollten dieselbe Version haben.

```bash
cd deploy/voice && docker compose pull && docker compose up -d
```

Beim Neustart eines Agenten werden seine Teilnehmer getrennt und verbinden sich automatisch neu – auf einem anderen Relay, sobald er als ausgefallen gilt (bis zu etwa 15 Sekunden). Für unterbrechungsfreie Updates mehrere Agenten nacheinander aktualisieren.

**Neustart des Chat-Servers:** Laufende Sprachverbindungen auf Agenten bleiben bestehen. Die Agenten melden ihre Teilnehmer nach spätestens 3 Sekunden wieder. Verloren gehen nur Moderations-Zustände, die im Arbeitsspeicher liegen: Freischaltungen auf der Bühne, Stummschaltungen im Sprachkanal und die einminütige Sperre nach dem Entfernen. Chat-Sperren gelten weiter.

**Schlüssel wechseln:**

1. Neuen Schlüssel beim Chat-Server setzen und neu starten.
2. Danach bei allen Agenten setzen und neu starten.

Dazwischen melden sich die Agenten mit `401`, laufende Gespräche bleiben aber bestehen. Der Signaturschlüssel für die Zutrittskarten ist davon unabhängig – er liegt in `voice.json` im Datenordner des Chats und wird beim Backup mitgesichert.

**Überwachung:**

- `GET /healthz` am Agenten (für Uptime-Monitore).
- `GET /api/admin/voice` am Chat-Server (mit Admin-Token) liefert alle Relays mit `participants`, `mbit`, `estimateMbit` und `healthy` als JSON.

**Grenzen:** Ein Chat-Server nimmt bis zu 64 Agenten an. Ein Sprachkanal liegt immer ganz auf einem Relay; seine Größe begrenzt also die Kapazität des einen Agenten. Viele Kanäle verteilen sich dagegen auf beliebig viele Agenten.

---

## 11. Sicherheit auf einen Blick

- Agenten entscheiden nichts selbst. Ohne gültige, frische Zutrittskarte des Chat-Servers kommt niemand in einen Sprachkanal; jede Karte gilt 60 Sekunden und nur einmal.
- Sprache ist auf dem Weg per TLS verschlüsselt, aber **nicht Ende-zu-Ende**: Wer den Agenten betreibt, könnte die Opus-Rahmen mitschneiden. Betreibe Agenten nur auf Servern, denen du wie dem Chat-Server vertraust.
- Der Agenten-Schlüssel erlaubt das Melden von Teilnehmern und das Ausführen von Steuerbefehlen. Behandle ihn wie ein Passwort: nicht ins Repository, `.env` mit Rechten `600`.
- `/control` nicht öffentlich erreichbar machen (Proxy-Regel aus Abschnitt 7).
- Der Container läuft ohne Root, schreibgeschützt und ohne Linux-Capabilities.
