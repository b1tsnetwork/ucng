// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Ende-zu-Ende-Verschlüsselung der Sprachkanäle. Jeder Browser erzeugt je Sitzung ein
// ECDH-Schlüsselpaar (P-256) und veröffentlicht den öffentlichen Teil über den Relay. Die
// Person mit dem kleinsten Index („schlüsselführend“) erzeugt den Kanalschlüssel (AES-GCM 128)
// und schickt ihn jeder Person einzeln verpackt (ECDH → HKDF → AES-GCM). Verlässt jemand den
// Kanal oder wechselt die Führung, gibt es einen neuen Schlüssel (Generation 0–255).
//
// Jeder Rahmen (Sprache, Bildschirm, Ton, Kamera) wird so verschickt:
//   [Generation u8][IV 12][Geheimtext + Prüfsumme 16]
// Der Relay sieht nur das. Gegen einen Server, der öffentliche Schlüssel austauscht (Mittelsmann),
// hilft der Sicherheitscode: SHA-256 über den öffentlichen Schlüssel der führenden Person und den
// Kanalschlüssel. Er muss bei allen gleich sein – das vergleicht man mündlich.
(() => {
  "use strict";

  const enc = new TextEncoder();
  const b64 = (u8) => btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const unb64 = (s) => { s = s.replace(/-/g, "+").replace(/_/g, "/"); while (s.length % 4) s += "="; return Uint8Array.from(atob(s), (c) => c.charCodeAt(0)); };

  let st = null; // { link, pair, pubRaw, pkText, keys: Map(ep → CryptoKey), raw: Map(ep → Uint8Array), cur, leader, salt, ctr, onChange, rekeyTimer, wrappedFor:Set }

  function supported() { return !!(window.crypto && crypto.subtle); }

  // start: für eine Sprachverbindung. link: { sendText(o), peers(), you(), onChange() }
  async function start(link) {
    stop();
    const s = { link, keys: new Map(), raw: new Map(), cur: -1, leader: 0, ctr: 0n, wrappedFor: new Map(), code: "" };
    st = s;
    s.salt = crypto.getRandomValues(new Uint8Array(4));
    s.pair = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, false, ["deriveBits"]);
    s.pubRaw = new Uint8Array(await crypto.subtle.exportKey("raw", s.pair.publicKey));
    s.pkText = b64(s.pubRaw);
    if (st !== s) return;
    link.sendText({ t: "state", pk: s.pkText });
    peersChanged();
  }
  function stop() { if (st) clearTimeout(st.rekeyTimer); st = null; }

  // Schlüssel zum Verpacken zwischen mir und einer anderen Person
  async function wrapKey(pkText) {
    const pub = await crypto.subtle.importKey("raw", unb64(pkText), { name: "ECDH", namedCurve: "P-256" }, false, []);
    const bits = await crypto.subtle.deriveBits({ name: "ECDH", public: pub }, st.pair.privateKey, 256);
    const hk = await crypto.subtle.importKey("raw", bits, "HKDF", false, ["deriveKey"]);
    return crypto.subtle.deriveKey({ name: "HKDF", hash: "SHA-256", salt: enc.encode("ucng-e2ee-v1"), info: enc.encode("wrap") }, hk, { name: "AES-GCM", length: 128 }, false, ["encrypt", "decrypt"]);
  }

  // Wer führt? Kleinster Index unter allen mit öffentlichem Schlüssel.
  function leaderOf() {
    let best = 0;
    for (const p of st.link.peers()) if (p.pk && (!best || p.i < best)) best = p.i;
    return best;
  }

  // Nach jeder Änderung der Teilnehmer
  function peersChanged() {
    const s = st;
    if (!s || !s.pkText) return;
    const me = s.link.you();
    const lead = leaderOf();
    const others = s.link.peers().filter((p) => p.i !== me && p.pk);
    const prevLeader = s.leader;
    s.leader = lead;
    if (lead !== me) { emit(); return; }
    // ich führe: neuer Schlüssel, wenn ich gerade Führung übernehme oder jemand ging
    const gone = [...s.wrappedFor.keys()].some((i) => !others.find((p) => p.i === i && p.pk === s.wrappedFor.get(i)));
    if (prevLeader !== me || s.cur < 0 || gone) {
      clearTimeout(s.rekeyTimer);
      s.rekeyTimer = setTimeout(() => rekey(s), s.cur < 0 ? 0 : 400);
      return;
    }
    for (const p of others) if (s.wrappedFor.get(p.i) !== p.pk) sendKeyTo(s, p, s.cur); // neu dabei
  }

  async function rekey(s) {
    if (st !== s) return;
    const raw = crypto.getRandomValues(new Uint8Array(16));
    const ep = (s.cur + 1) & 255;
    await install(s, ep, raw);
    s.wrappedFor = new Map();
    const me = s.link.you();
    for (const p of s.link.peers()) if (p.i !== me && p.pk) sendKeyTo(s, p, ep);
  }

  async function sendKeyTo(s, p, ep) {
    const raw = s.raw.get(ep);
    if (!raw) return;
    s.wrappedFor.set(p.i, p.pk);
    try {
      const w = await wrapKey(p.pk);
      const iv = crypto.getRandomValues(new Uint8Array(12));
      const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv, additionalData: enc.encode("ucng-key:" + ep) }, w, raw));
      if (st !== s) return;
      const x = new Uint8Array(12 + ct.length);
      x.set(iv); x.set(ct, 12);
      s.link.sendText({ t: "e2k", to: p.i, ep, x: b64(x) });
    } catch { s.wrappedFor.delete(p.i); }
  }

  async function install(s, ep, raw) {
    const key = await crypto.subtle.importKey("raw", raw, { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
    if (st !== s) return;
    s.keys.set(ep, key);
    s.raw.set(ep, raw);
    for (const k of [...s.keys.keys()]) if (k !== ep && k !== ((ep + 255) & 255)) { s.keys.delete(k); s.raw.delete(k); } // die vorige behalten (Übergang)
    s.cur = ep;
    s.ctr = 0n;
    await updateCode(s);
    emit();
  }

  // verpackter Schlüssel von der führenden Person
  async function onKey(m) {
    const s = st;
    if (!s || m.from !== s.leader) return;
    const lp = s.link.peers().find((p) => p.i === m.from);
    if (!lp || !lp.pk) return;
    try {
      const x = unb64(m.x);
      const w = await wrapKey(lp.pk);
      const raw = new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: x.subarray(0, 12), additionalData: enc.encode("ucng-key:" + m.ep) }, w, x.subarray(12)));
      if (raw.length === 16) await install(s, m.ep & 255, raw);
    } catch { /* nicht für mich oder manipuliert */ }
  }

  async function updateCode(s) {
    const lead = s.link.peers().find((p) => p.i === s.leader);
    const pk = s.leader === s.link.you() ? s.pkText : lead ? lead.pk : "";
    const raw = s.raw.get(s.cur);
    if (!pk || !raw) { s.code = ""; return; }
    const data = new Uint8Array([...enc.encode("ucng-sas-v1|" + pk + "|"), ...raw]);
    const h = new Uint8Array(await crypto.subtle.digest("SHA-256", data));
    const groups = [];
    for (let i = 0; i < 4; i++) groups.push(String(((h[i * 2] << 8) | h[i * 2 + 1]) % 10000).padStart(4, "0"));
    s.code = groups.join(" ");
  }

  function emit() { if (st && st.link.onChange) st.link.onChange(); }

  // seal: Rahmen verschlüsseln (Promise<Uint8Array|null>; null = noch kein Schlüssel)
  async function seal(data) {
    const s = st;
    if (!s || s.cur < 0) return null;
    const key = s.keys.get(s.cur);
    const iv = new Uint8Array(12);
    iv.set(s.salt);
    let c = s.ctr++;
    for (let i = 11; i >= 4; i--) { iv[i] = Number(c & 255n); c >>= 8n; }
    const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, data));
    const out = new Uint8Array(13 + ct.length);
    out[0] = s.cur; out.set(iv, 1); out.set(ct, 13);
    return out;
  }

  // open: Rahmen entschlüsseln (Promise<Uint8Array|null>)
  async function open(data) {
    const s = st;
    if (!s || data.length < 29) return null;
    const key = s.keys.get(data[0]);
    if (!key) return null;
    try {
      return new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: data.subarray(1, 13) }, key, data.subarray(13)));
    } catch { return null; }
  }

  // Reihenfolge wahren: je Strom eine Kette (WebCrypto verspricht keine Reihenfolge)
  function chain(holder, fn) {
    holder.e2 = (holder.e2 || Promise.resolve()).then(fn, fn);
    return holder.e2;
  }

  window.UCNGE2EE = {
    supported, start, stop, peersChanged, onKey, seal, open, chain,
    get active() { return !!st; },
    get ready() { return !!st && st.cur >= 0; },
    get code() { return st ? st.code : ""; },
    get leading() { return !!st && st.leader === st.link.you(); },
  };
})();
