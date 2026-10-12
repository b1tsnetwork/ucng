// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
/*!
 * UCNG – Sprach-Editor für die Verwaltung (Chat und Beacon)
 *
 *   GoChatI18nEditor.mount(element, {
 *     request(method, path, body) → Promise<JSON>   (mit Anmeldung; wirft Error mit Meldung)
 *     raw(method, path, body, contentType) → Promise<Response>
 *     toast(text, isError)
 *     areas: [["app.", "Chat"], …]                  (Filter nach Bereich, optional)
 *   }) → { load() }
 *
 * Braucht i18n.js (_t, GoChatI18n) und i18n-editor.css. Texte: Schlüssel „i18ned.*“.
 * Dieselbe Datei nutzt der Beacon (cmd/gochat-beacon/web/i18n-editor.js – bitte gleich halten).
 */
(function () {
  "use strict";

  var BASE = "/api/admin/i18n";
  var PAGE = 40;

  function h(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function btn(cls, text, onClick) {
    var b = h("button", "ie-btn" + (cls ? " " + cls : ""), text);
    b.type = "button";
    if (onClick) b.addEventListener("click", onClick);
    return b;
  }
  function t(k, v) { return window._t(k, v); }

  function mount(root, host) {
    var info = null;      // {languages, default, auto, source}
    var lang = "";        // im Editor geöffnete Sprache
    var entries = [];
    var filter = "all";
    var shown = 0;
    var listCache = [];
    var pct = new Intl.NumberFormat(GoChatI18n.locale, { style: "percent", maximumFractionDigits: 0 });
    var num = new Intl.NumberFormat(GoChatI18n.locale);

    root.classList.add("ie");
    root.replaceChildren();

    // --- Einstellungen ---
    var settings = h("section", "ie-card");
    settings.append(h("h2", "ie-title", t("i18ned.einstellungen")));
    var defLabel = h("label", "ie-field");
    var defSel = h("select");
    defLabel.append(h("span", null, t("i18ned.standardsprache")), defSel, h("small", null, t("i18ned.standard_hint")));
    var autoRow = h("label", "ie-switch-row");
    var autoText = h("span");
    autoText.append(h("strong", null, t("i18ned.auto")), h("small", null, t("i18ned.auto_hint")));
    var autoCb = h("input", "ie-switch");
    autoCb.type = "checkbox";
    autoRow.append(autoText, autoCb);
    settings.append(defLabel, autoRow);

    // --- Sprachen ---
    var langsCard = h("section", "ie-card ie-flush");
    var langsHead = h("div", "ie-head");
    var lh = h("div");
    lh.append(h("h2", "ie-title", t("i18ned.sprachen")), h("p", "ie-hint", t("i18ned.liste_hint")));
    langsHead.append(lh, btn("small", t("i18ned.hinzufuegen"), function () { openLangDialog(null); }));
    var langList = h("ul", "ie-langs");
    langsCard.append(langsHead, langList);

    // --- Editor ---
    var ed = h("section", "ie-card ie-editor");
    ed.hidden = true;
    var edHead = h("div", "ie-head ie-head-plain");
    var edTitle = h("h2", "ie-title");
    edHead.append(edTitle, btn("small", t("i18ned.schliessen"), close));
    var tools = h("div", "ie-tools");
    var search = h("input");
    search.type = "search";
    search.placeholder = t("i18ned.suchen");
    search.autocomplete = "off";
    search.setAttribute("aria-label", t("i18ned.suchen"));
    var area = h("select");
    area.setAttribute("aria-label", t("i18ned.bereich"));
    var areas = [["", t("i18ned.alle_bereiche")]].concat(host.areas || []);
    areas.forEach(function (a) { var o = h("option", null, a[1]); o.value = a[0]; area.append(o); });
    tools.append(search, area);
    var filters = h("div", "ie-seg");
    filters.setAttribute("role", "tablist");
    var FILTERS = [["all", "i18ned.f_alle"], ["missing", "i18ned.f_fehlend"], ["changed", "i18ned.f_geaendert"], ["outdated", "i18ned.f_veraltet"]];
    FILTERS.forEach(function (f) {
      var b = btn("", t(f[1]), function () {
        filter = f[0];
        filters.querySelectorAll("button").forEach(function (x) { x.setAttribute("aria-selected", x === b ? "true" : "false"); });
        render(true);
      });
      b.dataset.f = f[0];
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", f[0] === "all" ? "true" : "false");
      filters.append(b);
    });
    var count = h("p", "ie-hint");
    var list = h("ul", "ie-list");
    var moreWrap = h("div", "ie-more");
    var more = btn("", "", function () { render(false); });
    moreWrap.append(more);
    var help = h("details", "ie-help");
    help.append(h("summary", null, t("i18ned.hilfe")));
    var helpBody = h("div");
    helpBody.append(_tH("i18ned.hilfe_text"));
    help.append(helpBody);

    var io = h("div", "ie-io");
    var ex = h("div");
    ex.append(h("h3", null, t("i18ned.exportieren")));
    var exRow = h("div", "ie-inline");
    var exFull = btn("small", t("i18ned.export_voll"), function () { download("/file/" + encodeURIComponent(lang) + "?full=1", lang + ".json", exFull); });
    var exDiff = btn("small", t("i18ned.export_diff"), function () { download("/file/" + encodeURIComponent(lang) + "?full=0", lang + "-changes.json", exDiff); });
    exRow.append(exFull, exDiff);
    ex.append(exRow);
    // Übersetzungspaket für eine KI (oder Übersetzer): Anleitung, Ausgangstexte, Platzhalter
    var ai = h("div", "ie-ai");
    ai.append(h("h3", null, t("i18ned.ki_titel")));
    var aiText = h("p", "ie-hint");
    aiText.append(_tH("i18ned.ki_text"));
    var aiRow = h("div", "ie-inline");
    var aiOpenLab = h("label", "ie-check");
    var aiOpen = h("input");
    aiOpen.type = "checkbox";
    aiOpen.checked = true;
    aiOpenLab.append(aiOpen, h("span", null, t("i18ned.ki_nur_offen")));
    var aiBtn = btn("small primary", t("i18ned.ki_paket"), function () {
      download("/package/" + encodeURIComponent(lang) + (aiOpen.checked ? "?open=1" : ""), lang + "-translation.json", aiBtn);
    });
    aiRow.append(aiBtn, aiOpenLab);
    ai.append(aiText, aiRow);
    var im = h("div");
    im.append(h("h3", null, t("i18ned.importieren")));
    var imRow = h("div", "ie-inline");
    var file = h("input");
    file.type = "file";
    file.accept = ".json,application/json";
    file.setAttribute("aria-label", t("i18ned.datei"));
    var mode = h("select");
    mode.setAttribute("aria-label", t("i18ned.vorgehen"));
    [["merge", "i18ned.import_merge"], ["replace", "i18ned.import_replace"]].forEach(function (m) { var o = h("option", null, t(m[1])); o.value = m[0]; mode.append(o); });
    var imBtn = btn("small", t("i18ned.importieren"), importFile);
    imRow.append(file, mode, imBtn);
    im.append(imRow, h("small", "ie-hint", t("i18ned.import_hint")));
    io.append(ai, ex, im);
    ed.append(edHead, tools, filters, count, list, moreWrap, help, io);

    root.append(settings, langsCard, ed);

    // --- Dialoge ---
    var dlg = h("dialog", "ie-dialog");
    root.append(dlg);
    function modal(build) {
      dlg.replaceChildren(build());
      if (!dlg.open) dlg.showModal();
      var f = dlg.querySelector("input:not([disabled]),select,textarea");
      if (f) setTimeout(function () { f.focus(); }, 30);
    }
    function confirmBox(title, text, okLabel) {
      return new Promise(function (resolve) {
        var done = false;
        modal(function () {
          var f = h("form");
          f.append(h("h2", null, title), h("p", null, text));
          var act = h("div", "ie-actions");
          var ok = btn("danger", okLabel);
          ok.type = "submit";
          act.append(btn("", t("i18ned.abbrechen"), function () { dlg.close(); }), ok);
          f.append(act);
          f.addEventListener("submit", function (e) { e.preventDefault(); done = true; dlg.close(); resolve(true); });
          return f;
        });
        dlg.addEventListener("close", function () { if (!done) resolve(false); }, { once: true });
      });
    }

    function nameOf(code) {
      var l = info && info.languages.filter(function (x) { return x.code === code; })[0];
      return l ? l.name : code;
    }

    // --- Laden und Darstellen ---
    function load() {
      return host.request("GET", BASE).then(function (r) { info = r; renderLangs(); })
        .catch(function (err) { host.toast(err.message, true); });
    }

    function renderLangs() {
      defSel.replaceChildren.apply(defSel, info.languages.filter(function (l) { return !l.hidden || l.code === info.default; }).map(function (l) {
        var o = h("option", null, l.name + " (" + l.code + ")");
        o.value = l.code;
        o.selected = l.code === info.default;
        return o;
      }));
      autoCb.checked = info.auto;
      langList.replaceChildren.apply(langList, info.languages.map(function (l) {
        var li = h("li", "ie-lang" + (l.hidden ? " off" : ""));
        var code = h("span", "ie-code", l.code.slice(0, 5));
        var main = h("div", "ie-lang-main");
        var top = h("div", "ie-lang-top");
        top.append(h("strong", null, l.name));
        top.append(h("span", "ie-chip" + (l.official ? " ok" : ""), l.official ? t("i18ned.offiziell") : t("i18ned.eigene")));
        if (l.code === info.default) top.append(h("span", "ie-chip accent", t("i18ned.standard")));
        if (l.hidden) top.append(h("span", "ie-chip", t("i18ned.ausgeblendet")));
        if (l.outdated) top.append(h("span", "ie-chip warn", t("i18ned.n_veraltet", { n: l.outdated })));
        var meta = [t("i18ned.vollstaendig", { pct: pct.format(l.complete) }), t("i18ned.n_geaendert", { n: l.changed })];
        if (l.locale) meta.push(l.locale);
        if (l.dir === "rtl") meta.push(t("i18ned.rtl"));
        main.append(top, h("div", "ie-meta", meta.join(" · ")));
        var bar = h("div", "ie-lang-actions");
        bar.append(btn("small", t("i18ned.texte_bearbeiten"), function () { open(l.code); }));
        if (l.code !== info.default) {
          bar.append(btn("small", l.hidden ? t("i18ned.einblenden") : t("i18ned.ausblenden"), function () {
            var hidden = info.languages.filter(function (x) { return x.hidden && x.code !== l.code; }).map(function (x) { return x.code; });
            if (!l.hidden) hidden.push(l.code);
            saveSettings({ hidden: hidden });
          }));
        }
        if (!l.official) {
          bar.append(btn("small", t("i18ned.bearbeiten"), function () { openLangDialog(l); }));
          var del = btn("small danger-text", t("i18ned.loeschen"), function () {
            confirmBox(t("i18ned.loeschen_titel", { name: l.name }), t("i18ned.loeschen_text"), t("i18ned.loeschen")).then(function (ok) {
              if (!ok) return;
              host.request("DELETE", BASE + "/languages/" + encodeURIComponent(l.code)).then(function (r) {
                info = r;
                if (lang === l.code) close();
                renderLangs();
              }).catch(function (err) { host.toast(err.message, true); });
            });
          });
          del.disabled = l.code === info.default;
          bar.append(del);
        }
        li.append(code, main, bar);
        return li;
      }));
    }

    function saveSettings(change) {
      var body = {
        default: info.default, auto: info.auto,
        hidden: info.languages.filter(function (x) { return x.hidden; }).map(function (x) { return x.code; }),
      };
      Object.keys(change).forEach(function (k) { body[k] = change[k]; });
      return host.request("PUT", BASE + "/settings", body).then(function (r) {
        info = r;
        host.toast(t("i18ned.gespeichert"));
      }).catch(function (err) { host.toast(err.message, true); }).then(renderLangs);
    }
    defSel.addEventListener("change", function () { saveSettings({ default: defSel.value }); });
    autoCb.addEventListener("change", function () { saveSettings({ auto: autoCb.checked }); });

    // Sprache anlegen / Namen, Format und Schreibrichtung ändern
    function openLangDialog(l) {
      modal(function () {
        var f = h("form");
        f.append(h("h2", null, l ? t("i18ned.sprache_bearbeiten", { name: l.name }) : t("i18ned.hinzufuegen")));
        function field(label, input, hint) {
          var w = h("label", "ie-field");
          w.append(h("span", null, label), input);
          if (hint) w.append(h("small", null, hint));
          return w;
        }
        var code = h("input"); code.maxLength = 12; code.required = true; code.autocomplete = "off"; code.spellcheck = false;
        code.placeholder = t("i18ned.code_ph"); code.value = l ? l.code : ""; code.disabled = !!l;
        var name = h("input"); name.maxLength = 40; name.required = true; name.autocomplete = "off";
        name.placeholder = t("i18ned.name_ph"); name.value = l ? l.name : "";
        var loc = h("input"); loc.maxLength = 20; loc.autocomplete = "off"; loc.spellcheck = false; loc.placeholder = "fr-FR";
        loc.value = l ? l.locale || "" : "";
        var dir = h("select");
        [["", "i18ned.ltr"], ["rtl", "i18ned.rtl"]].forEach(function (d) { var o = h("option", null, t(d[1])); o.value = d[0]; dir.append(o); });
        dir.value = l && l.dir === "rtl" ? "rtl" : "";
        var row = h("div", "ie-grid2");
        row.append(field(t("i18ned.locale"), loc), field(t("i18ned.dir"), dir));
        f.append(field(t("i18ned.code"), code, t("i18ned.code_hint")), field(t("i18ned.name"), name, t("i18ned.name_hint")), row);
        if (!l) f.append(h("p", "ie-hint", t("i18ned.neu_hint")));
        var errP = h("p", "ie-error");
        errP.setAttribute("role", "alert");
        var act = h("div", "ie-actions");
        var ok = btn("primary", t("i18ned.speichern"));
        ok.type = "submit";
        act.append(btn("", t("i18ned.abbrechen"), function () { dlg.close(); }), ok);
        f.append(errP, act);
        f.addEventListener("submit", function (e) {
          e.preventDefault();
          ok.disabled = true;
          errP.textContent = "";
          var body = { name: name.value.trim(), locale: loc.value.trim(), dir: dir.value };
          var p;
          if (l) p = host.request("PATCH", BASE + "/languages/" + encodeURIComponent(l.code), body);
          else { body.code = code.value.trim(); p = host.request("POST", BASE + "/languages", body); }
          p.then(function (r) {
            info = r;
            dlg.close();
            renderLangs();
            if (!l) open(body.code);
          }).catch(function (err) { errP.textContent = err.message; }).then(function () { ok.disabled = false; });
        });
        return f;
      });
    }

    // --- Editor ---
    function open(code) {
      lang = code;
      edTitle.textContent = t("i18ned.texte_titel", { name: nameOf(code), code: code });
      ed.hidden = false;
      list.replaceChildren(h("li", "ie-empty", t("i18ned.laedt")));
      return host.request("GET", BASE + "/texts/" + encodeURIComponent(code)).then(function (r) {
        entries = r;
        render(true);
        ed.scrollIntoView({ behavior: "smooth", block: "start" });
      }).catch(function (err) { host.toast(err.message, true); });
    }
    function close() {
      lang = "";
      entries = [];
      ed.hidden = true;
    }

    function isMissing(e) { return !e.changed && !e.official; }
    function matches(e, q, a) {
      if (a && e.key.indexOf(a) !== 0) return false;
      if (filter === "missing" && !isMissing(e)) return false;
      if (filter === "changed" && !e.changed) return false;
      if (filter === "outdated" && !e.outdated) return false;
      if (!q) return true;
      return e.key.toLowerCase().indexOf(q) >= 0 || e.source.toLowerCase().indexOf(q) >= 0 ||
        (e.official || "").toLowerCase().indexOf(q) >= 0 || (e.text || "").toLowerCase().indexOf(q) >= 0;
    }

    function render(reset) {
      if (reset) {
        var q = search.value.trim().toLowerCase();
        listCache = entries.filter(function (e) { return matches(e, q, area.value); });
        shown = Math.min(PAGE, listCache.length);
        list.replaceChildren.apply(list, listCache.slice(0, shown).map(row));
      } else {
        var next = Math.min(shown + PAGE, listCache.length);
        list.append.apply(list, listCache.slice(shown, next).map(row));
        shown = next;
      }
      if (!listCache.length) list.replaceChildren(h("li", "ie-empty", t("i18ned.keine_treffer")));
      updateCounts();
      more.hidden = shown >= listCache.length;
      more.textContent = t("i18ned.mehr", { n: Math.min(PAGE, listCache.length - shown) });
    }
    function updateCounts() {
      var c = { missing: 0, changed: 0, outdated: 0 };
      entries.forEach(function (e) {
        if (isMissing(e)) c.missing++;
        if (e.changed) c.changed++;
        if (e.outdated) c.outdated++;
      });
      filters.querySelectorAll("button").forEach(function (b) {
        var f = FILTERS.filter(function (x) { return x[0] === b.dataset.f; })[0];
        var n = c[b.dataset.f];
        b.textContent = t(f[1]) + (n ? " (" + num.format(n) + ")" : "");
      });
      count.textContent = t("i18ned.treffer", { n: listCache.length, total: entries.length });
    }

    var timer = 0;
    search.addEventListener("input", function () { clearTimeout(timer); timer = setTimeout(function () { render(true); }, 150); });
    area.addEventListener("change", function () { render(true); });

    // Beispielwerte für die Vorschau: Zahlen für Mehrzahl-Platzhalter, sonst ‹name›.
    function preview(key, text, vars) {
      if (!vars.length || !text) return "";
      if (key.indexOf("srv.") === 0) return text.replace(/\{(\d+)\}/g, function (_, i) { return "‹" + i + "›"; });
      var plural = {};
      text.replace(/\{\s*([\w.]+)\s*,\s*plural/g, function (_, n) { plural[n] = true; return _; });
      function sample(n) {
        var v = {};
        vars.forEach(function (x) { v[x] = plural[x] ? n : "‹" + x + "›"; });
        return v;
      }
      if (!Object.keys(plural).length) return GoChatI18n.format(text, sample(1));
      var out = [];
      [0, 1, 5].forEach(function (n) { var s = GoChatI18n.format(text, sample(n)); if (out.indexOf(s) < 0) out.push(s); });
      return out.join("  ·  ");
    }

    function row(e) {
      var li = h("li", "ie-row");
      var head = h("div", "ie-key");
      head.append(h("code", null, e.key));
      var state = h("span");
      function setState() {
        state.replaceChildren();
        if (e.outdated) state.append(h("span", "ie-chip warn", t("i18ned.veraltet")));
        else if (e.changed) state.append(h("span", "ie-chip accent", t("i18ned.geaendert")));
        else if (isMissing(e)) state.append(h("span", "ie-chip danger", t("i18ned.fehlt")));
      }
      setState();
      head.append(state);
      li.append(head);

      var ref = h("dl", "ie-ref");
      function addRef(label, text, cls) { ref.append(h("dt", null, label), h("dd", cls || null, text)); }
      if (lang !== info.source) addRef(nameOf(info.source), e.source);
      if (e.official && (lang === info.source ? e.changed : true)) addRef(t("i18ned.original"), e.official);
      if (e.outdated) addRef(t("i18ned.original_vorher"), e.base || "–", "old");
      if (ref.children.length) li.append(ref);

      var ta = h("textarea", "ie-text");
      var shownText = e.text || e.official || e.source;
      ta.rows = Math.min(8, Math.max(1, Math.ceil(shownText.length / 70), (shownText.match(/\n/g) || []).length + 1));
      ta.placeholder = e.source;
      ta.lang = lang;
      ta.setAttribute("aria-label", e.key);
      li.append(ta);

      var foot = h("div", "ie-foot");
      var chips = h("div", "ie-vars");
      if (e.vars.length) {
        chips.append(h("span", "ie-vars-label", t("i18ned.platzhalter")));
        e.vars.forEach(function (v) {
          var b = btn("ie-var", "{" + v + "}", function () {
            var s = ta.selectionStart == null ? ta.value.length : ta.selectionStart;
            var en = ta.selectionEnd == null ? s : ta.selectionEnd;
            ta.setRangeText("{" + v + "}", s, en, "end");
            ta.focus();
            ta.dispatchEvent(new Event("input"));
          });
          b.className = "ie-var";
          b.title = t("i18ned.einfuegen");
          chips.append(b);
        });
      }
      var act = h("div", "ie-row-actions");
      var keep = btn("small", t("i18ned.pruefen_ok"), function () { put({ confirm: true }).then(function () { done("confirm"); }).catch(fail); });
      keep.title = t("i18ned.pruefen_ok_hint");
      var reset = btn("small", t("i18ned.zuruecksetzen"), function () {
        put({ text: "" }).then(function () { done("reset"); msg.className = "ie-msg ok"; msg.textContent = t("i18ned.zurueckgesetzt"); }).catch(fail);
      });
      var save = btn("small primary", t("i18ned.speichern"), doSave);
      act.append(keep, reset, save);
      foot.append(chips, act);
      var pv = h("div", "ie-preview");
      var msg = h("div", "ie-msg");
      msg.setAttribute("role", "status");
      li.append(foot, pv, msg);

      function current() { return e.changed ? e.text : e.official; }
      function sync() {
        var dirty = ta.value !== current();
        save.disabled = !dirty || (!ta.value && !e.changed);
        reset.hidden = !e.changed;
        keep.hidden = !e.outdated || dirty;
        var p = preview(e.key, ta.value, e.vars);
        pv.textContent = p ? t("i18ned.vorschau", { text: p }) : "";
        li.classList.toggle("dirty", dirty);
      }
      ta.value = current() || "";
      sync();
      ta.addEventListener("input", function () { msg.textContent = ""; msg.className = "ie-msg"; sync(); });
      ta.addEventListener("keydown", function (ev) {
        if (ev.key === "Enter" && (ev.ctrlKey || ev.metaKey)) { ev.preventDefault(); doSave(); }
      });

      function put(body) {
        body.key = e.key;
        return host.request("PUT", BASE + "/texts/" + encodeURIComponent(lang), body).then(function (r) { return (r && r.warnings) || []; });
      }
      function fail(err) { msg.className = "ie-msg error"; msg.textContent = err.message; sync(); }
      // Zustand lokal nachführen (wie der Server: gleich dem Original = keine Änderung)
      function done(kind, text) {
        if (kind === "set" && text && text !== e.official) { e.text = text; e.changed = true; e.outdated = false; e.base = e.official; }
        else if (kind === "confirm") { e.outdated = false; e.base = e.official; }
        else { e.text = undefined; e.changed = false; e.outdated = false; e.base = undefined; }
        setState();
        ta.value = current() || "";
        sync();
        updateCounts();
        load();
      }
      function doSave() {
        if (save.disabled) return;
        save.disabled = true;
        var text = ta.value;
        put({ text: text }).then(function (warns) {
          done("set", text);
          msg.className = "ie-msg " + (warns.length ? "warn" : "ok");
          msg.textContent = warns.length ? warns.join(" · ") : t("i18ned.gespeichert");
        }).catch(fail);
      }
      return li;
    }

    function download(path, name, b) {
      b.disabled = true;
      host.raw("GET", BASE + path).then(function (res) {
        if (!res.ok) throw new Error(t("i18ned.download_fehlgeschlagen"));
        return res.blob();
      }).then(function (blob) {
        var a = document.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = name;
        document.body.append(a); a.click(); a.remove();
        setTimeout(function () { URL.revokeObjectURL(a.href); }, 2000);
      }).catch(function (err) { host.toast(err.message, true); }).then(function () { b.disabled = false; });
    }

    function importFile() {
      var f = file.files[0];
      if (!f) { host.toast(t("i18ned.datei_waehlen"), true); return; }
      var replace = mode.value === "replace";
      var ask = replace ? confirmBox(t("i18ned.ersetzen_titel"), t("i18ned.ersetzen_text", { name: nameOf(lang) }), t("i18ned.ersetzen_ok")) : Promise.resolve(true);
      ask.then(function (ok) {
        if (!ok) return;
        imBtn.disabled = true;
        return f.text().then(function (body) {
          return host.raw("POST", BASE + "/file/" + encodeURIComponent(lang) + (replace ? "?replace=1" : ""), body, "application/json");
        }).then(function (res) {
          return res.json().catch(function () { return {}; }).then(function (r) {
            if (!res.ok) throw new Error(r.error || t("i18ned.fehler"));
            var text = t("i18ned.import_ergebnis", { applied: r.applied, unchanged: r.unchanged, unknown: r.unknown });
            if (r.errors && r.errors.length) text += " " + t("i18ned.import_fehler", { n: r.errors.length, list: r.errors.slice(0, 5).join("; ") });
            host.toast(text, !!(r.errors && r.errors.length));
            file.value = "";
            load();
            return open(lang);
          });
        }).catch(function (err) { host.toast(err.message, true); }).then(function () { imBtn.disabled = false; });
      });
    }

    return { load: load, open: open, close: close };
  }

  window.GoChatI18nEditor = { mount: mount };
})();
