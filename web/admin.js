// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const TOKEN_KEY = "gochat:admin";
  const store = {
    get() { try { return localStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; } },
    set(v) { try { v ? localStorage.setItem(TOKEN_KEY, v) : localStorage.removeItem(TOKEN_KEY); } catch { /* egal */ } },
  };

  let token = store.get();
  let settings = { title: "UCNG", allowUserRooms: true };
  let rooms = [];
  let actor = { name: "", display: "", role: "" };
  const isAdmin = () => actor.role === "admin";

  // ---------- API ----------

  class ApiError extends Error {
    constructor(status, msg) { super(msg); this.status = status; }
  }

  async function api(method, path, body) {
    const headers = { Authorization: "Bearer " + token };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    if (res.status === 401 && path !== "/api/admin/login") {
      showLogin();
      throw new ApiError(401, _t("admin.sitzung_abgelaufen_bitte_neu_anmelden"));
    }
    if (res.status === 204) return null;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      const err = new ApiError(res.status, data.error || _t("admin.das_hat_nicht_geklappt"));
      if (data.problems) err.problems = data.problems;
      throw err;
    }
    lastHasMore = res.headers.get("X-Has-More") === "true";
    return data;
  }
  let lastHasMore = false; // X-Has-More der letzten Antwort (seitenweises Laden)

  // ---------- Hilfen ----------

  const AVATAR_COLORS = ["#5848f4", "#0f8f7e", "#d1495b", "#2a7fd4", "#b05cc9", "#c46a12", "#3d8b3d", "#7a5af0", "#8a6a00", "#1f7a99"];
  function avatar(n) {
    let h = 0;
    for (const c of n.toLowerCase()) h = (h * 31 + c.codePointAt(0)) >>> 0;
    const el = document.createElement("span");
    el.className = "avatar";
    el.style.setProperty("--avatar", AVATAR_COLORS[h % AVATAR_COLORS.length]);
    el.textContent = ([...n.trim()][0] || "?").toUpperCase();
    return el;
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  const timeFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
  const relFmt = new Intl.RelativeTimeFormat(GoChatI18n.locale, { numeric: "auto" });
  function ago(iso) {
    const t = new Date(iso);
    if (!iso || t.getFullYear() < 2000) return _t("admin.noch_keine_nachrichten");
    const s = (t - Date.now()) / 1000;
    const steps = [[60, "second"], [3600, "minute"], [86400, "hour"], [604800, "day"], [Infinity, "week"]];
    const div = { second: 1, minute: 60, hour: 3600, day: 86400, week: 604800 };
    for (const [lim, unit] of steps) if (Math.abs(s) < lim) return _t("admin.last_ago", { when: relFmt.format(Math.round(s / div[unit]), unit) });
    return "";
  }

  // „zuletzt aktiv vor 5 Minuten“
  function agoActive(iso) {
    const t = new Date(iso);
    if (!iso || t.getFullYear() < 2000) return _t("admin.never_active");
    const s = (t - Date.now()) / 1000;
    const steps = [[60, "second"], [3600, "minute"], [86400, "hour"], [604800, "day"], [Infinity, "week"]];
    const div = { second: 1, minute: 60, hour: 3600, day: 86400, week: 604800 };
    for (const [lim, unit] of steps) if (Math.abs(s) < lim) return _t("admin.last_active", { when: relFmt.format(Math.round(s / div[unit]), unit) });
    return "";
  }

  function toast(text, isError) {
    const t = el("div", "toast", text);
    if (isError) t.style.background = "var(--danger)", t.style.color = "#fff";
    let box = document.querySelector(".toasts");
    if (!box) { box = document.createElement("div"); box.className = "toasts"; box.setAttribute("aria-live", "polite"); document.body.append(box); }
    box.append(t);
    setTimeout(() => t.remove(), 3500);
  }

  function escAttr(s) {
    return String(s).replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  // ---------- Anmeldung ----------

  function showLogin() {
    token = ""; store.set("");
    stopModeration();
    $("adminView").hidden = true;
    $("loginView").hidden = false;
    $("loginPassword").value = "";
    ($("loginUser").value ? $("loginPassword") : $("loginUser")).focus();
  }

  $("loginForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("loginError").textContent = "";
    $("loginSubmit").disabled = true;
    try {
      const data = await api("POST", "/api/admin/login", { username: $("loginUser").value.trim(), password: $("loginPassword").value });
      token = data.token; store.set(token);
      await showAdmin();
    } catch (err) {
      $("loginError").textContent = err.message;
    } finally {
      $("loginSubmit").disabled = false;
    }
  });

  $("logoutBtn").addEventListener("click", async () => {
    try { await api("POST", "/api/admin/logout"); } catch { /* egal */ }
    showLogin();
  });

  let pollTimer = null;
  async function showAdmin() {
    const me = await api("GET", "/api/admin/me");
    actor = me.actor;
    document.body.classList.toggle("role-moderator", !isAdmin());
    $("whoami").replaceChildren(el("b", null, actor.display || actor.name), el("span", null, isAdmin() ? _t("admin.admin") : _t("admin.moderator")));
    $("loginView").hidden = true;
    $("adminView").hidden = false;
    await Promise.all([loadSettings(), loadRooms(), loadQueueCount(), loadSanctionCount(), loadReportCount()]);
    loadAbout();
    route();
    clearInterval(pollTimer);
    pollTimer = setInterval(() => {
      if (!token) return;
      if ($("view-automod").hidden) loadQueueCount();
      loadSanctionCount();
      loadReportCount();
    }, 15000);
  }

  // ---------- Navigation ----------

  const TITLES = {
    uebersicht: _t("admin.uebersicht"), moderation: _t("admin.moderation"), automod: _t("admin.automod"), nutzer: _t("admin.personen"), protokoll: _t("admin.protokoll"), sprachserver: _t("admin.sprachserver"),
    kanaele: _t("admin.kanaele"), allgemein: _t("admin.allgemein"), medien: _t("admin.emotes_gifs"), befehle: _t("admin.befehle"), widgets: _t("admin.widgets"), bots: _t("admin.bots"),
    konten: _t("admin.team_zugaenge"), hosting: _t("admin.hosting"), beacon: _t("admin.beacon"), vorlagen: _t("admin.vorlagen"), sprachen: _t("admin.sprachen"), backups: _t("admin.backups"), transkripte: _t("admin.transkripte"), meldungen: _t("admin.meldungen"), ankuendigungen: _t("admin.ankuendigungen"),
    kanalabos: _t("admin.cf_nav"),
  };
  const ADMIN_VIEWS = new Set(["kanaele", "allgemein", "medien", "befehle", "widgets", "bots", "konten", "hosting", "beacon", "vorlagen", "sprachen", "backups", "ankuendigungen", "transkripte", "kanalabos"]);

  function route() {
    let view = TITLES[location.hash.slice(1)] ? location.hash.slice(1) : (location.hash === "#einbetten" ? "widgets" : "uebersicht");
    if (ADMIN_VIEWS.has(view) && !isAdmin()) view = "uebersicht";
    for (const id of Object.keys(TITLES)) $("view-" + id).hidden = id !== view;
    document.querySelectorAll(".admin-nav a").forEach((a) => {
      if (a.dataset.view === view) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
    });
    const cur = document.querySelector(".admin-nav a[aria-current]");
    if (cur) cur.scrollIntoView({ block: "nearest" }); // in niedrigen Fenstern scrollt nur das Menü
    $("viewTitle").textContent = TITLES[view];
    $("sidebar").classList.remove("open");
    if (view !== "allgemein") discardPreview();
    if (view === "moderation") startModeration(); else stopModeration();
    if (view === "automod") loadAutomod(); else stopQueuePolling();
    if (view === "bots") startBots(); else stopBots();
    if (view === "kanaele") loadRooms();
    if (view === "uebersicht") loadStats();
    if (view === "nutzer") loadPeople();
    if (view === "protokoll") loadAudit(true);
    if (view === "widgets") loadWidgets(); else closeWidgetEditor();
    if (view === "konten") loadAccounts();
    if (view === "hosting") loadHosting();
    if (view === "beacon") loadBeacon();
    if (view === "kanalabos") loadChanFed();
    if (view === "vorlagen") loadTemplates();
    if (view === "backups") loadBackups();
    if (view === "transkripte") loadTranscripts();
    if (view === "sprachserver") startVoiceDash(); else stopVoiceDash();
    if (view === "sprachen") loadLanguages();
    if (view === "medien") loadMediaAdmin();
    if (view === "befehle") loadCommandsAdmin();
    if (view === "meldungen") loadReports();
    if (view === "ankuendigungen") loadSchedules();
    if (view === "automod") loadFilter();
  }
  window.addEventListener("hashchange", route);
  $("menuBtn").addEventListener("click", () => $("sidebar").classList.toggle("open"));
  $("scrim").addEventListener("click", () => $("sidebar").classList.remove("open"));

  // ---------- Allgemein ----------

  // Version, Lizenz und Quellcode (AGPL: wer eine geänderte Fassung betreibt, muss den Quellcode anbieten)
  async function loadAbout() {
    let pub;
    try { pub = await (await fetch("/api/settings")).json(); } catch { return; }
    const a = pub.about || {};
    const line = $("aboutLine");
    line.replaceChildren(el("span", null, `UCNG ${a.version || ""}`.trim()));
    line.append(document.createTextNode(" · "));
    if (a.source) {
      const link = el("a", null, _t("admin.quellcode"));
      link.href = a.source; link.target = "_blank"; link.rel = "noopener";
      line.append(link);
    } else {
      const warn = el("span", "about-warn", _t("admin.quellcode_fehlt"));
      warn.title = _t("admin.quellcode_fehlt_hint");
      line.append(warn);
    }
  }

  async function loadSettings() {
    settings = await api("GET", "/api/admin/settings");
    fillSettingsForm();
    $("brandTitle").textContent = settings.title;
    document.title = _t("admin.title_admin", { title: settings.title });
  }

  function fillSettingsForm() {
    $("setTitle").value = settings.title;
    $("setAllowRooms").checked = settings.allowUserRooms;
    fillStartRoom();
    $("setDMs").checked = !settings.noDMs;
    theme = { accent: settings.accent, sidebar: settings.sidebar || "", mode: settings.mode };
    renderTheme();
    $("setPrivacy").value = settings.privacyUrl || "";
    $("setImprint").value = settings.imprintUrl || "";
    const days = settings.retentionDays || 0;
    const known = [...$("setRetention").options].some((o) => o.value === String(days));
    $("setRetention").value = known ? String(days) : "custom";
    $("setRetentionDays").value = days || 60;
    $("setRetentionDays").hidden = known;
    $("setTimezone").value = settings.timezone || "Europe/Berlin";
    $("setHoursOn").checked = !!(settings.hours && settings.hours.enabled);
    $("setClosed").value = (settings.hours && settings.hours.closed) || "";
    renderHours();
    $("policyAny").checked = settings.embedPolicy !== "listed";
    $("policyListed").checked = settings.embedPolicy === "listed";
  }

  // Startkanal: Auswahl aus den Kanälen, die alle sehen dürfen (plus der aktuelle Wert).
  function fillStartRoom(keep) {
    const sel = $("setStartRoom");
    const cur = keep && sel.options.length ? sel.value : settings.startRoom || "";
    const names = rooms.filter((r) => !(r.readRoles && r.readRoles.length)).map((r) => r.name);
    if (cur && !names.includes(cur)) names.unshift(cur);
    const auto = rooms.some((r) => r.name === "lobby") ? "#lobby" : rooms.length ? "#" + rooms[0].name : "–";
    sel.replaceChildren(new Option(_t("admin.startkanal_automatisch", { room: auto }), ""), ...names.map((n) => new Option("#" + n, n)));
    sel.value = cur;
  }

  const WEEKDAYS = [_t("admin.montag"), _t("admin.dienstag"), _t("admin.mittwoch"), _t("admin.donnerstag"), _t("admin.freitag"), _t("admin.samstag"), _t("admin.sonntag")];
  function renderHours() {
    const days = (settings.hours && settings.hours.days) || [];
    $("hoursGrid").replaceChildren(...WEEKDAYS.map((name, i) => {
      const d = days[i] || { open: i < 5, from: "09:00", to: "18:00" };
      const row = el("div", "hours-row");
      const lab = el("label", "check-row");
      const cb = el("input"); cb.type = "checkbox"; cb.checked = d.open; cb.dataset.day = i;
      lab.append(cb, el("span", null, name));
      const from = el("input"); from.type = "time"; from.value = d.from; from.setAttribute("aria-label", _t("admin.x_hours_from", { day: name }));
      const to = el("input"); to.type = "time"; to.value = d.to; to.setAttribute("aria-label", _t("admin.x_hours_to", { day: name }));
      const sync = () => row.classList.toggle("closed", !cb.checked);
      cb.addEventListener("change", sync); sync();
      row.append(lab, from, el("span", "dash", "–"), to);
      return row;
    }));
    $("hoursGrid").classList.toggle("disabled", !$("setHoursOn").checked);
  }
  $("setHoursOn").addEventListener("change", () => $("hoursGrid").classList.toggle("disabled", !$("setHoursOn").checked));
  $("setRetention").addEventListener("change", () => {
    $("setRetentionDays").hidden = $("setRetention").value !== "custom";
    if (!$("setRetentionDays").hidden) $("setRetentionDays").focus();
  });
  try {
    const zones = Intl.supportedValuesOf ? Intl.supportedValuesOf("timeZone") : [];
    $("tzList").replaceChildren(...zones.map((z) => Object.assign(el("option"), { value: z })));
  } catch { /* alter Browser */ }

  function settingsBody() {
    const ret = $("setRetention").value === "custom" ? parseInt($("setRetentionDays").value, 10) || 0 : parseInt($("setRetention").value, 10);
    const days = [...$("hoursGrid").querySelectorAll(".hours-row")].map((row) => {
      const [from, to] = row.querySelectorAll("input[type=time]");
      return { open: row.querySelector("input[type=checkbox]").checked, from: from.value || "09:00", to: to.value || "18:00" };
    });
    return Object.assign({}, settings, {
      title: $("setTitle").value, allowUserRooms: $("setAllowRooms").checked, noDMs: !$("setDMs").checked,
      startRoom: $("setStartRoom").value,
      accent: theme.accent, sidebar: theme.sidebar, mode: theme.mode,
      privacyUrl: $("setPrivacy").value.trim(), imprintUrl: $("setImprint").value.trim(),
      retentionDays: ret, timezone: $("setTimezone").value.trim() || "Europe/Berlin",
      hours: { enabled: $("setHoursOn").checked, days, closed: $("setClosed").value },
    });
  }

  // --- Farbgestaltung ---

  const ACCENTS = [
    // alle mit weißer Schrift gut lesbar (Kontrast ≥ 4,5:1)
    ["#5848f4", _t("admin.violett")], ["#2563eb", _t("admin.blau")], ["#0f766e", _t("admin.petrol")], ["#15803d", _t("admin.gruen")],
    ["#c2410c", _t("admin.orange")], ["#dc2626", _t("admin.rot")], ["#be185d", _t("admin.pink")], ["#374151", _t("admin.graphit")],
  ];
  const SIDEBARS = [
    ["", _t("admin.standard_dunkles_blau")], ["#111318", _t("admin.schwarz")], ["#1f2937", _t("admin.schiefer")],
    ["#f3f4f6", _t("admin.hellgrau")], ["#ffffff", _t("admin.weiss")],
  ];
  let theme = { accent: "#5848f4", sidebar: "", mode: "auto" };
  let previewing = false;

  function swatch(color, label, checked, onPick, cls) {
    const b = el("button", "swatch" + (cls ? " " + cls : ""));
    b.type = "button";
    b.setAttribute("role", "radio");
    b.setAttribute("aria-checked", String(checked));
    b.setAttribute("aria-label", label);
    b.title = label;
    if (color) {
      b.style.setProperty("--sw", color);
      b.style.setProperty("--sw-on", lum(color) > 0.45 ? "#111321" : "#fff");
    }
    b.addEventListener("click", onPick);
    return b;
  }

  // relative Helligkeit 0..1 (nur für den Haken auf den Farbfeldern)
  function lum(hex) {
    const n = parseInt(hex.slice(1), 16);
    return (0.299 * (n >> 16) + 0.587 * ((n >> 8) & 255) + 0.114 * (n & 255)) / 255;
  }

  function renderTheme() {
    $("accentSwatches").replaceChildren(...ACCENTS.map(([c, l]) =>
      swatch(c, l, theme.accent === c, () => { theme.accent = c; themeChanged(); })));
    $("sidebarSwatches").replaceChildren(...SIDEBARS.map(([c, l]) =>
      swatch(c, l, theme.sidebar === c, () => { theme.sidebar = c; themeChanged(); }, c ? "" : "std")));
    $("setAccent").value = theme.accent;
    $("setAccentText").textContent = theme.accent;
    $("setSidebar").value = theme.sidebar || "#15172e";
    $("setSidebarText").textContent = theme.sidebar || _t("admin.standard");
    $("setMode").value = theme.mode;
  }

  function themeChanged() {
    renderTheme();
    const dirty = theme.accent !== settings.accent || theme.sidebar !== (settings.sidebar || "") || theme.mode !== settings.mode;
    previewing = dirty;
    $("settingsReset").hidden = !dirty;
    const q = new URLSearchParams({ accent: theme.accent, sidebar: theme.sidebar, mode: theme.mode });
    $("themeCSS").href = dirty ? "/theme.css?" + q : "/theme.css?v=" + Date.now();
  }

  function discardPreview() {
    if (!previewing) return;
    theme = { accent: settings.accent, sidebar: settings.sidebar || "", mode: settings.mode };
    themeChanged();
  }

  $("setAccent").addEventListener("input", () => { theme.accent = $("setAccent").value.toLowerCase(); themeChanged(); });
  $("setSidebar").addEventListener("input", () => { theme.sidebar = $("setSidebar").value.toLowerCase(); themeChanged(); });
  $("setMode").addEventListener("change", () => { theme.mode = $("setMode").value; themeChanged(); });
  $("settingsReset").addEventListener("click", discardPreview);

  $("settingsForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("settingsSaved").textContent = "";
    const body = settingsBody();
    if (body.retentionDays && body.retentionDays !== settings.retentionDays && body.retentionDays < (settings.retentionDays || Infinity)) {
      const ok = await confirmPromise(_t("admin.aufbewahrungsfrist_verkuerzen"),
        _t("admin.alle_nachrichten_die_aelter_als_tage", { retentionDays: body.retentionDays }),
        _t("admin.speichern_und_loeschen"));
      if (!ok) return;
    }
    try {
      settings = await api("PUT", "/api/admin/settings", body);
      fillSettingsForm();
      themeChanged();
      $("brandTitle").textContent = settings.title;
      document.title = _t("admin.title_admin", { title: settings.title });
      $("settingsSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("settingsSaved").textContent = ""), 2500);
      renderRooms();
    } catch (err) { toast(err.message, true); }
  });

  // ---------- Kanäle ----------

  const ICON_EDIT = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M12.5 4.5l3 3L7 16H4v-3z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/></svg>';
  const ICON_TRASH = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M4 6h12M8 6V4h4v2M6 6l1 10h6l1-10" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>';

  let layout = { groups: [], ungrouped: [] };
  let voiceSTT = false; // Untertitel/Live-Transkription möglich (STT_URL gesetzt) – aus /api/admin/voice

  async function loadRooms() {
    try {
      let vs, cf;
      [rooms, layout, vs, cf] = await Promise.all([api("GET", "/api/admin/rooms"), api("GET", "/api/admin/layout"),
        api("GET", "/api/admin/voice").catch(() => ({ enabled: false })), api("GET", "/api/admin/channelfed").catch(() => ({ on: false }))]);
      chanFedOn = !!cf.on;
      voiceAvailable = !!vs.enabled;
      voicePeers = vs.rooms || {};
      voiceSTT = !!vs.stt; // sonst nur nach der Übersicht bekannt – „Untertitel“ war beim direkten Aufruf gesperrt
    } catch { return; }
    if (settings.startRoom && !rooms.some((r) => r.name === settings.startRoom)) { // umbenannt oder gelöscht
      try { settings.startRoom = (await api("GET", "/api/admin/settings")).startRoom || ""; } catch { /* egal */ }
      fillStartRoom();
    }
    renderRooms();
    fillRoomSelects();
    fillStartRoom(true);
    if (tempCfg) fillTempForm(); else loadTempRooms();
  }

  // Anordnung speichern (Gruppen, Reihenfolge) und neu zeichnen.
  async function saveLayout() {
    try {
      layout = await api("PUT", "/api/admin/layout", {
        groups: layout.groups.map((g) => ({ id: g.id, name: g.name, rooms: g.rooms })),
        ungrouped: layout.ungrouped,
      });
      rooms = await api("GET", "/api/admin/rooms");
    } catch (err) { toast(err.message, true); }
    renderRooms();
  }

  // Kanal in eine Gruppe (gid "" = ohne) an Position idx verschieben.
  function moveRoom(name, gid, idx) {
    const buckets = [layout.ungrouped, ...layout.groups.map((g) => g.rooms)];
    for (const b of buckets) { const i = b.indexOf(name); if (i >= 0) b.splice(i, 1); }
    const target = gid ? (layout.groups.find((g) => g.id === gid) || {}).rooms : layout.ungrouped;
    if (!target) return;
    target.splice(idx == null || idx > target.length ? target.length : idx, 0, name);
    saveLayout();
  }

  function roomRow(r, gid, list, i) {
    const li = el("li", "room-row drag");
    li.draggable = true;
    li.dataset.room = r.name;
    const handle = el("span", "drag-handle");
    handle.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="7" cy="5" r="1.4"/><circle cx="13" cy="5" r="1.4"/><circle cx="7" cy="10" r="1.4"/><circle cx="13" cy="10" r="1.4"/><circle cx="7" cy="15" r="1.4"/><circle cx="13" cy="15" r="1.4"/></svg>';
    handle.title = _t("admin.ziehen_zum_verschieben");
    const main = el("div", "room-main");
    const strong = el("strong");
    strong.append(el("span", "hash", "#"), r.name);
    main.append(strong);
    main.append(el("p", null, r.description || _t("admin.keine_beschreibung")));
    const stats = el("div", "room-stats");
    if (r.online) stats.append(el("span", "on", `${r.online} online`));
    stats.append(el("span", null, r.messages === 1 ? _t("admin.1_nachricht") : _t("admin.nachrichten", { messages: r.messages })));
    stats.append(el("span", null, ago(r.lastAt)));
    if (r.slowMode) stats.append(el("span", "rule", _t("admin.langsam", { time: r.slowMode >= 60 ? r.slowMode / 60 + " min" : r.slowMode + " s" })));
    if (r.links === "known") stats.append(el("span", "rule", _t("admin.links_ab_24_h")));
    if (r.links === "team") stats.append(el("span", "rule", _t("admin.links_nur_team")));
    if (r.botSlowMode) stats.append(el("span", "rule", _t("admin.bots_langsam", { fmtSecs: fmtSecs(r.botSlowMode) })));
    if (settings.startRoom === r.name) stats.append(el("span", "rule", _t("admin.startkanal")));
    const acc = roomAccessLabel(r);
    if (acc) stats.append(el("span", "rule access", acc));
    if (r.botLinks === "none") stats.append(el("span", "rule", _t("admin.bots_ohne_links")));
    if (r.temp) stats.append(el("span", "rule temp", _t("admin.temp_badge", { min: r.temp.idleMinutes, owner: r.temp.ownerName || (r.temp.by || "").replace(/^bot:.*/, "Bot") })));
    else if (r.createdBy) stats.append(el("span", "rule", _t("admin.bot_created")));
    if (r.password) stats.append(el("span", "rule", _t("admin.room_badge_password")));
    if (r.voice && r.voice.video) stats.append(el("span", "rule voice", _t("admin.room_badge_video")));
    if (r.voice) stats.append(el("span", "rule voice", _t(r.voice.stage ? "admin.voice_badge_stage" : "admin.voice_badge", { n: (voicePeers[r.name] || 0) })));
    if (r.automod && r.automod !== "global") stats.append(el("span", "rule", AM_MODES[r.automod]));
    if (r.share) stats.append(el("span", "rule fed", r.share === "write" ? _t("admin.cf_badge_write") : _t("admin.cf_badge_read")));
    if (r.remote) {
      const b = el("span", "rule fed", "🌐 " + r.remote.address + (r.remote.ended ? " · " + _t("admin.cf_ended") : ""));
      b.title = r.remote.mode === "write" ? _t("admin.cf_mode_write") : _t("admin.cf_mode_read");
      stats.append(b);
    }
    const actions = el("div", "room-actions");
    const btn = (html, label, fn, cls, disabled) => {
      const b = el("button", "icon-action" + (cls ? " " + cls : ""));
      b.type = "button"; b.innerHTML = html; b.setAttribute("aria-label", label); b.title = label; b.disabled = !!disabled;
      b.addEventListener("click", fn);
      return b;
    };
    const sel = el("select", "group-select");
    sel.setAttribute("aria-label", _t("admin.gruppe_von", { name: r.name }));
    sel.title = _t("admin.gruppe");
    sel.append(new Option(_t("admin.ohne_gruppe"), ""), ...layout.groups.map((g) => new Option(g.name, g.id)));
    sel.value = gid;
    sel.addEventListener("change", () => moveRoom(r.name, sel.value, null));
    actions.append(
      sel,
      btn("↑", _t("admin.room_up", { room: r.name }), () => moveRoom(r.name, gid, i - 1), "arrow", i === 0),
      btn("↓", _t("admin.room_down", { room: r.name }), () => moveRoom(r.name, gid, i + 1), "arrow", i === list.length - 1),
      btn(ICON_EDIT, _t("admin.x_edit", { name: "#" + r.name }), () => openRoomDialog(r)),
      btn(ICON_TRASH, _t("admin.room_delete", { room: r.name }), () => openDeleteDialog(r), "danger"),
    );
    main.append(stats);
    li.append(handle, main, actions);
    li.addEventListener("dragstart", (e) => { e.dataTransfer.setData("text/x-gochat-room", r.name); e.dataTransfer.effectAllowed = "move"; li.classList.add("dragging"); });
    li.addEventListener("dragend", () => li.classList.remove("dragging"));
    li.addEventListener("dragover", (e) => {
      if (!e.dataTransfer.types.includes("text/x-gochat-room")) return;
      e.preventDefault();
      const after = e.offsetY > li.offsetHeight / 2;
      li.classList.toggle("drop-before", !after);
      li.classList.toggle("drop-after", after);
    });
    li.addEventListener("dragleave", () => li.classList.remove("drop-before", "drop-after"));
    li.addEventListener("drop", (e) => {
      e.preventDefault(); e.stopPropagation();
      const name = e.dataTransfer.getData("text/x-gochat-room");
      const after = li.classList.contains("drop-after");
      li.classList.remove("drop-before", "drop-after");
      if (!name || name === r.name) return;
      const without = list.filter((n) => n !== name);
      moveRoom(name, gid, without.indexOf(r.name) + (after ? 1 : 0));
    });
    return li;
  }

  function renderRooms() {
    const n = rooms.length;
    $("roomsIntro").textContent = (n === 1 ? _t("admin.1_kanal") : _t("admin.kanaele_2", { n: n })) +
      (settings.allowUserRooms ? _t("admin.besucher_koennen_zusaetzlich_eigene") : _t("admin.besucher_koennen_keine_eigenen_kanaele")) +
      _t("admin.ziehe_kanaele_um_reihenfolge_und_gruppe");
    const box = $("roomTable");
    if (!n) {
      box.replaceChildren(el("li", "empty-row", _t("admin.noch_keine_kanaele_leg_den_ersten_an")));
      return;
    }
    const byName = new Map(rooms.map((r) => [r.name, r]));
    const section = (gid, title, names, gi) => {
      const sec = el("li", "room-section");
      sec.dataset.group = gid;
      const head = el("div", "section-head");
      head.append(el("span", "section-title", title), el("span", "section-count", String(names.length)));
      if (gid) {
        const g = layout.groups[gi];
        const mk = (txt, label, fn, cls, dis) => {
          const b = el("button", "icon-action small" + (cls ? " " + cls : ""), txt);
          b.type = "button"; b.title = label; b.setAttribute("aria-label", label); b.disabled = !!dis;
          b.addEventListener("click", fn);
          return b;
        };
        const tools = el("div", "section-tools");
        tools.append(
          mk("↑", _t("admin.gruppe_nach_oben", { name: g.name }), () => { layout.groups.splice(gi - 1, 0, layout.groups.splice(gi, 1)[0]); saveLayout(); }, "arrow", gi === 0),
          mk("↓", _t("admin.gruppe_nach_unten", { name: g.name }), () => { layout.groups.splice(gi + 1, 0, layout.groups.splice(gi, 1)[0]); saveLayout(); }, "arrow", gi === layout.groups.length - 1),
          mk("", _t("admin.gruppe_umbenennen", { name: g.name }), () => {
            const name = prompt(_t("admin.name_der_gruppe"), g.name);
            if (name && name.trim()) { g.name = name.trim(); saveLayout(); }
          }),
          mk("", _t("admin.gruppe_loeschen", { name: g.name }), () => {
            if (!confirm(_t("admin.gruppe_loeschen_die_kanaele_bleiben", { name: g.name }))) return;
            layout.ungrouped.push(...g.rooms);
            layout.groups.splice(gi, 1);
            saveLayout();
          }, "danger"),
        );
        tools.children[2].innerHTML = ICON_EDIT;
        tools.children[3].innerHTML = ICON_TRASH;
        head.append(tools);
      }
      sec.append(head);
      const ul = el("ul", "room-table");
      names.forEach((nm, i) => { const r = byName.get(nm); if (r) ul.append(roomRow(r, gid, names, i)); });
      if (!names.length) ul.append(el("li", "drop-empty", gid ? _t("admin.kanaele_hierher_ziehen") : _t("admin.kanaele_ohne_gruppe_stehen_im_chat_ganz")));
      // auf den Abschnitt fallen lassen = ans Ende
      sec.addEventListener("dragover", (e) => { if (e.dataTransfer.types.includes("text/x-gochat-room")) { e.preventDefault(); sec.classList.add("drop-into"); } });
      sec.addEventListener("dragleave", (e) => { if (!sec.contains(e.relatedTarget)) sec.classList.remove("drop-into"); });
      sec.addEventListener("drop", (e) => {
        e.preventDefault();
        sec.classList.remove("drop-into");
        const name = e.dataTransfer.getData("text/x-gochat-room");
        if (name) moveRoom(name, gid, null);
      });
      sec.append(ul);
      return sec;
    };
    box.replaceChildren(section("", _t("admin.ohne_gruppe"), layout.ungrouped, -1), ...layout.groups.map((g, i) => section(g.id, g.name, g.rooms, i)));
  }

  $("newGroupBtn").addEventListener("click", () => {
    const name = prompt(_t("admin.name_der_neuen_gruppe_z_b_projekte"));
    if (!name || !name.trim()) return;
    layout.groups.push({ id: "", name: name.trim(), rooms: [] });
    saveLayout();
  });

  const fmtSecs = (s) => (s >= 60 ? s / 60 + " min" : s + " s");
  const AM_MODES = { global: _t("admin.automod_global"), both: _t("admin.automod_global_eigene"), own: _t("admin.automod_nur_eigene"), off: _t("admin.automod_aus") };

  // Auswahlfeld auf einen Wert setzen; fehlt die Option (eigener Wert per API), wird sie ergänzt.
  function setSelect(id, value, label) {
    const sel = $(id);
    value = String(value);
    if (![...sel.options].some((o) => o.value === value)) sel.append(Object.assign(el("option", null, label), { value }));
    sel.value = value;
  }

  let editing = null; // null = neuer Kanal
  let chanFedOn = false; // Kanal-Föderation an (Admin → Föderierte Kanäle)
  let voiceAvailable = false; // Sprachkanäle auf diesem Server
  let voicePeers = {}; // Kanal → Teilnehmer gerade

  async function openRoomDialog(r) {
    editing = r || null;
    if (!roleCfg.roles.length) await loadRolesAdmin();
    fillRolePicks("roomReadRoles", r ? r.readRoles || [] : []);
    fillRolePicks("roomWriteRoles", r ? r.writeRoles || [] : []);
    const v = r && r.voice;
    $("roomVoiceOn").checked = !!v;
    $("roomVoicePreset").value = v ? v.preset : "standard";
    $("roomVoiceGuests").value = v ? v.guests : "speak";
    $("roomVoicePeers").value = v ? v.maxPeers : 25;
    $("roomVoiceSpeakers").value = v ? v.maxSpeakers : 4;
    $("roomVoiceStage").checked = !!(v && v.stage);
    fillRolePicks("roomVoiceStageRoles", v ? v.stageRoles || [] : [], true);
    $("roomScreenMode").value = v ? v.screen || "" : "speakers";
    $("roomScreenQuality").value = v && v.screenQuality ? v.screenQuality : "standard";
    $("roomScreenMax").value = v && v.maxScreens ? v.maxScreens : 1;
    fillRolePicks("roomScreenRoles", v ? v.screenRoles || [] : [], true);
    $("roomVideoMode").value = v ? v.video || "" : "";
    $("roomVideoQuality").value = v && v.videoQuality ? v.videoQuality : "hd";
    $("roomVideoFeatured").value = v && v.featured ? v.featured : 1;
    fillRolePicks("roomVideoRoles", v ? v.videoRoles || [] : [], true);
    $("roomVoiceMods").value = v && v.mods ? v.mods : "";
    fillRolePicks("roomModRoles", v ? v.modRoles || [] : [], true);
    $("roomBotMusic").checked = !!(v && v.botMusic);
    $("roomE2EE").checked = !!(v && v.e2ee);
    $("roomCaptions").checked = !!(v && v.captions);
    $("roomPassword").value = "";
    $("roomPasswordClear").checked = false;
    $("roomPasswordClearRow").hidden = !(r && r.password);
    $("roomVoiceSet").hidden = !voiceAvailable;
    // Freigabe für andere Instanzen
    $("roomShare").value = r && r.share ? r.share : "";
    $("roomShareSet").hidden = !!(r && r.remote);
    $("roomShare").disabled = !chanFedOn;
    $("roomShareHint").textContent = chanFedOn ? _t("admin.cf_room_hint") : _t("admin.cf_room_off");
    syncVoiceFields();
    $("roomDialogTitle").textContent = r ? _t("admin.x_edit", { name: "#" + r.name }) : _t("admin.kanal_anlegen");
    $("roomSubmit").textContent = r ? _t("admin.aenderungen_speichern") : _t("admin.kanal_anlegen");
    $("roomName").value = r ? r.name : "";
    $("roomDesc").value = r ? r.description : "";
    const slow = r ? r.slowMode || 0 : 0, botSlow = r ? r.botSlowMode || 0 : 0;
    setSelect("roomSlow", slow, _t("admin.n_seconds", { n: slow }));
    setSelect("roomBotSlow", botSlow, _t("admin.n_seconds", { n: botSlow }));
    $("roomLinks").value = r ? r.links || "all" : "all";
    $("roomBotLinks").value = r ? r.botLinks || "all" : "all";
    const note = $("roomAutomodNote");
    note.hidden = !r;
    if (r) {
      const a = el("a", null, _t("admin.fuer_diesen_kanal_einstellen"));
      a.href = "#automod";
      a.addEventListener("click", (e) => { e.preventDefault(); $("roomDialog").close(); openAutomodFor(r.name); });
      note.replaceChildren(document.createTextNode((AM_MODES[r.automod || "global"] || _t("admin.automod")) + " – "), a);
    }
    $("roomError").textContent = "";
    $("renameWarn").hidden = true;
    $("roomDialog").showModal();
    $("roomName").focus();
  }
  $("newRoomBtn").addEventListener("click", () => openRoomDialog(null));

  // ---------- Temporäre Kanäle ----------

  let tempCfg = null;
  async function loadTempRooms() {
    try { tempCfg = await api("GET", "/api/admin/temprooms"); } catch { return; }
    fillTempForm();
  }
  function fillTempForm() {
    if (!tempCfg) return;
    const c = tempCfg;
    $("tempEnabled").checked = !!c.enabled;
    $("tempIdle").value = c.idleMinutes;
    $("tempMaxTotal").value = c.maxTotal;
    $("tempMaxUser").value = c.maxPerUser;
    const sel = $("tempGroup");
    sel.replaceChildren(new Option(_t("admin.ohne_gruppe"), ""), ...(layout.groups || []).map((g) => new Option(g.name, g.id)));
    sel.value = c.group || "";
    $("tempVoice").value = c.voice ? c.voice.preset : "";
    $("tempScreen").value = c.voice && c.voice.screen === "speakers" ? "speakers" : "";
    $("tempGuests").value = c.voice ? c.voice.guests : "speak";
    $("tempVoiceMore").hidden = !c.voice;
    $("tempError").textContent = "";
  }
  $("tempVoice").addEventListener("change", () => { $("tempVoiceMore").hidden = !$("tempVoice").value; });
  $("tempForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const preset = $("tempVoice").value;
    const body = {
      enabled: $("tempEnabled").checked, idleMinutes: parseInt($("tempIdle").value, 10) || 5,
      maxTotal: parseInt($("tempMaxTotal").value, 10) || 20, maxPerUser: parseInt($("tempMaxUser").value, 10) || 1,
      group: $("tempGroup").value,
      voice: preset ? Object.assign({}, tempCfg && tempCfg.voice, { preset, guests: $("tempGuests").value, screen: $("tempScreen").value }) : null,
    };
    try {
      tempCfg = await api("PUT", "/api/admin/temprooms", body);
      fillTempForm();
      toast(_t("admin.temp_saved"));
    } catch (err) { $("tempError").textContent = err.message; }
  });

  // Sprachkanal: Felder ein-/ausblenden und Bandbreite schätzen
  const VOICE_KBPS = { sparsam: 16, standard: 24, hoch: 40, musik: 96 };
  function syncVoiceFields() {
    const on = $("roomVoiceOn").checked;
    $("roomVoiceOpts").hidden = !on;
    $("roomVoiceStageRow").hidden = !$("roomVoiceStage").checked;
    const scr = $("roomScreenMode").value;
    $("roomScreenQualityRow").hidden = $("roomScreenMore").hidden = !scr;
    $("roomScreenRolesRow").hidden = scr !== "roles";
    const vid = $("roomVideoMode").value;
    $("roomVideoQualityRow").hidden = $("roomVideoMore").hidden = !vid;
    $("roomVideoRolesRow").hidden = vid !== "roles";
    $("roomModRolesRow").hidden = $("roomVoiceMods").value !== "roles";
    const kb = VOICE_KBPS[$("roomVoicePreset").value] || 24;
    const sp = Math.max(1, Math.min(16, parseInt($("roomVoiceSpeakers").value, 10) || 4));
    const peers = Math.max(2, parseInt($("roomVoicePeers").value, 10) || 25);
    const per = 50 * 8 * (72 + sp * (6 + kb * 1000 / 400)) / 1000; // wie voicerelay.EstimateKbps
    $("roomVoiceEstimate").textContent = _t("admin.voice_estimate", { per: Math.round(per), total: (Math.round(per * peers / 100) / 10).toLocaleString(GoChatI18n.locale), peers });
    // Verschlüsselt: keine Musik von Bots, keine Untertitel
    const e2 = $("roomE2EE").checked;
    $("roomBotMusic").disabled = e2; $("roomCaptions").disabled = e2 || !voiceSTT;
    if (e2) { $("roomBotMusic").checked = false; $("roomCaptions").checked = false; }
    $("roomCaptionsHint").textContent = voiceSTT ? _t("admin.voice_captions_hint") : _t("admin.voice_captions_nostt");
    if (scr) {
      const sk = { text: 3, standard: 4.5, motion: 8, high: 12, max: 16 }[$("roomScreenQuality").value] || 4.5;
      const hints = { text: _t("admin.screen_q_hint_text"), standard: _t("admin.screen_q_hint_standard"), motion: _t("admin.screen_q_hint_motion"),
        high: _t("admin.screen_q_hint_high"), max: _t("admin.screen_q_hint_max") };
      $("roomScreenQualityHint").textContent = hints[$("roomScreenQuality").value] || "";
      $("roomVoiceEstimate").textContent += " " + _t("admin.screen_estimate", { mbit: sk.toLocaleString(GoChatI18n.locale), total: (Math.round(sk * (peers - 1) * 10) / 10).toLocaleString(GoChatI18n.locale) });
    }
  }
  ["roomVoiceOn", "roomVoicePreset", "roomVoicePeers", "roomVoiceSpeakers", "roomVoiceStage", "roomScreenMode", "roomScreenQuality", "roomVideoMode"].forEach((id) => $(id).addEventListener("input", syncVoiceFields));
  ["roomVoiceOn", "roomVoiceStage", "roomScreenMode", "roomScreenQuality", "roomVideoMode", "roomE2EE", "roomVoiceMods"].forEach((id) => $(id).addEventListener("change", syncVoiceFields));
  $("roomName").addEventListener("input", () => {
    $("renameWarn").hidden = !editing || $("roomName").value.trim().toLowerCase() === editing.name;
  });

  $("roomForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("roomError").textContent = "";
    const body = { name: $("roomName").value.trim(), description: $("roomDesc").value,
      slowMode: parseInt($("roomSlow").value, 10), links: $("roomLinks").value,
      botSlowMode: parseInt($("roomBotSlow").value, 10), botLinks: $("roomBotLinks").value,
      readRoles: pickedRoles("roomReadRoles"), writeRoles: pickedRoles("roomWriteRoles") };
    if (voiceAvailable) {
      body.voice = $("roomVoiceOn").checked ? {
        preset: $("roomVoicePreset").value, guests: $("roomVoiceGuests").value,
        maxPeers: parseInt($("roomVoicePeers").value, 10) || 25, maxSpeakers: parseInt($("roomVoiceSpeakers").value, 10) || 4,
        stage: $("roomVoiceStage").checked, stageRoles: pickedRoles("roomVoiceStageRoles"),
        screen: $("roomScreenMode").value, screenQuality: $("roomScreenQuality").value,
        maxScreens: parseInt($("roomScreenMax").value, 10) || 1, screenRoles: pickedRoles("roomScreenRoles"),
        video: $("roomVideoMode").value, videoQuality: $("roomVideoQuality").value,
        featured: parseInt($("roomVideoFeatured").value, 10) || 1, videoRoles: pickedRoles("roomVideoRoles"),
        botMusic: $("roomBotMusic").checked && !$("roomE2EE").checked,
        mods: $("roomVoiceMods").value, modRoles: $("roomVoiceMods").value === "roles" ? pickedRoles("roomModRoles") : [],
        e2ee: $("roomE2EE").checked,
        captions: $("roomCaptions").checked && !$("roomE2EE").checked,
      } : null;
    }
    if ($("roomPasswordClear").checked) body.password = "";
    else if ($("roomPassword").value) body.password = $("roomPassword").value;
    try {
      let saved;
      if (editing) saved = await api("PATCH", "/api/admin/rooms/" + encodeURIComponent(editing.name), body);
      else saved = await api("POST", "/api/admin/rooms", body);
      const share = $("roomShare").value;
      if (chanFedOn && !(editing && editing.remote) && share !== ((editing && editing.share) || "")) {
        await api("PUT", "/api/admin/rooms/" + encodeURIComponent((saved && saved.name) || body.name.toLowerCase()) + "/share", { mode: share });
      }
      $("roomDialog").close();
      toast(editing ? _t("admin.kanal_gespeichert") : _t("admin.kanal_angelegt"));
      loadRooms();
    } catch (err) { $("roomError").textContent = err.message; }
  });

  let deleting = null;
  function openDeleteDialog(r) {
    deleting = r;
    $("deleteText").textContent = r.messages
      ? _t("admin.room_delete_with_messages", { room: r.name, n: r.messages })
      : _t("admin.room_delete_empty", { room: r.name });
    $("deleteError").textContent = "";
    $("deleteDialog").showModal();
  }
  $("deleteForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("DELETE", "/api/admin/rooms/" + encodeURIComponent(deleting.name));
      $("deleteDialog").close();
      toast(_t("admin.room_deleted", { room: deleting.name }));
      loadRooms();
    } catch (err) { $("deleteError").textContent = err.message; }
  });

  document.querySelectorAll("[data-close]").forEach((b) =>
    b.addEventListener("click", () => b.closest("dialog").close()));

  function fillRoomSelects() {
    for (const id of ["modRoom", "embRoom", "cmdTestRoom"]) {
      const sel = $(id);
      const prev = sel.value;
      sel.replaceChildren(...rooms.map((r) => {
        const o = el("option", null, "#" + r.name);
        o.value = r.name;
        return o;
      }));
      if (rooms.some((r) => r.name === prev)) sel.value = prev;
    }
    const sc = $("amScope");
    const prev = sc.value;
    sc.replaceChildren(Object.assign(el("option", null, _t("admin.alle_kanaele_globale_regeln")), { value: "" }),
      ...rooms.map((r) => Object.assign(el("option", null, `#${r.name}` + (r.automod && r.automod !== "global" ? ` – ${AM_MODES[r.automod].replace("Automod: ", "").replace("Automod aus", "aus")}` : "")), { value: r.name })));
    if (rooms.some((r) => r.name === prev)) sc.value = prev;
  }

  // ---------- Moderation ----------

  let modES = null;
  let modRoom = "";

  // Live-Stream per fetch mit Authorization-Kopfzeile – das Token steht so nie in der Adresse
  // (EventSource kann keine Kopfzeilen schicken; Adressen landen in Proxy- und Server-Logs).
  // Liefert ein Objekt mit close(), onmessage({data}) und onerror() wie EventSource.
  function authStream(url) {
    const s = { onmessage: null, onerror: null, closed: false, ctl: null };
    s.close = () => { s.closed = true; if (s.ctl) s.ctl.abort(); };
    const run = async () => {
      let wait = 3000;
      while (!s.closed) {
        s.ctl = new AbortController();
        try {
          const res = await fetch(url, { headers: { Authorization: "Bearer " + token, Accept: "text/event-stream" }, signal: s.ctl.signal, cache: "no-store" });
          if (res.status === 401) { s.closed = true; showLogin(); return; }
          if (!res.ok || !res.body) {
            if (res.status === 404) { s.closed = true; if (s.onerror) s.onerror(); return; }
            throw new Error(String(res.status));
          }
          wait = 3000;
          const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
          let buf = "";
          for (;;) {
            const { value, done } = await reader.read();
            if (done) break;
            buf += value;
            let i;
            while ((i = buf.indexOf("\n\n")) >= 0) {
              const block = buf.slice(0, i);
              buf = buf.slice(i + 2);
              const data = block.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5).replace(/^ /, "")).join("\n");
              if (data && s.onmessage && !s.closed) s.onmessage({ data });
            }
          }
        } catch (err) {
          if (s.closed) return;
          if (err && err.message === "429") wait = 15000 + Math.random() * 15000;
        }
        if (s.closed) return;
        if (s.onerror) s.onerror();
        await new Promise((r) => setTimeout(r, wait));
        wait = Math.min(wait * 2, 30000);
      }
    };
    run();
    return s;
  }

  function stopModeration() {
    if (modES) { modES.close(); modES = null; }
  }

  const delFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });

  // Ein Eintrag für Moderation und Automod-Prüfliste.
  // room: Kanal der Nachricht; showRoom: Kanalnamen anzeigen (Prüfliste über alle Kanäle).
  function modItem(m, room, showRoom) {
    room = room || modRoom;
    const auto = m.deleted && m.deletedBy === "automod";
    const li = el("li", "mod-item" + (m.deleted ? " is-deleted" : ""));
    li.dataset.id = m.id;
    const body = el("div");
    const meta = el("div", "meta");
    meta.append(el("b", null, m.name));
    if (m.bot) meta.append(el("span", "bot-badge", _t("admin.bot")));
    if (m.team) meta.append(el("span", "team-badge badge", _t("admin.team")));
    meta.append(el("time", null, timeFmt.format(new Date(m.time))));
    if (showRoom) meta.append(el("span", "mod-room", "#" + room));
    if (m.editedAt) meta.append(el("span", "edit-badge", "bearbeitet"));
    if (m.deleted && m.deletedBy === "self") {
      meta.append(el("span", "del-badge self", _t("admin.selbst_geloescht")));
    } else if (auto) {
      meta.append(el("span", "del-badge auto", m.match ? _t("admin.automod_2", { match: m.match }) : _t("admin.automod")));
      if (m.reviewed) meta.append(el("span", "del-badge", _t("admin.geprueft_bleibt_ausgeblendet")));
    } else if (m.deleted) {
      const when = m.deletedAt ? ` am ${delFmt.format(new Date(m.deletedAt))}` : "";
      const who = m.modBy ? " " + _t(m.deletedBy === "bot" ? "admin.x_by_bot" : "admin.x_by", { name: m.modBy }) : "";
      meta.append(el("span", "del-badge", _t("admin.geloescht") + who + when));
    }
    let text;
    if (m.deleted && m.deletedBy === "self") text = el("div", "text missing", _t("admin.text_auf_wunsch_der_person_entfernt"));
    else if (m.deleted) text = m.original ? el("div", "text", m.original) : m.media ? null : el("div", "text missing", _t("admin.originaltext_nicht_gespeichert_vor_dem"));
    else text = m.text ? el("div", "text", m.text) : null;
    body.append(meta);
    if (m.reply) {
      const q = el("div", "mod-reply");
      q.append(el("b", null, "↪ " + (m.reply.name || "")), document.createTextNode(" " + (m.reply.missing ? _t("admin.nicht_mehr_vorhanden") : m.reply.deleted ? _t("admin.geloescht_2") : m.reply.text)));
      body.append(q);
    }
    if (text) body.append(text);
    if (m.poll) {
      const pl = el("div", "mod-poll");
      pl.append(el("b", null, _t("admin.umfrage") + m.poll.question));
      m.poll.options.forEach((o) => pl.append(el("span", null, `${o.text} (${o.votes.length})`)));
      body.append(pl);
    }
    if (m.unfiltered) body.append(el("div", "mod-unfiltered", _t("admin.ungefiltert") + m.unfiltered));
    if (m.media && m.media.src && !(m.deleted && m.deletedBy === "self")) {
      const img = el("img", "mod-gif");
      img.src = m.media.src; img.alt = m.media.title || "GIF"; img.loading = "lazy";
      img.width = m.media.w; img.height = m.media.h;
      body.append(img);
    }
    if (m.edits && m.edits.length) {
      const d = el("details", "edits");
      d.append(el("summary", null, m.edits.length === 1 ? _t("admin.1_fruehere_fassung") : _t("admin.fruehere_fassungen", { editsCount: m.edits.length })));
      m.edits.forEach((e) => d.append(el("div", "edit-old", e)));
      body.append(d);
    }
    if (m.deleted && m.deletedBy !== "self") {
      body.append(el("div", "del-note", auto
        ? _t("admin.fuer_alle_anderen_sichtbar_als_nachricht")
        : _t("admin.fuer_alle_anderen_sichtbar_als_nachricht_2")));
    }
    li.append(avatar(m.name), body);

    const actions = el("div", "mod-actions");
    const base = `/api/admin/rooms/${encodeURIComponent(room)}/messages/${m.id}`;
    const act = async (btn, method, path, done) => {
      btn.disabled = true;
      try {
        const updated = await api(method, base + path);
        toast(done);
        afterModeration(room, updated, li, showRoom);
      } catch (err) { toast(err.message, true); btn.disabled = false; }
    };

    if (!m.deleted) {
      const del = el("button", "mod-del", _t("admin.loeschen"));
      del.type = "button";
      let armed = null;
      del.addEventListener("click", () => {
        if (!del.classList.contains("confirm")) {
          del.classList.add("confirm"); del.textContent = _t("admin.wirklich_loeschen");
          armed = setTimeout(() => { del.classList.remove("confirm"); del.textContent = _t("admin.loeschen"); }, 3000);
          return;
        }
        clearTimeout(armed);
        act(del, "DELETE", "", _t("admin.nachricht_geloescht"));
      });
      actions.append(del);
    } else {
      const release = el("button", "mod-del release", auto ? _t("admin.freischalten") : _t("admin.wiederherstellen"));
      release.type = "button";
      if ((!m.original && !m.media) || m.deletedBy === "self") release.hidden = true;
      release.addEventListener("click", () => act(release, "POST", "/restore", auto ? _t("admin.nachricht_freigeschaltet") : _t("admin.nachricht_wiederhergestellt")));
      actions.append(release);
      if (auto && !m.reviewed) {
        const keep = el("button", "mod-del", _t("admin.ausgeblendet_lassen"));
        keep.type = "button";
        keep.addEventListener("click", () => act(keep, "POST", "/confirm", _t("admin.bleibt_ausgeblendet")));
        actions.append(keep);
      }
    }
    if (!m.deleted) {
      const pinned = !!(modPins[room] && modPins[room].has(m.id));
      const pin = el("button", "mod-del", pinned ? _t("admin.loesen") : _t("admin.anpinnen"));
      pin.type = "button";
      pin.title = pinned ? _t("admin.nicht_mehr_oben_im_kanal_anzeigen") : _t("admin.oben_im_kanal_anzeigen");
      pin.addEventListener("click", async () => {
        try {
          const list = await api("POST", base + "/pin", { pin: !pinned });
          modPins[room] = new Set(list.map((x) => x.id));
          toast(pinned ? _t("admin.geloest") : _t("admin.angepinnt"));
          li.replaceWith(modItem(m, room, showRoom));
        } catch (err) { toast(err.message, true); }
      });
      actions.append(pin);
    }
    if (m.userId && !m.bot) {
      const person = el("button", "mod-del", _t("admin.person"));
      person.type = "button";
      person.title = _t("admin.stummschalten_ausschliessen_team");
      person.addEventListener("click", () => openUserDialog({ id: m.userId, name: m.name }));
      actions.append(person);
    }
    if (isAdmin()) {
      const purge = el("button", "mod-del purge", _t("admin.endgueltig_loeschen"));
      purge.type = "button";
      purge.title = _t("admin.entfernt_die_nachricht_vollstaendig_auch");
      purge.addEventListener("click", () => confirmAction(_t("admin.nachricht_endgueltig_loeschen"),
        _t("admin.die_nachricht_wird_vollstaendig_entfernt"),
        _t("admin.endgueltig_loeschen"),
        async () => {
          await api("POST", base + "/purge");
          toast(_t("admin.nachricht_endgueltig_geloescht"));
          li.remove();
          loadQueueCount();
        }));
      actions.append(purge);
    }
    li.append(actions);
    return li;
  }

  // Nach einer Moderationsaktion: Eintrag aktualisieren bzw. aus der Prüfliste nehmen.
  function afterModeration(room, updated, li, inQueue) {
    if (inQueue && !(updated.deleted && updated.deletedBy === "automod" && !updated.reviewed)) {
      li.remove();
      if (!$("queueList").children.length) renderQueue([]);
    } else {
      li.replaceWith(modItem(updated, room, inQueue));
    }
    loadQueueCount();
  }

  // Verlauf über die Admin-API laden – nur dort sind die Originaltexte gelöschter Nachrichten enthalten.
  let modOldest = 0;

  async function loadModHistory(room) {
    const msgs = await api("GET", `/api/admin/rooms/${encodeURIComponent(room)}/messages`);
    if (room !== modRoom) return;
    const more = lastHasMore;
    const list = $("modList");
    const items = msgs.slice().reverse().map((m) => modItem(m, room)); // neueste oben
    list.replaceChildren(...(items.length ? items : [el("li", "empty-row", _t("admin.in_diesem_kanal_gibt_es_noch_keine"))]));
    modOldest = msgs.length ? msgs[0].id : 0;
    if (more) list.append(modOlderRow(room));
  }

  // Unten in der Liste: ältere Nachrichten nachladen.
  function modOlderRow(room) {
    const li = el("li", "empty-row older-row");
    const b = el("button", "btn-quiet small", _t("admin.aeltere_nachrichten_laden"));
    b.type = "button";
    b.addEventListener("click", async () => {
      b.disabled = true; b.textContent = _t("admin.lade");
      try {
        const msgs = await api("GET", `/api/admin/rooms/${encodeURIComponent(room)}/messages?before=${modOldest}&limit=200`);
        const more = lastHasMore;
        if (room !== modRoom) return;
        li.replaceWith(...msgs.slice().reverse().map((m) => modItem(m, room)));
        if (msgs.length) modOldest = msgs[0].id;
        if (more) $("modList").append(modOlderRow(room));
      } catch (err) { toast(err.message, true); b.disabled = false; b.textContent = _t("admin.aeltere_nachrichten_laden"); }
    });
    li.append(b);
    return li;
  }

  async function refreshModItem(id) {
    // genau diese eine Nachricht holen – funktioniert auch für weit zurückliegende
    const msgs = await api("GET", `/api/admin/rooms/${encodeURIComponent(modRoom)}/messages?before=${id + 1}&limit=1`);
    const m = msgs.find((x) => x.id === id);
    const old = $("modList").querySelector(`[data-id="${id}"]`);
    if (m && old) old.replaceWith(modItem(m, modRoom));
  }

  function startModeration() {
    const room = $("modRoom").value;
    if (!room) {
      $("modList").replaceChildren(el("li", "empty-row", _t("admin.es_gibt_noch_keine_kanaele")));
      return;
    }
    if (modES && modRoom === room) return;
    stopModeration();
    modRoom = room;
    $("modStatus").textContent = _t("admin.verbinde");
    const list = $("modList");
    modES = authStream(`/api/admin/rooms/${encodeURIComponent(room)}/events`);
    modES.onmessage = (e) => {
      const ev = JSON.parse(e.data);
      if (ev.type === "pins") { modPins[room] = new Set((ev.messages || []).map((m) => m.id)); return; }
      if (ev.type === "history") {
        modPins[room] = new Set((ev.pinned || []).map((m) => m.id));
        if (!modSearching) loadModHistory(room).catch((err) => toast(err.message, true));
      } else if (ev.type === "message") {
        if (modSearching) return;
        list.querySelector(".empty-row")?.remove();
        list.prepend(modItem(ev.message));
        if (ev.message.deletedBy === "automod") { refreshModItem(ev.message.id).catch(() => {}); loadQueueCount(); }
      } else if (ev.type === "update") {
        refreshModItem(ev.message.id).catch(() => {});
      } else if (ev.type === "removed") {
        list.querySelector(`[data-id="${ev.id}"]`)?.remove();
      } else if (ev.type === "user_renamed" || ev.type === "user_updated") {
        if (!modSearching) loadModHistory(room).catch(() => {});
      } else if (ev.type === "room_deleted" || ev.type === "room_renamed") {
        stopModeration();
        loadRooms().then(startModeration);
        return;
      }
      $("modStatus").textContent = _t("admin.live_neue_nachrichten_erscheinen_oben");
    };
    modES.onerror = () => { $("modStatus").textContent = _t("admin.verbindung_unterbrochen_neuer_versuch"); };
  }
  $("modRoom").addEventListener("change", () => { endModSearch(); stopModeration(); startModeration(); });

  // Suche über alle Kanäle (auch Originaltexte gelöschter Nachrichten)
  let modSearching = false;
  let modSearchTimer = null;
  $("modSearch").addEventListener("input", () => {
    clearTimeout(modSearchTimer);
    modSearchTimer = setTimeout(runModSearch, 300);
  });
  async function runModSearch() {
    const q = $("modSearch").value.trim();
    if (q.length < 2) { if (modSearching) endModSearch(true); return; }
    try {
      const hits = await api("GET", "/api/admin/search?q=" + encodeURIComponent(q));
      modSearching = true;
      const head = $("modSearchHead");
      const close = el("button", "text-btn", _t("admin.suche_beenden"));
      close.type = "button";
      close.addEventListener("click", () => endModSearch(true));
      head.replaceChildren(el("span", null, hits.length ? (hits.length === 100 ? _t("admin.search_newest_hits", { n: 100, q: q }) : _t("admin.treffer_fuer_in_allen_kanaelen", { n: hits.length, q: q })) : _t("admin.nichts_gefunden_fuer", { q: q })), close);
      head.hidden = false;
      $("modList").replaceChildren(...hits.map((h) => modItem(h.message, h.room, true)));
    } catch (err) { toast(err.message, true); }
  }
  function endModSearch(reload) {
    if (!modSearching) return;
    modSearching = false;
    $("modSearch").value = "";
    $("modSearchHead").hidden = true;
    if (reload && modRoom) loadModHistory(modRoom).catch(() => {});
  }

  // ---------- Automod ----------

  let queueTimer = null;

  function renderQueue(items) {
    const list = $("queueList");
    if (!items.length) {
      list.replaceChildren(el("li", "empty-row", _t("admin.nichts_zu_pruefen_neue_treffer")));
      return;
    }
    list.replaceChildren(...items.map((q) => modItem(q.message, q.room, true)));
  }

  async function loadQueue() {
    const items = await api("GET", "/api/admin/automod/queue");
    renderQueue(items);
    setQueueCount(items.length);
  }

  function setQueueCount(n) {
    $("queueCount").hidden = !n;
    $("queueCount").textContent = n > 99 ? "99+" : String(n);
    $("queueCount").title = n === 1 ? _t("admin.1_nachricht_zur_pruefung") : _t("admin.nachrichten_zur_pruefung", { n: n });
  }

  async function loadQueueCount() {
    try { setQueueCount((await api("GET", "/api/admin/automod/queue")).length); } catch { /* egal */ }
  }

  function stopQueuePolling() { clearInterval(queueTimer); queueTimer = null; }

  let amLoaded = ""; // Inhalt des Musterfelds beim Laden (ungespeicherte Änderungen erkennen)

  async function loadAutomod() {
    try {
      await loadScope();
      await loadQueue();
    } catch (err) { toast(err.message, true); }
    stopQueuePolling();
    queueTimer = setInterval(() => loadQueue().catch(() => {}), 8000);
  }

  // Regeln für den gewählten Bereich laden: global oder ein Kanal.
  async function loadScope() {
    const room = $("amScope").value;
    $("amProblems").replaceChildren();
    $("amEnabledRow").hidden = !!room;
    $("amRoomModeRow").hidden = !room;
    if (!room) {
      const cfg = await api("GET", "/api/admin/automod");
      $("amEnabled").checked = cfg.enabled;
      $("amPatterns").value = cfg.patterns.join("\n");
      $("amPatternsLabel").textContent = _t("admin.gesperrte_woerter_und_muster");
      $("amScopeHint").textContent = _t("admin.die_globalen_regeln_gelten_in_allen");
    } else {
      const cfg = await api("GET", `/api/admin/rooms/${encodeURIComponent(room)}/automod`);
      $("amRoomMode").value = cfg.mode;
      $("amPatterns").value = cfg.patterns.join("\n");
      $("amPatternsLabel").textContent = _t("admin.eigene_muster_fuer", { room: room });
      $("amScopeHint").textContent = _t("admin.eigene_muster_wirken_nur_in_diesem_kanal");
    }
    amLoaded = $("amPatterns").value;
    syncRoomMode();
    updatePatternCount();
    runTest();
  }

  function syncRoomMode() {
    const room = $("amScope").value;
    const mode = $("amRoomMode").value;
    const unused = room && (mode === "global" || mode === "off");
    $("amPatterns").classList.toggle("muted-area", !!unused);
    $("amPatternsLabel").title = unused ? _t("admin.werden_erst_mit_globale_und_eigene") : "";
  }
  $("amRoomMode").addEventListener("change", syncRoomMode);

  let amScopePrev = "";
  $("amScope").addEventListener("focus", () => { amScopePrev = $("amScope").value; });
  $("amScope").addEventListener("change", () => {
    if ($("amPatterns").value !== amLoaded && !confirm(_t("admin.ungespeicherte_aenderungen_an_den"))) {
      $("amScope").value = amScopePrev;
      return;
    }
    amScopePrev = $("amScope").value;
    loadScope().catch((err) => toast(err.message, true));
  });

  // Aus dem Kanal-Dialog: Automod-Ansicht für einen Kanal öffnen.
  function openAutomodFor(room) {
    location.hash = "#automod";
    $("amScope").value = room;
    amScopePrev = room;
  }

  function patternLines() {
    return $("amPatterns").value.split("\n").map((l) => l.trim()).filter((l) => l && !l.startsWith("#"));
  }
  // Zähler zeigt nur echte Muster, keine Kommentare.
  function updatePatternCount() {
    const n = patternLines().length;
    $("amCount").textContent = n ? `(${n})` : "";
  }
  $("amPatterns").addEventListener("input", updatePatternCount);

  $("automodForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("amProblems").replaceChildren();
    $("amSaved").textContent = "";
    const room = $("amScope").value;
    try {
      const cfg = room
        ? await api("PUT", `/api/admin/rooms/${encodeURIComponent(room)}/automod`, {
          mode: $("amRoomMode").value, patterns: $("amPatterns").value.split("\n"),
        })
        : await api("PUT", "/api/admin/automod", {
          enabled: $("amEnabled").checked, patterns: $("amPatterns").value.split("\n"),
        });
      $("amPatterns").value = cfg.patterns.join("\n");
      amLoaded = $("amPatterns").value;
      if (room) loadRooms();
      updatePatternCount();
      $("amSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("amSaved").textContent = ""), 2500);
      runTest();
    } catch (err) {
      const problems = err.problems || [err.message];
      $("amProblems").replaceChildren(...problems.map((p) => el("li", null, p)));
    }
  });

  let testTimer = null;
  async function runTest() {
    const text = $("amTest").value;
    const box = $("amResult");
    if (!text.trim()) { box.className = "test-result"; box.replaceChildren(); return; }
    try {
      const r = await api("POST", "/api/admin/automod/test", { text, room: $("amScope").value });
      if (text !== $("amTest").value) return; // inzwischen weitergetippt
      box.className = "test-result " + (r.match ? "hit" : "clean");
      const head = r.match ? _t("admin.wuerde_ausgeblendet_muster", { pattern: r.pattern }) : _t("admin.unauffaellig_wuerde_angezeigt");
      const detail = el("small", null, _t("admin.geprueft_als", { text: r.normalized || "–" }));
      box.replaceChildren(document.createTextNode(head), detail);
    } catch (err) { toast(err.message, true); }
  }
  $("amTest").addEventListener("input", () => {
    clearTimeout(testTimer);
    testTimer = setTimeout(runTest, 250);
  });

  // ---------- Bots ----------

  let bots = [];
  let botsTimer = null;
  let editingBot = null;

  const relFmt2 = new Intl.RelativeTimeFormat(GoChatI18n.locale, { numeric: "auto" });
  function rel(iso) {
    const sec = (new Date(iso) - Date.now()) / 1000;
    for (const [lim, unit, div] of [[60, "second", 1], [3600, "minute", 60], [86400, "hour", 3600], [Infinity, "day", 86400]]) {
      if (Math.abs(sec) < lim) return relFmt2.format(Math.round(sec / div), unit);
    }
    return "";
  }

  function startBots() {
    loadBots();
    stopBots();
    botsTimer = setInterval(loadBots, 5000); // Verbindungsstatus aktuell halten
    renderBotHelp();
  }
  function stopBots() { clearInterval(botsTimer); botsTimer = null; }

  async function loadBots() {
    try { bots = await api("GET", "/api/admin/bots"); } catch { return; }
    renderBots();
  }

  function renderBots() {
    const ul = $("botList");
    if (!bots.length) {
      ul.replaceChildren(el("li", "empty-row", _t("admin.noch_keine_bots_leg_den_ersten_an_du")));
      return;
    }
    ul.replaceChildren(...bots.map((b) => {
      const li = el("li", "bot-row" + (b.enabled ? "" : " off"));
      const av = avatar(b.name);
      av.classList.add("bot");
      const main = el("div");
      const title = el("div", "bot-title");
      title.append(el("strong", null, b.name), el("span", "bot-badge", _t("admin.bot")));
      if (!b.enabled) title.append(el("span", "off-badge", "deaktiviert"));
      main.append(title);
      if (b.description) main.append(el("p", "bot-desc", b.description));
      const meta = el("div", "bot-meta");
      meta.append(el("span", null, b.allRooms ? _t("admin.alle_kanaele") : b.rooms.map((r) => "#" + r).join(", ") || _t("admin.keine_kanaele")));
      for (const [txt, cls] of permChips(b.perms || {})) meta.append(el("span", "perm-chip" + (cls ? " " + cls : ""), txt));
      if (b.connected) meta.append(el("span", "live", b.connected === 1 ? _t("admin.live_verbunden") : _t("admin.live_verbindungen", { connected: b.connected })));
      if (b.webhookUrl) {
        const d = b.lastDelivery;
        if (!d) meta.append(el("span", null, _t("admin.webhook_noch_nicht_aufgerufen")));
        else if (d.error) meta.append(el("span", "err", _t("admin.webhook_fehler", { rel: rel(d.at), error: d.error })));
        else meta.append(el("span", "ok", _t("admin.webhook_ok", { status: d.status, rel: rel(d.at) })));
      }
      if (!b.connected && b.lastActive) meta.append(el("span", null, _t("admin.last_active", { when: rel(b.lastActive) })));
      else if (!b.connected && !b.lastActive && !b.webhookUrl) meta.append(el("span", null, _t("admin.noch_nie_verbunden")));
      main.append(meta);

      const actions = el("div", "room-actions");
      const edit = el("button", "icon-action");
      edit.type = "button"; edit.innerHTML = ICON_EDIT; edit.title = _t("admin.bearbeiten");
      edit.setAttribute("aria-label", _t("admin.x_edit", { name: b.name }));
      edit.addEventListener("click", () => openBotDialog(b));
      const del = el("button", "icon-action danger");
      del.type = "button"; del.innerHTML = ICON_TRASH; del.title = _t("admin.loeschen");
      del.setAttribute("aria-label", _t("admin.loeschen_2", { name: b.name }));
      del.addEventListener("click", () => confirmAction(
        _t("admin.loeschen_3", { name: b.name }),
        _t("admin.der_bot_kann_sich_danach_nicht_mehr"),
        _t("admin.bot_loeschen"),
        async () => { await api("DELETE", "/api/admin/bots/" + b.id); toast(_t("admin.geloescht_3", { name: b.name })); loadBots(); }));
      actions.append(edit, del);
      li.append(av, main, actions);
      return li;
    }));
  }

  function renderBotRooms(selected) {
    const box = $("botRooms");
    box.replaceChildren(...rooms.map((r) => {
      const l = el("label");
      const cb = el("input");
      cb.type = "checkbox"; cb.value = r.name; cb.checked = selected.includes(r.name);
      l.append(cb, document.createTextNode("#" + r.name));
      return l;
    }));
    if (!rooms.length) box.append(el("span", null, _t("admin.noch_keine_kanaele_angelegt")));
  }

  // Kurzüberblick der Rechte in der Bot-Liste
  function permChips(p) {
    const out = [];
    if (p.receive === "commands") out.push([_t("admin.empfaengt_nur_befehle")]);
    if (!p.write) out.push([_t("admin.nur_lesen")]);
    const extra = [p.react && "reagiert", p.editOwn && "bearbeitet eigene", p.automodExempt && _t("admin.ohne_automod")].filter(Boolean);
    if (extra.length) out.push([extra.join(", ")]);
    if (p.rooms) out.push([_t("admin.bot_chip_rooms")]);
    if (p.music) out.push([_t("admin.bot_chip_music")]);
    const mod = [p.delete && _t("admin.loeschen_4"), p.mute && "stumm", p.ban && _t("admin.ausschliessen")].filter(Boolean);
    if (mod.length) {
      const max = p.maxMinutes >= 1440 ? `${p.maxMinutes / 1440} T` : p.maxMinutes >= 60 ? `${p.maxMinutes / 60} h` : `${p.maxMinutes} min`;
      out.push([`moderiert: ${mod.join(", ")}` + (p.mute || p.ban ? ` (≤ ${max})` : ""), "mod"]);
    }
    return out;
  }

  // Abhängigkeiten im Dialog: ohne Schreiben kein Bearbeiten, Sperrdauer nur bei Sperrrechten.
  function syncPerms() {
    $("permEdit").disabled = !$("permWrite").checked;
    if (!$("permWrite").checked) $("permEdit").checked = false;
    $("botMaxRow").classList.toggle("disabled", !($("permMute").checked || $("permBan").checked));
  }
  ["permWrite", "permMute", "permBan"].forEach((id) => $(id).addEventListener("change", syncPerms));

  function syncScope() { $("botRooms").classList.toggle("disabled", $("botAll").checked); }
  $("botAll").addEventListener("change", syncScope);
  $("botSome").addEventListener("change", syncScope);

  async function openBotDialog(b) {
    editingBot = b || null;
    await loadRooms();
    $("botDialogTitle").textContent = b ? _t("admin.x_edit", { name: b.name }) : _t("admin.bot_anlegen");
    $("botSubmit").textContent = b ? _t("admin.aenderungen_speichern") : _t("admin.bot_anlegen");
    $("botName").value = b ? b.name : "";
    $("botDesc").value = b ? b.description : "";
    (b && !b.allRooms ? $("botSome") : $("botAll")).checked = true;
    renderBotRooms(b ? b.rooms : []);
    syncScope();
    $("botWebhook").value = b ? b.webhookUrl : "";
    $("botReceiveBots").checked = b ? b.receiveBots : false;
    const p = (b && b.perms) || { receive: "all", history: true, write: true, maxMinutes: 60 };
    (p.receive === "commands" ? $("botRecvCmd") : $("botRecvAll")).checked = true;
    $("permWrite").checked = !!p.write;
    $("permReact").checked = !!p.react;
    $("permEdit").checked = !!p.editOwn;
    $("permAutomod").checked = !!p.automodExempt;
    $("permHistory").checked = !!p.history;
    $("permPresence").checked = !!p.presence;
    $("permDelete").checked = !!p.delete;
    $("permMute").checked = !!p.mute;
    $("permBan").checked = !!p.ban;
    $("permRooms").checked = !!p.rooms;
    $("permMusic").checked = !!p.music;
    $("botRate").value = p.rateLimit || 0;
    setSelect("botMax", p.maxMinutes || 60, _t("admin.n_minutes", { n: p.maxMinutes || 60 }));
    syncPerms();
    $("botEnabledRow").hidden = !b;
    $("botEnabled").checked = b ? b.enabled : true;
    $("botCreds").hidden = !b;
    $("botSecret").textContent = "••••••••••••••••";
    $("botSecret").classList.add("masked");
    $("botSecretShow").textContent = _t("admin.anzeigen");
    $("botTest").hidden = !(b && b.webhookUrl);
    $("botTestResult").textContent = "";
    $("botError").textContent = "";
    $("botDialog").showModal();
    $("botName").focus();
  }
  $("newBotBtn").addEventListener("click", () => openBotDialog(null));

  function botBody() {
    return {
      name: $("botName").value.trim(),
      description: $("botDesc").value,
      allRooms: $("botAll").checked,
      rooms: [...$("botRooms").querySelectorAll("input:checked")].map((c) => c.value),
      webhookUrl: $("botWebhook").value.trim(),
      receiveBots: $("botReceiveBots").checked,
      perms: {
        receive: $("botRecvCmd").checked ? "commands" : "all",
        write: $("permWrite").checked, react: $("permReact").checked, editOwn: $("permEdit").checked,
        automodExempt: $("permAutomod").checked, history: $("permHistory").checked, presence: $("permPresence").checked,
        delete: $("permDelete").checked, mute: $("permMute").checked, ban: $("permBan").checked, rooms: $("permRooms").checked, music: $("permMusic").checked,
        rateLimit: Math.max(0, parseInt($("botRate").value, 10) || 0), maxMinutes: parseInt($("botMax").value, 10),
      },
      enabled: $("botEnabled").checked,
    };
  }

  $("botForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("botError").textContent = "";
    try {
      if (editingBot) {
        await api("PATCH", "/api/admin/bots/" + editingBot.id, botBody());
        $("botDialog").close();
        toast(_t("admin.bot_gespeichert"));
      } else {
        const res = await api("POST", "/api/admin/bots", botBody());
        $("botDialog").close();
        showToken(res.bot, res.token, res.bot.webhookUrl ? res.bot.webhookSecret : "");
      }
      loadBots();
    } catch (err) { $("botError").textContent = err.message; }
  });

  $("botSecretShow").addEventListener("click", () => {
    const masked = $("botSecret").classList.toggle("masked");
    $("botSecret").textContent = masked ? "••••••••••••••••" : editingBot.webhookSecret;
    $("botSecretShow").textContent = masked ? _t("admin.anzeigen") : _t("admin.verbergen");
  });
  $("botSecretCopy").addEventListener("click", () => copyText(editingBot.webhookSecret, _t("admin.webhook_geheimnis_kopiert")));

  $("botTest").addEventListener("click", async () => {
    const line = $("botTestResult");
    line.className = "test-line"; line.textContent = _t("admin.rufe_webhook_auf");
    try {
      const d = await api("POST", `/api/admin/bots/${editingBot.id}/test`);
      line.className = "test-line " + (d.error ? "err" : "ok");
      line.textContent = d.error ? _t("admin.fehler", { error: d.error }) : _t("admin.erreichbar_antwort_nach_ms", { status: d.status, durationMs: d.durationMs });
    } catch (err) { line.className = "test-line err"; line.textContent = err.message; }
  });

  $("botRenew").addEventListener("click", () => {
    const b = editingBot;
    confirmAction(_t("admin.neuen_schluessel_fuer", { name: b.name }),
      _t("admin.der_bisherige_api_schluessel_und_das"),
      _t("admin.neu_erzeugen"),
      async () => {
        const r = await api("POST", `/api/admin/bots/${b.id}/renew`);
        $("botDialog").close();
        showToken(b, r.token, b.webhookUrl ? r.webhookSecret : "");
        loadBots();
      });
  });

  function showToken(b, token, secret) {
    $("tokenTitle").textContent = _t("admin.zugangsdaten_fuer", { name: b.name });
    $("tokenValue").textContent = token;
    $("tokenSecretRow").hidden = !secret;
    $("tokenSecret").textContent = secret;
    const room = b.allRooms ? (rooms[0] ? rooms[0].name : "lobby") : b.rooms[0] || "lobby";
    $("tokenExample").textContent =
      `curl -X POST ${location.origin}/api/bot/rooms/${room}/messages \\\n` +
      `  -H "Authorization: Bearer ${token}" \\\n` +
      `  -H "Content-Type: application/json" \\\n` +
      `  -d '${JSON.stringify({ text: _t("admin.x_hello") })}'`;
    $("tokenDialog").showModal();
  }

  function renderBotHelp() {
    $("helpStream").textContent =
      `# ${_t("admin.x_cmt_receive")}\n` +
      `curl -N ${location.origin}/api/bot/events -H "Authorization: Bearer gcb_…"\n\n` +
      `# ${_t("admin.x_cmt_reply")}\n` +
      `curl -X POST ${location.origin}/api/bot/rooms/lobby/messages \\\n` +
      `  -H "Authorization: Bearer gcb_…" -H "Content-Type: application/json" \\\n` +
      `  -d '${JSON.stringify({ text: _t("admin.x_hello") })}'`;
  }

  async function copyText(text, msg) {
    try { await navigator.clipboard.writeText(text); toast(msg || _t("admin.kopiert")); }
    catch { prompt(_t("admin.kopieren"), text); }
  }
  document.querySelectorAll("[data-copy]").forEach((b) =>
    b.addEventListener("click", () => copyText($(b.dataset.copy).textContent)));

  // Allgemeiner Bestätigungsdialog
  let confirmFn = null;
  function confirmAction(title, text, okLabel, fn, option) {
    $("confirmTitle").textContent = title;
    $("confirmText").textContent = text;
    $("confirmOk").textContent = okLabel;
    $("confirmOptRow").hidden = !option;
    $("confirmOpt").checked = !!(option && option.checked);
    $("confirmOptText").textContent = option ? option.label : "";
    confirmFn = () => fn($("confirmOpt").checked);
    $("confirmDialog").showModal();
  }
  function confirmPromise(title, text, okLabel) {
    return new Promise((resolve) => {
      let done = false;
      confirmAction(title, text, okLabel, async () => { done = true; resolve(true); });
      $("confirmDialog").addEventListener("close", () => { if (!done) resolve(false); }, { once: true });
    });
  }
  $("confirmForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("confirmOk").disabled = true;
    try { await confirmFn(); $("confirmDialog").close(); }
    catch (err) { toast(err.message, true); }
    finally { $("confirmOk").disabled = false; }
  });

  // ---------- Übersicht (Statistik) ----------

  let stats = null;
  let metric = "messages";
  const numFmt = new Intl.NumberFormat(GoChatI18n.locale);
  const dayShort = new Intl.DateTimeFormat(GoChatI18n.locale, { day: "numeric", month: "numeric" });
  const dayLong = new Intl.DateTimeFormat(GoChatI18n.locale, { weekday: "short", day: "numeric", month: "long" });

  async function loadStats() {
    try { stats = await api("GET", "/api/admin/stats?days=30"); } catch (err) { toast(err.message, true); return; }
    try { stats.voice = await api("GET", "/api/admin/voice"); } catch { stats.voice = { enabled: false }; }
    renderStats();
  }

  function kpi(label, value, sub, href) {
    const k = el(href ? "a" : "div", "kpi");
    if (href) k.href = href;
    k.append(el("span", "kpi-label", label), el("strong", null, typeof value === "number" ? numFmt.format(value) : value));
    if (sub) k.append(el("small", null, sub));
    return k;
  }

  function renderStats() {
    const st = stats.stats, days = st.days, today = days[days.length - 1];
    $("kpis").replaceChildren(
      kpi(_t("admin.nachrichten_30_tage"), st.period, _t("admin.x_today_n", { n: today.messages })),
      kpi(_t("admin.personen_heute"), today.users, _t("admin.have_written")),
      kpi(_t("admin.gerade_online"), st.online, _t("admin.x_in_rooms", { n: st.rooms })),
      kpi(_t("admin.zur_pruefung"), st.pending, _t("admin.automod_treffer"), "#automod"),
      kpi(_t("admin.aktive_sperren"), stats.sanctions, _t("admin.stumm_oder_ausgeschlossen"), "#nutzer"),
      kpi(_t("admin.reservierte_namen"), stats.names, _t("admin.nachrichten_gespeichert", { n: st.messages })),
    );
    renderVoiceStats(stats.voice);
    renderChart();
    const max = Math.max(1, ...st.topRooms.map((r) => r.messages));
    $("topRooms").replaceChildren(...(st.topRooms.length ? st.topRooms.map((r) => {
      const li = el("li");
      const bar = el("span", "bar");
      bar.style.setProperty("--w", (r.messages / max) * 100 + "%");
      li.append(el("span", "name", "#" + r.room), bar, el("span", "num", numFmt.format(r.messages)));
      return li;
    }) : [el("li", "empty-row", _t("admin.in_den_letzten_30_tagen_wurde_nichts"))]));
  }

  // Sprach-Server: eingebauter Relay und Agenten
  function renderVoiceStats(v) {
    voiceSTT = !!(v && v.stt);
    $("voicePanel").hidden = !v || !v.enabled;
    if (!v || !v.enabled) return;
    let total = 0;
    for (const r of v.relays) total += r.stats.participants;
    $("voiceSummary").textContent = _t("admin.voice_kpi_sub", { n: total, rooms: Object.keys(v.rooms || {}).length, relays: v.relays.length });
    const mb = (x) => (Math.round(x * 10) / 10).toLocaleString(GoChatI18n.locale);
    $("voiceRelays").replaceChildren(...(v.relays.length ? v.relays.map((r) => {
      const li = el("li", r.healthy ? "" : "down");
      const name = el("span", "name", r.builtin ? _t("admin.voice_builtin") : r.id);
      if (r.url) name.title = r.url;
      const load = Math.max(r.stats.participants / Math.max(1, r.stats.max), r.stats.estimateMbit / Math.max(0.1, r.stats.maxMbit));
      const bar = el("span", "bar");
      bar.style.setProperty("--w", Math.min(100, load * 100) + "%");
      const num = el("span", "num", _t("admin.voice_relay_load", { n: r.stats.participants, max: r.stats.max, mbit: mb(r.stats.mbit), est: mb(r.stats.estimateMbit), maxMbit: mb(r.stats.maxMbit) }));
      li.append(name, bar, num);
      if (r.role === "media") li.append(el("span", "rule voice", _t("admin.voice_relay_media")));
      const streams = Object.values(r.stats.media || {}).reduce((n, m) => n + (m.pub || 0), 0);
      if (streams) li.append(el("span", "rule", _t("admin.voice_relay_streams", { n: streams })));
      if (!r.healthy) li.append(el("span", "rule", _t("admin.voice_relay_down")));
      return li;
    }) : [el("li", "empty-row", _t("admin.voice_no_relay"))]));
    $("voiceRelayHint").textContent = v.agents ? _t("admin.voice_agents_on") : _t("admin.voice_agents_off");
  }

  const METRICS = {
    messages: [_t("admin.nachrichten_pro_tag"), _t("admin.nachrichten_2")],
    users: [_t("admin.verschiedene_personen_pro_tag_die"), _t("admin.personen")],
    automod: [_t("admin.automod_treffer_pro_tag"), _t("admin.treffer")],
  };

  function niceMax(v) {
    if (v <= 4) return 4;
    const p = Math.pow(10, Math.floor(Math.log10(v)));
    for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
    return 10 * p;
  }

  function renderChart() {
    if (!stats) return;
    const days = stats.stats.days;
    const vals = days.map((d) => d[metric]);
    $("chartSub").textContent = METRICS[metric][0] + _t("admin.zeitzone", { timezone: stats.timezone });
    const W = Math.max(340, Math.min(1000, $("chart").clientWidth || 860)), H = W < 600 ? 200 : 240, L = 36, R = 8, T = 12, B = 28;
    const max = niceMax(Math.max(...vals));
    const bw = (W - L - R) / days.length;
    const NS = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(NS, "svg");
    svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", _t("admin.letzte_tage_hoechstens", { metric: METRICS[metric][0], days: days.length, max: max }));
    const mk = (tag, attrs, text) => {
      const n = document.createElementNS(NS, tag);
      for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
      if (text != null) n.textContent = text;
      svg.append(n);
      return n;
    };
    for (let i = 0; i <= 4; i++) {
      const v = (max / 4) * i, y = T + (H - T - B) * (1 - v / max);
      mk("line", { x1: L, x2: W - R, y1: y, y2: y, class: i ? "grid" : "axis" });
      mk("text", { x: L - 8, y: y + 4, class: "tick", "text-anchor": "end" }, numFmt.format(Math.round(v * 10) / 10));
    }
    days.forEach((d, i) => {
      const v = vals[i], h = ((H - T - B) * v) / max;
      const x = L + i * bw + bw * 0.18, w = bw * 0.64;
      const date = new Date(d.day + "T12:00:00");
      const g = document.createElementNS(NS, "g");
      g.setAttribute("class", "bar-g");
      const hit = document.createElementNS(NS, "rect");
      Object.entries({ x: L + i * bw, y: T, width: bw, height: H - T - B, class: "hit" }).forEach(([k, val]) => hit.setAttribute(k, val));
      const bar = document.createElementNS(NS, "rect");
      Object.entries({ x, y: H - B - h, width: w, height: Math.max(h, v ? 2 : 0), rx: Math.min(4, w / 2), class: "bar" + (i === days.length - 1 ? " today" : "") }).forEach(([k, val]) => bar.setAttribute(k, val));
      const title = document.createElementNS(NS, "title");
      title.textContent = `${dayLong.format(date)}: ${numFmt.format(v)} ${METRICS[metric][1]}`;
      g.append(hit, bar, title);
      g.addEventListener("mouseenter", () => showTip(i, x + w / 2, H - B - h));
      g.addEventListener("mouseleave", hideTip);
      svg.append(g);
      const every = W < 600 ? 7 : 5;
      if (i % every === (days.length - 1) % every) mk("text", { x: x + w / 2, y: H - 8, class: "tick", "text-anchor": "middle" }, i === days.length - 1 ? _t("admin.x_today") : dayShort.format(date));
    });
    const tip = el("div", "chart-tip");
    tip.hidden = true;
    $("chart").replaceChildren(svg, tip);
    function showTip(i, cx, cy) {
      const d = days[i];
      tip.replaceChildren(el("b", null, numFmt.format(vals[i]) + " " + METRICS[metric][1]), el("span", null, dayLong.format(new Date(d.day + "T12:00:00"))));
      tip.hidden = false;
      const box = svg.getBoundingClientRect(), scale = box.width / W;
      tip.style.left = Math.min(box.width - 90, Math.max(60, cx * scale)) + "px";
      tip.style.top = cy * scale + "px";
    }
    function hideTip() { tip.hidden = true; }
  }

  document.querySelectorAll(".chart-panel .seg button").forEach((b) => b.addEventListener("click", () => {
    metric = b.dataset.metric;
    document.querySelectorAll(".chart-panel .seg button").forEach((x) => x.setAttribute("aria-selected", String(x === b)));
    renderChart();
  }));
  let chartResize = null;
  window.addEventListener("resize", () => { clearTimeout(chartResize); chartResize = setTimeout(() => { if (!$("view-uebersicht").hidden) renderChart(); }, 150); });

  // ---------- Personen ----------

  const untilFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit" });
  function sanctionLabel(s) {
    const what = s.kind === "mute" ? _t("admin.stummgeschaltet") : _t("admin.ausgeschlossen");
    return what + (s.until ? " " + _t("admin.x_until", { time: untilFmt.format(new Date(s.until)) }) : _t("admin.dauerhaft")) + (s.ipHash ? _t("admin.auch_verbindung") : "");
  }

  async function loadSanctionCount() {
    try {
      const list = await api("GET", "/api/admin/sanctions");
      $("sanctionCount").hidden = !list.length;
      $("sanctionCount").textContent = list.length;
      $("sanctionCount").title = _t("admin.aktive_sperre_n", { listCount: list.length });
      return list;
    } catch { return null; }
  }

  async function loadPeople() {
    const list = await loadSanctionCount();
    if (list) {
      $("sanctionList").replaceChildren(...(list.length ? list.map((s) => {
        const li = el("li", "user-row");
        const main = el("div", "user-main");
        main.append(el("strong", null, s.name), el("span", "chip " + s.kind, sanctionLabel(s)));
        const meta = el("div", "user-meta", _t("admin.von", { by: s.by, date: untilFmt.format(new Date(s.created)) }) + (s.reason ? _t("admin.grund", { reason: s.reason }) : ""));
        main.append(meta);
        const act = el("div", "room-actions");
        const lift = el("button", "btn-quiet small", _t("admin.aufheben"));
        lift.type = "button";
        lift.addEventListener("click", async () => {
          try { await api("DELETE", `/api/admin/users/${encodeURIComponent(s.userId)}/sanction`); toast(_t("admin.sperre_von_aufgehoben", { name: s.name })); loadPeople(); }
          catch (err) { toast(err.message, true); }
        });
        const more = el("button", "btn-quiet small", _t("admin.verwalten"));
        more.type = "button";
        more.addEventListener("click", () => openUserDialog({ id: s.userId, name: s.name }));
        act.append(lift, more);
        li.append(avatar(s.name), main, act);
        return li;
      }) : [el("li", "empty-row small", _t("admin.niemand_ist_gesperrt"))]));
    }
    await loadRolesAdmin();
    loadUsers();
    try {
      const pn = await api("GET", "/api/admin/protected-names");
      $("protectedNames").value = pn.patterns.join("\n");
      $("protectedNames").readOnly = !isAdmin();
    } catch { /* egal */ }
  }

  let userTimer = null;
  $("userSearch").addEventListener("input", () => { clearTimeout(userTimer); userTimer = setTimeout(loadUsers, 250); });

  async function loadUsers() {
    try {
      const q = $("userSearch").value.trim();
      const res = await fetch("/api/admin/users?q=" + encodeURIComponent(q), { headers: { Authorization: "Bearer " + token } });
      if (!res.ok) throw new Error(_t("admin.liste_konnte_nicht_geladen_werden"));
      const list = await res.json();
      const total = +res.headers.get("X-Total") || list.length;
      $("usersInfo").textContent = q
        ? _t("admin.treffer_2", { total: total })
        : (total > list.length ? _t("admin.names_reserved_first", { n: total, shown: list.length }) : _t("admin.names_reserved", { n: total }));
      $("userList").replaceChildren(...(list.length ? list.map(userRow) : [el("li", "empty-row small", q ? _t("admin.kein_name_gefunden") : _t("admin.noch_hat_niemand_einen_namen_gewaehlt"))]));
    } catch (err) { toast(err.message, true); }
  }

  function userRow(u) {
    const li = el("li", "user-row");
    const main = el("div", "user-main");
    main.append(el("strong", null, u.name));
    if (u.team) main.append(el("span", "badge team-badge", _t("admin.team")));
    for (const r of rolesOf(u.roles)) main.append(roleBadge(r));
    if (u.sanction) main.append(el("span", "chip " + u.sanction.kind, sanctionLabel(u.sanction)));
    main.append(el("div", "user-meta", _t("admin.seit", { date: untilFmt.format(new Date(u.created)), active: agoActive(u.lastSeen) })));
    const act = el("div", "room-actions");
    const b = el("button", "btn-quiet small", _t("admin.verwalten"));
    b.type = "button";
    b.addEventListener("click", () => openUserDialog(u));
    act.append(b);
    li.append(avatar(u.name), main, act);
    return li;
  }

  let udUser = null;
  async function openUserDialog(u) {
    let info = { id: u.id, name: u.name, reserved: false };
    try { info = await api("GET", "/api/admin/users/" + encodeURIComponent(u.id)); }
    catch (err) { if (err.status !== 404) { toast(err.message, true); return; } }
    if (!info.name) info.name = u.name;
    udUser = info;
    $("udAvatar").replaceWith(Object.assign(avatar(info.name), { id: "udAvatar" }));
    if (!roleCfg.roles.length) await loadRolesAdmin();
    renderUdRoles(info);
    $("udMeta").textContent = info.reserved
      ? _t("admin.name_reserviert_seit", { date: untilFmt.format(new Date(info.created)), active: agoActive(info.lastSeen) })
      : _t("admin.der_name_ist_nicht_mehr_reserviert_eine");
    const s = info.sanction;
    $("udCurrent").hidden = !s;
    $("udCurrent").textContent = s ? sanctionLabel(s) + (s.reason ? _t("admin.grund_2", { reason: s.reason }) : "") + _t("admin.von_2", { by: s.by }) : "";
    $("udLift").hidden = !s;
    $("udSanctionFields").hidden = info.team;
    $("udSubmit").hidden = info.team;
    if (info.team) { $("udCurrent").hidden = false; $("udCurrent").textContent = _t("admin.personen_mit_team_abzeichen_koennen"); }
    $("udReason").value = "";
    $("udIP").checked = false;
    document.querySelector('input[name="udKind"][value="mute"]').checked = true;
    $("udMinutes").value = "60";
    syncUdKind();
    $("udTeam").textContent = info.team ? _t("admin.team_abzeichen_entziehen") : _t("admin.team_abzeichen_vergeben");
    $("udTeam").disabled = !info.reserved;
    $("udError").textContent = "";
    renderUdAccount(info);
    $("userDialog").showModal();
  }

  // Konto (Modus „hosted“): Status und Aktionen
  function renderUdAccount(info) {
    const a = info.account;
    $("udAccount").hidden = !a;
    if (!a) return;
    const flag = (t, cls) => el("span", "acct-flag" + (cls ? " " + cls : ""), t);
    const flags = [flag(_t("admin.konto"))];
    if (a.email) flags.push(flag(a.email + (a.emailOk ? " ✓" : _t("admin.unbestaetigt")), a.emailOk ? "ok" : "bad"));
    flags.push(flag(a.twoFA ? _t("admin.2fa_aktiv") : _t("admin.ohne_2fa"), a.twoFA ? "ok" : ""));
    flags.push(flag(_t("admin.geraet_e", { sessions: a.sessions })));
    flags.push(flag(_t("admin.wiederherstellungscodes", { recoveryLeft: a.recoveryLeft })));
    if (a.disabled) flags.push(flag("gesperrt", "bad"));
    $("udAcctFlags").replaceChildren(...flags);
    $("udAcctDisable").hidden = a.disabled;
    $("udAcctEnable").hidden = !a.disabled;
    $("udAcct2fa").hidden = !a.twoFA;
    $("udAcctVerify").hidden = !a.email || a.emailOk;
    $("udAcctMail").hidden = !a.email || !a.emailOk;
  }

  document.querySelectorAll("[data-acct]").forEach((b) => b.addEventListener("click", async () => {
    const action = b.dataset.acct;
    const ask = {
      disable: _t("admin.konto_sperren_alle_geraete_werden"),
      reset2fa: _t("admin.zwei_faktor_anmeldung_zuruecksetzen_nur"),
      logout: _t("admin.die_person_auf_allen_geraeten_abmelden"),
    }[action];
    if (ask && !confirm(ask)) return;
    try {
      const info = await api("POST", `/api/admin/users/${encodeURIComponent(udUser.id)}/account`, { action });
      udUser = Object.assign(udUser, info);
      renderUdAccount(udUser);
      toast({ disable: _t("admin.konto_gesperrt"), enable: _t("admin.konto_entsperrt"), reset2fa: _t("admin.2fa_zurueckgesetzt"), verify: _t("admin.e_mail_bestaetigt"), resetmail: _t("admin.e_mail_verschickt"), logout: _t("admin.ueberall_abgemeldet") }[action]);
      if (typeof loadPeople === "function") loadPeople();
    } catch (err) { toast(err.message, true); }
  }));

  // ---------- Rollen ----------

  let roleCfg = { roles: [], counts: {} };
  const ROLE_PERMS = [["rpLinks", "links", _t("admin.links")], ["rpNoSlow", "noSlow", _t("admin.ohne_langsam_modus")], ["rpProtected", "protected", _t("admin.geschuetzte_namen")],
    ["rpPin", "pin", "anpinnen"], ["rpUpload", "upload", _t("admin.bilder")], ["rpDelete", "delete", _t("admin.loeschen_4")], ["rpMute", "mute", "stummschalten"], ["rpTempRooms", "tempRooms", _t("admin.temp_perm_short")]];
  let editingRole = -1;
  const ROLE_COLORS = ["#e53935", "#d81b60", "#8e24aa", "#5e35b1", "#3949ab", "#1e88e5", "#00897b", "#43a047", "#7cb342", "#f9a825", "#fb8c00", "#6d4c41", "#546e7a"];

  async function loadRolesAdmin() {
    try { roleCfg = await api("GET", "/api/admin/roles"); } catch { return; }
    renderRolesAdmin();
  }
  function rolesOf(ids) {
    return (ids || []).map((id) => roleCfg.roles.find((r) => r.id === id)).filter(Boolean);
  }
  function roleBadge(r) {
    const b = el("span", "badge role-badge", r.name);
    b.style.setProperty("--role", r.color);
    return b;
  }

  function renderRolesAdmin() {
    const list = roleCfg.roles;
    $("roleList").replaceChildren(...(list.length ? list.map((r, i) => {
      const li = el("li", "role-row");
      const main = el("div", "role-main");
      main.append(roleBadge(r), el("span", "role-count", (roleCfg.counts[r.id] || 0) === 1 ? _t("admin.1_person") : _t("admin.personen_2", { n: roleCfg.counts[r.id] || 0 })));
      if (r.colorName) main.append(el("span", "role-flag", _t("admin.faerbt_den_namen")));
      if (r.mentionable) main.append(el("span", "role-flag", _t("admin.mentionable")));
      const pl = ROLE_PERMS.filter(([, k]) => r.perms && r.perms[k]).map(([, , label]) => label);
      if (pl.length) main.append(el("span", "role-perms", pl.join(" · ")));
      const act = el("div", "room-actions admin-only");
      const up = el("button", "icon-action", "↑");
      up.type = "button"; up.disabled = i === 0; up.title = _t("admin.nach_oben");
      up.setAttribute("aria-label", _t("admin.nach_oben_2", { name: r.name }));
      up.addEventListener("click", () => moveRole(i, -1));
      const down = el("button", "icon-action", "↓");
      down.type = "button"; down.disabled = i === list.length - 1; down.title = _t("admin.nach_unten");
      down.setAttribute("aria-label", _t("admin.nach_unten_2", { name: r.name }));
      down.addEventListener("click", () => moveRole(i, 1));
      const ed = el("button", "icon-action");
      ed.type = "button"; ed.innerHTML = ICON_EDIT; ed.title = _t("admin.bearbeiten");
      ed.setAttribute("aria-label", _t("admin.x_edit", { name: r.name }));
      ed.addEventListener("click", () => openRoleDialog(i));
      act.append(up, down, ed);
      li.append(main, act);
      return li;
    }) : [el("li", "empty-row small", _t("admin.noch_keine_rollen_lege_z_b_moderator"))]));
    document.querySelectorAll("#roleList .admin-only").forEach((x) => { x.hidden = !isAdmin(); });
  }

  async function saveRoles(list) {
    const out = await api("PUT", "/api/admin/roles", { roles: list });
    await loadRolesAdmin();
    return out;
  }
  async function moveRole(i, d) {
    const list = roleCfg.roles.slice();
    [list[i], list[i + d]] = [list[i + d], list[i]];
    try { await saveRoles(list); } catch (err) { toast(err.message, true); }
  }

  function selectedRoleColor() {
    const c = $("roleColors").querySelector("input[name=roleColor]:checked");
    return c ? (c.value === "custom" ? $("roleCustom").value : c.value) : ROLE_COLORS[5];
  }
  function renderRolePreview() {
    const r = { name: $("roleName").value.trim() || _t("admin.rolle"), color: selectedRoleColor(), colorName: $("roleColorName").checked };
    const name = el("b", null, _t("admin.mia"));
    if (r.colorName) { name.classList.add("role-name"); name.style.setProperty("--role", r.color); }
    $("rolePreview").replaceChildren(avatar(_t("admin.mia")), name, roleBadge(r), el("time", null, "14:05"));
  }

  function openRoleDialog(i) {
    editingRole = i;
    const r = i >= 0 ? roleCfg.roles[i] : { name: "", color: ROLE_COLORS[(roleCfg.roles.length * 3 + 5) % ROLE_COLORS.length], colorName: false };
    $("roleDialogTitle").textContent = i >= 0 ? _t("admin.rolle_bearbeiten", { name: r.name }) : _t("admin.rolle_anlegen");
    $("roleName").value = r.name;
    $("roleColorName").checked = r.colorName;
    $("roleMention").checked = !!r.mentionable;
    const pp = r.perms || {};
    for (const [id, k] of ROLE_PERMS) $(id).checked = !!pp[k];
    const preset = ROLE_COLORS.includes(r.color);
    $("roleColors").replaceChildren(...ROLE_COLORS.map((c) => {
      const l = el("label", "role-swatch");
      l.style.setProperty("--sw", c);
      l.title = c;
      const inp = el("input");
      inp.type = "radio"; inp.name = "roleColor"; inp.value = c; inp.checked = c === r.color;
      inp.setAttribute("aria-label", _t("admin.farbe") + c);
      inp.addEventListener("change", renderRolePreview);
      l.append(inp);
      return l;
    }), (() => {
      const l = el("label", "role-swatch custom");
      l.title = _t("admin.eigene_farbe");
      const inp = el("input");
      inp.type = "radio"; inp.name = "roleColor"; inp.value = "custom"; inp.checked = !preset;
      inp.setAttribute("aria-label", _t("admin.eigene_farbe"));
      const col = el("input");
      col.type = "color"; col.id = "roleCustom"; col.value = preset ? "#888888" : r.color;
      col.setAttribute("aria-label", _t("admin.eigene_farbe_waehlen"));
      col.addEventListener("input", () => { inp.checked = true; renderRolePreview(); });
      inp.addEventListener("change", renderRolePreview);
      l.append(inp, col);
      return l;
    })());
    $("roleDelete").hidden = i < 0;
    $("roleError").textContent = "";
    renderRolePreview();
    $("roleDialog").showModal();
    $("roleName").focus();
  }
  $("newRoleBtn").addEventListener("click", () => openRoleDialog(-1));
  $("roleName").addEventListener("input", () => { $("roleError").textContent = ""; renderRolePreview(); });
  $("roleColorName").addEventListener("change", renderRolePreview);
  $("roleForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const list = roleCfg.roles.slice();
    const perms = {};
    for (const [id, k] of ROLE_PERMS) perms[k] = $(id).checked;
    const r = { id: editingRole >= 0 ? list[editingRole].id : "", name: $("roleName").value, color: selectedRoleColor(), colorName: $("roleColorName").checked,
      mentionable: $("roleMention").checked, perms };
    if (editingRole >= 0) list[editingRole] = r; else list.push(r);
    try {
      await saveRoles(list);
      $("roleDialog").close();
      toast(editingRole >= 0 ? _t("admin.rolle_gespeichert") : _t("admin.rolle_angelegt"));
      loadUsers();
    } catch (err) { $("roleError").textContent = err.message; }
  });
  $("roleDelete").addEventListener("click", () => {
    const r = roleCfg.roles[editingRole];
    const n = roleCfg.counts[r.id] || 0;
    $("roleDialog").close();
    confirmAction(_t("admin.rolle_loeschen", { name: r.name }),
      n ? _t("admin.dieses_abzeichen", { n: n }) : _t("admin.niemand_traegt_diese_rolle_gerade"),
      _t("admin.rolle_loeschen_2"),
      async () => { await saveRoles(roleCfg.roles.filter((x) => x.id !== r.id)); toast(_t("admin.rolle_geloescht")); loadUsers(); });
  });

  function renderUdProfile(info) {
    const hasSomething = !!(info.avatar || info.statusText || info.status);
    $("udProfile").hidden = !hasSomething;
    if (!hasSomething) return;
    const st = { away: "abwesend", busy: _t("admin.beschaeftigt") }[info.status] || "";
    const parts = [st, info.statusText ? "„" + info.statusText + "“" : ""].filter(Boolean);
    $("udStatus").replaceChildren();
    if (info.avatar) { const im = el("img", "ud-avatar"); im.src = info.avatar; im.alt = _t("admin.profilbild"); $("udStatus").append(im); }
    $("udStatus").append(document.createTextNode(parts.length ? _t("admin.status") + parts.join(" · ") : _t("admin.eigenes_profilbild")));
    $("udAvatarRemove").hidden = !(info.avatar || info.statusText);
  }
  $("udAvatarRemove").addEventListener("click", async () => {
    try {
      await api("DELETE", `/api/admin/users/${encodeURIComponent(udUser.id)}/avatar`);
      toast(_t("admin.profilbild_und_statustext_entfernt"));
      udUser.avatar = ""; udUser.statusText = "";
      renderUdProfile(udUser);
    } catch (err) { $("udError").textContent = err.message; }
  });

  function renderUdRoles(info) {
    renderUdProfile(info);
    $("udName").replaceChildren(document.createTextNode(info.name));
    if (info.team) $("udName").append(" ", el("span", "badge team-badge", _t("admin.team")));
    for (const r of rolesOf(info.roles)) $("udName").append(" ", roleBadge(r));
    const box = $("udRolesBox");
    box.hidden = !isAdmin() || !roleCfg.roles.length;
    const mine = new Set(info.roles || []);
    $("udRoles").replaceChildren(...roleCfg.roles.map((r) => {
      const b = el("button", "role-toggle", r.name);
      b.type = "button";
      b.style.setProperty("--role", r.color);
      b.setAttribute("aria-pressed", String(mine.has(r.id)));
      b.disabled = !info.reserved;
      b.addEventListener("click", async () => {
        const next = new Set(mine);
        if (next.has(r.id)) next.delete(r.id); else next.add(r.id);
        b.disabled = true;
        try {
          const u = await api("POST", `/api/admin/users/${encodeURIComponent(info.id)}/roles`, { roles: [...next] });
          info.roles = u.roles;
          udUser.roles = u.roles;
          renderUdRoles(info);
          loadRolesAdmin();
          if (!$("view-nutzer").hidden) loadUsers();
        } catch (err) { $("udError").textContent = err.message; b.disabled = false; }
      });
      return b;
    }));
  }

  function syncUdKind() {
    const ban = document.querySelector('input[name="udKind"]:checked').value === "ban";
    $("udIPRow").hidden = !ban;
    $("udSubmit").textContent = ban ? _t("admin.ausschliessen_2") : _t("admin.stummschalten");
    $("udMinutes").querySelector('option[value="0"]').disabled = !ban;
    if (!ban && $("udMinutes").value === "0") $("udMinutes").value = "1440";
  }
  document.querySelectorAll('input[name="udKind"]').forEach((r) => r.addEventListener("change", syncUdKind));

  async function afterPersonChange() {
    $("userDialog").close();
    if (!$("view-nutzer").hidden) loadPeople(); else loadSanctionCount();
    if (!$("view-moderation").hidden && modRoom) loadModHistory(modRoom).catch(() => {});
  }

  $("sanctionForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("udError").textContent = "";
    const kind = document.querySelector('input[name="udKind"]:checked').value;
    try {
      await api("POST", `/api/admin/users/${encodeURIComponent(udUser.id)}/sanction`, {
        kind, minutes: parseInt($("udMinutes").value, 10), reason: $("udReason").value.trim(),
        ip: kind === "ban" && $("udIP").checked, name: udUser.name,
      });
      toast(kind === "ban" ? `${udUser.name} ausgeschlossen` : `${udUser.name} stummgeschaltet`);
      afterPersonChange();
    } catch (err) { $("udError").textContent = err.message; }
  });
  $("udLift").addEventListener("click", async () => {
    try { await api("DELETE", `/api/admin/users/${encodeURIComponent(udUser.id)}/sanction`); toast(_t("admin.sperre_aufgehoben")); afterPersonChange(); }
    catch (err) { $("udError").textContent = err.message; }
  });
  $("udTeam").addEventListener("click", async () => {
    try {
      await api("POST", `/api/admin/users/${encodeURIComponent(udUser.id)}/team`, { team: !udUser.team });
      toast(udUser.team ? _t("admin.team_abzeichen_entzogen") : _t("admin.traegt_jetzt_das_team_abzeichen", { name: udUser.name }));
      afterPersonChange();
    } catch (err) { $("udError").textContent = err.message; }
  });
  $("udPurge").addEventListener("click", () => {
    const u = udUser;
    $("userDialog").close();
    confirmAction(_t("admin.alle_nachrichten_von_endgueltig_loeschen", { name: u.name }),
      _t("admin.alle_nachrichten_und_reaktionen_dieser"),
      _t("admin.endgueltig_loeschen"),
      async (release) => {
        const r = await api("POST", `/api/admin/users/${encodeURIComponent(u.id)}/purge`, { releaseName: release });
        toast(_t("admin.nachrichten_geloescht", { deleted: r.deleted }) + (r.released ? _t("admin.name_freigegeben") : ""));
        if (!$("view-nutzer").hidden) loadPeople();
        if (!$("view-moderation").hidden && modRoom) loadModHistory(modRoom).catch(() => {});
      },
      { label: _t("admin.auch_den_namen_freigeben_die_person_wird"), checked: true });
  });

  $("protectedForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("protectedProblems").replaceChildren();
    try {
      const r = await api("PUT", "/api/admin/protected-names", { patterns: $("protectedNames").value.split("\n") });
      $("protectedNames").value = r.patterns.join("\n");
      $("protectedSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("protectedSaved").textContent = ""), 2500);
    } catch (err) {
      $("protectedProblems").replaceChildren(...(err.problems || [err.message]).map((p) => el("li", null, p)));
    }
  });

  // ---------- Protokoll ----------

  const ACTIONS = {
    "login": _t("admin.hat_sich_angemeldet"),
    "settings.update": _t("admin.hat_einstellungen_geaendert"),
    "room.create": _t("admin.hat_einen_kanal_angelegt"),
    "room.update": _t("admin.hat_einen_kanal_geaendert"),
    "room.delete": _t("admin.hat_einen_kanal_geloescht"),
    "message.delete": _t("admin.hat_eine_nachricht_geloescht"),
    "message.restore": _t("admin.hat_eine_nachricht_wiederhergestellt"),
    "message.confirm": _t("admin.hat_einen_automod_treffer_bestaetigt"),
    "message.purge": _t("admin.hat_eine_nachricht_endgueltig_geloescht"),
    "automod.update": _t("admin.hat_die_automod_regeln_geaendert"),
    "user.mute": _t("admin.audit_mute"),
    "user.ban": _t("admin.audit_ban"),
    "user.unsanction": _t("admin.hat_eine_sperre_aufgehoben"),
    "user.team.add": _t("admin.hat_das_team_abzeichen_vergeben"),
    "user.team.remove": _t("admin.hat_das_team_abzeichen_entzogen"),
    "user.purge": _t("admin.hat_alle_nachrichten_einer_person"),
    "names.protected": _t("admin.hat_die_geschuetzten_namen_geaendert"),
    "account.create": _t("admin.hat_ein_konto_angelegt"),
    "account.update": _t("admin.hat_ein_konto_geaendert"),
    "account.delete": _t("admin.hat_ein_konto_geloescht"),
    "widget.create": _t("admin.hat_ein_widget_angelegt"),
    "widget.update": _t("admin.hat_ein_widget_geaendert"),
    "widget.delete": _t("admin.hat_ein_widget_geloescht"),
    "backup.create": _t("admin.hat_ein_backup_angelegt"),
    "backup.download": _t("admin.hat_ein_backup_heruntergeladen"),
    "retention": _t("admin.aufbewahrungsfrist_angewendet"),
  };
  const DANGER = /delete|purge|ban|mute/;
  let auditNext = 0;
  let auditTimer = null;
  $("auditSearch").addEventListener("input", () => { clearTimeout(auditTimer); auditTimer = setTimeout(() => loadAudit(true), 250); });

  async function loadAudit(reset) {
    const q = $("auditSearch").value.trim();
    try {
      const r = await api("GET", `/api/admin/audit?q=${encodeURIComponent(q)}&before=${reset ? 0 : auditNext}`);
      const list = $("auditList");
      list.querySelector(".older-row")?.remove();
      const rows = r.entries.map(auditRow);
      if (reset) list.replaceChildren(...(rows.length ? rows : [el("li", "empty-row", q ? _t("admin.keine_passenden_eintraege") : _t("admin.noch_keine_eintraege"))]));
      else list.append(...rows);
      auditNext = r.next;
      if (r.next) {
        const li = el("li", "empty-row older-row");
        const b = el("button", "btn-quiet small", _t("admin.aeltere_eintraege"));
        b.type = "button";
        b.addEventListener("click", () => loadAudit(false));
        li.append(b);
        list.append(li);
      }
    } catch (err) { toast(err.message, true); }
  }

  function auditRow(e) {
    const li = el("li", "audit-row" + (DANGER.test(e.action) ? " danger" : ""));
    const t = el("time", null, untilFmt.format(new Date(e.time)));
    t.dateTime = e.time;
    const main = el("div");
    const line = el("div", "audit-line");
    const who = el("b", e.actor.startsWith("bot:") ? "is-bot" : "", e.display || e.actor);
    line.append(who, document.createTextNode(" " + (ACTIONS[e.action] || e.action)));
    if (e.userName) line.append(document.createTextNode(" · "), el("span", "audit-target", e.userName));
    if (e.room) line.append(document.createTextNode(" · "), el("span", "audit-target", "#" + e.room));
    main.append(line);
    if (e.detail) main.append(el("div", "audit-detail", e.detail));
    li.append(t, main);
    return li;
  }

  // ---------- Konten ----------

  let accounts = [];
  let editingAccount = null;

  async function loadAccounts() {
    try { accounts = await api("GET", "/api/admin/accounts"); } catch (err) { toast(err.message, true); return; }
    $("accountList").replaceChildren(...accounts.map((a) => {
      const li = el("li", "user-row" + (a.disabled ? " off" : ""));
      const main = el("div", "user-main");
      main.append(el("strong", null, a.display || a.name), el("span", "chip role-" + a.role, a.role === "admin" ? _t("admin.admin") : _t("admin.moderator")));
      if (a.disabled) main.append(el("span", "chip", "deaktiviert"));
      main.append(el("div", "user-meta", a.super
        ? _t("admin.haupt_admin_anmeldename_admin_passwort")
        : _t("admin.anmeldename", { name: a.name }) + (a.lastLogin ? _t("admin.last_login", { date: untilFmt.format(new Date(a.lastLogin)) }) : _t("admin.noch_nie_angemeldet"))));
      const act = el("div", "room-actions");
      if (!a.super) {
        const edit = el("button", "icon-action");
        edit.type = "button"; edit.innerHTML = ICON_EDIT; edit.title = _t("admin.bearbeiten");
        edit.setAttribute("aria-label", _t("admin.x_edit", { name: a.name }));
        edit.addEventListener("click", () => openAccountDialog(a));
        const del = el("button", "icon-action danger");
        del.type = "button"; del.innerHTML = ICON_TRASH; del.title = _t("admin.loeschen");
        del.setAttribute("aria-label", _t("admin.loeschen_2", { name: a.name }));
        del.addEventListener("click", () => confirmAction(_t("admin.konto_loeschen", { name: a.name }),
          _t("admin.die_person_kann_sich_nicht_mehr_anmelden"),
          _t("admin.konto_loeschen_2"),
          async () => { await api("DELETE", "/api/admin/accounts/" + encodeURIComponent(a.name)); toast(_t("admin.konto_geloescht")); loadAccounts(); }));
        act.append(edit, del);
      }
      li.append(avatar(a.display || a.name), main, act);
      return li;
    }));
  }

  function openAccountDialog(a) {
    editingAccount = a || null;
    $("accountTitle").textContent = a ? _t("admin.konto_bearbeiten", { name: a.name }) : _t("admin.konto_anlegen");
    $("accSubmit").textContent = a ? _t("admin.speichern") : _t("admin.konto_anlegen");
    $("accName").value = a ? a.name : "";
    $("accName").disabled = !!a;
    $("accDisplay").value = a ? a.display : "";
    $("accRole").value = a ? a.role : "moderator";
    $("accPassword").value = "";
    $("accPassword").required = !a;
    $("accPwLabel").textContent = a ? _t("admin.neues_passwort_leer_unveraendert") : _t("admin.passwort");
    $("accDisabledRow").hidden = !a;
    $("accDisabled").checked = a ? a.disabled : false;
    $("accError").textContent = "";
    $("accountDialog").showModal();
    (a ? $("accDisplay") : $("accName")).focus();
  }
  $("newAccountBtn").addEventListener("click", () => openAccountDialog(null));
  $("accGenerate").addEventListener("click", () => {
    const chars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
    const buf = new Uint32Array(16);
    crypto.getRandomValues(buf);
    $("accPassword").value = [...buf].map((n, i) => (i && i % 4 === 0 ? "-" : "") + chars[n % chars.length]).join("");
  });
  $("accountForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("accError").textContent = "";
    const body = { name: $("accName").value.trim().toLowerCase(), display: $("accDisplay").value.trim(), role: $("accRole").value,
      password: $("accPassword").value, disabled: $("accDisabled").checked };
    try {
      if (editingAccount) await api("PATCH", "/api/admin/accounts/" + encodeURIComponent(editingAccount.name), body);
      else await api("POST", "/api/admin/accounts", body);
      $("accountDialog").close();
      toast(editingAccount ? _t("admin.konto_gespeichert") : _t("admin.konto_angelegt_gib_anmeldename_und", { name: body.name }));
      loadAccounts();
    } catch (err) { $("accError").textContent = err.message; }
  });

  // ---------- Föderierte Kanäle ----------

  let cfData = null;
  function relAgo(iso) { // „vor 5 Minuten“
    const t = new Date(iso);
    if (!iso || t.getFullYear() < 2000) return "";
    const s = (t - Date.now()) / 1000;
    const steps = [[60, "second", 1], [3600, "minute", 60], [86400, "hour", 3600], [604800, "day", 86400], [Infinity, "week", 604800]];
    for (const [lim, unit, d] of steps) if (Math.abs(s) < lim) return relFmt.format(Math.round(s / d), unit);
    return "";
  }
  async function loadChanFed() {
    try { cfData = await api("GET", "/api/admin/channelfed"); } catch (err) { toast(err.message, true); return; }
    const c = cfData.config || {};
    chanFedOn = !!cfData.on;
    $("cfOn").checked = !!c.enabled;
    $("cfOn").disabled = !cfData.publicUrl && !c.enabled;
    $("cfUserSubs").checked = !!c.userSubs;
    $("cfAllow").value = (c.allow || []).join("\n");
    $("cfDeny").value = (c.deny || []).join("\n");
    $("cfHint").textContent = !cfData.publicUrl ? _t("admin.cf_need_url")
      : cfData.on ? _t("admin.cf_addr_hint", { domain: cfData.domain }) : _t("admin.cf_off_hint");
    $("cfError").textContent = "";
    $("cfSubForm").hidden = $("cfFindPanel").hidden = !cfData.on;
    renderShared();
    renderMirrors();
  }
  function renderShared() {
    const ul = $("cfShared");
    if (!cfData.shared.length) { ul.replaceChildren(el("li", "empty-row", cfData.on ? _t("admin.cf_no_shared") : _t("admin.cf_is_off"))); return; }
    ul.replaceChildren(...cfData.shared.map((sh) => {
      const li = el("li", "user-row cf-row");
      const info = el("div", "user-main");
      const t = el("strong", null, "#" + sh.name + "@" + cfData.domain);
      info.append(t, el("small", null, (sh.mode === "write" ? _t("admin.cf_mode_write") : _t("admin.cf_mode_read")) + " · " +
        _t("admin.cf_n_subscribers", { n: sh.subscribers.length })));
      if (sh.subscribers.length) {
        const subs = el("ul", "cf-subs");
        for (const s of sh.subscribers) {
          const row = el("li");
          row.append(el("span", "cf-dom", s.domain), el("small", s.error ? "cf-err" : null, s.error ? "⚠ " + s.error : s.last ? _t("admin.cf_last_delivery", { ago: relAgo(s.last) }) : _t("admin.cf_since", { ago: relAgo(s.since) })));
          const kick = el("button", "btn-quiet small", _t("admin.cf_kick"));
          kick.type = "button";
          kick.addEventListener("click", async () => {
            if (!confirm(_t("admin.cf_kick_confirm", { domain: s.domain, room: sh.name }))) return;
            try { await api("DELETE", `/api/admin/channelfed/shared/${encodeURIComponent(sh.name)}/${encodeURIComponent(s.domain)}`); loadChanFed(); }
            catch (err) { toast(err.message, true); }
          });
          row.append(kick);
          subs.append(row);
        }
        info.append(subs);
      }
      const copy = el("button", "btn-quiet", _t("admin.cf_copy"));
      copy.type = "button";
      copy.addEventListener("click", () => copyText("#" + sh.name + "@" + cfData.domain));
      li.append(info, copy);
      return li;
    }));
  }
  function copyText(t) {
    if (navigator.clipboard) navigator.clipboard.writeText(t).then(() => toast(_t("admin.cf_copied")), () => window.prompt("", t));
    else window.prompt("", t);
  }
  function renderMirrors() {
    const ul = $("cfMirrors");
    if (!cfData.mirrors.length) { ul.replaceChildren(el("li", "empty-row", _t("admin.cf_no_mirrors"))); return; }
    ul.replaceChildren(...cfData.mirrors.map((m) => {
      const li = el("li", "user-row cf-row");
      const info = el("div", "user-main");
      const t = el("strong");
      t.append(el("span", "hash", "#"), m.room, el("span", "cf-addr", " ← " + m.address));
      const bits = [m.mode === "write" ? _t("admin.cf_m_write") : _t("admin.cf_m_read")];
      if (m.title) bits.push(m.title);
      bits.push(m.public ? _t("admin.cf_for_all") : _t("admin.cf_for_n", { n: m.subs || 0 }));
      if (m.ended) bits.push("⚠ " + _t("admin.cf_ended_long"));
      else if (m.lastAt && !m.lastAt.startsWith("0001")) bits.push(_t("admin.cf_last_delivery", { ago: relAgo(m.lastAt) }));
      info.append(t, el("small", null, bits.join(" · ")));
      if (m.users && m.users.length) info.append(el("small", null, _t("admin.cf_personal_by", { names: m.users.slice(0, 10).join(", ") + (m.users.length > 10 ? " …" : "") })));
      const tools = el("div", "row-tools");
      const pub = el("button", "btn-quiet", m.public ? _t("admin.cf_make_private") : _t("admin.cf_make_public"));
      pub.type = "button";
      pub.title = m.public ? _t("admin.cf_make_private_hint") : "";
      pub.addEventListener("click", async () => {
        try { await api("PATCH", "/api/admin/channelfed/mirrors/" + encodeURIComponent(m.room), { public: !m.public }); loadChanFed(); }
        catch (err) { toast(err.message, true); }
      });
      const del = el("button", "btn-quiet danger", _t("admin.cf_unsubscribe"));
      del.type = "button";
      del.addEventListener("click", async () => {
        if (!confirm(_t("admin.cf_unsubscribe_confirm", { room: m.room, address: m.address }))) return;
        try { await api("DELETE", "/api/admin/rooms/" + encodeURIComponent(m.room)); toast(_t("admin.cf_unsubscribed")); loadChanFed(); }
        catch (err) { toast(err.message, true); }
      });
      tools.append(pub, del);
      li.append(info, tools);
      return li;
    }));
  }
  $("cfForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("cfError").textContent = "";
    try {
      await api("PUT", "/api/admin/channelfed", { enabled: $("cfOn").checked, userSubs: $("cfUserSubs").checked,
        allow: lines($("cfAllow").value), deny: lines($("cfDeny").value) });
      $("cfSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("cfSaved").textContent = ""), 2500);
      loadChanFed();
    } catch (err) { $("cfError").textContent = err.message; }
  });
  async function cfSubscribe(address, local, isPublic) {
    $("cfSubError").textContent = "";
    $("cfSubBtn").disabled = true;
    try {
      const r = await api("POST", "/api/admin/channelfed/subscribe", { address, name: local || "", public: isPublic });
      toast(_t("admin.cf_subscribed", { room: r.room }));
      $("cfAddr").value = ""; $("cfLocal").value = "";
      loadChanFed();
      return true;
    } catch (err) { $("cfSubError").textContent = err.message; return false; }
    finally { $("cfSubBtn").disabled = false; }
  }
  $("cfSubForm").addEventListener("submit", (e) => {
    e.preventDefault();
    const a = $("cfAddr").value.trim();
    if (!a) { $("cfAddr").focus(); return; }
    cfSubscribe(a, $("cfLocal").value.trim(), $("cfPublic").checked);
  });
  function renderFound(list, empty) {
    const ul = $("cfFound");
    if (!list.length) { ul.replaceChildren(el("li", "empty-row", empty)); return; }
    ul.replaceChildren(...list.map((c) => {
      const li = el("li", "user-row cf-row");
      const info = el("div", "user-main");
      info.append(el("strong", null, c.address), el("small", null, [c.title && c.title !== c.domain ? c.title : "", c.mode === "write" ? _t("admin.cf_m_write") : _t("admin.cf_m_read"), c.description].filter(Boolean).join(" · ")));
      if (c.room) { li.append(info, el("span", "rule fed", _t("admin.cf_already", { room: c.room }))); return li; }
      const b = el("button", "btn-quiet", _t("admin.cf_subscribe_btn"));
      b.type = "button";
      b.addEventListener("click", async () => { b.disabled = true; if (await cfSubscribe(c.address, "", $("cfPublic").checked)) li.remove(); else b.disabled = false; });
      li.append(info, b);
      return li;
    }));
  }
  $("cfBrowseBtn").addEventListener("click", async () => {
    const d = $("cfBrowseDomain").value.trim();
    if (!d) { $("cfBrowseDomain").focus(); return; }
    $("cfFound").replaceChildren(el("li", "empty-row", _t("admin.cf_loading")));
    try { const r = await api("GET", "/api/admin/channelfed/browse?domain=" + encodeURIComponent(d)); renderFound(r.channels, _t("admin.cf_none_shared_there")); }
    catch (err) { $("cfFound").replaceChildren(el("li", "empty-row error", err.message)); }
  });
  $("cfBrowseDomain").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); $("cfBrowseBtn").click(); } });
  $("cfDirBtn").addEventListener("click", async () => {
    $("cfFound").replaceChildren(el("li", "empty-row", _t("admin.cf_loading")));
    try { const r = await api("GET", "/api/admin/channelfed/directory?q=" + encodeURIComponent($("cfBrowseDomain").value.trim())); renderFound(r.channels, _t("admin.cf_dir_empty")); }
    catch (err) { $("cfFound").replaceChildren(el("li", "empty-row error", err.message)); }
  });

  // ---------- Hosting: Nutzerkonten, E-Mail, Föderation ----------

  let hostCfg = null;
  const lines = (t) => t.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);

  async function loadHosting() {
    let r, fed;
    try { [r, fed] = await Promise.all([api("GET", "/api/admin/hosted"), api("GET", "/api/admin/federation")]); }
    catch (err) { toast(err.message, true); return; }
    hostCfg = r.config;
    const hosted = r.mode === "hosted";
    const st = $("hostStatus");
    st.replaceChildren();
    const rows = [
      [_t("admin.betriebsart"), hosted ? _t("admin.hosted_nutzerkonten_2fa_verschluesselte") : _t("admin.simple_namen_ohne_konto")],
      [_t("admin.speicher"), { files: _t("admin.dateien_im_datenordner"), sqlite: _t("admin.sqlite_datenbank"), mysql: _t("admin.mysql_mariadb") }[r.storage || "files"] || r.storage],
      [_t("admin.oeffentliche_adresse"), r.publicUrl || _t("admin.nicht_gesetzt_public_url")],
    ];
    if (hosted && fed.domain) rows.push([_t("admin.foederations_adresse"), "name@" + fed.domain], [_t("admin.serverschluessel"), fed.fingerprint]);
    const dl = el("dl", "kv");
    for (const [k, v] of rows) dl.append(el("dt", null, k), el("dd", null, v));
    st.append(el("h2", "panel-title", _t("admin.betrieb")), dl);
    if (!hosted) {
      st.append(el("p", "set-hint", _t("admin.nutzerkonten_zwei_faktor_anmeldung")));
    }
    for (const id of ["hostForm", "invitePanel", "fedPanel"]) $(id).hidden = !hosted;
    if (!hosted) return;
    document.querySelectorAll('input[name="hostGuests"]').forEach((i) => (i.checked = i.value === hostCfg.guests));
    $("hostReg").checked = hostCfg.registration;
    $("hostVerify").checked = hostCfg.requireVerify;
    const sm = hostCfg.smtp || {};
    $("smtpHost").value = sm.host || "";
    $("smtpPort").value = sm.port || 587;
    $("smtpSec").value = sm.security || "starttls";
    $("smtpUser").value = sm.user || "";
    $("smtpPass").value = sm.password || "";
    $("smtpFrom").value = sm.from || "";
    const fc = hostCfg.federation || {};
    $("fedOn").checked = !!fc.enabled;
    $("fedOn").disabled = !r.publicUrl;
    $("fedAllow").value = (fc.allow || []).join("\n");
    $("fedDeny").value = (fc.deny || []).join("\n");
    $("hostError").textContent = "";
    renderPeers(fed);
  }

  function renderPeers(fed) {
    const ul = $("fedPeers");
    if (!fed.peers.length) {
      ul.replaceChildren(el("li", "empty-row", fed.enabled ? _t("admin.noch_keine_instanz_hat_mit_dieser") : _t("admin.foederation_ist_aus")));
      return;
    }
    ul.replaceChildren(...fed.peers.map((p) => {
      const li = el("li", "user-row peer-row");
      const info = el("div", "user-main");
      info.append(el("strong", null, p.domain), el("small", null, _t("admin.schluessel_zuletzt", { fingerprint: p.fingerprint, date: timeFmt.format(new Date(p.last)) })));
      const b = el("button", "btn-quiet", _t("admin.schluessel_vergessen"));
      b.type = "button";
      b.title = _t("admin.nur_noetig_wenn_die_instanz_ihren");
      b.addEventListener("click", async () => {
        if (!confirm(_t("admin.gemerkten_schluessel_von_vergessen_nur", { domain: p.domain }))) return;
        try { await api("DELETE", "/api/admin/federation/peers/" + encodeURIComponent(p.domain)); loadHosting(); }
        catch (err) { toast(err.message, true); }
      });
      li.append(info, b);
      return li;
    }));
    if (fed.queued) ul.append(el("li", "empty-row", _t("admin.nachricht_en_warten_auf_zustellung", { queued: fed.queued })));
  }

  $("hostForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("hostError").textContent = "";
    const g = document.querySelector('input[name="hostGuests"]:checked');
    const body = {
      guests: g ? g.value : "read",
      registration: $("hostReg").checked,
      requireVerify: $("hostVerify").checked,
      smtp: { host: $("smtpHost").value.trim(), port: +$("smtpPort").value || 587, security: $("smtpSec").value,
        user: $("smtpUser").value.trim(), password: $("smtpPass").value, from: $("smtpFrom").value.trim() },
      federation: { enabled: $("fedOn").checked, allow: lines($("fedAllow").value), deny: lines($("fedDeny").value) },
    };
    try {
      hostCfg = await api("PUT", "/api/admin/hosted", body);
      $("hostSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("hostSaved").textContent = ""), 2500);
      loadHosting();
    } catch (err) { $("hostError").textContent = err.message; }
  });

  $("smtpTest").addEventListener("click", async () => {
    const to = $("smtpTestTo").value.trim();
    if (!to) { $("smtpTestTo").focus(); return; }
    $("smtpTest").disabled = true;
    try { await api("POST", "/api/admin/hosted/testmail", { to }); toast(_t("admin.test_e_mail_verschickt_speichere_vorher")); }
    catch (err) { toast(err.message, true); }
    finally { $("smtpTest").disabled = false; }
  });

  $("inviteBtn").addEventListener("click", async () => {
    try {
      const r = await api("POST", "/api/admin/invites", { note: $("inviteNote").value.trim() });
      $("inviteLink").textContent = r.link;
      $("inviteOut").hidden = false;
      $("inviteNote").value = "";
    } catch (err) { toast(err.message, true); }
  });
  $("inviteCopy").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText($("inviteLink").textContent); toast(_t("admin.link_kopiert")); }
    catch { toast(_t("admin.bitte_den_link_markieren_und_kopieren"), true); }
  });

  // ---------- Beacon: Verzeichnis und GIF-Sammlungen ----------

  const BC_LANGS = ["de", "en", "fr", "es", "it", "nl", "pl", "pt", "pt-BR", "cs", "da", "sv", "nb", "fi", "hu", "ro", "el", "tr", "ru", "uk", "ar", "he", "fa", "hi", "ja", "ko", "zh", "id", "vi", "th", "eo"];
  const BC_COUNTRIES = ["DE", "AT", "CH", "LI", "LU", "BE", "NL", "FR", "IT", "ES", "PT", "GB", "IE", "DK", "SE", "NO", "FI", "IS", "PL", "CZ", "SK", "HU", "SI", "HR", "RO", "BG", "GR", "TR", "EE", "LV", "LT", "UA", "US", "CA", "MX", "BR", "AR", "CL", "AU", "NZ", "JP", "KR", "CN", "TW", "IN", "ID", "SG", "ZA", "IL", "AE"];
  const langNames = (() => { try { return new Intl.DisplayNames([GoChatI18n.locale], { type: "language" }); } catch { return null; } })();
  const regionNames = (() => { try { return new Intl.DisplayNames([GoChatI18n.locale], { type: "region" }); } catch { return null; } })();
  const langName = (c) => (langNames && langNames.of(c)) || c;
  const regionName = (c) => (regionNames && regionNames.of(c)) || c;
  // Flaggen: Windows zeigt Flaggen-Emojis nur als Buchstaben (DE, AT …). Deshalb Bilder (/flags/xx.svg)
  // und in Auswahllisten das Emoji nur, wenn der Browser es wirklich als Flagge zeichnet.
  const flagEmojiOK = (() => {
    try {
      const c = document.createElement("canvas");
      c.width = c.height = 24;
      const x = c.getContext("2d");
      x.font = "20px sans-serif"; x.textBaseline = "top";
      x.fillText("\u{1F1E9}\u{1F1EA}", 0, 0); // DE: schwarz-rot-gold
      const d = x.getImageData(0, 0, 24, 24).data;
      for (let i = 0; i < d.length; i += 4) if (d[i + 3] > 0 && d[i] > 150 && d[i + 1] < 90 && d[i + 2] < 90) return true;
    } catch { /* Canvas gesperrt */ }
    return false;
  })();
  const flagEmoji = (cc) => (flagEmojiOK && /^[A-Z]{2}$/.test(cc || "") ? String.fromCodePoint(...[...cc].map((c) => 0x1f1a5 + c.charCodeAt(0))) + " " : "");
  function flagImg(cc) {
    const i = document.createElement("img");
    i.className = "flag"; i.alt = ""; i.width = 20; i.height = 15; i.loading = "lazy";
    i.src = "/flags/" + String(cc).toLowerCase() + ".svg";
    i.onerror = () => i.remove();
    return i;
  }

  function fillSelect(sel, codes, nameOf, current, empty) {
    const list = codes.map((c) => [c, nameOf(c)]).sort((a, b) => a[1].localeCompare(b[1], GoChatI18n.locale));
    if (current && !codes.includes(current)) list.unshift([current, nameOf(current)]);
    sel.replaceChildren(new Option(empty, ""), ...list.map(([c, n]) => new Option(sel === $("bcCountry") ? `${flagEmoji(c)}${n}` : `${n} (${c})`, c)));
    sel.value = current || "";
  }

  let beaconSubs = [];

  async function loadBeacon() {
    let r;
    try { r = await api("GET", "/api/admin/beacon"); } catch (err) { toast(err.message, true); return; }
    const s = r.settings;
    $("bcUrl").value = s.url || "";
    $("bcListed").checked = !!s.listed;
    $("bcListed").disabled = !r.domain;
    $("bcSync").disabled = !r.domain;
    fillSelect($("bcLang"), BC_LANGS, langName, s.language || "", _t("admin.keine_angabe"));
    fillSelect($("bcCountry"), BC_COUNTRIES, regionName, s.country || "", _t("admin.keine_angabe"));
    $("bcDesc").value = s.description || "";
    $("bcError").textContent = "";
    $("beaconHint").textContent = r.domain
      ? _t("admin.diese_instanz_meldet_sich_als_der_beacon", { domain: r.domain, publicUrl: r.publicUrl })
      : _t("admin.fuer_das_verzeichnis_braucht_der_server");
    renderBeaconPreview(r.preview);
    renderBeaconStatus(s);
    beaconSubs = s.subscriptions || [];
    renderSubs();
  }

  function renderBeaconPreview(p) {
    const box = $("bcPreview");
    box.replaceChildren(el("b", null, _t("admin.das_wird_uebermittelt")));
    const dl = el("dl", "kv");
    const lang = $("bcLang").value, cc = $("bcCountry").value;
    const rows = [
      [_t("admin.name"), p.name], [_t("admin.adresse"), p.url || "–"], [_t("admin.nutzer"), String(p.users)],
      [_t("admin.sprache"), lang ? langName(lang) : "–"], [_t("admin.land"), cc || "–"],
      [_t("admin.betriebsart"), p.mode + (p.federation ? _t("admin.foederation_an") : "")],
    ];
    for (const [k, v] of rows) {
      const dd = el("dd", "kv-flag");
      if (k === _t("admin.land") && cc) dd.append(flagImg(cc), regionName(cc)); else dd.textContent = v;
      dl.append(el("dt", null, k), dd);
    }
    box.append(dl);
  }

  function renderBeaconStatus(s) {
    const st = $("bcStatus");
    st.className = "beacon-status";
    if (s.lastError) { st.classList.add("bad"); st.textContent = _t("admin.letzte_meldung_fehlgeschlagen") + s.lastError; }
    else if (s.listed && s.wasListed && s.lastOk) {
      st.classList.add("ok");
      const level = { official: _t("admin.offiziell"), recommended: _t("admin.empfohlen"), standard: _t("admin.standard") }[s.trust] || _t("admin.community_vom_beacon_team_nicht_geprueft");
      st.textContent = _t("admin.gelistet_bei_stufe_zuletzt_gemeldet", { wasListed: s.wasListed, level: level, date: timeFmt.format(new Date(s.lastOk)) });
    }
    else if (s.listed) st.textContent = _t("admin.wird_gleich_gemeldet");
    else st.textContent = _t("admin.nicht_im_verzeichnis");
  }

  for (const id of ["bcLang", "bcCountry"]) $(id).addEventListener("change", async () => {
    try { renderBeaconPreview((await api("GET", "/api/admin/beacon")).preview); } catch { /* egal */ }
  });

  $("beaconForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("bcError").textContent = "";
    const body = { url: $("bcUrl").value.trim(), listed: $("bcListed").checked, language: $("bcLang").value,
      country: $("bcCountry").value, description: $("bcDesc").value.trim() };
    if (body.listed && !body.url) { $("bcError").textContent = _t("admin.bitte_die_adresse_des_beacons_angeben"); $("bcUrl").focus(); return; }
    try {
      await api("PUT", "/api/admin/beacon", body);
      const s = await api("POST", "/api/admin/beacon/sync");
      $("bcSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("bcSaved").textContent = ""), 2500);
      renderBeaconStatus(s);
    } catch (err) { $("bcError").textContent = err.message; }
  });

  $("bcSync").addEventListener("click", async () => {
    $("bcSync").disabled = true;
    try { renderBeaconStatus(await api("POST", "/api/admin/beacon/sync")); }
    catch (err) { toast(err.message, true); }
    finally { $("bcSync").disabled = false; }
  });

  async function saveSubs() {
    $("subError").textContent = "";
    try { beaconSubs = await api("PUT", "/api/admin/beacon/subscriptions", beaconSubs); renderSubs(); return true; }
    catch (err) { $("subError").textContent = err.message; return false; }
  }

  // Abo-Editor: Sammlungen eines Beacons laden und zum Auswählen anzeigen.
  async function subEditor(addr, current) {
    const r = await api("GET", "/api/admin/beacon/collections?url=" + encodeURIComponent(addr));
    const box = el("div", "sub-edit");
    if (!r.collections.length) { box.append(el("p", "set-hint", _t("admin.dieser_beacon_hat_noch_keine_sammlungen"))); return { r, box, picked: () => [] }; }
    box.append(el("p", "set-hint", _t("admin.nichts_angehakt_alle_sammlungen_auch")));
    const checks = el("div", "sub-colls");
    const sel = new Set(current || (r.selected ? [r.selected] : []));
    for (const c of r.collections) {
      const lab = el("label", "check-row");
      const cb = document.createElement("input");
      cb.type = "checkbox"; cb.value = c.id; cb.checked = sel.has(c.id);
      const txt = el("span", null, c.name);
      txt.append(el("small", null, _t("admin.gifs", { count: c.count })));
      if (c.description) txt.title = c.description;
      lab.append(cb, txt);
      checks.append(lab);
    }
    box.append(checks);
    return { r, box, picked: () => [...checks.querySelectorAll("input:checked")].map((i) => i.value) };
  }

  $("subAddForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("subError").textContent = "";
    const addr = $("subAddUrl").value.trim();
    if (!addr) { $("subAddUrl").focus(); return; }
    const btn = e.submitter || $("subAddForm").querySelector("button");
    btn.disabled = true;
    let ed;
    try { ed = await subEditor(addr, null); } catch (err) { $("subError").textContent = err.message; return; }
    finally { btn.disabled = false; }
    const existing = beaconSubs.find((s) => s.url === ed.r.url);
    const li = el("li", "sub-item pending");
    const head = el("div", "sub-head");
    head.append(el("strong", null, ed.r.name || ed.r.url), el("small", null, ed.r.url));
    const act = el("div", "panel-actions");
    const cancel = el("button", "btn-quiet small", _t("admin.abbrechen")); cancel.type = "button";
    const ok = el("button", "btn-primary small", existing ? _t("admin.abo_aendern") : _t("admin.abonnieren")); ok.type = "button";
    cancel.addEventListener("click", () => li.remove());
    ok.addEventListener("click", async () => {
      const sub = { url: ed.r.url, name: ed.r.name || "", collections: ed.picked() };
      const prev = beaconSubs;
      beaconSubs = existing ? beaconSubs.map((s) => (s.url === sub.url ? sub : s)) : [...beaconSubs, sub];
      if (await saveSubs()) { $("subAddUrl").value = ""; toast(_t("admin.abonniert")); } else beaconSubs = prev;
    });
    act.append(cancel, ok);
    li.append(head, ed.box, act);
    $("subList").querySelectorAll(".sub-item.pending").forEach((x) => x.remove());
    $("subList").prepend(li);
  });

  function renderSubs() {
    const ul = $("subList");
    if (!beaconSubs.length) { ul.replaceChildren(el("li", "empty-row", _t("admin.noch_keine_sammlungen_abonniert"))); return; }
    ul.replaceChildren(...beaconSubs.map((s) => {
      const li = el("li", "sub-item");
      const head = el("div", "sub-head");
      head.append(el("strong", null, s.name || s.url), el("small", null, s.url));
      const what = el("div", "sub-what", s.collections.length ? _t("admin.sammlung_en", { collectionsCount: s.collections.length }) : _t("admin.alle_sammlungen"));
      const act = el("div", "room-actions");
      const edit = el("button", "btn-quiet small", _t("admin.sammlungen")); edit.type = "button";
      const del = el("button", "btn-quiet small danger-text", _t("admin.entfernen")); del.type = "button";
      del.addEventListener("click", async () => {
        if (!confirm(_t("admin.abo_von_entfernen", { name: s.name || s.url }))) return;
        beaconSubs = beaconSubs.filter((x) => x.url !== s.url);
        saveSubs();
      });
      edit.addEventListener("click", async () => {
        edit.disabled = true;
        let ed;
        try { ed = await subEditor(s.url, s.collections); } catch (err) { $("subError").textContent = err.message; edit.disabled = false; return; }
        const save = el("button", "btn-primary small", _t("admin.uebernehmen")); save.type = "button";
        save.addEventListener("click", () => { s.collections = ed.picked(); saveSubs(); });
        const row = el("div", "panel-actions"); row.append(save);
        li.append(ed.box, row);
      });
      act.append(edit, del);
      const top = el("div", "sub-top"); top.append(head, what, act);
      li.append(top);
      return li;
    }));
  }

  // ---------- Vorlagen: Grundkonfiguration exportieren und importieren ----------

  const TPL_PARTS = [
    ["roles", _t("admin.rollen"), _t("admin.namen_farben_und_rechte")],
    ["channels", _t("admin.kanaele_gruppen"), _t("admin.beschreibungen_regeln_rechte_anordnung")],
    ["automod", _t("admin.automod_wortfilter"), _t("admin.wortlisten_und_muster")],
    ["commands", _t("admin.befehle"), _t("admin.eigene_und_fertige_befehle_bot_name")],
    ["bots", _t("admin.bots"), _t("admin.einstellungen_und_rechte_ohne_schluessel")],
    ["widgets", _t("admin.widgets"), _t("admin.aussehen_kanal_und_erlaubte_domains")],
    ["emotes", _t("admin.emotes"), _t("admin.mit_bildern")],
    ["gifs", _t("admin.gifs_2"), _t("admin.bibliothek_mit_bildern_und_beacon_abos")],
    ["media", _t("admin.medien_einstellungen"), _t("admin.groessen_reaktionen_bilder_ohne_api")],
    ["i18n", _t("admin.tpl_i18n"), _t("admin.tpl_i18n_hint")],
    ["settings", _t("admin.allgemein_design"), _t("admin.titel_farben_oeffnungszeiten_links")],
  ];
  const TPL_UNITS = { roles: _t("admin.rollen"), channels: _t("admin.kanaele"), automod: _t("admin.muster"), commands: _t("admin.befehle"), bots: _t("admin.bots"), widgets: _t("admin.widgets"), emotes: _t("admin.emotes"), gifs: _t("admin.eintraege"), i18n: _t("admin.tpl_i18n_unit") };
  let tplToken = "";

  function tplChecks(box, available, counts, checked) {
    box.replaceChildren(...TPL_PARTS.map(([id, name, hint]) => {
      const ok = !available || available.includes(id);
      const lab = el("label", "check-row" + (ok ? "" : " off"));
      const cb = document.createElement("input");
      cb.type = "checkbox"; cb.value = id; cb.disabled = !ok; cb.checked = ok && checked;
      const txt = el("span", null, name);
      let small = hint;
      if (counts && ok && TPL_UNITS[id] && counts[id] != null) small = `${counts[id]} ${TPL_UNITS[id]} · ${hint}`;
      if (!ok) small = _t("admin.nicht_in_der_vorlage");
      txt.append(el("small", null, small));
      lab.append(cb, txt);
      return lab;
    }));
  }
  const tplPicked = (box) => [...box.querySelectorAll("input:checked")].map((i) => i.value);

  function loadTemplates() {
    if (!$("tplExportParts").childElementCount) tplChecks($("tplExportParts"), null, null, true);
  }

  $("tplAll").addEventListener("click", () => {
    const boxes = [...$("tplExportParts").querySelectorAll("input")];
    const all = boxes.every((b) => b.checked);
    boxes.forEach((b) => (b.checked = !all));
    $("tplAll").textContent = all ? _t("admin.alle_auswaehlen") : _t("admin.keine_auswaehlen");
  });

  $("tplExport").addEventListener("click", async () => {
    const parts = tplPicked($("tplExportParts"));
    if (!parts.length) { toast(_t("admin.bitte_mindestens_einen_teil_auswaehlen"), true); return; }
    const btn = $("tplExport");
    btn.disabled = true;
    try {
      const res = await fetch("/api/admin/template?parts=" + parts.join(","), { headers: { Authorization: "Bearer " + token } });
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || _t("admin.export_fehlgeschlagen"));
      const name = (/filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") || "") || [])[1] || "gochat-vorlage.zip";
      const blob = await res.blob();
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = name;
      document.body.append(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 2000);
    } catch (err) { toast(err.message, true); }
    finally { btn.disabled = false; }
  });

  $("tplInspect").addEventListener("click", async () => {
    $("tplError").textContent = "";
    $("tplResult").hidden = true;
    const f = $("tplFile").files[0];
    if (!f) { $("tplFile").focus(); return; }
    const fd = new FormData();
    fd.append("file", f);
    $("tplInspect").disabled = true;
    try {
      const res = await fetch("/api/admin/template/inspect", { method: "POST", headers: { Authorization: "Bearer " + token }, body: fd });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data.error || _t("admin.die_vorlage_konnte_nicht_gelesen_werden"));
      tplToken = data.token;
      const m = data.manifest;
      $("tplInfo").textContent = _t("admin.vorlage_aus_vom_waehle_was_uebernommen", { source: m.source || _t("admin.unknown"), date: timeFmt.format(new Date(m.created)) });
      tplChecks($("tplImportParts"), m.parts, m.counts, true);
      $("tplImportBox").hidden = false;
    } catch (err) { $("tplError").textContent = err.message; $("tplImportBox").hidden = true; }
    finally { $("tplInspect").disabled = false; }
  });

  $("tplApply").addEventListener("click", async () => {
    $("tplError").textContent = "";
    const parts = tplPicked($("tplImportParts"));
    if (!parts.length) { $("tplError").textContent = _t("admin.bitte_mindestens_einen_teil_auswaehlen"); return; }
    const mode = (document.querySelector('input[name="tplMode"]:checked') || {}).value || "merge";
    if (mode === "replace" && !confirm(_t("admin.ersetzen_entfernt_vorhandene_rollen_bots"))) return;
    $("tplApply").disabled = true;
    try {
      const r = await api("POST", "/api/admin/template/apply", { token: tplToken, parts, mode });
      renderTplResult(r.results);
      $("tplImportBox").hidden = true;
      $("tplFile").value = "";
      tplToken = "";
      toast(_t("admin.vorlage_uebernommen"));
    } catch (err) { $("tplError").textContent = err.message; }
    finally { $("tplApply").disabled = false; }
  });

  function renderTplResult(results) {
    const box = $("tplResult");
    const names = Object.fromEntries(TPL_PARTS.map(([id, n]) => [id, n]));
    box.replaceChildren(el("b", null, _t("admin.ergebnis")));
    const ul = el("ul");
    for (const r of results) {
      const bits = [];
      if (r.added) bits.push(_t("admin.x_n_new", { n: r.added }));
      if (r.updated) bits.push(`${r.updated} aktualisiert`);
      if (r.removed) bits.push(`${r.removed} entfernt`);
      if (r.skipped) bits.push(_t("admin.uebersprungen", { skipped: r.skipped }));
      const li = el("li", null, `${names[r.part] || r.part}: ${bits.join(", ") || _t("admin.x_no_change")}`);
      for (const e of r.errors || []) li.append(el("div", "warn", e));
      ul.append(li);
      if (r.keys && r.keys.length) {
        const k = el("div", "tpl-keys");
        k.append(el("b", null, _t("admin.neue_bot_schluessel_nur_jetzt_sichtbar")));
        for (const x of r.keys) {
          const row = el("div");
          row.append(el("span", null, x.bot + ": "), el("code", null, x.token));
          k.append(row);
        }
        li.append(k);
      }
    }
    box.append(ul);
    box.hidden = false;
  }

  // ---------- Sprachen (gemeinsamer Editor, web/i18n-editor.js) ----------

  let i18nEditor = null;
  function loadLanguages() {
    if (!i18nEditor) {
      i18nEditor = GoChatI18nEditor.mount($("i18nRoot"), {
        request: (method, path, body) => api(method, path, body),
        raw: (method, path, body, type) => fetch(path, {
          method, body, headers: Object.assign({ Authorization: "Bearer " + token }, type ? { "Content-Type": type } : {}),
        }),
        toast,
        areas: [["app.", _t("admin.i18n_area_app")], ["acct.", _t("admin.i18n_area_acct")], ["widget.", _t("admin.i18n_area_widget")],
          ["admin.", _t("admin.i18n_area_admin")], ["i18ned.", _t("admin.i18n_area_editor")], ["srv.", _t("admin.i18n_area_srv")],
          ["mail.", _t("admin.i18n_area_mail")], ["fmt.", _t("admin.i18n_area_fmt")], ["common.", _t("admin.i18n_area_common")]],
      });
    }
    i18nEditor.load();
    loadLangPacks(false);
  }

  // Sprachpakete vom Beacon
  const lpPct = new Intl.NumberFormat(GoChatI18n.locale, { style: "percent", maximumFractionDigits: 0 });
  const lpDate = new Intl.DateTimeFormat(GoChatI18n.locale, { dateStyle: "medium" });
  async function loadLangPacks(fromInput) {
    $("lpError").textContent = "";
    const url = fromInput ? $("lpUrl").value.trim() : "";
    let r;
    try { r = await api("GET", "/api/admin/i18n/beacon" + (url ? "?url=" + encodeURIComponent(url) : "")); }
    catch (err) { $("lpError").textContent = err.message; $("lpList").replaceChildren(); return; }
    $("lpUrls").replaceChildren(...(r.suggestions || []).map((u) => { const o = document.createElement("option"); o.value = u; return o; }));
    if (r.url) $("lpUrl").value = r.url;
    if (!r.url) { $("lpList").replaceChildren(el("li", "empty-row small", _t("admin.lp_keine_adresse"))); return; }
    if (!r.packs.length) { $("lpList").replaceChildren(el("li", "empty-row small", _t("admin.lp_keine", { name: r.name || r.url }))); return; }
    $("lpList").replaceChildren(...r.packs.map((p) => {
      const li = el("li", "user-row");
      li.append(el("span", "file-icon lp-code", p.code.slice(0, 5)));
      const main = el("div", "user-main");
      main.append(el("strong", null, p.name));
      if (p.update) main.append(el("span", "chip mute", _t("admin.lp_update")));
      else if (p.fromHere) main.append(el("span", "chip ok", _t("admin.lp_installiert")));
      else if (p.official) main.append(el("span", "chip", _t("admin.lp_eingebaut")));
      else if (p.installed) main.append(el("span", "chip", _t("admin.lp_vorhanden")));
      const meta = [lpPct.format(p.complete), _t("admin.lp_texte", { n: p.texts, total: p.total })];
      if (p.outdated) meta.push(_t("admin.lp_veraltet", { n: p.outdated }));
      meta.push(_t("admin.lp_stand", { date: lpDate.format(new Date(p.updated)) }));
      main.append(el("div", "user-meta", meta.join(" · ") + (p.note ? " · " + p.note : "")));
      const act = el("div", "room-actions");
      const label = p.update ? _t("admin.lp_aktualisieren") : p.fromHere ? _t("admin.lp_erneut") : _t("admin.lp_installieren");
      const b = el("button", (p.update || !p.fromHere ? "btn-primary" : "btn-quiet") + " small", label);
      b.type = "button";
      b.addEventListener("click", async () => {
        b.disabled = true;
        try {
          const res = await api("POST", "/api/admin/i18n/beacon/install", { url: r.url, code: p.code, updated: p.updated });
          let msg = _t("admin.lp_ergebnis", { name: p.name, applied: res.applied, unchanged: res.unchanged });
          if (res.errors && res.errors.length) msg += " " + _t("i18ned.import_fehler", { n: res.errors.length, list: res.errors.slice(0, 3).join("; ") });
          toast(msg, !!(res.errors && res.errors.length));
          if (i18nEditor) i18nEditor.load();
          loadLangPacks(true);
        } catch (err) { toast(err.message, true); b.disabled = false; }
      });
      act.append(b);
      li.append(main, act);
      return li;
    }));
  }
  $("lpForm").addEventListener("submit", (e) => { e.preventDefault(); loadLangPacks(true); });

  // ---------- Backups ----------

  const sizeFmt = (b) => (b < 1024 * 1024 ? `${numFmt.format(Math.max(1, Math.round(b / 1024)))} KB` : `${new Intl.NumberFormat(GoChatI18n.locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 }).format(b / 1024 / 1024)} MB`);

  // ---------- Sprach-Server: Funktionen und Auslastung je Server ----------

  let vsTimer = 0;
  function startVoiceDash() { stopVoiceDash(); loadVoiceDash(); vsTimer = setInterval(loadVoiceDash, 5000); }
  function stopVoiceDash() { clearInterval(vsTimer); vsTimer = 0; }
  const FUNC = {
    voice: ["admin.vs_f_voice", "admin.vs_f_voice_hint"],
    media: ["admin.vs_f_media", "admin.vs_f_media_hint"],
    tap: ["admin.vs_f_tap", "admin.vs_f_tap_hint"],
    stt: ["admin.vs_f_stt", "admin.vs_f_stt_hint"],
    "stt-chat": ["admin.vs_f_sttchat", "admin.vs_f_sttchat_hint"],
  };
  const KIND = { voice: "admin.vs_k_voice", screen: "admin.vs_k_screen", video: "admin.vs_k_video" };
  const mbit = (x) => (Math.round((x || 0) * 10) / 10).toLocaleString(GoChatI18n.locale);
  const vsAgo = (ms) => { const s = Math.max(0, Math.round((Date.now() - ms) / 1000)); return s < 90 ? _t("admin.vs_ago_s", { s }) : _t("admin.vs_ago_m", { m: Math.round(s / 60) }); };

  function meter(label, val, max, text) {
    const row = el("div", "vs-meter");
    const bar = el("span", "bar");
    const pct = max > 0 ? Math.min(100, (val / max) * 100) : 0;
    bar.style.setProperty("--w", pct + "%");
    if (pct >= 85) bar.classList.add("hot"); else if (pct >= 60) bar.classList.add("warm");
    row.append(el("span", "vs-meter-label", label), bar, el("span", "num", text));
    return row;
  }

  function sttBlock(title, st) {
    const box = el("div", "vs-stt");
    const head = el("div", "vs-stt-head");
    head.append(el("strong", null, title), el("span", "vs-stt-model", st.model + " · " + st.host + (st.lang ? " · " + st.lang : "")));
    box.append(head);
    box.append(meter(_t("admin.vs_stt_busy"), st.busy, st.concurrency, _t("admin.vs_stt_busy_n", { n: st.busy, max: st.concurrency, q: st.queue })));
    const facts = el("div", "vs-facts");
    const fact = (k, v, cls) => { const d = el("span", "vs-fact" + (cls ? " " + cls : "")); d.append(el("small", null, k), el("b", null, v)); facts.append(d); };
    fact(_t("admin.vs_stt_done"), numFmt.format(st.done || 0));
    fact(_t("admin.vs_stt_avg"), st.avgMs ? (st.avgMs / 1000).toLocaleString(GoChatI18n.locale, { maximumFractionDigits: 1 }) + " s" : "–");
    fact(_t("admin.vs_stt_errors"), numFmt.format(st.errors || 0), st.errors ? "bad" : "");
    fact(_t("admin.vs_stt_dropped"), numFmt.format(st.dropped || 0), st.dropped ? "warn" : "");
    box.append(facts);
    if (st.lastError && (!st.lastOkAt || st.lastErrorAt > st.lastOkAt)) {
      box.append(el("p", "vs-err", _t("admin.vs_stt_lasterr", { when: vsAgo(st.lastErrorAt), err: st.lastError })));
    } else if (st.lastOkAt) {
      box.append(el("p", "vs-ok", _t("admin.vs_stt_lastok", { when: vsAgo(st.lastOkAt) })));
    }
    return box;
  }

  async function loadVoiceDash() {
    let v;
    try { v = await api("GET", "/api/admin/voice"); } catch (err) { toast(err.message, true); stopVoiceDash(); return; }
    if (!v.enabled) {
      $("vsKpis").replaceChildren();
      $("vsGrid").replaceChildren(el("p", "empty-row", _t("admin.vs_off")));
      $("vsHint").textContent = "";
      return;
    }
    voiceSTT = !!v.stt;
    const up = v.relays.filter((r) => r.healthy).length;
    const people = v.relays.reduce((n, r) => n + (r.stats.participants || 0), 0);
    const kpi = (label, val, sub, warn) => { const d = el("div", "kpi" + (warn ? " warn" : "")); d.append(el("span", "kpi-label", label), el("strong", null, val), el("small", null, sub)); return d; };
    const sttWhere = v.chatSTT ? _t("admin.vs_kpi_stt_chat", { model: v.chatSTT.model }) : v.stt ? _t("admin.vs_kpi_stt_agents") : _t("admin.vs_kpi_stt_off");
    $("vsKpis").replaceChildren(
      kpi(_t("admin.sprachserver"), up + " / " + v.relays.length, up < v.relays.length ? _t("admin.vs_kpi_down", { n: v.relays.length - up }) : _t("admin.vs_kpi_alive"), up < v.relays.length),
      kpi(_t("admin.vs_kpi_people"), numFmt.format(people), _t("admin.vs_kpi_rooms", { n: Object.keys(v.rooms || {}).length })),
      kpi(_t("admin.vs_kpi_mbit"), mbit(v.relays.reduce((n, r) => n + (r.stats.mbit || 0), 0)), _t("admin.vs_kpi_mbit_sub")),
      kpi(_t("admin.vs_kpi_stt"), v.stt ? _t("admin.vs_on") : _t("admin.vs_off_short"), sttWhere, !v.stt),
    );
    const cards = v.relays.map((r) => {
      const card = el("article", "vs-card" + (r.healthy ? "" : " down"));
      const head = el("header", "vs-head");
      const dot = el("span", "vs-dot" + (r.healthy ? " ok" : ""));
      const title = el("div", "vs-title");
      title.append(el("strong", null, r.builtin ? _t("admin.voice_builtin") : r.id));
      const meta = [];
      if (r.version) meta.push(/^\d/.test(r.version) ? "v" + r.version : r.version);
      if (r.url) meta.push(r.url);
      if (r.seen) meta.push(r.healthy ? _t("admin.vs_seen", { when: vsAgo(new Date(r.seen).getTime()) }) : _t("admin.vs_silent", { when: vsAgo(new Date(r.seen).getTime()) }));
      title.append(el("small", null, meta.join(" · ") || _t("admin.vs_builtin_meta")));
      head.append(dot, title);
      if (r.role === "media") head.append(el("span", "chip", _t("admin.voice_relay_media")));
      card.append(head);
      const fl = el("div", "vs-funcs");
      const fs = r.functions || [];
      const chip = (f, on) => {
        const c = el("span", "vs-func" + (on ? " on" : " off"), _t(FUNC[f][0]));
        c.title = on ? _t(FUNC[f][1]) : _t("admin.vs_f_stt_missing");
        fl.append(c);
      };
      for (const f of ["voice", "media", "tap"]) if (fs.includes(f)) chip(f, true);
      if (fs.includes("stt")) chip("stt", true);
      else if (fs.includes("stt-chat")) chip("stt-chat", true);
      else if (fs.includes("tap")) chip("stt", false); // könnte mitschneiden, aber niemand verschriftlicht
      card.append(fl);
      const st = r.stats || {};
      card.append(meter(_t("admin.vs_people"), st.participants || 0, st.max || 0, `${st.participants || 0} / ${st.max || 0}`));
      card.append(meter(_t("admin.vs_bandwidth"), Math.max(st.mbit || 0, st.estimateMbit || 0), st.maxMbit || 0,
        _t("admin.vs_bandwidth_n", { mbit: mbit(st.mbit), est: mbit(st.estimateMbit), max: mbit(st.maxMbit) })));
      if (st.mediaPeers) {
        const pubs = Object.values(st.media || {}).reduce((n, m) => n + (m.pub || 0), 0);
        card.append(el("p", "vs-line", _t("admin.vs_media", { n: st.mediaPeers, pub: pubs })));
      }
      if (r.stt) card.append(sttBlock(_t("admin.vs_stt_own"), r.stt));
      const rl = el("ul", "vs-rooms");
      for (const u of r.rooms || []) {
        const li = el("li");
        li.append(el("span", "vs-room", "#" + u.room + (u.peers ? " · " + u.peers : "")));
        for (const k of u.kinds) li.append(el("span", "vs-kind " + k, _t(KIND[k] || k)));
        rl.append(li);
      }
      if (!(r.rooms || []).length) rl.append(el("li", "vs-empty", _t("admin.vs_no_rooms")));
      card.append(el("h3", "vs-sub", _t("admin.vs_rooms")), rl);
      return card;
    });
    if (v.chatSTT) {
      const card = el("article", "vs-card stt-card");
      const head = el("header", "vs-head");
      const title = el("div", "vs-title");
      title.append(el("strong", null, _t("admin.vs_chat_stt")), el("small", null, _t("admin.vs_chat_stt_meta")));
      head.append(el("span", "vs-dot ok"), title);
      card.append(head, sttBlock(_t("admin.vs_stt_queue"), v.chatSTT));
      cards.push(card);
    }
    $("vsGrid").replaceChildren(...(cards.length ? cards : [el("p", "empty-row", _t("admin.voice_no_relay"))]));
    $("vsHint").textContent = v.agents ? _t("admin.voice_agents_on") : _t("admin.voice_agents_off");
    const down = v.relays.length - up;
    $("relayDownCount").hidden = !down;
    $("relayDownCount").textContent = down || "";
  }

  // ---------- Transkripte (Live-Transkription) ----------

  let trItems = [];
  async function loadTranscripts() {
    let r;
    try { r = await api("GET", "/api/admin/transcripts"); } catch (err) { toast(err.message, true); return; }
    trItems = r.items || [];
    $("trIntro").textContent = !r.stt ? _t("admin.tr_intro_nostt")
      : _t("admin.tr_intro", { n: trItems.length }) + " " + (r.keepDays > 0 ? _t("admin.tr_keep_days", { days: r.keepDays }) : _t("admin.tr_keep_forever"))
        + (r.stored ? "" : " " + _t("admin.tr_not_stored"));
    renderTranscripts();
  }
  function renderTranscripts() {
    const q = $("trFilter").value.trim().toLowerCase();
    const list = trItems.filter((x) => !q || x.room.includes(q) || (x.byName || "").toLowerCase().includes(q));
    $("trList").replaceChildren(...(list.length ? list.map((x) => {
      const li = el("li", "user-row");
      const main = el("div", "user-main");
      main.append(el("strong", null, "#" + x.room + " · " + untilFmt.format(new Date(x.started))));
      if (x.active) main.append(el("span", "chip ban", _t("admin.tr_live")));
      const mins = x.ended ? Math.max(1, Math.round((x.ended - x.started) / 60000)) : Math.round((Date.now() - x.started) / 60000);
      main.append(el("div", "user-meta", _t("admin.tr_meta", { min: mins, n: x.lines || 0, name: x.byName || "?" })));
      const act = el("div", "room-actions");
      for (const fmt of ["txt", "json"]) {
        const b = el("button", "btn-quiet small", fmt.toUpperCase());
        b.type = "button";
        b.title = _t("admin.herunterladen");
        b.addEventListener("click", () => downloadTranscript(x, fmt, b));
        act.append(b);
      }
      const del = el("button", "btn-quiet small danger", _t("admin.loeschen"));
      del.type = "button";
      del.addEventListener("click", () => confirmAction(_t("admin.tr_delete_title", { room: x.room }),
        x.active ? _t("admin.tr_delete_active") : _t("admin.tr_delete_text"), _t("admin.loeschen"), async () => {
          try { await api("DELETE", "/api/admin/transcripts/" + encodeURIComponent(x.id)); toast(_t("admin.tr_deleted")); loadTranscripts(); }
          catch (err) { toast(err.message, true); }
        }));
      act.append(del);
      const icon = el("span", "file-icon");
      icon.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M5 2.5h7l3.5 3.5v11.5H5z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="M12 2.5V6h3.5M7.5 10h5M7.5 13h5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>';
      li.append(icon, main, act);
      return li;
    }) : [el("li", "empty-row small", _t("admin.tr_none"))]));
  }
  $("trFilter").addEventListener("input", renderTranscripts);
  async function downloadTranscript(x, fmt, btn) {
    btn.disabled = true;
    try {
      const res = await fetch("/api/admin/transcripts/" + encodeURIComponent(x.id) + "?format=" + fmt, { headers: { Authorization: "Bearer " + token } });
      if (!res.ok) throw new Error(_t("admin.download_fehlgeschlagen"));
      const blob = await res.blob();
      const m = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") || "");
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = m ? m[1] : "transkript." + fmt;
      document.body.append(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 2000);
    } catch (err) { toast(err.message, true); }
    btn.disabled = false;
  }

  async function loadBackups() {
    let r;
    try { r = await api("GET", "/api/admin/backups"); } catch (err) { toast(err.message, true); return; }
    $("backupNow").hidden = !r.enabled;
    if (!r.enabled) {
      $("backupIntro").textContent = _t("admin.automatische_backups_sind_aus_entweder");
      $("backupList").replaceChildren();
      return;
    }
    $("backupIntro").textContent = _t("admin.taeglich_um_3_uhr_wird_der_komplette", { timezone: settings.timezone, keep: r.keep, dir: r.dir }) +
      (r.lastError ? _t("admin.letzter_fehler", { lastError: r.lastError }) : "");
    $("backupList").replaceChildren(...(r.backups.length ? r.backups.map((b, i) => {
      const li = el("li", "user-row");
      const main = el("div", "user-main");
      main.append(el("strong", null, untilFmt.format(new Date(b.created))));
      if (i === 0) main.append(el("span", "chip ok", _t("admin.x_newest")));
      main.append(el("div", "user-meta", `${b.name} · ${sizeFmt(b.size)}`));
      const act = el("div", "room-actions");
      const dl = el("button", "btn-quiet small", _t("admin.herunterladen"));
      dl.type = "button";
      dl.addEventListener("click", () => downloadBackup(b.name, dl));
      act.append(dl);
      const icon = el("span", "file-icon");
      icon.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><ellipse cx="10" cy="5" rx="6" ry="2.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M4 5v10c0 1.4 2.7 2.5 6 2.5s6-1.1 6-2.5V5M4 10c0 1.4 2.7 2.5 6 2.5s6-1.1 6-2.5" fill="none" stroke="currentColor" stroke-width="1.6"/></svg>';
      li.append(icon, main, act);
      return li;
    }) : [el("li", "empty-row small", _t("admin.noch_keine_sicherung_vorhanden"))]));
  }

  async function downloadBackup(name, btn) {
    btn.disabled = true;
    try {
      const res = await fetch("/api/admin/backups/" + encodeURIComponent(name), { headers: { Authorization: "Bearer " + token } });
      if (!res.ok) throw new Error(_t("admin.download_fehlgeschlagen"));
      const blob = await res.blob();
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = name;
      document.body.append(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 2000);
    } catch (err) { toast(err.message, true); }
    finally { btn.disabled = false; }
  }

  $("backupNow").addEventListener("click", async () => {
    $("backupNow").disabled = true;
    try { const b = await api("POST", "/api/admin/backups"); toast(_t("admin.sicherung_angelegt", { sizeFmt: sizeFmt(b.size) })); loadBackups(); }
    catch (err) { toast(err.message, true); }
    finally { $("backupNow").disabled = false; }
  });

  // ---------- Widgets ----------

  let widgets = [];
  let editingWidget = null; // null = neu
  let widgetColor = ""; // leer = Akzentfarbe der Anwendung
  let previewTimer = null;
  let lastPreview = "";

  async function loadWidgets() {
    try { widgets = await api("GET", "/api/admin/widgets"); } catch (err) { toast(err.message, true); return; }
    renderWidgets();
  }

  function renderWidgets() {
    $("widgetList").replaceChildren(...(widgets.length ? widgets.map((w) => {
      const li = el("li", "user-row");
      const main = el("div", "user-main");
      main.append(el("strong", null, w.name), el("span", "chip", "#" + w.room));
      const bits = [w.mode === "inline" ? _t("admin.fest_im_inhalt") : "schwebend"];
      if (w.greeting) bits.push(_t("admin.mit_begruessung"));
      if (w.showHours) bits.push(_t("admin.oeffnungszeiten"));
      bits.push(w.domains.length ? _t("admin.domains") + w.domains.join(", ") : _t("admin.keine_domains_eingetragen"));
      main.append(el("div", "user-meta", bits.join(" · ")));
      const act = el("div", "room-actions");
      const copy = el("button", "btn-quiet small", _t("admin.code_kopieren"));
      copy.type = "button";
      copy.addEventListener("click", () => copyText(savedSnippet(w), _t("admin.code_kopiert")));
      const edit = el("button", "icon-action");
      edit.type = "button"; edit.innerHTML = ICON_EDIT; edit.title = _t("admin.bearbeiten");
      edit.setAttribute("aria-label", _t("admin.x_edit", { name: w.name }));
      edit.addEventListener("click", () => openWidgetEditor(w));
      act.append(copy, edit);
      const sw = el("span", "widget-swatch");
      sw.style.background = w.color || settings.accent;
      li.append(sw, main, act);
      return li;
    }) : [el("li", "empty-row small", _t("admin.noch_keine_widgets_gespeichert_leg_eines"))]));
    if (settings.embedPolicy === "listed" && !widgets.some((w) => w.domains.length)) {
      $("policySaved").textContent = _t("admin.achtung_kein_widget_hat_eine_domain_der");
      $("policySaved").className = "saved warn-text";
    }
  }

  function openWidgetEditor(w) {
    editingWidget = w || null;
    $("widgetEditor").hidden = false;
    $("widgetEditorTitle").textContent = w ? w.name : _t("admin.neues_widget");
    $("embName").value = w ? w.name : "";
    if (rooms.length) $("embRoom").value = w ? w.room : (rooms.some((r) => r.name === "lobby") ? "lobby" : rooms[0].name);
    $("embTitle").value = w ? w.title : _t("admin.chat");
    $("embMode").value = w ? w.mode : "floating";
    $("embPosition").value = w ? w.position : "right";
    $("embTheme").value = w ? w.theme : "light";
    widgetColor = w ? w.color || "" : "";
    $("embColor").value = widgetColor || settings.accent;
    $("embGreeting").value = w ? w.greeting || "" : "";
    $("embHours").checked = w ? w.showHours : false;
    $("embDomains").value = w ? w.domains.join("\n") : "";
    $("widgetDelete").hidden = !w;
    $("widgetError").textContent = "";
    $("embHoursHint").textContent = settings.hours && settings.hours.enabled ? _t("admin.zeigt_an_ob_gerade_jemand_da_ist") : _t("admin.oeffnungszeiten_sind_unter_allgemein");
    $("embDomainsHint").textContent = settings.embedPolicy === "listed"
      ? _t("admin.eine_domain_pro_zeile_nur_dort")
      : _t("admin.eine_domain_pro_zeile_wird_erst_geprueft");
    lastPreview = "";
    updateEmbed(true);
    $("widgetEditor").scrollIntoView({ behavior: "smooth", block: "start" });
    $("embName").focus({ preventScroll: true });
  }
  function closeWidgetEditor() { $("widgetEditor").hidden = true; editingWidget = null; }
  $("newWidgetBtn").addEventListener("click", () => openWidgetEditor(null));
  $("widgetCancel").addEventListener("click", closeWidgetEditor);

  function widgetBody() {
    return {
      name: $("embName").value.trim(), room: $("embRoom").value, title: $("embTitle").value.trim() || _t("admin.chat"),
      color: widgetColor, theme: $("embTheme").value, position: $("embPosition").value, mode: $("embMode").value,
      target: $("embMode").value === "inline" ? "#gochat" : "", greeting: $("embGreeting").value.trim(), showHours: $("embHours").checked,
      domains: $("embDomains").value.split(/[\n,]+/).map((d) => d.trim()).filter(Boolean),
    };
  }

  // Code mit gespeichertem Widget: Einstellungen kommen vom Server.
  function savedSnippet(w) {
    const script = `<script src="${location.origin}/embed/chat.js" data-widget="${escAttr(w.id)}" defer></` + "script>";
    return w.mode === "inline" ? `<div id="${escAttr((w.target || "#gochat").replace(/^#/, ""))}" style="height: 520px"></div>\n${script}` : script;
  }

  // Vorschau: alle Einstellungen als Attribute, damit ungespeicherte Änderungen sichtbar sind.
  function previewSnippet() {
    const b = widgetBody();
    const attrs = [`src="${location.origin}/embed/chat.js"`, `data-room="${escAttr(b.room || "lobby")}"`, `data-title="${escAttr(b.title)}"`];
    if (b.color) attrs.push(`data-color="${b.color}"`);
    if (b.theme !== "light") attrs.push(`data-theme="${b.theme}"`);
    if (b.mode === "inline") attrs.push('data-target="#gochat"');
    else { if (b.position === "left") attrs.push('data-position="left"'); attrs.push('data-open="true"'); }
    if (b.greeting) attrs.push(`data-greeting="${escAttr(b.greeting)}"`);
    if (b.showHours) attrs.push('data-hours="true"');
    const script = `<script ${attrs.join(" ")}></` + "script>";
    return b.mode === "inline" ? `<div id="gochat" style="height: 520px"></div>\n${script}` : script;
  }

  function updateEmbed(immediate) {
    const inline = $("embMode").value === "inline";
    $("embPositionField").style.visibility = inline ? "hidden" : "";
    $("embColorText").textContent = widgetColor || _t("admin.wie_anwendung");
    $("embColorReset").hidden = !widgetColor;
    if (editingWidget) {
      $("embedCode").textContent = savedSnippet(Object.assign({}, editingWidget, { mode: $("embMode").value }));
      $("embedHint").textContent = (inline ? _t("admin.fuege_den_code_dort_ein_wo_der_chat") : _t("admin.paste_before_body")) +
        _t("admin.spaetere_aenderungen_hier_wirken_sofort");
      $("copyEmbed").disabled = false;
    } else {
      $("embedCode").textContent = _t("admin.speichere_das_widget_dann_erscheint_hier");
      $("embedHint").textContent = "";
      $("copyEmbed").disabled = true;
    }
    clearTimeout(previewTimer);
    previewTimer = setTimeout(renderPreview, immediate ? 0 : 400);
  }

  function renderPreview() {
    if ($("widgetEditor").hidden) return;
    const html = `<!doctype html><html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:28px;font:15px/1.5 system-ui,sans-serif;color:#555;background:${$("embTheme").value === "dark" ? "#1b1d26" : "#f7f7f8"}">
<p style="margin:0 0 16px">So erscheint der Chat auf deiner Webseite.</p>
${previewSnippet()}
</body></html>`;
    if (html === lastPreview) return;
    lastPreview = html;
    $("embedPreview").srcdoc = html;
  }

  $("embColor").addEventListener("input", () => { widgetColor = $("embColor").value.toLowerCase(); updateEmbed(false); });
  $("embColorReset").addEventListener("click", () => { widgetColor = ""; $("embColor").value = settings.accent; updateEmbed(false); });
  ["embRoom", "embTitle", "embMode", "embPosition", "embTheme", "embGreeting", "embHours"].forEach((id) => {
    $(id).addEventListener("input", () => updateEmbed(false));
    $(id).addEventListener("change", () => updateEmbed(false));
  });

  $("embedForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("widgetError").textContent = "";
    try {
      const w = editingWidget
        ? await api("PUT", "/api/admin/widgets/" + editingWidget.id, widgetBody())
        : await api("POST", "/api/admin/widgets", widgetBody());
      const isNew = !editingWidget;
      editingWidget = w;
      $("widgetEditorTitle").textContent = w.name;
      $("widgetDelete").hidden = false;
      $("embDomains").value = w.domains.join("\n");
      $("widgetSaved").textContent = isNew ? _t("admin.angelegt_code_unten_kopieren") : _t("admin.gespeichert");
      setTimeout(() => ($("widgetSaved").textContent = ""), 3500);
      updateEmbed(true);
      loadWidgets();
    } catch (err) { $("widgetError").textContent = err.message; }
  });

  $("widgetDelete").addEventListener("click", () => {
    const w = editingWidget;
    confirmAction(_t("admin.widget_loeschen", { name: w.name }),
      _t("admin.webseiten_die_diesen_code_eingebaut"),
      _t("admin.widget_loeschen_2"),
      async () => { await api("DELETE", "/api/admin/widgets/" + w.id); toast(_t("admin.widget_geloescht")); closeWidgetEditor(); loadWidgets(); });
  });

  $("copyEmbed").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText($("embedCode").textContent);
      $("copyEmbed").textContent = _t("admin.kopiert");
      setTimeout(() => ($("copyEmbed").textContent = _t("admin.code_kopieren")), 1500);
    } catch { toast(_t("admin.kopieren_nicht_moeglich_bitte_den_code"), true); }
  });

  document.querySelectorAll('input[name="embedPolicy"]').forEach((r) => r.addEventListener("change", async () => {
    const policy = $("policyListed").checked ? "listed" : "any";
    try {
      settings = await api("PUT", "/api/admin/settings", Object.assign({}, settings, { embedPolicy: policy }));
      $("policySaved").className = "saved";
      $("policySaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("policySaved").textContent = ""), 2500);
      renderWidgets();
      if (!$("widgetEditor").hidden) openWidgetEditor(editingWidget);
    } catch (err) { toast(err.message, true); }
  }));

  // ---------- Emotes & GIFs ----------

  let mediaAdmin = { settings: {}, emotes: [], gifs: [] };

  async function loadMediaAdmin() {
    loadUploads();
    try { mediaAdmin = await api("GET", "/api/admin/media"); } catch (err) { toast(err.message, true); return; }
    const st = mediaAdmin.settings;
    $("msEmoteSize").value = String(st.emoteSize);
    $("msGifWidth").value = String(st.gifWidth);
    $("msGifs").checked = st.gifsEnabled;
    $("msProvider").value = st.provider || "";
    $("msRating").value = st.rating || "g";
    $("msKey").value = "";
    $("msKey").placeholder = st.hasKey ? _t("admin.gespeichert_leer_lassen_um_ihn_zu") : _t("admin.api_schluessel_eintragen");
    renderEmotesAdmin();
    renderGifsAdmin();
    if (!reactDirty) {
      reactDraft = [...mediaAdmin.reactions.reactions];
      $("reactEmotes").checked = mediaAdmin.reactions.emoteReactions;
    }
    renderReactAdmin();
  }

  // ---------- Reaktionen ----------

  let reactDraft = [];
  let reactDirty = false;
  const REACT_SUGGEST = ["👍", "❤️", "😂", "😮", "😢", "🙏", "🎉", "🔥", "👀", "✅", "👏", "🤔", "😍", "🙌", "💯", "😎", "🤝", "⭐", "👎", "🚀"];

  function reactIcon(e, size) {
    const m = /^:([a-z0-9_]{2,32}):$/.exec(e);
    const em = m && mediaAdmin && mediaAdmin.emotes.find((x) => x.name === m[1]);
    if (em) {
      const img = el("img");
      img.src = em.url; img.alt = e; img.width = size; img.height = size;
      return img;
    }
    return el("span", "react-emoji", e);
  }

  function markReactDirty() { reactDirty = true; $("reactSaved").textContent = ""; renderReactAdmin(); }
  function addReaction(e) {
    $("reactError").textContent = "";
    if (!e) return;
    const em = /^:([a-z0-9_]{2,32}):$/i.exec(e);
    if (em) {
      e = ":" + em[1].toLowerCase() + ":";
      if (!mediaAdmin.emotes.some((x) => x.name === em[1].toLowerCase())) { $("reactError").textContent = _t("admin.das_emote_gibt_es_nicht", { emoji: e }); return; }
    } else if (/[\p{L}\s]/u.test(e) || !/[\p{Extended_Pictographic}\p{Regional_Indicator}\u20e3]/u.test(e) || [...e].length > 8) {
      $("reactError").textContent = _t("admin.bitte_ein_einzelnes_emoji_oder_ein_emote");
      return;
    }
    if (reactDraft.includes(e)) { $("reactError").textContent = _t("admin.ist_schon_in_der_auswahl", { emoji: e }); return; }
    if (reactDraft.length >= 12) { $("reactError").textContent = _t("admin.hoechstens_12_reaktionen_entferne_erst"); return; }
    reactDraft.push(e);
    markReactDirty();
  }

  function renderReactAdmin() {
    let dragFrom = -1;
    $("reactChips").replaceChildren(...reactDraft.map((e, i) => {
      const li = el("li", "react-chip");
      li.draggable = true;
      li.title = e;
      li.addEventListener("dragstart", (ev) => { dragFrom = i; li.classList.add("dragging"); ev.dataTransfer.effectAllowed = "move"; });
      li.addEventListener("dragend", () => li.classList.remove("dragging"));
      li.addEventListener("dragover", (ev) => { ev.preventDefault(); li.classList.add("over"); });
      li.addEventListener("dragleave", () => li.classList.remove("over"));
      li.addEventListener("drop", (ev) => {
        ev.preventDefault();
        if (dragFrom < 0 || dragFrom === i) return;
        const [x] = reactDraft.splice(dragFrom, 1);
        reactDraft.splice(i, 0, x);
        markReactDirty();
      });
      const move = (d) => {
        const j = i + d;
        if (j < 0 || j >= reactDraft.length) return;
        [reactDraft[i], reactDraft[j]] = [reactDraft[j], reactDraft[i]];
        markReactDirty();
        const btn = $("reactChips").children[j] && $("reactChips").children[j].querySelector(d < 0 ? ".mv-l" : ".mv-r");
        if (btn) btn.focus();
      };
      const left = el("button", "mv mv-l", "‹");
      left.type = "button"; left.disabled = i === 0;
      left.setAttribute("aria-label", _t("admin.nach_vorn", { emoji: e }));
      left.addEventListener("click", () => move(-1));
      const right = el("button", "mv mv-r", "›");
      right.type = "button"; right.disabled = i === reactDraft.length - 1;
      right.setAttribute("aria-label", _t("admin.nach_hinten", { emoji: e }));
      right.addEventListener("click", () => move(1));
      const del = el("button", "rm", "×");
      del.type = "button";
      del.setAttribute("aria-label", _t("admin.x_remove", { name: e }));
      del.addEventListener("click", () => { reactDraft.splice(i, 1); markReactDirty(); });
      li.append(left, reactIcon(e, 24), right, del);
      return li;
    }));
    if (!reactDraft.length) $("reactChips").append(el("li", "react-empty", _t("admin.leer_fuege_mindestens_eine_reaktion")));
    $("reactSuggest").replaceChildren(...REACT_SUGGEST.filter((e) => !reactDraft.includes(e)).map((e) => {
      const b = el("button", "react-pick", e);
      b.type = "button";
      b.setAttribute("aria-label", _t("admin.hinzufuegen", { emoji: e }));
      b.addEventListener("click", () => addReaction(e));
      return b;
    }));
    const ems = (mediaAdmin ? mediaAdmin.emotes : []).filter((x) => !reactDraft.includes(":" + x.name + ":"));
    $("reactEmoteRow").hidden = !ems.length;
    $("reactEmoteSuggest").replaceChildren(...ems.map((x) => {
      const b = el("button", "react-pick");
      b.type = "button";
      b.title = `:${x.name}:`;
      b.setAttribute("aria-label", _t("admin.hinzufuegen_2", { name: x.name }));
      b.append(reactIcon(":" + x.name + ":", 24));
      b.addEventListener("click", () => addReaction(":" + x.name + ":"));
      return b;
    }));
  }

  const addTyped = () => { const v = $("reactAdd").value.trim(); if (v) { addReaction(v); if (!$("reactError").textContent) $("reactAdd").value = ""; } };
  $("reactAddBtn").addEventListener("click", addTyped);
  $("reactAdd").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); addTyped(); } });
  $("reactEmotes").addEventListener("change", () => { reactDirty = true; $("reactSaved").textContent = ""; });
  $("reactForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("reactError").textContent = "";
    try {
      const out = await api("PUT", "/api/admin/reactions", { reactions: reactDraft, emoteReactions: $("reactEmotes").checked });
      reactDraft = [...out.reactions];
      reactDirty = false;
      mediaAdmin.reactions = out;
      renderReactAdmin();
      $("reactSaved").textContent = _t("admin.gespeichert");
    } catch (err) { $("reactError").textContent = err.message; }
  });

  $("mediaSettingsForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = {
      emoteSize: +$("msEmoteSize").value, gifWidth: +$("msGifWidth").value, gifsEnabled: $("msGifs").checked,
      provider: $("msProvider").value, rating: $("msRating").value,
    };
    if ($("msKey").value.trim()) body.apiKey = $("msKey").value.trim();
    if (body.provider && !body.apiKey && !mediaAdmin.settings.hasKey) { toast(_t("admin.fuer_die_web_suche_wird_ein_api"), true); return; }
    try {
      mediaAdmin.settings = await api("PUT", "/api/admin/media/settings", body);
      $("msKey").value = "";
      $("msKey").placeholder = mediaAdmin.settings.hasKey ? _t("admin.gespeichert_leer_lassen_um_ihn_zu") : _t("admin.api_schluessel_eintragen");
      $("msSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("msSaved").textContent = ""), 2500);
      setTimeout(loadMediaAdmin, 1500); // GIFs werden ggf. im Hintergrund neu skaliert
    } catch (err) { toast(err.message, true); }
  });

  function renderEmotesAdmin() {
    const list = mediaAdmin.emotes;
    $("emoteCount").textContent = list.length ? `(${list.length})` : "";
    $("emoteList").replaceChildren(...(list.length ? list.map((em) => {
      const li = el("li", "emote-card");
      const box = el("div", "emote-box");
      const img = el("img");
      img.src = em.url; img.width = em.size; img.height = em.size; img.alt = `:${em.name}:`;
      box.append(img);
      const name = el("code", null, `:${em.name}:`);
      const meta = el("small", null, `${em.size} × ${em.size}` + (em.animated ? _t("admin.animiert") : ""));
      const act = el("div", "emote-actions");
      const size = el("button", "btn-quiet small", em.size === 32 ? _t("admin.64_px") : _t("admin.32_px"));
      size.type = "button";
      size.title = _t("admin.groesse_aendern_aus_dem_original_neu");
      size.addEventListener("click", () => updateEmote(em.name, { size: em.size === 32 ? 64 : 32 }));
      const ren = el("button", "icon-action");
      ren.type = "button"; ren.innerHTML = ICON_EDIT; ren.title = _t("admin.umbenennen");
      ren.setAttribute("aria-label", `:${em.name}: umbenennen`);
      ren.addEventListener("click", () => {
        const n = prompt(_t("admin.neuer_name_fuer_alte_nachrichten_mit", { name: em.name }), em.name);
        if (n && n !== em.name) updateEmote(em.name, { name: n });
      });
      const del = el("button", "icon-action danger");
      del.type = "button"; del.innerHTML = ICON_TRASH; del.title = _t("admin.loeschen");
      del.setAttribute("aria-label", _t("admin.loeschen_5", { name: em.name }));
      del.addEventListener("click", () => confirmAction(_t("admin.loeschen_6", { name: em.name }),
        _t("admin.das_emote_verschwindet_aus_der_auswahl"), _t("admin.emote_loeschen"),
        async () => { await api("DELETE", "/api/admin/emotes/" + encodeURIComponent(em.name)); toast(_t("admin.emote_geloescht")); loadMediaAdmin(); }));
      act.append(size, ren, del);
      li.append(box, name, meta, act);
      return li;
    }) : [el("li", "empty-row small", _t("admin.noch_keine_emotes_lade_png_jpeg_oder_gif"))]));
  }

  async function updateEmote(name, body) {
    try { await api("PATCH", "/api/admin/emotes/" + encodeURIComponent(name), body); loadMediaAdmin(); }
    catch (err) { toast(err.message, true); }
  }

  // Hochladen per multipart/form-data (api() sendet nur JSON)
  async function uploadFile(path, file, fields) {
    const fd = new FormData();
    fd.append("file", file);
    for (const [k, v] of Object.entries(fields)) if (v) fd.append(k, v);
    const res = await fetch(path, { method: "POST", headers: { Authorization: "Bearer " + token }, body: fd });
    const data = await res.json().catch(() => ({}));
    if (res.status === 401) { showLogin(); throw new Error(_t("admin.sitzung_abgelaufen")); }
    if (!res.ok) throw new Error(data.error || _t("admin.hochladen_fehlgeschlagen"));
    return data;
  }

  async function uploadMany(files, statusId, one) {
    const status = $(statusId);
    let ok = 0;
    const errs = [];
    for (let i = 0; i < files.length; i++) {
      status.textContent = _t("admin.lade_von_hoch_und_skaliere", { i: i + 1, total: files.length });
      try { await one(files[i]); ok++; } catch (err) { errs.push(`${files[i].name}: ${err.message}`); }
    }
    status.textContent = (ok ? _t("admin.hinzugefuegt", { n: ok }) : "") + (errs.length ? _t("admin.fehler_2") + errs.join(" · ") : "");
    status.classList.toggle("err", errs.length > 0);
    loadMediaAdmin();
  }

  $("emoteFiles").addEventListener("change", () => {
    const files = [...$("emoteFiles").files];
    const name = files.length === 1 ? $("emoteName").value.trim() : "";
    uploadMany(files, "emoteStatus", (f) => uploadFile("/api/admin/emotes", f, { name: name || f.name, size: $("emoteSize").value !== "0" ? $("emoteSize").value : "" }))
      .then(() => { $("emoteFiles").value = ""; $("emoteName").value = ""; });
  });
  $("emoteImportForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("emoteStatus").textContent = _t("admin.lade_und_skaliere");
    try {
      await api("POST", "/api/admin/emotes", { url: $("emoteURL").value.trim(), name: $("emoteName").value.trim(), size: +$("emoteSize").value });
      $("emoteStatus").textContent = _t("admin.importiert");
      $("emoteURL").value = ""; $("emoteName").value = "";
      loadMediaAdmin();
    } catch (err) { $("emoteStatus").textContent = err.message; $("emoteStatus").classList.add("err"); }
  });

  function renderGifsAdmin() {
    const list = mediaAdmin.gifs;
    $("gifCount").textContent = list.length ? `(${list.length})` : "";
    $("gifList").replaceChildren(...(list.length ? list.map((g) => {
      const li = el("li", "gif-card");
      const img = el("img");
      img.src = g.url; img.width = g.w; img.height = g.h; img.alt = g.title || "GIF"; img.loading = "lazy";
      const body = el("div", "gif-card-body");
      body.append(el("strong", null, g.title || _t("admin.ohne_titel")), el("small", null, `${g.w} × ${g.h}` + (g.tags.length ? " · " + g.tags.join(", ") : "")));
      const act = el("div", "emote-actions");
      const ed = el("button", "icon-action");
      ed.type = "button"; ed.innerHTML = ICON_EDIT; ed.title = _t("admin.titel_und_schlagwoerter");
      ed.setAttribute("aria-label", _t("admin.titel_und_schlagwoerter_aendern"));
      ed.addEventListener("click", async () => {
        const title = prompt(_t("admin.titel"), g.title);
        if (title === null) return;
        const tags = prompt(_t("admin.schlagwoerter_mit_komma"), g.tags.join(", "));
        if (tags === null) return;
        try { await api("PATCH", "/api/admin/gifs/" + g.id, { title, tags: tags.split(",") }); loadMediaAdmin(); }
        catch (err) { toast(err.message, true); }
      });
      const del = el("button", "icon-action danger");
      del.type = "button"; del.innerHTML = ICON_TRASH; del.title = _t("admin.loeschen");
      del.setAttribute("aria-label", _t("admin.gif_loeschen"));
      del.addEventListener("click", () => confirmAction(_t("admin.gif_loeschen_2"),
        _t("admin.das_gif_verschwindet_aus_der_bibliothek"), _t("admin.gif_loeschen"),
        async () => { await api("DELETE", "/api/admin/gifs/" + g.id); toast(_t("admin.gif_geloescht")); loadMediaAdmin(); }));
      act.append(ed, del);
      li.append(img, body, act);
      return li;
    }) : [el("li", "empty-row small", _t("admin.noch_keine_gifs_in_der_bibliothek"))]));
  }

  $("gifFiles").addEventListener("change", () => {
    const files = [...$("gifFiles").files];
    uploadMany(files, "gifStatus", (f) => uploadFile("/api/admin/gifs", f, { title: f.name.replace(/\.gif$/i, "").replace(/[-_]+/g, " "), tags: $("gifTags").value }))
      .then(() => { $("gifFiles").value = ""; });
  });
  $("gifImportForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("gifStatus").textContent = _t("admin.lade_und_skaliere");
    $("gifStatus").classList.remove("err");
    try {
      const u = $("gifURL").value.trim();
      const title = decodeURIComponent((u.split("/").pop() || "").replace(/\.gif.*$/i, "")).replace(/[-_]+/g, " ").slice(0, 80);
      await api("POST", "/api/admin/gifs", { url: u, title, tags: $("gifTags").value.split(",") });
      $("gifStatus").textContent = _t("admin.importiert");
      $("gifURL").value = "";
      loadMediaAdmin();
    } catch (err) { $("gifStatus").textContent = err.message; $("gifStatus").classList.add("err"); }
  });

  // ---------- Befehle ----------

  let cmdCfg = { enabled: true, prefix: "!", botName: "", commands: [] };
  let editingCmd = -1;
  const KIND_LABEL = { text: _t("admin.text"), help: _t("admin.hilfe"), dice: _t("admin.wuerfel"), coin: _t("admin.muenze"), time: _t("admin.uhrzeit"), online: _t("admin.online"), choose: _t("admin.auswahl") };
  const KIND_DEFAULT = {
    text: "", help: _t("admin.das_kann_ich"), dice: _t("admin.wuerfelt_eine"), coin: _t("admin.text_2"),
    time: _t("admin.es_ist_uhr_am"), online: _t("admin.text_3"), choose: _t("admin.ich_nehme"),
    poll: "", temproom: _t("admin.cmd_temproom_default"),
  };

  async function loadCommandsAdmin() {
    try { cmdCfg = await api("GET", "/api/admin/commands"); } catch (err) { toast(err.message, true); return; }
    $("cmdEnabled").checked = cmdCfg.enabled;
    $("cmdBotName").value = cmdCfg.botName;
    $("cmdPrefix").value = cmdCfg.prefix;
    renderCommands();
  }

  async function saveCommands(next) {
    const saved = await api("PUT", "/api/admin/commands", next);
    cmdCfg = saved;
    renderCommands();
    return saved;
  }

  $("cmdSettingsForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await saveCommands(Object.assign({}, cmdCfg, { enabled: $("cmdEnabled").checked, botName: $("cmdBotName").value, prefix: $("cmdPrefix").value }));
      $("cmdSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("cmdSaved").textContent = ""), 2500);
    } catch (err) { toast(err.message, true); }
  });

  function renderCommands() {
    const list = cmdCfg.commands;
    $("cmdList").replaceChildren(...(list.length ? list.map((c, i) => {
      const li = el("li", "cmd-row" + (c.enabled ? "" : " off"));
      const main = el("div", "cmd-main");
      const head = el("div", "cmd-head");
      head.append(el("code", "cmd-trigger", cmdCfg.prefix + c.trigger));
      if (c.aliases.length) head.append(el("span", "cmd-alias", c.aliases.map((a) => cmdCfg.prefix + a).join(" ")));
      head.append(el("span", "chip", KIND_LABEL[c.kind] || c.kind));
      if (c.ephemeral) head.append(el("span", "chip eph", _t("admin.nur_fuer_die_person")));
      if (c.teamOnly) head.append(el("span", "chip", _t("admin.nur_team")));
      if (c.rooms.length) head.append(el("span", "chip", c.rooms.map((r) => "#" + r).join(", ")));
      if (c.cooldown) head.append(el("span", "chip", _t("admin.s_pause", { cooldown: c.cooldown })));
      main.append(head);
      main.append(el("p", "cmd-desc", c.description || F_plain(c.response).slice(0, 120)));
      const act = el("div", "room-actions");
      const tog = el("input", "switch small-switch");
      tog.type = "checkbox";
      tog.checked = c.enabled;
      tog.title = c.enabled ? _t("admin.aktiv") : _t("admin.aus");
      tog.setAttribute("aria-label", _t("admin.x_active", { name: cmdCfg.prefix + c.trigger }));
      tog.addEventListener("change", async () => {
        const next = structuredClone(cmdCfg);
        next.commands[i].enabled = tog.checked;
        try { await saveCommands(next); } catch (err) { toast(err.message, true); tog.checked = !tog.checked; }
      });
      const edit = el("button", "icon-action");
      edit.type = "button"; edit.innerHTML = ICON_EDIT; edit.title = _t("admin.bearbeiten");
      edit.setAttribute("aria-label", _t("admin.x_edit", { name: cmdCfg.prefix + c.trigger }));
      edit.addEventListener("click", () => openCmdDialog(i));
      act.append(tog, edit);
      li.append(main, act);
      return li;
    }) : [el("li", "empty-row", _t("admin.keine_befehle"))]));
  }

  function F_plain(t) { return window.GoChatFormat ? window.GoChatFormat.plain(t || "") : (t || ""); }

  function syncEphemeral() {
    const on = $("cmdEphemeral").checked;
    $("cmdReply").disabled = on;
    $("cmdReply").closest("label").classList.toggle("disabled", on);
    $("cmdReply").closest("label").title = on ? _t("admin.private_antworten_zeigen_den_befehl") : "";
    $("cmdCooldownUnit").textContent = on ? _t("admin.sekunden_je_person") : _t("admin.sekunden_je_kanal");
  }
  $("cmdEphemeral").addEventListener("change", syncEphemeral);

  function openCmdDialog(i) {
    editingCmd = i;
    const c = i >= 0 ? cmdCfg.commands[i] : { trigger: "", aliases: [], kind: "text", response: "", description: "", cooldown: 0, teamOnly: false, rooms: [], reply: false, enabled: true };
    $("cmdDialogTitle").textContent = i >= 0 ? _t("admin.x_edit", { name: cmdCfg.prefix + c.trigger }) : _t("admin.befehl_anlegen");
    $("cmdPrefixShow").textContent = cmdCfg.prefix;
    $("cmdTrigger").value = c.trigger;
    $("cmdAliases").value = c.aliases.join(", ");
    $("cmdKind").value = c.kind;
    $("cmdDesc").value = c.description;
    $("cmdResponse").value = c.response;
    $("cmdReply").checked = c.reply;
    $("cmdEphemeral").checked = !!c.ephemeral;
    syncEphemeral();
    $("cmdTeam").checked = c.teamOnly;
    $("cmdOn").checked = c.enabled;
    $("cmdCooldown").value = c.cooldown;
    (c.rooms.length ? $("cmdSome") : $("cmdAll")).checked = true;
    $("cmdRooms").replaceChildren(...rooms.map((r) => {
      const l = el("label");
      const cb = el("input");
      cb.type = "checkbox"; cb.value = r.name; cb.checked = c.rooms.includes(r.name);
      l.append(cb, document.createTextNode("#" + r.name));
      return l;
    }));
    syncCmdScope();
    $("cmdDelete").hidden = i < 0;
    $("cmdError").textContent = "";
    $("cmdDialog").showModal();
    $("cmdTrigger").focus();
  }
  function syncCmdScope() { $("cmdRooms").classList.toggle("disabled", $("cmdAll").checked); }
  $("cmdAll").addEventListener("change", syncCmdScope);
  $("cmdSome").addEventListener("change", syncCmdScope);
  $("cmdKind").addEventListener("change", () => {
    // Vorlage vorschlagen, solange die Antwort leer oder eine Standardvorlage ist
    const cur = $("cmdResponse").value.trim();
    if (!cur || Object.values(KIND_DEFAULT).includes(cur)) $("cmdResponse").value = KIND_DEFAULT[$("cmdKind").value];
  });
  $("newCmdBtn").addEventListener("click", () => openCmdDialog(-1));

  $("cmdForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("cmdError").textContent = "";
    const next = structuredClone(cmdCfg);
    const c = {
      id: editingCmd >= 0 ? cmdCfg.commands[editingCmd].id : "",
      trigger: $("cmdTrigger").value, aliases: $("cmdAliases").value.split(","), kind: $("cmdKind").value,
      response: $("cmdResponse").value, description: $("cmdDesc").value, cooldown: Math.max(0, parseInt($("cmdCooldown").value, 10) || 0),
      teamOnly: $("cmdTeam").checked, reply: $("cmdReply").checked, enabled: $("cmdOn").checked, ephemeral: $("cmdEphemeral").checked,
      rooms: $("cmdSome").checked ? [...$("cmdRooms").querySelectorAll("input:checked")].map((x) => x.value) : [],
    };
    if (editingCmd >= 0) next.commands[editingCmd] = c; else next.commands.push(c);
    try {
      await saveCommands(next);
      $("cmdDialog").close();
      toast(editingCmd >= 0 ? _t("admin.befehl_gespeichert") : _t("admin.befehl_angelegt"));
    } catch (err) { $("cmdError").textContent = err.message; }
  });
  $("cmdDelete").addEventListener("click", () => {
    const c = cmdCfg.commands[editingCmd];
    $("cmdDialog").close();
    confirmAction(_t("admin.loeschen_7", { prefix: cmdCfg.prefix, trigger: c.trigger }), _t("admin.der_bot_reagiert_danach_nicht_mehr_auf"), _t("admin.befehl_loeschen"), async () => {
      const next = structuredClone(cmdCfg);
      next.commands = next.commands.filter((x) => x.id !== c.id);
      await saveCommands(next);
      toast(_t("admin.befehl_geloescht"));
    });
  });

  $("cmdTestForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const box = $("cmdPreview");
    try {
      const r = await api("POST", "/api/admin/commands/test", { text: $("cmdTestText").value, room: $("cmdTestRoom").value, team: $("cmdTestTeam").checked });
      box.hidden = false;
      box.replaceChildren();
      if (!r.matched) {
        box.className = "cmd-preview none";
        box.textContent = r.note || _t("admin.keine_antwort");
        return;
      }
      box.className = "cmd-preview";
      const who = el("div", "cmd-preview-head");
      who.append(el("b", null, cmdCfg.botName), el("span", "bot-badge", _t("admin.bot")), el("small", null, r.ephemeral ? _t("admin.nur_fuer_die_person_sichtbar_im_kanal") : r.reply ? _t("admin.antwortet_mit_zitat") : ""));
      box.classList.toggle("eph", !!r.ephemeral);
      const text = el("div", "cmd-preview-text");
      let emoteMap = null;
      try { emoteMap = new Map((await (await fetch("/api/media")).json()).emotes.map((x) => [x.name, x])); } catch { /* egal */ }
      if (window.GoChatFormat) window.GoChatFormat.render(text, r.text, { emotes: emoteMap });
      else text.textContent = r.text;
      box.append(who, text);
    } catch (err) { toast(err.message, true); }
  });

  // ---------- Rollen-Auswahl an Kanälen ----------

  function fillRolePicks(id, selected, noTeam) {
    const box = $(id);
    const sel = new Set(selected);
    const items = (noTeam ? [] : [{ id: "team", name: _t("admin.nur_team_2"), color: "#16a34a" }]).concat(roleCfg.roles);
    box.replaceChildren(...items.map((r) => {
      const b = el("button", "role-toggle", r.name);
      b.type = "button";
      b.dataset.id = r.id;
      b.style.setProperty("--role", r.color);
      b.setAttribute("aria-pressed", String(sel.has(r.id)));
      b.addEventListener("click", () => {
        const on = b.getAttribute("aria-pressed") !== "true";
        b.setAttribute("aria-pressed", String(on));
        // "Nur Team" schließt die anderen aus und umgekehrt
        if (on && r.id === "team") box.querySelectorAll(".role-toggle").forEach((x) => { if (x !== b) x.setAttribute("aria-pressed", "false"); });
        const team = box.querySelector('[data-id="team"]');
        if (on && r.id !== "team" && team) team.setAttribute("aria-pressed", "false");
      });
      return b;
    }));
    if (!roleCfg.roles.length) box.append(el("small", "role-hint", _t("admin.lege_unter_personen_rollen_an_wer_zugang")));
  }
  function pickedRoles(id) {
    return [...$(id).querySelectorAll('.role-toggle[aria-pressed="true"]')].map((b) => b.dataset.id);
  }
  function roomAccessLabel(r) {
    const name = (id) => (id === "team" ? _t("admin.team") : (roleCfg.roles.find((x) => x.id === id) || {}).name || "?");
    const out = [];
    if (r.readRoles && r.readRoles.length) out.push("🔒 " + r.readRoles.map(name).join(", "));
    if (r.writeRoles && r.writeRoles.length) out.push("✍ " + r.writeRoles.map(name).join(", "));
    return out.join(" · ");
  }

  // ---------- Angepinnt (Moderation) ----------

  const modPins = {}; // Kanal → Set(IDs)
  async function loadModPins(room) {
    try { modPins[room] = new Set((await api("GET", `/api/admin/rooms/${encodeURIComponent(room)}/pins`)).map((m) => m.id)); }
    catch { modPins[room] = new Set(); }
  }

  // ---------- Meldungen ----------

  async function loadReportCount() {
    try {
      const n = (await api("GET", "/api/admin/reports/count")).open;
      $("reportCount").hidden = !n;
      $("reportCount").textContent = n > 99 ? "99+" : String(n);
      $("reportCount").title = n === 1 ? _t("admin.1_offene_meldung") : _t("admin.offene_meldungen", { n: n });
    } catch { /* egal */ }
  }

  async function loadReports() {
    const status = document.querySelector('input[name="repStatus"]:checked').value;
    let list;
    try { list = await api("GET", "/api/admin/reports?status=" + status); } catch (err) { toast(err.message, true); return; }
    $("reportList").replaceChildren(...(list.length ? list.map((r) => reportItem(r, status === "open")) : [el("li", "empty-row", status === "open" ? _t("admin.keine_offenen_meldungen") : _t("admin.noch_nichts_erledigt"))]));
    loadReportCount();
  }
  document.querySelectorAll('input[name="repStatus"]').forEach((x) => x.addEventListener("change", loadReports));

  function reportItem(r, open) {
    const li = el("li", "mod-item report-item");
    const m = r.message;
    const body = el("div");
    const meta = el("div", "meta");
    const where = r.room.startsWith("dm-") ? _t("admin.direktnachricht") : "#" + r.room;
    meta.append(el("span", "mod-room", where), el("time", null, timeFmt.format(new Date(r.created))));
    if (r.count > 1) meta.append(el("span", "chip ban", _t("admin.meldungen_2", { count: r.count })));
    body.append(meta);
    if (m) {
      const who = el("div", "report-msg");
      who.append(el("b", null, m.name + ": "));
      const txt = m.deleted ? (m.original || _t("admin.geloescht_2")) : m.text || (m.poll ? _t("admin.umfrage") + m.poll.question : m.media ? _t("admin.x_image") : "");
      who.append(document.createTextNode(txt));
      if (m.deleted) who.append(" ", el("span", "del-badge", _t("admin.geloescht_4")));
      body.append(who);
      if (m.media && m.media.src && !m.deleted) {
        const img = el("img", "mod-gif");
        img.src = m.media.src; img.loading = "lazy"; img.alt = "";
        body.append(img);
      }
    } else body.append(el("div", "text missing", _t("admin.die_nachricht_gibt_es_nicht_mehr")));
    if (r.plaintext) {
      const p = el("div", "report-plain");
      p.append(el("b", null, _t("admin.verschluesselte_nachricht_laut_meldender")), document.createTextNode(r.plaintext));
      p.title = _t("admin.diesen_text_hat_der_browser_der");
      body.append(p);
    }
    body.append(el("div", "report-reason", _t("admin.gemeldet_von", { reporterName: r.reporterName }) + (r.reason ? `: „${r.reason}“` : "")));
    if (!open) body.append(el("div", "report-done", _t(r.action === "delete" ? "admin.x_deleted_by" : "admin.x_dismissed_by", { name: r.by || "?" }) + (r.closed ? " · " + timeFmt.format(new Date(r.closed)) : "")));
    const actions = el("div", "mod-actions");
    if (open) {
      const close = async (action, btn) => {
        btn.disabled = true;
        try { await api("POST", "/api/admin/reports/" + r.id, { action }); toast(action === "delete" ? _t("admin.nachricht_geloescht") : _t("admin.meldung_verworfen")); loadReports(); }
        catch (err) { toast(err.message, true); btn.disabled = false; }
      };
      if (m && !m.deleted) {
        const del = el("button", "mod-del", _t("admin.nachricht_loeschen"));
        del.type = "button";
        del.addEventListener("click", () => close("delete", del));
        actions.append(del);
      }
      const dis = el("button", "mod-del release", m && m.deleted ? _t("admin.erledigt") : _t("admin.verwerfen"));
      dis.type = "button";
      dis.addEventListener("click", () => close("dismiss", dis));
      actions.append(dis);
    }
    if (m && m.userId && !m.bot) {
      const person = el("button", "mod-del", _t("admin.person"));
      person.type = "button";
      person.addEventListener("click", () => openUserDialog({ id: m.userId, name: m.name }));
      actions.append(person);
    }
    if (!r.room.startsWith("dm-")) {
      const go = el("button", "mod-del", _t("admin.im_kanal_zeigen"));
      go.type = "button";
      go.addEventListener("click", () => { $("modRoom").value = r.room; location.hash = "#moderation"; stopModeration(); startModeration(); });
      actions.append(go);
    }
    li.append(avatar(m ? m.name : "?"), body, actions);
    return li;
  }

  // ---------- Wortfilter ----------

  async function loadFilter() {
    try {
      const f = await api("GET", "/api/admin/filter");
      $("filterPatterns").value = f.patterns.join("\n");
    } catch { /* egal */ }
  }
  $("filterForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("filterProblems").replaceChildren();
    try {
      const f = await api("PUT", "/api/admin/filter", { patterns: $("filterPatterns").value.split("\n") });
      $("filterPatterns").value = f.patterns.join("\n");
      $("filterSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("filterSaved").textContent = ""), 2500);
    } catch (err) {
      $("filterProblems").replaceChildren(...(err.problems || [err.message]).map((p) => el("li", null, p)));
    }
  });

  // ---------- Bilder und Profilbilder ----------

  async function loadUploads() {
    try {
      const u = await api("GET", "/api/admin/uploads");
      $("upMode").value = u.mode;
      $("upSize").value = String(u.size);
      $("upAvatars").checked = u.avatars;
    } catch { /* egal */ }
  }
  $("uploadsForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("PUT", "/api/admin/uploads", { mode: $("upMode").value, size: +$("upSize").value, avatars: $("upAvatars").checked });
      $("upSaved").textContent = _t("admin.gespeichert");
      setTimeout(() => ($("upSaved").textContent = ""), 2500);
    } catch (err) { toast(err.message, true); }
  });

  // ---------- Ankündigungen (geplante Nachrichten) ----------

  const SCHED_DAYS = [_t("admin.mo"), _t("admin.di"), _t("admin.mi"), _t("admin.do"), _t("admin.fr"), _t("admin.sa"), _t("admin.so")];
  const dtFmt = new Intl.DateTimeFormat(GoChatI18n.locale, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
  let schedules = [];
  let editingSched = null;

  async function loadSchedules() {
    try { schedules = await api("GET", "/api/admin/schedules"); } catch (err) { toast(err.message, true); return; }
    $("schedList").replaceChildren(...(schedules.length ? schedules.map(schedRow) : [el("li", "empty-row", _t("admin.noch_nichts_geplant_zum_beispiel_jeden"))]));
  }

  function schedWhen(s) {
    if (s.mode === "once") return "einmalig am " + dtFmt.format(new Date(s.at));
    if (s.mode === "daily") return _t("admin.taeglich_um_uhr", { time: s.time });
    return _t("admin.um_uhr", { days: s.weekdays.map((d) => SCHED_DAYS[d]).join(", "), time: s.time });
  }

  function schedRow(s) {
    const li = el("li", "sched-row" + (s.enabled ? "" : " off"));
    const main = el("div", "sched-main");
    const head = el("div", "sched-head");
    head.append(el("span", "chip", "#" + s.room), el("b", null, schedWhen(s)));
    if (s.pin) head.append(el("span", "chip", _t("admin.anpinnen_2")));
    if (!s.enabled) head.append(el("span", "off-badge", "aus"));
    main.append(head);
    const txt = el("p", "sched-text");
    if (window.GoChatFormat) window.GoChatFormat.render(txt, s.text, {});
    else txt.textContent = s.text;
    main.append(txt);
    const info = [];
    if (s.next) info.push(_t("admin.naechstes_mal") + dtFmt.format(new Date(s.next)));
    else if (s.enabled) info.push(_t("admin.kein_weiterer_termin"));
    if (s.lastRun) info.push(_t("admin.last_run", { date: dtFmt.format(new Date(s.lastRun)) }));
    const meta = el("div", "sched-meta", info.join(" · "));
    if (s.error) meta.append(el("span", "err", _t("admin.fehler_3") + s.error));
    main.append(meta);
    const act = el("div", "room-actions");
    const now = el("button", "btn-quiet small", _t("admin.jetzt_senden"));
    now.type = "button";
    now.addEventListener("click", async () => {
      try { await api("POST", `/api/admin/schedules/${s.id}/run`); toast(_t("admin.gesendet")); loadSchedules(); } catch (err) { toast(err.message, true); }
    });
    const ed = el("button", "icon-action");
    ed.type = "button"; ed.innerHTML = ICON_EDIT; ed.title = _t("admin.bearbeiten");
    ed.setAttribute("aria-label", _t("admin.ankuendigung_bearbeiten"));
    ed.addEventListener("click", () => openSchedDialog(s));
    act.append(now, ed);
    li.append(main, act);
    return li;
  }

  function syncSchedMode() {
    const mode = document.querySelector('input[name="schedMode"]:checked').value;
    $("schedAtField").hidden = mode !== "once";
    $("schedTimeField").hidden = mode === "once";
    $("schedDaysField").hidden = mode !== "weekly";
  }
  document.querySelectorAll('input[name="schedMode"]').forEach((x) => x.addEventListener("change", syncSchedMode));

  function openSchedDialog(s) {
    editingSched = s || null;
    $("schedTitle").textContent = s ? _t("admin.ankuendigung_bearbeiten") : _t("admin.ankuendigung_planen");
    $("schedRoom").replaceChildren(...rooms.map((r) => { const o = el("option", null, "#" + r.name); o.value = r.name; return o; }));
    $("schedRoom").value = s ? s.room : (rooms.find((r) => r.name === "lobby") || rooms[0] || {}).name || "";
    $("schedText").value = s ? s.text : "";
    const mode = s ? s.mode : "once";
    document.querySelector(`input[name="schedMode"][value="${mode}"]`).checked = true;
    const soon = new Date(Date.now() + 3600e3);
    soon.setMinutes(0, 0, 0);
    const local = (d) => new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
    $("schedAt").value = s && s.at ? s.at : local(soon);
    $("schedTime").value = s && s.time ? s.time : "09:00";
    const days = new Set(s && s.weekdays ? s.weekdays : [4]);
    $("schedDays").replaceChildren(...SCHED_DAYS.map((d, i) => {
      const b = el("button", "day-pick", d);
      b.type = "button";
      b.dataset.day = i;
      b.setAttribute("aria-pressed", String(days.has(i)));
      b.addEventListener("click", () => b.setAttribute("aria-pressed", String(b.getAttribute("aria-pressed") !== "true")));
      return b;
    }));
    $("schedPin").checked = s ? s.pin : false;
    $("schedOn").checked = s ? s.enabled : true;
    $("schedDelete").hidden = !s;
    $("schedError").textContent = "";
    syncSchedMode();
    $("schedDialog").showModal();
  }
  $("newSchedBtn").addEventListener("click", () => openSchedDialog(null));
  $("schedForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = {
      room: $("schedRoom").value, text: $("schedText").value,
      mode: document.querySelector('input[name="schedMode"]:checked').value,
      at: $("schedAt").value, time: $("schedTime").value,
      weekdays: [...$("schedDays").querySelectorAll('[aria-pressed="true"]')].map((b) => +b.dataset.day),
      pin: $("schedPin").checked, enabled: $("schedOn").checked,
    };
    try {
      if (editingSched) await api("PUT", "/api/admin/schedules/" + editingSched.id, body);
      else await api("POST", "/api/admin/schedules", body);
      $("schedDialog").close();
      toast(_t("admin.gespeichert"));
      loadSchedules();
    } catch (err) { $("schedError").textContent = err.message; }
  });
  $("schedDelete").addEventListener("click", () => {
    const s = editingSched;
    $("schedDialog").close();
    confirmAction(_t("admin.ankuendigung_loeschen"), _t("admin.sie_wird_nicht_mehr_gesendet_bereits"), _t("admin.loeschen"),
      async () => { await api("DELETE", "/api/admin/schedules/" + s.id); toast(_t("admin.geloescht")); loadSchedules(); });
  });

  // ---------- Start ----------

  (async () => {
    if (token) {
      try {
        await api("GET", "/api/admin/me");
        await showAdmin();
        return;
      } catch { /* Login anzeigen */ }
    }
    showLogin();
  })();
})();
