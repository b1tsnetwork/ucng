// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Konferenz-Zusätze rund um voice.js: Reaktionen, Werkzeuge der Leitung, Breakout-Räume,
// Aufnahme (im eigenen Browser), Untertitel (eigenes Whisper über den Chat-Server), Statistik,
// Bild-in-Bild und die Bedienung auf dem Handy (Anrufleiste, Ton im Hintergrund, Bildschirm an).
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const t = (k, v) => window._t(k, v);
  const V = () => window.UCNGVoice;
  const D = () => V().deps();
  const h = (tag, cls, text) => D().h(tag, cls, text);
  const isPhone = () => window.matchMedia("(pointer: coarse)").matches && window.innerWidth <= 900;

  // ---------- Reaktionen ----------

  function onReact(m) {
    V().addReaction(m.u, m.e); // Seitenleiste und Kacheln zeigen sie beim Zeichnen
    if (window.UCNGVideo) UCNGVideo.refresh();
  }

  // ---------- Aufnahme-Hinweise ----------

  const recBy = new Map(); // Nutzer-ID → Name (wer gerade aufnimmt)
  function onPeer(p) {
    const was = recBy.has(p.u);
    if (p.rec && !was) { recBy.set(p.u, p.n); D().toast(t("voice.rec_started", { name: p.n })); }
    if (!p.rec && was) { recBy.delete(p.u); D().toast(t("voice.rec_stopped", { name: p.n })); }
  }

  // ---------- Leitung ----------

  async function hostAction(action) {
    const S = V().session();
    if (!S) return;
    if (action === "endall" && !confirm(t("voice.end_all_confirm"))) return;
    try {
      await D().api(`/api/voice/${encodeURIComponent(S.room)}/mod`, { method: "POST", body: JSON.stringify({ action }) });
      if (action === "muteall") D().toast(t("voice.muted_all"));
      if (action === "camsoff") D().toast(t("voice.cams_off_all"));
    } catch (e) { D().toast(e.message); }
  }

  // ---------- Breakout-Räume ----------

  const breakouts = {}; // Hauptkanal → { parent, rooms, ends }
  let tick = 0;
  function onBreakout(b) {
    if (!b || !b.parent) return;
    const S = V().session();
    const mine = S && (S.room === b.parent || (b.rooms || []).includes(S.room));
    // dasselbe Ereignis kommt je Kanal (Hauptkanal und Gruppen) – nur einmal melden
    const known = breakouts[b.parent];
    if (b.ended) {
      if (!known) return;
      delete breakouts[b.parent];
      if (mine) D().toast(t("voice.breakout_ended", { room: b.parent }));
    } else {
      if (known && known.rooms.join() === b.rooms.join()) return;
      breakouts[b.parent] = b;
      if (mine) D().toast(t("voice.breakout_started", { n: b.rooms.length }));
    }
    ensureTick();
    V().render();
  }
  function ensureTick() {
    clearInterval(tick);
    if (Object.values(breakouts).some((b) => b.ends)) tick = setInterval(() => { if (V().session()) V().render(); }, 1000);
  }
  function breakoutOf(room) {
    for (const b of Object.values(breakouts)) if (b.parent === room || b.rooms.includes(room)) return b;
    return null;
  }
  function openBreakout() {
    const dlg = $("breakoutDialog");
    const S = V().session();
    if (!dlg || !S) return;
    const n = [...S.peers.values()].filter((p) => !p.mo && !p.mu).length;
    $("breakoutGroups").value = String(Math.max(2, Math.min(20, Math.round(n / 3) || 2)));
    $("breakoutError").textContent = "";
    $("breakoutInfo").textContent = t("voice.breakout_people", { n });
    dlg.showModal();
  }
  function bindBreakout() {
    const f = $("breakoutForm");
    if (!f || f.dataset.bound) return;
    f.dataset.bound = "1";
    f.addEventListener("submit", async (e) => {
      e.preventDefault();
      const S = V().session();
      if (!S) return;
      try {
        await D().api(`/api/voice/${encodeURIComponent(S.room)}/breakout`, { method: "POST",
          body: JSON.stringify({ groups: +$("breakoutGroups").value, minutes: +$("breakoutMinutes").value }) });
        $("breakoutDialog").close();
      } catch (err) { $("breakoutError").textContent = err.message; }
    });
  }
  async function endBreakout(parent) {
    try { await D().api(`/api/voice/${encodeURIComponent(parent)}/breakout`, { method: "DELETE" }); } catch (e) { D().toast(e.message); }
  }
  const mmss = (ms) => { const s = Math.max(0, Math.round(ms / 1000)); return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0"); };

  // ---------- Aufnahme (im eigenen Browser, als WebM) ----------

  let rec = null; // { mr, chunks, canvas, ctx2d, timer, dest, src, room, started }

  function pickMime() {
    for (const m of ["video/webm;codecs=vp9,opus", "video/webm;codecs=vp8,opus", "video/webm", "video/mp4"]) {
      if (window.MediaRecorder && MediaRecorder.isTypeSupported(m)) return m;
    }
    return "";
  }

  async function startRec() {
    const S = V().session();
    if (!S || rec || !S.ctx) return;
    const mime = pickMime();
    if (!mime) { D().toast(t("voice.rec_unsupported")); return; }
    const dlg = $("recDialog");
    if (dlg) {
      const ok = await new Promise((res) => {
        dlg.returnValue = "";
        dlg.onclose = () => res(dlg.returnValue === "ok");
        dlg.showModal();
      });
      if (!ok) return;
    }
    try { await D().api(`/api/voice/${encodeURIComponent(S.room)}/mod`, { method: "POST", body: JSON.stringify({ action: "recstart" }) }); }
    catch (e) { D().toast(e.message); return; }
    const canvas = document.createElement("canvas");
    canvas.width = 1280; canvas.height = 720;
    const dest = S.ctx.createMediaStreamDestination();
    S.play.connect(dest); // alle anderen (inkl. Ton der Bildschirmfreigabe)
    const r = { canvas, c2d: canvas.getContext("2d"), dest, room: S.room, chunks: [], started: Date.now(), sess: S };
    r.timer = setInterval(() => drawRec(r), 1000 / 30);
    drawRec(r);
    const stream = new MediaStream([...canvas.captureStream(30).getVideoTracks(), ...dest.stream.getAudioTracks()]);
    r.mr = new MediaRecorder(stream, { mimeType: mime, videoBitsPerSecond: 4e6, audioBitsPerSecond: 128000 });
    r.mr.ondataavailable = (e) => { if (e.data && e.data.size) r.chunks.push(e.data); };
    r.mr.onstop = () => saveRec(r);
    r.mr.start(1000);
    rec = r;
    V().render();
  }

  // eigenes Mikrofon mitschneiden (die Quelle wechselt, wenn das Mikrofon neu startet)
  function ownMic(r) {
    const S = V().session();
    if (!S || S !== r.sess) return;
    if (S.src && S.src !== r.src) {
      try { S.src.connect(r.dest); r.src = S.src; } catch { /* egal */ }
    }
  }

  // Bild der Aufnahme: Bildschirm (falls offen) mit sprechender Kamera, sonst die Kameras, sonst Namen
  function drawRec(r) {
    ownMic(r);
    const g = r.c2d, W = r.canvas.width, H = r.canvas.height;
    g.fillStyle = "#0b0c14"; g.fillRect(0, 0, W, H);
    const fit = (src, x, y, w, hh) => {
      const sw = src.videoWidth || src.width, sh = src.videoHeight || src.height;
      if (!sw || !sh) return;
      const k = Math.min(w / sw, hh / sh), dw = sw * k, dh = sh * k;
      try { g.drawImage(src, x + (w - dw) / 2, y + (hh - dh) / 2, dw, dh); } catch { /* noch kein Bild */ }
    };
    const screen = $("screenView") && !$("screenView").hidden ? $("screenCanvas") : null;
    const cams = [...document.querySelectorAll("#confStage .conf-tile canvas, #confStage .conf-tile video, #confStrip .conf-tile canvas, #confStrip .conf-tile video")];
    if (screen && screen.width > 16) {
      fit(screen, 0, 0, W, H);
      if (cams[0]) { g.fillStyle = "#000"; g.fillRect(W - 330, H - 190, 320, 180); fit(cams[0], W - 330, H - 190, 320, 180); }
    } else if (cams.length) {
      const n = Math.min(cams.length, 9), cols = Math.ceil(Math.sqrt(n)), rows = Math.ceil(n / cols);
      const cw = W / cols, ch = H / rows;
      cams.slice(0, n).forEach((c, i) => fit(c, (i % cols) * cw + 4, Math.floor(i / cols) * ch + 4, cw - 8, ch - 8));
    } else {
      const S = V().session();
      g.fillStyle = "#e7e8f2"; g.font = "600 40px system-ui, sans-serif"; g.textAlign = "center";
      g.fillText("#" + r.room, W / 2, H / 2 - 40);
      g.font = "26px system-ui, sans-serif"; g.fillStyle = "#9a9db5";
      const names = S ? [...S.peers.values()].map((p) => p.n).slice(0, 12).join(" · ") : "";
      g.fillText(names, W / 2, H / 2 + 20);
    }
    g.fillStyle = "#d93f4c"; g.beginPath(); g.arc(28, 28, 9, 0, Math.PI * 2); g.fill();
    g.fillStyle = "#fff"; g.font = "600 18px system-ui, sans-serif"; g.textAlign = "left";
    g.fillText(mmss(Date.now() - r.started), 46, 35);
  }

  function stopRec() {
    const r = rec;
    if (!r) return;
    rec = null;
    clearInterval(r.timer);
    try { r.mr.stop(); } catch { saveRec(r); }
    const S = V().session();
    if (S && S.room === r.room) D().api(`/api/voice/${encodeURIComponent(r.room)}/mod`, { method: "POST", body: JSON.stringify({ action: "recstop" }) }).catch(() => {});
    V().render();
  }

  function saveRec(r) {
    try { if (r.sess && r.sess.play) r.sess.play.disconnect(r.dest); } catch { /* egal */ }
    if (!r.chunks.length) return;
    const type = r.mr ? r.mr.mimeType.split(";")[0] : "video/webm";
    const blob = new Blob(r.chunks, { type });
    const d = new Date(r.started), pad = (x) => String(x).padStart(2, "0");
    const name = `ucng-${r.room}-${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}.${type.includes("mp4") ? "mp4" : "webm"}`;
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = name;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 60000);
    D().toast(t("voice.rec_saved", { name }));
  }

  // ---------- Untertitel (eigenes Whisper) ----------

  const transcript = []; // { at, name, text }
  let seg = null; // { frames: [], quiet }
  let capShown = [];

  // aus voice.js: 20-ms-Rahmen des eigenen Mikrofons; on = wird gerade gesendet
  function onCapture(s, frame, on) {
    const prefs = V().prefs();
    if (!s.cfg || !s.cfg.captions || prefs.noCapSend || !s.speak || s.muted) { seg = null; return; }
    if (on) {
      if (!seg) seg = { frames: [], quiet: 0, room: s.room };
      seg.frames.push(frame[0]);
      seg.quiet = 0;
      if (seg.frames.length >= 600) flushSeg(s); // 12 s
    } else if (seg) {
      seg.frames.push(frame[0]);
      if (++seg.quiet >= 35) flushSeg(s); // 0,7 s Pause: Abschnitt fertig
    }
  }

  function flushSeg(s) {
    const g = seg;
    seg = null;
    if (!g || g.frames.length - g.quiet < 30) return; // unter 0,6 s Sprache: auslassen
    const frames = g.frames.slice(0, g.frames.length - Math.max(0, g.quiet - 10));
    // 48 kHz → 16 kHz (Mittelwert aus je drei Werten), 16 bit
    const n = frames.length * 320;
    const pcm = new Int16Array(n);
    let k = 0;
    for (const f of frames) {
      for (let i = 0; i + 2 < f.length; i += 3) {
        const v = Math.max(-1, Math.min(1, (f[i] + f[i + 1] + f[i + 2]) / 3));
        pcm[k++] = v < 0 ? v * 0x8000 : v * 0x7fff;
      }
    }
    const wav = new ArrayBuffer(44 + k * 2);
    const dv = new DataView(wav);
    const str = (o, x) => { for (let i = 0; i < x.length; i++) dv.setUint8(o + i, x.charCodeAt(i)); };
    str(0, "RIFF"); dv.setUint32(4, 36 + k * 2, true); str(8, "WAVE"); str(12, "fmt ");
    dv.setUint32(16, 16, true); dv.setUint16(20, 1, true); dv.setUint16(22, 1, true); dv.setUint32(24, 16000, true);
    dv.setUint32(28, 32000, true); dv.setUint16(32, 2, true); dv.setUint16(34, 16, true); str(36, "data"); dv.setUint32(40, k * 2, true);
    new Int16Array(wav, 44, k).set(pcm.subarray(0, k));
    const lang = (D().locale ? D().locale() : "de").split("-")[0];
    fetch(`/api/voice/${encodeURIComponent(g.room)}/stt?lang=${encodeURIComponent(lang)}`, {
      method: "POST", body: wav, headers: { "Content-Type": "audio/wav", Authorization: "Bearer " + D().token() },
    }).catch(() => {});
  }

  function onCaption(m) {
    const name = V().peerName(m.u) || "?";
    transcript.push({ at: Date.now(), name, text: m.x });
    if (transcript.length > 5000) transcript.shift();
    if (!V().prefs().captions) return;
    capShown.push({ name, text: m.x, at: Date.now() });
    capShown = capShown.slice(-3);
    renderCaptions();
  }
  let capTimer = 0;
  function renderCaptions() {
    let box = $("captionBox");
    const main = document.querySelector("main.chat");
    if (!box && main) { box = h("div", "caption-box"); box.id = "captionBox"; box.setAttribute("aria-live", "polite"); main.append(box); }
    if (!box) return;
    const now = Date.now();
    capShown = capShown.filter((c) => now - c.at < 9000);
    box.hidden = !capShown.length;
    box.replaceChildren(...capShown.map((c) => { const p = h("p"); p.append(h("b", null, c.name + ": "), document.createTextNode(c.text)); return p; }));
    clearTimeout(capTimer);
    if (capShown.length) capTimer = setTimeout(renderCaptions, 1500);
  }

  function transcriptText() {
    const pad = (x) => String(x).padStart(2, "0");
    return transcript.map((l) => { const d = new Date(l.at); return `[${pad(d.getHours())}:${pad(d.getMinutes())}] ${l.name}: ${l.text}`; }).join("\n");
  }
  function downloadTranscript() {
    const S = V().session();
    const blob = new Blob([transcriptText() + "\n"], { type: "text/plain;charset=utf-8" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `mitschrift-${S ? S.room : "ucng"}-${new Date().toISOString().slice(0, 16).replace(/[:T]/g, "-")}.txt`;
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 60000);
  }
  // in den Kanal: in Nachrichten zu je höchstens 1900 Zeichen
  async function postTranscript() {
    const S = V().session();
    if (!S || !transcript.length) return;
    const lines = transcriptText().split("\n");
    const parts = [];
    let cur = t("voice.transcript_head") + "\n";
    for (const l of lines) {
      if (cur.length + l.length + 1 > 1900) { parts.push(cur); cur = ""; }
      cur += l + "\n";
    }
    if (cur.trim()) parts.push(cur);
    if (parts.length > 10) { D().toast(t("voice.transcript_too_long")); downloadTranscript(); return; }
    try {
      for (const text of parts) await D().api(`${D().roomPath(S.room)}/messages`, { method: "POST", body: JSON.stringify({ text: text.trim() }) });
      D().toast(t("voice.transcript_posted"));
    } catch (e) { D().toast(e.message); }
  }

  // ---------- Statistik ----------

  let statsTimer = 0;
  function toggleStats() {
    const old = $("voiceStats");
    if (old) { old.remove(); clearInterval(statsTimer); return; }
    const box = h("aside", "voice-stats");
    box.id = "voiceStats";
    box.setAttribute("aria-label", t("voice.stats"));
    document.body.append(box);
    const upd = () => {
      const S = V().session();
      if (!S || !document.body.contains(box)) { box.remove(); clearInterval(statsTimer); return; }
      const rows = [];
      const row = (k, v) => { const d = h("div", "vs-row"); d.append(h("span", null, k), h("b", null, v)); rows.push(d); };
      rows.push(h("h3", null, t("voice.stats")));
      row(t("voice.stats_ping"), S.ping ? S.ping + " ms" : "–");
      row(t("voice.stats_loss"), S.loss != null ? (Math.round(S.loss * 1000) / 10) + " %" : "–");
      row(t("voice.stats_quality"), [t("voice.quality_unknown"), t("voice.quality_bad"), t("voice.quality_mid"), t("voice.quality_good")][S.qSent || 0]);
      for (const [kind, m] of Object.entries(S.media || {})) row(t(kind === "screen" ? "voice.stats_server_screen" : "voice.stats_server_video"), (m.relay || "–") + (m.offloaded ? " ↗" : ""));
      const enc = (x) => `${x.codec}${x.hw ? " · HW" : ""}${x.svc ? " · " + x.svc : ""}`;
      const sc = window.UCNGScreen ? UCNGScreen.stats() : {};
      if (sc.send) row(t("voice.stats_screen_send"), `${sc.send.w}×${sc.send.h} · ${sc.send.fps} fps · ${sc.send.kbps}/${sc.send.target} kbit/s · ${enc(sc.send)}${sc.send.tone ? " · 🔊" : ""}`);
      if (sc.recv) row(t("voice.stats_screen_recv"), `${sc.recv.w}×${sc.recv.h} · ${sc.recv.fps} fps · ${sc.recv.kbps} kbit/s · ${sc.recv.codec}${sc.recv.tone ? " · 🔊" : ""}`);
      const vd = window.UCNGVideo ? UCNGVideo.stats() : {};
      if (vd.send) row(t("voice.stats_cam_send"), `${vd.send.w}×${vd.send.h} · ${vd.send.high ? vd.send.fps + " fps · " + vd.send.kbps + "/" + vd.send.target + " kbit/s" : t("voice.stats_thumb_only")} · ${enc(vd.send)}`);
      for (const r of vd.recv || []) row(r.name, `${r.w}×${r.h}${r.big ? "" : " · " + t("voice.stats_thumb")}`);
      if (S.cfg && S.cfg.e2ee) row(t("voice.stats_e2ee"), window.UCNGE2EE && UCNGE2EE.ready ? UCNGE2EE.code : "…");
      const close = h("button", "btn-quiet", t("voice.done"));
      close.type = "button";
      close.addEventListener("click", toggleStats);
      box.replaceChildren(...rows, close);
    };
    upd();
    statsTimer = setInterval(upd, 1000);
  }

  // ---------- Bild-in-Bild ----------

  let pipVideo = null;
  async function pip(canvas) {
    try {
      if (document.pictureInPictureElement) { await document.exitPictureInPicture(); return; }
      if (!canvas || !canvas.captureStream) return;
      if (!pipVideo) {
        pipVideo = document.createElement("video");
        pipVideo.muted = true; pipVideo.playsInline = true;
        pipVideo.addEventListener("leavepictureinpicture", () => { if (pipVideo.srcObject) pipVideo.srcObject.getTracks().forEach((x) => x.stop()); pipVideo.srcObject = null; });
      }
      pipVideo.srcObject = canvas.captureStream(30);
      await pipVideo.play();
      await pipVideo.requestPictureInPicture();
    } catch { D().toast(t("voice.pip_failed")); }
  }

  // ---------- Handy: Ton im Hintergrund, Media Session, Bildschirm an ----------

  function routeAudio(S) {
    if (!isPhone()) return false;
    try {
      const dest = S.ctx.createMediaStreamDestination();
      S.play.connect(dest);
      const a = new Audio();
      a.autoplay = true;
      a.srcObject = dest.stream;
      a.play().catch(() => {});
      S.audioEl = a;
      return true;
    } catch { return false; }
  }

  let wake = null;
  async function keepAwake(on) {
    try {
      if (on && !wake && "wakeLock" in navigator && document.visibilityState === "visible") {
        wake = await navigator.wakeLock.request("screen");
        wake.addEventListener("release", () => { wake = null; });
      } else if (!on && wake) { await wake.release(); wake = null; }
    } catch { wake = null; }
  }
  document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible" && V() && V().session()) keepAwake(true); });

  function mediaSession(S) {
    if (!("mediaSession" in navigator)) return;
    const ms = navigator.mediaSession;
    try { ms.metadata = new MediaMetadata({ title: "#" + S.room, artist: document.title || "UCNG" }); } catch { /* egal */ }
    const set = (a, fn) => { try { ms.setActionHandler(a, fn); } catch { /* nicht unterstützt */ } };
    set("togglemicrophone", () => V().toggleMute());
    set("togglecamera", () => { if (window.UCNGVideo) UCNGVideo.toggleCam(); });
    set("hangup", () => V().leave());
  }
  function mediaSessionState() {
    if (!("mediaSession" in navigator)) return;
    const st = V().state;
    try { navigator.mediaSession.setMicrophoneActive(!st.selfMute && !st.deaf && st.canTalk); } catch { /* egal */ }
    try { navigator.mediaSession.setCameraActive(!!(window.UCNGVideo && UCNGVideo.on)); } catch { /* egal */ }
  }

  // Anrufleiste auf schmalen Bildschirmen (die Seitenleiste ist dort meist zu)
  function renderMobile(S) {
    let bar = $("mobileCall");
    if (!bar) return;
    const show = !!S && window.innerWidth <= 760;
    bar.hidden = !show;
    if (!show) return;
    const st = V().state, ic = V().icons;
    const btn = (cls, label, svg, fn, pressed) => {
      const b = D().iconBtn("mc-btn " + cls, label, svg);
      if (pressed != null) b.setAttribute("aria-pressed", String(!!pressed));
      b.addEventListener("click", fn);
      return b;
    };
    const items = [h("span", "mc-room", "#" + S.room)];
    items.push(btn(st.selfMute || st.deaf ? "off" : "", st.selfMute ? t("voice.unmute_me") : t("voice.mute_me"), st.selfMute || st.deaf ? ic.micOff : ic.mic, () => V().toggleMute(), st.selfMute));
    items.push(btn(st.deaf ? "off" : "", st.deaf ? t("voice.undeafen") : t("voice.deafen"), st.deaf ? ic.headOff : ic.head, () => V().toggleDeaf(), st.deaf));
    if (S.videoOK && window.UCNGVideo && UCNGVideo.supported()) items.push(btn(UCNGVideo.on ? "on" : "", UCNGVideo.on ? t("voice.camera_off") : t("voice.camera_start"), UCNGVideo.on ? ic.camOff : ic.cam, () => UCNGVideo.toggleCam(), UCNGVideo.on));
    items.push(btn(S.hand ? "on" : "", S.hand ? t("voice.lower_my_hand") : t("voice.raise_hand"), ic.hand, () => V().toggleHand(), S.hand));
    items.push(btn("leave", t("voice.leave"), ic.leave, () => V().leave()));
    bar.replaceChildren(...items);
    mediaSessionState();
  }
  window.addEventListener("resize", () => { if (V() && V().session()) renderMobile(V().session()); });

  // ---------- Einbindung in voice.js ----------

  function onJoined(S) {
    bindBreakout();
    if (S.cfg && S.cfg.breakout) { breakouts[S.cfg.breakout.parent] = S.cfg.breakout; ensureTick(); }
    if (isPhone() || (S.cfg && (S.cfg.videoOn || S.cfg.screenOn))) keepAwake(true);
    mediaSession(S);
  }
  function onLeft(s, reconnecting) {
    seg = null;
    if (reconnecting) return;
    if (rec && rec.sess === s) stopRec();
    if (rec) stopRec();
    recBy.clear();
    keepAwake(false);
    if (s && s.audioEl) { s.audioEl.srcObject = null; }
    const st = $("voiceStats");
    if (st) { st.remove(); clearInterval(statsTimer); }
  }

  function barNotes(S) {
    const out = [];
    if (rec && rec.sess === S) {
      const b = h("button", "voice-bar-note rec", t("voice.rec_running_me", { time: mmss(Date.now() - rec.started) }));
      b.type = "button";
      b.title = t("voice.rec_stop");
      b.addEventListener("click", stopRec);
      out.push(b);
      if (!S.recTick) S.recTick = setInterval(() => { if (rec) V().render(); else { clearInterval(S.recTick); S.recTick = 0; } }, 1000);
    } else {
      const others = [...S.peers.values()].filter((p) => p.rec).map((p) => p.n);
      if (others.length) out.push(h("p", "voice-bar-note rec", t("voice.rec_running", { name: others.join(", ") })));
    }
    if (S.cfg && S.cfg.captions && S.status === "connected") {
      out.push(h("p", "voice-bar-note", V().prefs().noCapSend ? t("voice.captions_on_nosend") : t("voice.captions_on")));
    }
    const b = breakoutOf(S.room);
    if (b) {
      const inGroup = b.rooms.includes(S.room);
      const box = h("div", "voice-breakout");
      box.append(h("span", null, (inGroup ? t("voice.breakout_in", { room: b.parent }) : t("voice.breakout_running", { n: b.rooms.length }))
        + (b.ends ? " · " + t("voice.breakout_left", { time: mmss(b.ends - Date.now()) }) : "")));
      if (inGroup) {
        const back = h("button", "btn-quiet", t("voice.breakout_back"));
        back.type = "button";
        back.addEventListener("click", () => V().join(b.parent, true));
        box.append(back);
      }
      out.push(box);
    }
    return out;
  }

  function menuItems(S, { host, prefs }) {
    const items = [];
    if (S.cfg && S.cfg.videoOn && window.UCNGVideo) {
      items.push(["", UCNGVideo.view === "gallery" ? t("voice.view_speaker") : t("voice.view_gallery"), () => UCNGVideo.setView(UCNGVideo.view === "gallery" ? "speaker" : "gallery")]);
    }
    if (S.cfg && S.cfg.captions) {
      items.push(["", prefs.captions ? t("voice.captions_hide") : t("voice.captions_show"), () => { prefs.captions = !prefs.captions; V().savePrefs(); renderCaptions(); }]);
      items.push(["", prefs.noCapSend ? t("voice.captions_send_on") : t("voice.captions_send_off"), () => { prefs.noCapSend = !prefs.noCapSend; V().savePrefs(); V().render(); }]);
    }
    if (transcript.length) {
      items.push(["", t("voice.transcript_download"), downloadTranscript]);
      if (host) items.push(["", t("voice.transcript_post"), postTranscript]);
    }
    items.push(["", t("voice.stats"), toggleStats]);
    if (host) {
      items.push(["", t("voice.mute_all"), () => hostAction("muteall")]);
      if (S.cfg && S.cfg.videoOn) items.push(["", t("voice.cams_off"), () => hostAction("camsoff")]);
      const b = breakoutOf(S.room);
      if (b && b.parent === S.room) items.push(["", t("voice.breakout_end"), () => endBreakout(S.room)]);
      else if (!b) items.push(["", t("voice.breakout"), openBreakout]);
      items.push(["", rec ? t("voice.rec_stop") : t("voice.rec_start"), () => (rec ? stopRec() : startRec())]);
      items.push(["", t("voice.end_all"), () => hostAction("endall"), "danger"]);
    }
    return items;
  }

  window.UCNGExtras = {
    onReact, onPeer, onCaption, onCapture, onBreakout, onJoined, onLeft,
    barNotes, menuItems, renderMobile, routeAudio, pip,
    get recording() { return !!rec; },
  };
})();
