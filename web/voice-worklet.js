// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Audio-Worklets der Sprachkanäle:
//   ucng-capture  – sammelt das Mikrofon zu 20-ms-Rahmen (960 Werte bei 48 kHz) mit Pegel
//   ucng-playback – Jitter-Puffer je Teilnehmer, mischt alle mit eigener Lautstärke

const FRAME = 960; // 20 ms bei 48 kHz

class Capture extends AudioWorkletProcessor {
  constructor(opts) {
    super();
    this.channels = (opts.processorOptions && opts.processorOptions.channels) || 1;
    this.buf = [];
    for (let c = 0; c < this.channels; c++) this.buf.push(new Float32Array(FRAME));
    this.pos = 0;
    this.on = true;
    this.port.onmessage = (e) => { if (e.data && "on" in e.data) this.on = !!e.data.on; };
  }

  process(inputs) {
    const input = inputs[0];
    if (!input || !input.length) return true;
    const n = input[0].length;
    let i = 0;
    while (i < n) {
      const take = Math.min(n - i, FRAME - this.pos);
      for (let c = 0; c < this.channels; c++) {
        const src = input[Math.min(c, input.length - 1)];
        this.buf[c].set(src.subarray(i, i + take), this.pos);
      }
      this.pos += take;
      i += take;
      if (this.pos === FRAME) {
        this.pos = 0;
        let sum = 0;
        const b0 = this.buf[0];
        for (let k = 0; k < FRAME; k++) sum += b0[k] * b0[k];
        const rms = Math.sqrt(sum / FRAME);
        if (this.on) {
          const out = this.buf.map((b) => b.slice());
          this.port.postMessage({ rms, frame: out }, out.map((b) => b.buffer));
        } else {
          this.port.postMessage({ rms });
        }
      }
    }
    return true;
  }
}

const START = 3; // so viele Rahmen sammeln, bevor abgespielt wird (60 ms)
const MAXQ = 12; // mehr gepuffert → auf START kürzen (Verzögerung begrenzen)

class Playback extends AudioWorkletProcessor {
  constructor() {
    super();
    this.peers = new Map(); // idx → { q: [Float32Array[]], off, playing, gain, sum, n }
    this.master = 1;
    this.blocks = 0;
    this.port.onmessage = (e) => {
      const d = e.data;
      if (d.type === "frame") {
        const p = this.peer(d.idx);
        p.q.push(d.frame);
        if (p.q.length > MAXQ) { p.q.splice(0, p.q.length - START); p.off = 0; }
      } else if (d.type === "gain") {
        this.peer(d.idx).gain = d.gain;
      } else if (d.type === "master") {
        this.master = d.gain;
      } else if (d.type === "remove") {
        this.peers.delete(d.idx);
      } else if (d.type === "clear") {
        this.peers.clear();
      }
    };
  }

  peer(idx) {
    let p = this.peers.get(idx);
    if (!p) { p = { q: [], off: 0, playing: false, gain: 1, sum: 0, n: 0 }; this.peers.set(idx, p); }
    return p;
  }

  process(_inputs, outputs) {
    const out = outputs[0];
    const len = out[0].length;
    for (const ch of out) ch.fill(0);
    for (const p of this.peers.values()) {
      if (!p.playing) {
        if (p.q.length >= START) p.playing = true;
        else continue;
      }
      let w = 0;
      while (w < len) {
        const f = p.q[0];
        if (!f) { p.playing = false; break; } // Lücke: wieder puffern
        const take = Math.min(len - w, f[0].length - p.off);
        const g = p.gain * this.master;
        for (let c = 0; c < out.length; c++) {
          const src = f[Math.min(c, f.length - 1)];
          const dst = out[c];
          for (let k = 0; k < take; k++) {
            const s = src[p.off + k];
            dst[w + k] += s * g;
            if (c === 0) { p.sum += s * s; p.n++; }
          }
        }
        w += take;
        p.off += take;
        if (p.off >= f[0].length) { p.q.shift(); p.off = 0; }
      }
    }
    // weiche Begrenzung (über 100 % Lautstärke, mehrere gleichzeitig)
    for (const ch of out) {
      for (let k = 0; k < len; k++) {
        const x = ch[k];
        if (x > 0.8) ch[k] = 0.8 + 0.2 * Math.tanh((x - 0.8) / 0.2);
        else if (x < -0.8) ch[k] = -0.8 + 0.2 * Math.tanh((x + 0.8) / 0.2);
      }
    }
    // Pegel für die Sprech-Anzeige, etwa 12 × pro Sekunde
    if (++this.blocks >= 32) {
      this.blocks = 0;
      const levels = {};
      for (const [idx, p] of this.peers) {
        levels[idx] = p.n ? Math.sqrt(p.sum / p.n) : 0;
        p.sum = 0; p.n = 0;
      }
      this.port.postMessage({ levels });
    }
    return true;
  }
}

registerProcessor("ucng-capture", Capture);
registerProcessor("ucng-playback", Playback);
