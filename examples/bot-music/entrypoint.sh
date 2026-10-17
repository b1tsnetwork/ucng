#!/bin/sh
# Startet den Musik-Bot. Mit YTDLP_UPDATE=1 vorher die neueste yt-dlp-Fassung nach /tmp holen
# (YouTube ändert sich oft) – klappt auch bei schreibgeschütztem Container; ein Fehler ist egal.
if [ "${YTDLP_UPDATE:-0}" = 1 ]; then
  mkdir -p /tmp/yt-dlp && cp /opt/yt-dlp/yt-dlp /tmp/yt-dlp/yt-dlp
  if YTDLP_BIN=/tmp/yt-dlp/yt-dlp yt-dlp -U >/dev/null 2>&1; then
    export YTDLP_BIN=/tmp/yt-dlp/yt-dlp
  else
    echo "yt-dlp: Update fehlgeschlagen – nutze die Fassung aus dem Image"
  fi
fi
echo "yt-dlp $(yt-dlp --version 2>/dev/null), $(deno --version 2>/dev/null | head -1)"
exec bot-music "$@"
