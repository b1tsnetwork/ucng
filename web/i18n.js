// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
/*!
 * UCNG – Übersetzungen im Browser
 *
 * Der Server liefert vorab window.I18N = {lang, locale, dir, dict, langs} (siehe /i18n/boot.js).
 *   _t("app.send")                       → Text
 *   _t("app.unread", {n: 3})             → Text mit Werten ({n}, Mehrzahl über {n, plural, one {…} other {…}})
 *   _tH("admin.intro", {...})            → DocumentFragment mit erlaubtem Markup (b, strong, em, i, code, br, small, kbd, a)
 *   GoChatI18n.apply(root)               → data-i18n, data-i18n-html, data-i18n-placeholder|title|aria-label|alt
 *   GoChatI18n.setLang("en")             → Sprache wechseln (Cookie) und neu laden
 *   <label data-i18n-langs hidden>       → bekommt eine Sprachauswahl, sobald mehr als eine Sprache angeboten wird
 *     (data-i18n-langs-id="x" setzt die id der Auswahl; [data-i18n-langs-label] wird dann ebenfalls sichtbar)
 * Dieselbe Datei nutzt der Beacon (cmd/gochat-beacon/web/i18n.js – bitte gleich halten).
 */
(function () {
  "use strict";

  var cfg = window.I18N || { lang: "de", locale: "de-DE", dir: "ltr", dict: {}, langs: [] };
  var dict = cfg.dict || {};
  var locale = cfg.locale || cfg.lang || "de";
  var plural;
  try { plural = new Intl.PluralRules(locale); } catch (e) { plural = { select: function (n) { return n === 1 ? "one" : "other"; } }; }
  var numFmt;
  try { numFmt = new Intl.NumberFormat(locale); } catch (e) { numFmt = { format: String }; }

  function matchBrace(s, i) {
    var depth = 0;
    for (var j = i; j < s.length; j++) {
      if (s[j] === "{") depth++;
      else if (s[j] === "}" && --depth === 0) return j;
    }
    return -1;
  }

  function parseCases(body) {
    var out = {}, s = body.trim();
    while (s.length) {
      var i = s.indexOf("{");
      if (i < 0) break;
      var sel = s.slice(0, i).trim(), end = matchBrace(s, i);
      if (end < 0) break;
      out[sel] = s.slice(i + 1, end);
      s = s.slice(end + 1).trim();
    }
    return out;
  }

  function show(v) {
    if (typeof v === "number") return numFmt.format(v);
    return v == null ? "" : String(v);
  }

  // Werte einsetzen. esc: Funktion für eingesetzte Werte (HTML-Modus).
  function format(s, vars, esc, hash) {
    var out = "";
    for (var i = 0; i < s.length; i++) {
      var ch = s[i];
      if (ch === "{" && s[i + 1] === "{") { out += "{"; i++; continue; }
      if (ch === "}" && s[i + 1] === "}") { out += "}"; i++; continue; }
      if (ch === "#" && hash != null) { out += esc(hash); continue; }
      if (ch !== "{") { out += ch; continue; }
      var end = matchBrace(s, i);
      if (end < 0) { out += s.slice(i); break; }
      var arg = s.slice(i + 1, end), comma = arg.indexOf(",");
      var name = (comma < 0 ? arg : arg.slice(0, comma)).trim();
      var has = vars && Object.prototype.hasOwnProperty.call(vars, name);
      if (comma < 0) {
        out += has ? esc(show(vars[name])) : "{" + name + "}";
      } else {
        var rest = arg.slice(comma + 1), c2 = rest.indexOf(",");
        var kind = (c2 < 0 ? rest : rest.slice(0, c2)).trim();
        var n = Number(has ? vars[name] : 0);
        if (kind === "plural" && c2 >= 0) {
          var cases = parseCases(rest.slice(c2 + 1));
          var choice = cases["=" + n];
          if (choice == null) choice = cases[plural.select(n)];
          if (choice == null) choice = cases.other || "";
          out += format(choice, vars, esc, show(n));
        } else {
          out += has ? esc(show(vars[name])) : "";
        }
      }
      i = end;
    }
    return out;
  }

  function same(x) { return x; }
  function escHTML(x) {
    return String(x).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  function text(key) {
    var s = dict[key];
    if (s == null) {
      if (window.console && cfg.debug) console.warn("i18n: fehlender Text", key);
      return key;
    }
    return s;
  }

  function t(key, vars) { return format(text(key), vars, same); }

  // Erlaubtes Markup in Übersetzungen (ohne Attribute, außer href bei <a>).
  var ALLOWED = { B: 1, STRONG: 1, EM: 1, I: 1, CODE: 1, BR: 1, SMALL: 1, KBD: 1, A: 1, SPAN: 1 };
  function sanitize(node) {
    var kids = Array.prototype.slice.call(node.childNodes);
    for (var i = 0; i < kids.length; i++) {
      var k = kids[i];
      if (k.nodeType === 3) continue;
      if (k.nodeType !== 1 || !ALLOWED[k.tagName]) {
        node.replaceChild(document.createTextNode(k.textContent || ""), k);
        continue;
      }
      var href = k.tagName === "A" ? k.getAttribute("href") : null;
      while (k.attributes.length) k.removeAttribute(k.attributes[0].name);
      if (href && /^(https?:\/\/|\/|#|mailto:)/i.test(href)) {
        k.setAttribute("href", href);
        if (/^https?:/i.test(href)) { k.setAttribute("target", "_blank"); k.setAttribute("rel", "noopener noreferrer"); }
      }
      sanitize(k);
    }
  }

  function tH(key, vars) {
    var tpl = document.createElement("template");
    tpl.innerHTML = format(text(key), vars, escHTML);
    sanitize(tpl.content);
    return tpl.content;
  }

  var ATTRS = ["placeholder", "title", "aria-label", "alt"];
  function apply(root) {
    root = root || document;
    var els = root.querySelectorAll("[data-i18n],[data-i18n-html],[data-i18n-placeholder],[data-i18n-title],[data-i18n-aria-label],[data-i18n-alt]");
    for (var i = 0; i < els.length; i++) {
      var e = els[i], k = e.getAttribute("data-i18n");
      if (k) e.textContent = t(k);
      k = e.getAttribute("data-i18n-html");
      if (k) e.replaceChildren(tH(k));
      for (var a = 0; a < ATTRS.length; a++) {
        k = e.getAttribute("data-i18n-" + ATTRS[a]);
        if (k) e.setAttribute(ATTRS[a], t(k));
      }
    }
  }

  function setCookie(code) {
    var secure = location.protocol === "https:" ? "; Secure" : "";
    document.cookie = "gochat_lang=" + encodeURIComponent(code) + "; Path=/; Max-Age=31536000; SameSite=Lax" + secure;
  }

  function setLang(code) {
    setCookie(code);
    try { localStorage.setItem("gochat:lang", code); } catch (e) { /* egal */ }
    location.reload();
  }

  // Auswahlliste für die Sprache.
  function select(opts) {
    opts = opts || {};
    var s = document.createElement("select");
    s.className = opts.className || "lang-select";
    s.setAttribute("aria-label", t("common.language"));
    (cfg.langs || []).forEach(function (l) {
      var o = document.createElement("option");
      o.value = l.code; o.textContent = l.name;
      if (l.code === cfg.lang) o.selected = true;
      s.appendChild(o);
    });
    s.addEventListener("change", function () { setLang(s.value); });
    return s;
  }

  // Sprachauswahl in alle Plätze mit data-i18n-langs setzen (nur wenn es etwas zu wählen gibt).
  function mount(root) {
    root = root || document;
    if ((cfg.langs || []).length < 2) return;
    var slots = root.querySelectorAll("[data-i18n-langs]");
    for (var i = 0; i < slots.length; i++) {
      var e = slots[i];
      if (e.querySelector("select")) continue;
      var s = select({ className: e.getAttribute("data-i18n-langs") || "lang-select" });
      var id = e.getAttribute("data-i18n-langs-id");
      if (id) s.id = id;
      e.appendChild(s);
      e.hidden = false;
    }
    var labels = root.querySelectorAll("[data-i18n-langs-label]");
    for (var j = 0; j < labels.length; j++) labels[j].hidden = false;
  }

  document.documentElement.lang = cfg.lang || "de";
  if (cfg.dir === "rtl") document.documentElement.dir = "rtl";

  window._t = t;
  window._tH = tH;
  window.GoChatI18n = {
    t: t, tH: tH, format: function (s, vars) { return format(s, vars, same); }, apply: apply,
    setLang: setLang, select: select, mount: mount, lang: cfg.lang, locale: locale, langs: cfg.langs || [], has: function (k) { return dict[k] != null; },
  };
  // Die Skripte stehen am Ende von <body>: Das HTML davor ist schon da. Sofort übersetzen,
  // bevor die Anwendung Texte dynamisch setzt.
  apply(document);
  mount(document);
})();
