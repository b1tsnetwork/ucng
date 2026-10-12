// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const ROOM_RE = /^[\p{Ll}\p{Lo}\p{N}_-]{1,32}$/u;
  const normRoom = (r) => r.trim().toLowerCase();
  const F = window.GoChatFormat;

  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* egal */ } },
    json(k, def) { try { return JSON.parse(localStorage.getItem(k)) || def; } catch { return def; } },
    setJSON(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* egal */ } },
  };

  let token = store.get("gochat:token") || "";
  let me = { id: "", name: "", team: false, viewer: "", sanction: null, roles: [], perms: {} };
  let settings = { title: "UCNG", allowUserRooms: true, reactions: ["👍", "❤️", "😂", "😮", "😢", "🙏"], editMinutes: 15 };
  let rooms = [];
  let room = "";
  let es = null;
  let lastMsg = null;
  let groupBreak = false; // nach einer privaten Antwort: nächste Nachricht wieder mit Kopf
  let isAdmin = false; // Admin oder Moderator (Löschen-Knopf im Chat)
  const adminToken = store.get("gochat:admin") || "";

  // Einstellungen für Benachrichtigungen (gehören zum Profil, werden mit exportiert).
  const DEFAULT_PREFS = { sound: "mentions", notify: "off" };
  let prefs = Object.assign({}, DEFAULT_PREFS, store.json("gochat:prefs", {}));
  // Zuletzt gelesene Nachricht je Kanal.
  let lastRead = store.json("gochat:read", {});
  let unread = {}; // Kanal → { unread, mentions }
  let unseenHere = 0; // neue Nachrichten im offenen Kanal, während das Fenster im Hintergrund ist
  // Emotes, GIFs und Befehle (vom Server)
  let emotes = new Map(); // Name → {name, url, size, animated}
  let media = { gifs: false, web: "", gifWidth: 300 };
  let roles = { byId: new Map(), users: {} }; // Rollen-Abzeichen (nur Anzeige)
  let commands = { enabled: false, prefix: "!", commands: [] };
  let profiles = {}; // Nutzer-ID → { avatar, status, text }
  let dms = []; // Direktnachrichten: [{ room, with, name, lastId }]
  let dmsEnabled = false;
  let roomInfo = null; // Infos zum offenen Kanal (canWrite, readRoles …)
  let pinned = []; // angepinnte Nachrichten im offenen Kanal
  let thread = null; // offener Thread: { root, msgs }

  // ---------- Hilfsfunktionen ----------

  const AVATAR_COLORS = ["#5848f4", "#0f8f7e", "#d1495b", "#2a7fd4", "#b05cc9", "#c46a12", "#3d8b3d", "#7a5af0", "#8a6a00", "#1f7a99"];

  function colorFor(str) {
    let h = 0;
    for (const c of str.toLowerCase()) h = (h * 31 + c.codePointAt(0)) >>> 0;
    return AVATAR_COLORS[h % AVATAR_COLORS.length];
  }

  function avatar(n, id) {
    const el = document.createElement("span");
    el.className = "avatar";
    el.style.setProperty("--avatar", colorFor(n));
    el.title = n;
    const p = id && profiles[id];
    if (p && p.avatar && media.avatars !== false) {
      const img = document.createElement("img");
      img.src = p.avatar; img.alt = ""; img.loading = "lazy"; img.decoding = "async";
      el.classList.add("has-img");
      el.append(img);
    } else el.textContent = ([...n.trim()][0] || "?").toUpperCase();
    if (p && p.status) el.classList.add("st-" + p.status);
    return el;
  }

  const isDM = (r) => /^dm-/.test(r || "");
  const dmOf = (r) => dms.find((d) => d.room === r);
  const roomLabel = (r) => (isDM(r) ? "@" + ((dmOf(r) || {}).name || _t("app.direktnachricht")) : "#" + r);
  // Blockierte Personen: keine Direktnachrichten; ihre Beiträge in Kanälen sind eingeklappt.
  const isBlocked = (id) => !!id && (me.blocked || []).includes(id);
  const revealed = new Set(); // aufgeklappte Nachrichten blockierter Personen
  const hideBlocked = (m) => !isDM(room) && !m.deleted && isBlocked(m.userId) && !revealed.has(m.id);

  function h(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function iconBtn(cls, label, svg) {
    const b = h("button", cls);
    b.type = "button";
    b.title = label;
    b.setAttribute("aria-label", label);
    b.innerHTML = svg; // nur statische Icons
    return b;
  }

  const timeFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { hour: "2-digit", minute: "2-digit" });
  const dayFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { weekday: "long", day: "numeric", month: "long" });
  const fullFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { dateStyle: "medium", timeStyle: "short" });

  function toast(text) {
    const t = h("div", "toast", text);
    t.setAttribute("role", "status");
    let box = document.querySelector(".toasts");
    if (!box) { box = document.createElement("div"); box.className = "toasts"; box.setAttribute("aria-live", "polite"); document.body.append(box); }
    box.append(t);
    setTimeout(() => t.remove(), 3800);
  }

  async function api(path, opts = {}) {
    const headers = Object.assign({ "Content-Type": "application/json" }, opts.headers || {});
    if (token && !headers.Authorization) headers.Authorization = "Bearer " + token;
    const res = await fetch(path, Object.assign({}, opts, { headers }));
    const data = res.status === 204 ? null : await res.json().catch(() => null);
    if (res.status === 401 && token && path.startsWith("/api/rooms/")) {
      token = ""; store.set("gochat:token", "");
      askName();
    }
    if (!res.ok) throw new Error((data && data.error) || _t("app.das_hat_nicht_geklappt"));
    return data;
  }

  const isOwn = (m) => (m.userId ? m.userId === me.id : !!me.name && m.name === me.name);
  const author = (m) => m.userId || "name:" + m.name;
  const roomPath = (r = room) => `/api/rooms/${encodeURIComponent(r)}`;

  const ICON = {
    thread: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M4 4.5h12a1.5 1.5 0 0 1 1.5 1.5v6a1.5 1.5 0 0 1-1.5 1.5H10l-3.5 3v-3H4A1.5 1.5 0 0 1 2.5 12V6A1.5 1.5 0 0 1 4 4.5z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="M6 8h8M6 10.5h5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
    more: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="5" cy="10" r="1.6" fill="currentColor"/><circle cx="10" cy="10" r="1.6" fill="currentColor"/><circle cx="15" cy="10" r="1.6" fill="currentColor"/></svg>',
    pin: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M12.5 2.5 17.5 7.5l-2.2.6-3.2 3.2.3 3.9-1.6 1.6-3-3-3.6 3.6-.9-.9L6.9 12.9l-3-3 1.6-1.6 3.9.3 3.2-3.2z" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"/></svg>',
    flag: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M5 17V3.5M5 4h9l-2 3.5 2 3.5H5" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    mail: '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="3" y="5" width="14" height="10" rx="2" fill="none" stroke="currentColor" stroke-width="1.7"/><path d="m3.5 6 6.5 5 6.5-5" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/></svg>',
    mute: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M4 8h3l4-3.5v11L7 12H4z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/><path d="m13.5 8 4 4m0-4-4 4" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>',
    trash: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M4 6h12M8 6V4h4v2M6 6l1 10h6l1-10" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    shield: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M10 2.5 4 5v4.5c0 3.7 2.6 6.6 6 8 3.4-1.4 6-4.3 6-8V5z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/><path d="M7.5 10h5" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>',
    reply: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M8 5 3 10l5 5M3.5 10H12a5 5 0 0 1 5 5v1" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    smile: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="7" fill="none" stroke="currentColor" stroke-width="1.7"/><path d="M7 11.5a3.5 3.5 0 0 0 6 0" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/><circle cx="7.6" cy="8" r="1" fill="currentColor"/><circle cx="12.4" cy="8" r="1" fill="currentColor"/></svg>',
    edit: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M4 16h3l8.5-8.5a2.1 2.1 0 0 0-3-3L4 13z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/></svg>',
    x: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M5 5l10 10M15 5 5 15" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>',
    block: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="6.8" fill="none" stroke="currentColor" stroke-width="1.7"/><path d="m5.2 14.8 9.6-9.6" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/></svg>',
  };

  // ---------- Verschlüsselte Direktnachrichten (Modus „hosted“) ----------

  const hostedMode = () => settings.appMode === "hosted";
  const dmE2E = (r = room) => isDM(r) && !!(dmOf(r) || {}).e2e;
  const dmFed = (r = room) => isDM(r) && !!(dmOf(r) || {}).fed;
  const plainCache = new Map(); // Geheimtext → { text } | { error }
  const decrypting = new Set();
  let e2eTimer = 0;

  // e2eState: Zustand einer verschlüsselten Nutzlast. Mit m wird zusätzlich geprüft, ob der
  // Absenderschlüssel zur Person passt (eigene Nachricht → mein Schlüssel, sonst der des Gegenübers).
  function e2eState(payload, m) {
    const key = (m ? (isOwn(m) ? "own" : "peer:" + room) : "any") + "|" + payload;
    const c = plainCache.get(key);
    if (c) return c;
    if (!window.GoAccount) return { error: "broken" };
    if (!decrypting.has(key)) {
      decrypting.add(key);
      const r = room;
      GoAccount.decrypt(payload, m ? isOwn(m) : undefined, r)
        .then((x) => plainCache.set(key, x), (e) => plainCache.set(key, { error: e.message }))
        .finally(() => { decrypting.delete(key); clearTimeout(e2eTimer); e2eTimer = setTimeout(refreshE2E, 20); });
    }
    return null;
  }

  // textOf: Klartext einer Nachricht (bei verschlüsselten erst nach dem Entschlüsseln, sonst null).
  function textOf(m) {
    if (!m.e2e) return m.text;
    const s = e2eState(m.e2e, m);
    return s && s.text != null ? s.text : null;
  }

  function e2eError(err) {
    if (err === "locked") return _t("app.verschluesselt_auf_diesem_geraet_noch");
    if (err === "oldkey") return _t("app.mit_einem_frueheren_schluessel");
    return _t("app.konnte_nicht_entschluesselt_werden");
  }

  function refreshE2E() {
    for (const row of list.querySelectorAll(".msg[data-wait]")) {
      const m = loaded.find((x) => String(x.id) === row.dataset.id);
      if (m) row.replaceWith(buildRow(m, row.classList.contains("cont"), false));
    }
  }

  function onKeyChanged() { // nach Entsperren/Anmelden: unlesbare Nachrichten erneut versuchen
    for (const [k, v] of plainCache) if (v.error === "locked") plainCache.delete(k);
    if (room) renderAll();
  }

  async function unlockKeys() {
    if (window.GoAccount && await GoAccount.unlock()) onKeyChanged();
  }

  // ---------- Nachrichten ----------

  const list = $("messages");

  function atBottom() {
    return list.scrollHeight - list.scrollTop - list.clientHeight < 80;
  }

  function deletedText(m) {
    switch (m.deletedBy) {
      case "automod":
        return isOwn(m)
          ? _t("app.deine_nachricht_wurde_automatisch")
          : _t("app.nachricht_wurde_automatisch_ausgeblendet");
      case "self": return _t("app.nachricht_wurde_geloescht");
      case "bot": return _t("app.nachricht_wurde_von_einem_moderations");
      default: return _t("app.nachricht_wurde_von_einem_moderator");
    }
  }

  // Namen, die im Kanal bekannt sind (für Erwähnungen und Autovervollständigung).
  function knownNames() {
    const names = new Map();
    for (const u of presence.users) names.set(u.name.toLowerCase(), u.name);
    for (let i = loaded.length - 1; i >= 0 && names.size < 200; i--) {
      const m = loaded[i];
      if (!m.bot && !names.has(m.name.toLowerCase())) names.set(m.name.toLowerCase(), m.name);
    }
    return names;
  }

  function fillBubble(bubble, m) {
    bubble.replaceChildren();
    bubble.classList.toggle("deleted", !!m.deleted);
    if (m.deleted) { bubble.textContent = deletedText(m); return; }
    bubble.classList.toggle("blocked-msg", hideBlocked(m));
    if (hideBlocked(m)) {
      bubble.append(h("span", "blocked-note", _t("app.nachricht_einer_blockierten_person")), " ");
      const b = h("button", "text-btn", _t("app.trotzdem_anzeigen"));
      b.type = "button";
      b.addEventListener("click", (e) => { e.stopPropagation(); revealed.add(m.id); fillBubble(bubble, m); });
      bubble.append(b);
      return;
    }
    let text = m.text;
    if (m.e2e) {
      const st = e2eState(m.e2e, m);
      text = "";
      if (!st) { bubble.dataset.wait = "1"; bubble.append(h("span", "e2e-note", _t("app.wird_entschluesselt"))); }
      else if (st.error) {
        bubble.classList.add("e2e-error");
        bubble.append(h("span", "e2e-note", "🔒 " + e2eError(st.error)));
        if (st.error === "locked") {
          const b = h("button", "text-btn", _t("app.entsperren"));
          b.type = "button";
          b.addEventListener("click", unlockKeys);
          bubble.append(" ", b);
        }
      } else {
        text = st.text;
        if (!st.trusted) {
          const w = h("span", "e2e-warn", _t("app.schluessel_passt_nicht"));
          w.title = isOwn(m)
            ? _t("app.diese_nachricht_ist_nicht_mit_deinem")
            : _t("app.der_absenderschluessel_ist_nicht_der");
          bubble.append(w, " ");
        }
      }
    }
    if (text) F.render(bubble, text, { me: me.name, names: new Set(knownNames().keys()), emotes, meAlso: myRoleMentions() });
    if (m.poll) { bubble.classList.add("has-poll"); bubble.append(buildPoll(m)); }
    if (m.media && m.media.kind === "image") {
      const a = h("a", "msg-img-link");
      a.href = m.media.src; a.target = "_blank"; a.rel = "noopener";
      const img = h("img", "msg-img");
      img.src = m.media.src; img.width = m.media.w; img.height = m.media.h;
      img.alt = _t("app.bild"); img.loading = "lazy"; img.decoding = "async";
      a.append(img);
      bubble.classList.add("has-media");
      bubble.classList.toggle("media-only", !m.text);
      bubble.append(a);
    }
    if (m.media && m.media.kind === "gif") {
      const img = h("img", "msg-gif");
      img.src = m.media.src;
      img.width = m.media.w;
      img.height = m.media.h;
      img.alt = m.media.title || "GIF";
      img.title = m.media.title || "";
      img.loading = "lazy";
      img.decoding = "async";
      bubble.classList.add("has-media");
      bubble.classList.toggle("media-only", !m.text);
      bubble.append(img);
      if (m.media.source && m.media.source !== "library") bubble.append(h("span", "gif-source", m.media.source === "beacon" ? (m.media.credit || _t("app.via_beacon")) : ({ giphy: _t("app.via_giphy"), tenor: _t("app.via_tenor") })[m.media.source] || ""));
    }
    if (m.editedAt) {
      const e = h("span", "edited", "bearbeitet");
      e.title = _t("app.edited_at", { date: fullFmt.format(new Date(m.editedAt)) });
      bubble.append(e);
    }
    if (m.fed && m.fed.state && m.fed.state !== "sent") {
      const f = h("span", "fed-state " + m.fed.state, m.fed.state === "failed" ? _t("app.nicht_zugestellt") : _t("app.wird_zugestellt"));
      f.title = m.fed.state === "failed" ? _t("app.die_andere_instanz_hat_die_nachricht") : _t("app.wird_an_die_andere_instanz_uebertragen");
      bubble.append(f);
    }
  }

  function canEdit(m) {
    return isOwn(m) && !m.deleted && !!(m.e2e ? textOf(m) : m.text) && !m.bot && Date.now() - new Date(m.time) < settings.editMinutes * 60000;
  }

  function buildActions(m, inThread) {
    const bar = h("div", "msg-actions");
    if (m.deleted) return bar;
    const canWrite = !!me.name && !me.sanction;
    if (canWrite) {
      if (!dmFed()) {
        const react = iconBtn("act", _t("app.reagieren"), ICON.smile);
        react.addEventListener("click", (e) => { e.stopPropagation(); openReactPicker(react, m); });
        bar.append(react);
      }
      if (!inThread && writable() && !dmFed()) {
        const rep = iconBtn("act", _t("app.antworten"), ICON.reply);
        rep.addEventListener("click", () => startReply(m));
        bar.append(rep);
      }
      if (!m.thread && !inThread && writable() && !dmE2E()) {
        const th = iconBtn("act", _t("app.im_thread_antworten"), ICON.thread);
        th.addEventListener("click", () => openThread(m));
        bar.append(th);
      }
    }
    if (canWrite && canEdit(m)) {
      const ed = iconBtn("act", _t("app.bearbeiten"), ICON.edit);
      ed.addEventListener("click", () => startEdit(m));
      bar.append(ed);
    }
    if (isOwn(m) && me.name) {
      const del = iconBtn("act danger", _t("app.loeschen"), ICON.trash);
      del.addEventListener("click", () => deleteOwn(m));
      bar.append(del);
    } else if (isAdmin && !me.perms.delete) {
      const mod = iconBtn("act danger", _t("app.loeschen_moderation"), ICON.shield);
      mod.addEventListener("click", () => modDelete(m));
      bar.append(mod);
    }
    if (me.name && msgMenuItems(m).length) {
      const more = iconBtn("act", _t("app.mehr"), ICON.more);
      more.addEventListener("click", (e) => { e.stopPropagation(); openMsgMenu(more, m); });
      bar.append(more);
    }
    return bar;
  }

  // Reaktion als Emoji oder (bei ":name:") als Emote-Bild
  function reactionIcon(e, size) {
    const em = /^:([a-z0-9_]{2,32}):$/.exec(e);
    if (em && emotes.has(em[1])) {
      const img = h("img", "emoji emote-react");
      img.src = emotes.get(em[1]).url; img.alt = e; img.width = size; img.height = size; img.draggable = false;
      return img;
    }
    return h("span", em ? "emoji emote-gone" : "emoji", e);
  }

  function buildReactions(m) {
    const box = h("div", "reactions");
    if (m.deleted || !m.reactions || !m.reactions.length) return box;
    for (const r of m.reactions) {
      const mine = r.users.some((u) => u.id === me.id);
      const b = h("button", "reaction" + (mine ? " mine" : ""));
      b.type = "button";
      b.append(reactionIcon(r.emoji, 18), h("span", "count", String(r.users.length)));
      const names = r.users.map((u) => u.name);
      b.title = names.slice(0, 12).join(", ") + (names.length > 12 ? _t("app.und_weitere", { n: names.length - 12 }) : "");
      b.setAttribute("aria-pressed", mine);
      b.setAttribute("aria-label", `${r.emoji} ${r.users.length}: ${b.title}`);
      b.addEventListener("click", () => react(m, r.emoji));
      box.append(b);
    }
    return box;
  }

  function buildReply(m) {
    if (!m.reply) return null;
    const q = h("button", "reply-quote");
    q.type = "button";
    const who = h("b", null, m.reply.name || "");
    const span = h("span");
    if (m.reply.missing) span.textContent = _t("app.nachricht_nicht_mehr_vorhanden");
    else if (m.reply.deleted) span.textContent = _t("app.nachricht_wurde_geloescht");
    else if (m.reply.e2e) {
      const st = e2eState(m.reply.e2e);
      if (st && st.text != null) F.preview(span, st.text, { emotes });
      else { span.textContent = _t("app.verschluesselte_nachricht"); if (!st) q.dataset.wait = "1"; }
    } else F.preview(span, m.reply.text || "", { emotes });
    q.append(who, span);
    if (m.reply.missing || m.reply.deleted) q.classList.add("gone");
    q.title = _t("app.zur_nachricht_springen");
    q.addEventListener("click", () => jumpTo(m.reply.id));
    return q;
  }

  function mentionsMe(m) {
    const text = textOf(m);
    if (m.deleted || !text) return false;
    return F.mentions(text, me.name) || myRoleMentions().some((n) => F.mentions(text, n));
  }

  function buildRow(m, cont, live, inThread) {
    const own = isOwn(m);
    const t = new Date(m.time);
    const row = h("div", "msg" + (own ? " own" : "") + (cont ? " cont" : "") + (live ? " new" : "") + (isPinned(m.id) ? " pinned" : ""));
    row.dataset.id = m.id;
    if (!own && mentionsMe(m)) row.classList.add("mentioned");

    const body = h("div", "body");
    const meta = h("div", "meta");
    meta.append(h("b", null, m.name));
    if (own) meta.append(h("span", "you-tag", "du"));
    if (m.bot) meta.append(h("span", "badge bot-badge", _t("app.bot")));
    if (m.team) meta.append(h("span", "badge team-badge", _t("app.team")));
    if (!m.bot) addRoleBadges(meta, m.userId, meta.firstChild);
    if (isPinned(m.id)) meta.append(h("span", "pin-tag", "angepinnt"));
    const time = h("time", null, timeFmt.format(t));
    time.dateTime = m.time;
    time.title = fullFmt.format(t);
    meta.append(time);
    const bubble = h("div", "bubble");
    bubble.title = timeFmt.format(t);
    fillBubble(bubble, m);
    body.append(meta);
    const rq = buildReply(m);
    if (rq) body.append(rq);
    body.append(bubble, buildReactions(m));
    if (m.threadInfo && !inThread) body.append(threadSummary(m));
    body.append(buildActions(m, inThread));
    if (bubble.dataset.wait || (rq && rq.dataset.wait)) row.dataset.wait = "1";
    const av = avatar(m.name, m.bot ? "" : m.userId);
    if (m.bot) av.classList.add("bot");
    row.append(av, body);
    return row;
  }

  // Alle geladenen Nachrichten des Kanals (älteste zuerst) und ob es noch ältere gibt.
  let loaded = [];
  let hasMore = false;
  let loadingOlder = false;
  let readMark = 0; // beim Öffnen des Kanals zuletzt gelesene Nachricht (für „Neu“-Trenner)

  function addMessage(m, forceScroll, live) {
    if (live) loaded.push(m);
    if (m.thread) return; // Antworten im Thread erscheinen nur im Thread
    const stick = forceScroll || atBottom();
    list.querySelector(".empty")?.remove();

    const t = new Date(m.time);
    if (!lastMsg || new Date(lastMsg.time).toDateString() !== t.toDateString()) {
      list.append(h("div", "day", dayFmt.format(t)));
      lastMsg = null;
    }
    if (!live && readMark && m.id > readMark && !isOwn(m) && !list.querySelector(".new-divider")) {
      const d = h("div", "new-divider");
      d.append(h("span", null, _t("app.neue_nachrichten")));
      list.append(d);
      lastMsg = null;
    }

    const cont = !groupBreak && lastMsg && author(lastMsg) === author(m) && !m.reply && (t - new Date(lastMsg.time)) < 5 * 60 * 1000;
    list.append(buildRow(m, cont, live));
    lastMsg = m;
    groupBreak = false;
    if (stick) list.scrollTop = list.scrollHeight;
  }

  function updateMessage(m) {
    const i = loaded.findIndex((x) => x.id === m.id);
    if (i >= 0) loaded[i] = m;
    const row = list.querySelector(`.msg[data-id="${m.id}"]`);
    if (row) {
      const stick = atBottom();
      const fresh = buildRow(m, row.classList.contains("cont"), false);
      row.replaceWith(fresh);
      if (stick) list.scrollTop = list.scrollHeight;
    }
    threadUpdate(m);
    if (isPinned(m.id)) { pinned = pinned.map((x) => (x.id === m.id ? m : x)); renderPins(); }
    // Antworten auf diese Nachricht: Vorschau anpassen
    for (const x of loaded) {
      if (x.reply && x.reply.id === m.id) {
        x.reply = { id: m.id, name: m.name, deleted: !!m.deleted, text: m.deleted || m.e2e ? "" : m.text, e2e: m.deleted ? "" : m.e2e || "" };
        const r = list.querySelector(`.msg[data-id="${x.id}"]`);
        if (r) r.replaceWith(buildRow(x, r.classList.contains("cont"), false));
      }
    }
    if (editing && editing.id === m.id && m.deleted) cancelCompose();
  }

  function removeMessage(id) {
    const i = loaded.findIndex((x) => x.id === id);
    if (i < 0) return;
    loaded.splice(i, 1);
    for (const x of loaded) if (x.reply && x.reply.id === id) x.reply = { id, missing: true };
    const fromBottom = list.scrollHeight - list.scrollTop;
    renderAll();
    list.scrollTop = list.scrollHeight - fromBottom;
  }

  function showEmpty(title, text) {
    const e = h("div", "empty");
    e.innerHTML = '<svg viewBox="0 0 48 48" aria-hidden="true"><rect x="4" y="8" width="30" height="22" rx="8" fill="currentColor" opacity=".18"/><rect x="14" y="18" width="30" height="22" rx="8" fill="currentColor"/><circle cx="23" cy="29" r="2" fill="#fff"/><circle cx="29" cy="29" r="2" fill="#fff"/><circle cx="35" cy="29" r="2" fill="#fff"/></svg>';
    e.append(h("strong", null, title), h("span", null, text));
    list.replaceChildren(e);
  }

  function showHistory(msgs, more) {
    loaded = msgs.slice();
    hasMore = !!more;
    renderAll();
    const div = list.querySelector(".new-divider");
    if (div && div.offsetTop > list.clientHeight / 2) div.scrollIntoView({ block: "center" });
    else list.scrollTop = list.scrollHeight;
    markRead();
  }

  function renderAll() {
    list.replaceChildren();
    lastMsg = null;
    groupBreak = false;
    if (hasMore) list.append(olderButton());
    const priv = ephemerals.filter((x) => x.room === room);
    if (!loaded.length && !hasMore && !priv.length) { showEmpty(_t("app.noch_ist_es_ruhig_hier"), _t("app.schreib_die_erste_nachricht_in", { room: room })); return; }
    const placed = new Set();
    const putAfter = (id) => priv.forEach((x) => { if (x.afterId === id && !placed.has(x)) { placed.add(x); appendEphemeral(x); } });
    if (!hasMore) putAfter(0);
    loaded.forEach((m) => { addMessage(m, false); putAfter(m.id); });
    const lastId = loaded.length ? loaded[loaded.length - 1].id : 0;
    priv.forEach((x) => { if (!placed.has(x) && x.afterId > lastId) appendEphemeral(x); });
  }

  // ---------- Private Antworten des Befehls-Bots (nur für mich, nicht gespeichert) ----------

  let ephemerals = []; // { key, room, afterId, name, text, command, time, notice }
  function addEphemeral(e) {
    const x = Object.assign({ key: "e" + Date.now() + Math.random().toString(36).slice(2, 6), room, afterId: loaded.length ? loaded[loaded.length - 1].id : 0 }, e);
    ephemerals.push(x);
    if (ephemerals.length > 30) ephemerals.shift();
    list.querySelector(".empty")?.remove();
    appendEphemeral(x, true);
    list.scrollTop = list.scrollHeight;
  }
  function appendEphemeral(x, live) {
    const row = h("div", "msg ephemeral" + (live ? " new" : "") + (x.notice ? " notice" : ""));
    row.dataset.key = x.key;
    const body = h("div", "body");
    const meta = h("div", "meta");
    meta.append(h("b", null, x.name), h("span", "badge bot-badge", _t("app.bot")));
    const t = new Date(x.time);
    const time = h("time", null, timeFmt.format(t));
    time.dateTime = x.time;
    meta.append(time, h("span", "eph-tag", _t("app.nur_fuer_dich_sichtbar")));
    const q = h("div", "reply-quote eph-quote");
    const span = h("span");
    F.preview(span, x.command, { emotes });
    q.append(h("b", null, me.name || _t("app.du")), span);
    const bubble = h("div", "bubble");
    F.render(bubble, x.text, { me: me.name, names: new Set(knownNames().keys()), emotes });
    const hide = h("button", "eph-hide", _t("app.ausblenden"));
    hide.type = "button";
    hide.addEventListener("click", () => {
      ephemerals = ephemerals.filter((y) => y !== x);
      row.remove();
    });
    body.append(meta, q, bubble, hide);
    const av = avatar(x.name);
    av.classList.add("bot");
    row.append(av, body);
    list.append(row);
    groupBreak = true;
  }

  function olderButton() {
    const b = h("button", "load-older", _t("app.aeltere_nachrichten_laden"));
    b.type = "button";
    b.addEventListener("click", loadOlder);
    olderObserver.observe(b);
    return b;
  }
  const olderObserver = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) loadOlder();
  }, { root: list, rootMargin: "300px 0px 0px 0px" });

  async function loadOlder() {
    if (loadingOlder || !hasMore || !loaded.length) return false;
    loadingOlder = true;
    const current = room;
    const btn = list.querySelector(".load-older");
    if (btn) btn.textContent = _t("app.lade");
    try {
      const res = await fetch(`${roomPath()}/messages?before=${loaded[0].id}&limit=100`, { headers: token ? { Authorization: "Bearer " + token } : {} });
      if (!res.ok || room !== current) return false;
      const older = await res.json();
      const fromBottom = list.scrollHeight - list.scrollTop;
      loaded = older.concat(loaded);
      hasMore = res.headers.get("X-Has-More") === "true";
      renderAll();
      list.scrollTop = list.scrollHeight - fromBottom;
      return true;
    } catch {
      if (btn) btn.textContent = _t("app.aeltere_nachrichten_laden");
      return false;
    } finally {
      loadingOlder = false;
    }
  }

  // Zu einer Nachricht springen (lädt bei Bedarf ältere Nachrichten nach).
  async function jumpTo(id) {
    let row = list.querySelector(`.msg[data-id="${id}"]`);
    for (let i = 0; !row && hasMore && i < 60; i++) {
      if (loaded.length && loaded[0].id < id) break;
      if (!(await loadOlder())) break;
      row = list.querySelector(`.msg[data-id="${id}"]`);
    }
    if (!row) { toast(_t("app.diese_nachricht_ist_nicht_mehr_vorhanden")); return; }
    row.scrollIntoView({ block: "center", behavior: "smooth" });
    row.classList.remove("flash");
    void row.offsetWidth;
    row.classList.add("flash");
  }

  // Touch-Geräte: Antippen einer Nachricht zeigt ihre Aktionen.
  list.addEventListener("click", (e) => {
    if (window.matchMedia("(hover: hover)").matches || e.target.closest("a, button")) return;
    const row = e.target.closest(".msg");
    list.querySelectorAll(".msg.tapped").forEach((r) => { if (r !== row) r.classList.remove("tapped"); });
    if (row) row.classList.toggle("tapped");
  });

  // ---------- Aktionen an Nachrichten ----------

  async function react(m, emoji) {
    if (!me.name) { askName(); return; }
    try { await api(`${roomPath()}/messages/${m.id}/react`, { method: "POST", body: JSON.stringify({ emoji }) }); }
    catch (e) { toast(e.message); }
  }

  const picker = $("reactPicker");
  let pickerFor = null;
  function openReactPicker(anchor, m) {
    pickerFor = m;
    const quick = h("div", "react-quick");
    quick.append(...settings.reactions.map((e) => pickBtn(m, e)));
    const extra = [...emotes.values()].filter((em) => !settings.reactions.includes(":" + em.name + ":"));
    picker.classList.remove("expanded");
    picker.replaceChildren(quick);
    if (media.emoteReactions && extra.length) {
      const more = h("button", "react-more");
      more.type = "button";
      more.title = _t("app.weitere_emotes");
      more.setAttribute("aria-label", _t("app.weitere_emotes_als_reaktion"));
      more.setAttribute("aria-expanded", "false");
      more.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M10 4v12M4 10h12" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>';
      more.addEventListener("click", (e) => {
        e.stopPropagation();
        if (picker.classList.contains("expanded")) return;
        picker.classList.add("expanded");
        more.setAttribute("aria-expanded", "true");
        const grid = h("div", "react-emotes");
        const fill = (q) => grid.replaceChildren(...extra.filter((em) => !q || em.name.includes(q)).map((em) => pickBtn(m, ":" + em.name + ":")));
        if (extra.length > 16) {
          const search = h("input", "react-search");
          search.type = "search"; search.placeholder = _t("app.emote_suchen"); search.autocomplete = "off";
          search.addEventListener("input", () => fill(search.value.trim().toLowerCase().replace(/:/g, "")));
          picker.append(search);
          fill("");
          picker.append(grid);
          placePicker(anchor);
          search.focus();
        } else {
          fill("");
          picker.append(grid);
          placePicker(anchor);
          const first = grid.querySelector("button");
          if (first) first.focus();
        }
      });
      quick.append(more);
    }
    picker.hidden = false;
    placePicker(anchor);
    picker.querySelector("button").focus();
  }
  function pickBtn(m, e) {
    const b = h("button");
    b.type = "button";
    b.title = e;
    b.setAttribute("aria-label", _t("app.reaktion") + e);
    b.append(reactionIcon(e, 24));
    b.addEventListener("click", () => { closePicker(); react(m, e); });
    return b;
  }
  let pickerAnchor = null;
  function placePicker(anchor) {
    pickerAnchor = anchor;
    const r = anchor.getBoundingClientRect();
    const w = picker.offsetWidth, ph = picker.offsetHeight;
    picker.style.left = Math.max(8, Math.min(window.innerWidth - w - 8, r.left + r.width / 2 - w / 2)) + "px";
    let top = r.top - ph - 6;
    if (top < 8) top = Math.min(window.innerHeight - ph - 8, r.bottom + 6);
    picker.style.top = Math.max(8, top) + "px";
  }
  function closePicker() { picker.hidden = true; pickerFor = null; }
  document.addEventListener("click", (e) => { if (!picker.hidden && e.target.isConnected && !picker.contains(e.target)) closePicker(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !picker.hidden) closePicker(); });
  list.addEventListener("scroll", () => { if (!picker.hidden) closePicker(); }, { passive: true });

  async function deleteOwn(m) {
    if (!confirm(_t("app.diese_nachricht_loeschen_der_text_wird"))) return;
    try { await api(`${roomPath()}/messages/${m.id}`, { method: "DELETE" }); }
    catch (e) { toast(e.message); }
  }

  async function modDelete(m) {
    if (!confirm(_t("app.nachricht_von_fuer_alle_loeschen", { name: m.name }))) return;
    const res = await fetch(`/api/admin/rooms/${encodeURIComponent(room)}/messages/${m.id}`, {
      method: "DELETE", headers: { Authorization: "Bearer " + adminToken },
    }).catch(() => null);
    if (!res || !res.ok) toast(_t("app.loeschen_hat_nicht_geklappt_bist_du_noch"));
  }

  // ---------- Online-Liste ----------

  let presence = { users: [], guests: 0, online: 0 };

  function renderPresence() {
    const box = $("people");
    const shown = presence.users.slice(0, 4);
    const items = shown.map((u) => avatar(u.name, u.id));
    const rest = presence.users.length - shown.length + presence.guests;
    if (rest > 0) items.push(h("span", "avatar more", "+" + rest));
    box.replaceChildren(...items);
    box.hidden = !items.length;
    box.title = _t("app.wer_ist_online");
    box.setAttribute("aria-label", _t("app.online_liste_anzeigen", { online: presence.online }));
    if (!$("presencePop").hidden) fillPresencePop();
  }

  function fillPresencePop() {
    const pop = $("presencePop");
    const ul = h("ul");
    for (const u of presence.users) {
      const li = h("li");
      const who = h("div", "pwho");
      who.append(h("span", "pname", u.name));
      const p = profiles[u.id];
      if (p && (p.status || p.text)) who.append(h("span", "pstatus", [p.status === "away" ? "abwesend" : p.status === "busy" ? _t("app.beschaeftigt") : "", p.text || ""].filter(Boolean).join(" · ")));
      li.append(avatar(u.name, u.id), who);
      if (u.team) li.append(h("span", "badge team-badge", _t("app.team")));
      addRoleBadges(li, u.id, who.querySelector(".pname"));
      if (u.id === me.id) li.append(h("span", "you", "du"));
      else if (dmsEnabled && me.name) {
        const b = iconBtn("pdm", _t("app.direktnachricht_an", { name: u.name }), ICON.mail);
        b.addEventListener("click", (e) => { e.stopPropagation(); $("presencePop").hidden = true; startDM(u.id); });
        li.append(b);
      }
      ul.append(li);
    }
    const head = h("div", "pop-head", _t("app.online_in", { online: presence.online, roomLabel: roomLabel(room) }));
    const parts = [head, ul];
    if (presence.guests) parts.push(h("div", "pop-foot", presence.guests === 1 ? _t("app.und_1_gast_ohne_namen") : _t("app.und_gaeste_ohne_namen", { guests: presence.guests })));
    if (!presence.users.length && !presence.guests) parts.push(h("div", "pop-foot", _t("app.gerade_ist_niemand_hier")));
    pop.replaceChildren(...parts);
  }

  $("people").addEventListener("click", (e) => {
    e.stopPropagation();
    const pop = $("presencePop");
    pop.hidden = !pop.hidden;
    if (!pop.hidden) fillPresencePop();
  });
  document.addEventListener("click", (e) => { if (!$("presencePop").contains(e.target)) $("presencePop").hidden = true; });

  // ---------- Verbindung ----------

  function setStatus(state, text) {
    $("statusDot").className = "dot " + state;
    $("roomStatus").textContent = text;
  }

  function setDescription(d) {
    $("roomDesc").textContent = d || "";
    $("roomDesc").title = d || "";
  }

  function setRoomInfo(info) {
    if (!info) return;
    setDescription(info.description || (isDM(room) ? dmDescription(room) : ""));
    if (isDM(room)) addSafetyButton(room);
    const notes = [];
    if (info.slowMode) notes.push(_t("app.langsam_modus_s", { slowMode: info.slowMode }));
    if (info.links === "known") notes.push(_t("app.links_erst_nach_24_h"));
    if (info.links === "team") notes.push(_t("app.links_nur_vom_team"));
    $("roomRules").textContent = notes.join(" · ");
    $("roomRules").hidden = !notes.length;
  }

  const loginRequired = () => hostedMode() && settings.hosted && settings.hosted.guests === "none" && !me.member;

  function connect() {
    if (es) es.close();
    es = null;
    if (!room) return;
    if (loginRequired()) {
      setStatus("err", _t("app.anmeldung_noetig"));
      showEmpty(_t("app.bitte_melde_dich_an"), _t("app.dieser_chat_ist_nur_fuer_angemeldete"));
      return;
    }
    setStatus("", _t("app.verbinde"));
    const current = room;
    const q = me.viewer ? "?viewer=" + encodeURIComponent(me.viewer) : "";
    es = new EventSource(`${roomPath()}/events${q}`);
    es.onmessage = (e) => {
      const ev = JSON.parse(e.data);
      switch (ev.type) {
        case "history":
          pinned = ev.pinned || [];
          roomInfo = ev.roomInfo || null;
          showHistory(ev.messages || [], ev.more);
          setRoomInfo(ev.roomInfo || { description: ev.description });
          renderPins();
          updateComposer();
          if (thread) reloadThread();
          break;
        case "pins": pinned = ev.messages || []; renderPins(); renderAll(); break;
        case "profiles": loadProfiles().then(() => { renderAll(); renderPresence(); if (thread) renderThread(); }); break;
        case "dm": if (ev.user === me.id) loadDMs(); break;
        case "layout": loadGroups().then(loadRooms); break;
        case "message": onLiveMessage(ev.message); break;
        case "update": updateMessage(ev.message); break;
        case "removed": removeMessage(ev.id); break;
        case "presence":
          presence = { users: ev.users || [], guests: ev.guests || 0, online: ev.online };
          renderPresence();
          break;
        case "room":
          if (ev.roomInfo) { roomInfo = Object.assign({}, roomInfo, ev.roomInfo, { canWrite: roomInfo ? roomInfo.canWrite : true }); }
          setRoomInfo(ev.roomInfo || { description: ev.description });
          loadRooms().then(updateComposer);
          break;
        case "settings": applySettings(ev.settings); break;
        case "media": loadMedia().then(() => renderAll()); break;
        case "roles":
          Promise.all([loadRoles(), restoreSession()]).then(() => { loadRooms().then(updateComposer); renderAll(); if (!$("presencePop").hidden) fillPresencePop(); });
          break;
        case "user_renamed":
        case "user_updated":
          if (ev.user === me.id) restoreSession(); // z. B. Team-Abzeichen erhalten
          connect(); return; // Verlauf mit neuen Namen/Abzeichen neu laden
        case "sanction":
          if (ev.user === me.id) restoreSession().then(() => { renderAll(); updateComposer(); });
          break;
        case "room_renamed":
          es.close(); es = null;
          toast(_t("app.dieser_kanal_heisst_jetzt", { room: ev.room }));
          lastRead[ev.room] = lastRead[current];
          loadRooms().then(() => switchRoom(ev.room));
          return;
        case "room_deleted":
          es.close(); es = null;
          toast(_t("app.room_deleted", { room: current }));
          loadRooms().then(() => switchRoom(firstRoom()));
          return;
      }
      setStatus("ok", `${ev.online} online`);
    };
    es.onerror = async () => {
      setStatus("err", _t("app.verbindung_unterbrochen_neuer_versuch"));
      const res = await fetch(`/api/rooms/${encodeURIComponent(current)}/messages?limit=1`, { headers: token ? { Authorization: "Bearer " + token } : {} }).catch(() => null);
      if (res && res.status === 404 && room === current) {
        if (es) { es.close(); es = null; }
        await loadRooms();
        const next = firstRoom();
        if (next && next !== current) switchRoom(next);
        else noRoom();
      }
    };
  }

  function onLiveMessage(m) {
    const own = isOwn(m);
    if (own) list.querySelector(".new-divider")?.remove();
    addMessage(m, own, true);
    if (m.thread) { threadLive(m); clearTimeout(onLiveMessage.t); onLiveMessage.t = setTimeout(loadRooms, 800); } // Thread-Liste in der Seitenleiste
    if (own) return;
    if (!isDM(room) && isBlocked(m.userId)) return; // kein Hinweis, kein Ton
    const visible = !document.hidden && atBottom();
    if (visible) markRead();
    else {
      if (document.hidden) unseenHere++;
      updateTitle();
    }
    const mention = mentionsMe(m) || isDM(room);
    if (document.hidden || !visible) alertUser(mention, roomLabel(room), m);
  }

  function noRoom() {
    room = "";
    if (es) { es.close(); es = null; }
    $("roomTitle").textContent = "";
    setDescription("");
    setStatus("err", _t("app.kein_kanal_verfuegbar"));
    showEmpty(_t("app.gerade_gibt_es_keinen_kanal"), _t("app.sobald_ein_kanal_angelegt_ist_kannst_du"));
    renderRoomList();
  }

  function firstRoom() {
    if (rooms.some((r) => r.name === "lobby")) return "lobby";
    return rooms.length ? rooms[0].name : "";
  }

  function switchRoom(r) {
    r = normRoom(r || "");
    if (!r) { noRoom(); return; }
    if (!ROOM_RE.test(r)) { toast(_t("app.ungueltiger_kanalname_erlaubt_sind")); return; }
    if (!settings.allowUserRooms && !rooms.some((x) => x.name === r) && !(isDM(r) && dmOf(r))) {
      toast(_t("app.x_room_missing", { room: r }));
      r = firstRoom();
      if (!r) { noRoom(); return; }
    }
    if (room !== r) { cancelCompose(); closeThread(); pinned = []; renderPins(); roomInfo = rooms.find((x) => x.name === r) || null; }
    room = r;
    readMark = lastRead[r] || 0;
    presence = { users: [], guests: 0, online: 0 };
    renderPresence();
    closeSearch();
    $("roomTitle").textContent = isDM(r) ? ((dmOf(r) || {}).name || _t("app.direktnachricht")) : r;
    $("roomHash").textContent = isDM(r) ? "@" : "#";
    $("dmMenuBtn").hidden = !isDM(r) || !me.name;
    if (isDM(r) && me.name && !dmOf(r)) loadDMs(); // neu erschienene Unterhaltung
    setRoomInfo(rooms.find((x) => x.name === r) || (isDM(r) ? { description: dmDescription(r) } : null));
    if (decodeURIComponent(location.hash.slice(1)) !== r) history.replaceState(null, "", "#" + encodeURIComponent(r));
    $("sidebar").classList.remove("open");
    connect();
    loadCommands();
    renderRoomList();
    updateTitle();
    if (!rooms.some((x) => x.name === r) && !isDM(r)) setTimeout(loadRooms, 500);
    $("input").placeholder = _t("app.nachricht_an", { roomLabel: roomLabel(r) });
    updateComposer();
    if (window.matchMedia("(hover: hover)").matches) $("input").focus();
  }

  function dmDescription(r) {
    const d = dmOf(r) || {};
    if (d.fed) return _t("app.andere_instanz_ende_zu_ende");
    if (d.e2e) return _t("app.ende_zu_ende_verschluesselt_nur_ihr");
    return _t("app.direktnachricht_nur_ihr_beide_seht_diese");
  }

  function addSafetyButton(r) {
    if (!dmE2E(r) || !window.GoAccount) return;
    const b = h("button", "text-btn safety-btn", _t("app.sicherheitsnummer"));
    b.type = "button";
    b.addEventListener("click", async () => {
      if (!GoAccount.hasKey()) { await unlockKeys(); if (!GoAccount.hasKey()) return; }
      try {
        const n = await GoAccount.safetyNumber(r);
        if (!n) { toast(_t("app.die_andere_person_hat_noch_keinen")); return; }
        $("safetyName").textContent = (dmOf(r) || {}).name || "";
        $("safetyNumber").textContent = n;
        $("safetyDialog").showModal();
      } catch (e) { toast(e.message); }
    });
    $("roomDesc").append(" ", b);
  }

  // ---------- Gelesen / Ungelesen ----------

  function markRead() {
    if (!room || !loaded.length || document.hidden) return;
    const last = loaded[loaded.length - 1].id;
    if ((lastRead[room] || 0) < last) {
      lastRead[room] = last;
      store.setJSON("gochat:read", lastRead);
    }
    unseenHere = 0;
    if (unread[room]) delete unread[room];
    renderRoomList();
    updateTitle();
  }

  list.addEventListener("scroll", () => { if (atBottom()) markRead(); }, { passive: true });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && atBottom()) markRead();
    updateTitle();
  });

  function updateTitle() {
    let n = unseenHere;
    for (const [r, u] of Object.entries(unread)) if (r !== room) n += u.unread;
    const base = room ? `${roomLabel(room)} – ${settings.title}` : settings.title;
    document.title = (n ? `(${n > 99 ? "99+" : n}) ` : "") + base;
  }

  async function refreshUnread() {
    if (!rooms.length) return;
    const since = {};
    let changed = false;
    for (const r of rooms.concat(dms.map((d) => ({ name: d.room, lastId: d.lastId, dm: true })))) {
      if (r.name === room) continue;
      if (lastRead[r.name] == null) { lastRead[r.name] = r.dm ? 0 : r.lastId || 0; changed = true; }
      if ((r.lastId || 0) > lastRead[r.name]) since[r.name] = lastRead[r.name];
    }
    if (changed) store.setJSON("gochat:read", lastRead);
    const next = {};
    if (Object.keys(since).length) {
      try {
        const res = await fetch("/api/unread", {
          method: "POST",
          headers: Object.assign({ "Content-Type": "application/json" }, token ? { Authorization: "Bearer " + token } : {}),
          body: JSON.stringify({ since }),
        });
        if (res.ok) Object.assign(next, await res.json());
      } catch { return; }
    }
    for (const [r, u] of Object.entries(next)) {
      const before = unread[r] || { unread: 0, mentions: 0 };
      if (u.mentions > before.mentions) alertUser(true, roomLabel(r), null, r);
      else if (u.unread > before.unread && prefs.notify === "all" && document.hidden) alertUser(false, roomLabel(r), null, r);
    }
    unread = {};
    for (const [r, u] of Object.entries(next)) if (u.unread > 0) unread[r] = u;
    renderRoomList();
    updateTitle();
  }

  // ---------- Benachrichtigungen ----------

  let audioCtx = null;
  function ping(strong) {
    try {
      audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
      const t = audioCtx.currentTime;
      [0, strong ? 0.12 : null].forEach((d, i) => {
        if (d === null) return;
        const o = audioCtx.createOscillator(), g = audioCtx.createGain();
        o.type = "sine";
        o.frequency.value = i ? 1320 : 880;
        g.gain.setValueAtTime(0.0001, t + d);
        g.gain.exponentialRampToValueAtTime(0.18, t + d + 0.015);
        g.gain.exponentialRampToValueAtTime(0.0001, t + d + 0.22);
        o.connect(g).connect(audioCtx.destination);
        o.start(t + d); o.stop(t + d + 0.25);
      });
    } catch { /* kein Ton möglich */ }
  }

  function alertUser(mention, where, m, target) {
    target = target || room;
    const want = (p) => p === "all" || (p === "mentions" && mention);
    if (want(prefs.sound)) ping(mention);
    if (want(prefs.notify) && document.hidden && "Notification" in window && Notification.permission === "granted") {
      const title = isDM(target) ? _t("app.direktnachricht_von", { name: m ? m.name : where.slice(1) })
        : mention ? _t("app.hat_dich_in_erwaehnt", { name: m ? m.name : _t("app.someone"), where: where }) : (m ? _t("app.name_in_where", { name: m.name, where: where }) : _t("app.neue_nachrichten_in", { where: where }));
      const body = m && !m.deleted ? (F.plain((m.e2e ? textOf(m) || _t("app.verschluesselte_nachricht") : m.text) || "") || (m.media ? (m.media.kind === "image" ? _t("app.bild") : "GIF") : m.poll ? _t("app.umfrage") + m.poll.question : "")).slice(0, 140) : "";
      try {
        const n = new Notification(title, { body, tag: "gochat-" + where, renotify: true });
        n.onclick = () => { window.focus(); switchRoom(target); n.close(); };
      } catch { /* z. B. mobile Browser ohne Unterstützung */ }
    }
  }

  // ---------- Kanalliste ----------

  async function loadRooms() {
    try {
      const res = await fetch("/api/rooms", { headers: token ? { Authorization: "Bearer " + token } : {} });
      if (!res.ok) { rooms = []; renderRoomList(); return; }
      rooms = await res.json();
    } catch { return; }
    const cur = rooms.find((x) => x.name === room);
    if (cur && roomInfo) roomInfo.canWrite = cur.canWrite;
    renderRoomList();
    refreshUnread();
  }

  // ---------- Kanalliste: Gruppen, Angepinnt, Sortierung, aktive Threads ----------

  let groups = []; // [{id, name}] in Reihenfolge des Teams
  let collapsed = new Set(store.json("gochat:collapsed", []));
  let sortMode = store.get("gochat:sort") || "team"; // team | az | active
  const SORT_LABEL = { team: _t("app.reihenfolge_des_teams"), az: _t("app.alphabetisch"), active: _t("app.zuletzt_aktiv") };
  let guestPins = store.json("gochat:pins", []);
  const myPins = () => (me.name ? me.pinned || [] : guestPins);

  async function loadGroups() {
    try { const res = await fetch("/api/groups"); if (res.ok) groups = await res.json() || []; } catch { /* ohne */ }
    renderRoomList();
  }

  function sortList(list) {
    const l = list.slice();
    if (sortMode === "az") l.sort((a, b) => a.name.localeCompare(b.name, GoChatI18n.locale));
    else if (sortMode === "active") l.sort((a, b) => new Date(b.lastAt || 0) - new Date(a.lastAt || 0));
    return l; // „team“: Reihenfolge vom Server
  }

  async function togglePin(name) {
    const list = myPins().slice();
    const i = list.indexOf(name);
    if (i >= 0) list.splice(i, 1); else list.push(name);
    if (me.name) {
      try { const r = await api("/api/profile/pins", { method: "PUT", body: JSON.stringify({ rooms: list }) }); me.pinned = r.rooms; }
      catch (e) { toast(e.message); return; }
    } else { guestPins = list; store.setJSON("gochat:pins", list); }
    toast(i >= 0 ? _t("app.room_unpinned", { room: name }) : _t("app.room_pinned", { room: name }));
    renderRoomList();
  }

  function unreadOf(name) { return name !== room && unread[name]; }

  function roomItem(r, opts = {}) {
    const li = h("li", "room-item");
    const a = h("a");
    a.href = "#" + encodeURIComponent(r.name);
    if (r.name === room && !opts.dup) a.setAttribute("aria-current", "page");
    if (r.description) a.title = r.description;
    a.append(h("span", "hash", "#"), h("span", "room-name", r.name));
    if (r.readRoles && r.readRoles.length) a.append(Object.assign(h("span", "room-flag", "🔒"), { title: _t("app.privater_kanal") }));
    else if (r.writeRoles && r.writeRoles.length) a.append(Object.assign(h("span", "room-flag", "📣"), { title: r.canWrite ? _t("app.hier_duerfen_nur_bestimmte_rollen") : _t("app.nur_lesen") }));
    const u = unreadOf(r.name);
    if (u) {
      a.classList.add("unread");
      const badge = h("span", "unread-badge" + (u.mentions ? " mention" : ""), u.mentions ? "@" + (u.mentions > 1 ? u.mentions : "") : (u.unread > 99 ? "99+" : String(u.unread)));
      badge.title = u.mentions ? _t("app.erwaehnung_en_ungelesen", { mentions: u.mentions, unread: u.unread }) : `${u.unread} ungelesen`;
      a.append(badge);
    } else if (r.online) {
      const c = h("span", "online", String(r.online));
      c.title = `${r.online} online`;
      a.append(c);
    }
    const pinned = myPins().includes(r.name);
    const pb = iconBtn("pin-btn" + (pinned ? " on" : ""), pinned ? _t("app.room_unpin", { room: r.name }) : _t("app.room_pin", { room: r.name }), ICON.pin);
    pb.title = pinned ? _t("app.loesen") : _t("app.oben_anpinnen");
    pb.addEventListener("click", (e) => { e.preventDefault(); e.stopPropagation(); togglePin(r.name); });
    const row = h("div", "room-row");
    row.append(a, pb);
    li.append(row);
    // aktive Threads unter dem Kanal
    const th = (r.threads || []).slice(0, r.name === room ? 5 : 3);
    if (th.length && !opts.dup) {
      const ul = h("ul", "room-threads");
      for (const t of th) {
        const tl = h("li");
        const b = h("button", "thread-link");
        b.type = "button";
        b.append(h("span", "thread-title", t.title || _t("app.thread")), h("span", "thread-count", String(t.count)));
        b.title = _t("app.thread_antwort", { title: t.title || "", count: t.count });
        if (thread && thread.root && thread.root.id === t.id && r.name === room) b.setAttribute("aria-current", "true");
        b.addEventListener("click", () => {
          if (r.name !== room) switchRoom(r.name);
          openThreadById(t.id);
          $("sidebar").classList.remove("open");
        });
        tl.append(b);
        ul.append(tl);
      }
      li.append(ul);
    }
    return li;
  }

  function renderRoomList() {
    const shown = rooms.slice();
    if (room && !isDM(room) && !shown.some((r) => r.name === room)) shown.unshift({ name: room, online: 0 });
    const ul = $("roomList");
    const items = [];
    // Angepinnt (eigene Auswahl)
    const pins = myPins().map((n) => shown.find((r) => r.name === n)).filter(Boolean);
    if (pins.length) {
      items.push(Object.assign(h("li", "group-label", _t("app.angepinnt")), { role: "presentation" }));
      for (const r of pins) items.push(roomItem(r, { dup: false }));
      items.push(Object.assign(h("li", "group-sep"), { role: "presentation" }));
    }
    const pinnedSet = new Set(pins.map((r) => r.name));
    const known = new Set(groups.map((g) => g.id));
    const rest = shown.filter((r) => !pinnedSet.has(r.name));
    // ohne Gruppe
    for (const r of sortList(rest.filter((r) => !r.group || !known.has(r.group)))) items.push(roomItem(r));
    // Gruppen
    for (const g of groups) {
      const list = sortList(rest.filter((r) => r.group === g.id));
      if (!list.length) continue;
      const closed = collapsed.has(g.id);
      const li = h("li", "room-group" + (closed ? " closed" : ""));
      const head = h("button", "group-head");
      head.type = "button";
      head.setAttribute("aria-expanded", String(!closed));
      head.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="m7 5 5 5-5 5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
      head.append(h("span", "group-name", g.name));
      if (closed) {
        let n = 0, m = 0;
        for (const r of list) { const u = unreadOf(r.name); if (u) { n += u.unread; m += u.mentions; } }
        if (n) head.append(h("span", "unread-badge" + (m ? " mention" : ""), m ? "@" : (n > 99 ? "99+" : String(n))));
      }
      head.addEventListener("click", () => {
        if (collapsed.has(g.id)) collapsed.delete(g.id); else collapsed.add(g.id);
        store.setJSON("gochat:collapsed", [...collapsed]);
        renderRoomList();
      });
      li.append(head);
      const sub = h("ul", "rooms group-rooms");
      // zugeklappt: nur der geöffnete Kanal bleibt sichtbar
      for (const r of list) if (!closed || r.name === room) sub.append(roomItem(r));
      li.append(sub);
      items.push(li);
    }
    ul.replaceChildren(...items);
    $("sortBtn").title = _t("app.sortierung") + SORT_LABEL[sortMode];
    $("sortBtn").setAttribute("aria-label", _t("app.sortierung") + SORT_LABEL[sortMode]);
    renderDMList();
  }

  $("sortBtn").addEventListener("click", () => {
    const modes = ["team", "az", "active"];
    sortMode = modes[(modes.indexOf(sortMode) + 1) % modes.length];
    store.set("gochat:sort", sortMode);
    renderRoomList();
    toast(_t("app.kanaele") + SORT_LABEL[sortMode]);
  });

  // ---------- Einstellungen ----------

  function applySettings(s) {
    if (!s) return;
    const themeChanged = s.accent !== settings.accent || s.sidebar !== settings.sidebar || s.mode !== settings.mode;
    if (themeChanged && settings.accent) $("themeCSS").href = "/theme.css?v=" + Date.now();
    const roomsWereAllowed = settings.allowUserRooms;
    settings = Object.assign({}, settings, s);
    $("brandTitle").textContent = s.title;
    updateTitle();
    $("joinForm").hidden = !s.allowUserRooms;
    if (roomsWereAllowed && !s.allowUserRooms) loadRooms();
    const links = [];
    if (s.privacyUrl) links.push([_t("app.datenschutz"), s.privacyUrl]);
    if (s.imprintUrl) links.push([_t("app.impressum"), s.imprintUrl]);
    // Quellcode dieser Fassung (AGPL)
    if (s.about && s.about.source) links.push([_t("app.quellcode"), s.about.source]);
    $("legalLinks").replaceChildren(...links.map(([t, u]) => {
      const a = h("a", null, t);
      a.href = u; a.target = "_blank"; a.rel = "noopener";
      return a;
    }));
    $("legalLinks").hidden = !links.length;
  }

  // ---------- Eingabe: Senden, Antworten, Bearbeiten ----------

  const input = $("input");
  let replyTo = null;
  let editing = null;

  function autosize() {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 180) + "px";
  }

  function showComposeBar(kind, title, text) {
    const bar = $("composeBar");
    bar.className = "compose-bar " + kind;
    $("composeTitle").textContent = title;
    const box = $("composeText");
    box.replaceChildren();
    F.preview(box, text, { emotes, max: 140 });
    bar.hidden = false;
  }

  function startReply(m) {
    editing = null;
    replyTo = m;
    showComposeBar("reply", _t("app.antwort_an", { name: m.name }), textOf(m) || (m.e2e ? _t("app.verschluesselte_nachricht") : "") || (m.media ? "GIF" + (m.media.title ? ": " + m.media.title : "") : ""));
    input.focus();
  }

  function startEdit(m) {
    replyTo = null;
    editing = m;
    showComposeBar("edit", _t("app.nachricht_bearbeiten"), _t("app.enter_zum_speichern_esc_zum_abbrechen"));
    input.value = textOf(m) || "";
    autosize();
    input.focus();
    input.setSelectionRange(input.value.length, input.value.length);
  }

  function cancelCompose() {
    if (editing) { input.value = ""; autosize(); }
    replyTo = null;
    editing = null;
    $("composeBar").hidden = true;
  }
  $("composeCancel").addEventListener("click", () => { cancelCompose(); input.focus(); });

  function updateComposer() {
    const s = me.sanction;
    const note = $("sanctionNote");
    if (s) {
      let txt = s.kind === "mute" ? _t("app.du_bist_stummgeschaltet") : _t("app.du_bist_vom_chat_ausgeschlossen");
      if (s.until) txt += " " + _t("app.x_until", { time: fullFmt.format(new Date(s.until)) });
      txt += ".";
      if (s.reason) txt += _t("app.grund") + s.reason;
      note.textContent = txt;
      cancelCompose();
    }
    note.hidden = !s;
    const ro = !s && !writable();
    const dm = isDM(room) ? dmOf(room) : null;
    const ron = $("readonlyNote");
    ron.textContent = !ro ? "" : dm && dm.blocked
      ? (dm.blockedByMe ? _t("app.du_hast_name_blockiert", { name: dm.name }) : _t("app.in_dieser_unterhaltung_kannst_du_nicht"))
      : _t("app.in_diesem_kanal_duerfen_nur_bestimmte");
    if (ro && dm && dm.blockedByMe) {
      const b = h("button", "text-btn", _t("app.blockierung_aufheben"));
      b.type = "button";
      b.addEventListener("click", () => setBlocked(dm.with, dm.name, false));
      ron.append(" ", b);
    }
    ron.hidden = !ro;
    input.disabled = !!s || ro;
    $("composer").classList.toggle("blocked", !!s || ro);
    $("composer").classList.toggle("e2e", dmE2E());
    if (hostedMode() && !me.name && room) input.placeholder = _t("app.zum_schreiben_bitte_anmelden");
    else if (room && input.placeholder === _t("app.zum_schreiben_bitte_anmelden")) input.placeholder = _t("app.nachricht_an", { roomLabel: roomLabel(room) });
    updateUploadBtn();
    if (s && s.until) {
      clearTimeout(updateComposer.timer);
      updateComposer.timer = setTimeout(() => restoreSession().then(() => { updateComposer(); renderAll(); }),
        Math.max(1000, Math.min(new Date(s.until) - Date.now() + 1000, 2 ** 31 - 1)));
    }
  }

  async function send(e) {
    e.preventDefault();
    if (mentionState) { pickMention(); return; }
    const text = input.value.trim();
    if (!text || !room) return;
    if (!me.name) { askName(); return; }
    $("sendBtn").disabled = true;
    try {
      if (editing) {
        if (text !== textOf(editing)) {
          const body = editing.e2e ? { e2e: await encryptHere(text) } : { text };
          await api(`${roomPath()}/messages/${editing.id}`, { method: "PATCH", body: JSON.stringify(body) });
        }
        input.value = "";
        cancelCompose();
      } else {
        const body = dmE2E() ? { e2e: await encryptHere(text) } : { text };
        if (replyTo && !dmFed()) body.replyTo = replyTo.id;
        const res = await api(`${roomPath()}/messages`, { method: "POST", body: JSON.stringify(body) });
        if (res && res.ephemeral) addEphemeral(res.ephemeral);
        input.value = "";
        cancelCompose();
        list.scrollTop = list.scrollHeight;
      }
      autosize();
    } catch (err) {
      if (err.message === "locked") { toast(_t("app.bitte_zuerst_entsperren_um")); unlockKeys(); return; }
      toast(err.message === "Failed to fetch" ? _t("app.server_nicht_erreichbar_die_nachricht") : err.message);
      if (/stummgeschaltet|ausgeschlossen/.test(err.message)) restoreSession().then(updateComposer);
    } finally {
      $("sendBtn").disabled = false;
      if (!input.disabled) input.focus();
    }
  }

  async function encryptHere(text) {
    if (!window.GoAccount) throw new Error(_t("app.verschluesselung_nicht_verfuegbar"));
    if (!GoAccount.hasKey()) throw new Error("locked");
    const k = await GoAccount.roomKey(room);
    if (k.changed) toast(_t("app.achtung_der_schluessel_von_hat_sich", { name: (dmOf(room) || {}).name || _t("app.your_contact") }));
    return GoAccount.encryptFor(room, text);
  }

  // --- Vervollständigen: @Erwähnungen, :Emotes:, !Befehle ---
  let mentionState = null; // { start, end, items: [{label, insert, icon, hint}], index }
  const mentionBox = $("mentionBox");

  function suggestions(before, pos) {
    // @Name
    let m = /(^|\s)@([\p{L}\p{N}_.\- ]{0,32})$/u.exec(before);
    if (m && !m[2].includes("  ")) {
      const q = m[2].toLowerCase();
      const items = [...knownNames().values()]
        .filter((n) => n.toLowerCase() !== me.name.toLowerCase() && n.toLowerCase().startsWith(q))
        .sort((a, b) => a.localeCompare(b, GoChatI18n.locale)).slice(0, 6)
        .map((n) => ({ label: n, insert: "@" + n + " ", icon: avatar(n) }));
      // @Rolle (nur erwähnbare Rollen)
      for (const r of roles.byId.values()) {
        if (r.mentionable && r.name.toLowerCase().startsWith(q) && items.length < 8) {
          const dot = h("span", "sugg-role");
          dot.style.setProperty("--role", r.color);
          items.push({ label: "@" + r.name, insert: "@" + r.name + " ", hint: _t("app.rolle_alle_mit_dieser_rolle"), icon: dot });
        }
      }
      if (items.length) return { start: pos - m[2].length - 1, items };
    }
    // :emote
    m = /(^|\s):([a-z0-9_]{1,32})$/i.exec(before);
    if (m && emotes.size) {
      const q = m[2].toLowerCase();
      const items = [...emotes.values()]
        .filter((e) => e.name.includes(q))
        .sort((a, b) => (a.name.startsWith(q) ? 0 : 1) - (b.name.startsWith(q) ? 0 : 1) || a.name.localeCompare(b.name))
        .slice(0, 8)
        .map((e) => ({ label: ":" + e.name + ":", insert: ":" + e.name + ": ", icon: emoteIcon(e) }));
      if (items.length) return { start: pos - m[2].length - 1, items };
    }
    // !Befehl (nur am Anfang)
    if (commands.enabled && before.startsWith(commands.prefix) && !/\s/.test(before)) {
      const q = before.slice(commands.prefix.length).toLowerCase();
      const items = commands.commands
        .filter((c) => (!c.teamOnly || me.team) && c.trigger.startsWith(q))
        .slice(0, 8)
        .map((c) => ({ label: commands.prefix + c.trigger, insert: commands.prefix + c.trigger + " ", hint: c.description, icon: cmdIcon() }));
      if (items.length) return { start: 0, items };
    }
    return null;
  }

  function emoteIcon(e) {
    const img = h("img", "sugg-emote");
    img.src = e.url; img.alt = ""; img.width = 24; img.height = 24;
    return img;
  }
  function cmdIcon() {
    const s = h("span", "sugg-cmd", commands.prefix);
    s.setAttribute("aria-hidden", "true");
    return s;
  }

  function updateMentions() {
    const pos = input.selectionStart;
    const sg = suggestions(input.value.slice(0, pos), pos);
    if (!sg) { closeMentions(); return; }
    mentionState = { start: sg.start, end: pos, items: sg.items, index: Math.min(mentionState ? mentionState.index : 0, sg.items.length - 1) };
    renderMentions();
  }

  function renderMentions() {
    mentionBox.replaceChildren(...mentionState.items.map((it, i) => {
      const b = h("button", i === mentionState.index ? "active" : "");
      b.type = "button";
      b.setAttribute("role", "option");
      b.setAttribute("aria-selected", i === mentionState.index);
      b.append(it.icon, h("span", "sugg-label", it.label));
      if (it.hint) b.append(h("span", "sugg-hint", it.hint));
      b.addEventListener("mousedown", (e) => { e.preventDefault(); mentionState.index = i; pickMention(); });
      return b;
    }));
    mentionBox.hidden = false;
  }

  function closeMentions() { mentionState = null; mentionBox.hidden = true; }

  function pickMention() {
    if (!mentionState) return;
    const it = mentionState.items[mentionState.index];
    const v = input.value;
    input.value = v.slice(0, mentionState.start) + it.insert + v.slice(mentionState.end);
    const p = mentionState.start + it.insert.length;
    input.setSelectionRange(p, p);
    closeMentions();
    autosize();
    input.focus();
  }

  input.addEventListener("input", () => { autosize(); updateMentions(); });
  input.addEventListener("blur", () => setTimeout(closeMentions, 150));
  input.addEventListener("keydown", (e) => {
    if (mentionState) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const n = mentionState.items.length;
        mentionState.index = (mentionState.index + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
        renderMentions();
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        // schon vollständig getippt (z. B. "!hilfe")? Dann sendet Enter direkt
        const typed = input.value.slice(mentionState.start, mentionState.end).toLowerCase();
        const full = mentionState.items[mentionState.index].insert.trim().toLowerCase();
        if (e.key === "Enter" && typed === full) closeMentions();
        else { e.preventDefault(); pickMention(); return; }
      }
      if (e.key === "Escape") { e.preventDefault(); closeMentions(); return; }
    }
    if (e.key === "Escape" && !$("composeBar").hidden) { e.preventDefault(); cancelCompose(); return; }
    if (e.key === "ArrowUp" && !input.value && !editing) {
      const last = [...loaded].reverse().find((m) => canEdit(m));
      if (last) { e.preventDefault(); startEdit(last); }
      return;
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("composer").requestSubmit(); }
  });
  $("composer").addEventListener("submit", send);

  $("formatBtn").addEventListener("click", (e) => {
    e.stopPropagation();
    $("formatHelp").hidden = !$("formatHelp").hidden;
  });
  document.addEventListener("click", (e) => { if (!$("formatHelp").contains(e.target)) $("formatHelp").hidden = true; });

  // ---------- Schreiben erlaubt? ----------

  function writable() {
    if (isDM(room) && (dmOf(room) || {}).blocked) return false;
    return !roomInfo || roomInfo.canWrite !== false;
  }

  // Namen meiner Rollen, die per @ erwähnt werden können (kleingeschrieben).
  function myRoleMentions() {
    return (me.roles || []).map((id) => roles.byId.get(id)).filter((r) => r && r.mentionable).map((r) => r.name.toLowerCase());
  }

  // ---------- Umfragen ----------

  function buildPoll(m) {
    const p = m.poll;
    const box = h("div", "poll" + (p.closed ? " closed" : ""));
    box.append(h("div", "poll-q", p.question));
    const total = p.options.reduce((n, o) => n + o.votes.length, 0);
    const voters = new Set(p.options.flatMap((o) => o.votes)).size;
    p.options.forEach((o, i) => {
      const mine = o.votes.includes(me.id);
      const pct = total ? Math.round((o.votes.length / total) * 100) : 0;
      const b = h("button", "poll-opt" + (mine ? " mine" : ""));
      b.type = "button";
      b.disabled = p.closed || !me.name || !!me.sanction;
      b.style.setProperty("--pct", pct + "%");
      b.setAttribute("aria-pressed", String(mine));
      b.append(h("span", "poll-check", mine ? "✓" : ""), h("span", "poll-text", o.text), h("span", "poll-num", `${o.votes.length} · ${pct} %`));
      b.addEventListener("click", () => vote(m, i));
      box.append(b);
    });
    const foot = h("div", "poll-foot");
    foot.append(h("span", null, (voters === 1 ? _t("app.1_person_hat_abgestimmt") : _t("app.personen_haben_abgestimmt", { voters: voters })) +
      (p.multi ? _t("app.mehrere_antworten_moeglich") : "") + (p.closed ? _t("app.beendet") : "")));
    if (me.name && (isOwn(m) || me.perms.delete)) {
      const c = h("button", "text-btn small", p.closed ? _t("app.wieder_oeffnen") : _t("app.beenden"));
      c.type = "button";
      c.addEventListener("click", () => vote(m, null, p.closed ? "reopen" : "close"));
      foot.append(c);
    }
    box.append(foot);
    return box;
  }

  async function vote(m, option, action) {
    if (!me.name) { askName(); return; }
    const body = action ? { [action]: true } : { option };
    try { await api(`${roomPath()}/messages/${m.id}/vote`, { method: "POST", body: JSON.stringify(body) }); }
    catch (e) { toast(e.message); }
  }

  // ---------- Threads ----------

  function threadSummary(m) {
    const t = m.threadInfo;
    const b = h("button", "thread-sum");
    b.type = "button";
    b.innerHTML = ICON.thread;
    b.append(h("b", null, t.count === 1 ? _t("app.1_antwort") : _t("app.antworten_2", { count: t.count })),
      h("span", null, (t.names && t.names.length ? t.names.join(", ") + " · " : "") + "zuletzt " + timeFmt.format(new Date(t.lastAt))));
    b.addEventListener("click", () => openThread(m));
    return b;
  }

  async function fetchThread(id) {
    const res = await fetch(`${roomPath()}/threads/${id}`, { headers: token ? { Authorization: "Bearer " + token } : {} });
    const d = await res.json();
    if (!res.ok) throw new Error(d.error || _t("app.thread_nicht_gefunden"));
    return d;
  }

  async function openThread(m, focusId) {
    thread = { root: m, msgs: [], loading: true };
    $("threadPanel").hidden = false;
    document.querySelector(".chat").classList.add("with-thread");
    renderThread();
    renderRoomList();
    try {
      const d = await fetchThread(m.id);
      if (!thread || thread.root.id !== m.id) return;
      thread = { root: d.root, msgs: d.messages };
      renderThread(focusId);
    } catch (e) { toast(e.message); closeThread(); return; }
    if (window.matchMedia("(hover: hover)").matches) $("threadInput").focus();
  }
  async function openThreadById(id, focusId) {
    try { const d = await fetchThread(id); openThread(d.root, focusId); } catch (e) { toast(e.message); }
  }
  async function reloadThread() {
    if (!thread) return;
    try { const d = await fetchThread(thread.root.id); thread = { root: d.root, msgs: d.messages }; renderThread(); }
    catch { closeThread(); }
  }
  function closeThread() {
    thread = null;
    $("threadPanel").hidden = true;
    document.querySelector(".chat").classList.remove("with-thread");
    if (rooms.length) renderRoomList();
  }
  $("threadClose").addEventListener("click", closeThread);

  function renderThread(focusId) {
    if (!thread) return;
    const box = $("threadList");
    const n = thread.msgs.length;
    $("threadSub").textContent = thread.loading ? _t("app.lade") : n === 1 ? _t("app.1_antwort") : _t("app.antworten_3", { n: n });
    const rows = [buildRow(thread.root, false, false, true), h("div", "thread-sep", n ? "" : _t("app.noch_keine_antworten_schreib_die_erste"))];
    let prev = null;
    for (const m of thread.msgs) {
      const cont = prev && author(prev) === author(m) && !m.reply && (new Date(m.time) - new Date(prev.time)) < 5 * 60 * 1000;
      rows.push(buildRow(m, cont, false, true));
      prev = m;
    }
    box.replaceChildren(...rows);
    const target = focusId && box.querySelector(`.msg[data-id="${focusId}"]`);
    if (target) { target.scrollIntoView({ block: "center" }); target.classList.add("flash"); }
    else box.scrollTop = box.scrollHeight;
    $("threadInput").disabled = !me.name || !!me.sanction || !writable();
    $("threadInput").placeholder = writable() ? _t("app.im_thread_antworten") : _t("app.hier_duerfen_nur_bestimmte_rollen_2");
  }
  function threadLive(m) {
    if (!thread || m.thread !== thread.root.id) return;
    if (!thread.msgs.some((x) => x.id === m.id)) thread.msgs.push(m);
    const stick = $("threadList").scrollHeight - $("threadList").scrollTop - $("threadList").clientHeight < 80;
    renderThread();
    if (!stick) $("threadList").scrollTop = $("threadList").scrollHeight; // eigene Antworten sichtbar halten
  }
  function threadUpdate(m) {
    if (!thread) return;
    if (m.id === thread.root.id) { thread.root = m; renderThread(); return; }
    const i = thread.msgs.findIndex((x) => x.id === m.id);
    if (i >= 0) { thread.msgs[i] = m; renderThread(); }
  }

  const tInput = $("threadInput");
  tInput.addEventListener("input", () => { tInput.style.height = "auto"; tInput.style.height = Math.min(tInput.scrollHeight, 140) + "px"; });
  tInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("threadComposer").requestSubmit(); }
    if (e.key === "Escape") closeThread();
  });
  $("threadComposer").addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = tInput.value.trim();
    if (!text || !thread) return;
    if (!me.name) { askName(); return; }
    try {
      const res = await api(`${roomPath()}/messages`, { method: "POST", body: JSON.stringify({ text, thread: thread.root.id }) });
      if (res && res.ephemeral) addEphemeral(res.ephemeral);
      tInput.value = ""; tInput.style.height = "";
    } catch (err) { toast(err.message); }
  });

  // ---------- Angepinnt ----------

  const isPinned = (id) => pinned.some((p) => p.id === id);

  function pinSnippet(m) {
    if (m.deleted) return _t("app.nachricht_geloescht");
    return F.plain(textOf(m) || (m.e2e ? _t("app.verschluesselte_nachricht") : "")) || (m.poll ? _t("app.umfrage") + m.poll.question : m.media ? (m.media.kind === "image" ? _t("app.bild") : "GIF") : "");
  }

  function renderPins() {
    const bar = $("pinBar");
    bar.hidden = !pinned.length;
    if (!pinned.length) { $("pinList").hidden = true; return; }
    const top = pinned[0];
    $("pinLabel").replaceChildren(h("b", null, top.name + ": "), document.createTextNode(pinSnippet(top)));
    $("pinCount").textContent = pinned.length > 1 ? `+${pinned.length - 1}` : "";
    $("pinCount").hidden = pinned.length < 2;
    $("pinList").replaceChildren(...pinned.map((m) => {
      const row = h("div", "pin-item");
      const go = h("button", "pin-go");
      go.type = "button";
      go.append(avatar(m.name, m.bot ? "" : m.userId), h("b", null, m.name), h("span", null, pinSnippet(m)), h("time", null, fullFmt.format(new Date(m.time))));
      go.addEventListener("click", () => { $("pinList").hidden = true; $("pinToggle").setAttribute("aria-expanded", "false"); if (m.thread) openThreadById(m.thread, m.id); else jumpTo(m.id); });
      row.append(go);
      if (me.perms.pin) {
        const un = iconBtn("icon-btn small", _t("app.loesen"), ICON.x);
        un.addEventListener("click", () => setPin(m, false));
        row.append(un);
      }
      return row;
    }));
  }
  $("pinToggle").addEventListener("click", (e) => {
    e.stopPropagation();
    if (pinned.length === 1) { const m = pinned[0]; if (m.thread) openThreadById(m.thread, m.id); else jumpTo(m.id); return; }
    const open = $("pinList").hidden;
    $("pinList").hidden = !open;
    $("pinToggle").setAttribute("aria-expanded", String(open));
  });
  document.addEventListener("click", (e) => { if (!$("pinBar").contains(e.target)) { $("pinList").hidden = true; $("pinToggle").setAttribute("aria-expanded", "false"); } });

  async function setPin(m, pin) {
    try { pinned = await api(`${roomPath()}/messages/${m.id}/pin`, { method: "POST", body: JSON.stringify({ pin }) }); renderPins(); renderAll(); toast(pin ? _t("app.angepinnt") : _t("app.geloest")); }
    catch (e) { toast(e.message); }
  }

  // ---------- Menü „Mehr“ an Nachrichten ----------

  function msgMenuItems(m) {
    const items = [];
    const own = isOwn(m);
    if (me.perms.pin && !m.deleted) items.push([ICON.pin, isPinned(m.id) ? _t("app.loesen") : _t("app.anpinnen"), () => setPin(m, !isPinned(m.id))]);
    if (!own && !m.deleted) items.push([ICON.flag, _t("app.melden"), () => openReport(m)]);
    if (!own && !m.bot && m.userId && dmsEnabled && !isDM(room) && !isBlocked(m.userId)) items.push([ICON.mail, _t("app.direktnachricht_an", { name: m.name }), () => startDM(m.userId)]);
    if (!own && !m.bot && m.userId && me.name && !isDM(room)) {
      items.push(isBlocked(m.userId)
        ? [ICON.block, _t("app.blockierung_von_aufheben", { name: m.name }), () => setBlocked(m.userId, m.name, false)]
        : [ICON.block, _t("app.name_blockieren", { name: m.name }), () => openBlock(m.userId, m.name)]);
    }
    if (!own && me.perms.delete && !m.deleted && (!m.team || me.team)) items.push([ICON.shield, _t("app.loeschen_moderation"), () => chatModDelete(m), "danger"]);
    if (!own && me.perms.mute && !m.bot && m.userId && (!m.team || me.team)) items.push([ICON.mute, _t("app.stummschalten", { name: m.name }), () => openMute(m), "danger"]);
    return items;
  }

  const menu = $("msgMenu");
  function openMsgMenu(anchor, m) { openMenu(anchor, msgMenuItems(m)); }
  function openMenu(anchor, items) {
    menu.replaceChildren(...items.map(([icon, label, fn, cls]) => {
      const b = h("button", "menu-item" + (cls ? " " + cls : ""));
      b.type = "button";
      b.setAttribute("role", "menuitem");
      b.innerHTML = icon;
      b.append(h("span", null, label));
      b.addEventListener("click", () => { menu.hidden = true; fn(); });
      return b;
    }));
    menu.hidden = false;
    menu.dataset.opened = String(Date.now());
    const r = anchor.getBoundingClientRect();
    const w = menu.offsetWidth, mh = menu.offsetHeight;
    menu.style.left = Math.max(8, Math.min(window.innerWidth - w - 8, r.right - w)) + "px";
    let top = r.bottom + 4;
    if (top + mh > window.innerHeight - 8) top = Math.max(8, r.top - mh - 4);
    menu.style.top = top + "px";
    menu.querySelector("button").focus();
  }
  document.addEventListener("click", (e) => { if (!menu.hidden && !menu.contains(e.target)) menu.hidden = true; });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") menu.hidden = true; });
  // Scrollen schließt das Menü – außer direkt nach dem Öffnen (z. B. Nachricht wurde gerade ins Bild gescrollt)
  list.addEventListener("scroll", () => { if (Date.now() - +(menu.dataset.opened || 0) > 400) menu.hidden = true; }, { passive: true });

  async function chatModDelete(m) {
    if (!confirm(_t("app.nachricht_von_fuer_alle_loeschen_2", { name: m.name }))) return;
    try { await api(`${roomPath()}/messages/${m.id}/mod`, { method: "DELETE" }); toast(_t("app.nachricht_geloescht")); }
    catch (e) { toast(e.message); }
  }

  let reportFor = null;
  function openReport(m) {
    reportFor = m;
    $("reportE2E").hidden = !m.e2e;
    $("reportQuote").replaceChildren(h("b", null, m.name + ": "), document.createTextNode(pinSnippet(m).slice(0, 200)));
    $("reportPreset").value = _t("app.beleidigung");
    $("reportReason").value = "";
    $("reportError").textContent = "";
    $("reportDialog").showModal();
  }
  $("reportForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const reason = [$("reportPreset").value, $("reportReason").value.trim()].filter(Boolean).join(": ");
    try {
      const body = { reason };
      if (reportFor.e2e) body.plaintext = textOf(reportFor) || "";
      await api(`${roomPath()}/messages/${reportFor.id}/report`, { method: "POST", body: JSON.stringify(body) });
      $("reportDialog").close();
      toast(_t("app.danke_die_moderation_schaut_sich_das_an"));
    } catch (err) { $("reportError").textContent = err.message; }
  });

  let muteFor = null;
  function openMute(m) {
    muteFor = m;
    $("muteTitle").textContent = `${m.name} stummschalten`;
    $("muteReason").value = "";
    $("muteError").textContent = "";
    $("muteDialog").showModal();
  }
  $("muteForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const minutes = +document.querySelector('input[name="muteMin"]:checked').value;
    try {
      const r = await api(`/api/users/${encodeURIComponent(muteFor.userId)}/mute`, { method: "POST", body: JSON.stringify({ minutes, reason: $("muteReason").value.trim() }) });
      $("muteDialog").close();
      toast(r.unchanged ? _t("app.ist_bereits_laenger_gesperrt", { name: muteFor.name }) : _t("app.ist_fuer_minuten_stummgeschaltet", { name: muteFor.name, minutes: minutes }));
    } catch (err) { $("muteError").textContent = err.message; }
  });

  // ---------- Direktnachrichten ----------

  async function loadDMs() {
    if (!token) { dms = []; dmsEnabled = false; renderDMList(); return; }
    try {
      const res = await fetch("/api/dms", { headers: { Authorization: "Bearer " + token } });
      if (!res.ok) return;
      const d = await res.json();
      const was = isDM(room) ? !!(dmOf(room) || {}).blocked : null;
      dms = d.dms || [];
      dmsEnabled = !!d.enabled;
      if (was !== null && dmOf(room) && was !== !!dmOf(room).blocked) connect(); // Schreibrecht neu holen
    } catch { return; }
    renderDMList();
    if (isDM(room)) {
      $("roomTitle").textContent = (dmOf(room) || {}).name || _t("app.direktnachricht");
      $("input").placeholder = _t("app.nachricht_an", { roomLabel: roomLabel(room) });
      setRoomInfo({ description: dmDescription(room) });
      updateComposer();
    }
  }

  function renderDMList() {
    const shown = dms.slice();
    if (isDM(room) && !shown.some((d) => d.room === room)) shown.unshift({ room, name: _t("app.direktnachricht") });
    $("dmNav").hidden = !shown.length && !(hostedMode() && me.member && dmsEnabled);
    $("dmAddBtn").hidden = !(hostedMode() && me.member && dmsEnabled);
    $("dmList").replaceChildren(...shown.map((d) => {
      const li = h("li");
      const a = h("a");
      a.href = "#" + d.room;
      if (d.room === room) a.setAttribute("aria-current", "page");
      a.append(avatar(d.name, d.with), h("span", "room-name", d.name));
      if (d.e2e) a.append(Object.assign(h("span", "room-flag", d.fed ? "🌐" : "🔒"), { title: d.fed ? _t("app.andere_instanz_ende_zu_ende_2") : _t("app.ende_zu_ende_verschluesselt") }));
      const u = d.room !== room && unread[d.room];
      if (u) {
        a.classList.add("unread");
        a.append(h("span", "unread-badge mention", u.unread > 99 ? "99+" : String(u.unread)));
      }
      li.append(a);
      return li;
    }));
  }

  $("dmAddBtn").addEventListener("click", () => {
    const f = $("dmAddForm");
    f.hidden = !f.hidden;
    if (!f.hidden) $("dmAddInput").focus();
  });
  $("dmAddForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const address = $("dmAddInput").value.trim();
    if (!address) return;
    try {
      const d = await api("/api/dms", { method: "POST", body: JSON.stringify({ address }) });
      $("dmAddInput").value = "";
      $("dmAddForm").hidden = true;
      if (!dms.some((x) => x.room === d.room)) dms.unshift(d);
      renderDMList();
      switchRoom(d.room);
    } catch (err) { toast(err.message); }
  });

  async function startDM(userID) {
    if (!me.name) { askName(); return; }
    try {
      const d = await api("/api/dms", { method: "POST", body: JSON.stringify({ userId: userID }) });
      if (!dms.some((x) => x.room === d.room)) dms.unshift(d);
      renderDMList();
      switchRoom(d.room);
    } catch (e) { toast(e.message); }
  }

  // ---------- Blockieren und Unterhaltungen löschen ----------

  let dmAction = null; // { run(checked) }
  function openDMAction(o) {
    dmAction = o;
    $("dmActionTitle").textContent = o.title;
    $("dmActionText").textContent = o.text;
    $("dmActionOpt").hidden = !o.check;
    $("dmActionCheck").checked = false;
    $("dmActionCheckText").textContent = o.check || "";
    $("dmActionSubmit").textContent = o.submit;
    $("dmActionError").textContent = "";
    $("dmActionDialog").showModal();
  }
  $("dmActionForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!dmAction) return;
    $("dmActionSubmit").disabled = true;
    try {
      await dmAction.run($("dmActionCheck").checked);
      $("dmActionDialog").close();
    } catch (err) { $("dmActionError").textContent = err.message; }
    finally { $("dmActionSubmit").disabled = false; }
  });

  function openBlock(id, name) {
    const d = dms.find((x) => x.with === id);
    openDMAction({
      title: _t("app.name_blockieren", { name }),
      text: _t("app.blockieren_erklaerung", { name }),
      check: d && d.messages ? _t("app.unterhaltung_mit_name_auch_loeschen", { name }) : "",
      submit: _t("app.blockieren"),
      run: (clear) => setBlocked(id, name, true, clear, true),
    });
  }

  async function setBlocked(id, name, block, clear = false, throwErr = false) {
    try {
      const r = await api("/api/blocks", { method: "POST", body: JSON.stringify({ userId: id, block, clear }) });
      me.blocked = (r.blocked || []).map((x) => x.id);
      blockedCache = r.blocked || [];
      renderBlockedList(true);
      toast(block ? _t("app.name_ist_blockiert", { name }) : _t("app.blockierung_von_name_aufgehoben", { name }));
      await loadDMs();
      if (isDM(room) && !dmOf(room)) switchRoom(firstRoom()); // Unterhaltung gelöscht
      else if (isDM(room)) connect();
      renderAll();
      updateComposer();
    } catch (err) { if (throwErr) throw err; toast(err.message); }
  }

  function openClearDM(r) {
    const d = dmOf(r) || {};
    openDMAction({
      title: _t("app.unterhaltung_loeschen"),
      text: d.fed ? _t("app.unterhaltung_loeschen_fed", { name: d.name }) : _t("app.unterhaltung_loeschen_erklaerung", { name: d.name }),
      check: _t("app.meine_nachrichten_auch_bei_name_loeschen", { name: d.name }),
      submit: _t("app.loeschen"),
      run: async (own) => {
        await api(`/api/dms/${encodeURIComponent(r)}${own ? "?own=1" : ""}`, { method: "DELETE" });
        toast(_t("app.unterhaltung_geloescht"));
        await loadDMs();
        if (room === r) switchRoom(dmOf(r) ? r : firstRoom());
      },
    });
  }

  $("dmMenuBtn").addEventListener("click", async (e) => {
    e.stopPropagation();
    if (!menu.hidden) { menu.hidden = true; return; }
    if (!dmOf(room)) await loadDMs(); // z. B. gerade erst neu erschienen
    const d = dmOf(room);
    if (!d) return;
    const items = [];
    if (d.blockedByMe) items.push([ICON.block, _t("app.blockierung_von_aufheben", { name: d.name }), () => setBlocked(d.with, d.name, false)]);
    else items.push([ICON.block, _t("app.name_blockieren", { name: d.name }), () => openBlock(d.with, d.name), "danger"]);
    items.push([ICON.trash, _t("app.unterhaltung_loeschen"), () => openClearDM(room), "danger"]);
    openMenu($("dmMenuBtn"), items);
  });

  let blockedCache = null;
  async function renderBlockedList(cached) {
    const ul = $("blockedList");
    if (!cached || !blockedCache) {
      try { blockedCache = (await api("/api/blocks")).blocked || []; } catch { blockedCache = []; }
    }
    if (!blockedCache.length) { ul.replaceChildren(h("li", "blocked-empty", _t("app.niemand_blockiert"))); return; }
    ul.replaceChildren(...blockedCache.map((b) => {
      const li = h("li");
      li.append(avatar(b.name, b.fed ? "" : b.id), h("span", "blocked-name", b.name));
      const btn = h("button", "text-btn", _t("app.aufheben"));
      btn.type = "button";
      btn.addEventListener("click", () => setBlocked(b.id, b.name, false));
      li.append(btn);
      return li;
    }));
  }

  // ---------- Profilbilder und Status ----------

  async function loadProfiles() {
    try {
      const res = await fetch("/api/profiles", { headers: token ? { Authorization: "Bearer " + token } : {} });
      if (res.ok) profiles = await res.json() || {};
    } catch { /* ohne */ }
    if (me.id) $("meAvatar").replaceWith(Object.assign(avatar(me.name, me.id), { id: "meAvatar" }));
    renderDMList();
  }

  function fillProfileLook() {
    const p = profiles[me.id] || {};
    $("avatarRow").hidden = media.avatars === false;
    const big = avatar(me.name, me.id);
    big.id = "profileBig";
    big.classList.add("big");
    $("profileBig").replaceWith(big);
    $("avatarRemove").hidden = !p.avatar;
    $("statusSelect").value = p.status || "";
    $("statusText").value = p.text || "";
    $("statusError").textContent = "";
  }
  $("avatarUpload").addEventListener("click", () => $("avatarFile").click());
  $("avatarFile").addEventListener("change", async () => {
    const f = $("avatarFile").files[0];
    $("avatarFile").value = "";
    if (!f) return;
    const fd = new FormData();
    fd.append("file", f);
    $("statusError").textContent = _t("app.lade_hoch");
    try {
      const res = await fetch("/api/profile/avatar", { method: "POST", headers: { Authorization: "Bearer " + token }, body: fd });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || _t("app.hochladen_fehlgeschlagen"));
      profiles[me.id] = Object.assign({}, profiles[me.id], { avatar: d.avatar });
      $("statusError").textContent = "";
      fillProfileLook();
      $("profileAvatar").replaceWith(Object.assign(avatar(me.name, me.id), { id: "profileAvatar" }));
      $("meAvatar").replaceWith(Object.assign(avatar(me.name, me.id), { id: "meAvatar" }));
    } catch (e) { $("statusError").textContent = e.message; }
  });
  $("avatarRemove").addEventListener("click", async () => {
    try {
      await api("/api/profile/avatar", { method: "DELETE" });
      if (profiles[me.id]) delete profiles[me.id].avatar;
      fillProfileLook();
      $("profileAvatar").replaceWith(Object.assign(avatar(me.name, me.id), { id: "profileAvatar" }));
      $("meAvatar").replaceWith(Object.assign(avatar(me.name, me.id), { id: "meAvatar" }));
    } catch (e) { $("statusError").textContent = e.message; }
  });
  $("statusForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("statusError").textContent = "";
    try {
      const p = await api("/api/profile/status", { method: "PUT", body: JSON.stringify({ status: $("statusSelect").value, text: $("statusText").value }) });
      profiles[me.id] = Object.assign({}, profiles[me.id], { status: p.status, text: p.text });
      fillProfileLook();
      toast(_t("app.status_gespeichert"));
    } catch (err) { $("statusError").textContent = err.message; }
  });

  // ---------- Bilder hochladen ----------

  function updateUploadBtn() {
    const allowed = !!me.canUpload && (media.uploads || "off") !== "off";
    const show = allowed && !!me.name && !me.sanction && writable();
    $("uploadBtn").hidden = !show;
    $("threadUploadBtn").hidden = !show;
  }

  async function uploadImage(file, inThread) {
    if (!me.name) { askName(); return; }
    if (!/^image\/(png|jpeg|gif)$/.test(file.type)) { toast(_t("app.bitte_ein_png_jpeg_oder_gif_bild_waehlen")); return; }
    if (file.size > 8 * 1024 * 1024) { toast(_t("app.das_bild_ist_zu_gross_hoechstens_8_mb")); return; }
    const fd = new FormData();
    fd.append("file", file);
    const box = inThread ? tInput : input;
    const text = box.value.trim();
    if (text) fd.append("text", text);
    if (inThread && thread) fd.append("thread", thread.root.id);
    else if (replyTo) fd.append("replyTo", replyTo.id);
    const btn = inThread ? $("threadUploadBtn") : $("uploadBtn");
    btn.classList.add("sending");
    try {
      const res = await fetch(`${roomPath()}/upload`, { method: "POST", headers: { Authorization: "Bearer " + token }, body: fd });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || _t("app.hochladen_fehlgeschlagen"));
      box.value = "";
      if (!inThread) { autosize(); cancelCompose(); list.scrollTop = list.scrollHeight; }
    } catch (e) { toast(e.message); }
    finally { btn.classList.remove("sending"); }
  }
  let uploadTarget = false;
  $("uploadBtn").addEventListener("click", () => { uploadTarget = false; $("uploadFile").click(); });
  $("threadUploadBtn").addEventListener("click", () => { uploadTarget = true; $("uploadFile").click(); });
  $("uploadFile").addEventListener("change", () => {
    const f = $("uploadFile").files[0];
    $("uploadFile").value = "";
    if (f) uploadImage(f, uploadTarget);
  });
  function pastedImage(e) {
    const items = (e.clipboardData && e.clipboardData.files) || [];
    return [...items].find((f) => /^image\//.test(f.type));
  }
  input.addEventListener("paste", (e) => { const f = pastedImage(e); if (f && !$("uploadBtn").hidden) { e.preventDefault(); uploadImage(f, false); } });
  tInput.addEventListener("paste", (e) => { const f = pastedImage(e); if (f && !$("threadUploadBtn").hidden) { e.preventDefault(); uploadImage(f, true); } });
  for (const [el, inThread] of [[$("composer"), false], [$("threadComposer"), true]]) {
    el.addEventListener("dragover", (e) => { if (!$("uploadBtn").hidden && e.dataTransfer && [...e.dataTransfer.types].includes("Files")) { e.preventDefault(); el.classList.add("drop"); } });
    el.addEventListener("dragleave", () => el.classList.remove("drop"));
    el.addEventListener("drop", (e) => {
      el.classList.remove("drop");
      const f = e.dataTransfer && [...e.dataTransfer.files].find((x) => /^image\//.test(x.type));
      if (f) { e.preventDefault(); uploadImage(f, inThread); }
    });
  }

  // ---------- Rollen ----------

  function userRoles(id) {
    return ((id && roles.users[id]) || []).map((r) => roles.byId.get(r)).filter(Boolean);
  }
  // Abzeichen anhängen; nameEl wird ggf. in der Farbe der ersten passenden Rolle eingefärbt.
  function addRoleBadges(box, id, nameEl) {
    const list = userRoles(id);
    for (const r of list) {
      const b = h("span", "badge role-badge", r.name);
      b.style.setProperty("--role", r.color);
      box.append(b);
    }
    const tint = list.find((r) => r.colorName);
    if (tint && nameEl) { nameEl.classList.add("role-name"); nameEl.style.setProperty("--role", tint.color); }
  }
  async function loadRoles() {
    try {
      const d = await (await fetch("/api/roles")).json();
      roles = { byId: new Map((d.roles || []).map((r) => [r.id, r])), users: d.users || {} };
    } catch { /* ohne Rollen weiter */ }
  }

  // ---------- Emotes und GIFs ----------

  async function loadMedia() {
    try {
      const d = await (await fetch("/api/media")).json();
      emotes = new Map((d.emotes || []).map((e) => [e.name, e]));
      media = { gifs: !!d.gifs, web: d.web || "", beacon: !!d.beacon, gifWidth: d.gifWidth || 300, emoteReactions: !!d.emoteReactions,
        uploads: d.uploads || "off", uploadSize: d.uploadSize || 480, avatars: d.avatars !== false };
      if (Array.isArray(d.reactions) && d.reactions.length) settings.reactions = d.reactions;
    } catch { /* ohne Emotes weiter */ }
    $("emoteBtn").hidden = !emotes.size;
    $("gifBtn").hidden = !media.gifs;
    updateUploadBtn();
    if (!$("mediaPanel").hidden) renderMediaTab();
  }

  async function loadCommands() {
    try {
      commands = await (await fetch("/api/commands?room=" + encodeURIComponent(room))).json();
    } catch { /* ohne Vervollständigung */ }
  }

  let mediaTab = "emotes";
  let gifTimer = 0;

  function openMediaPanel(tab) {
    const panel = $("mediaPanel");
    if (!panel.hidden && mediaTab === tab) { panel.hidden = true; return; }
    mediaTab = tab;
    panel.hidden = false;
    $("formatHelp").hidden = true;
    $("mediaSearch").value = "";
    renderMediaTab();
    if (window.matchMedia("(hover: hover)").matches) $("mediaSearch").focus();
  }

  function renderMediaTab() {
    const tabs = [["emotes", _t("app.emotes"), emotes.size > 0], ["library", _t("app.gifs"), media.gifs], ["beacon", _t("app.sammlungen"), media.gifs && !!media.beacon], ["web", media.web === "giphy" ? "GIPHY" : _t("app.tenor"), media.gifs && !!media.web]];
    if (!tabs.some((t) => t[0] === mediaTab && t[2])) mediaTab = (tabs.find((t) => t[2]) || ["emotes"])[0];
    $("mediaTabs").replaceChildren(...tabs.filter((t) => t[2]).map(([id, label]) => {
      const b = h("button", null, label);
      b.type = "button";
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", String(id === mediaTab));
      b.addEventListener("click", () => { mediaTab = id; $("mediaSearch").value = ""; renderMediaTab(); $("mediaSearch").focus(); });
      return b;
    }));
    $("mediaSearch").placeholder = mediaTab === "emotes" ? _t("app.emote_suchen") : mediaTab === "web" ? _t("app.gifs_im_web_suchen") : mediaTab === "beacon" ? _t("app.abonnierte_sammlungen_durchsuchen") : _t("app.gif_bibliothek_durchsuchen");
    $("mediaNote").textContent = mediaTab === "web" ? (media.web === "giphy" ? _t("app.powered_by_giphy_vorschau_ueber_diesen") : _t("app.ueber_tenor_vorschau_ueber_diesen_server"))
      : mediaTab === "beacon" ? _t("app.aus_abonnierten_beacon_sammlungen") : "";
    $("mediaNote").hidden = mediaTab !== "web" && mediaTab !== "beacon";
    loadMediaItems();
  }

  async function loadMediaItems() {
    const q = $("mediaSearch").value.trim().toLowerCase();
    const grid = $("mediaGrid");
    grid.className = "media-grid " + (mediaTab === "emotes" ? "emotes" : "gifs");
    if (mediaTab === "emotes") {
      const list = [...emotes.values()].filter((e) => !q || e.name.includes(q));
      grid.replaceChildren(...(list.length ? list.map((e) => {
        const b = h("button", "emote-pick");
        b.type = "button";
        b.title = `:${e.name}:`;
        b.setAttribute("aria-label", _t("app.emote", { name: e.name }));
        const img = h("img");
        img.src = e.url; img.alt = ""; img.width = 32; img.height = 32; img.loading = "lazy";
        b.append(img);
        b.addEventListener("click", () => insertEmote(e.name));
        return b;
      }) : [h("p", "media-empty", _t("app.kein_emote_gefunden"))]));
      return;
    }
    grid.replaceChildren(h("p", "media-empty", _t("app.lade")));
    const tab = mediaTab;
    try {
      const url = tab === "web" ? "/api/gifs/web?q=" + encodeURIComponent(q) : tab === "beacon" ? "/api/gifs/beacon?q=" + encodeURIComponent(q) : "/api/gifs?q=" + encodeURIComponent(q);
      const res = await fetch(url);
      const list = await res.json();
      if (tab !== mediaTab) return;
      if (!res.ok) throw new Error(list.error);
      grid.replaceChildren(...(list.length ? list.map((g) => {
        const b = h("button", "gif-pick");
        b.type = "button";
        b.title = g.title || "GIF";
        b.setAttribute("aria-label", _t("app.gif_senden") + (g.title || _t("app.ohne_titel")));
        const img = h("img");
        img.src = tab === "web" || tab === "beacon" ? g.preview : g.url;
        img.alt = "";
        img.loading = "lazy";
        if (g.w && g.h) { img.width = g.w; img.height = g.h; }
        b.append(img);
        b.addEventListener("click", () => sendGIF(tab === "web" || tab === "beacon" ? g.id : "lib:" + g.id, b));
        return b;
      }) : [h("p", "media-empty", q ? _t("app.nichts_gefunden") : _t("app.noch_keine_gifs_in_der_bibliothek"))]));
    } catch (e) {
      grid.replaceChildren(h("p", "media-empty", e.message || _t("app.gifs_konnten_nicht_geladen_werden")));
    }
  }

  $("mediaSearch").addEventListener("input", () => {
    clearTimeout(gifTimer);
    gifTimer = setTimeout(loadMediaItems, mediaTab === "web" || mediaTab === "beacon" ? 350 : 120);
  });
  $("mediaSearch").addEventListener("keydown", (e) => { if (e.key === "Escape") { $("mediaPanel").hidden = true; input.focus(); } });
  $("emoteBtn").addEventListener("click", (e) => { e.stopPropagation(); openMediaPanel("emotes"); });
  $("gifBtn").addEventListener("click", (e) => { e.stopPropagation(); openMediaPanel("library"); });
  document.addEventListener("click", (e) => {
    if (!$("mediaPanel").hidden && e.target.isConnected && !$("mediaPanel").contains(e.target)) $("mediaPanel").hidden = true;
  });

  function insertEmote(name) {
    const pos = input.selectionStart ?? input.value.length;
    const v = input.value;
    const pre = v.slice(0, pos);
    const add = (pre && !/\s$/.test(pre) ? " " : "") + `:${name}: `;
    input.value = pre + add + v.slice(pos);
    input.setSelectionRange(pos + add.length, pos + add.length);
    autosize();
    input.focus();
  }

  async function sendGIF(ref, btn) {
    if (!me.name) { askName(); return; }
    if (editing) cancelCompose();
    btn.classList.add("sending");
    try {
      const body = { gif: ref };
      if (replyTo) body.replyTo = replyTo.id;
      const text = input.value.trim();
      if (text) body.text = text; // geschriebener Text wird zur Bildunterschrift
      await api(`${roomPath()}/messages`, { method: "POST", body: JSON.stringify(body) });
      $("mediaPanel").hidden = true;
      input.value = "";
      autosize();
      cancelCompose();
      list.scrollTop = list.scrollHeight;
    } catch (err) {
      toast(err.message);
    } finally {
      btn.classList.remove("sending");
    }
  }

  // ---------- Suche ----------

  let searchTimer = 0;
  function openSearch() {
    $("searchPanel").hidden = false;
    $("searchInput").value = "";
    $("searchResults").replaceChildren();
    $("searchInfo").textContent = _t("app.durchsucht_die_nachrichten_in", { roomLabel: roomLabel(room) });
    $("searchInput").focus();
  }
  function closeSearch() { $("searchPanel").hidden = true; }
  $("searchBtn").addEventListener("click", () => ($("searchPanel").hidden ? openSearch() : closeSearch()));
  $("searchClose").addEventListener("click", closeSearch);
  $("searchInput").addEventListener("keydown", (e) => { if (e.key === "Escape") closeSearch(); });
  $("searchInput").addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(runSearch, 250);
  });

  function highlight(target, text, q) {
    const lower = text.toLowerCase(), needle = q.toLowerCase();
    let i = 0, j;
    while (needle && (j = lower.indexOf(needle, i)) >= 0) {
      target.append(text.slice(i, j), h("mark", null, text.slice(j, j + needle.length)));
      i = j + needle.length;
    }
    target.append(text.slice(i));
  }

  async function runSearch() {
    const q = $("searchInput").value.trim().replace(/\s+/g, " ");
    const out = $("searchResults");
    if (q.length < 2) { out.replaceChildren(); $("searchInfo").textContent = _t("app.mindestens_2_zeichen_eingeben"); return; }
    try {
      const res = await fetch(`${roomPath()}/search?q=${encodeURIComponent(q)}`, { headers: token ? { Authorization: "Bearer " + token } : {} });
      const hits = await res.json();
      if (!res.ok) throw new Error(hits.error);
      $("searchInfo").textContent = hits.length ? (hits.length === 50 ? _t("app.newest_hits", { n: 50 }) : _t("app.treffer", { n: hits.length })) : _t("app.nichts_gefunden");
      out.replaceChildren(...hits.map(({ message: m }) => {
        const b = h("button", "hit");
        b.type = "button";
        const head = h("div", "hit-head");
        head.append(avatar(m.name, m.userId), h("b", null, m.name), h("time", null, fullFmt.format(new Date(m.time))));
        const p = h("p");
        highlight(p, F.plain(m.text), q);
        b.append(head, p);
        if (m.thread) head.append(h("span", "hit-thread", _t("app.im_thread")));
        b.addEventListener("click", () => { closeSearch(); if (m.thread) openThreadById(m.thread, m.id); else jumpTo(m.id); });
        return b;
      }));
    } catch (e) {
      $("searchInfo").textContent = e.message || _t("app.suche_fehlgeschlagen");
    }
  }

  // ---------- Name ----------

  function setMe(d) {
    const changed = me.id !== d.id || me.name !== d.name || me.team !== !!d.team;
    me = { id: d.id, name: d.name, team: !!d.team, viewer: d.viewer || "", sanction: d.sanction || null, roles: d.roles || [], perms: d.perms || {}, canUpload: !!d.canUpload, member: !!d.member, pinned: d.pinned || [], blocked: d.blocked || [] };
    store.set("gochat:name", d.name);
    $("meName").textContent = d.name;
    $("meAvatar").replaceWith(Object.assign(avatar(d.name, d.id), { id: "meAvatar" }));
    $("meHint").textContent = (me.team ? _t("app.team_2") : "") + (me.member ? _t("app.konto") : _t("app.profil"));
    updateComposer();
    return changed;
  }

  function askName() {
    if (hostedMode() && window.GoAccount) { GoAccount.showLogin(); return; }
    askGuestName();
  }

  function askGuestName() {
    $("nameInput").value = me.name || store.get("gochat:name") || "";
    $("nameError").textContent = "";
    if (!$("nameDialog").open) $("nameDialog").showModal();
  }

  $("nameForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const n = $("nameInput").value.trim();
    if (!n) return;
    $("nameSubmit").disabled = true;
    try {
      const headers = { "Content-Type": "application/json" };
      if (token) headers.Authorization = "Bearer " + token;
      const res = await fetch("/api/session", { method: "POST", headers, body: JSON.stringify({ name: n }) });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        $("nameError").textContent = data.error || _t("app.der_name_konnte_nicht_uebernommen_werden");
        return;
      }
      token = data.token;
      store.set("gochat:token", token);
      setMe(data);
      $("nameDialog").close();
      if (window.GoAccount) GoAccount.close();
      loadDMs();
      loadRooms();
      connect();
    } catch {
      $("nameError").textContent = _t("app.server_nicht_erreichbar");
    } finally {
      $("nameSubmit").disabled = false;
    }
  });
  $("nameDialog").addEventListener("cancel", (e) => { if (!me.name && !hostedMode()) e.preventDefault(); });

  // ---------- Profil: ändern, Benachrichtigungen, exportieren, importieren ----------

  function openProfile() {
    $("profileName").textContent = me.name;
    $("profileAvatar").replaceWith(Object.assign(avatar(me.name, me.id), { id: "profileAvatar" }));
    fillProfileLook();
    $("profileTeam").hidden = !me.team;
    for (const id of ["renameForm", "profileExport", "profileImport"]) $(id).hidden = !!me.member;
    $("profileAccount").hidden = !me.member;
    $("profileLead").textContent = me.member ? _t("app.du_bist_mit_deinem_konto_angemeldet") : _t("app.dein_name_ist_fuer_dich_reserviert_alle");
    $("logoutBtn").textContent = me.member ? _t("app.abmelden") : _t("app.abmelden_und_namen_freigeben");
    $("renameInput").value = me.name;
    $("renameError").textContent = "";
    $("prefSound").value = prefs.sound;
    $("prefNotify").value = prefs.notify;
    updateNotifyHint();
    const days = settings.retentionDays;
    $("retentionNote").textContent = days ? _t("app.nachrichten_werden_nach_tagen", { days: days }) : "";
    $("retentionNote").hidden = !days;
    renderBlockedList();
    $("profileDialog").showModal();
  }
  $("profileBtn").addEventListener("click", () => (me.name ? openProfile() : askName()));
  document.querySelectorAll("dialog [data-close]").forEach((b) => b.addEventListener("click", () => b.closest("dialog").close()));

  function savePrefs() { store.setJSON("gochat:prefs", prefs); }

  function updateNotifyHint() {
    const hint = $("notifyHint");
    if (!("Notification" in window)) { hint.textContent = _t("app.dieser_browser_unterstuetzt_keine"); return; }
    if (prefs.notify !== "off" && Notification.permission === "denied") {
      hint.textContent = _t("app.der_browser_blockiert_benachrichtigungen");
    } else hint.textContent = _t("app.benachrichtigungen_erscheinen_wenn_der");
  }

  $("prefSound").addEventListener("change", () => {
    prefs.sound = $("prefSound").value;
    savePrefs();
    if (prefs.sound !== "off") ping(true);
  });
  $("prefNotify").addEventListener("change", async () => {
    prefs.notify = $("prefNotify").value;
    savePrefs();
    if (prefs.notify !== "off" && "Notification" in window && Notification.permission === "default") {
      try { await Notification.requestPermission(); } catch { /* egal */ }
    }
    updateNotifyHint();
  });

  $("renameForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const n = $("renameInput").value.trim();
    if (!n || n === me.name) return;
    $("renameSubmit").disabled = true;
    $("renameError").textContent = "";
    try {
      const res = await fetch("/api/session", {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
        body: JSON.stringify({ name: n }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) { $("renameError").textContent = data.error || _t("app.der_name_konnte_nicht_geaendert_werden"); return; }
      token = data.token; store.set("gochat:token", token);
      setMe(data);
      $("profileDialog").close();
      toast(_t("app.du_heisst_jetzt", { name: data.name }));
      connect();
    } catch {
      $("renameError").textContent = _t("app.server_nicht_erreichbar");
    } finally {
      $("renameSubmit").disabled = false;
    }
  });

  // Format: Datei = JSON, Code = "UCNG1:" + base64url(JSON). Beides enthält das geheime Token.
  // Codes von vor der Umbenennung beginnen mit "GOCHAT1:" und werden weiter angenommen.
  const CODE_PREFIX = "UCNG1:";
  const CODE_PREFIXES = [CODE_PREFIX, "GOCHAT1:"];
  function b64urlEncode(str) {
    const bytes = new TextEncoder().encode(str);
    let bin = "";
    bytes.forEach((b) => (bin += String.fromCharCode(b)));
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function b64urlDecode(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    const bin = atob(s);
    return new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
  }
  function profileCode() {
    return CODE_PREFIX + b64urlEncode(JSON.stringify({ s: location.origin, n: me.name, t: token, p: prefs }));
  }

  $("exportFileBtn").addEventListener("click", () => {
    const data = {
      format: "ucng-profile", version: 2, server: location.origin,
      name: me.name, id: me.id, token, prefs, exportedAt: new Date().toISOString(),
      hinweis: _t("app.wer_diese_datei_hat_kann_unter_deinem"),
      code: profileCode(),
    };
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `ucng-profil-${me.name.replace(/[^\p{L}\p{N}_-]+/gu, "_")}.json`;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    toast(_t("app.profil_datei_gespeichert"));
  });

  $("exportCodeBtn").addEventListener("click", async () => {
    const code = profileCode();
    try {
      await navigator.clipboard.writeText(code);
      toast(_t("app.code_kopiert_fuege_ihn_auf_dem_anderen"));
    } catch {
      prompt(_t("app.code_kopieren_strg_c_lange_druecken"), code);
    }
  });

  function openImport() {
    $("importCode").value = "";
    $("importFile").value = "";
    $("importFileName").textContent = "";
    $("importError").textContent = "";
    const warn = $("importReplaceWarn");
    warn.hidden = !me.name;
    warn.textContent = me.name ? _t("app.dein_aktuelles_profil_wird_auf_diesem", { name: me.name }) : "";
    $("importDialog").showModal();
  }
  $("profileImportBtn").addEventListener("click", () => { $("profileDialog").close(); openImport(); });
  $("nameImportBtn").addEventListener("click", openImport);
  $("importFileBtn").addEventListener("click", () => $("importFile").click());
  $("importFile").addEventListener("change", async () => {
    const f = $("importFile").files[0];
    if (!f) return;
    $("importFileName").textContent = f.name;
    if (f.size > 64 * 1024) { $("importError").textContent = _t("app.das_ist_keine_profil_datei_zu_gross"); return; }
    $("importCode").value = await f.text();
  });

  function parseProfile(text) {
    text = text.trim();
    let d;
    const prefix = CODE_PREFIXES.find((p) => text.toUpperCase().startsWith(p));
    if (prefix) {
      try { d = JSON.parse(b64urlDecode(text.slice(prefix.length).replace(/\s+/g, ""))); } catch { return null; }
      return d && d.t ? { token: d.t, server: d.s, name: d.n, prefs: d.p } : null;
    }
    try { d = JSON.parse(text); } catch { return null; }
    if (d && (d.format === "ucng-profile" || d.format === "gochat-profile") && d.token) return { token: d.token, server: d.server, name: d.name, prefs: d.prefs };
    return null;
  }

  $("importForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("importError").textContent = "";
    const p = parseProfile($("importCode").value);
    if (!p) { $("importError").textContent = _t("app.das_ist_kein_gueltiges_gochat_profil"); return; }
    $("importSubmit").disabled = true;
    try {
      const res = await fetch("/api/session", { headers: { Authorization: "Bearer " + p.token } });
      if (res.status === 401) {
        $("importError").textContent = p.server && p.server !== location.origin
          ? _t("app.dieses_profil_stammt_von_und_gilt_nur", { server: p.server })
          : _t("app.dieses_profil_gilt_nicht_mehr_der_name");
        return;
      }
      if (!res.ok) { $("importError").textContent = _t("app.das_profil_konnte_nicht_geprueft_werden"); return; }
      const d = await res.json();
      token = p.token; store.set("gochat:token", token);
      if (p.prefs && typeof p.prefs === "object") {
        prefs = Object.assign({}, DEFAULT_PREFS, { sound: p.prefs.sound || prefs.sound, notify: p.prefs.notify || prefs.notify });
        savePrefs();
      }
      setMe(d);
      loadDMs();
      loadRooms();
      $("importDialog").close();
      if ($("nameDialog").open) $("nameDialog").close();
      toast(_t("app.willkommen_zurueck", { name: d.name }));
      connect();
    } catch {
      $("importError").textContent = _t("app.server_nicht_erreichbar");
    } finally {
      $("importSubmit").disabled = false;
    }
  });

  $("accountBtn").addEventListener("click", () => { $("profileDialog").close(); if (window.GoAccount) GoAccount.openSettings(); });

  async function signedOut() {
    token = ""; store.set("gochat:token", "");
    if (window.GoAccount) await GoAccount.forget();
    plainCache.clear();
    me = { id: "", name: "", team: false, viewer: "", sanction: null, roles: [], perms: {} };
    dms = []; renderDMList();
    $("meName").textContent = "–";
    if ($("profileDialog").open) $("profileDialog").close();
    updateComposer();
    if (isDM(room)) switchRoom(firstRoom()); else connect();
    askName();
  }

  $("logoutBtn").addEventListener("click", async () => {
    if (me.member) {
      if (!confirm(_t("app.auf_diesem_geraet_abmelden"))) return;
      await fetch("/api/session", { method: "DELETE", headers: { Authorization: "Bearer " + token } }).catch(() => {});
      await signedOut();
      return;
    }
    if (!confirm(_t("app.wirklich_abmelden_der_name_wird", { name: me.name }))) return;
    await fetch("/api/session", { method: "DELETE", headers: { Authorization: "Bearer " + token } }).catch(() => {});
    token = ""; store.set("gochat:token", "");
    me = { id: "", name: "", team: false, viewer: "", sanction: null, roles: [], perms: {} };
    dms = []; renderDMList();
    $("meName").textContent = "–";
    $("profileDialog").close();
    updateComposer();
    connect();
    askName();
  });

  // ---------- Navigation ----------

  $("joinForm").addEventListener("submit", (e) => {
    e.preventDefault();
    const r = $("joinInput").value.trim();
    if (r) { $("joinInput").value = ""; switchRoom(r); }
  });
  $("menuBtn").addEventListener("click", () => $("sidebar").classList.toggle("open"));
  $("scrim").addEventListener("click", () => $("sidebar").classList.remove("open"));
  window.addEventListener("hashchange", () => {
    const r = normRoom(decodeURIComponent(location.hash.slice(1)));
    if (r && r !== room) switchRoom(r);
  });
  document.addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k" && room) { e.preventDefault(); openSearch(); }
  });

  // ---------- Start ----------

  async function restoreSession() {
    if (token) {
      try {
        const res = await fetch("/api/session", { headers: { Authorization: "Bearer " + token } });
        if (res.ok) {
          setMe(await res.json());
          if (me.member && window.GoAccount) await GoAccount.loadKey(me.id);
          return true;
        }
        if (res.status === 401) { token = ""; store.set("gochat:token", ""); }
      } catch { /* Server nicht erreichbar */ }
    }
    return false;
  }

  async function checkAdmin() {
    if (!adminToken) return false;
    const res = await fetch("/api/admin/me", { headers: { Authorization: "Bearer " + adminToken } }).catch(() => null);
    return !!(res && res.ok);
  }

  (async () => {
    const [st, , , hasName, admin] = await Promise.all([
      fetch("/api/settings").then((r) => r.json()).catch(() => null),
      loadRooms(),
      Promise.all([loadMedia(), loadRoles(), loadProfiles(), loadGroups()]),
      restoreSession().then(async (ok) => { if (ok) { await loadDMs(); await loadRooms(); } return ok; }),
      checkAdmin(),
    ]);
    isAdmin = admin;
    applySettings(st || settings);
    let accountFlow = false;
    if (hostedMode() && window.GoAccount) {
      GoAccount.init({
        settings: () => settings,
        token: () => token,
        setToken: (t) => { token = t; store.set("gochat:token", t); },
        toast,
        guestName: askGuestName,
        loggedIn: () => !!me.name,
        onKey: onKeyChanged,
        onSession: async (s) => {
          setMe(s);
          if ($("nameDialog").open) $("nameDialog").close();
          await loadDMs();
          await Promise.all([loadRooms(), loadProfiles()]);
          if (!room || (!rooms.some((x) => x.name === room) && !isDM(room))) switchRoom(firstRoom()); else connect();
          toast(_t("app.willkommen", { name: s.name }));
        },
        onLogout: () => signedOut(),
      });
      accountFlow = await GoAccount.start();
      if (!hasName && !rooms.length) await loadRooms();
    }
    switchRoom(decodeURIComponent(location.hash.slice(1)) || firstRoom());
    let reader = false;
    try { reader = hostedMode() && settings.hosted && settings.hosted.guests === "read" && sessionStorage.getItem("gochat:reader") === "1"; } catch { /* egal */ }
    if (!hasName && !accountFlow && !reader) askName();
    setInterval(() => { loadRooms(); loadDMs(); }, 15000);
    setInterval(() => { // Bearbeiten-Knopf nach 15 Minuten ausblenden
      for (const m of loaded) {
        if (isOwn(m) && !m.deleted && !canEdit(m)) {
          const row = list.querySelector(`.msg[data-id="${m.id}"] .msg-actions [aria-label="Bearbeiten"]`);
          if (row) row.remove();
        }
      }
    }, 30000);
  })();
})();
