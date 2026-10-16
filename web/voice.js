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
  const DEFAULTS = { input: "", output: "", mode: "vad", sens: -50, pttKey: "F9", pttLabel: "F9", ns: true, vol: {}, mute: {} };
  let prefs = Object.assign({}, DEFAULTS, store.json("ucng:voice", {}));
  const savePrefs = () => store.set("ucng:voice", prefs);

  let deps = null; // von app.js: { api, toast, me, isBlocked, avatar, h, iconBtn, switchRoom, render }
  const roomPeers = {}; // Kanal → [{i,u,n,s,m,sm,h}] (vom Server, für die Seitenleiste)
  const speaking = new Set(); // Nutzer-IDs, die gerade zu hören sind

  // Verbindung
  let S = null; // { room, ws, ctx, play, cap, enc, stream, decoders, peers, you, ... }
  let wantRoom = ""; // gewünschter Kanal (für Wiederverbinden)
  let retries = 0;
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
      j = await deps.api(`/api/voice/${encodeURIComponent(room)}/join`, { method: "POST", body: "{}" });
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
      S.play.connect(S.ctx.destination);
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
        prefs: () => prefs, savePrefs,
      });
    }
    ws.onopen = () => ws.send(JSON.stringify({ t: "auth", ticket: j.ticket }));
    ws.onmessage = (e) => { if (S === mine) onMessage(e.data); };
    ws.onclose = (e) => { if (S === mine) onClose(e); };
  }

  function absURL(u) { return u.startsWith("/") ? (location.protocol === "https:" ? "wss://" : "ws://") + location.host + u : u; }

  // ---------- Medienverbindungen (Bildschirm, Kameras) ----------
  // Je Art eine eigene WebSocket – getrennt von der Sprache, damit ein großes Bild nie die Sprache
  // aufhält, und auf Wunsch des Chat-Servers auf einem anderen Sprach-Server (Lastverteilung).

  const MOD = { screen: () => window.UCNGScreen, video: () => window.UCNGVideo };

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
      prefs: () => prefs, savePrefs,
      prepare: () => prepareMedia(s, kind),
      hint: (codec) => sendHint(s, kind, codec),
    };
  }

  // openMedia verbindet eine Art (neu). pre: schon geholte Zutrittskarte. Liefert, ob es klappte.
  async function openMedia(s, kind, pre, publish) {
    if (S !== s) return false;
    const old = s.media[kind];
    if (old) closeMedia(s, kind);
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

  function closeMedia(s, kind) {
    const m = s.media[kind];
    if (!m) return;
    delete s.media[kind];
    clearTimeout(m.timer);
    const mod = MOD[kind]();
    if (mod && S === s) mod.detach();
    if (m.ws) { m.ws.onclose = null; m.ws.onmessage = null; try { m.ws.close(1000); } catch { /* egal */ } }
    if (m.ready) m.ready(false);
    sendHint(s, kind, "");
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
        if (m.ready) { m.ready(true); m.ready = null; }
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
    if (mod) mod.detach();
    sendHint(s, m.kind, "");
    if (m.bye || e.code === 4003 || e.code === 4000 || e.code === 1000) { m.status = "closed"; renderAll(); return; }
    if (e.code === 4409) deps.toast(t("voice.media_busy"));
    retryMedia(s, m);
    renderAll();
  }

  function onMessage(data) {
    if (typeof data !== "string") {
      const b = new Uint8Array(data);
      if (b[0] === 2) { if (window.UCNGScreen) UCNGScreen.onVideo(b); }
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
        S.pinger = setInterval(() => { if (S && S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify({ t: "ping", ts: Math.round(performance.now()) })); }, 5000);
        S.ws.send(JSON.stringify({ t: "ping", ts: Math.round(performance.now()) }));
        deps.toast(t("voice.joined", { room: S.room }));
        if (S.cfg.mediaConn) {
          if (S.cfg.screenOn) openMedia(S, "screen");
          if (S.cfg.videoOn) openMedia(S, "video");
        }
        break;
      case "media": // eine Art ist auf einen anderen Sprach-Server umgezogen
        if (S.media && S.media[m.kind] && S.media[m.kind].relay !== m.relay) openMedia(S, m.kind);
        break;
      case "peer": upsertPeer(m.peer); if (window.UCNGScreen) UCNGScreen.onPeer(m.peer); if (window.UCNGVideo) UCNGVideo.onPeer(m.peer); break;
      case "leave": removePeer(m.i); if (window.UCNGScreen) UCNGScreen.onLeave(m.i); if (window.UCNGVideo) UCNGVideo.onLeave(m.i); break;
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
    teardown();
    const code = e.code;
    if (bye) { deps.toast(byeText(bye)); wantRoom = ""; renderAll(); return; }
    if (code === 1000 || (code === 1001 && !wantRoom)) { renderAll(); return; }
    if (code === 4000) { deps.toast(t("voice.other_window")); wantRoom = ""; renderAll(); return; }
    if (code === 4003) { deps.toast(byeText(e.reason)); wantRoom = ""; renderAll(); return; }
    if (code === 4409) { deps.toast(/voll|full/i.test(e.reason) ? t("voice.full") : t("voice.busy")); wantRoom = ""; renderAll(); return; }
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
    renderAll();
  }

  function byeText(code) {
    const known = { kicked: "voice.bye_kicked", moved: "voice.bye_moved", deleted: "voice.bye_deleted", renamed: "voice.bye_renamed",
      session: "voice.bye_session", banned: "voice.bye_banned", noguest: "voice.bye_noguest", novoice: "voice.bye_novoice", noaccess: "voice.bye_noaccess" };
    return t(known[code] || "voice.bye_kicked");
  }

  // teardown räumt eine Verbindung ab (ohne Meldung).
  function teardown() {
    if (!S) return;
    const s = S;
    S = null;
    clearInterval(s.pinger);
    if (window.UCNGScreen) UCNGScreen.detach();
    if (window.UCNGVideo) UCNGVideo.detach();
    for (const m of Object.values(s.media || {})) {
      clearTimeout(m.timer);
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
    if (!S) return;
    teardown();
    if (!silent) deps.toast(t("voice.left"));
    renderAll();
  }

  // ---------- Teilnehmer ----------

  function upsertPeer(p) {
    if (p.mu) musicVol[p.u] = p.mu / 100;
    const old = S.peers.get(p.i);
    S.peers.set(p.i, p);
    if (!old && S.play) S.play.port.postMessage({ type: "gain", idx: p.i, gain: gainFor(p.u) });
    if (p.i === S.you) { S.hand = !!p.h; S.muted = p.m; S.speak = p.s; }
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
    if (S && S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify({ t: "state", mute: selfMute || deaf, hand: !!S.hand }));
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
    const b = new Uint8Array(4 + chunk.byteLength);
    b[0] = 1;
    s.seq = (s.seq + 1) & 0xffff;
    b[1] = s.seq >> 8; b[2] = s.seq & 255;
    b[3] = s.lastLevel || 0;
    chunk.copyTo(b.subarray(4));
    s.ws.send(b);
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
      if (!p || gainFor(p.u) === 0) continue; // stumm: gar nicht erst dekodieren
      try {
        const d = decoderFor(idx);
        d.decode(new EncodedAudioChunk({ type: "key", timestamp: d.ts, data }));
        d.ts += 20000;
      } catch { S.decoders.delete(idx); }
    }
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
    if (deps.canModerate && deps.canModerate()) { // Ziel für Drag-and-Drop
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
    const ul = deps.h("ul", "voice-peers");
    for (const p of list) ul.append(peerItem(r.name, p, r.voice));
    li.append(ul);
  }

  function peersOf(room) {
    if (S && S.room === room && S.status === "connected") return [...S.peers.values()];
    return roomPeers[room] || [];
  }

  function peerItem(room, p, cfg) {
    const li = deps.h("li", "voice-peer");
    li.dataset.user = p.u;
    if (deps.canModerate && deps.canModerate() && !p.mu) { // Team: per Drag-and-Drop in einen anderen Sprachkanal
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
    if (p.cm) flags.append(flag(ICON.cam, t("voice.camera_on"), "cam"));
    if (p.h) flags.append(flag(ICON.hand, t("voice.hand_raised"), "hand"));
    if (cfg && cfg.stage && p.s) flags.append(flag(ICON.stage, t("voice.on_stage"), "stage"));
    if (p.m) flags.append(flag(ICON.micOff, t("voice.muted_by_team"), "srv"));
    else if (p.sm) flags.append(flag(ICON.micOff, t("voice.self_muted"), ""));
    else if (!p.s) flags.append(flag(ICON.head, t("voice.listening"), "listen"));
    if (prefs.mute[p.u] || deps.isBlocked(p.u)) flags.append(flag(ICON.headOff, t("voice.locally_muted"), "local"));
    else if (prefs.vol[p.u] != null) flags.append(deps.h("span", "voice-vol", Math.round(prefs.vol[p.u] * 100) + "%"));
    b.append(flags);
    if (speaking.has(p.u)) li.classList.add("speaking");
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
    if (here && !isMe) {
      acts.push(p.m ? [t("voice.unmute"), "unmute"] : [t("voice.server_mute"), "mute"]);
      if (S.cfg && S.cfg.videoOn) acts.push(p.co || p.cm ? [t("voice.video_stop_other"), "stopvideo"] : [t("voice.video_allow"), "allowvideo"]);
      if (S.cfg && S.cfg.screenOn) acts.push(p.so || p.sc ? [t("voice.screen_stop_other"), "stopscreen"] : [t("voice.screen_allow"), "allowscreen"]);
      if (S.stage) acts.push(p.s ? [t("voice.revoke_speak"), "nospeak"] : [t("voice.grant_speak"), "speak"]);
      if (p.h) acts.push([t("voice.lower_hand"), "lowerhand"]);
      if (deps.canModerate && deps.canModerate() && deps.voiceRooms) {
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
    if (S.stage && !S.speak) {
      const hb = deps.iconBtn("vb" + (S.hand ? " on" : ""), S.hand ? t("voice.lower_my_hand") : t("voice.raise_hand"), ICON.hand);
      hb.setAttribute("aria-pressed", String(!!S.hand));
      hb.addEventListener("click", toggleHand);
      btns.append(hb);
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
    const gear = deps.iconBtn("vb", t("voice.settings"), ICON.gear);
    gear.addEventListener("click", openSettings);
    const lv = deps.iconBtn("vb leave", t("voice.leave"), ICON.leave);
    lv.addEventListener("click", () => leave());
    btns.append(gear, lv);
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
    if (window.UCNGScreen && UCNGScreen.sharing) parts.push(deps.h("p", "voice-bar-note sharing", UCNGScreen.starting ? t("voice.screen_starting") : t("voice.screen_sharing_now")));
    if (S.status === "connected" && !S.speak) parts.push(deps.h("p", "voice-bar-note", S.stage ? t("voice.stage_note") : t("voice.listen_only")));
    bar.replaceChildren(...parts);
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
    toggleMute, toggleDeaf, openSettings,
    get state() {
      return { selfMute, deaf, connected: !!(S && S.status === "connected"), canTalk: !S || (S.speak && !S.muted), serverMuted: !!(S && S.muted), listenOnly: !!(S && !S.speak) };
    },
    icons: ICON,
  };
})();
