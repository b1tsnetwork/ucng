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
  // Ansicht: "speaker" (die Sprechenden groß) oder "gallery" (alle gleich groß); angeheftet: Nutzer-ID
  const store = { get(k) { try { return localStorage.getItem(k); } catch { return null; } }, set(k, v) { try { localStorage.setItem(k, v); } catch { /* egal */ } } };
  let viewMode = store.get("ucng:confView") === "gallery" ? "gallery" : "speaker";
  let pinned = "";
  let lastPins = "";

  const supported = () => !!(window.VideoEncoder && window.VideoDecoder && navigator.mediaDevices && window.UCNGMedia);

  // ---------- eigene Kamera ----------

  function layerConf(layer, w, h) {
    const p = layer === 0 ? L.cfg.thumbPreset : L.cfg.videoPreset;
    const scale = Math.min(1, p.height / h);
    return { w: even(w * scale), h: even(h * scale), kbps: layer === 0 ? p.kbps : (cam ? cam.kbps : p.kbps), fps: p.fps };
  }
  // Ebene 1 (groß) mit zeitlichen Ebenen, die Miniansicht ohne
  function encConfig(c, conf, layer) {
    return UCNGMedia.config({ codec: c.codec, hw: c.hw }, conf.w, conf.h, conf.fps, conf.kbps, layer === 1 ? c.svc : "", { hint: "motion" });
  }

  async function startCam() {
    if (!L || cam || !L.videoOK()) return;
    if (!supported()) { L.toast(t("voice.video_unsupported")); return; }
    const vp = L.cfg.videoPreset;
    const prefs = L.prefs();
    const video = { width: { ideal: vp.width }, height: { ideal: vp.height }, frameRate: { ideal: vp.fps, max: vp.fps } };
    if (prefs.camera) video.deviceId = { exact: prefs.camera };
    let stream;
    try { stream = await navigator.mediaDevices.getUserMedia({ video, audio: false }); }
    catch (e) {
      if (prefs.camera && e.name === "OverconstrainedError") { prefs.camera = ""; L.savePrefs(); return startCam(); }
      L.toast(t("voice.camera_denied")); return;
    }
    // Medienverbindung vorbereiten (der Chat-Server kann die Kameras jetzt auf einen anderen
    // Sprach-Server legen); danach ist L womöglich eine neue Verbindung
    if (L && L.prepare && !(await L.prepare())) { stream.getTracks().forEach((x) => x.stop()); return; }
    if (!L || cam) { stream.getTracks().forEach((x) => x.stop()); return; }
    const track = stream.getVideoTracks()[0];
    const st = track.getSettings();
    const hc = { w: even(Math.min(vp.width, st.width || vp.width)), h: even(Math.min(vp.height, st.height || vp.height)) };
    const enc = await UCNGMedia.pick(hc.w, hc.h, vp.fps, vp.kbps, { hint: "motion" });
    if (!enc || !L) { stream.getTracks().forEach((x) => x.stop()); if (L) L.toast(t("voice.video_unsupported")); return; }
    const codec = enc.codec;
    cam = { stream, track, codec, hw: enc.hw, svc: enc.svc, enc: [null, null], conf: [null, null], high: false, seq: [0, 0], ts: [0, 0], last: [0, 0],
      forceKey: [true, true], kbps: vp.kbps, on: false, canvas: [null, null], drops: 0 };
    track.addEventListener("ended", () => stopCam());
    L.sendText({ t: "cam", on: true, codec });
    render();
  }

  function begin(high) {
    const c = cam;
    c.on = true;
    c.high = !!high;
    if (L.hint) L.hint(c.codec);
    if (c.started) { c.forceKey = [true, true]; c.drops = 0; render(); return; } // nach einem Abriss weiter
    c.started = true;
    const loop = (frame) => { if (cam === c) encodeFrame(c, frame); else frame.close(); };
    if (window.MediaStreamTrackProcessor) {
      c.reader = new MediaStreamTrackProcessor({ track: c.track, maxBufferSize: 1 }).readable.getReader();
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
    const budget = UCNGMedia.sendBudget(max + L.cfg.thumbPreset.kbps);
    if (c.drops > 2 || ws.bufferedAmount > budget * 0.6) k = Math.max(min, Math.round(k * 0.75));
    else if (c.drops === 0 && ws.bufferedAmount < budget * 0.1) k = Math.min(max, Math.round(k * 1.15));
    c.drops = 0;
    if (Math.abs(k - c.kbps) / c.kbps >= 0.1) {
      c.kbps = k;
      if (c.enc[1] && c.conf[1]) { c.conf[1].kbps = k; try { c.enc[1].configure(encConfig(c, c.conf[1], 1)); } catch { /* egal */ } }
    }
  }

  function ensureEncoder(c, layer, w, h) {
    const conf = layerConf(layer, w, h);
    if (c.enc[layer] && c.conf[layer] && c.conf[layer].w === conf.w && c.conf[layer].h === conf.h) return c.enc[layer];
    if (!c.enc[layer]) {
      c.enc[layer] = new VideoEncoder({ output: (chunk, meta) => sendChunk(c, layer, chunk, meta), error: () => { c.enc[layer] = null; } });
    }
    c.conf[layer] = conf;
    c.enc[layer].configure(encConfig(c, conf, layer));
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
        // Leitung voll: Bild auslassen (die Miniansicht hat Vorrang, ≈ 250 ms Puffer)
        const budget = UCNGMedia.sendBudget(c.kbps + L.cfg.thumbPreset.kbps);
        if (!ws || ws.bufferedAmount > (layer === 0 ? budget : budget * 0.6)) { if (layer === 1) c.drops++; continue; }
        const enc = ensureEncoder(c, layer, frame.displayWidth, frame.displayHeight);
        if (!enc || enc.state !== "configured" || enc.encodeQueueSize > 1) continue;
        c.last[layer] = now;
        c.ts[layer] = Math.round(now * 1000);
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

  function sendChunk(c, layer, chunk, meta) {
    if (cam !== c || !c.on) return;
    const flags = (chunk.type === "key" ? 1 : 0) | (UCNGMedia.tidOf(meta) << 1);
    c.seq[layer] = (c.seq[layer] + 1) & 0xffff;
    const seq = c.seq[layer];
    const raw = new Uint8Array(chunk.byteLength);
    chunk.copyTo(raw);
    if (!c.stat) c.stat = { bytes: 0, frames: 0, t0: performance.now() };
    if (layer === 1) { c.stat.bytes += raw.length; c.stat.frames++; }
    const out = (payload) => {
      const b = new Uint8Array(5 + payload.length);
      b[0] = 3; b[1] = layer; b[2] = flags; b[3] = seq >> 8; b[4] = seq & 255;
      b.set(payload, 5);
      if (L) L.sendBin(b);
    };
    const e2 = L.e2ee && L.e2ee();
    if (!e2) { out(raw); return; }
    const holder = c.chains || (c.chains = [{}, {}]);
    e2.chain(holder[layer], async () => { const ct = await e2.seal(raw); if (ct && cam === c) out(ct); });
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
    if (L && L.hint) L.hint("");
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
    d.configure(UCNGMedia.decoderConfig(tl.codec));
    return d;
  }

  function onCam(b) {
    if (b.length < 8) return;
    const idx = (b[1] << 8) | b[2], layer = b[3] & 1, key = (b[4] & 1) === 1, tid = (b[4] >> 1) & 3;
    const p = L && L.peer(idx);
    if (!p || !p.cm) return;
    const tl = tileFor(idx);
    if (tl.codec !== p.cm) { // neuer Codec: Decoder neu
      tl.codec = p.cm;
      for (const d of tl.dec) if (d) try { d.close(); } catch { /* egal */ }
      tl.dec = [null, null]; tl.needKey = [true, true];
    }
    if (tl.needKey[layer] && !key) return;
    if (!tl.skip) tl.skip = [null, null];
    if (tl.skip[layer] != null) { // Zeitebene ausgelassen: alles darüber auch
      if (tid >= tl.skip[layer] && !key) return;
      tl.skip[layer] = null;
    }
    tl.needKey[layer] = false;
    try {
      if (!tl.dec[layer] || tl.dec[layer].state === "closed") tl.dec[layer] = makeDecoder(idx, tl, layer);
      const d = tl.dec[layer];
      const q = d.decodeQueueSize;
      if (q > 2 && tid > 0 && !key) { tl.skip[layer] = tid; return; }
      if (q > 5) { tl.needKey[layer] = true; L.sendText({ t: "camkey", i: idx, l: layer }); return; }
      const e2 = L.e2ee && L.e2ee();
      if (!e2) { d.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp: Math.round(performance.now() * 1000), data: b.subarray(7) })); return; }
      const copy = b.slice(7);
      const holder = tl.chains || (tl.chains = [{}, {}]);
      e2.chain(holder[layer], async () => {
        const pt = await e2.open(copy);
        if (!pt || tiles.get(idx) !== tl || d.state !== "configured") return;
        try { d.decode(new EncodedVideoChunk({ type: key ? "key" : "delta", timestamp: Math.round(performance.now() * 1000), data: pt })); }
        catch { tl.needKey[layer] = true; }
      });
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
        for (const [idx, tl] of tiles) if (!featured.includes(idx) && !pinIds().includes(idx)) { tl.shown[1] = false; tl.needKey[1] = true; }
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

  // Wen will ich groß sehen? (für den Relay: dessen großes Bild an mich)
  function pinIds() {
    if (!L) return [];
    const me = L.you();
    const cams = L.peers().filter((p) => p.cm && p.i !== me);
    if (viewMode === "gallery") return cams.length <= 4 ? cams.map((p) => p.i) : [];
    const p = pinned && cams.find((x) => x.u === pinned);
    return p ? [p.i] : [];
  }
  function sendPins(force) {
    if (!L) return;
    const ids = pinIds();
    const key = ids.join(",");
    if (!force && key === lastPins) return;
    lastPins = key;
    L.sendText({ t: "pins", ids });
    for (const id of ids) { const tl = tiles.get(id); if (tl) tl.needKey[1] = true; }
  }
  const highOK = (idx) => featured.includes(idx) || pinIds().includes(idx);
  function canvasFor(idx, wantBig) {
    const tl = tileFor(idx);
    const big = wantBig && highOK(idx) && tl.shown[1] && !degraded;
    return { canvas: tl.canvas[big ? 1 : 0], soft: wantBig && !big };
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
    box.classList.toggle("gallery", viewMode === "gallery" && !screenOn);
    const lay = $("confLayout");
    if (lay) {
      lay.title = viewMode === "gallery" ? t("voice.view_speaker") : t("voice.view_gallery");
      lay.setAttribute("aria-label", lay.title);
      lay.classList.toggle("on", viewMode === "gallery");
    }
    if (pinned && !peers.some((p) => p.u === pinned)) pinned = ""; // Kamera aus oder gegangen
    sendPins();
    if (box.hidden) { layoutMedia(); return; }
    const stage = $("confStage"), strip = $("confStrip");
    const others = peers.filter((p) => p.i !== me);
    if (screenOn) { // Präsentation: Bildschirm groß, angeheftete bzw. sprechende Person zuerst und etwas größer
      const leadIdx = pinIds()[0] || featured.find((i) => others.some((p) => p.i === i));
      const order = others.slice().sort((a, b) => (b.i === leadIdx) - (a.i === leadIdx));
      const all = order.map((p) => { const c = canvasFor(p.i, p.i === leadIdx); return tileEl(p, c.canvas, "conf-tile" + (p.i === leadIdx ? " lead" : "") + (c.soft ? " soft" : "")); });
      if (cam) all.push(selfTile("conf-tile self"));
      stage.replaceChildren(); strip.replaceChildren(...all);
      layoutMedia();
      return;
    }
    if (viewMode === "gallery") { // alle gleich groß
      const all = others.map((p) => { const c = canvasFor(p.i, others.length <= 4); return tileEl(p, c.canvas, "conf-tile big" + (c.soft ? " soft" : "")); });
      if (cam) all.push(selfTile("conf-tile big self"));
      stage.replaceChildren(...all);
      stage.dataset.n = String(Math.min(all.length, 16));
      strip.replaceChildren();
      $("confNote").textContent = degraded ? t("voice.video_degraded") : "";
      layoutMedia();
      return;
    }
    // Sprecher-Ansicht: angeheftet bzw. die zuletzt Sprechenden groß (ohne mich selbst, außer ich bin allein)
    let big = pinIds();
    if (!big.length) big = featured.filter((i) => i !== me && peers.some((p) => p.i === i));
    if (!big.length && others.length) big = [others[0].i];
    const bigTiles = big.map((idx) => {
      const p = peers.find((x) => x.i === idx);
      const c = canvasFor(idx, true); // ohne großes Bild: Miniansicht vergrößert
      return tileEl(p, c.canvas, "conf-tile big" + (c.soft ? " soft" : ""));
    });
    if (!bigTiles.length && cam) bigTiles.push(selfTile("conf-tile big self"));
    stage.replaceChildren(...bigTiles);
    stage.dataset.n = String(bigTiles.length);
    const small = others.filter((p) => !big.includes(p.i)).map((p) => tileEl(p, tileFor(p.i).canvas[0], "conf-tile"));
    if (cam && bigTiles[0] && !bigTiles[0].classList.contains("self")) small.unshift(selfTile("conf-tile self"));
    strip.replaceChildren(...small);
    $("confNote").textContent = degraded ? t("voice.video_degraded") : "";
    layoutMedia();
  }

  function setView(mode) {
    viewMode = mode === "gallery" ? "gallery" : "speaker";
    store.set("ucng:confView", viewMode);
    if (viewMode === "gallery") pinned = "";
    draw();
  }
  function pinUser(uid) {
    pinned = uid || "";
    if (pinned && viewMode === "gallery") { viewMode = "speaker"; store.set("ucng:confView", viewMode); }
    draw();
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
    d.className = cls + (L && L.speaking(p.u) ? " speaking" : "") + (pinned === p.u ? " pinned" : "");
    d.dataset.user = p.u;
    d.append(canvas);
    const n = document.createElement("span");
    n.className = "conf-name";
    // Zustand aus der Sprachverbindung (Hand, stumm) – dort steht er, nicht in der Medienverbindung
    const V = window.UCNGVoice, S = V && V.session(), ap = S && S.peers ? [...S.peers.values()].find((x) => x.u === p.u) : null;
    n.textContent = p.n + ((ap ? ap.m || ap.sm : p.m || p.sm) ? " 🔇" : "");
    d.append(n);
    if (ap && ap.h) { // gemeldet: große, pulsierende Hand mit der Nummer in der Reihenfolge
      d.classList.add("hand-up");
      const hb = document.createElement("span");
      hb.className = "conf-hand";
      const no = V.handNo ? V.handNo(p.u) : 0;
      hb.textContent = "✋" + (no ? " " + no : "");
      hb.title = t("voice.hand_raised_n", { n: no || 1 });
      d.append(hb);
    }
    const pin = document.createElement("button");
    pin.type = "button";
    pin.className = "conf-pin";
    pin.textContent = pinned === p.u ? "📌" : "📍";
    pin.title = pinned === p.u ? t("voice.unpin") : t("voice.pin");
    pin.setAttribute("aria-label", pin.title);
    pin.addEventListener("click", (e) => { e.stopPropagation(); pinUser(pinned === p.u ? "" : p.u); });
    d.append(pin);
    const rx = window.UCNGVoice && UCNGVoice.reaction(p.u);
    if (rx) {
      const bub = document.createElement("span");
      bub.className = "conf-react";
      bub.textContent = rx.e;
      bub.style.animationDelay = -Math.round(performance.now() - rx.at) + "ms";
      d.append(bub);
    }
    d.addEventListener("dblclick", () => { if (document.fullscreenElement) document.exitFullscreen().catch(() => {}); else d.requestFullscreen().catch(() => {}); });
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
    const lay = $("confLayout");
    if (lay) lay.addEventListener("click", () => setView(viewMode === "gallery" ? "speaker" : "gallery"));
    const pip = $("confPip");
    if (pip) {
      pip.hidden = !document.pictureInPictureEnabled;
      pip.addEventListener("click", () => { const c = document.querySelector("#confStage canvas, #confStrip canvas"); if (c && window.UCNGExtras) UCNGExtras.pip(c); });
    }
  }

  function stats() {
    const out = {};
    if (cam && cam.on) {
      const st = cam.stat || { bytes: 0, frames: 0, t0: performance.now() }, now = performance.now(), sec = Math.max(0.5, (now - st.t0) / 1000);
      const c1 = cam.conf[1] || cam.conf[0] || {};
      out.send = { codec: cam.codec, hw: cam.hw === "prefer-hardware", svc: cam.svc, high: cam.high, w: c1.w, h: c1.h,
        fps: cam.high ? Math.round(st.frames / sec) : 0, kbps: Math.round(st.bytes * 8 / sec / 1000), target: cam.kbps };
      cam.stat = { bytes: 0, frames: 0, t0: now };
    }
    out.recv = [];
    for (const [idx, tl] of tiles) {
      const p = L && L.peer(idx);
      if (!p) continue;
      const big = tl.shown[1] && highOK(idx);
      const cv = tl.canvas[big ? 1 : 0];
      out.recv.push({ name: p.n, w: cv.width, h: cv.height, big });
    }
    return out;
  }

  window.UCNGVideo = {
    supported,
    attach(link) { L = link; featured = link.featured || []; degraded = false; lastPins = "-"; bindView(); render(); },
    detach() {
      stopCam(true);
      for (const idx of [...tiles.keys()]) dropTile(idx);
      featured = []; L = null; draw();
    },
    // Abriss: Kamera weiterlaufen lassen; resume() meldet sie neu an
    suspend() {
      if (cam) cam.on = false;
      for (const idx of [...tiles.keys()]) dropTile(idx);
      featured = [];
    },
    resume() {
      sendPins(true);
      if (!cam || !L) return;
      if (!L.videoOK()) { stopCam(true); return; }
      L.sendText({ t: "cam", on: true, codec: cam.codec });
    },
    stats, setView, pinUser,
    get pinnedUser() { return pinned; },
    get view() { return viewMode; },
    onText, onCam, onPeer, onLeave,
    setFeatured(ids) { featured = ids || []; render(); },
    toggleCam() { if (cam) stopCam(); else startCam(); },
    stopCam: () => stopCam(true),
    get on() { return !!cam; },
    refresh: () => draw(),
  };
})();
