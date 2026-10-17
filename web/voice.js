// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Sprachkanäle im Browser: Mikrofon → Opus (WebCodecs) → WebSocket zum Relay; empfangene Rahmen
// je Teilnehmer dekodieren und im Audio-Worklet mischen – jede Person mit eigener Lautstärke.
(() => {
  "use strict";

  const FRAME = 960; // 20 ms bei 48 kHz
  const $ = (id) => document.getElementById(id);
  const t = (k, v) => window._t(k, v);

  const store = {
    json(k, def) { try { return JSON.parse(localStorage.getItem(k)) || def; } catch { return def; } },
    set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* egal */ } },
  };
  const DEFAULTS = { input: "", output: "", mode: "vad", sens: -50, pttKey: "F9", pttLabel: "F9", ns: true, vol: {}, mute: {},
    audioOnly: false, noCapSend: false, captions: true, toneVol: 1, saver: "auto" };
  let prefs = Object.assign({}, DEFAULTS, store.json("ucng:voice", {}));
  const savePrefs = () => store.set("ucng:voice", prefs);

  let deps = null; // von app.js: { api, toast, me, isBlocked, avatar, h, iconBtn, switchRoom, render }
  const roomPeers = {}; // Kanal → [{i,u,n,s,m,sm,h}] (vom Server, für die Seitenleiste)
  const speaking = new Set(); // Nutzer-IDs, die gerade zu hören sind
  const reactions = new Map(); // Nutzer-ID → { e, at, until } (laufende Reaktion)

  // Verbindung
  let S = null; // { room, ws, ctx, play, cap, enc, stream, decoders, peers, you, ... }
  let wantRoom = ""; // gewünschter Kanal (für Wiederverbinden)
  let retries = 0;
  let rewatchAfter = ""; // nach dem Neuverbinden der Sprache dort weiter zusehen (Nutzer-ID)
  let pttDown = false;
  let vadUntil = 0;
  let selfMute = false;
  let deaf = false;

  const ICON = {
    mic: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="7" y="2.5" width="6" height="10" rx="3" fill="none" stroke="currentColor" stroke-width="1.7"/><path d="M4.5 9.5a5.5 5.5 0 0 0 11 0M10 15v2.5" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>',
    micOff: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="7" y="2.5" width="6" height="10" rx="3" fill="none" stroke="currentColor" stroke-width="1.7"/><path d="M4.5 9.5a5.5 5.5 0 0 0 11 0M10 15v2.5" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/><path d="m3 3 14 14" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>',
    head: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3.5 13v-3a6.5 6.5 0 0 1 13 0v3" fill="none" stroke="currentColor" stroke-width="1.7"/><rect x="2.5" y="11.5" width="4" height="5.5" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.7"/><rect x="13.5" y="11.5" width="4" height="5.5" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.7"/></svg>',
    headOff: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3.5 13v-3a6.5 6.5 0 0 1 13 0v3" fill="none" stroke="currentColor" stroke-width="1.7"/><rect x="2.5" y="11.5" width="4" height="5.5" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.7"/><rect x="13.5" y="11.5" width="4" height="5.5" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.7"/><path d="m3 3 14 14" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>',
    hand: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M7 10V4.5a1.2 1.2 0 0 1 2.4 0V9m0-5.5V3.2a1.2 1.2 0 0 1 2.4 0V9m0-4.6a1.2 1.2 0 0 1 2.4 0V10m0-3.4a1.2 1.2 0 0 1 2.3 0v4.9a6 6 0 0 1-6 6h-.6a5.5 5.5 0 0 1-4.3-2.1L3.4 12.8a1.3 1.3 0 0 1 2-1.7L7 12.6" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    gear: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="2.6" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M10 2.5v2M10 15.5v2M17.5 10h-2M4.5 10h-2M15.3 4.7l-1.4 1.4M6.1 13.9l-1.4 1.4M15.3 15.3l-1.4-1.4M6.1 6.1 4.7 4.7" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
    leave: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3.2 11.6c4-3.6 9.6-3.6 13.6 0l-1.6 2.2-2.8-1.1v-2a8.3 8.3 0 0 0-4.8 0v2l-2.8 1.1z" fill="currentColor"/></svg>',
    speaker: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3.5 8h3l4-3.5v11l-4-3.5h-3z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="M13.5 7.2a4 4 0 0 1 0 5.6M15.6 5a7 7 0 0 1 0 10" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
    join: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3.5 13v-3a6.5 6.5 0 0 1 13 0v3" fill="none" stroke="currentColor" stroke-width="1.7"/><rect x="2.5" y="11.5" width="4" height="5.5" rx="1.6" fill="currentColor"/><rect x="13.5" y="11.5" width="4" height="5.5" rx="1.6" fill="currentColor"/></svg>',
    screen: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="2.5" y="3.5" width="15" height="10" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M7 17h6M10 13.5V17" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
    screenOff: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="2.5" y="3.5" width="15" height="10" rx="1.6" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M7 17h6M10 13.5V17" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><path d="m7.5 6 5 5m0-5-5 5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
    cam: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="2.5" y="5.5" width="10.5" height="9" rx="2" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m13 9 4.5-2.5v7L13 11" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/></svg>',
    camOff: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="2.5" y="5.5" width="10.5" height="9" rx="2" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m13 9 4.5-2.5v7L13 11" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="m3 3 14 14" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>',
    music: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M7.5 15V4.5l9-2V13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><circle cx="5.5" cy="15" r="2" fill="currentColor"/><circle cx="14.5" cy="13" r="2" fill="currentColor"/></svg>',
    react: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="7.2" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M7 11.8c.8 1.1 1.8 1.7 3 1.7s2.2-.6 3-1.7" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><circle cx="7.6" cy="8.2" r="1" fill="currentColor"/><circle cx="12.4" cy="8.2" r="1" fill="currentColor"/></svg>',
    more: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="4.5" cy="10" r="1.6" fill="currentColor"/><circle cx="10" cy="10" r="1.6" fill="currentColor"/><circle cx="15.5" cy="10" r="1.6" fill="currentColor"/></svg>',
    shield: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M10 2.5 4 5v4.5c0 3.8 2.6 6.6 6 8 3.4-1.4 6-4.2 6-8V5z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/></svg>',
    rec: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="5" fill="currentColor"/></svg>',
    stage: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M2.5 16.5h15M4.5 16.5V9l5.5-4.5L15.5 9v7.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><circle cx="10" cy="10.5" r="1.8" fill="currentColor"/></svg>',
  };

  // ---------- Unterstützung ----------

  function unsupported() {
    if (!window.isSecureContext) return t("voice.need_https");
    if (!window.AudioEncoder || !window.AudioDecoder || !window.AudioWorkletNode || !navigator.mediaDevices) return t("voice.unsupported");
    return "";
  }

  // ---------- Lautstärke je Person ----------

  const musicVol = {}; // Musik von Bots: voreingestellte Lautstärke (0–1)
  function gainFor(uid) {
    if (deps.isBlocked(uid) || prefs.mute[uid]) return 0;
    const v = prefs.vol[uid];
    if (v != null) return v;
    return musicVol[uid] != null ? musicVol[uid] : 1;
  }
  function setVolume(uid, v) {
    if (Math.abs(v - 1) < 0.01) delete prefs.vol[uid]; else prefs.vol[uid] = Math.round(v * 100) / 100;
    savePrefs();
    applyGains();
  }
  function setLocalMute(uid, on) {
    if (on) prefs.mute[uid] = true; else delete prefs.mute[uid];
    savePrefs();
    applyGains();
    renderAll();
  }
  function applyGains() {
    if (!S || !S.play) return;
    for (const p of S.peers.values()) S.play.port.postMessage({ type: "gain", idx: p.i, gain: gainFor(p.u) });
  }

  // ---------- Verbinden ----------

  async function join(room, moved) {
    const why = unsupported();
    if (why) { deps.toast(why); return; }
    if (S && S.room === room) return;
    if (deps.requireName && !deps.requireName()) return;
    if (!moved && deps.isLocked && deps.isLocked(room)) { // Passwort zuerst
      if (!(await deps.unlock(room))) return;
    }
    await leave(true);
    wantRoom = room;
    retries = 0;
    await connect(room);
  }

  async function connect(room) {
    let j;
    S = { room, status: "connecting", peers: new Map(), decoders: new Map(), seq: 0, ts: 0, ping: 0, level: 0 };
    renderAll();
    try {
      const lang = ((deps.locale && deps.locale()) || "").split("-")[0]; // Hinweis für die Spracherkennung
      j = await deps.api(`/api/voice/${encodeURIComponent(room)}/join?lang=${encodeURIComponent(lang)}`, { method: "POST", body: "{}" });
    } catch (e) {
      deps.toast(e.message);
      S = null; wantRoom = "";
      renderAll();
      return;
    }
    if (!S || S.room !== room) return;
    Object.assign(S, { cfg: j, speak: j.speak, muted: j.muted, stage: j.stage, moderate: j.moderate, guest: j.guest, hand: false });
    try {
      S.ctx = new AudioContext({ sampleRate: 48000, latencyHint: "interactive" });
      await S.ctx.audioWorklet.addModule("/voice-worklet.js");
      S.play = new AudioWorkletNode(S.ctx, "ucng-playback", { numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [2] });
      S.play.port.onmessage = (e) => onLevels(e.data.levels);
      if (!(X() && X().routeAudio(S))) S.play.connect(S.ctx.destination); // mobil: über <audio> (läuft im Hintergrund weiter)
      S.play.port.postMessage({ type: "master", gain: deaf ? 0 : 1 });
      if (prefs.output && S.ctx.setSinkId) S.ctx.setSinkId(prefs.output).catch(() => {});
      if (S.ctx.state === "suspended") await S.ctx.resume();
    } catch (e) {
      deps.toast(t("voice.audio_failed"));
      teardown();
      return;
    }
    const ws = new WebSocket(absURL(j.url));
    ws.binaryType = "arraybuffer";
    S.ws = ws;
    S.media = {};
    const mine = S;
    if (j.mediaConn) { // Bildschirm und Kameras über eigene Verbindungen (nach dem „hello“)
      ws.onopen = () => ws.send(JSON.stringify({ t: "auth", ticket: j.ticket }));
      ws.onmessage = (e) => { if (S === mine) onMessage(e.data); };
      ws.onclose = (e) => { if (S === mine) onClose(e); };
      return;
    }
    // ältere Relays: alles über die Sprachverbindung
    if (window.UCNGScreen) {
      UCNGScreen.attach({
        cfg: j, toast: deps.toast, render: () => { if (S === mine) renderAll(); },
        ws: () => (S === mine ? mine.ws : null),
        sendText: (o) => { if (S === mine && ws.readyState === 1) ws.send(JSON.stringify(o)); },
        sendBin: (b) => { if (S === mine && ws.readyState === 1) ws.send(b); },
        peer: (idx) => mine.peers.get(idx),
        peers: () => [...mine.peers.values()],
        screenOK: () => S === mine && !!mine.screenOK,
        ...extraLink(mine),
      });
    }
    if (window.UCNGVideo) {
      UCNGVideo.attach({
        cfg: j, toast: deps.toast, render: () => { if (S === mine) renderBar(); },
        ws: () => (S === mine ? mine.ws : null),
        sendText: (o) => { if (S === mine && ws.readyState === 1) ws.send(JSON.stringify(o)); },
        sendBin: (b) => { if (S === mine && ws.readyState === 1) ws.send(b); },
        peer: (idx) => mine.peers.get(idx),
        peers: () => [...mine.peers.values()],
        you: () => mine.you,
        videoOK: () => S === mine && !!mine.videoOK,
        speaking: (uid) => speaking.has(uid),
        ...extraLink(mine),
      });
    }
    ws.onopen = () => ws.send(JSON.stringify({ t: "auth", ticket: j.ticket }));
    ws.onmessage = (e) => { if (S === mine) onMessage(e.data); };
    ws.onclose = (e) => { if (S === mine) onClose(e); };
  }

  // gemeinsam für beide Varianten: Verschlüsselung, Ton der Freigabe, Einstellungen
  function extraLink(s) {
    return {
      e2ee: () => (s.cfg && s.cfg.e2ee ? window.UCNGE2EE : null),
      playTone: (key, frame) => { if (S === s && s.play && !deaf) s.play.port.postMessage({ type: "frame", idx: key, frame }, frame.map((f) => f.buffer)); },
      toneGain: (key, g) => { if (S === s && s.play) s.play.port.postMessage({ type: "gain", idx: key, gain: g }); },
      toneRemove: (key) => { if (S === s && s.play) s.play.port.postMessage({ type: "remove", idx: key }); },
      prefs: () => prefs, savePrefs,
    };
  }

  function absURL(u) { return u.startsWith("/") ? (location.protocol === "https:" ? "wss://" : "ws://") + location.host + u : u; }

  // ---------- Medienverbindungen (Bildschirm, Kameras) ----------
  // Je Art eine eigene WebSocket – getrennt von der Sprache, damit ein großes Bild nie die Sprache
  // aufhält, und auf Wunsch des Chat-Servers auf einem anderen Sprach-Server (Lastverteilung).

  const MOD = { screen: () => window.UCNGScreen, video: () => window.UCNGVideo };
  const E2 = () => (S && S.cfg && S.cfg.e2ee ? window.UCNGE2EE : null); // Ende-zu-Ende (nur in solchen Kanälen)
  const X = () => window.UCNGExtras || null; // Aufnahme, Untertitel, Breakout, Statistik, Mobil (voice-extras.js)

  function mediaLink(s, m) {
    const kind = m.kind;
    const open = () => S === s && s.media[kind] === m && m.ws && m.ws.readyState === 1;
    return {
      cfg: s.cfg, toast: deps.toast,
      render: () => { if (S === s) { if (kind === "video") renderBar(); else renderAll(); } },
      ws: () => (S === s && s.media[kind] === m ? m.ws : null),
      sendText: (o) => { if (open()) m.ws.send(JSON.stringify(o)); },
      sendBin: (b) => { if (open()) m.ws.send(b); },
      peer: (idx) => m.peers.get(idx),
      peers: () => [...m.peers.values()],
      you: () => m.you,
      screenOK: () => S === s && !!m.screenOK,
      videoOK: () => S === s && !!m.videoOK,
      speaking: (uid) => speaking.has(uid),
      prepare: () => prepareMedia(s, kind),
      hint: (codec) => sendHint(s, kind, codec),
      ...extraLink(s),
    };
  }

  // openMedia verbindet eine Art (neu). pre: schon geholte Zutrittskarte. Liefert, ob es klappte.
  async function openMedia(s, kind, pre, publish) {
    if (S !== s || prefs.audioOnly) return false;
    const old = s.media[kind];
    rememberWatch(s, kind);
    if (old) closeMedia(s, kind, true); // neu verbinden: laufende Freigabe/Kamera behalten
    const m = { kind, peers: new Map(), you: 0, status: "connecting", retries: old ? old.retries : 0 };
    s.media[kind] = m;
    let j = pre;
    if (!j) {
      try {
        j = await deps.api(`/api/voice/${encodeURIComponent(s.room)}/media`, { method: "POST", body: JSON.stringify({ kind, publish: !!publish }) });
      } catch (e) {
        if (S === s && s.media[kind] === m) retryMedia(s, m);
        return false;
      }
    }
    if (S !== s || s.media[kind] !== m) return false;
    m.relay = j.relay; m.offloaded = !!j.offloaded;
    const ws = new WebSocket(absURL(j.url));
    ws.binaryType = "arraybuffer";
    m.ws = ws;
    const mod = MOD[kind]();
    if (mod) mod.attach(mediaLink(s, m));
    return new Promise((resolve) => {
      m.ready = resolve;
      ws.onopen = () => ws.send(JSON.stringify({ t: "auth", ticket: j.ticket }));
      ws.onmessage = (e) => { if (S === s && s.media[kind] === m) onMediaMessage(s, m, e.data); };
      ws.onclose = (e) => { resolve(false); if (S === s && s.media[kind] === m) onMediaClose(s, m, e); };
    });
  }

  // ---------- Sparmodus: weniger Video empfangen ----------
  // Pause, solange nichts zu sehen ist (Tab im Hintergrund, Ansicht zu – außer Bild-in-Bild), und
  // auf Wunsch nur jedes 2. bzw. 4. Bild (der Relay lässt obere Zeitebenen weg; der Sender
  // muss dafür nichts ändern). Spart vor allem auf der Leitung zu mir.
  const SAVER = ["auto", 0, 1, 2];
  function saverDrop() {
    if (prefs.saver === 0 || prefs.saver === 1 || prefs.saver === 2) return prefs.saver;
    const slow = S && S.qSent === 1, save = navigator.connection && navigator.connection.saveData;
    return slow || save ? 1 : 0;
  }
  function viewOpen(kind) {
    const el = $(kind === "screen" ? "screenView" : "confView");
    return !el || !el.hidden;
  }
  function sendLimits() {
    if (!S || S.status !== "connected") return;
    const pip = !!document.pictureInPictureElement, drop = saverDrop();
    const conns = S.cfg && S.cfg.mediaConn ? Object.values(S.media || {})
      : [{ kind: "", ws: S.ws, holder: S }]; // ältere Relays: alles über die Sprachverbindung
    for (const m of conns) {
      if (!m.ws || m.ws.readyState !== 1 || (m.kind && m.status !== "connected")) continue;
      const pause = !pip && (document.hidden || (!!m.kind && !viewOpen(m.kind)));
      const msg = JSON.stringify({ t: "limit", pause, drop });
      const h = m.holder || m;
      if (h.limSent === msg) continue;
      h.limSent = msg;
      m.ws.send(msg);
    }
  }
  let limitQueued = false;
  function queueLimits() {
    if (limitQueued) return;
    limitQueued = true;
    setTimeout(() => { limitQueued = false; sendLimits(); }, 120);
  }
  document.addEventListener("visibilitychange", queueLimits);
  document.addEventListener("enterpictureinpicture", queueLimits, true);
  document.addEventListener("leavepictureinpicture", queueLimits, true);
  const watchViews = () => {
    for (const id of ["confView", "screenView"]) {
      const el = $(id);
      if (el) new MutationObserver(queueLimits).observe(el, { attributes: true, attributeFilter: ["hidden"] });
    }
  };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", watchViews); else watchViews();
  function setSaver(v) {
    prefs.saver = SAVER.includes(v) ? v : "auto";
    savePrefs();
    sendLimits();
    renderAll();
  }
  function saverLabel() {
    const v = prefs.saver;
    return t("voice.saver", { mode: v === 0 ? t("voice.saver_off") : v === 1 ? t("voice.saver_half") : v === 2 ? t("voice.saver_quarter") : t("voice.saver_auto") });
  }

  // Wem sehe ich gerade zu? Nach dem Neuverbinden geht es dort weiter.
  function rememberWatch(s, kind) {
    if (kind === "screen" && window.UCNGScreen && UCNGScreen.watchingUser) s.rewatch = UCNGScreen.watchingUser;
  }

  function closeMedia(s, kind, keep) {
    const m = s.media[kind];
    if (!m) return;
    delete s.media[kind];
    clearTimeout(m.timer);
    clearInterval(m.pinger);
    const mod = MOD[kind]();
    if (mod && S === s) { if (keep && mod.suspend) mod.suspend(); else mod.detach(); }
    if (m.ws) { m.ws.onclose = null; m.ws.onmessage = null; try { m.ws.close(1000); } catch { /* egal */ } }
    if (m.ready) m.ready(false);
    if (!keep) sendHint(s, kind, "");
  }

  function retryMedia(s, m) {
    if (m.retries >= 6) { m.status = "failed"; deps.toast(t("voice.media_lost")); return; }
    m.retries++;
    m.status = "reconnecting";
    const wait = Math.min(15000, 600 * 2 ** m.retries) * (0.7 + Math.random() * 0.6);
    m.timer = setTimeout(() => { if (S === s && s.media[m.kind] === m) openMedia(s, m.kind); }, wait);
  }

  // Vor dem Senden: Der Chat-Server darf die Art jetzt umziehen lassen (dann neu verbinden).
  async function prepareMedia(s, kind) {
    const m = s.media[kind];
    let j;
    try {
      j = await deps.api(`/api/voice/${encodeURIComponent(s.room)}/media`, { method: "POST", body: JSON.stringify({ kind, publish: true }) });
    } catch (e) { deps.toast(e.message); return false; }
    if (S !== s) return false;
    if (m && s.media[kind] === m && m.status === "connected" && j.relay === m.relay) return true;
    return openMedia(s, kind, j);
  }

  // Hinweis auf der Sprachverbindung: „teile den Bildschirm“ / „Kamera an“ (für die Seitenleiste)
  function sendHint(s, kind, codec) {
    if (S !== s || !s.ws || s.ws.readyState !== 1 || !s.cfg || !s.cfg.mediaConn) return;
    s.ws.send(JSON.stringify(kind === "screen" ? { t: "state", sc: codec || "" } : { t: "state", cm: codec || "" }));
  }

  function onMediaMessage(s, m, data) {
    const mod = MOD[m.kind]();
    if (typeof data !== "string") {
      const b = new Uint8Array(data);
      if (!mod) return;
      if (b[0] === 2 && m.kind === "screen") mod.onVideo(b);
      else if (b[0] === 4 && m.kind === "screen") mod.onTone(b);
      else if (b[0] === 3 && m.kind === "video") mod.onCam(b);
      return;
    }
    let msg;
    try { msg = JSON.parse(data); } catch { return; }
    switch (msg.t) {
      case "hello":
        m.status = "connected"; m.you = msg.you; m.retries = 0;
        m.screenOK = !!msg.screen; m.videoOK = !!msg.video;
        for (const p of msg.peers || []) m.peers.set(p.i, p);
        if (m.kind === "video" && mod) mod.setFeatured(msg.featured);
        if (mod) for (const p of m.peers.values()) mod.onPeer(p);
        // Lebenszeichen, auch wenn ich selbst nichts sende (nur zusehe)
        clearInterval(m.pinger);
        m.pinger = setInterval(() => { if (m.ws && m.ws.readyState === 1) m.ws.send('{"t":"ping"}'); }, 10000);
        m.limSent = ""; queueLimits();
        if (m.kind === "screen" && (s.rewatch || rewatchAfter) && mod) { // nach dem Neuverbinden weiter zusehen
          const uid = s.rewatch || rewatchAfter;
          s.rewatch = ""; rewatchAfter = "";
          const p = [...m.peers.values()].find((x) => x.u === uid && x.sc);
          if (p) mod.watch(p.i);
        }
        if (m.ready) { m.ready(true); m.ready = null; }
        if (mod && mod.resume) mod.resume(); // laufende Freigabe/Kamera nach einem Abriss fortsetzen
        break;
      case "peer": m.peers.set(msg.peer.i, msg.peer); if (mod) mod.onPeer(msg.peer); break;
      case "leave": m.peers.delete(msg.i); if (mod) mod.onLeave(msg.i); break;
      case "you":
        m.screenOK = !!msg.screen; m.videoOK = !!msg.video;
        if (m.kind === "video" && !msg.video && mod && mod.on) mod.stopCam();
        break;
      case "bye": m.bye = msg.reason || "kicked"; break;
      case "pong": break;
      default: if (mod) mod.onText(msg);
    }
    renderAll();
  }

  function onMediaClose(s, m, e) {
    const mod = MOD[m.kind]();
    clearInterval(m.pinger);
    rememberWatch(s, m.kind);
    if (m.bye || e.code === 4003 || e.code === 4000 || e.code === 1000) {
      if (mod) mod.detach();
      sendHint(s, m.kind, "");
      m.status = "closed"; s.rewatch = ""; renderAll(); return;
    }
    // Abriss: eigene Freigabe/Kamera weiterlaufen lassen und nach dem Neuverbinden fortsetzen
    if (mod) { if (mod.suspend) mod.suspend(); else mod.detach(); }
    if (e.code === 4409) deps.toast(t("voice.media_busy"));
    retryMedia(s, m);
    renderAll();
  }

  function onMessage(data) {
    if (typeof data !== "string") {
      const b = new Uint8Array(data);
      if (b[0] === 2) { if (window.UCNGScreen) UCNGScreen.onVideo(b); }
      else if (b[0] === 4) { if (window.UCNGScreen) UCNGScreen.onTone(b); }
      else if (b[0] === 3) { if (window.UCNGVideo) UCNGVideo.onCam(b); }
      else onAudio(b);
      return;
    }
    let m;
    try { m = JSON.parse(data); } catch { return; }
    switch (m.t) {
      case "hello":
        S.status = "connected";
        S.you = m.you;
        S.speak = m.speak; S.muted = m.muted; S.screenOK = !!m.screen; S.videoOK = !!m.video;
        retries = 0;
        if (window.UCNGVideo) UCNGVideo.setFeatured(m.featured);
        for (const p of m.peers || []) upsertPeer(p);
        sendState();
        if (S.speak) startMic();
        clearInterval(S.pinger);
        S.pinger = setInterval(() => {
          if (S && S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify({ t: "ping", ts: Math.round(performance.now()) }));
          measureQuality();
          // Sparmodus regelmäßig bestätigen (Qualität kann ihn ändern; eine verlorene Nachricht heilt so)
          S.limSent = ""; for (const m of Object.values(S.media || {})) m.limSent = "";
          sendLimits();
        }, 5000);
        S.limSent = ""; queueLimits();
        S.ws.send(JSON.stringify({ t: "ping", ts: Math.round(performance.now()) }));
        deps.toast(t("voice.joined", { room: S.room }));
        if (S.cfg.mediaConn) {
          if (S.cfg.screenOn) openMedia(S, "screen");
          if (S.cfg.videoOn) openMedia(S, "video");
        }
        if (E2()) {
          const me2 = S;
          E2().start({ sendText: (o) => { if (S === me2 && me2.ws && me2.ws.readyState === 1) me2.ws.send(JSON.stringify(o)); },
            peers: () => [...me2.peers.values()], you: () => me2.you, onChange: () => { if (S === me2) renderAll(); } });
        }
        if (!S.cfg.mediaConn) { // ältere Relays: Freigabe/Kamera über diese Verbindung fortsetzen
          if (window.UCNGScreen && UCNGScreen.resume) UCNGScreen.resume();
          if (window.UCNGVideo && UCNGVideo.resume) UCNGVideo.resume();
        }
        if (X()) X().onJoined(S);
        break;
      case "e2k": if (E2()) E2().onKey(m); break;
      case "react": if (X()) X().onReact(m); break;
      case "cap": if (X()) X().onCaption(m); break;
      case "tap": { // Live-Transkription an/aus (vom Relay, für alle im Sprachkanal)
        const on = m.mode === 2;
        const note = (k) => (window.UCNGTranscript ? UCNGTranscript.notify(t(k)) : deps.toast(t(k)));
        if (on) note("voice.tr_started_voice");
        if (!on && S.cfg.transcript) note("voice.tr_ended_toast");
        S.cfg.transcript = on ? (S.cfg.transcript || { active: true, started: Date.now() }) : null;
        renderAll();
        break;
      }
      case "forcemute": // „alle stummschalten“ – selbst wieder einschaltbar
        if (!selfMute) { selfMute = true; sendState(); deps.toast(t("voice.forcemuted")); }
        break;
      case "askunmute":
        if (selfMute || deaf) { S.askUnmute = true; deps.toast(t("voice.asked_unmute")); }
        break;
      case "media": // eine Art ist auf einen anderen Sprach-Server umgezogen
        if (S.media && S.media[m.kind] && S.media[m.kind].relay !== m.relay) openMedia(S, m.kind);
        break;
      case "peer":
        upsertPeer(m.peer);
        if (!S.cfg.mediaConn) { if (window.UCNGScreen) UCNGScreen.onPeer(m.peer); if (window.UCNGVideo) UCNGVideo.onPeer(m.peer); }
        if (E2()) E2().peersChanged();
        if (X()) X().onPeer(m.peer);
        break;
      case "leave":
        removePeer(m.i);
        if (!S.cfg.mediaConn) { if (window.UCNGScreen) UCNGScreen.onLeave(m.i); if (window.UCNGVideo) UCNGVideo.onLeave(m.i); }
        if (E2()) E2().peersChanged();
        break;
      case "cam": case "layer": case "camkey": case "featured": case "degraded":
        if (window.UCNGVideo) UCNGVideo.onText(m);
        break;
      case "move": // vom Team in einen anderen Sprachkanal geschickt
        if (m.room) { deps.toast(t("voice.moved", { room: m.room })); const to = m.room; setTimeout(() => { leave(true).then(() => join(to, true)); }, 50); }
        break;
      case "share": case "watch": case "keyreq":
        if (window.UCNGScreen) UCNGScreen.onText(m);
        break;
      case "you":
        if (m.speak && !S.speak) deps.toast(t("voice.you_can_speak"));
        if (!m.speak && S.speak) deps.toast(t("voice.you_cannot_speak"));
        if (m.muted && !S.muted) deps.toast(t("voice.you_were_muted"));
        S.speak = m.speak; S.muted = m.muted; S.hand = m.hand; S.screenOK = !!m.screen; S.videoOK = !!m.video;
        if (m.mod != null) S.moderate = !!m.mod;
        if (!m.video && window.UCNGVideo && UCNGVideo.on) UCNGVideo.stopCam();
        if (S.peers.has(S.you)) Object.assign(S.peers.get(S.you), { s: m.speak, m: m.muted, h: m.hand }); // eigene Zeile
        if (S.speak) startMic(); else stopMic();
        break;
      case "pong": S.ping = Math.max(1, Math.round(performance.now() - m.ts)); break;
      case "bye": S.bye = m.reason || "kicked"; break;
    }
    renderAll();
  }

  function onClose(e) {
    const room = S.room, bye = S.bye;
    rewatchAfter = window.UCNGScreen && UCNGScreen.watchingUser || S.rewatch || "";
    teardown(true); // Freigabe und Kamera erst einmal behalten (Fortsetzen nach dem Neuverbinden)
    const code = e.code;
    const final = (msg) => { if (msg) deps.toast(msg); wantRoom = ""; detachModules(); renderAll(); };
    if (bye) { final(byeText(bye)); return; }
    if (code === 1000 || (code === 1001 && !wantRoom)) { detachModules(); renderAll(); return; }
    if (code === 4000) { final(t("voice.other_window")); return; }
    if (code === 4003) { final(byeText(e.reason)); return; }
    if (code === 4409) { final(/voll|full/i.test(e.reason) ? t("voice.full") : t("voice.busy")); return; }
    if (wantRoom === room && retries < 6) {
      retries++;
      const wait = Math.min(15000, 800 * 2 ** retries) * (0.7 + Math.random() * 0.6);
      S = { room, status: "reconnecting", peers: new Map(), decoders: new Map() };
      renderAll();
      setTimeout(() => { if (wantRoom === room && S && S.status === "reconnecting") connect(room); }, wait);
      return;
    }
    if (wantRoom) deps.toast(t("voice.lost"));
    wantRoom = "";
    detachModules();
    renderAll();
  }

  // endgültig: laufende Freigabe und Kamera beenden
  function detachModules() {
    if (window.UCNGScreen) UCNGScreen.detach();
    if (window.UCNGVideo) UCNGVideo.detach();
  }

  function byeText(code) {
    const known = { kicked: "voice.bye_kicked", moved: "voice.bye_moved", deleted: "voice.bye_deleted", renamed: "voice.bye_renamed", ended: "voice.bye_ended",
      session: "voice.bye_session", banned: "voice.bye_banned", noguest: "voice.bye_noguest", novoice: "voice.bye_novoice", noaccess: "voice.bye_noaccess" };
    return t(known[code] || "voice.bye_kicked");
  }

  function safe(fn) {
    try { fn(); } catch (e) { console.error("UCNG Sprache:", e); }
  }

  // teardown räumt eine Verbindung ab (ohne Meldung).
  function teardown(keepCapture) {
    if (!S) return;
    const s = S;
    S = null;
    clearInterval(s.pinger);
    if (keepCapture) { // Abriss: Aufnahme von Bildschirm/Kamera behalten
      if (window.UCNGScreen && UCNGScreen.suspend) UCNGScreen.suspend(); else if (window.UCNGScreen) UCNGScreen.detach();
      if (window.UCNGVideo && UCNGVideo.suspend) UCNGVideo.suspend(); else if (window.UCNGVideo) UCNGVideo.detach();
    } else safe(detachModules);
    // Zusätze dürfen das Abräumen nie aufhalten – sonst bliebe die Verbindung offen und die
    // Sprachleiste zeigte weiter „verbunden“.
    if (window.UCNGE2EE) safe(() => UCNGE2EE.stop());
    if (X()) safe(() => X().onLeft(s, !!keepCapture));
    for (const m of Object.values(s.media || {})) {
      clearTimeout(m.timer); clearInterval(m.pinger);
      if (m.ws) { m.ws.onclose = null; m.ws.onmessage = null; try { m.ws.close(1000); } catch { /* egal */ } }
      if (m.ready) m.ready(false);
    }
    s.media = {};
    if (s.ws) { s.ws.onclose = null; s.ws.onmessage = null; try { s.ws.close(1000); } catch { /* egal */ } }
    stopMicOf(s);
    for (const d of (s.decoders || new Map()).values()) try { d.close(); } catch { /* egal */ }
    if (s.ctx) s.ctx.close().catch(() => {});
    speaking.clear();
  }

  async function leave(silent) {
    wantRoom = "";
    rewatchAfter = "";
    if (!S) { renderAll(); return; }
    teardown();
    if (!silent) deps.toast(t("voice.left"));
    renderAll();
  }

  // ---------- Teilnehmer ----------

  function upsertPeer(p) {
    if (p.mu) musicVol[p.u] = p.mu / 100;
    const old = S.peers.get(p.i);
    S.peers.set(p.i, p);
    if ((!old || old.mu !== p.mu) && S.play) S.play.port.postMessage({ type: "gain", idx: p.i, gain: gainFor(p.u) }); // auch: Bot ändert die Lautstärke
    if (p.i === S.you) { S.hand = !!p.h; S.muted = p.m; S.speak = p.s; }
    if (!!(old && old.h) !== !!p.h) {
      if (window.UCNGVideo && UCNGVideo.refresh) queueMicrotask(() => UCNGVideo.refresh()); // große Hand auf der Kachel
      if (p.h && old && p.i !== S.you && S.moderate) handAlert(p); // neue Meldung: Moderierende bekommen Ton und Hinweis
    }
  }

  // Wer moderiert, hört bei einer neuen Meldung einen kurzen Doppelton und sieht einen Hinweis.
  let lastHandTone = 0;
  function handAlert(p) {
    const n = handOrder(S.room).indexOf(p.u) + 1;
    deps.toast(t("voice.hand_new", { name: p.n, n: n || 1 }));
    const now = performance.now();
    if (deaf || now - lastHandTone < 1500 || !S.ctx) return;
    lastHandTone = now;
    try {
      const c = S.ctx, t0 = c.currentTime;
      [[660, 0], [990, 0.14]].forEach(([f, d]) => {
        const o = c.createOscillator(), g = c.createGain();
        o.type = "triangle"; o.frequency.value = f;
        g.gain.setValueAtTime(0.0001, t0 + d);
        g.gain.exponentialRampToValueAtTime(0.12, t0 + d + 0.02);
        g.gain.exponentialRampToValueAtTime(0.0001, t0 + d + 0.25);
        o.connect(g).connect(c.destination);
        o.start(t0 + d); o.stop(t0 + d + 0.3);
      });
    } catch { /* kein Ton möglich */ }
  }

  // Reihenfolge der Meldungen (wer zuerst die Hand gehoben hat, ist dran): Liste von Nutzer-IDs.
  function handOrder(room) {
    return peersOf(room).filter((p) => p.h && !p.mu).sort((a, b) => (a.ha || 0) - (b.ha || 0) || a.i - b.i).map((p) => p.u);
  }

  function removePeer(idx) {
    const p = S.peers.get(idx);
    S.peers.delete(idx);
    const d = S.decoders.get(idx);
    if (d) { try { d.close(); } catch { /* egal */ } S.decoders.delete(idx); }
    if (S.play) S.play.port.postMessage({ type: "remove", idx });
    if (p) speaking.delete(p.u);
  }

  function sendState() {
    if (S && S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify({ t: "state", mute: selfMute || deaf, hand: !!S.hand, nc: !!prefs.noCapSend }));
  }

  // ---------- Mikrofon ----------

  async function startMic() {
    if (!S || S.stream || S.micStarting) return;
    S.micStarting = true;
    const s = S;
    const music = !!s.cfg.music, stereo = !!s.cfg.stereo;
    const audio = { echoCancellation: !music, noiseSuppression: !music && prefs.ns, autoGainControl: !music, channelCount: stereo ? 2 : 1, sampleRate: 48000 };
    if (prefs.input) audio.deviceId = { exact: prefs.input };
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ audio });
    } catch (e) {
      if (prefs.input && e.name === "OverconstrainedError") { prefs.input = ""; savePrefs(); s.micStarting = false; return startMic(); }
      s.micStarting = false;
      deps.toast(t("voice.mic_denied"));
      return;
    }
    s.micStarting = false;
    if (S !== s || !s.speak) { stream.getTracks().forEach((x) => x.stop()); return; }
    const channels = stereo ? 2 : 1;
    const config = { codec: "opus", sampleRate: 48000, numberOfChannels: channels, bitrate: s.cfg.bitrate,
      opus: { frameDuration: 20000, complexity: 9, useinbandfec: !music, packetlossperc: music ? 0 : 5 } };
    try {
      const ok = await AudioEncoder.isConfigSupported(config);
      if (!ok.supported) throw new Error("opus");
      s.enc = new AudioEncoder({ output: (chunk) => sendChunk(s, chunk), error: () => { s.enc = null; } });
      s.enc.configure(config);
    } catch {
      stream.getTracks().forEach((x) => x.stop());
      deps.toast(t("voice.unsupported"));
      return;
    }
    s.stream = stream;
    try {
      s.src = s.ctx.createMediaStreamSource(stream);
    } catch { // z. B. Firefox, wenn das Mikrofon nicht mit 48 kHz liefert
      stopMicOf(s);
      deps.toast(t("voice.mic_rate"));
      return;
    }
    s.cap = new AudioWorkletNode(s.ctx, "ucng-capture", { numberOfInputs: 1, numberOfOutputs: 0, processorOptions: { channels } });
    s.cap.port.onmessage = (e) => onCapture(s, e.data);
    s.src.connect(s.cap);
    renderAll();
  }

  function stopMic() { if (S) stopMicOf(S); }
  function stopMicOf(s) {
    if (s.src) try { s.src.disconnect(); } catch { /* egal */ }
    if (s.cap) { s.cap.port.onmessage = null; try { s.cap.disconnect(); } catch { /* egal */ } }
    if (s.stream) s.stream.getTracks().forEach((x) => x.stop());
    if (s.enc && s.enc.state !== "closed") try { s.enc.close(); } catch { /* egal */ }
    s.src = s.cap = s.stream = s.enc = null;
    s.level = 0;
    if (s.sending) { s.sending = false; if (S === s) renderSelf(); }
  }

  async function restartMic() {
    if (!S || !S.stream) return;
    stopMic();
    await startMic();
  }

  function transmitting(s) {
    if (!s.speak || s.muted || selfMute || deaf || !s.ws || s.ws.readyState !== 1) return false;
    return prefs.mode === "ptt" ? pttDown : performance.now() < vadUntil;
  }

  function onCapture(s, d) {
    const db = d.rms > 0 ? 20 * Math.log10(d.rms) : -100;
    s.level = db;
    if (db > prefs.sens) vadUntil = performance.now() + 350;
    const on = transmitting(s);
    if (on !== s.sending) { s.sending = on; renderSelf(); }
    if (levelCb) levelCb(db);
    if (!d.frame || !on || !s.enc || s.enc.state !== "configured") return;
    const ch = d.frame.length;
    const data = new Float32Array(FRAME * ch);
    for (let c = 0; c < ch; c++) data.set(d.frame[c], c * FRAME);
    const ad = new AudioData({ format: "f32-planar", sampleRate: 48000, numberOfFrames: FRAME, numberOfChannels: ch, timestamp: s.ts, data });
    s.ts += 20000;
    s.lastLevel = Math.max(0, Math.min(255, Math.round((db + 60) / 60 * 255)));
    // Kamera an und hörbar: der Medienverbindung sagen, dass ich spreche (für die großen Bilder)
    const mv = s.media && s.media.video;
    if (s.lastLevel >= 64 && mv && mv.ws && mv.ws.readyState === 1 && window.UCNGVideo && UCNGVideo.on) {
      const now = performance.now();
      if (!s.lastSpk || now - s.lastSpk > 400) { s.lastSpk = now; mv.ws.send('{"t":"spk"}'); }
    }
    try { s.enc.encode(ad); } catch { /* Encoder neu */ }
    ad.close();
  }

  function sendChunk(s, chunk) {
    if (S !== s || !s.ws || s.ws.readyState !== 1 || s.ws.bufferedAmount > 32768) return;
    s.seq = (s.seq + 1) & 0xffff;
    const seq = s.seq, level = s.lastLevel || 0;
    const frame = (payload) => {
      const b = new Uint8Array(4 + payload.byteLength);
      b[0] = 1; b[1] = seq >> 8; b[2] = seq & 255; b[3] = level;
      b.set(payload, 4);
      return b;
    };
    const raw = new Uint8Array(chunk.byteLength);
    chunk.copyTo(raw);
    const e2 = E2();
    if (!e2) { s.ws.send(frame(raw)); return; }
    // verschlüsselt: in Reihenfolge, ohne Schlüssel gar nicht
    e2.chain(s, async () => {
      const ct = await e2.seal(raw);
      if (ct && S === s && s.ws && s.ws.readyState === 1) s.ws.send(frame(ct));
    });
  }

  // ---------- Empfang ----------

  function decoderFor(idx) {
    let d = S.decoders.get(idx);
    if (d && d.state !== "closed") return d;
    const s = S;
    const pp = S.peers.get(idx);
    const channels = s.cfg.stereo || (pp && pp.mu) ? 2 : 1; // Musik von Bots in Stereo
    d = new AudioDecoder({
      output: (ad) => {
        if (S !== s || !s.play) { ad.close(); return; }
        const n = ad.numberOfFrames, frame = [];
        const out = Math.min(ad.numberOfChannels, 2);
        for (let c = 0; c < out; c++) {
          const f = new Float32Array(n);
          ad.copyTo(f, { planeIndex: c, format: "f32-planar" });
          frame.push(f);
        }
        ad.close();
        s.play.port.postMessage({ type: "frame", idx, frame }, frame.map((f) => f.buffer));
      },
      error: () => { s.decoders.delete(idx); },
    });
    d.configure({ codec: "opus", sampleRate: 48000, numberOfChannels: channels });
    d.ts = 0;
    S.decoders.set(idx, d);
    return d;
  }

  function onAudio(b) {
    if (b[0] !== 1 || !S || deaf) return;
    const n = b[1];
    let o = 2;
    for (let k = 0; k < n && o + 6 <= b.length; k++) {
      const idx = (b[o] << 8) | b[o + 1];
      const len = (b[o + 4] << 8) | b[o + 5];
      o += 6;
      if (o + len > b.length) break;
      const data = b.subarray(o, o + len);
      o += len;
      const p = S.peers.get(idx);
      noteSeq(idx, (b[o - 4] << 8) | b[o - 3]);
      if (!p || gainFor(p.u) === 0) continue; // stumm: gar nicht erst dekodieren
      const e2 = E2();
      if (e2) {
        const s = S, copy = data.slice();
        e2.chain(p, async () => {
          const pt = await e2.open(copy);
          if (pt && S === s) decodeAudio(idx, pt);
        });
        continue;
      }
      decodeAudio(idx, data);
    }
  }

  function decodeAudio(idx, data) {
    try {
      const d = decoderFor(idx);
      d.decode(new EncodedAudioChunk({ type: "key", timestamp: d.ts, data }));
      d.ts += 20000;
    } catch { S.decoders.delete(idx); }
  }

  // ---------- Verbindungsqualität ----------
  // Verlust aus den Lücken der Folgenummern (je Sprecher), dazu Ping und Sendestau. Alle 5 s an den
  // Relay ("q": 1 schlecht, 2 mittel, 3 gut) – die anderen sehen es an meinem Namen.
  function noteSeq(idx, seq) {
    const q = S.qual || (S.qual = { last: new Map(), got: 0, lost: 0 });
    const prev = q.last.get(idx);
    q.last.set(idx, seq);
    q.got++;
    if (prev != null) {
      const gap = (seq - prev - 1 + 65536) & 0xffff;
      if (gap > 0 && gap < 8) q.lost += gap; // längere Lücken: Sprechpause (die Sprachaktivierung hält 350 ms)
    }
  }
  function measureQuality() {
    if (!S || S.status !== "connected") return;
    const q = S.qual || { got: 0, lost: 0 };
    const loss = q.got + q.lost > 20 ? q.lost / (q.got + q.lost) : 0;
    const buffered = S.ws ? S.ws.bufferedAmount : 0;
    const ping = S.ping || 0;
    const v = ping > 400 || loss > 0.08 || buffered > 128000 ? 1 : ping > 150 || loss > 0.02 || buffered > 32000 ? 2 : 3;
    S.loss = loss;
    if (S.qual) { S.qual.got = 0; S.qual.lost = 0; }
    if (v !== S.qSent && S.ws && S.ws.readyState === 1) { S.qSent = v; S.ws.send(JSON.stringify({ t: "state", q: v })); }
  }

  function onLevels(levels) {
    if (!S || !levels) return;
    let changed = false;
    const now = new Set();
    for (const [idx, rms] of Object.entries(levels)) {
      const p = S.peers.get(+idx);
      if (p && rms > 0.008) now.add(p.u);
    }
    if (S.sending && deps.me().id) now.add(deps.me().id);
    for (const u of now) if (!speaking.has(u)) { speaking.add(u); changed = true; }
    for (const u of [...speaking]) if (!now.has(u)) { speaking.delete(u); changed = true; }
    if (changed) renderSpeaking();
  }

  // ---------- Schalter ----------

  function toggleMute() {
    if (deaf) { deaf = false; selfMute = false; } else selfMute = !selfMute;
    if (S && S.play) S.play.port.postMessage({ type: "master", gain: deaf ? 0 : 1 });
    sendState();
    renderAll();
  }
  function toggleDeaf() {
    deaf = !deaf;
    if (S && S.play) {
      S.play.port.postMessage({ type: "master", gain: deaf ? 0 : 1 });
      if (deaf) S.play.port.postMessage({ type: "clear" });
      if (!deaf) applyGains();
    }
    sendState();
    renderAll();
  }
  function toggleHand() {
    if (!S) return;
    S.hand = !S.hand;
    sendState();
    renderAll();
  }

  // Push-to-Talk: gedrückt halten (nicht beim Tippen in Feldern, außer F-Tasten)
  const typing = (e) => /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName) || e.target.isContentEditable;
  document.addEventListener("keydown", (e) => {
    if (!S || prefs.mode !== "ptt" || e.code !== prefs.pttKey || e.repeat) return;
    if (typing(e) && !/^F\d+$/.test(e.code)) return;
    pttDown = true;
    e.preventDefault();
  });
  document.addEventListener("keyup", (e) => { if (e.code === prefs.pttKey) pttDown = false; });
  window.addEventListener("blur", () => { pttDown = false; });

  // ---------- Moderation ----------

  async function mod(userId, action) {
    if (!S) return;
    try { await deps.api(`/api/voice/${encodeURIComponent(S.room)}/mod`, { method: "POST", body: JSON.stringify({ userId, action }) }); }
    catch (e) { deps.toast(e.message); }
  }

  // Team: Person in einen anderen Sprachkanal schicken
  async function moveUser(user, name, from, to) {
    try {
      await deps.api(`/api/voice/${encodeURIComponent(from)}/move`, { method: "POST", body: JSON.stringify({ userId: user, to }) });
      deps.toast(t("voice.moved_other", { name, room: to }));
    } catch (e) { deps.toast(e.message); }
  }

  // ---------- Darstellung ----------

  function myPeer() { return S && S.you ? S.peers.get(S.you) : null; }

  // Seitenleiste: Knopf „Beitreten“ und Teilnehmerliste unter einem Sprachkanal.
  function decorateRoom(li, r) {
    if (S && S.room === r.name && S.stage !== !!(r.voice && r.voice.stage)) { // Bühne ein-/ausgeschaltet
      S.stage = !!(r.voice && r.voice.stage);
      queueMicrotask(renderBar);
    }
    if (!r.voice || !enabled) return;
    li.classList.add("voice-room");
    const a = li.querySelector(".room-row > a");
    const hash = a && a.querySelector(".hash");
    if (hash) { hash.innerHTML = r.voice.video ? ICON.cam : ICON.speaker; hash.classList.add("voice-icon"); }
    const row = li.querySelector(".room-row");
    if (deps.canModerate && deps.canModerate() || S && S.moderate) { // Ziel für Drag-and-Drop (der Server prüft)
      li.addEventListener("dragover", (e) => {
        if (!e.dataTransfer.types.includes("text/x-ucng-voice")) return;
        e.preventDefault(); e.dataTransfer.dropEffect = "move"; li.classList.add("drop-target");
      });
      li.addEventListener("dragleave", (e) => { if (!li.contains(e.relatedTarget)) li.classList.remove("drop-target"); });
      li.addEventListener("drop", (e) => {
        li.classList.remove("drop-target");
        let d;
        try { d = JSON.parse(e.dataTransfer.getData("text/x-ucng-voice")); } catch { return; }
        e.preventDefault();
        if (d && d.from !== r.name) moveUser(d.user, d.name, d.from, r.name);
      });
    }
    const here = S && S.room === r.name;
    const b = deps.iconBtn("voice-join" + (here ? " on" : ""), here ? t("voice.leave") : t("voice.join_room", { room: r.name }), here ? ICON.leave : ICON.join);
    b.addEventListener("click", (e) => { e.preventDefault(); e.stopPropagation(); here ? leave() : join(r.name); });
    row.append(b);
    const list = peersOf(r.name);
    if (!list.length) return;
    // Meldungen nach oben (in der Reihenfolge, in der sich die Personen gemeldet haben) und ein
    // blinkender Zähler am Kanal – auch sichtbar, wenn die Liste eingeklappt oder lang ist.
    const order = handOrder(r.name);
    if (order.length) {
      const c = deps.h("span", "voice-hands", "✋ " + order.length);
      c.title = t("voice.hands_count", { n: order.length });
      c.setAttribute("role", "status");
      if (a) a.append(c); else row.append(c);
      li.classList.add("has-hands");
    }
    const rank = (p) => { const k = order.indexOf(p.u); return k < 0 ? 1e9 : k; };
    const sorted = list.map((p, k) => [p, k]).sort((x, y) => rank(x[0]) - rank(y[0]) || x[1] - y[1]).map((x) => x[0]);
    const ul = deps.h("ul", "voice-peers");
    for (const p of sorted) ul.append(peerItem(r.name, p, r.voice, order.indexOf(p.u) + 1));
    li.append(ul);
  }

  function peersOf(room) {
    if (S && S.room === room && S.status === "connected") return [...S.peers.values()];
    return roomPeers[room] || [];
  }

  function peerItem(room, p, cfg, handNo) {
    const li = deps.h("li", "voice-peer");
    li.dataset.user = p.u;
    if (p.h && !p.mu) li.classList.add("hand-up");
    if ((deps.canModerate && deps.canModerate() || S && S.room === room && S.moderate) && !p.mu) { // Moderierende: per Drag-and-Drop in einen anderen Sprachkanal
      li.draggable = true;
      li.addEventListener("dragstart", (e) => {
        e.dataTransfer.setData("text/x-ucng-voice", JSON.stringify({ user: p.u, name: p.n, from: room }));
        e.dataTransfer.effectAllowed = "move";
        document.body.classList.add("voice-dragging");
      });
      li.addEventListener("dragend", () => document.body.classList.remove("voice-dragging"));
    }
    const b = deps.h("button", "voice-peer-btn");
    b.type = "button";
    const av = deps.avatar(p.n, p.u);
    av.classList.add("voice-av");
    b.append(av, deps.h("span", "voice-name", p.n));
    const flags = deps.h("span", "voice-flags");
    if (p.mu) flags.append(flag(ICON.music, t("voice.music_bot"), "music"));
    if (p.rec) flags.append(flag(ICON.rec, t("voice.recording_by", { name: p.n }), "rec"));
    if (p.q === 1 || p.q === 2) flags.append(qualityFlag(p.q));
    if (p.mo && !p.mu) flags.append(flag(ICON.shield, t("voice.moderates"), "mod"));
    if (p.cm) flags.append(flag(ICON.cam, t("voice.camera_on"), "cam"));
    if (p.h) {
      const f = flag(ICON.hand, t("voice.hand_raised_n", { n: handNo || 1 }), "hand");
      if (handNo) f.append(deps.h("span", "voice-hand-no", String(handNo)));
      flags.append(f);
    }
    if (cfg && cfg.stage && p.s) flags.append(flag(ICON.stage, t("voice.on_stage"), "stage"));
    if (p.m) flags.append(flag(ICON.micOff, t("voice.muted_by_team"), "srv"));
    else if (p.sm) flags.append(flag(ICON.micOff, t("voice.self_muted"), ""));
    else if (!p.s) flags.append(flag(ICON.head, t("voice.listening"), "listen"));
    if (prefs.mute[p.u] || deps.isBlocked(p.u)) flags.append(flag(ICON.headOff, t("voice.locally_muted"), "local"));
    else if (prefs.vol[p.u] != null) flags.append(deps.h("span", "voice-vol", Math.round(prefs.vol[p.u] * 100) + "%"));
    b.append(flags);
    if (speaking.has(p.u)) li.classList.add("speaking");
    const rx = reactions.get(p.u); // Reaktion läuft (übersteht das Neuzeichnen)
    if (rx && performance.now() < rx.until) {
      const bub = deps.h("span", "voice-react", rx.e);
      bub.style.animationDelay = -Math.round(performance.now() - rx.at) + "ms";
      li.append(bub);
    }
    b.title = p.n;
    b.addEventListener("click", (e) => { e.stopPropagation(); openPeer(b, room, p); });
    li.append(b);
    if (p.sc) { // teilt den Bildschirm: Knopf „Ansehen“ (nur im eigenen Sprachkanal)
      const here = S && S.room === room && S.status === "connected";
      const watching = here && window.UCNGScreen && UCNGScreen.watchingUser === p.u;
      const isMe = here && p.i === S.you;
      const w = deps.iconBtn("voice-watch" + (watching ? " on" : ""), isMe ? t("voice.screen_you_share") : here ? t("voice.screen_watch", { name: p.n }) : t("voice.screen_join_to_watch"), ICON.screen);
      w.disabled = !here || isMe;
      w.addEventListener("click", (e) => { e.stopPropagation(); if (watching) UCNGScreen.unwatch(); else UCNGScreen.watchUser(p.u); });
      li.classList.add("sharing");
      li.append(w);
    }
    return li;
  }

  function qualityFlag(q) {
    const s = deps.h("span", "voice-flag quality q" + q);
    s.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="3" y="12" width="3" height="5" rx="1" fill="currentColor"/><rect x="8.5" y="8" width="3" height="9" rx="1" fill="currentColor" opacity="' + (q >= 2 ? 1 : 0.25) + '"/><rect x="14" y="4" width="3" height="13" rx="1" fill="currentColor" opacity="0.25"/></svg>';
    const label = q === 1 ? t("voice.quality_bad") : t("voice.quality_mid");
    s.title = label; s.setAttribute("aria-label", label);
    return s;
  }

  function flag(svg, label, cls) {
    const s = deps.h("span", "voice-flag " + cls);
    s.innerHTML = svg;
    s.title = label;
    s.setAttribute("aria-label", label);
    return s;
  }

  function renderSpeaking() {
    document.querySelectorAll(".voice-peer").forEach((li) => li.classList.toggle("speaking", speaking.has(li.dataset.user)));
  }
  function renderSelf() {
    const me = deps.me().id;
    if (S && S.sending && me) speaking.add(me); else if (me) speaking.delete(me);
    renderSpeaking();
  }

  // Einstellungen einer Person: Lautstärke, lokal stumm, Moderation
  let pop = null;
  function openPeer(anchor, room, p) {
    closePop();
    const me = deps.me();
    const isMe = p.u === me.id;
    pop = deps.h("div", "popover voice-pop");
    pop.setAttribute("role", "dialog");
    pop.append(deps.h("div", "voice-pop-name", p.n));
    if (!isMe) {
      const row = deps.h("label", "voice-slider");
      const lab = deps.h("span", null, t("voice.volume"));
      const val = deps.h("output", null, Math.round(gainFor(p.u) * 100) + "%");
      const r = document.createElement("input");
      r.type = "range"; r.min = "0"; r.max = "200"; r.step = "5";
      r.value = String(Math.round((prefs.vol[p.u] == null ? 1 : prefs.vol[p.u]) * 100));
      r.setAttribute("aria-label", t("voice.volume_of", { name: p.n }));
      r.addEventListener("input", () => { val.textContent = r.value + "%"; setVolume(p.u, +r.value / 100); });
      r.addEventListener("change", renderAll);
      r.addEventListener("dblclick", () => { r.value = "100"; val.textContent = "100%"; setVolume(p.u, 1); renderAll(); });
      row.append(lab, r, val);
      pop.append(row);
      const ck = deps.h("label", "check");
      const cb = document.createElement("input");
      cb.type = "checkbox"; cb.checked = !!prefs.mute[p.u];
      cb.addEventListener("change", () => setLocalMute(p.u, cb.checked));
      ck.append(cb, deps.h("span", null, t("voice.mute_for_me")));
      pop.append(ck);
      if (deps.isBlocked(p.u)) pop.append(deps.h("p", "voice-pop-hint", t("voice.blocked_hint")));
    } else {
      pop.append(deps.h("p", "voice-pop-hint", t("voice.thats_you")));
    }
    const here = S && S.room === room && S.moderate;
    const acts = [];
    if (p.sc && !isMe && S && S.room === room && window.UCNGScreen) {
      acts.push(UCNGScreen.watchingUser === p.u ? [t("voice.screen_unwatch"), "", "", () => UCNGScreen.unwatch()] : [t("voice.screen_watch", { name: p.n }), "", "", () => UCNGScreen.watchUser(p.u)]);
    }
    if (p.cm && !isMe && S && S.room === room && window.UCNGVideo && UCNGVideo.pinUser) {
      acts.push(UCNGVideo.pinnedUser === p.u ? [t("voice.unpin"), "", "", () => UCNGVideo.pinUser("")] : [t("voice.pin"), "", "", () => UCNGVideo.pinUser(p.u)]);
    }
    if (here && !isMe) {
      if (p.sm && !p.m) acts.push([t("voice.ask_unmute"), "askunmute"]);
      acts.push(p.m ? [t("voice.unmute"), "unmute"] : [t("voice.server_mute"), "mute"]);
      if (S.cfg && S.cfg.host && !p.mu) acts.push(p.mo ? [t("voice.cohost_remove"), "uncohost"] : [t("voice.cohost_make"), "cohost"]);
      if (S.cfg && S.cfg.videoOn) acts.push(p.co || p.cm ? [t("voice.video_stop_other"), "stopvideo"] : [t("voice.video_allow"), "allowvideo"]);
      if (S.cfg && S.cfg.screenOn) acts.push(p.so || p.sc ? [t("voice.screen_stop_other"), "stopscreen"] : [t("voice.screen_allow"), "allowscreen"]);
      if (S.stage) acts.push(p.s ? [t("voice.revoke_speak"), "nospeak"] : [t("voice.grant_speak"), "speak"]);
      if (p.h) acts.push([t("voice.lower_hand"), "lowerhand"]);
      if (deps.voiceRooms) { // „here“: moderiert diesen Kanal (je nach Kanal-Einstellung)
        for (const to of deps.voiceRooms()) if (to !== room) acts.push([t("voice.move_to", { room: to }), "", "", () => moveUser(p.u, p.n, room, to)]);
      }
      acts.push([t("voice.kick"), "kick", "danger"]);
    } else if (here && isMe && S.stage && !p.s) {
      acts.push([t("voice.grant_speak"), "speak"]);
    }
    if (acts.length) {
      const box = deps.h("div", "voice-pop-actions");
      for (const [label, action, cls, fn] of acts) {
        const b = deps.h("button", "menu-item" + (cls ? " " + cls : ""), label);
        b.type = "button";
        b.addEventListener("click", () => { closePop(); if (fn) fn(); else mod(p.u, action); });
        box.append(b);
      }
      pop.append(box);
    }
    document.body.append(pop);
    const r = anchor.getBoundingClientRect();
    const w = pop.offsetWidth, ph = pop.offsetHeight;
    pop.style.left = Math.max(8, Math.min(window.innerWidth - w - 8, r.right + 8)) + "px";
    pop.style.top = Math.max(8, Math.min(window.innerHeight - ph - 8, r.top - 8)) + "px";
    if (r.right + 8 + w > window.innerWidth) pop.style.left = Math.max(8, r.left) + "px", pop.style.top = Math.min(window.innerHeight - ph - 8, r.bottom + 4) + "px";
    const f = pop.querySelector("input,button");
    if (f) f.focus();
  }
  function closePop() { if (pop) { pop.remove(); pop = null; } }
  document.addEventListener("click", (e) => { if (pop && !pop.contains(e.target)) closePop(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closePop(); });

  // Leiste unten in der Seitenleiste, solange man verbunden ist
  function renderBar() {
    const bar = $("voiceBar");
    if (!bar) return;
    if (!S) { bar.hidden = true; return; }
    bar.hidden = false;
    const head = deps.h("div", "voice-bar-head");
    const st = deps.h("button", "voice-bar-status " + S.status);
    st.type = "button";
    const label = S.status === "connected" ? t("voice.connected") : S.status === "reconnecting" ? t("voice.reconnecting") : t("voice.connecting");
    st.append(deps.h("b", null, label), deps.h("span", null, "#" + S.room + (S.ping ? " · " + S.ping + " ms" : "")));
    st.title = t("voice.open_room");
    st.addEventListener("click", () => deps.switchRoom(S.room));
    head.append(st);
    const btns = deps.h("div", "voice-bar-btns");
    const canTalk = S.speak && !S.muted; // Mikrofon und Kopfhörer sitzen in der Nutzerleiste darunter
    if (S.status === "connected") { // Hand heben – überall, nicht nur auf der Bühne
      const hb = deps.iconBtn("vb" + (S.hand ? " on" : ""), S.hand ? t("voice.lower_my_hand") : t("voice.raise_hand"), ICON.hand);
      hb.setAttribute("aria-pressed", String(!!S.hand));
      hb.addEventListener("click", toggleHand);
      const rb = deps.iconBtn("vb", t("voice.react"), ICON.react);
      rb.addEventListener("click", (e) => { e.stopPropagation(); openReactions(rb); });
      btns.append(hb, rb);
    }
    if (S.videoOK && window.UCNGVideo && UCNGVideo.supported()) {
      const on = UCNGVideo.on;
      const cb = deps.iconBtn("vb" + (on ? " sharing" : ""), on ? t("voice.camera_off") : t("voice.camera_start"), on ? ICON.camOff : ICON.cam);
      cb.setAttribute("aria-pressed", String(on));
      cb.addEventListener("click", () => UCNGVideo.toggleCam());
      btns.append(cb);
    }
    if (S.screenOK && window.UCNGScreen && UCNGScreen.supported()) {
      const on = UCNGScreen.sharing;
      const sb = deps.iconBtn("vb" + (on ? " sharing" : ""), on ? t("voice.screen_stop") : t("voice.screen_share"), on ? ICON.screenOff : ICON.screen);
      sb.setAttribute("aria-pressed", String(on));
      sb.addEventListener("click", () => UCNGScreen.toggleShare());
      btns.append(sb);
    }
    const more = deps.iconBtn("vb", t("voice.more"), ICON.more);
    more.addEventListener("click", (e) => { e.stopPropagation(); const r = more.getBoundingClientRect(); deps.menuAt(r.left, r.top - 8, moreItems(), true); });
    const lv = deps.iconBtn("vb leave", t("voice.leave"), ICON.leave);
    lv.addEventListener("click", () => leave());
    btns.append(more, lv);
    head.append(btns);
    const parts = [head];
    if (S.status === "connected" && canTalk && prefs.mode === "ptt") {
      const ptt = deps.h("button", "voice-ptt", t("voice.hold_to_talk", { key: prefs.pttLabel || prefs.pttKey }));
      ptt.type = "button";
      const down = (e) => { e.preventDefault(); pttDown = true; ptt.classList.add("on"); };
      const up = () => { pttDown = false; ptt.classList.remove("on"); };
      ptt.addEventListener("pointerdown", down);
      ptt.addEventListener("pointerup", up);
      ptt.addEventListener("pointerleave", up);
      ptt.addEventListener("pointercancel", up);
      parts.push(ptt);
    }
    if (S.status === "connected" && S.moderate) { // Meldungen der Reihe nach – ein Klick: Wort erteilen / Hand senken
      const order = handOrder(S.room).filter((u) => !S.peers.has(S.you) || S.peers.get(S.you).u !== u);
      if (order.length) {
        const box = deps.h("div", "voice-hands-bar");
        box.append(deps.h("span", "voice-hands", "✋ " + order.length));
        for (const [k, u] of order.entries()) {
          const p = [...S.peers.values()].find((x) => x.u === u);
          if (!p) continue;
          const b = deps.h("button", "voice-hand-chip", (k + 1) + ". " + p.n);
          b.type = "button";
          b.title = t("voice.hand_raised_n", { n: k + 1 });
          b.addEventListener("click", (e) => {
            e.stopPropagation();
            const r = b.getBoundingClientRect(), items = [];
            if (S.stage && !p.s) items.push(["", t("voice.grant_speak"), () => { mod(p.u, "speak"); mod(p.u, "lowerhand"); }]);
            else if (p.sm && !p.m) items.push(["", t("voice.ask_unmute"), () => { mod(p.u, "askunmute"); mod(p.u, "lowerhand"); }]);
            items.push(["", t("voice.lower_hand"), () => mod(p.u, "lowerhand")]);
            deps.menuAt(r.left, r.top - 8, items, true);
          });
          box.append(b);
        }
        parts.push(box);
      }
    }
    if (window.UCNGScreen && UCNGScreen.sharing) parts.push(deps.h("p", "voice-bar-note sharing", UCNGScreen.starting ? t("voice.screen_starting") : t("voice.screen_sharing_now")));
    if (S.status === "connected" && !S.speak) parts.push(deps.h("p", "voice-bar-note", S.stage ? t("voice.stage_note") : t("voice.listen_only")));
    if (S.askUnmute && (selfMute || deaf)) { // „bitte Mikrofon einschalten“
      const box = deps.h("div", "voice-ask");
      box.append(deps.h("span", null, t("voice.asked_unmute")));
      const yes = deps.h("button", "btn-primary", t("voice.unmute_me"));
      yes.type = "button";
      yes.addEventListener("click", () => { S.askUnmute = false; deaf = false; selfMute = false; if (S.play) S.play.port.postMessage({ type: "master", gain: 1 }); sendState(); renderAll(); });
      const no = deps.h("button", "btn-quiet", t("voice.not_now"));
      no.type = "button";
      no.addEventListener("click", () => { S.askUnmute = false; renderAll(); });
      box.append(yes, no);
      parts.push(box);
    }
    if (S.cfg && S.cfg.e2ee && S.status === "connected") {
      const e2 = window.UCNGE2EE;
      const note = deps.h("button", "voice-bar-note e2ee" + (e2 && e2.ready ? " ok" : ""), e2 && e2.ready ? t("voice.e2ee_code", { code: e2.code }) : t("voice.e2ee_wait"));
      note.type = "button";
      note.title = t("voice.e2ee_hint");
      note.addEventListener("click", () => deps.toast(t("voice.e2ee_hint")));
      parts.push(note);
    }
    if (prefs.audioOnly && S.cfg && (S.cfg.screenOn || S.cfg.videoOn)) parts.push(deps.h("p", "voice-bar-note", t("voice.audio_only_on")));
    if (X()) parts.push(...X().barNotes(S));
    bar.replaceChildren(...parts);
    if (X()) X().renderMobile(S);
  }

  // Reaktionen: kleine Auswahl über dem Knopf
  function openReactions(anchor) {
    const items = ["👍", "👏", "😂", "❤️", "🎉", "😮", "👎", "🙏"].map((e) => ["", e, () => sendReact(e)]);
    const r = anchor.getBoundingClientRect();
    deps.menuAt(r.left, r.top - 8, items, true, "react-menu");
  }
  function sendReact(e) {
    if (S && S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify({ t: "react", e }));
  }

  // „Mehr“: Ansicht, Statistik, Nur Audio, Leitung …
  function moreItems() {
    const items = [];
    const host = S && S.moderate;
    if (X()) items.push(...X().menuItems(S, { host, prefs }));
    items.push(["", prefs.audioOnly ? t("voice.audio_only_off") : t("voice.audio_only"), () => setAudioOnly(!prefs.audioOnly)]);
    if (!prefs.audioOnly && S && S.cfg && (S.cfg.videoOn || S.cfg.screenOn)) { // Bildrate: Auto → alle → ½ → ¼
      items.push(["", saverLabel(), () => setSaver(SAVER[(SAVER.indexOf(prefs.saver) + 1) % SAVER.length])]);
    }
    items.push(["", t("voice.settings"), openSettings]);
    return items;
  }

  function setAudioOnly(on) {
    prefs.audioOnly = !!on;
    savePrefs();
    if (!S || !S.cfg || !S.cfg.mediaConn) { renderAll(); return; }
    if (on) {
      for (const k of Object.keys(S.media || {})) closeMedia(S, k);
      detachModules();
    } else if (S.status === "connected") {
      if (S.cfg.screenOn) openMedia(S, "screen");
      if (S.cfg.videoOn) openMedia(S, "video");
    }
    renderAll();
  }

  function renderAll() {
    renderBar();
    if (deps && deps.render) deps.render();
  }

  // ---------- Einstellungen ----------

  let levelCb = null;
  let testStream = null;

  async function openSettings() {
    const dlg = $("voiceDialog");
    if (!dlg) return;
    const inSel = $("voiceInput"), outSel = $("voiceOutput");
    let devices = [];
    try { devices = await navigator.mediaDevices.enumerateDevices(); } catch { /* ohne */ }
    const fill = (sel, kind, cur) => {
      const opts = [new Option(t("voice.default_device"), "")];
      devices.filter((d) => d.kind === kind && d.deviceId && d.deviceId !== "default").forEach((d, i) => opts.push(new Option(d.label || `${kind} ${i + 1}`, d.deviceId)));
      sel.replaceChildren(...opts);
      sel.value = cur;
      if (sel.value !== cur) sel.value = "";
    };
    fill(inSel, "audioinput", prefs.input);
    fill(outSel, "audiooutput", prefs.output);
    if ($("voiceCamera")) fill($("voiceCamera"), "videoinput", prefs.camera || "");
    $("voiceOutputRow").hidden = !("setSinkId" in AudioContext.prototype);
    $("voiceModeVad").checked = prefs.mode !== "ptt";
    $("voiceModePtt").checked = prefs.mode === "ptt";
    $("voiceSens").value = String(prefs.sens);
    $("voiceMeter").style.setProperty("--sens", ((prefs.sens + 80) / 80 * 100) + "%");
    $("voiceSensVal").textContent = prefs.sens + " dB";
    $("voiceKey").textContent = prefs.pttLabel || prefs.pttKey;
    $("voiceNS").checked = !!prefs.ns;
    syncMode();
    levelCb = (db) => {
      const pct = Math.max(0, Math.min(100, (db + 80) / 80 * 100));
      $("voiceMeter").style.setProperty("--lvl", pct + "%");
      $("voiceMeter").classList.toggle("open", db > prefs.sens);
    };
    if (!S || !S.stream) startTestMeter();
    dlg.showModal();
  }

  // Pegelanzeige im Dialog, auch ohne Verbindung
  async function startTestMeter() {
    try {
      const audio = prefs.input ? { deviceId: { exact: prefs.input } } : true;
      testStream = await navigator.mediaDevices.getUserMedia({ audio });
      const ctx = new AudioContext();
      const an = ctx.createAnalyser();
      an.fftSize = 1024;
      ctx.createMediaStreamSource(testStream).connect(an);
      const buf = new Float32Array(an.fftSize);
      const tick = () => {
        if (!testStream) { ctx.close(); return; }
        an.getFloatTimeDomainData(buf);
        let s = 0;
        for (const x of buf) s += x * x;
        const rms = Math.sqrt(s / buf.length);
        if (levelCb) levelCb(rms > 0 ? 20 * Math.log10(rms) : -100);
        requestAnimationFrame(tick);
      };
      tick();
    } catch { /* ohne Anzeige */ }
  }
  function stopTestMeter() {
    if (testStream) testStream.getTracks().forEach((x) => x.stop());
    testStream = null;
  }

  function syncMode() {
    const ptt = $("voiceModePtt").checked;
    $("voiceVadRow").hidden = ptt;
    $("voiceKeyRow").hidden = !ptt;
  }

  function bindDialog() {
    const dlg = $("voiceDialog");
    if (!dlg) return;
    dlg.addEventListener("close", () => { levelCb = null; stopTestMeter(); keyWait = false; });
    $("voiceInput").addEventListener("change", (e) => {
      prefs.input = e.target.value; savePrefs();
      if (testStream) { stopTestMeter(); startTestMeter(); }
      restartMic();
    });
    if ($("voiceCamera")) $("voiceCamera").addEventListener("change", (e) => { prefs.camera = e.target.value; savePrefs(); });
    $("voiceOutput").addEventListener("change", (e) => {
      prefs.output = e.target.value; savePrefs();
      if (S && S.ctx && S.ctx.setSinkId) S.ctx.setSinkId(prefs.output).catch(() => deps.toast(t("voice.output_failed")));
    });
    $("voiceModeVad").addEventListener("change", () => { prefs.mode = "vad"; savePrefs(); syncMode(); renderAll(); });
    $("voiceModePtt").addEventListener("change", () => { prefs.mode = "ptt"; savePrefs(); syncMode(); renderAll(); });
    $("voiceSens").addEventListener("input", (e) => { prefs.sens = +e.target.value; $("voiceSensVal").textContent = prefs.sens + " dB"; $("voiceMeter").style.setProperty("--sens", ((prefs.sens + 80) / 80 * 100) + "%"); savePrefs(); });
    $("voiceNS").addEventListener("change", (e) => { prefs.ns = e.target.checked; savePrefs(); restartMic(); });
    $("voiceKeyBtn").addEventListener("click", () => { keyWait = true; $("voiceKey").textContent = t("voice.press_key"); });
    $("voiceResetVol").addEventListener("click", () => { prefs.vol = {}; prefs.mute = {}; savePrefs(); applyGains(); renderAll(); deps.toast(t("voice.volumes_reset")); });
    document.addEventListener("keydown", (e) => {
      if (!keyWait) return;
      e.preventDefault(); e.stopPropagation();
      if (e.key === "Escape") { keyWait = false; $("voiceKey").textContent = prefs.pttLabel || prefs.pttKey; return; }
      prefs.pttKey = e.code;
      prefs.pttLabel = e.key.length === 1 ? e.key.toUpperCase() : e.key === " " ? t("voice.key_space") : e.code;
      savePrefs();
      keyWait = false;
      $("voiceKey").textContent = prefs.pttLabel;
      renderAll();
    }, true);
  }
  let keyWait = false;

  // ---------- Schnittstelle für app.js ----------

  let enabled = false;
  window.UCNGVoice = {
    init(d) { deps = d; bindDialog(); },
    setEnabled(on) { enabled = !!on; if (!on) leave(true); },
    get enabled() { return enabled; },
    async load() {
      if (!enabled) return;
      try {
        const res = await fetch("/api/voice", { headers: deps.token() ? { Authorization: "Bearer " + deps.token() } : {} });
        if (!res.ok) return;
        const d = await res.json();
        for (const k of Object.keys(roomPeers)) delete roomPeers[k];
        Object.assign(roomPeers, d.rooms || {});
        deps.render();
      } catch { /* später */ }
    },
    // Ereignis „voice“ aus dem Live-Stream
    onEvent(ev) {
      for (const p of ev.voice || []) if (p.mu) musicVol[p.u] = p.mu / 100;
      roomPeers[ev.room] = ev.voice || [];
      if (!roomPeers[ev.room].length) delete roomPeers[ev.room];
      deps.render();
    },
    decorateRoom,
    join, leave,
    get room() { return S ? S.room : ""; },
    refreshGains() { applyGains(); },
    // Nutzerleiste (immer sichtbar, wie bei Discord): gilt auch vor dem Beitreten
    toggleMute, toggleDeaf, openSettings, toggleHand,
    // für voice-extras.js
    session: () => S,
    mod, sendReact, sendState, setAudioOnly, prefs: () => prefs, savePrefs, deps: () => deps, render: () => renderAll(),
    reaction(uid) { const r = reactions.get(uid); return r && performance.now() < r.until ? r : null; },
    addReaction(uid, e) { const now = performance.now(); reactions.set(uid, { e, at: now, until: now + 2500 }); setTimeout(() => { if (reactions.get(uid) && performance.now() >= reactions.get(uid).until) { reactions.delete(uid); renderAll(); } }, 2600); renderAll(); },
    setSaver, saverDrop, sendLimits,
    handNo(uid) { return S ? handOrder(S.room).indexOf(uid) + 1 : 0; }, // 0 = keine Meldung
    peerName(uid) { const p = S && [...S.peers.values()].find((x) => x.u === uid); return p ? p.n : ""; },
    get state() {
      return { selfMute, deaf, connected: !!(S && S.status === "connected"), canTalk: !S || (S.speak && !S.muted), serverMuted: !!(S && S.muted), listenOnly: !!(S && !S.speak) };
    },
    icons: ICON,
  };
})();
