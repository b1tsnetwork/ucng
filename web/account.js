// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
// UCNG – Nutzerkonten und Ende-zu-Ende-Verschlüsselung (Modus „hosted“).
//
// Das Passwort verlässt den Browser nie: Aus Passwort + Salz werden per PBKDF2 512 Bit abgeleitet.
// Die erste Hälfte („auth“) geht an den Server, die zweite verschlüsselt den privaten ECDH-Schlüssel.
// Direktnachrichten: ECDH (P-256) → HKDF → AES-GCM. Der Server sieht nur Geheimtext.
(() => {
  "use strict";

  const enc = new TextEncoder();
  const dec = new TextDecoder();
  const subtle = window.crypto && window.crypto.subtle;
  const KDF_ITER = 300000;
  const AAD = enc.encode("gochat-dm-v1"); // Version 1 (ältere Nachrichten)
  // Version 2: Absender- und Empfängerschlüssel sind mit authentifiziert – der Server kann sie
  // nicht austauschen, ohne dass das Entschlüsseln scheitert.
  const aad2 = (spk, rpk) => enc.encode("gochat-dm-v2|" + spk + "|" + rpk);
  const MIN_ITER = 100000;

  const b64 = (buf) => {
    const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
    let s = "";
    for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
    return btoa(s);
  };
  const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
  const rand = (n) => crypto.getRandomValues(new Uint8Array(n));
  const concat = (...arrs) => {
    const out = new Uint8Array(arrs.reduce((n, a) => n + a.length, 0));
    let o = 0;
    for (const a of arrs) { out.set(a, o); o += a.length; }
    return out;
  };

  // ---------- Kryptografie ----------

  const C = {
    available: !!subtle,

    // derive: Passwort → { auth (Base64, für den Server), key (AES-GCM, bleibt hier) }
    async derive(password, saltB64, iter) {
      if (!(iter >= MIN_ITER)) throw new Error(_t("acct.der_server_verlangt_unsichere"));
      const base = await subtle.importKey("raw", enc.encode(password.normalize("NFKC")), "PBKDF2", false, ["deriveBits"]);
      const bits = new Uint8Array(await subtle.deriveBits({ name: "PBKDF2", hash: "SHA-256", salt: unb64(saltB64), iterations: iter }, base, 512));
      const key = await subtle.importKey("raw", bits.slice(32), "AES-GCM", false, ["encrypt", "decrypt"]);
      return { auth: b64(bits.slice(0, 32)), key };
    },

    async seal(key, bytes) {
      const iv = rand(12);
      const ct = new Uint8Array(await subtle.encrypt({ name: "AES-GCM", iv }, key, bytes));
      return b64(concat(iv, ct));
    },

    async open(key, b64data) {
      const raw = unb64(b64data);
      return new Uint8Array(await subtle.decrypt({ name: "AES-GCM", iv: raw.slice(0, 12) }, key, raw.slice(12)));
    },

    // newKeyPair: neues ECDH-Schlüsselpaar; der private Schlüssel kommt verschlüsselt zurück.
    async newKeyPair(wrapKey) {
      const kp = await subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits"]);
      const pub = b64(await subtle.exportKey("raw", kp.publicKey));
      const pkcs8 = new Uint8Array(await subtle.exportKey("pkcs8", kp.privateKey));
      const privKey = await C.seal(wrapKey, pkcs8);
      pkcs8.fill(0);
      return { pubKey: pub, privKey, priv: await C.importPriv(await C.open(wrapKey, privKey)) };
    },

    async importPriv(pkcs8) {
      const k = await subtle.importKey("pkcs8", pkcs8, { name: "ECDH", namedCurve: "P-256" }, false, ["deriveBits"]);
      pkcs8.fill(0);
      return k;
    },

    async unwrap(wrapKey, privKeyB64) {
      return C.importPriv(await C.open(wrapKey, privKeyB64));
    },

    async rewrap(oldKey, newKey, privKeyB64) {
      const raw = await C.open(oldKey, privKeyB64);
      const out = await C.seal(newKey, raw);
      raw.fill(0);
      return out;
    },

    // Schlüssel für eine Unterhaltung: gleich auf beiden Seiten, auch instanzübergreifend.
    dmKeys: new Map(),
    async dmKey(priv, myPub, peerPub) {
      const id = myPub + "|" + peerPub;
      if (C.dmKeys.has(id)) return C.dmKeys.get(id);
      const p = (async () => {
        const peer = await subtle.importKey("raw", unb64(peerPub), { name: "ECDH", namedCurve: "P-256" }, false, []);
        const shared = await subtle.deriveBits({ name: "ECDH", public: peer }, priv, 256);
        const hk = await subtle.importKey("raw", shared, "HKDF", false, ["deriveKey"]);
        const [a, b] = [myPub, peerPub].sort();
        const salt = await subtle.digest("SHA-256", concat(unb64(a), unb64(b)));
        return subtle.deriveKey({ name: "HKDF", hash: "SHA-256", salt, info: enc.encode("gochat-dm-v1") }, hk, { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
      })();
      C.dmKeys.set(id, p);
      p.catch(() => C.dmKeys.delete(id));
      return p;
    },

    async encrypt(priv, myPub, peerPub, text) {
      const key = await C.dmKey(priv, myPub, peerPub);
      const iv = rand(12);
      const ct = await subtle.encrypt({ name: "AES-GCM", iv, additionalData: aad2(myPub, peerPub) }, key, enc.encode(text));
      return JSON.stringify({ v: 2, iv: b64(iv), ct: b64(ct), spk: myPub, rpk: peerPub });
    },

    async decrypt(priv, myPub, payload) {
      const p = JSON.parse(payload);
      let peer;
      if (p.spk === myPub) peer = p.rpk;
      else if (p.rpk === myPub) peer = p.spk;
      else throw new Error("oldkey");
      const key = await C.dmKey(priv, myPub, peer);
      const aad = p.v === 2 ? aad2(p.spk, p.rpk) : AAD;
      const pt = await subtle.decrypt({ name: "AES-GCM", iv: unb64(p.iv), additionalData: aad }, key, unb64(p.ct));
      return { text: dec.decode(pt), spk: p.spk, v: p.v };
    },

    // Sicherheitsnummer: 60 Ziffern, auf beiden Seiten gleich – zum Vergleichen (Telefon, persönlich).
    async safetyNumber(pubA, pubB) {
      const [a, b] = [pubA, pubB].sort();
      let data = concat(unb64(a), unb64(b));
      for (let i = 0; i < 1000; i++) data = new Uint8Array(await subtle.digest("SHA-256", data));
      let digits = "";
      for (let i = 0; i < 30; i += 5) {
        const n = ((data[i] * 2 ** 32) + ((data[i + 1] << 24) >>> 0) + (data[i + 2] << 16) + (data[i + 3] << 8) + data[i + 4]) % 1e10;
        digits += String(n).padStart(10, "0");
      }
      return digits.match(/.{5}/g).join(" ");
    },
  };

  // ---------- Schlüsselablage im Browser (IndexedDB, Schlüssel nicht exportierbar) ----------

  const KS = {
    db: null,
    open() {
      if (KS.db) return KS.db;
      KS.db = new Promise((resolve) => {
        try {
          const req = indexedDB.open("gochat-keys", 1);
          req.onupgradeneeded = () => req.result.createObjectStore("keys");
          req.onsuccess = () => resolve(req.result);
          req.onerror = () => resolve(null);
        } catch { resolve(null); }
      });
      return KS.db;
    },
    async get(id) {
      const db = await KS.open();
      if (!db) return null;
      return new Promise((resolve) => {
        try {
          const r = db.transaction("keys").objectStore("keys").get(id);
          r.onsuccess = () => resolve(r.result || null);
          r.onerror = () => resolve(null);
        } catch { resolve(null); }
      });
    },
    async put(id, v) {
      const db = await KS.open();
      if (!db) return;
      return new Promise((resolve) => {
        try {
          const tx = db.transaction("keys", "readwrite");
          tx.objectStore("keys").put(v, id);
          tx.oncomplete = tx.onerror = () => resolve();
        } catch { resolve(); }
      });
    },
    async del(id) {
      const db = await KS.open();
      if (!db) return;
      return new Promise((resolve) => {
        try {
          const tx = db.transaction("keys", "readwrite");
          tx.objectStore("keys").delete(id);
          tx.oncomplete = tx.onerror = () => resolve();
        } catch { resolve(); }
      });
    },
  };

  // ---------- Zustand ----------

  let ctx = null;      // von app.js: { settings, token, onSession, onLogout, toast, guestName, onKey }
  let myKey = null;    // { id, priv, pub }
  const peerKeys = new Map(); // room → { pubKey, fed, address, e2e, ts }

  async function call(path, body, method) {
    const headers = { "Content-Type": "application/json" };
    const tok = ctx && ctx.token();
    if (tok) headers.Authorization = "Bearer " + tok;
    const res = await fetch(path, { method: method || (body ? "POST" : "GET"), headers, body: body ? JSON.stringify(body) : undefined });
    const data = res.status === 204 ? {} : await res.json().catch(() => ({}));
    if (!res.ok) { const e = new Error(data.error || _t("acct.das_hat_nicht_geklappt")); e.data = data; e.status = res.status; throw e; }
    return data;
  }

  async function rememberKey(id, priv, pub) {
    myKey = { id, priv, pub };
    C.dmKeys.clear();
    await KS.put(location.origin + "|" + id, { priv, pub });
    if (ctx && ctx.onKey) ctx.onKey();
  }

  // ---------- Oberfläche ----------

  const H = (html) => { const t = document.createElement("template"); t.innerHTML = html.trim(); return t.content.firstElementChild; };
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  // Übersetzte Texte für die HTML-Vorlagen: T als reiner Text, TH mit erlaubtem Markup (<b>, <em> …).
  const T = (k, v) => esc(_t(k, v));
  const TH = (k, v) => { const d = document.createElement("div"); d.append(_tH(k, v)); return d.innerHTML; };

  let dlg, view = "login", pending = null; // pending: { login, auth, key, mode } während 2FA
  let invite = "", resetTicket = "";

  function buildDialog() {
    dlg = H(`<dialog id="authDialog" class="auth-dialog"><div class="auth-body"></div></dialog>`);
    document.body.append(dlg);
    dlg.addEventListener("cancel", (e) => { if (!closable()) e.preventDefault(); });
    // Wenn Gäste nichts sehen dürfen, bleibt die Anmeldung offen (Browser schließen Dialoge
    // nach wiederholtem Esc trotz preventDefault).
    dlg.addEventListener("close", () => {
      if (hosted().guests === "none" && !ctx.loggedIn()) setTimeout(() => { if (!dlg.open && !ctx.loggedIn()) show("login"); }, 50);
    });
  }

  function hosted() { return (ctx.settings().hosted) || {}; }
  function closable() { return hosted().guests !== "none" || view === "codes-done"; }

  function show(v, opts = {}) {
    view = v;
    const body = dlg.querySelector(".auth-body");
    body.replaceChildren(VIEWS[v](opts));
    if (!dlg.open) dlg.showModal();
    const f = body.querySelector("input:not([type=checkbox]):not([readonly])");
    if (f) setTimeout(() => f.focus(), 30);
  }

  function err(form, msg) { const e = form.querySelector(".form-error"); if (e) e.textContent = msg || ""; }
  function busy(form, on) { form.querySelectorAll("button").forEach((b) => (b.disabled = on)); }

  function guestLinks() {
    const h = hosted();
    const parts = [];
    if (h.guests === "write") parts.push(`<button type="button" class="text-btn" data-go="guest">${T("acct.ui_guest_write")}</button>`);
    if (h.guests === "read") parts.push(`<button type="button" class="text-btn" data-go="close">${T("acct.ui_guest_read")}</button>`);
    return parts.length ? `<p class="alt-action">${parts.join(" · ")}</p>` : "";
  }

  function wire(node) {
    node.querySelectorAll("[data-go]").forEach((b) => b.addEventListener("click", () => {
      const to = b.dataset.go;
      if (to === "close") { try { sessionStorage.setItem("gochat:reader", "1"); } catch { /* egal */ } view = "codes-done"; dlg.close(); return; }
      if (to === "guest") { dlg.close(); ctx.guestName(); return; }
      show(to);
    }));
    return node;
  }

  const pwField = (id, label, auto) =>
    `<label class="field"><span>${esc(label)}</span><input id="${id}" type="password" required minlength="${auto === "new-password" ? 10 : 1}" autocomplete="${auto}"></label>`;

  const VIEWS = {
    login() {
      const h = hosted();
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_login")}</h2>
        <p>${TH("acct.ui_login_lead", { title: ctx.settings().title || "UCNG" })}</p>
        <label class="field"><span>${T("acct.ui_name_or_mail")}</span><input id="authLogin" required autocomplete="username" maxlength="200"></label>
        ${pwField("authPw", _t("acct.ui_password"), "current-password")}
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="submit" class="btn-primary">${T("acct.ui_login")}</button></div>
        <p class="alt-action">
          <button type="button" class="text-btn" data-go="forgot">${T("acct.ui_forgot_q")}</button>
          ${h.registration || invite ? ` · <button type="button" class="text-btn" data-go="register">${T("acct.ui_register")}</button>` : ""}
        </p>
        ${guestLinks()}
      </form>`);
      f.querySelector("#authLogin").value = localStorage.getItem("gochat:login") || "";
      f.addEventListener("submit", async (e) => {
        e.preventDefault();
        err(f); busy(f, true);
        const login = f.querySelector("#authLogin").value.trim();
        try {
          const pre = await call("/api/account/prelogin", { login });
          const d = await C.derive(f.querySelector("#authPw").value, pre.kdfSalt, pre.kdfIter);
          pending = { login, auth: d.auth, key: d.key };
          await finishLogin({});
          try { localStorage.setItem("gochat:login", login); } catch { /* egal */ }
        } catch (x) {
          if (x.data && x.data.need2fa) { show("code"); return; }
          if (x.data && x.data.unverified) { show("verify", { login }); return; }
          err(f, x.message === "Failed to fetch" ? _t("acct.server_nicht_erreichbar") : x.message);
        } finally { busy(f, false); }
      });
      return wire(f);
    },

    code() {
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_code_title")}</h2>
        <p>${T("acct.ui_code_lead")}</p>
        <label class="field"><span>${T("acct.ui_code")}</span><input id="authCode" inputmode="numeric" autocomplete="one-time-code" maxlength="7" pattern="[0-9 ]{6,7}" required></label>
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" data-go="login">${T("acct.ui_back")}</button><button type="submit" class="btn-primary">${T("acct.ui_confirm")}</button></div>
        <p class="alt-action">${T("acct.ui_no_app")} <button type="button" class="text-btn" data-go="recovery">${T("acct.ui_use_recovery")}</button></p>
      </form>`);
      f.addEventListener("submit", async (e) => {
        e.preventDefault(); err(f); busy(f, true);
        try { await finishLogin({ code: f.querySelector("#authCode").value.replace(/\s/g, "") }); }
        catch (x) { err(f, x.message); } finally { busy(f, false); }
      });
      return wire(f);
    },

    recovery() {
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_recovery_code")}</h2>
        <p>${T("acct.ui_recovery_lead")}</p>
        <label class="field"><span>${T("acct.ui_code")}</span><input id="authRec" autocomplete="off" maxlength="20" required placeholder="xxxxx-xxxxx"></label>
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" data-go="code">${T("acct.ui_back")}</button><button type="submit" class="btn-primary">${T("acct.ui_login")}</button></div>
      </form>`);
      f.addEventListener("submit", async (e) => {
        e.preventDefault(); err(f); busy(f, true);
        try { await finishLogin({ recovery: f.querySelector("#authRec").value.trim() }); }
        catch (x) { err(f, x.message); } finally { busy(f, false); }
      });
      return wire(f);
    },

    register() {
      const h = hosted();
      const needMail = h.requireVerify;
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_register")}</h2>
        <p>${h.federation && h.domain ? TH("acct.ui_register_lead_fed", { domain: h.domain }) : T("acct.ui_register_lead")}</p>
        <label class="field"><span>${T("acct.ui_name")}</span><input id="regName" required autocomplete="username" maxlength="32" pattern="[A-Za-z0-9][A-Za-z0-9_.\\-]{0,30}[A-Za-z0-9_]" title="${T("acct.ui_name_rule")}"></label>
        <label class="field"><span>${needMail ? T("acct.ui_email") : T("acct.ui_email_optional")}</span><input id="regMail" type="email" ${needMail ? "required" : ""} autocomplete="email" maxlength="200"></label>
        ${pwField("regPw", _t("acct.ui_new_pw_min"), "new-password")}
        ${pwField("regPw2", _t("acct.ui_pw_repeat"), "new-password")}
        <p class="hint">${T("acct.ui_pw_hint")}</p>
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" data-go="login">${T("acct.ui_back")}</button><button type="submit" class="btn-primary">${T("acct.ui_register")}</button></div>
        ${guestLinks()}
      </form>`);
      if (!h.email) f.querySelector("#regMail").closest(".field").querySelector("span").textContent = _t("acct.e_mail_adresse_optional");
      f.addEventListener("submit", async (e) => {
        e.preventDefault(); err(f);
        const pw = f.querySelector("#regPw").value;
        if (pw !== f.querySelector("#regPw2").value) { err(f, _t("acct.die_passwoerter_stimmen_nicht_ueberein")); return; }
        if (pw.length < 10) { err(f, _t("acct.bitte_mindestens_10_zeichen")); return; }
        busy(f, true);
        try {
          const name = f.querySelector("#regName").value.trim();
          const salt = b64(rand(16));
          const d = await C.derive(pw, salt, KDF_ITER);
          const kp = await C.newKeyPair(d.key);
          const out = await call("/api/account/register", {
            name, email: f.querySelector("#regMail").value.trim(), invite,
            keys: { auth: d.auth, kdfSalt: salt, kdfIter: KDF_ITER, pubKey: kp.pubKey, privKey: kp.privKey },
          });
          invite = "";
          try { localStorage.setItem("gochat:login", name); } catch { /* egal */ }
          if (out.session) {
            ctx.setToken(out.session.token);
            await rememberKey(out.session.id, kp.priv, kp.pubKey);
            show("codes", { codes: out.recovery, then: () => ctx.onSession(out.session, true) });
          } else {
            show("codes", { codes: out.recovery, then: () => show("verify", { login: name, sent: true, mailError: out.mailError }) });
          }
        } catch (x) { err(f, x.message === "Failed to fetch" ? _t("acct.server_nicht_erreichbar") : x.message); }
        finally { busy(f, false); }
      });
      return wire(f);
    },

    codes({ codes, then, title }) {
      const text = codes.join("\n");
      const f = H(`<form class="auth-form">
        <h2>${title ? esc(title) : T("acct.ui_codes_title")}</h2>
        <p>${TH("acct.ui_codes_lead")}</p>
        <pre class="codes">${esc(text)}</pre>
        <div class="btn-row"><button type="button" class="btn-quiet" id="codesCopy">${T("acct.ui_copy")}</button><button type="button" class="btn-quiet" id="codesSave">${T("acct.ui_save_file")}</button></div>
        <label class="check"><input type="checkbox" id="codesOk"> ${T("acct.ui_codes_ok")}</label>
        <div class="actions"><button type="submit" class="btn-primary" disabled>${T("acct.ui_continue")}</button></div>
      </form>`);
      f.querySelector("#codesOk").addEventListener("change", (e) => (f.querySelector("[type=submit]").disabled = !e.target.checked));
      f.querySelector("#codesCopy").addEventListener("click", async () => {
        try { await navigator.clipboard.writeText(text); ctx.toast(_t("acct.codes_kopiert")); } catch { ctx.toast(_t("acct.kopieren_ging_nicht_bitte_markieren_und")); }
      });
      f.querySelector("#codesSave").addEventListener("click", () => {
        const a = document.createElement("a");
        a.href = URL.createObjectURL(new Blob([_t("acct.wiederherstellungscodes_fuer", { host: location.host, text: text })], { type: "text/plain" }));
        a.download = `gochat-recovery-${location.hostname}.txt`;
        document.body.append(a); a.click(); a.remove();
      });
      f.addEventListener("submit", (e) => { e.preventDefault(); view = "codes-done"; dlg.close(); then && then(); });
      return f;
    },

    verify({ login, sent, mailError }) {
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_verify_title")}</h2>
        <p>${sent ? T("acct.ui_verify_sent") : T("acct.ui_verify_pending")}</p>
        ${mailError ? `<p class="warn-box">${esc(mailError)}</p>` : ""}
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" id="verResend">${T("acct.ui_resend")}</button><button type="button" class="btn-primary" data-go="login">${T("acct.ui_to_login")}</button></div>
      </form>`);
      f.querySelector("#verResend").addEventListener("click", async () => {
        try { await call("/api/account/resend", { login }); ctx.toast(_t("acct.falls_das_konto_existiert_ist_eine_neue")); }
        catch (x) { err(f, x.message); }
      });
      return wire(f);
    },

    forgot() {
      const h = hosted();
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_forgot")}</h2>
        ${h.email ? `<p>${T("acct.ui_forgot_lead")}</p>
        <label class="field"><span>${T("acct.ui_name_or_mail")}</span><input id="fgLogin" required autocomplete="username" maxlength="200"></label>
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" data-go="login">${T("acct.ui_back")}</button><button type="submit" class="btn-primary">${T("acct.ui_send_link")}</button></div>
        <p class="alt-action">${T("acct.ui_no_mail")} <button type="button" class="text-btn" data-go="recoverReset">${T("acct.ui_with_recovery")}</button></p>`
        : `<p>${T("acct.ui_no_mailer")}</p>
        <div class="actions"><button type="button" class="btn-quiet" data-go="login">${T("acct.ui_back")}</button><button type="button" class="btn-primary" data-go="recoverReset">${T("acct.ui_continue")}</button></div>`}
        <p class="warn-box">${T("acct.ui_forgot_warn")}</p>
      </form>`);
      f.addEventListener("submit", async (e) => {
        e.preventDefault(); err(f); busy(f, true);
        try { await call("/api/account/forgot", { login: f.querySelector("#fgLogin").value.trim() }); ctx.toast(_t("acct.falls_es_das_konto_gibt_ist_eine_e_mail")); show("login"); }
        catch (x) { err(f, x.message); } finally { busy(f, false); }
      });
      return wire(f);
    },

    recoverReset() { return resetForm({ withCode: true }); },
    reset(opts) { return resetForm(opts); },
  };

  function resetForm({ withCode, name, twoFA }) {
    const f = H(`<form class="auth-form">
      <h2>${T("acct.ui_new_pw")}</h2>
      ${name ? `<p>${TH("acct.ui_for_account", { name })}</p>` : ""}
      ${withCode ? `<label class="field"><span>${T("acct.ui_name_or_mail")}</span><input id="rsLogin" required autocomplete="username" maxlength="200"></label>
      <label class="field"><span>${T("acct.ui_recovery_code")}</span><input id="rsRec" required autocomplete="off" maxlength="20" placeholder="xxxxx-xxxxx"></label>` : ""}
      ${pwField("rsPw", _t("acct.ui_new_pw_min2"), "new-password")}
      ${pwField("rsPw2", _t("acct.ui_new_pw_repeat"), "new-password")}
      ${twoFA ? `<label class="field"><span>${T("acct.ui_code_authapp")}</span><input id="rsCode" inputmode="numeric" autocomplete="one-time-code" maxlength="7" required></label>` : ""}
      <p class="warn-box">${T("acct.ui_reset_warn")}</p>
      <p class="form-error" role="alert"></p>
      <div class="actions"><button type="button" class="btn-quiet" data-go="login">${T("acct.ui_cancel")}</button><button type="submit" class="btn-primary">${T("acct.ui_set_pw")}</button></div>
    </form>`);
    f.addEventListener("submit", async (e) => {
      e.preventDefault(); err(f);
      const pw = f.querySelector("#rsPw").value;
      if (pw !== f.querySelector("#rsPw2").value) { err(f, _t("acct.die_passwoerter_stimmen_nicht_ueberein")); return; }
      if (pw.length < 10) { err(f, _t("acct.bitte_mindestens_10_zeichen")); return; }
      busy(f, true);
      try {
        const salt = b64(rand(16));
        const d = await C.derive(pw, salt, KDF_ITER);
        const kp = await C.newKeyPair(d.key);
        const body = { keys: { auth: d.auth, kdfSalt: salt, kdfIter: KDF_ITER, pubKey: kp.pubKey, privKey: kp.privKey } };
        if (withCode) { body.login = f.querySelector("#rsLogin").value.trim(); body.recovery = f.querySelector("#rsRec").value.trim(); }
        else { body.ticket = resetTicket; if (twoFA) body.code = f.querySelector("#rsCode").value.replace(/\s/g, ""); }
        const out = await call("/api/account/reset", body);
        resetTicket = "";
        ctx.toast(_t("acct.neues_passwort_gesetzt_bitte_melde_dich"));
        try { localStorage.setItem("gochat:login", out.name || ""); } catch { /* egal */ }
        show("login");
      } catch (x) { err(f, x.message); } finally { busy(f, false); }
    });
    return wire(f);
  }

  async function finishLogin(extra) {
    const out = await call("/api/account/login", Object.assign({ login: pending.login, auth: pending.auth }, extra));
    const s = out.session;
    let priv = null, pub = out.pubKey, broken = false;
    if (out.privKey) {
      try { priv = await C.unwrap(pending.key, out.privKey); } catch { broken = true; }
    } else { // noch kein Schlüsselpaar: neues anlegen
      const kp = await C.newKeyPair(pending.key);
      ctx.setToken(s.token);
      await call("/api/account/keys", Object.assign({ auth: pending.auth, pubKey: kp.pubKey, privKey: kp.privKey }, extra.code ? { code: extra.code } : {}), "POST").catch(() => {});
      priv = kp.priv; pub = kp.pubKey;
    }
    pending = null;
    ctx.setToken(s.token);
    if (priv) await rememberKey(s.id, priv, pub);
    // Lässt sich der Schlüssel nicht öffnen, bleibt er unangetastet (sonst wären alle älteren
    // verschlüsselten Nachrichten verloren) – die Nachrichten zeigen dann „Entsperren“.
    if (broken) setTimeout(() => ctx.toast(_t("acct.dein_schluessel_fuer_verschluesselte")), 500);
    view = "codes-done";
    dlg.close();
    ctx.onSession(s, true);
  }

  // ---------- Entsperren (Schlüssel fehlt auf diesem Gerät) ----------

  async function unlock() {
    const acc = await call("/api/account");
    return new Promise((resolve) => {
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_unlock_title")}</h2>
        <p>${T("acct.ui_unlock_lead")}</p>
        ${pwField("ulPw", _t("acct.ui_password"), "current-password")}
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" id="ulCancel">${T("acct.ui_later")}</button><button type="submit" class="btn-primary">${T("acct.ui_unlock")}</button></div>
      </form>`);
      f.querySelector("#ulCancel").addEventListener("click", () => { dlg.close(); resolve(false); });
      f.addEventListener("submit", async (e) => {
        e.preventDefault(); err(f); busy(f, true);
        try {
          const d = await C.derive(f.querySelector("#ulPw").value, acc.kdfSalt, acc.kdfIter);
          let priv;
          try { priv = await C.unwrap(d.key, acc.privKey); } catch { throw new Error(_t("acct.das_passwort_stimmt_nicht")); }
          await rememberKey(acc.id, priv, acc.pubKey);
          dlg.close();
          resolve(true);
        } catch (x) { err(f, x.message); } finally { busy(f, false); }
      });
      view = "unlock";
      dlg.querySelector(".auth-body").replaceChildren(f);
      if (!dlg.open) dlg.showModal();
      setTimeout(() => f.querySelector("input").focus(), 30);
    });
  }

  // ---------- Konto & Sicherheit ----------

  let setDlg = null;

  // runWithPassword: Dialog mit Passwort (und weiteren Feldern). fn bekommt die abgeleiteten
  // Schlüssel und die Feldwerte; Fehler erscheinen im Dialog, der dann offen bleibt.
  function runWithPassword(acc, title, text, extraHTML, fn) {
    // Mit aktiver 2FA verlangen sensible Änderungen zusätzlich einen aktuellen Code.
    const tfa = acc.account && acc.account.twoFA && !(extraHTML || "").includes('id="apCode"')
      ? `<label class="field"><span>${T("acct.ui_code_authapp")}</span><input id="apTfa" inputmode="numeric" autocomplete="one-time-code" maxlength="7" required></label>` : "";
    return new Promise((resolve) => {
      const f = H(`<form class="auth-form">
        <h2>${esc(title)}</h2>
        <p>${text}</p>
        ${pwField("apPw", _t("acct.ui_password"), "current-password")}
        ${extraHTML || ""}
        ${tfa}
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" data-cancel>${T("acct.ui_cancel")}</button><button type="submit" class="btn-primary">${T("acct.ui_continue")}</button></div>
      </form>`);
      const d = H(`<dialog class="auth-dialog"></dialog>`);
      d.append(f);
      document.body.append(d);
      const done = (v) => { if (d.open) d.close(); d.remove(); resolve(v); };
      f.querySelector("[data-cancel]").addEventListener("click", () => done(false));
      d.addEventListener("cancel", (e) => { e.preventDefault(); done(false); });
      f.addEventListener("submit", async (e) => {
        e.preventDefault();
        err(f); busy(f, true);
        const vals = {};
        f.querySelectorAll("input").forEach((i) => (vals[i.id] = i.type === "checkbox" ? i.checked : i.value));
        vals.code = (vals.apTfa || "").replace(/\s/g, "");
        try {
          const k = await C.derive(vals.apPw, acc.kdfSalt, acc.kdfIter);
          await fn(k, vals);
          done(true);
        } catch (x) {
          err(f, x.status === 401 && !(x.data && x.data.need2fa) ? _t("acct.das_passwort_oder_der_code_stimmt_nicht") : x.message);
        } finally { busy(f, false); }
      });
      d.showModal();
      setTimeout(() => f.querySelector("input").focus(), 30);
    });
  }

  async function openSettings() {
    let acc;
    try { acc = await call("/api/account"); } catch (x) { ctx.toast(x.message); return; }
    const h = hosted();
    if (setDlg) setDlg.remove();
    setDlg = H(`<dialog class="profile-dialog account-dialog">
      <div class="profile-head">
        <span class="avatar-lock" aria-hidden="true">🔐</span>
        <div><h2>${T("acct.ui_settings_title")}</h2><p>${esc(acc.name)}${h.federation && h.domain ? " · " + TH("acct.ui_reachable_as", { address: acc.name + "@" + h.domain }) : ""}</p></div>
        <button type="button" class="icon-btn close-x" data-x aria-label="${T("acct.ui_close")}"><svg viewBox="0 0 20 20" aria-hidden="true"><path d="M5 5l10 10M15 5 5 15" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg></button>
      </div>
      <section class="profile-section">
        <h3 class="sec-title">${T("acct.ui_email")}</h3>
        <p>${acc.account.email ? `${esc(acc.account.email)} – ${acc.account.emailOk ? `<span class='ok'>${T("acct.ui_confirmed")}</span>` : `<span class='warn'>${T("acct.ui_unconfirmed")}</span>`}` : T("acct.ui_no_email")}${acc.account.pendingEmail ? "<br>" + TH("acct.ui_pending_email", { email: acc.account.pendingEmail }) : ""}</p>
        <div class="btn-row"><button type="button" class="btn-quiet" data-a="email">${acc.account.email ? T("acct.ui_change") : T("acct.ui_add")}</button></div>
      </section>
      <section class="profile-section">
        <h3 class="sec-title">${T("acct.ui_password")}</h3>
        <p>${T("acct.ui_pw_lead")}</p>
        <div class="btn-row"><button type="button" class="btn-quiet" data-a="password">${T("acct.ui_change_pw")}</button></div>
      </section>
      <section class="profile-section">
        <h3 class="sec-title">${T("acct.ui_2fa")}</h3>
        <p>${acc.account.twoFA ? TH("acct.ui_2fa_on") : T("acct.ui_2fa_off")}</p>
        <div class="btn-row">${acc.account.twoFA ? `<button type="button" class="btn-quiet" data-a="2faoff">${T("acct.ui_turn_off")}</button>` : `<button type="button" class="btn-quiet" data-a="2faon">${T("acct.ui_set_up")}</button>`}</div>
      </section>
      <section class="profile-section">
        <h3 class="sec-title">${T("acct.ui_recovery_codes")}</h3>
        <p>${T("acct.ui_codes_left", { n: acc.account.recoveryLeft })}${acc.account.recoveryLeft < 4 ? ` <span class='warn'>${T("acct.ui_codes_low")}</span>` : ""}</p>
        <div class="btn-row"><button type="button" class="btn-quiet" data-a="recovery">${T("acct.ui_new_codes")}</button></div>
      </section>
      <section class="profile-section">
        <h3 class="sec-title">${T("acct.ui_devices")}</h3>
        <ul class="session-list"></ul>
        <div class="btn-row"><button type="button" class="btn-quiet" data-a="others">${T("acct.ui_sign_out_others")}</button></div>
      </section>
      <section class="profile-section">
        <h3 class="sec-title">${T("acct.ui_encryption")}</h3>
        <p>${T("acct.ui_fingerprint")} <code class="fp"></code></p>
        ${h.federation ? `<label class="check"><input type="checkbox" id="accFed" ${acc.account.noFed ? "" : "checked"}> ${T("acct.ui_accept_fed")}</label>` : ""}
      </section>
      <div class="profile-foot"><button type="button" class="text-btn danger" data-a="delete">${T("acct.ui_delete_account")}</button></div>
    </dialog>`);
    document.body.append(setDlg);
    setDlg.querySelector("[data-x]").addEventListener("click", () => setDlg.close());
    const ul = setDlg.querySelector(".session-list");
    const fmt = new Intl.DateTimeFormat(GoChatI18n.locale, { dateStyle: "medium", timeStyle: "short" });
    for (const s of acc.sessions) {
      const li = H(`<li><span><b>${esc(agentName(s.agent))}</b>${s.current ? ` <span class='ok'>${T("acct.ui_this_device")}</span>` : ""}<small>${T("acct.ui_last_used", { time: fmt.format(new Date(s.lastUsed)) })}</small></span></li>`);
      if (!s.current) {
        const b = H(`<button type="button" class="text-btn">${T("acct.ui_sign_out")}</button>`);
        b.addEventListener("click", async () => { try { await call("/api/account/sessions/" + s.id, null, "DELETE"); li.remove(); } catch (x) { ctx.toast(x.message); } });
        li.append(b);
      }
      ul.append(li);
    }
    if (acc.pubKey) sha(acc.pubKey).then((fp) => (setDlg.querySelector(".fp").textContent = fp));
    const fed = setDlg.querySelector("#accFed");
    if (fed) fed.addEventListener("change", async () => {
      try { await call("/api/account/federation", { allow: fed.checked }); ctx.toast(fed.checked ? _t("acct.nachrichten_von_anderen_instanzen") : _t("acct.nachrichten_von_anderen_instanzen_2")); }
      catch (x) { ctx.toast(x.message); fed.checked = !fed.checked; }
    });
    setDlg.querySelectorAll("[data-a]").forEach((b) => b.addEventListener("click", () => ACTIONS[b.dataset.a](acc)));
    setDlg.addEventListener("close", () => setDlg && setDlg.remove());
    setDlg.showModal();
  }

  async function sha(b64key) {
    const d = new Uint8Array(await subtle.digest("SHA-256", unb64(b64key)));
    return Array.from(d.slice(0, 16), (x) => x.toString(16).padStart(2, "0")).join("").match(/.{4}/g).join(" ");
  }

  function agentName(a) {
    if (!a) return _t("acct.unbekanntes_geraet");
    const os = /Android/.test(a) ? "Android" : /iPhone|iPad/.test(a) ? "iOS" : /Mac OS/.test(a) ? "macOS" : /Windows/.test(a) ? "Windows" : /Linux/.test(a) ? "Linux" : "";
    const br = /Edg\//.test(a) ? "Edge" : /Firefox\//.test(a) ? "Firefox" : /Chrome\//.test(a) ? "Chrome" : /Safari\//.test(a) ? "Safari" : _t("acct.browser");
    return os ? _t("acct.ui_agent", { browser: br, os }) : br;
  }

  const reopen = () => { if (setDlg) setDlg.close(); openSettings(); };

  const ACTIONS = {
    async email(acc) {
      const ok = await runWithPassword(acc, _t("acct.e_mail_adresse"), _t("acct.bitte_bestaetige_mit_deinem_passwort"),
        `<label class="field"><span>${T("acct.ui_new_email")}</span><input id="apMail" type="email" maxlength="200" value="${esc(acc.account.email || "")}"></label>`,
        async (d, v) => {
          const r = await call("/api/account/email", { auth: d.auth, code: v.code, email: v.apMail.trim() });
          ctx.toast(r.verify ? _t("acct.wir_haben_dir_einen_bestaetigungslink") : _t("acct.gespeichert"));
        });
      if (ok) reopen();
    },
    async password(acc) {
      const ok = await runWithPassword(acc, _t("acct.passwort_aendern"), _t("acct.gib_zuerst_dein_aktuelles_passwort_ein"),
        `${pwField("apNew", _t("acct.ui_new_pw_min2"), "new-password")}${pwField("apNew2", _t("acct.ui_new_pw_repeat"), "new-password")}`,
        async (old, v) => {
          if (v.apNew !== v.apNew2) throw new Error(_t("acct.die_neuen_passwoerter_stimmen_nicht"));
          if (v.apNew.length < 10) throw new Error(_t("acct.bitte_mindestens_10_zeichen"));
          let privKey;
          const salt = b64(rand(16));
          const nd = await C.derive(v.apNew, salt, KDF_ITER);
          try { privKey = await C.rewrap(old.key, nd.key, acc.privKey); } catch { throw new Error(_t("acct.das_aktuelle_passwort_stimmt_nicht")); }
          await call("/api/account/password", { auth: old.auth, code: v.code, keys: { auth: nd.auth, kdfSalt: salt, kdfIter: KDF_ITER, pubKey: acc.pubKey, privKey } });
          ctx.toast(_t("acct.passwort_geaendert_andere_geraete_wurden"));
        });
      if (ok) reopen();
    },
    async "2faon"(acc) {
      let setup = null;
      const ok = await runWithPassword(acc, _t("acct.zwei_faktor_anmeldung_einrichten"), _t("acct.bitte_bestaetige_mit_deinem_passwort"), "", async (d, v) => {
        setup = await call("/api/account/2fa/begin", { auth: d.auth, code: v.code });
      });
      if (!ok || !setup) return;
      const f = H(`<form class="auth-form">
        <h2>${T("acct.ui_connect_app")}</h2>
        <p>${T("acct.ui_connect_lead")}</p>
        <div class="qr">${setup.svg || ""}</div>
        <p class="secret"><code>${esc(setup.secret.match(/.{1,4}/g).join(" "))}</code></p>
        <label class="field"><span>${T("acct.ui_code_app")}</span><input id="tfCode" inputmode="numeric" autocomplete="one-time-code" maxlength="7" required></label>
        <p class="form-error" role="alert"></p>
        <div class="actions"><button type="button" class="btn-quiet" id="tfCancel">${T("acct.ui_cancel")}</button><button type="submit" class="btn-primary">${T("acct.ui_activate")}</button></div>
      </form>`);
      const d = H(`<dialog class="auth-dialog"></dialog>`);
      d.append(f); document.body.append(d);
      f.querySelector("#tfCancel").addEventListener("click", () => { d.close(); d.remove(); });
      f.addEventListener("submit", async (e) => {
        e.preventDefault(); err(f); busy(f, true);
        try {
          const r = await call("/api/account/2fa/enable", { code: f.querySelector("#tfCode").value.replace(/\s/g, "") });
          d.close(); d.remove();
          if (setDlg) setDlg.close();
          show("codes", { codes: r.recovery, title: _t("acct.neue_wiederherstellungscodes"), then: () => openSettings() });
          ctx.toast(_t("acct.zwei_faktor_anmeldung_ist_aktiv_andere"));
        } catch (x) { err(f, x.message); busy(f, false); }
      });
      d.showModal();
    },
    async "2faoff"(acc) {
      const ok = await runWithPassword(acc, _t("acct.zwei_faktor_anmeldung_ausschalten"), _t("acct.passwort_und_ein_aktueller_code_aus_der"),
        `<label class="field"><span>${T("acct.ui_code_app")}</span><input id="apCode" inputmode="numeric" autocomplete="one-time-code" maxlength="7" required></label>`,
        async (d, v) => { await call("/api/account/2fa/disable", { auth: d.auth, code: v.apCode.replace(/\s/g, "") }); ctx.toast(_t("acct.zwei_faktor_anmeldung_ist_aus")); });
      if (ok) reopen();
    },
    async recovery(acc) {
      let codes = null;
      const ok = await runWithPassword(acc, _t("acct.neue_wiederherstellungscodes"), _t("acct.die_bisherigen_codes_werden_ungueltig"), "", async (d, v) => {
        codes = (await call("/api/account/recovery", { auth: d.auth, code: v.code })).recovery;
      });
      if (ok && codes) { if (setDlg) setDlg.close(); show("codes", { codes, title: _t("acct.neue_wiederherstellungscodes"), then: () => openSettings() }); }
    },
    async others() {
      try { await call("/api/account/sessions/others", null, "DELETE"); ctx.toast(_t("acct.alle_anderen_geraete_sind_abgemeldet")); reopen(); }
      catch (x) { ctx.toast(x.message); }
    },
    async delete(acc) {
      const ok = await runWithPassword(acc, _t("acct.konto_loeschen"), _t("acct.dein_konto_und_dein_name_werden"),
        `<label class="check"><input type="checkbox" id="apSure" required> ${T("acct.ui_delete_sure")}</label>`,
        async (d, v) => { await call("/api/account/delete", { auth: d.auth, code: v.code }); });
      if (ok) { if (setDlg) setDlg.close(); await forget(); ctx.onLogout(true); ctx.toast(_t("acct.dein_konto_wurde_geloescht")); }
    },
  };

  async function forget() {
    if (myKey) await KS.del(location.origin + "|" + myKey.id);
    myKey = null;
    C.dmKeys.clear();
    peerKeys.clear();
  }

  // ---------- Verschlüsselte Direktnachrichten ----------

  async function loadKey(userId) {
    if (myKey && myKey.id === userId) return true;
    const rec = await KS.get(location.origin + "|" + userId);
    if (rec && rec.priv) { myKey = { id: userId, priv: rec.priv, pub: rec.pub }; return true; }
    myKey = null;
    return false;
  }

  async function roomKey(room, force) {
    const c = peerKeys.get(room);
    if (c && !force && Date.now() - c.ts < 10 * 60 * 1000) return c;
    const k = await call(`/api/dms/${encodeURIComponent(room)}/key`);
    k.ts = Date.now();
    // Schlüsselwechsel des Gegenübers bemerken (TOFU im Browser)
    if (k.pubKey && myKey) {
      const store = `gochat:peerkey:${myKey.id}:${k.peer}`;
      let old = null;
      try { old = localStorage.getItem(store); localStorage.setItem(store, k.pubKey); } catch { /* egal */ }
      k.changed = !!(old && old !== k.pubKey);
    }
    peerKeys.set(room, k);
    return k;
  }

  async function encryptFor(room, text) {
    if (!myKey) throw new Error("locked");
    const k = await roomKey(room);
    if (!k.pubKey) throw new Error(k.error || _t("acct.die_andere_person_hat_noch_keinen"));
    return C.encrypt(myKey.priv, myKey.pub, k.pubKey, text);
  }

  // decrypt: Klartext und ob der Absenderschlüssel zur Person passt (own = eigene Nachricht).
  async function decrypt(payload, own, room) {
    if (!myKey) throw new Error("locked");
    let r;
    try { r = await C.decrypt(myKey.priv, myKey.pub, payload); }
    catch (e) { if (e.message === "oldkey") throw e; throw new Error("broken"); }
    let trusted = true;
    if (own !== undefined) {
      if (own) trusted = r.spk === myKey.pub;
      else {
        try { const k = await roomKey(room); trusted = !!k.pubKey && r.spk === k.pubKey; } catch { trusted = false; }
      }
    }
    return { text: r.text, trusted, legacy: r.v !== 2 };
  }

  async function safetyNumber(room) {
    const k = await roomKey(room);
    if (!myKey || !k.pubKey) return "";
    return C.safetyNumber(myKey.pub, k.pubKey);
  }

  // ---------- Start ----------

  function init(c) {
    ctx = c;
    if (!dlg) buildDialog();
    const q = new URLSearchParams(location.search);
    let msg = "";
    if (q.has("invite")) { invite = q.get("invite"); }
    if (q.has("reset")) { resetTicket = q.get("reset"); }
    if (q.get("verify") === "ok") msg = _t("acct.e_mail_adresse_bestaetigt_du_kannst_dich");
    if (q.get("verify") === "failed") msg = _t("acct.der_bestaetigungslink_ist_ungueltig_oder");
    if (q.has("invite") || q.has("reset") || q.has("verify")) history.replaceState(null, "", location.pathname + location.hash);
    if (msg) setTimeout(() => ctx.toast(msg), 300);
    return { invite: !!invite, reset: !!resetTicket, verified: q.get("verify") === "ok" };
  }

  async function start() {
    if (resetTicket) {
      try { const r = await call("/api/account/reset?t=" + encodeURIComponent(resetTicket)); show("reset", { name: r.name, twoFA: r.twoFA }); }
      catch (x) { ctx.toast(x.message); show("forgot"); }
      return true;
    }
    if (invite) {
      try { await call("/api/account/invite?t=" + encodeURIComponent(invite)); show("register"); }
      catch { invite = ""; ctx.toast(_t("acct.die_einladung_ist_ungueltig_oder")); show("login"); }
      return true;
    }
    return false;
  }

  window.GoAccount = {
    init, start, crypto: C,
    available: C.available,
    showLogin: () => show("login"),
    showRegister: () => show("register"),
    openSettings, unlock, loadKey, forget,
    hasKey: () => !!myKey,
    encryptFor, decrypt, roomKey, safetyNumber,
    isOpen: () => !!(dlg && dlg.open),
    close: () => { if (dlg && dlg.open) { view = "codes-done"; dlg.close(); } },
  };
})();
