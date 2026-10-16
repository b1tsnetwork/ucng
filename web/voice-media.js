// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Gemeinsames für Bildschirm und Kameras: Wahl des Video-Encoders und Einstellungen für eine
// geringe Verzögerung.
//
// Reihenfolge: zuerst Hardware (Grafikkarte) – H.264, VP9, AV1 –, dann Software (VP9, VP8, H.264).
// Wo möglich mit zeitlichen Ebenen (SVC, „L1T3“): Der Relay kann dann für langsame Zuschauer
// einzelne Bilder weglassen, ohne dass das Bild einfriert – sie sehen z. B. 30 statt 60 Bilder je
// Sekunde. H.264 kommt als Annex B (Parameter in jedem Schlüsselbild), damit Zuschauer jederzeit
// einsteigen können.
(() => {
  "use strict";

  function avc(w, h, fps) {
    const px = w * h * fps;
    // Level: 4.0 (1080p30), 4.2 (1080p60), 5.1 (1440p60, 4K30), 5.2 (4K60) – High Profile
    const level = px <= 1920 * 1088 * 30 ? "28" : px <= 1920 * 1088 * 64 ? "2a" : px <= 3840 * 2160 * 32 ? "33" : "34";
    return "avc1.6400" + level;
  }
  function vp9(w, h) { return w * h > 1920 * 1088 ? "vp09.00.51.08" : "vp09.00.41.08"; }
  function av1(w, h, fps) { return w * h > 1920 * 1088 ? (fps > 30 ? "av01.0.13M.08" : "av01.0.12M.08") : "av01.0.08M.08"; }

  // Kandidaten in der Reihenfolge der Vorliebe
  function candidates(w, h, fps, opts) {
    const hw = [avc(w, h, fps), vp9(w, h), av1(w, h, fps)];
    const sw = [vp9(w, h), "vp8", avc(w, h, fps)];
    if (opts && opts.text) { hw.splice(0, 3, vp9(w, h), avc(w, h, fps), av1(w, h, fps)); } // Text: VP9 ist schärfer
    const out = [];
    for (const codec of hw) out.push({ codec, hw: "prefer-hardware" });
    for (const codec of sw) out.push({ codec, hw: "prefer-software" });
    return out;
  }

  function config(c, w, h, fps, kbps, svc, opts) {
    const cfg = { codec: c.codec, width: w, height: h, bitrate: Math.round(kbps * 1000), framerate: fps,
      latencyMode: "realtime", bitrateMode: "variable", hardwareAcceleration: c.hw };
    if (svc) cfg.scalabilityMode = svc;
    if (c.codec.startsWith("avc1")) cfg.avc = { format: "annexb" };
    if (opts && opts.hint) cfg.contentHint = opts.hint;
    return cfg;
  }

  // pick: bester unterstützter Encoder für diese Größe. Liefert { codec, hw, svc } oder null.
  async function pick(w, h, fps, kbps, opts) {
    if (!window.VideoEncoder) return null;
    const svcModes = fps >= 30 ? ["L1T3", "L1T2", ""] : ["L1T2", ""];
    for (const c of candidates(w, h, fps, opts)) {
      for (const svc of svcModes) {
        try {
          const r = await VideoEncoder.isConfigSupported(config(c, w, h, fps, kbps, svc, opts));
          if (r.supported) return { codec: c.codec, hw: c.hw, svc };
        } catch { /* nächster */ }
      }
    }
    return null;
  }

  // Zeitebene eines kodierten Bildes (aus den Metadaten des Encoders)
  function tidOf(meta) {
    const id = meta && meta.svc && meta.svc.temporalLayerId;
    return id > 0 ? Math.min(3, id) : 0;
  }

  function decoderConfig(codec) {
    return { codec, optimizeForLatency: true, hardwareAcceleration: "no-preference" };
  }

  // Sendepuffer: höchstens so viele Bytes dürfen in der WebSocket anstehen (≈ 250 ms bei kbps)
  function sendBudget(kbps) { return Math.max(192 * 1024, Math.round(kbps * 125 * 0.25)); }

  window.UCNGMedia = { pick, config, tidOf, decoderConfig, sendBudget };
})();
