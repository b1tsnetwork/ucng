// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Bildschirmfreigabe in Sprachkanälen: Bildschirm aufnehmen → VP9/VP8 (WebCodecs) → über die
// Sprachverbindung zum Relay. Zuschauen: Bilder einer Person empfangen, dekodieren und auf ein
// <canvas> zeichnen. Video geht nur an Personen, die zusehen.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const t = (k, v) => window._t(k, v);

  let L = null; // Verbindung von voice.js: { sendText, sendBin, ws, peer(idx), you, cfg, toast, render }

  // Teilen
  let share = null; // { stream, track, enc, codec, w, h, seq, ts, forceKey, timer, reader, on }
  // Zusehen
  let view = null; // { idx, name, codec, dec, needKey, ts, frames, last }

  const supported = () => !!(window.VideoEncoder && window.VideoDecoder && navigator.mediaDevices && navigator.mediaDevices.getDisplayMedia);

  // ---------- Teilen ----------

  async function pickCodec(cfg, w, h) {
    // Wenig Bewegung: VP9 (schärfer bei Text); flüssig: VP8 (weniger Rechenlast)
    const list = cfg.screenFps <= 15 ? ["vp09.00.10.08", "vp8"] : ["vp8", "vp09.00.10.08"];
    for (const codec of list) {
      const c = encConfig(codec, cfg, w, h);
      try { if ((await VideoEncoder.isConfigSupported(c)).supported) return codec; } catch { /* nächster */ }
    }
    return "";
  }

  function encConfig(codec, cfg, w, h) {
    return { codec, width: w, height: h, bitrate: cfg.screenKbps * 1000, framerate: cfg.screenFps,
      latencyMode: "realtime", bitrateMode: "variable" };
  }

  const even = (x) => Math.max(2, Math.round(x / 2) * 2);

  async function startShare() {
    if (!L || share || !L.screenOK()) return;
    if (!supported()) { L.toast(t("voice.screen_unsupported")); return; }
    const cfg = L.cfg;
    let stream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({
        video: { frameRate: { ideal: cfg.screenFps, max: cfg.screenFps }, height: { max: cfg.screenHeight }, width: { max: Math.round(cfg.screenHeight * 16 / 9) } },
        audio: false,
      });
    } catch { return; } // abgebrochen
    if (!L) { stream.getTracks().forEach((x) => x.stop()); return; }
    const track = stream.getVideoTracks()[0];
    try { track.contentHint = cfg.screenFps <= 5 ? "text" : cfg.screenFps >= 30 ? "motion" : "detail"; } catch { /* egal */ }
    const st = track.getSettings();
    const scale = Math.min(1, cfg.screenHeight / (st.height || cfg.screenHeight));
    const w = even((st.width || 1280) * scale), h = even((st.height || 720) * scale);
    const codec = await pickCodec(cfg, w, h);
    if (!codec) { stream.getTracks().forEach((x) => x.stop()); L.toast(t("voice.screen_unsupported")); return; }
    share = { stream, track, codec, w, h, seq: 0, ts: 0, forceKey: true, on: false, lastAt: 0 };
    track.addEventListener("ended", () => stopShare());
    L.sendText({ t: "share", on: true, codec });
    L.render();
  }

  // Antwort des Relays: jetzt wirklich senden
  function begin() {
    const s = share;
    s.on = true;
    s.enc = new VideoEncoder({ output: (chunk) => sendChunk(s, chunk), error: () => { if (share === s) { L.toast(t("voice.screen_failed")); stopShare(); } } });
    s.enc.configure(encConfig(s.codec, L.cfg, s.w, s.h));
    const interval = 1000 / L.cfg.screenFps;
    if (window.MediaStreamTrackProcessor) { // läuft auch, wenn der Tab im Hintergrund ist
      s.reader = new MediaStreamTrackProcessor({ track: s.track }).readable.getReader();
      (async () => {
        for (;;) {
          let r;
          try { r = await s.reader.read(); } catch { break; }
          if (r.done || share !== s) { if (r.value) r.value.close(); break; }
          const now = performance.now();
          if (now - s.lastAt < interval * 0.9) { r.value.close(); continue; }
          s.lastAt = now;
          encode(s, r.value);
        }
      })();
    } else { // Firefox: Bild aus einem <video> abgreifen
      const v = document.createElement("video");
      v.muted = true; v.playsInline = true; v.srcObject = s.stream;
      v.play().catch(() => {});
      s.video = v;
      s.timer = setInterval(() => {
        if (share !== s || v.readyState < 2) return;
        let f;
        try { f = new VideoFrame(v, { timestamp: Math.round(performance.now() * 1000) }); } catch { return; }
        encode(s, f);
      }, interval);
    }
    L.render();
  }

  function encode(s, frame) {
    try {
      // Fenstergröße geändert: neu einstellen (gibt ein Schlüsselbild)
      const scale = Math.min(1, L.cfg.screenHeight / frame.displayHeight);
      const w = even(frame.displayWidth * scale), h = even(frame.displayHeight * scale);
      if (w !== s.w || h !== s.h) {
        s.w = w; s.h = h;
        s.enc.configure(encConfig(s.codec, L.cfg, w, h));
        s.forceKey = true;
      }
      // Gegendruck: Encoder oder Leitung voll → Bild auslassen (vor dem Kodieren schadet das nicht)
      if (s.enc.state !== "configured" || s.enc.encodeQueueSize > 2 || !L.ws() || L.ws().bufferedAmount > 1.5e6) return;
      s.ts += Math.round(1e6 / L.cfg.screenFps);
      const out = frame.displayWidth === w && frame.displayHeight === h ? frame : null;
      if (out) {
        const vf = new VideoFrame(out, { timestamp: s.ts });
        s.enc.encode(vf, { keyFrame: s.forceKey });
        vf.close();
      } else {
        // verkleinern
        if (!s.canvas) { s.canvas = new OffscreenCanvas(w, h); s.c2d = s.canvas.getContext("2d"); }
        if (s.canvas.width !== w || s.canvas.height !== h) { s.canvas.width = w; s.canvas.height = h; }
        s.c2d.drawImage(frame, 0, 0, w, h);
        const vf = new VideoFrame(s.canvas, { timestamp: s.ts });
        s.enc.encode(vf, { keyFrame: s.forceKey });
        vf.close();
      }
      s.forceKey = false;
    } catch { /* nächstes Bild */ } finally {
      frame.close();
    }
  }

  function sendChunk(s, chunk) {
    if (share !== s || !s.on) return;
    const b = new Uint8Array(4 + chunk.byteLength);
    b[0] = 2;
    b[1] = chunk.type === "key" ? 1 : 0;
    s.seq = (s.seq + 1) & 0xffff;
    b[2] = s.seq >> 8; b[3] = s.seq & 255;
    chunk.copyTo(b.subarray(4));
    L.sendBin(b);
  }

  function stopShare(fromRelay) {
    const s = share;
    if (!s) return;
    share = null;
    clearInterval(s.timer);
    if (s.reader) s.reader.cancel().catch(() => {});
    if (s.video) { s.video.srcObject = null; }
    s.stream.getTracks().forEach((x) => x.stop());
    if (s.enc && s.enc.state !== "closed") try { s.enc.close(); } catch { /* egal */ }
    if (!fromRelay && L) L.sendText({ t: "share", on: false });
    if (L) L.render();
  }

  // ---------- Zusehen ----------

  function watch(idx) {
    if (!L) return;
    if (!window.VideoDecoder) { L.toast(t("voice.screen_unsupported")); return; }
    if (view && view.idx === idx) { $("screenView").scrollIntoView({ block: "nearest" }); return; }
    if (view) unwatch();
    const p = L.peer(idx);
    view = { idx, name: p ? p.n : "", codec: "", dec: null, needKey: true, ts: 0, pending: true };
    L.sendText({ t: "watch", i: idx, on: true });
    showPanel();
  }

  function unwatch(silent) {
    const v = view;
    if (!v) return;
    view = null;
    if (v.dec && v.dec.state !== "closed") try { v.dec.close(); } catch { /* egal */ }
    if (L && !silent) L.sendText({ t: "watch", i: v.idx, on: false });
    hidePanel();
    if (L) L.render();
  }

  function makeDecoder(v) {
    v.dec = new VideoDecoder({
      output: (frame) => {
        if (view !== v) { frame.close(); return; }
        const cv = $("screenCanvas");
        if (cv.width !== frame.displayWidth || cv.height !== frame.displayHeight) { cv.width = frame.displayWidth; cv.height = frame.displayHeight; }
        cv.getContext("2d").drawImage(frame, 0, 0);
        frame.close();
        if (v.pending) { v.pending = false; $("screenView").classList.remove("waiting"); }
      },
      error: () => { // neu anfangen ab dem nächsten Schlüsselbild
        if (view !== v) return;
        v.needKey = true;
        try { makeDecoder(v); } catch { /* egal */ }
        if (L) L.sendText({ t: "keyreq", i: v.idx });
      },
    });
    v.dec.configure({ codec: v.codec, optimizeForLatency: true });
  }

  function onVideo(b) {
    const v = view;
    if (!v || !v.dec || b.length < 7) return;
    const idx = (b[1] << 8) | b[2];
    if (idx !== v.idx) return;
    const key = (b[3] & 1) === 1;
    if (v.needKey && !key) return;
    v.needKey = false;
    v.ts += 33333;
    try {
      if (v.dec.decodeQueueSize > 10) { v.needKey = true; L.sendText({ t: "keyreq", i: v.idx }); return; } // zu langsam: aufholen
      v.dec.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp: v.ts, data: b.subarray(6) }));
    } catch { v.needKey = true; L.sendText({ t: "keyreq", i: v.idx }); }
  }

  function showPanel() {
    const box = $("screenView");
    if (!box) return;
    box.hidden = false;
    box.classList.add("waiting");
    $("screenTitle").textContent = t("voice.screen_of", { name: view.name });
    const cv = $("screenCanvas");
    cv.getContext("2d").clearRect(0, 0, cv.width, cv.height);
  }
  function hidePanel() {
    const box = $("screenView");
    if (!box) return;
    if (document.fullscreenElement === box) document.exitFullscreen().catch(() => {});
    box.hidden = true;
  }

  // ---------- Nachrichten von voice.js ----------

  function onText(m) {
    switch (m.t) {
      case "share":
        if (m.on && share && !share.on) begin();
        else if (!m.on) {
          if (share && !share.on) { // abgelehnt
            L.toast(m.reason === "full" ? t("voice.screen_full") : t("voice.screen_denied"));
            stopShare(true);
          } else if (share) { stopShare(true); L.toast(t("voice.screen_stopped")); }
        }
        break;
      case "watch":
        if (!view || view.idx !== m.i) break;
        if (!m.on) {
          L.toast(m.reason === "busy" ? t("voice.screen_busy") : t("voice.screen_gone"));
          unwatch(true);
        } else if (!view.dec || view.codec !== m.codec) {
          view.codec = m.codec;
          if (view.dec) try { view.dec.close(); } catch { /* egal */ }
          try { makeDecoder(view); } catch { L.toast(t("voice.screen_unsupported")); unwatch(); }
        }
        break;
      case "keyreq":
        if (share) share.forceKey = true;
        break;
    }
  }

  function onPeer(p) {
    if (view && view.idx === p.i) {
      if (!p.sc) { L.toast(t("voice.screen_ended", { name: p.n })); unwatch(true); }
      else if (p.sc !== view.codec && view.codec) { // Codec gewechselt
        view.codec = p.sc;
        view.needKey = true;
        try { view.dec.close(); } catch { /* egal */ }
        makeDecoder(view);
      }
    }
  }

  function onLeave(idx) { if (view && view.idx === idx) unwatch(true); }

  // Bedienelemente des Fensters
  function bindPanel() {
    const box = $("screenView");
    if (!box || box.dataset.bound) return;
    box.dataset.bound = "1";
    $("screenClose").addEventListener("click", () => unwatch());
    $("screenFull").addEventListener("click", () => {
      if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
      else box.requestFullscreen().catch(() => {});
    });
    $("screenCanvas").addEventListener("dblclick", () => $("screenFull").click());
  }

  window.UCNGScreen = {
    supported,
    attach(link) { L = link; bindPanel(); },
    detach() { stopShare(true); unwatch(true); L = null; },
    onText, onVideo, onPeer, onLeave,
    toggleShare() { if (share) stopShare(); else startShare(); },
    stopShare: () => stopShare(true),
    get sharing() { return !!share; },
    get starting() { return !!share && !share.on; },
    watch, unwatch: () => unwatch(),
    get watching() { return view ? view.idx : 0; },
  };
})();
