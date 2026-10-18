// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const h = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
  const langNames = (() => { try { return new Intl.DisplayNames([GoChatI18n.locale], { type: "language" }); } catch { return null; } })();
  const regionNames = (() => { try { return new Intl.DisplayNames([GoChatI18n.locale], { type: "region" }); } catch { return null; } })();
  const langName = (c) => { try { return (langNames && langNames.of(c)) || c; } catch { return c; } };
  const regionName = (c) => { try { return (regionNames && regionNames.of(c)) || c; } catch { return c; } };
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
  const num = new Intl.NumberFormat(GoChatI18n.locale);
  const ago = (t) => {
    const s = (Date.now() - new Date(t)) / 1000;
    if (s < 3600) return _t("beacon.ago_min", { n: Math.max(1, Math.round(s / 60)) });
    if (s < 86400) return _t("beacon.ago_h", { n: Math.round(s / 3600) });
    return _t("beacon.ago_d", { n: Math.round(s / 86400) });
  };
  const COLORS = ["#5848f4", "#0f8f7e", "#d1495b", "#2a7fd4", "#b05cc9", "#c46a12", "#3d8b3d", "#1f7a99"];
  const colorFor = (s) => { let x = 0; for (const c of s) x = (x * 31 + c.charCodeAt(0)) >>> 0; return COLORS[x % COLORS.length]; };
  let token = "";
  try { token = sessionStorage.getItem("beacon:admin") || ""; } catch { /* ohne */ }

  async function api(path, opts = {}) {
    const headers = Object.assign({}, opts.body && !(opts.body instanceof FormData) ? { "Content-Type": "application/json" } : {}, token ? { Authorization: "Bearer " + token } : {});
    const res = await fetch(path, Object.assign({}, opts, { headers }));
    const data = res.status === 204 ? null : await res.json().catch(() => null);
    if (res.status === 401 && path.startsWith("/api/admin/") && path !== "/api/admin/login") { setToken(""); showAdmin(); }
    if (!res.ok) throw new Error((data && data.error) || _t("beacon.das_hat_nicht_geklappt"));
    return data;
  }
  function setToken(t) { token = t; try { sessionStorage.setItem("beacon:admin", t); } catch { /* egal */ } }

  // ---------- Kopf, Statistik ----------
  let info = null;
  async function loadInfo() {
    try { info = await api("/api/v1/info"); } catch { return; }
    $("brandName").textContent = info.name;
    document.title = info.name;
    if (info.description) heroDefault = info.description;
    if (info.description && !location.hash.startsWith("#kanaele")) $("heroText").textContent = info.description;
    $("beaconUrl").textContent = info.url;
    renderFoot();
    const stats = [[_t("beacon.instanzen"), info.instances], [_t("beacon.nutzer_insgesamt"), info.users], [_t("beacon.gifs"), info.gifs], [_t("beacon.sammlungen"), info.collections]];
    $("stats").replaceChildren(...stats.map(([k, v]) => { const d = h("div"); d.append(h("dt", null, k), h("dd", null, num.format(v))); return d; }));
    const fill = (sel, map, label) => {
      const cur = sel.value;
      const opts = Object.entries(map).sort((a, b) => b[1] - a[1]).map(([c, n]) => new Option(`${label(c)} (${n})`, c));
      sel.replaceChildren(sel.options[0], ...opts);
      sel.value = cur;
    };
    fill($("instLang"), info.languages || {}, langName);
    fill($("instCountry"), info.countries || {}, (c) => `${flagEmoji(c)}${regionName(c)}`);
    renderTrustChips();
  }

  // Fußzeile: Lizenz und Quellcode dieses Beacons (AGPL)
  function renderFoot() {
    const f = $("pageFoot");
    const parts = [h("span", null, (info && info.name) || "UCNG Beacon")];
    if (info && info.source) {
      const a = h("a", null, _t("beacon.quellcode_agpl"));
      a.href = info.source; a.target = "_blank"; a.rel = "noopener";
      parts.push(a);
    } else parts.push(h("span", null, "AGPL-3.0"));
    const api = h("a", null, "API"); api.href = "/api/v1/info";
    parts.push(api);
    const out = [];
    parts.forEach((p, i) => { if (i) out.push(document.createTextNode(" · ")); out.push(p); });
    f.replaceChildren(...out);
  }

  // ---------- Tabs ----------
  function route() {
    const tab = (location.hash.slice(1) || "instanzen").split("/")[0];
    const t = ["instanzen", "kanaele", "gifs", "sprachpakete", "verwaltung"].includes(tab) ? tab : "instanzen";
    for (const id of ["instanzen", "kanaele", "gifs", "sprachpakete", "verwaltung"]) $("tab-" + id).hidden = id !== t;
    document.querySelectorAll(".tabs a").forEach((a) => (a.dataset.tab === t ? a.setAttribute("aria-current", "page") : a.removeAttribute("aria-current")));
    $("hero").hidden = t === "verwaltung" || t === "sprachpakete";
    $("heroTitle").textContent = t === "gifs" ? _t("beacon.gif_sammlungen_zum_abonnieren") : t === "kanaele" ? _t("beacon.kanaele_zum_abonnieren") : _t("beacon.finde_eine_gochat_instanz");
    if (t === "instanzen") loadInstances();
    if (t === "kanaele") loadChannels();
    $("heroText").textContent = t === "kanaele" ? _t("beacon.kanaele_hero") : t === "instanzen" ? heroDefault || _t("beacon.instanzen_melden_sich_hier_selbst_an_und") : $("heroText").textContent;
    if (t === "gifs") loadGifs(true);
    if (t === "verwaltung") showAdmin();
    if (t === "sprachpakete") loadLangPacks();
  }

  // ---------- Sprachpakete für Chat-Server ----------
  let langPacks = null;
  const pctFmt = new Intl.NumberFormat(GoChatI18n.locale, { style: "percent", maximumFractionDigits: 0 });
  function packFlag(p) {
    const region = (p.locale || "").split("-")[1];
    return region && /^[A-Z]{2}$/.test(region) ? flagImg(region) : h("span", "lp-code", p.code);
  }
  function packStats(p) {
    const bits = [_t("beacon.lp_texte", { n: p.texts, total: p.total })];
    if (p.outdated) bits.push(_t("beacon.lp_veraltet", { n: p.outdated }));
    bits.push(_t("beacon.lp_aktualisiert", { ago: ago(p.updated) }));
    return bits.join(" · ");
  }
  function downloadLink(href, text, cls) {
    const a = h("a", "btn small" + (cls ? " " + cls : ""), text);
    a.href = href;
    a.setAttribute("download", "");
    return a;
  }
  async function loadLangPacks() {
    try { langPacks = await api("/api/v1/languages"); } catch (e) { $("lpCount").textContent = e.message; return; }
    $("lpCount").textContent = _t("beacon.lp_anzahl", { n: langPacks.packs.length });
    if (!langPacks.packs.length) { $("lpList").replaceChildren(h("li", "empty", _t("beacon.lp_keine"))); }
    else $("lpList").replaceChildren(...langPacks.packs.map((p) => {
      const li = h("li", "lp");
      const head = h("div", "lp-top");
      const name = h("b", null, p.name);
      head.append(packFlag(p), name, h("span", "lp-code", p.code));
      if (p.official) head.append(h("span", "badge", _t("beacon.lp_eingebaut")));
      const bar = h("div", "lp-bar");
      const fill = h("span");
      fill.style.width = Math.round(p.complete * 100) + "%";
      bar.append(fill);
      bar.title = pctFmt.format(p.complete);
      const meta = h("p", "lp-meta", pctFmt.format(p.complete) + " · " + packStats(p));
      li.append(head, bar, meta);
      if (p.note) li.append(h("p", "lp-note", p.note));
      const act = h("div", "lp-actions");
      const code = encodeURIComponent(p.code);
      act.append(downloadLink("/api/v1/languages/" + code + "?download=1", _t("beacon.lp_datei_laden")));
      if (p.complete < 1 || p.outdated) act.append(downloadLink("/api/v1/languages/" + code + "/package?open=1", _t("beacon.lp_paket_offen"), "primary"));
      act.append(downloadLink("/api/v1/languages/" + code + "/package", _t("beacon.lp_paket_voll")));
      li.append(act);
      return li;
    }));
    if (token) renderLangPackAdmin();
  }
  $("lpNewForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("lpNewErr").textContent = "";
    const code = $("lpNewCode").value.trim();
    const q = new URLSearchParams({ name: $("lpNewName").value.trim(), locale: $("lpNewLocale").value.trim() });
    // Fehler (z. B. ungültiges Kürzel) anzeigen statt eine Fehlerseite herunterzuladen
    const res = await fetch("/api/v1/languages/" + encodeURIComponent(code) + "/package?" + q);
    if (!res.ok) { const d = await res.json().catch(() => ({})); $("lpNewErr").textContent = d.error || _t("beacon.das_hat_nicht_geklappt"); return; }
    const a = document.createElement("a");
    a.href = URL.createObjectURL(await res.blob());
    a.download = "ucng-" + code + "-translation.json";
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 2000);
  });

  // Verwaltung der Sprachpakete
  function renderLangPackAdmin() {
    const list = (langPacks && langPacks.packs) || [];
    if (!list.length) { $("lpAdmin").replaceChildren(h("li", null, _t("beacon.lp_keine"))); return; }
    $("lpAdmin").replaceChildren(...list.map((p) => {
      const li = h("li");
      const t = h("div", "grow");
      const top = h("div");
      top.append(h("b", null, p.name + " (" + p.code + ")"));
      t.append(top, h("small", null, pctFmt.format(p.complete) + " · " + packStats(p) + (p.note ? " · " + p.note : "")));
      const act = h("div", "lp-actions");
      const edit = h("button", "btn small", _t("beacon.lp_bearbeiten"));
      edit.type = "button";
      edit.addEventListener("click", () => {
        $("lpCode").value = p.code; $("lpName").value = p.name; $("lpLocale").value = p.locale || ""; $("lpDir").value = p.dir || ""; $("lpNote").value = p.note || "";
        $("lpUpForm").scrollIntoView({ behavior: "smooth", block: "center" });
        $("lpFile").focus();
      });
      const del = h("button", "btn small danger", _t("beacon.loeschen"));
      del.type = "button";
      del.addEventListener("click", async () => {
        if (!confirm(_t("beacon.lp_loeschen_frage", { name: p.name }))) return;
        try { await api("/api/admin/langpacks/" + encodeURIComponent(p.code), { method: "DELETE" }); loadLangPacks(); } catch (err) { $("lpErr").textContent = err.message; }
      });
      act.append(downloadLink("/api/v1/languages/" + encodeURIComponent(p.code) + "/package?open=1", _t("beacon.lp_paket_offen")), edit, del);
      li.append(t, act);
      return li;
    }));
  }
  $("lpUpForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("lpErr").textContent = "";
    const code = $("lpCode").value.trim();
    const f = $("lpFile").files[0];
    const btn = e.submitter || $("lpUpForm").querySelector("[type=submit]");
    btn.disabled = true;
    try {
      if (!f) {
        await api("/api/admin/langpacks/" + encodeURIComponent(code), { method: "PATCH", body: JSON.stringify({ name: $("lpName").value.trim(), locale: $("lpLocale").value.trim(), dir: $("lpDir").value, note: $("lpNote").value.trim() }) });
        toast(_t("beacon.gespeichert"));
      } else {
        const q = new URLSearchParams({ note: $("lpNote").value.trim(), dir: $("lpDir").value });
        if ($("lpName").value.trim()) q.set("name", $("lpName").value.trim());
        if ($("lpLocale").value.trim()) q.set("locale", $("lpLocale").value.trim());
        if ($("lpMode").value === "replace") q.set("replace", "1");
        const r = await api("/api/admin/langpacks/" + encodeURIComponent(code) + "?" + q, { method: "POST", body: await f.text() });
        const res = r.result;
        let msg = _t("beacon.lp_ergebnis", { applied: res.applied, unknown: res.unknown, empty: res.unchanged });
        if (res.errors && res.errors.length) msg += " " + _t("beacon.lp_fehler", { n: res.errors.length, list: res.errors.slice(0, 3).join("; ") });
        toast(msg, !!(res.errors && res.errors.length));
        $("lpFile").value = "";
      }
      loadLangPacks();
    } catch (err) { $("lpErr").textContent = err.message; }
    finally { btn.disabled = false; }
  });
  window.addEventListener("hashchange", () => { route(); loadInfo(); });

  // ---------- Instanzen ----------
  let instTimer = 0;
  let trustSel = "";
  const TRUST = {
    official: [_t("beacon.offiziell"), _t("beacon.vom_team_dieses_beacons_bzw_vom_projekt")],
    recommended: [_t("beacon.empfohlen"), _t("beacon.vom_team_des_beacons_geprueft_und")],
    standard: [_t("beacon.standard"), _t("beacon.vom_team_gesichtet_und_unauffaellig")],
    "": [_t("beacon.community"), _t("beacon.nicht_geprueft_kann_gut_sein_oder_auch")],
  };
  const TRUST_ICON = {
    official: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.2 2.5 3.4v4c0 3.3 2.3 6.1 5.5 7.4 3.2-1.3 5.5-4.1 5.5-7.4v-4L8 1.2Z" fill="currentColor"/><path d="m5.4 8 1.8 1.8 3.4-3.6" fill="none" stroke="var(--surface)" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    recommended: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="m8 1.5 1.9 4 4.3.5-3.2 3 .9 4.3L8 11.1l-3.9 2.2.9-4.3-3.2-3 4.3-.5L8 1.5Z" fill="currentColor"/></svg>',
    standard: '<svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="6.3" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m5.3 8.1 1.9 1.9 3.6-3.8" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    "": '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.8 14.6 13.6H1.4L8 1.8Z" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="M8 6.2v3.4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><circle cx="8" cy="11.6" r=".9" fill="currentColor"/></svg>',
  };
  function trustPill(level, note) {
    const [label, hint] = TRUST[level] || TRUST[""];
    const p = h("span", "trust " + (level || "community"));
    p.innerHTML = TRUST_ICON[level || ""];
    p.append(document.createTextNode(label));
    p.title = note ? `${label}: ${note}` : hint;
    return p;
  }
  function renderTrustChips() {
    const c = (info && info.trust) || {};
    const total = (c.official || 0) + (c.recommended || 0) + (c.standard || 0) + (c.community || 0);
    const chips = [["", _t("beacon.alle"), total], ["official", _t("beacon.offiziell"), c.official], ["recommended", _t("beacon.empfohlen"), c.recommended], ["standard", _t("beacon.standard"), c.standard], ["community", _t("beacon.community"), c.community]];
    $("trustChips").replaceChildren(...chips.map(([id, label, n]) => {
      const b = h("button", "chip" + (id ? " t-" + id : ""));
      b.type = "button";
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", String(id === trustSel));
      b.append(label, h("span", "n", num.format(n || 0)));
      b.addEventListener("click", () => { trustSel = id; renderTrustChips(); loadInstances(); });
      return b;
    }));
  }
  async function loadInstances() {
    const p = new URLSearchParams({ q: $("instQ").value.trim(), language: $("instLang").value, country: $("instCountry").value, sort: $("instSort").value });
    if (trustSel) p.set("trust", trustSel);
    let d;
    try { d = await api("/api/v1/instances?" + p); } catch (e) { $("instCount").textContent = e.message; return; }
    $("instCount").textContent = _t("beacon.n_instances", { n: d.total });
    if (!d.instances.length) { $("instList").replaceChildren(h("li", "empty", _t("beacon.keine_instanz_gefunden"))); return; }
    $("instList").replaceChildren(...d.instances.map((x) => {
      const li = h("li", "inst trust-" + (x.trust || "community"));
      const head = h("div", "inst-head");
      const mark = h("span", "inst-mark", (x.name || x.domain).slice(0, 1).toUpperCase());
      mark.style.background = colorFor(x.domain);
      const t = h("div");
      const name = h("div", "inst-name");
      const a = h("a", null, x.name);
      a.href = x.url; a.target = "_blank"; a.rel = "noopener";
      name.append(a);
      t.append(name, h("div", "inst-domain", x.domain));
      head.append(mark, t, trustPill(x.trust, x.trustNote));
      li.append(head);
      if (x.trustNote && x.trust) li.append(h("p", "inst-note", x.trustNote));
      if (x.description) li.append(h("p", "inst-desc", x.description));
      const b = h("div", "badges");
      b.append(h("span", "badge users", _t("beacon.n_users", { n: x.users })));
      if (x.language) b.append(h("span", "badge", langName(x.language)));
      if (x.country) { const c = h("span", "badge country"); c.append(flagImg(x.country), regionName(x.country)); b.append(c); }
      b.append(h("span", "badge", x.mode === "hosted" ? _t("beacon.konten") : _t("beacon.namen_ohne_konto")));
      if (x.federation) b.append(Object.assign(h("span", "badge ok", _t("beacon.foederation")), { title: _t("beacon.erreichbar_als_name", { domain: x.domain }) }));
      if (x.mode === "hosted") b.append(h("span", "badge" + (x.registration ? " ok" : ""), x.registration ? _t("beacon.registrierung_offen") : _t("beacon.nur_mit_einladung")));
      if (x.url && x.url.startsWith("http://")) b.append(Object.assign(h("span", "badge warn", _t("beacon.ohne_https")), { title: _t("beacon.die_instanz_ist_nur_unverschluesselt") }));
      if (x.port) b.append(Object.assign(h("span", "badge", _t("beacon.eigener_port")), { title: _t("beacon.die_adresse_enthaelt_einen_port") }));
      li.append(b);
      const foot = h("div", "inst-foot");
      foot.append(h("span", null, _t("beacon.gemeldet_ago", { ago: ago(x.last) })));
      // Quellcode der Fassung (AGPL): Änderungen an föderierten Instanzen sind offenzulegen
      if (x.source) {
        const src = h("a", "src-link", _t("beacon.quellcode"));
        src.href = x.source; src.target = "_blank"; src.rel = "noopener nofollow";
        if (x.version) src.title = _t("beacon.quellcode_version", { version: x.version });
        foot.append(src);
      }
      const go = h("a", "btn small", _t("beacon.oeffnen"));
      go.href = x.url; go.target = "_blank"; go.rel = "noopener";
      foot.append(go);
      li.append(foot);
      return li;
    }));
  }
  $("instFilters").addEventListener("input", () => { clearTimeout(instTimer); instTimer = setTimeout(loadInstances, 250); });

  // ---------- Kanäle (Kanal-Föderation) ----------
  var heroDefault = "";
  let chanTimer = 0;
  async function loadChannels() {
    const p = new URLSearchParams({ q: $("chanQ").value.trim(), mode: $("chanMode").value });
    let d;
    try { d = await api("/api/v1/channels?" + p); } catch (e) { $("chanCount").textContent = e.message; return; }
    $("chanCount").textContent = _t("beacon.n_channels", { n: d.total });
    if (!d.channels.length) { $("chanList").replaceChildren(h("li", "empty", _t("beacon.kein_kanal_gefunden"))); return; }
    $("chanList").replaceChildren(...d.channels.map((c) => {
      const li = h("li", "inst trust-" + (c.trust || "community"));
      const head = h("div", "inst-head");
      const mark = h("span", "inst-mark", "#");
      mark.style.background = colorFor(c.domain);
      const t = h("div");
      t.append(h("div", "inst-name", "#" + c.name), h("div", "inst-domain", c.title && c.title !== c.domain ? c.title + " · " + c.domain : c.domain));
      head.append(mark, t, trustPill(c.trust, ""));
      li.append(head);
      if (c.description) li.append(h("p", "inst-desc", c.description));
      const b = h("div", "badges");
      b.append(h("span", "badge" + (c.mode === "write" ? " ok" : ""), c.mode === "write" ? _t("beacon.mitschreiben") : _t("beacon.nur_lesen")));
      if (c.messages) b.append(h("span", "badge", _t("beacon.n_messages", { n: c.messages })));
      if (c.language) b.append(h("span", "badge", langName(c.language)));
      li.append(b, h("code", "chan-addr", c.address));
      const foot = h("div", "inst-foot");
      foot.append(h("span", null, c.lastAt && !c.lastAt.startsWith("0001") ? _t("beacon.zuletzt_aktiv", { ago: ago(c.lastAt) }) : ""));
      const copy = h("button", "btn small", _t("beacon.adresse_kopieren"));
      copy.type = "button";
      copy.addEventListener("click", async () => {
        try { await navigator.clipboard.writeText(c.address); copy.textContent = _t("beacon.kopiert"); setTimeout(() => (copy.textContent = _t("beacon.adresse_kopieren")), 1500); }
        catch { window.prompt(_t("beacon.adresse_kopieren"), c.address); }
      });
      foot.append(copy);
      li.append(foot);
      return li;
    }));
  }
  $("chanFilters").addEventListener("input", () => { clearTimeout(chanTimer); chanTimer = setTimeout(loadChannels, 250); });
  $("chanFilters").addEventListener("submit", (e) => e.preventDefault());
  $("instFilters").addEventListener("submit", (e) => e.preventDefault());

  // ---------- GIFs ----------
  let collections = [];
  let collSel = "";
  let gifOffset = 0;
  let gifTimer = 0;
  async function loadCollections() {
    try { collections = await api("/api/v1/collections"); } catch { collections = []; }
    const chips = [["", _t("beacon.alle"), null], ...collections.map((c) => [c.id, c.name, c.count])].map(([id, name, n]) => {
      const b = h("button", "chip", name);
      b.type = "button";
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", String(id === collSel));
      if (n != null) b.append(h("span", "n", String(n)));
      b.addEventListener("click", () => { collSel = id; loadCollections(); loadGifs(true); });
      return b;
    });
    $("collChips").replaceChildren(...chips);
    const c = collections.find((x) => x.id === collSel);
    const box = $("subscribeBox");
    box.hidden = !info;
    if (info) {
      $("subTitle").textContent = c ? _t("beacon.sammlung_abonnieren", { name: c.name }) : _t("beacon.alle_sammlungen_abonnieren");
      $("subText").textContent = (c && c.description ? c.description + " – " : "") +
        _t("beacon.fuege_diese_adresse_in_deiner_gochat");
      $("subAddr").textContent = info.url + (c ? "#" + c.id : "");
    }
  }
  $("subCopy").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText($("subAddr").textContent); $("subCopy").textContent = _t("beacon.kopiert"); setTimeout(() => ($("subCopy").textContent = _t("beacon.adresse_kopieren")), 1600); } catch { /* markieren */ }
  });

  function tile(g, onClick, add) {
    const b = h("button", "gif-tile" + (add ? " add" : ""));
    b.type = "button";
    const img = h("img");
    img.src = g.preview || g.url; img.alt = g.title || "GIF"; img.loading = "lazy";
    if (g.w && g.h) { img.width = g.w; img.height = g.h; }
    b.append(img);
    if (g.title) b.append(h("span", "t", g.title));
    b.addEventListener("click", () => onClick(g));
    return b;
  }

  async function loadGifs(reset) {
    if (reset) { gifOffset = 0; if (!collections.length) await loadCollections(); }
    const p = new URLSearchParams({ q: $("gifQ").value.trim(), collection: collSel, limit: "40", offset: String(gifOffset) });
    let d;
    try { d = await api("/api/v1/gifs?" + p); } catch (e) { $("gifCount").textContent = e.message; return; }
    const tiles = d.gifs.map((g) => tile(g, openGif));
    if (reset) $("gifGrid").replaceChildren(...tiles); else $("gifGrid").append(...tiles);
    gifOffset += d.gifs.length;
    $("gifCount").textContent = _t("beacon.n_gifs", { n: d.total });
    $("gifMore").hidden = gifOffset >= d.total;
    if (!d.total) $("gifGrid").replaceChildren(h("p", "empty", _t("beacon.keine_gifs_gefunden")));
  }
  $("gifQ").addEventListener("input", () => { clearTimeout(gifTimer); gifTimer = setTimeout(() => loadGifs(true), 250); });
  $("gifMore").addEventListener("click", () => loadGifs(false));

  let dlgGif = null;
  function openGif(g) {
    dlgGif = g;
    $("gdImg").src = g.url; $("gdImg").alt = g.title || "GIF";
    $("gdTitle").textContent = g.title || _t("beacon.ohne_titel");
    $("gdTags").textContent = g.tags && g.tags.length ? _t("beacon.schlagwoerter", { tags: g.tags.join(", ") }) : "";
    const names = (g.collections || []).map((id) => (collections.find((c) => c.id === id) || {}).name).filter(Boolean);
    $("gdColls").textContent = names.length ? _t("beacon.sammlungen_2", { names: names.join(", ") }) : "";
    $("gdEdit").hidden = !token;
    if (token) {
      $("gdTitleIn").value = g.title || "";
      $("gdTagsIn").value = (g.tags || []).join(", ");
      $("gdCollsIn").replaceChildren(...collChecks(g.collections || []));
    }
    $("gifDialog").showModal();
  }
  function collChecks(sel) {
    return collections.map((c) => {
      const l = h("label");
      const i = h("input"); i.type = "checkbox"; i.value = c.id; i.checked = sel.includes(c.id);
      l.append(i, document.createTextNode(c.name));
      return l;
    });
  }
  const checked = (box) => [...$(box).querySelectorAll("input:checked")].map((i) => i.value);
  const tagList = (s) => s.split(",").map((x) => x.trim()).filter(Boolean);
  $("gdSave").addEventListener("click", async () => {
    try {
      await api("/api/admin/gifs/" + dlgGif.id, { method: "PATCH", body: JSON.stringify({ title: $("gdTitleIn").value, tags: tagList($("gdTagsIn").value), collections: checked("gdCollsIn") }) });
      $("gifDialog").close(); refreshAll();
    } catch (e) { alert(e.message); }
  });
  $("gdDelete").addEventListener("click", async () => {
    if (!confirm(_t("beacon.dieses_gif_loeschen_instanzen_die_es"))) return;
    try { await api("/api/admin/gifs/" + dlgGif.id, { method: "DELETE" }); $("gifDialog").close(); refreshAll(); } catch (e) { alert(e.message); }
  });

  // ---------- Verwaltung ----------
  async function showAdmin() {
    $("loginForm").hidden = !!token;
    $("adminArea").hidden = !token;
    if (token) refreshAdmin();
  }
  $("loginForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("loginErr").textContent = "";
    try { const r = await api("/api/admin/login", { method: "POST", body: JSON.stringify({ password: $("loginPw").value }) }); setToken(r.token); $("loginPw").value = ""; showAdmin(); }
    catch (err) { $("loginErr").textContent = err.message; }
  });
  $("logoutBtn").addEventListener("click", async () => { await api("/api/admin/logout", { method: "POST" }).catch(() => {}); setToken(""); showAdmin(); });

  // Kurze Hinweise unten auf der Seite
  let toastTimer = 0;
  function toast(text, isError) {
    let t = $("toast");
    if (!t) { t = h("div", "toast"); t.id = "toast"; t.setAttribute("role", "status"); document.body.append(t); }
    t.textContent = text;
    t.classList.toggle("error", !!isError);
    t.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove("show"), isError ? 6000 : 2600);
  }

  // Sprachen: gemeinsamer Editor (i18n-editor.js)
  let i18nEditor = null;
  function loadI18nEditor() {
    if (!i18nEditor) {
      i18nEditor = GoChatI18nEditor.mount($("i18nRoot"), {
        request: (method, path, body) => api(path, { method, body: body === undefined ? undefined : JSON.stringify(body) }),
        raw: (method, path, body, type) => fetch(path, { method, body, headers: Object.assign({ Authorization: "Bearer " + token }, type ? { "Content-Type": type } : {}) }),
        toast,
        areas: [["beacon.", _t("beacon.area_beacon")], ["i18ned.", _t("beacon.area_editor")], ["srv.", _t("beacon.area_srv")], ["common.", _t("beacon.area_common")]],
      });
    }
    i18nEditor.load();
  }

  async function refreshAdmin() {
    try {
      const [cfg, inst] = await Promise.all([api("/api/admin/config"), api("/api/admin/instances")]);
      $("cfgName").value = cfg.name; $("cfgDesc").value = cfg.description || ""; $("cfgCredit").value = cfg.credit || ""; $("cfgProvider").value = cfg.provider || "";
      $("cfgKey").value = cfg.apiKey || ""; $("cfgRating").value = cfg.rating || "g";
      renderAdminInst(inst);
    } catch { return; }
    await loadCollections();
    renderColls();
    $("addColls").replaceChildren(...collChecks(collSel ? [collSel] : []));
    loadAdminGifs();
    loadI18nEditor();
    loadLangPacks();
  }
  function refreshAll() { loadInfo(); loadCollections().then(() => { if (token) { renderColls(); loadAdminGifs(); } }); if (!$("tab-gifs").hidden) loadGifs(true); }

  $("cfgForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("/api/admin/config", { method: "PUT", body: JSON.stringify({ name: $("cfgName").value, description: $("cfgDesc").value, credit: $("cfgCredit").value, provider: $("cfgProvider").value, apiKey: $("cfgKey").value, rating: $("cfgRating").value }) });
      $("cfgSaved").textContent = _t("beacon.gespeichert"); setTimeout(() => ($("cfgSaved").textContent = ""), 2000); loadInfo();
    } catch (err) { alert(err.message); }
  });

  function renderColls() {
    const list = $("collList");
    if (!collections.length) { list.replaceChildren(h("li", null, _t("beacon.noch_keine_sammlung"))); return; }
    list.replaceChildren(...collections.map((c) => {
      const li = h("li");
      const t = h("div", "grow");
      t.append(h("b", null, c.name), h("small", null, _t("beacon.n_gifs", { n: c.count }) + " · " + c.id + (c.description ? " · " + c.description : "")));
      const ren = h("button", "btn small", _t("beacon.umbenennen"));
      ren.type = "button";
      ren.addEventListener("click", async () => {
        const n = prompt(_t("beacon.name_der_sammlung"), c.name); if (!n) return;
        const d = prompt(_t("beacon.beschreibung"), c.description || "");
        try { await api("/api/admin/collections/" + c.id, { method: "PATCH", body: JSON.stringify({ name: n, description: d || "" }) }); refreshAll(); } catch (e) { alert(e.message); }
      });
      const del = h("button", "btn small danger", _t("beacon.loeschen"));
      del.type = "button";
      del.addEventListener("click", async () => {
        if (!confirm(_t("beacon.sammlung_loeschen_die_gifs_bleiben", { name: c.name }))) return;
        try { await api("/api/admin/collections/" + c.id, { method: "DELETE" }); if (collSel === c.id) collSel = ""; refreshAll(); } catch (e) { alert(e.message); }
      });
      li.append(t, ren, del);
      return li;
    }));
  }
  $("collForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try { await api("/api/admin/collections", { method: "POST", body: JSON.stringify({ name: $("collName").value, description: $("collDesc").value }) }); $("collName").value = ""; $("collDesc").value = ""; await refreshAdmin(); loadInfo(); }
    catch (err) { alert(err.message); }
  });

  async function addDone(p) {
    $("addErr").textContent = "";
    try { await p; refreshAll(); return true; } catch (e) { $("addErr").textContent = e.message; return false; }
  }
  $("upForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const fd = new FormData();
    fd.append("file", $("upFile").files[0]); fd.append("title", $("upTitle").value); fd.append("tags", $("upTags").value); fd.append("collections", checked("addColls").join(","));
    if (await addDone(api("/api/admin/gifs", { method: "POST", body: fd }))) { $("upForm").reset(); }
  });
  $("urlForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (await addDone(api("/api/admin/gifs", { method: "POST", body: JSON.stringify({ url: $("urlUrl").value, title: $("urlTitle").value, tags: tagList($("urlTags").value), collections: checked("addColls") }) }))) $("urlForm").reset();
  });
  $("webForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("addErr").textContent = "";
    try {
      const list = await api("/api/admin/web?q=" + encodeURIComponent($("webQ").value));
      $("webGrid").replaceChildren(...list.map((g) => tile({ url: g.preview, title: g.title, w: g.w, h: g.h, id: g.id }, async (x) => {
        if (await addDone(api("/api/admin/gifs", { method: "POST", body: JSON.stringify({ web: x.id, title: x.title, collections: checked("addColls") }) }))) $("addErr").textContent = "";
      }, true)));
    } catch (err) { $("addErr").textContent = err.message; }
  });

  async function loadAdminGifs() {
    try {
      const d = await api("/api/v1/gifs?limit=100");
      $("adminGifs").replaceChildren(...d.gifs.map((g) => tile(g, openGif)));
      if (!d.gifs.length) $("adminGifs").replaceChildren(h("p", "empty", _t("beacon.noch_keine_gifs")));
    } catch { /* egal */ }
  }

  function renderAdminInst(list) {
    if (!list.length) { $("adminInst").replaceChildren(h("li", null, _t("beacon.noch_hat_sich_keine_instanz_gemeldet"))); return; }
    $("adminInst").replaceChildren(...list.map((x) => {
      const li = h("li", "inst-row" + (x.hidden ? " hidden" : ""));
      const t = h("div", "grow");
      const top = h("div", "inst-row-top");
      const a = h("a", null, x.name); a.href = x.url; a.target = "_blank"; a.rel = "noopener";
      top.append(a, trustPill(x.trust, x.trustNote));
      t.append(top, h("small", null, _t("beacon.nutzer_gemeldet_schluessel", { domain: x.domain, users: x.users, ago: ago(x.last), key: x.key })));
      if (x.keyChanged) t.append(h("small", "warn", _t("beacon.der_serverschluessel_hat_sich_seit_der")));
      const tr = h("div", "trust-edit");
      const sel = h("select");
      sel.setAttribute("aria-label", _t("beacon.stufe_fuer", { domain: x.domain }));
      for (const [v, l] of [["", _t("beacon.community")], ["standard", _t("beacon.standard")], ["recommended", _t("beacon.empfohlen")], ["official", _t("beacon.offiziell")]]) sel.append(new Option(l, v));
      sel.value = x.trust || "";
      const note = h("input");
      note.maxLength = 80; note.placeholder = _t("beacon.hinweis_optional"); note.value = x.trustNote || "";
      note.setAttribute("aria-label", _t("beacon.hinweis_fuer", { domain: x.domain }));
      const save = h("button", "btn small primary", _t("beacon.uebernehmen"));
      save.type = "button";
      save.addEventListener("click", async () => {
        try { await api("/api/admin/trust/" + encodeURIComponent(x.domain), { method: "PUT", body: JSON.stringify({ level: sel.value, note: note.value }) }); refreshAdmin(); loadInfo(); }
        catch (e) { alert(e.message); }
      });
      tr.append(sel, note, save);
      t.append(tr);
      const hide = h("button", "btn small", x.hidden ? _t("beacon.wieder_zeigen") : _t("beacon.ausblenden"));
      hide.type = "button";
      hide.addEventListener("click", async () => { try { await api("/api/admin/instances/" + encodeURIComponent(x.domain), { method: "PATCH", body: JSON.stringify({ hidden: !x.hidden }) }); refreshAdmin(); loadInfo(); } catch (e) { alert(e.message); } });
      const del = h("button", "btn small danger", _t("beacon.entfernen"));
      del.type = "button";
      del.title = _t("beacon.die_instanz_kann_sich_danach_erneut");
      del.addEventListener("click", async () => { if (!confirm(_t("beacon.entfernen_2", { domain: x.domain }))) return; try { await api("/api/admin/instances/" + encodeURIComponent(x.domain), { method: "DELETE" }); refreshAdmin(); loadInfo(); } catch (e) { alert(e.message); } });
      const acts = h("div", "inst-acts");
      acts.append(hide, del);
      li.append(t, acts);
      return li;
    }));
    loadTrustPending(list);
  }

  async function loadTrustPending(list) {
    let all;
    try { all = await api("/api/admin/trust"); } catch { return; }
    const known = new Set(list.map((x) => x.domain));
    const rest = all.filter((t) => !known.has(t.domain));
    $("trustPending").replaceChildren(...rest.map((t) => {
      const li = h("li");
      const g = h("div", "grow");
      const top = h("div", "inst-row-top");
      top.append(h("b", null, t.domain), trustPill(t.level, t.note));
      g.append(top, h("small", null, t.key ? _t("beacon.gebunden_an_schluessel_noch_nicht_wieder", { key: t.key }) : _t("beacon.wartet_auf_die_erste_meldung")));
      const del = h("button", "btn small", _t("beacon.zuruecknehmen"));
      del.type = "button";
      del.addEventListener("click", async () => { try { await api("/api/admin/trust/" + encodeURIComponent(t.domain), { method: "PUT", body: JSON.stringify({ level: "" }) }); refreshAdmin(); } catch (e) { alert(e.message); } });
      li.append(g, del);
      return li;
    }));
  }

  $("trustForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const d = $("trustDomain").value.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/\/.*$/, "");
    try {
      await api("/api/admin/trust/" + encodeURIComponent(d), { method: "PUT", body: JSON.stringify({ level: $("trustLevel").value, note: $("trustNote").value }) });
      $("trustForm").reset(); refreshAdmin(); loadInfo();
    } catch (err) { alert(err.message); }
  });

  loadInfo().then(route);
})();
