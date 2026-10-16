// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Kamera in Konferenzen: Jede Kamera schickt eine Miniansicht (immer) und – solange der Relay
// sie „featured“ – ein großes Bild. Gezeigt werden alle Kameras als Miniansicht und die zuletzt
// sprechenden Personen groß. Das große Bild passt sich der Leitung an: Staut sich die Verbindung,
// sinkt die Bitrate; bekommt die eigene Verbindung zu wenig, zeigt der Relay vorübergehend nur
// Miniansichten.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const t = (k, v) => window._t(k, v);
  const even = (x) => Math.max(2, Math.round(x / 2) * 2);

  let L = null; // { cfg, ws(), sendText, sendBin, peer(idx), peers(), you(), videoOK(), toast, render, speaking(uid) }
  let cam = null; // eigene Kamera: { stream, track, codec, enc:[low, high], high, seq:[0,0], ts:[0,0], last:[0,0], forceKey:[..], kbps, on }
  const tiles = new Map(); // idx → { dec:[low, high], needKey:[true,true], codec, el, canvas, big }
  let featured = [];
  let degraded = false;

  const supported = () => !!(window.VideoEncoder && window.VideoDecoder && navigator.mediaDevices);

  // ---------- eigene Kamera ----------

  function layerConf(layer, w, h) {
    const p = layer === 0 ? L.cfg.thumbPreset : L.cfg.videoPreset;
    const scale = Math.min(1, p.height / h);
    return { w: even(w * scale), h: even(h * scale), kbps: layer === 0 ? p.kbps : (cam ? cam.kbps : p.kbps), fps: p.fps };
  }
  function encConfig(codec, c) {
    return { codec, width: c.w, height: c.h, bitrate: c.kbps * 1000, framerate: c.fps, latencyMode: "realtime", bitrateMode: "variable" };
  }

  async function pickCodec() {
    for (const codec of ["vp8", "vp09.00.10.08"]) {
      try { if ((await VideoEncoder.isConfigSupported(encConfig(codec, { w: 640, h: 360, kbps: 600, fps: 24 }))).supported) return codec; } catch { /* nächster */ }
    }
    return "";
  }

  async function startCam() {
    if (!L || cam || !L.videoOK()) return;
    if (!supported()) { L.toast(t("voice.video_unsupported")); return; }
    const vp = L.cfg.videoPreset;
    const prefs = L.prefs();
    const video = { width: { ideal: vp.width }, height: { ideal: vp.height }, frameRate: { ideal: vp.fps, max: 30 } };
    if (prefs.camera) video.deviceId = { exact: prefs.camera };
    let stream;
    try { stream = await navigator.mediaDevices.getUserMedia({ video, audio: false }); }
    catch (e) {
      if (prefs.camera && e.name === "OverconstrainedError") { prefs.camera = ""; L.savePrefs(); return startCam(); }
      L.toast(t("voice.camera_denied")); return;
    }
    if (!L) { stream.getTracks().forEach((x) => x.stop()); return; }
    const codec = await pickCodec();
    if (!codec) { stream.getTracks().forEach((x) => x.stop()); L.toast(t("voice.video_unsupported")); return; }
    const track = stream.getVideoTracks()[0];
    cam = { stream, track, codec, enc: [null, null], conf: [null, null], high: false, seq: [0, 0], ts: [0, 0], last: [0, 0],
      forceKey: [true, true], kbps: vp.kbps, on: false, canvas: [null, null] };
    track.addEventListener("ended", () => stopCam());
    L.sendText({ t: "cam", on: true, codec });
    render();
  }

  function begin(high) {
    const c = cam;
    c.on = true;
    c.high = !!high;
    const loop = (frame) => { if (cam === c) encodeFrame(c, frame); else frame.close(); };
    if (window.MediaStreamTrackProcessor) {
      c.reader = new MediaStreamTrackProcessor({ track: c.track }).readable.getReader();
      (async () => {
        for (;;) {
          let r;
          try { r = await c.reader.read(); } catch { break; }
          if (r.done || cam !== c) { if (r.value) r.value.close(); break; }
          loop(r.value);
        }
      })();
    } else {
      const v = document.createElement("video");
      v.muted = true; v.playsInline = true; v.srcObject = c.stream; v.play().catch(() => {});
      c.video = v;
      c.timer = setInterval(() => {
        if (cam !== c || v.readyState < 2) return;
        try { loop(new VideoFrame(v, { timestamp: Math.round(performance.now() * 1000) })); } catch { /* nächstes Bild */ }
      }, 1000 / 30);
    }
    c.adapt = setInterval(() => adapt(c), 2000);
    render();
  }

  // Bitrate des großen Bildes an die eigene Leitung anpassen
  function adapt(c) {
    const ws = L && L.ws();
    if (!ws || !c.high) return;
    const max = L.cfg.videoPreset.kbps, min = Math.max(150, Math.round(max / 5));
    let k = c.kbps;
    if (ws.bufferedAmount > 400000) k = Math.max(min, Math.round(k * 0.7));
    else if (ws.bufferedAmount < 50000) k = Math.min(max, Math.round(k * 1.15));
    if (k !== c.kbps) {
      c.kbps = k;
      if (c.enc[1] && c.conf[1]) { c.conf[1].kbps = k; try { c.enc[1].configure(encConfig(c.codec, c.conf[1])); c.forceKey[1] = true; } catch { /* egal */ } }
    }
  }

  function ensureEncoder(c, layer, w, h) {
    const conf = layerConf(layer, w, h);
    if (c.enc[layer] && c.conf[layer] && c.conf[layer].w === conf.w && c.conf[layer].h === conf.h) return c.enc[layer];
    if (!c.enc[layer]) {
      c.enc[layer] = new VideoEncoder({ output: (chunk) => sendChunk(c, layer, chunk), error: () => { c.enc[layer] = null; } });
    }
    c.conf[layer] = conf;
    c.enc[layer].configure(encConfig(c.codec, conf));
    c.forceKey[layer] = true;
    return c.enc[layer];
  }

  function encodeFrame(c, frame) {
    try {
      const now = performance.now();
      const ws = L && L.ws();
      for (const layer of [0, 1]) {
        if (layer === 1 && !c.high) continue;
        const conf = layerConf(layer, frame.displayWidth, frame.displayHeight);
        if (now - c.last[layer] < 1000 / conf.fps * 0.9) continue;
        if (!ws || ws.bufferedAmount > (layer === 0 ? 1.5e6 : 6e5)) continue; // Leitung voll: Bild auslassen
        const enc = ensureEncoder(c, layer, frame.displayWidth, frame.displayHeight);
        if (!enc || enc.state !== "configured" || enc.encodeQueueSize > 2) continue;
        c.last[layer] = now;
        c.ts[layer] += Math.round(1e6 / conf.fps);
        let vf;
        if (frame.displayWidth === c.conf[layer].w && frame.displayHeight === c.conf[layer].h) {
          vf = new VideoFrame(frame, { timestamp: c.ts[layer] });
        } else {
          let cv = c.canvas[layer];
          if (!cv) { cv = c.canvas[layer] = new OffscreenCanvas(c.conf[layer].w, c.conf[layer].h); cv.c2d = cv.getContext("2d"); }
          if (cv.width !== c.conf[layer].w || cv.height !== c.conf[layer].h) { cv.width = c.conf[layer].w; cv.height = c.conf[layer].h; }
          cv.c2d.drawImage(frame, 0, 0, cv.width, cv.height);
          vf = new VideoFrame(cv, { timestamp: c.ts[layer] });
        }
        enc.encode(vf, { keyFrame: c.forceKey[layer] });
        vf.close();
        c.forceKey[layer] = false;
      }
    } catch { /* nächstes Bild */ } finally {
      frame.close();
    }
  }

  function sendChunk(c, layer, chunk) {
    if (cam !== c || !c.on) return;
    const b = new Uint8Array(5 + chunk.byteLength);
    b[0] = 3; b[1] = layer; b[2] = chunk.type === "key" ? 1 : 0;
    c.seq[layer] = (c.seq[layer] + 1) & 0xffff;
    b[3] = c.seq[layer] >> 8; b[4] = c.seq[layer] & 255;
    chunk.copyTo(b.subarray(5));
    L.sendBin(b);
  }

  function stopCam(fromRelay) {
    const c = cam;
    if (!c) return;
    cam = null;
    clearInterval(c.timer); clearInterval(c.adapt);
    if (c.reader) c.reader.cancel().catch(() => {});
    if (c.video) c.video.srcObject = null;
    c.stream.getTracks().forEach((x) => x.stop());
    for (const e of c.enc) if (e && e.state !== "closed") try { e.close(); } catch { /* egal */ }
    if (!fromRelay && L) L.sendText({ t: "cam", on: false });
    render();
  }

  // ---------- Empfang ----------

  function tileFor(idx) {
    let tl = tiles.get(idx);
    if (!tl) {
      tl = { dec: [null, null], needKey: [true, true], codec: "", canvas: [document.createElement("canvas"), document.createElement("canvas")], shown: [false, false] };
      tiles.set(idx, tl);
    }
    return tl;
  }

  function makeDecoder(idx, tl, layer) {
    const d = new VideoDecoder({
      output: (frame) => {
        const cv = tl.canvas[layer];
        if (cv.width !== frame.displayWidth || cv.height !== frame.displayHeight) { cv.width = frame.displayWidth; cv.height = frame.displayHeight; }
        cv.getContext("2d").drawImage(frame, 0, 0);
        frame.close();
        if (!tl.shown[layer]) { tl.shown[layer] = true; render(); }
      },
      error: () => {
        tl.needKey[layer] = true;
        try { tl.dec[layer] = makeDecoder(idx, tl, layer); } catch { tl.dec[layer] = null; }
        if (L) L.sendText({ t: "camkey", i: idx, l: layer });
      },
    });
    d.configure({ codec: tl.codec, optimizeForLatency: true });
    d.ts = 0;
    return d;
  }

  function onCam(b) {
    if (b.length < 8) return;
    const idx = (b[1] << 8) | b[2], layer = b[3] & 1, key = (b[4] & 1) === 1;
    const p = L && L.peer(idx);
    if (!p || !p.cm) return;
    const tl = tileFor(idx);
    if (tl.codec !== p.cm) { // neuer Codec: Decoder neu
      tl.codec = p.cm;
      for (const d of tl.dec) if (d) try { d.close(); } catch { /* egal */ }
      tl.dec = [null, null]; tl.needKey = [true, true];
    }
    if (tl.needKey[layer] && !key) return;
    tl.needKey[layer] = false;
    try {
      if (!tl.dec[layer] || tl.dec[layer].state === "closed") tl.dec[layer] = makeDecoder(idx, tl, layer);
      const d = tl.dec[layer];
      if (d.decodeQueueSize > 8) { tl.needKey[layer] = true; L.sendText({ t: "camkey", i: idx, l: layer }); return; }
      d.ts += 33333;
      d.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp: d.ts, data: b.subarray(7) }));
    } catch { tl.needKey[layer] = true; L.sendText({ t: "camkey", i: idx, l: layer }); }
  }

  function dropTile(idx) {
    const tl = tiles.get(idx);
    if (!tl) return;
    for (const d of tl.dec) if (d && d.state !== "closed") try { d.close(); } catch { /* egal */ }
    tiles.delete(idx);
  }

  // ---------- Nachrichten von voice.js ----------

  function onText(m) {
    switch (m.t) {
      case "cam":
        if (m.on && cam && !cam.on) begin(m.high);
        else if (!m.on && cam) {
          if (!cam.on) L.toast(t("voice.camera_denied_room"));
          stopCam(true);
        }
        break;
      case "layer":
        if (cam) { cam.high = !!m.high; if (cam.high) cam.forceKey[1] = true; }
        break;
      case "camkey":
        if (cam) cam.forceKey[m.l || 0] = true;
        break;
      case "featured":
        featured = m.ids || [];
        for (const [idx, tl] of tiles) if (!featured.includes(idx)) { tl.shown[1] = false; tl.needKey[1] = true; }
        render();
        break;
      case "degraded":
        degraded = !!m.on;
        if (m.on) L.toast(t("voice.video_degraded"));
        render();
        break;
    }
  }
  function onPeer(p) {
    if (!p.cm) { if (tiles.has(p.i)) { dropTile(p.i); render(); } }
    else render();
  }
  function onLeave(idx) { dropTile(idx); featured = featured.filter((x) => x !== idx); render(); }

  // ---------- Ansicht ----------

  let timer = 0;
  function render() {
    clearTimeout(timer);
    timer = setTimeout(draw, 30);
    if (L) L.render();
  }

  function draw() {
    const box = $("confView");
    if (!box) return;
    const peers = L ? L.peers().filter((p) => p.cm) : [];
    const me = L ? L.you() : 0;
    const anyCam = peers.length > 0 || !!cam;
    box.hidden = !L || !L.cfg.videoOn || !anyCam || hiddenByUser;
    $("confToggle").hidden = !L || !L.cfg.videoOn || !anyCam;
    $("confToggle").classList.toggle("on", !box.hidden);
    const screenOn = !!($("screenView") && !$("screenView").hidden);
    box.classList.toggle("mini", screenOn);
    if (box.hidden) { layoutMedia(); return; }
    const stage = $("confStage"), strip = $("confStrip");
    if (screenOn) { // Bildschirm hat die Bühne: alle Kameras klein
      const all = peers.filter((p) => p.i !== me).map((p) => tileEl(p, tileFor(p.i).canvas[0], "conf-tile"));
      if (cam) all.unshift(selfTile("conf-tile self"));
      stage.replaceChildren(); strip.replaceChildren(...all);
      layoutMedia();
      return;
    }
    // groß: Featured (ohne mich selbst, außer ich bin allein)
    let big = featured.filter((i) => i !== me && peers.some((p) => p.i === i));
    if (!big.length && peers.length) big = peers.filter((p) => p.i !== me).slice(0, 1).map((p) => p.i);
    const bigTiles = big.map((idx) => {
      const p = peers.find((x) => x.i === idx);
      const tl = tileFor(idx);
      const layer = tl.shown[1] && !degraded ? 1 : 0; // ohne großes Bild: Miniansicht vergrößert
      return tileEl(p, tl.canvas[layer], "conf-tile big" + (layer === 0 ? " soft" : ""));
    });
    if (!bigTiles.length && cam) bigTiles.push(selfTile("conf-tile big self"));
    stage.replaceChildren(...bigTiles);
    stage.dataset.n = String(bigTiles.length);
    const small = peers.filter((p) => p.i !== me && !big.includes(p.i)).map((p) => tileEl(p, tileFor(p.i).canvas[0], "conf-tile"));
    if (cam && bigTiles[0] && !bigTiles[0].classList.contains("self")) small.unshift(selfTile("conf-tile self"));
    strip.replaceChildren(...small);
    $("confNote").textContent = degraded ? t("voice.video_degraded") : "";
    layoutMedia();
  }

  // Nur die Kamera-Leiste sichtbar (ohne Bildschirm)? Dann braucht der Bereich keine feste Höhe.
  function layoutMedia() {
    const area = $("mediaArea");
    if (!area) return;
    const sv = $("screenView"), cv = $("confView");
    area.classList.toggle("mini-only", (!sv || sv.hidden) && !!cv && !cv.hidden && cv.classList.contains("mini"));
  }

  // Höhe des Bereichs mit dem Trenner einstellen (gemerkt), Doppelklick setzt zurück
  function bindSplit() {
    const sp = $("mediaSplit"), area = $("mediaArea");
    if (!sp || sp.dataset.bound) return;
    sp.dataset.bound = "1";
    const KEY = "ucng:mediaH";
    const apply = (px) => { if (px) area.style.setProperty("--media-h", px + "px"); else area.style.removeProperty("--media-h"); };
    try { apply(+localStorage.getItem(KEY) || 0); } catch { /* egal */ }
    const save = (px) => { try { if (px) localStorage.setItem(KEY, String(px)); else localStorage.removeItem(KEY); } catch { /* egal */ } };
    const limit = (px) => { const chat = area.parentElement.clientHeight; return Math.round(Math.max(140, Math.min(chat - 260, px))); };
    sp.addEventListener("pointerdown", (e) => {
      e.preventDefault();
      sp.setPointerCapture(e.pointerId);
      sp.classList.add("drag");
      const top = area.getBoundingClientRect().top;
      const move = (ev) => apply(limit(ev.clientY - top - 4));
      const up = () => {
        sp.classList.remove("drag");
        sp.removeEventListener("pointermove", move);
        save(Math.round(area.getBoundingClientRect().height));
      };
      sp.addEventListener("pointermove", move);
      sp.addEventListener("pointerup", up, { once: true });
      sp.addEventListener("pointercancel", up, { once: true });
    });
    sp.addEventListener("dblclick", () => { apply(0); save(0); });
    sp.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
      e.preventDefault();
      const px = limit(area.getBoundingClientRect().height + (e.key === "ArrowUp" ? -40 : 40));
      apply(px); save(px);
    });
    // Bildschirm an/aus → Kameras zwischen Bühne und Leiste umstellen
    const sv = $("screenView");
    if (sv) new MutationObserver(() => draw()).observe(sv, { attributes: true, attributeFilter: ["hidden"] });
  }

  function tileEl(p, canvas, cls) {
    const d = document.createElement("div");
    d.className = cls + (L && L.speaking(p.u) ? " speaking" : "");
    d.dataset.user = p.u;
    d.append(canvas);
    const n = document.createElement("span");
    n.className = "conf-name";
    n.textContent = p.n + (p.m || p.sm ? " 🔇" : "");
    d.append(n);
    return d;
  }

  let preview = null;
  function selfTile(cls) {
    const d = document.createElement("div");
    d.className = cls;
    if (!preview) { preview = document.createElement("video"); preview.muted = true; preview.playsInline = true; preview.autoplay = true; }
    if (cam && preview.srcObject !== cam.stream) { preview.srcObject = cam.stream; preview.play().catch(() => {}); }
    d.append(preview);
    const n = document.createElement("span");
    n.className = "conf-name";
    n.textContent = t("voice.video_you");
    d.append(n);
    return d;
  }

  let hiddenByUser = false;
  function bindView() {
    const tg = $("confToggle");
    if (!tg || tg.dataset.bound) return;
    tg.dataset.bound = "1";
    bindSplit();
    tg.addEventListener("click", () => { hiddenByUser = !hiddenByUser; draw(); });
    $("confFull").addEventListener("click", () => {
      const box = $("confView");
      if (document.fullscreenElement) document.exitFullscreen().catch(() => {}); else box.requestFullscreen().catch(() => {});
    });
  }

  window.UCNGVideo = {
    supported,
    attach(link) { L = link; featured = link.featured || []; degraded = false; bindView(); render(); },
    detach() {
      stopCam(true);
      for (const idx of [...tiles.keys()]) dropTile(idx);
      featured = []; L = null; draw();
    },
    onText, onCam, onPeer, onLeave,
    setFeatured(ids) { featured = ids || []; render(); },
    toggleCam() { if (cam) stopCam(); else startCam(); },
    stopCam: () => stopCam(true),
    get on() { return !!cam; },
    refresh: () => draw(),
  };
})();
