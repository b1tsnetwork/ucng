// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
/*!
 * UCNG Embed-Widget
 *
 * Einbinden:
 *   <script src="https://DEIN-SERVER/embed/chat.js" data-room="lobby" defer></script>
 *
 * Optionen (data-Attribute oder UCNG.init({...}); GoChat.init ist der frühere Name und geht weiter):
 *   data-room      Raumname (Standard: lobby)
 *   data-title     Überschrift im Widget
 *   data-color     Akzentfarbe, z. B. "#e4572e"
 *   data-position  "right" (Standard) oder "left"
 *   data-theme     "light" (Standard), "dark" oder "auto" (folgt dem System)
 *   data-target    CSS-Selektor: Chat inline in dieses Element rendern statt schwebend
 *   data-name      Namen vorschlagen; ist er frei, wird er automatisch reserviert
 *   data-open      "true" = schwebendes Fenster direkt öffnen
 *   data-css       "false" = chat.css nicht automatisch laden
 *   data-server    Server-URL, falls abweichend von der Script-Quelle
 *   data-manual    "true" = nicht automatisch starten, nur UCNG.init() bereitstellen
 *   data-widget    ID eines im Admin-Bereich gespeicherten Widgets ("w_…"): alle Einstellungen
 *                  kommen dann vom Server; weitere data-Attribute überschreiben sie
 *   data-greeting  Begrüßung als erste Sprechblase (und Hinweis am Button)
 *   data-hours     "true" = Öffnungszeiten anzeigen (Einstellung im Admin-Bereich)
 *   data-lang      Sprache des Widgets, z. B. "en" (Standard: Sprache des Browsers bzw. der Instanz)
 */
(function () {
  "use strict";

  var script = document.currentScript;
  var defaultServer = script && script.src ? script.src.replace(/\/embed\/chat\.js(\?.*)?$/, "") : "";

  function storeGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
  function storeSet(k, v) { try { localStorage.setItem(k, v); } catch (e) { /* egal */ } }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function renderText(target, text) {
    var re = /https?:\/\/[^\s<>"']+/g, last = 0, m;
    while ((m = re.exec(text))) {
      target.appendChild(document.createTextNode(text.slice(last, m.index)));
      var a = el("a", null, m[0]);
      a.href = m[0]; a.target = "_blank"; a.rel = "noopener noreferrer nofollow";
      target.appendChild(a);
      last = m.index + m[0].length;
    }
    target.appendChild(document.createTextNode(text.slice(last)));
  }

  // Schrift auf der Akzentfarbe: Weiß, wenn gut lesbar (WCAG AA), sonst Fast-Schwarz.
  function readableOn(hex) {
    var n = parseInt(hex.slice(1), 16);
    var ch = function (v) { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    var L = 0.2126 * ch(n >> 16) + 0.7152 * ch((n >> 8) & 255) + 0.0722 * ch(n & 255);
    var dark = 0.0066; // Luminanz von #111321
    var cw = 1.05 / (L + 0.05), cd = (L + 0.05) / (dark + 0.05);
    return cw >= 4.5 || cw >= cd ? "#ffffff" : "#111321"; // Weiß, wenn gut lesbar
  }

  // Formatierung (fett, kursiv, Code, Links, @Erwähnungen) liegt in format.js – einmal nachladen.
  var formatWaiters = null;
  // --- Übersetzungen ---
  // Deutsche Texte als Rückfall (werden von go test mit web/i18n/de.json abgeglichen);
  // vor dem Start lädt das Widget die Texte der gewählten Sprache vom Server.
  /*I18N-DE-START*/
  var I18N_DE = {
    "common.language": "Sprache",
    "widget.abstimmen_fehlgeschlagen": "Abstimmen fehlgeschlagen",
    "widget.aeltere_nachrichten_laden": "Ältere Nachrichten laden",
    "widget.anmelden": "Anmelden",
    "widget.anmelden_geht_nur_auf_seiten_mit_https": "Anmelden geht nur auf Seiten mit https://.",
    "widget.anmeldung_fehlgeschlagen": "Anmeldung fehlgeschlagen",
    "widget.ausblenden": "Ausblenden",
    "widget.bild": "Bild",
    "widget.bitte_versuche_es_spaeter_noch_einmal": "Bitte versuche es später noch einmal.",
    "widget.bitte_waehle_erneut_einen_namen": "Bitte wähle erneut einen Namen.",
    "widget.bot": "Bot",
    "widget.chat": "Chat",
    "widget.chat_schliessen": "Chat schließen",
    "widget.code_aus_der_authenticator_app": "Code aus der Authenticator-App",
    "widget.das_ist_kein_gueltiger_profil_code": "Das ist kein gültiger Profil-Code.",
    "widget.dein_konto_ist_mit_zwei_faktor_anmeldung": "Dein Konto ist mit Zwei-Faktor-Anmeldung geschützt.",
    "widget.dein_name": "Dein Name",
    "widget.deine_nachricht_wurde_automatisch": "Deine Nachricht wurde automatisch zurückgehalten und wird geprüft.",
    "widget.den_code_findest_du_im_chat_unter_profil": "Den Code findest du im Chat unter „Profil“ → „Code kopieren“.",
    "widget.der_chat_ist_gerade_nicht_verfuegbar": "Der Chat ist gerade nicht verfügbar",
    "widget.der_code_konnte_nicht_geprueft_werden": "Der Code konnte nicht geprüft werden.",
    "widget.dieser_chat_ist_nur_fuer_mitglieder": "Dieser Chat ist nur für Mitglieder. Melde dich mit deinem Konto an.",
    "widget.dieser_code_gehoert_zu_einem_anderen": "Dieser Code gehört zu einem anderen Chat-Server.",
    "widget.dieses_profil_gilt_nicht_mehr": "Dieses Profil gilt nicht mehr.",
    "widget.doch_neuen_namen_waehlen": "Doch neuen Namen wählen",
    "widget.gerade_nicht_besetzt": "Gerade nicht besetzt",
    "widget.hier_schreiben_nur_bestimmte_rollen": "Hier schreiben nur bestimmte Rollen",
    "widget.hinweis_schliessen": "Hinweis schließen",
    "widget.hours_next": "wieder ab {next} Uhr",
    "widget.hours_today": "heute {hours} Uhr",
    "widget.jetzt_erreichbar": "Jetzt erreichbar",
    "widget.konto_anlegen": "Konto anlegen",
    "widget.lade": "Lade …",
    "widget.load_failed": "[UCNG] Widget {widget} konnte nicht geladen werden – gelöscht oder auf dieser Domain nicht erlaubt.",
    "widget.nachricht": "Nachricht",
    "widget.nachricht_nicht_mehr_vorhanden": "Nachricht nicht mehr vorhanden",
    "widget.nachricht_schreiben": "Nachricht schreiben",
    "widget.nachricht_wurde_automatisch_ausgeblendet": "Nachricht wurde automatisch ausgeblendet",
    "widget.nachricht_wurde_geloescht": "Nachricht wurde gelöscht",
    "widget.nachricht_wurde_von_einem_moderator": "Nachricht wurde von einem Moderator gelöscht",
    "widget.name": "Name",
    "widget.name_konnte_nicht_gesetzt_werden": "Name konnte nicht gesetzt werden",
    "widget.nicht_verfuegbar": "Nicht verfügbar",
    "widget.noch_ist_es_ruhig_hier": "Noch ist es ruhig hier",
    "widget.open_title": "{title} öffnen",
    "widget.passwort": "Passwort",
    "widget.profil_code": "Profil-Code",
    "widget.profil_code_einfuegen": "Profil-Code einfügen",
    "widget.profil_code_gochat1": "Profil-Code (UCNG1:…)",
    "widget.schreib_die_erste_nachricht": "Schreib die erste Nachricht.",
    "widget.senden": "Senden",
    "widget.senden_fehlgeschlagen": "Senden fehlgeschlagen",
    "widget.server_nicht_erreichbar": "Server nicht erreichbar",
    "widget.team": "Team",
    "widget.thread_replies": "{n, plural, one {# Antwort} other {# Antworten}} im Chat ansehen",
    "widget.time_only_you": "{time} · nur für dich",
    "widget.uebernehmen": "Übernehmen",
    "widget.umfrage_beendet": "Umfrage beendet",
    "widget.verbinde": "Verbinde …",
    "widget.verbindung_wird_wiederhergestellt": "Verbindung wird wiederhergestellt …",
    "widget.waehle_einen_namen_um_mitzuschreiben_er": "Wähle einen Namen, um mitzuschreiben. Er bleibt für dich reserviert.",
    "widget.weiter": "Weiter",
    "widget.zum_chat": "Zum Chat",
    "widget.zum_mitschreiben_melde_dich_mit_deinem": "Zum Mitschreiben melde dich mit deinem Konto an."
  };
  /*I18N-DE-END*/
  var i18nDict = I18N_DE, i18nLang = "de", i18nLocale = "de-DE", i18nWaiters = null, i18nLoaded = "";
  var i18nPlural = function (n) { return n === 1 ? "one" : "other"; };
  function i18nSetLocale(loc) {
    try { var pr = new Intl.PluralRules(loc); i18nPlural = function (n) { return pr.select(n); }; } catch (e) { /* einfache Regel */ }
  }
  function i18nBrace(s, i) {
    for (var d = 0, j = i; j < s.length; j++) {
      if (s[j] === "{") d++;
      else if (s[j] === "}" && --d === 0) return j;
    }
    return -1;
  }
  function i18nFormat(s, vars, hash) {
    var out = "";
    for (var i = 0; i < s.length; i++) {
      var ch = s[i];
      if (ch === "{" && s[i + 1] === "{") { out += "{"; i++; continue; }
      if (ch === "}" && s[i + 1] === "}") { out += "}"; i++; continue; }
      if (ch === "#" && hash != null) { out += hash; continue; }
      if (ch !== "{") { out += ch; continue; }
      var end = i18nBrace(s, i);
      if (end < 0) { out += s.slice(i); break; }
      var arg = s.slice(i + 1, end), c = arg.indexOf(",");
      var nm = (c < 0 ? arg : arg.slice(0, c)).trim();
      var has = vars && Object.prototype.hasOwnProperty.call(vars, nm);
      if (c < 0) out += has ? String(vars[nm] == null ? "" : vars[nm]) : "{" + nm + "}";
      else {
        var rest = arg.slice(c + 1), c2 = rest.indexOf(","), n = Number(has ? vars[nm] : 0);
        var cases = {}, body = c2 < 0 ? "" : rest.slice(c2 + 1).trim();
        while (body.length) {
          var k = body.indexOf("{"); if (k < 0) break;
          var e2 = i18nBrace(body, k); if (e2 < 0) break;
          cases[body.slice(0, k).trim()] = body.slice(k + 1, e2);
          body = body.slice(e2 + 1).trim();
        }
        var choice = cases["=" + n]; if (choice == null) choice = cases[i18nPlural(n)]; if (choice == null) choice = cases.other || "";
        out += i18nFormat(choice, vars, String(n));
      }
      i = end;
    }
    return out;
  }
  function _t(key, vars) {
    var s = i18nDict[key]; if (s == null) s = I18N_DE[key]; if (s == null) return key;
    return i18nFormat(s, vars);
  }
  // lädt die Texte (lang leer = Sprache des Browsers bzw. Standard der Instanz)
  function ensureI18n(server, lang, cb) {
    var want = lang || "";
    if (i18nLoaded && (!want || want === i18nLang)) { cb(); return; }
    if (i18nWaiters) { i18nWaiters.push(cb); return; }
    i18nWaiters = [cb];
    var done = function () { var w = i18nWaiters; i18nWaiters = null; w.forEach(function (f) { f(); }); };
    fetch(server + "/api/i18n/dict?scope=widget" + (want ? "&lang=" + encodeURIComponent(want) : ""))
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d && d.dict) { i18nDict = d.dict; i18nLang = d.lang; i18nLocale = d.locale || d.lang; i18nSetLocale(i18nLocale); i18nLoaded = d.lang; }
        done();
      }, done);
  }

  // fetch mit Sprache für Fehlermeldungen des Servers (nur bei Änderungen – GETs bleiben ohne Preflight)
  function gfetch(url, o) {
    o = o || {};
    if (o.method && o.method !== "GET" && i18nLoaded) {
      var h = {};
      for (var k in (o.headers || {})) h[k] = o.headers[k];
      h["X-Lang"] = i18nLang;
      o.headers = h;
    }
    return fetch(url, o);
  }

  function ensureFormat(server, cb) {
    if (window.GoChatFormat) { cb(); return; }
    if (formatWaiters) { formatWaiters.push(cb); return; }
    formatWaiters = [cb];
    var s = document.createElement("script");
    s.src = server + "/embed/format.js";
    s.async = true;
    s.onload = s.onerror = function () { var w = formatWaiters; formatWaiters = []; w.forEach(function (f) { f(); }); };
    document.head.appendChild(s);
  }

  function loadCSS(server) {
    if (document.getElementById("gochat-css")) return;
    var link = document.createElement("link");
    link.id = "gochat-css";
    link.rel = "stylesheet";
    link.href = server + "/embed/chat.css";
    document.head.appendChild(link);
  }

  // Statische Icons (keine Benutzerdaten)
  var ICON_CHAT = '<svg class="gochat-icon-chat" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M5.5 4h13A2.5 2.5 0 0 1 21 6.5v8a2.5 2.5 0 0 1-2.5 2.5H11l-4.6 3.6c-.7.5-1.6 0-1.6-.8V17A2.5 2.5 0 0 1 3 14.5v-8A2.5 2.5 0 0 1 5.5 4z"/></svg>';
  var ICON_CLOSE = '<svg class="gochat-icon-close" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6 6 18" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"/></svg>';
  var ICON_MARK = '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M5.5 4h13A2.5 2.5 0 0 1 21 6.5v8a2.5 2.5 0 0 1-2.5 2.5H11l-4.6 3.6c-.7.5-1.6 0-1.6-.8V17A2.5 2.5 0 0 1 3 14.5v-8A2.5 2.5 0 0 1 5.5 4z"/></svg>';
  var ICON_X = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M5 5l10 10M15 5 5 15" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>';
  var ICON_SEND = '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M10 16V4M5 9l5-5 5 5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';

  function init(opts) {
    opts = opts || {};
    var server = (opts.server || defaultServer || "").replace(/\/$/, "");
    var room = String(opts.room || "lobby").trim().toLowerCase();
    var title = opts.title || _t("widget.chat");
    var target = opts.target ? (typeof opts.target === "string" ? document.querySelector(opts.target) : opts.target) : null;
    var tokenKey = "gochat:token";
    var token = storeGet(tokenKey) || "";
    var name = "", myId = "", viewer = "";
    var greeting = String(opts.greeting || "").trim();
    var emotes = null; // Map Name → {url, size}
    var showHours = opts.hours === true || opts.hours === "true";

    if (!/^[\p{Ll}\p{Lo}\p{N}_-]{1,32}$/u.test(room)) {
      console.error("[UCNG] Ungültiger Kanalname:", room);
      return null;
    }
    if (opts.css !== false && opts.css !== "false") loadCSS(server);

    // --- DOM aufbauen ---
    var root = el("div", "gochat-root " + (target ? "gochat-inline" : "gochat-floating"));
    if (opts.position === "left") root.className += " gochat-left";
    if (opts.theme === "dark") root.className += " gochat-dark";
    else if (opts.theme === "auto") root.className += " gochat-auto";
    // Akzentfarbe: data-color der Webseite, sonst die der Anwendung (aus den Einstellungen).
    function applyAccent(c) {
      if (!/^#[0-9a-fA-F]{6}$/.test(c || "")) return;
      root.style.setProperty("--gochat-accent", c);
      root.style.setProperty("--gochat-on-accent", readableOn(c));
    }
    if (opts.color) applyAccent(opts.color);
    // Modus „hosted“: Ohne Gastnamen braucht das Widget eine Anmeldung mit Konto.
    var acct = null, loginStep = 1, loginAuth = "", loginName = "";
    gfetch(server + "/api/settings").then(function (r) { return r.json(); })
      .then(function (st) {
        if (!opts.color) applyAccent(st.accent);
        if (st.appMode === "hosted" && st.hosted && st.hosted.guests !== "write") { acct = st.hosted; updateNameUI(); }
      }, function () { /* Standardfarbe behalten */ });

    var panel = el("div", "gochat-panel");
    panel.setAttribute("role", "dialog");
    panel.setAttribute("aria-label", title);

    var header = el("div", "gochat-header");
    var mark = el("div", "gochat-mark");
    mark.innerHTML = ICON_MARK;
    var heading = el("div", "gochat-heading");
    var titleEl = el("div", "gochat-title", title);
    var status = el("div", "gochat-status", _t("widget.verbinde"));
    heading.appendChild(titleEl);
    heading.appendChild(status);
    var closeBtn = el("button", "gochat-close");
    closeBtn.type = "button";
    closeBtn.setAttribute("aria-label", _t("widget.chat_schliessen"));
    closeBtn.innerHTML = ICON_X;
    header.appendChild(mark);
    header.appendChild(heading);
    header.appendChild(closeBtn);

    var hoursBar = el("div", "gochat-hours");
    hoursBar.style.display = "none";
    var list = el("div", "gochat-messages");
    list.setAttribute("aria-live", "polite");
    var errorBox = el("div", "gochat-error");
    errorBox.setAttribute("role", "alert");
    var nameHint = el("div", "gochat-name-hint");
    var hintText = el("span", null, _t("widget.waehle_einen_namen_um_mitzuschreiben_er"));
    var codeToggle = el("button", "gochat-link", _t("widget.profil_code_einfuegen"));
    codeToggle.type = "button";
    nameHint.appendChild(hintText);
    nameHint.appendChild(document.createTextNode(" "));
    nameHint.appendChild(codeToggle);
    var codeMode = false; // true: Eingabefeld nimmt einen Profil-Code statt eines Namens

    // Bewusst kein <form>: Viele Webseiten (CMS, Shop-Systeme, Turbo usw.) fangen
    // Formular-Absendungen seitenweit ab – dann käme der Klick nie beim Widget an.
    var form = el("div", "gochat-form");
    var nameInput = el("input", "gochat-input");
    nameInput.placeholder = _t("widget.dein_name");
    nameInput.setAttribute("aria-label", _t("widget.dein_name"));
    nameInput.autocomplete = "nickname";
    nameInput.maxLength = 32;
    var pwInput = el("input", "gochat-input");
    pwInput.type = "password";
    pwInput.placeholder = _t("widget.passwort");
    pwInput.setAttribute("aria-label", _t("widget.passwort"));
    pwInput.autocomplete = "current-password";
    pwInput.style.display = "none";
    var textInput = el("textarea", "gochat-input");
    textInput.placeholder = _t("widget.nachricht_schreiben");
    textInput.setAttribute("aria-label", _t("widget.nachricht"));
    textInput.maxLength = 2000;
    textInput.rows = 1;
    var sendBtn = el("button", "gochat-send");
    sendBtn.type = "button";
    form.appendChild(nameInput);
    form.appendChild(pwInput);
    form.appendChild(textInput);
    form.appendChild(sendBtn);

    panel.appendChild(header);
    panel.appendChild(hoursBar);
    panel.appendChild(list);
    panel.appendChild(errorBox);
    panel.appendChild(nameHint);
    panel.appendChild(form);

    var launcher = el("button", "gochat-launcher");
    launcher.type = "button";
    launcher.setAttribute("aria-label", _t("widget.open_title", { title: title }));
    launcher.innerHTML = ICON_CHAT + ICON_CLOSE;
    var badge = el("span", "gochat-badge");
    launcher.appendChild(badge);

    // Begrüßung als kleiner Hinweis neben dem Button (einmal pro Sitzung)
    var teaser = null;
    if (greeting && !target) {
      teaser = el("div", "gochat-teaser");
      var teaserText = el("button", "gochat-teaser-text", greeting);
      teaserText.type = "button";
      var teaserX = el("button", "gochat-teaser-x");
      teaserX.type = "button";
      teaserX.setAttribute("aria-label", _t("widget.hinweis_schliessen"));
      teaserX.innerHTML = ICON_X;
      teaser.appendChild(teaserText);
      teaser.appendChild(teaserX);
      teaser.style.display = "none";
      teaserText.addEventListener("click", function () { hideTeaser(); setOpen(true); });
      teaserX.addEventListener("click", hideTeaser);
    }
    function hideTeaser() {
      if (!teaser) return;
      teaser.style.display = "none";
      try { sessionStorage.setItem("gochat:teaser", "1"); } catch (e) { /* egal */ }
    }

    root.appendChild(panel);
    if (teaser) root.appendChild(teaser);
    if (!target) root.appendChild(launcher);
    (target || document.body).appendChild(root);

    // --- Zustand ---
    var es = null, open = !!target, unread = 0, errTimer = null, lastMsg = null, unavailable = false;
    var DELETED_TEXT = _t("widget.nachricht_wurde_von_einem_moderator");

    // Eigene Nachrichten über die Nutzer-ID erkennen (ältere Nachrichten ohne ID über den Namen).
    function isOwn(userId, msgName) {
      return userId ? userId === myId : !!name && msgName === name;
    }
    function author(m) { return m.userId || "name:" + m.name; }

    function updateNameUI() {
      nameInput.style.display = name ? "none" : "";
      textInput.style.display = name ? "" : "none";
      nameHint.style.display = name ? "none" : "";
      pwInput.style.display = !name && acct && loginStep === 1 ? "" : "none";
      if (name) {
        sendBtn.className = "gochat-send";
        sendBtn.innerHTML = ICON_SEND;
        sendBtn.setAttribute("aria-label", _t("widget.senden"));
      } else {
        sendBtn.className = "gochat-send gochat-text-btn";
        sendBtn.textContent = codeMode ? _t("widget.uebernehmen") : _t("widget.weiter");
        sendBtn.removeAttribute("aria-label");
        nameInput.placeholder = codeMode ? _t("widget.profil_code_gochat1") : _t("widget.dein_name");
        nameInput.setAttribute("aria-label", codeMode ? _t("widget.profil_code") : _t("widget.dein_name"));
        nameInput.maxLength = codeMode ? 4000 : 32;
        hintText.textContent = codeMode
          ? _t("widget.den_code_findest_du_im_chat_unter_profil")
          : _t("widget.waehle_einen_namen_um_mitzuschreiben_er");
        codeToggle.textContent = codeMode ? _t("widget.doch_neuen_namen_waehlen") : _t("widget.profil_code_einfuegen");
        if (acct) {
          sendBtn.textContent = _t("widget.anmelden");
          nameInput.placeholder = loginStep === 2 ? _t("widget.code_aus_der_authenticator_app") : _t("widget.name");
          nameInput.setAttribute("aria-label", nameInput.placeholder);
          nameInput.autocomplete = loginStep === 2 ? "one-time-code" : "username";
          nameInput.maxLength = 200;
          hintText.textContent = loginStep === 2 ? _t("widget.dein_konto_ist_mit_zwei_faktor_anmeldung")
            : acct.guests === "none" ? _t("widget.dieser_chat_ist_nur_fuer_mitglieder")
            : _t("widget.zum_mitschreiben_melde_dich_mit_deinem");
          codeToggle.textContent = acct.registration ? _t("widget.konto_anlegen") : _t("widget.zum_chat");
        }
      }
      // eigene Nachrichten nachträglich markieren, falls der Name erst später feststeht
      var rows = list.querySelectorAll(".gochat-msg");
      for (var i = 0; i < rows.length; i++) {
        var own = isOwn(rows[i].getAttribute("data-user"), rows[i].getAttribute("data-name"));
        rows[i].classList.toggle("gochat-own", own);
      }
      form.style.display = unavailable ? "none" : "";
      if (unavailable) nameHint.style.display = "none";
    }

    function showError(msg) {
      errorBox.textContent = msg;
      clearTimeout(errTimer);
      errTimer = setTimeout(function () { errorBox.textContent = ""; }, 4000);
    }

    function setBadge() {
      badge.textContent = unread > 99 ? "99+" : String(unread);
      badge.className = "gochat-badge" + (unread ? " gochat-show" : "");
    }

    function fmtTime(t) {
      return new Date(t).toLocaleTimeString(i18nLocale, { hour: "2-digit", minute: "2-digit" });
    }

    function deletedText(m) {
      if (m.deletedBy === "automod") {
        return isOwn(m.userId, m.name) ? _t("widget.deine_nachricht_wurde_automatisch")
          : _t("widget.nachricht_wurde_automatisch_ausgeblendet");
      }
      if (m.deletedBy === "self") return _t("widget.nachricht_wurde_geloescht");
      return DELETED_TEXT;
    }

    function buildRow(m, cont) {
      var row = el("div", "gochat-msg" + (isOwn(m.userId, m.name) ? " gochat-own" : "") + (cont ? " gochat-cont" : ""));
      row.setAttribute("data-name", m.name);
      row.setAttribute("data-user", m.userId || "");
      row.setAttribute("data-id", m.id);
      row.title = fmtTime(m.time);
      var meta = el("div", "gochat-meta");
      meta.appendChild(el("b", null, m.name));
      if (m.bot) meta.appendChild(el("span", "gochat-bot", _t("widget.bot")));
      if (m.team) meta.appendChild(el("span", "gochat-bot gochat-team", _t("widget.team")));
      if (!m.bot && m.userId && roleUsers[m.userId]) {
        var tinted = false;
        for (var ri = 0; ri < roleUsers[m.userId].length; ri++) {
          var role = roleById[roleUsers[m.userId][ri]];
          if (!role) continue;
          var rb = el("span", "gochat-bot gochat-role", role.name);
          rb.style.setProperty("--gochat-role", role.color);
          meta.appendChild(rb);
          if (role.colorName && !tinted) {
            tinted = true;
            meta.firstChild.classList.add("gochat-role-name");
            meta.firstChild.style.setProperty("--gochat-role", role.color);
          }
        }
      }
      meta.appendChild(document.createTextNode(fmtTime(m.time)));
      row.appendChild(meta);
      if (m.reply) {
        var q = el("div", "gochat-reply");
        q.appendChild(el("b", null, m.reply.name || ""));
        var qs = el("span");
        if (m.reply.missing) qs.textContent = _t("widget.nachricht_nicht_mehr_vorhanden");
        else if (m.reply.deleted) qs.textContent = _t("widget.nachricht_wurde_geloescht");
        else if (window.GoChatFormat) window.GoChatFormat.preview(qs, m.reply.text || "", { cls: "gochat-", emotes: emotes, base: server });
        else qs.textContent = m.reply.text || "";
        q.appendChild(qs);
        row.appendChild(q);
      }
      var bubble = el("div", "gochat-bubble");
      fillBubble(bubble, m);
      row.appendChild(bubble);
      if (m.threadInfo && m.threadInfo.count) {
        var ts = el("a", "gochat-thread", "💬 " + _t("widget.thread_replies", { n: m.threadInfo.count }));
        ts.href = server + "/#" + encodeURIComponent(room);
        ts.target = "_blank"; ts.rel = "noopener";
        row.appendChild(ts);
      }
      if (!m.deleted && m.reactions && m.reactions.length) {
        var rs = el("div", "gochat-reactions");
        for (var i = 0; i < m.reactions.length; i++) {
          var r = m.reactions[i];
          var names = r.users.map(function (u) { return u.name; });
          var chip = el("span", "gochat-reaction" + (r.users.some(function (u) { return u.id === myId; }) ? " gochat-mine" : ""));
          var em = /^:([a-z0-9_]{2,32}):$/.exec(r.emoji);
          if (em && emotes && emotes.get(em[1])) {
            var img = el("img", "gochat-emote gochat-emote-mini");
            img.src = server + emotes.get(em[1]).url; img.alt = r.emoji; img.width = 16; img.height = 16;
            chip.appendChild(img);
            chip.appendChild(document.createTextNode(" " + r.users.length));
          } else {
            chip.textContent = r.emoji + " " + r.users.length;
          }
          chip.title = names.join(", ");
          rs.appendChild(chip);
        }
        row.appendChild(rs);
      }
      return row;
    }

    function addMessage(m) {
      if (m.thread) return; // Antworten in Threads zeigt nur die volle Chat-Seite
      var stick = list.scrollHeight - list.scrollTop - list.clientHeight < 60;
      var empty = list.querySelector(".gochat-empty");
      if (empty) empty.remove();
      var t = new Date(m.time);
      var cont = lastMsg && author(lastMsg) === author(m) && !m.reply && (t - new Date(lastMsg.time)) < 5 * 60 * 1000;
      lastMsg = m;
      list.appendChild(buildRow(m, cont));
      if (stick) list.scrollTop = list.scrollHeight;
    }

    function fillBubble(bubble, m) {
      bubble.textContent = "";
      bubble.className = "gochat-bubble" + (m.deleted ? " gochat-deleted" : "");
      if (m.deleted) { bubble.textContent = deletedText(m); return; }
      if (m.text) {
        if (window.GoChatFormat) window.GoChatFormat.render(bubble, m.text, { me: name, cls: "gochat-", emotes: emotes, base: server });
        else renderText(bubble, m.text);
      }
      if (m.poll) bubble.appendChild(buildPoll(m));
      if (m.media && m.media.kind === "image") {
        var a = el("a", "gochat-img-link");
        a.href = server + m.media.src; a.target = "_blank"; a.rel = "noopener";
        var pic = el("img", "gochat-gif");
        pic.src = server + m.media.src; pic.width = m.media.w; pic.height = m.media.h; pic.alt = _t("widget.bild"); pic.loading = "lazy";
        a.appendChild(pic);
        bubble.className += " gochat-has-media" + (m.text ? "" : " gochat-media-only");
        bubble.appendChild(a);
      }
      if (m.media && m.media.kind === "gif") {
        var img = el("img", "gochat-gif");
        img.src = server + m.media.src;
        img.width = m.media.w;
        img.height = m.media.h;
        img.alt = m.media.title || "GIF";
        img.loading = "lazy";
        bubble.className += " gochat-has-media" + (m.text ? "" : " gochat-media-only");
        bubble.appendChild(img);
      }
      if (m.editedAt) bubble.appendChild(el("span", "gochat-edited", "bearbeitet"));
    }

    // Umfrage mit Abstimmen (wenn ein Name gewählt ist)
    function buildPoll(m) {
      var p = m.poll, box = el("div", "gochat-poll" + (p.closed ? " gochat-closed" : ""));
      box.appendChild(el("div", "gochat-poll-q", p.question));
      var total = 0;
      p.options.forEach(function (o) { total += o.votes.length; });
      p.options.forEach(function (o, i) {
        var mine = o.votes.indexOf(myId) >= 0, pct = total ? Math.round(o.votes.length / total * 100) : 0;
        var b = el("button", "gochat-poll-opt" + (mine ? " gochat-mine" : ""));
        b.type = "button";
        b.disabled = p.closed || !token;
        b.style.setProperty("--gochat-pct", pct + "%");
        b.appendChild(el("span", "gochat-poll-text", (mine ? "✓ " : "") + o.text));
        b.appendChild(el("span", "gochat-poll-num", o.votes.length + " · " + pct + " %"));
        b.addEventListener("click", function () {
          gfetch(server + "/api/rooms/" + encodeURIComponent(room) + "/messages/" + m.id + "/vote", {
            method: "POST", headers: { "Content-Type": "application/json", "Authorization": "Bearer " + token },
            body: JSON.stringify({ option: i })
          }).then(function (res) { if (!res.ok) res.json().then(function (j) { showError(j.error || _t("widget.abstimmen_fehlgeschlagen")); }); });
        });
        box.appendChild(b);
      });
      if (p.closed) box.appendChild(el("div", "gochat-poll-foot", _t("widget.umfrage_beendet")));
      return box;
    }

    function greetingRow() {
      var row = el("div", "gochat-msg gochat-greeting");
      var meta = el("div", "gochat-meta");
      meta.appendChild(el("b", null, title));
      row.appendChild(meta);
      var bubble = el("div", "gochat-bubble");
      if (window.GoChatFormat) window.GoChatFormat.render(bubble, greeting, { cls: "gochat-" });
      else bubble.textContent = greeting;
      row.appendChild(bubble);
      return row;
    }

    // Kanal nur zum Lesen (nur bestimmte Rollen dürfen schreiben)
    var readOnly = false;
    function setWritable(ok) {
      readOnly = !ok;
      if (!textInput) return;
      textInput.disabled = readOnly;
      textInput.placeholder = readOnly ? _t("widget.hier_schreiben_nur_bestimmte_rollen") : _t("widget.nachricht_schreiben");
    }

    function disconnect() { if (es) { es.close(); es = null; } }

    // --- Verlauf: neueste Nachrichten, ältere per Knopf bzw. beim Hochscrollen ---
    var loaded = [], hasMore = false, loadingOlder = false;

    function renderAll() {
      list.innerHTML = "";
      lastMsg = null;
      if (hasMore) {
        var older = el("button", "gochat-older", _t("widget.aeltere_nachrichten_laden"));
        older.type = "button";
        older.addEventListener("click", loadOlder);
        if (olderObserver) olderObserver.observe(older);
        list.appendChild(older);
      }
      if (greeting && !hasMore) list.appendChild(greetingRow());
      if (!loaded.length && !hasMore && !greeting) {
        var empty = el("div", "gochat-empty");
        empty.appendChild(el("strong", null, _t("widget.noch_ist_es_ruhig_hier")));
        empty.appendChild(document.createTextNode(_t("widget.schreib_die_erste_nachricht")));
        list.appendChild(empty);
      }
      for (var i = 0; i < loaded.length; i++) addMessage(loaded[i]);
      for (var k = 0; k < ephs.length; k++) list.appendChild(ephRow(ephs[k]));
    }

    // Private Antwort des Befehls-Bots (nur hier sichtbar, nicht gespeichert)
    var ephs = [];
    function ephRow(x) {
      var row = el("div", "gochat-msg gochat-eph" + (x.notice ? " gochat-eph-notice" : ""));
      var meta = el("div", "gochat-meta");
      meta.appendChild(el("b", null, x.name));
      meta.appendChild(el("span", "gochat-bot", _t("widget.bot")));
      meta.appendChild(document.createTextNode(_t("widget.time_only_you", { time: fmtTime(x.time) })));
      row.appendChild(meta);
      var q = el("div", "gochat-reply");
      q.appendChild(el("b", null, name || ""));
      var qs = el("span");
      if (window.GoChatFormat) window.GoChatFormat.preview(qs, x.command, { cls: "gochat-", emotes: emotes, base: server });
      else qs.textContent = x.command;
      q.appendChild(qs);
      row.appendChild(q);
      var bubble = el("div", "gochat-bubble");
      if (window.GoChatFormat) window.GoChatFormat.render(bubble, x.text, { me: name, cls: "gochat-", emotes: emotes, base: server });
      else bubble.textContent = x.text;
      row.appendChild(bubble);
      var hide = el("button", "gochat-eph-hide", _t("widget.ausblenden"));
      hide.type = "button";
      hide.addEventListener("click", function () {
        ephs = ephs.filter(function (y) { return y !== x; });
        if (row.parentNode) row.parentNode.removeChild(row);
      });
      row.appendChild(hide);
      return row;
    }

    var olderObserver = window.IntersectionObserver ? new IntersectionObserver(function (entries) {
      for (var i = 0; i < entries.length; i++) if (entries[i].isIntersecting) { loadOlder(); return; }
    }, { root: list, rootMargin: "200px 0px 0px 0px" }) : null;

    function loadOlder() {
      if (loadingOlder || !hasMore || !loaded.length) return;
      loadingOlder = true;
      var btn = list.querySelector(".gochat-older");
      if (btn) btn.textContent = _t("widget.lade");
      var current = room;
      gfetch(server + "/api/rooms/" + encodeURIComponent(room) + "/messages?before=" + loaded[0].id + "&limit=100")
        .then(function (res) {
          if (!res.ok || room !== current) return;
          var more = res.headers.get("X-Has-More") === "true";
          return res.json().then(function (older) {
            var fromBottom = list.scrollHeight - list.scrollTop; // Position halten
            loaded = older.concat(loaded);
            hasMore = more;
            renderAll();
            list.scrollTop = list.scrollHeight - fromBottom;
          });
        })
        .catch(function () { if (btn) btn.textContent = _t("widget.aeltere_nachrichten_laden"); })
        .then(function () { loadingOlder = false; });
    }

    // Kanal gibt es nicht (mehr) – Hinweis statt Eingabefeld zeigen.
    function setUnavailable() {
      disconnect();
      unavailable = true;
      list.innerHTML = "";
      var box = el("div", "gochat-empty");
      box.appendChild(el("strong", null, _t("widget.der_chat_ist_gerade_nicht_verfuegbar")));
      box.appendChild(document.createTextNode(_t("widget.bitte_versuche_es_spaeter_noch_einmal")));
      list.appendChild(box);
      status.textContent = _t("widget.nicht_verfuegbar");
      status.className = "gochat-status gochat-err";
      updateNameUI();
    }

    function connect() {
      if (es || unavailable) return;
      es = new EventSource(server + "/api/rooms/" + encodeURIComponent(room) + "/events" + (viewer ? "?viewer=" + encodeURIComponent(viewer) : ""));
      es.onmessage = function (e) {
        var ev = JSON.parse(e.data);
        if (ev.type === "update") {
          for (var k = 0; k < loaded.length; k++) if (loaded[k].id === ev.message.id) loaded[k] = ev.message;
          var row = list.querySelector('.gochat-msg[data-id="' + ev.message.id + '"]');
          if (row) row.parentNode.replaceChild(buildRow(ev.message, row.classList.contains("gochat-cont")), row);
        } else if (ev.type === "media") {
          loadEmotes();
        } else if (ev.type === "roles") {
          loadRoles();
        } else if (ev.type === "removed") {
          loaded = loaded.filter(function (x) { return x.id !== ev.id; });
          var fromBottom = list.scrollHeight - list.scrollTop;
          renderAll();
          list.scrollTop = list.scrollHeight - fromBottom;
        } else if (ev.type === "user_renamed" || ev.type === "user_updated") {
          disconnect(); connect(); // Verlauf mit den neuen Namen neu laden
          return;
        } else if (ev.type === "room_renamed") {
          room = ev.room; // eingebettete Widgets folgen der Umbenennung bis zum Neuladen
          disconnect(); connect();
          return;
        } else if (ev.type === "room_deleted") {
          setUnavailable();
          return;
        } else if (ev.type === "history") {
          setWritable(!ev.roomInfo || ev.roomInfo.canWrite !== false);
          loaded = (ev.messages || []).slice();
          hasMore = !!ev.more;
          renderAll();
          list.scrollTop = list.scrollHeight;
        } else if (ev.type === "message") {
          loaded.push(ev.message);
          addMessage(ev.message);
          if (!open && !isOwn(ev.message.userId, ev.message.name)) { unread++; setBadge(); }
        }
        status.textContent = ev.online + " online";
        status.className = "gochat-status gochat-ok";
      };
      es.onerror = function () {
        status.textContent = _t("widget.verbindung_wird_wiederhergestellt");
        status.className = "gochat-status gochat-err";
        // Gibt es den Kanal überhaupt?
        gfetch(server + "/api/rooms/" + encodeURIComponent(room) + "/messages").then(function (res) {
          if (res.status === 404) setUnavailable();
        }, function () { /* Server offline – EventSource versucht es weiter */ });
      };
    }

    function setOpen(v) {
      if (target) return;
      open = v;
      root.className = root.className.replace(/\s*gochat-open/g, "") + (v ? " gochat-open" : "");
      if (v) {
        hideTeaser();
        connect();
        unread = 0; setBadge();
        list.scrollTop = list.scrollHeight;
        setTimeout(function () { (name ? textInput : nameInput).focus(); }, 0);
      }
    }

    // Namen beim Server reservieren. Ist schon ein Token da, wird umbenannt.
    function claim(n, quiet) {
      var headers = { "Content-Type": "application/json" };
      if (token) headers.Authorization = "Bearer " + token;
      sendBtn.disabled = true;
      return gfetch(server + "/api/session", {
        method: "POST", headers: headers, body: JSON.stringify({ name: n })
      }).then(function (res) {
        return res.json().then(function (j) {
          if (!res.ok) {
            if (!quiet) showError(j.error || _t("widget.name_konnte_nicht_gesetzt_werden"));
            else nameInput.value = n;
            return;
          }
          shareSession(j.token, j.name, j.id, j.viewer);
          textInput.focus();
        });
      }).catch(function () {
        if (!quiet) showError(_t("widget.server_nicht_erreichbar"));
      }).then(function () { sendBtn.disabled = false; });
    }

    // Profil-Code aus dem Chat übernehmen (gleicher Name wie in der Weboberfläche).
    function importCode(code) {
      var data = null;
      code = code.replace(/\s+/g, "");
      var pre = /^(UCNG1|GOCHAT1):/i.exec(code); // GOCHAT1: = Codes von vor der Umbenennung
      if (pre) {
        try {
          var b = code.slice(pre[0].length).replace(/-/g, "+").replace(/_/g, "/");
          while (b.length % 4) b += "=";
          var bin = atob(b), bytes = new Uint8Array(bin.length);
          for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
          data = JSON.parse(new TextDecoder().decode(bytes));
        } catch (e) { data = null; }
      }
      if (!data || !data.t) { showError(_t("widget.das_ist_kein_gueltiger_profil_code")); return; }
      sendBtn.disabled = true;
      gfetch(server + "/api/session", { headers: { "Authorization": "Bearer " + data.t } })
        .then(function (res) {
          if (res.ok) return res.json().then(function (j) {
            codeMode = false;
            nameInput.value = "";
            shareSession(data.t, j.name, j.id, j.viewer);
            textInput.focus();
          });
          showError(res.status === 401
            ? (data.s && data.s !== server ? _t("widget.dieser_code_gehoert_zu_einem_anderen") : _t("widget.dieses_profil_gilt_nicht_mehr"))
            : _t("widget.der_code_konnte_nicht_geprueft_werden"));
        })
        .catch(function () { showError(_t("widget.server_nicht_erreichbar")); })
        .then(function () { sendBtn.disabled = false; });
    }

    // Anmeldung mit Konto: Passwort wird wie in der Weboberfläche im Browser abgeleitet.
    function b64(buf) {
      var bytes = new Uint8Array(buf), s = "";
      for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
      return btoa(s);
    }
    function deriveAuth(pw, salt, iter) {
      var raw = Uint8Array.from(atob(salt), function (c) { return c.charCodeAt(0); });
      return crypto.subtle.importKey("raw", new TextEncoder().encode(pw.normalize("NFKC")), "PBKDF2", false, ["deriveBits"])
        .then(function (k) { return crypto.subtle.deriveBits({ name: "PBKDF2", hash: "SHA-256", salt: raw, iterations: iter }, k, 512); })
        .then(function (bits) { return b64(bits.slice(0, 32)); });
    }
    function postJSON(path, body) {
      return gfetch(server + path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) })
        .then(function (res) { return res.json().then(function (j) { return { ok: res.ok, j: j }; }, function () { return { ok: res.ok, j: {} }; }); });
    }
    function widgetLogin() {
      if (!window.crypto || !crypto.subtle) { showError(_t("widget.anmelden_geht_nur_auf_seiten_mit_https")); return; }
      var v = nameInput.value.trim();
      if (!v) return;
      sendBtn.disabled = true;
      var step = loginStep === 2
        ? Promise.resolve({ code: v.replace(/\s/g, "") })
        : postJSON("/api/account/prelogin", { login: v }).then(function (r) {
          loginName = v;
          return deriveAuth(pwInput.value, r.j.kdfSalt, r.j.kdfIter).then(function (a) { loginAuth = a; return {}; });
        });
      step.then(function (extra) {
        return postJSON("/api/account/login", { login: loginName, auth: loginAuth, code: extra.code || "" });
      }).then(function (r) {
        if (!r.ok) {
          if (r.j.need2fa) { loginStep = 2; nameInput.value = ""; updateNameUI(); nameInput.focus(); return; }
          showError(r.j.error || _t("widget.anmeldung_fehlgeschlagen"));
          return;
        }
        loginStep = 1; pwInput.value = ""; nameInput.value = ""; loginAuth = "";
        var s = r.j.session;
        shareSession(s.token, s.name, s.id, s.viewer);
        textInput.focus();
      }).catch(function () { showError(_t("widget.server_nicht_erreichbar")); })
        .then(function () { sendBtn.disabled = false; });
    }

    function send() {
      if (!name && acct) { widgetLogin(); return; }
      if (!name) {
        var n = nameInput.value.trim();
        if (!n) return;
        if (codeMode || /^(UCNG1|GOCHAT1):/i.test(n)) importCode(n);
        else claim(n, false);
        return;
      }
      var text = textInput.value.trim();
      if (!text) return;
      sendBtn.disabled = true;
      gfetch(server + "/api/rooms/" + encodeURIComponent(room) + "/messages", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Authorization": "Bearer " + token },
        body: JSON.stringify({ text: text })
      }).then(function (res) {
        if (res.ok) {
          textInput.value = ""; textInput.style.height = "";
          if (res.status === 200) {
            return res.json().then(function (j) {
              if (!j || !j.ephemeral) return;
              ephs.push(j.ephemeral);
              if (ephs.length > 20) ephs.shift();
              var empty = list.querySelector(".gochat-empty");
              if (empty) empty.parentNode.removeChild(empty);
              list.appendChild(ephRow(j.ephemeral));
              lastMsg = null;
              list.scrollTop = list.scrollHeight;
            }, function () {});
          }
          list.scrollTop = list.scrollHeight;
          return;
        }
        if (res.status === 401) { // Name abgelaufen oder freigegeben
          shareSession("", "", "", "");
          showError(_t("widget.bitte_waehle_erneut_einen_namen"));
          return;
        }
        if (res.status === 404) {
          return res.json().then(function (j) {
            if (/Kanal/.test(j.error || "")) setUnavailable(); else showError(j.error || _t("widget.senden_fehlgeschlagen"));
          }, function () { setUnavailable(); });
        }
        return res.json().then(function (j) { showError(j.error || _t("widget.senden_fehlgeschlagen")); },
                               function () { showError(_t("widget.senden_fehlgeschlagen")); });
      }).catch(function () {
        showError(_t("widget.server_nicht_erreichbar"));
      }).then(function () {
        sendBtn.disabled = false;
        textInput.focus();
      });
    }

    // --- Events ---
    launcher.addEventListener("click", function () { setOpen(!open); });
    closeBtn.addEventListener("click", function () { setOpen(false); });
    sendBtn.addEventListener("click", function (e) { e.preventDefault(); e.stopPropagation(); send(); });
    // Fokus im Eingabefeld halten: verhindert auf Smartphones, dass die Tastatur beim
    // Antippen zuklappt, das Layout springt und der Tipp danebengeht.
    sendBtn.addEventListener("pointerdown", function (e) {
      if (document.activeElement === textInput || document.activeElement === nameInput) e.preventDefault();
    });
    nameInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.isComposing) { e.preventDefault(); send(); }
    });
    textInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
    });
    textInput.addEventListener("input", function () {
      textInput.style.height = "auto";
      textInput.style.height = Math.min(textInput.scrollHeight, 120) + "px";
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && open && !target) setOpen(false);
    });

    // Token und Name zwischen mehreren Widgets auf derselben Seite synchron halten.
    function shareSession(t, n, id, v) {
      storeSet(tokenKey, t);
      window.dispatchEvent(new CustomEvent("gochat:session", { detail: { token: t, name: n, id: id, viewer: v || "" } }));
    }
    window.addEventListener("gochat:session", function (e) {
      var changed = viewer !== (e.detail.viewer || "");
      token = e.detail.token; name = e.detail.name; myId = e.detail.id || ""; viewer = e.detail.viewer || "";
      updateNameUI();
      if (changed && es) { disconnect(); connect(); } // in der Online-Liste mit Namen erscheinen
    });

    // Vorhandenes Token prüfen, sonst ggf. data-name automatisch reservieren.
    function restore() {
      if (!token) {
        if (opts.name) claim(String(opts.name), true);
        return;
      }
      gfetch(server + "/api/session", { headers: { "Authorization": "Bearer " + token } })
        .then(function (res) {
          if (res.ok) return res.json().then(function (j) {
            name = j.name; myId = j.id || ""; viewer = j.viewer || "";
            updateNameUI();
            if (es) { disconnect(); connect(); }
          });
          if (res.status === 401) {
            token = ""; storeSet(tokenKey, "");
            if (opts.name) claim(String(opts.name), true);
          }
        })
        .catch(function () { /* später erneut beim Senden */ });
    }

    pwInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter") { e.preventDefault(); send(); }
    });
    codeToggle.addEventListener("click", function () {
      if (acct) { window.open(server + "/", "_blank", "noopener"); return; }
      codeMode = !codeMode;
      nameInput.value = "";
      updateNameUI();
      nameInput.focus();
    });

    // Öffnungszeiten
    function loadHours() {
      gfetch(server + "/api/hours").then(function (r) { return r.json(); }).then(function (h) {
        if (!h.enabled) { hoursBar.style.display = "none"; return; }
        hoursBar.style.display = "";
        hoursBar.className = "gochat-hours " + (h.open ? "gochat-hours-open" : "gochat-hours-closed");
        hoursBar.textContent = "";
        var line = el("div", "gochat-hours-line");
        line.appendChild(el("b", null, h.open ? _t("widget.jetzt_erreichbar") : _t("widget.gerade_nicht_besetzt")));
        var when = h.open ? (h.today ? _t("widget.hours_today", { hours: h.today }) : "") : (h.next ? _t("widget.hours_next", { next: h.next }) : "");
        if (when) line.appendChild(el("span", null, when));
        hoursBar.appendChild(line);
        if (!h.open && h.message) hoursBar.appendChild(el("div", "gochat-hours-msg", h.message));
      }, function () { /* ohne Öffnungszeiten weiter */ });
    }
    if (showHours) { loadHours(); setInterval(loadHours, 5 * 60 * 1000); }

    if (teaser) {
      var seen = false;
      try { seen = sessionStorage.getItem("gochat:teaser") === "1"; } catch (e) { /* egal */ }
      if (!seen) setTimeout(function () { if (!open) teaser.style.display = ""; }, 1500);
    }

    // Rollen-Abzeichen laden
    var roleById = {}, roleUsers = {};
    function loadRoles() {
      gfetch(server + "/api/roles").then(function (r) { return r.json(); }).then(function (d) {
        roleById = {};
        (d.roles || []).forEach(function (r) { roleById[r.id] = r; });
        roleUsers = d.users || {};
        if (loaded.length) {
          var fromBottom = list.scrollHeight - list.scrollTop;
          renderAll();
          list.scrollTop = list.scrollHeight - fromBottom;
        }
      }, function () { /* ohne Rollen */ });
    }

    // Emotes laden (für :name: in Nachrichten)
    function loadEmotes() {
      gfetch(server + "/api/media").then(function (r) { return r.json(); }).then(function (d) {
        emotes = new Map((d.emotes || []).map(function (e) { return [e.name, e]; }));
        if (loaded.length) {
          var fromBottom = list.scrollHeight - list.scrollTop;
          renderAll();
          list.scrollTop = list.scrollHeight - fromBottom;
        }
      }, function () { /* ohne Emotes */ });
    }
    loadEmotes();
    loadRoles();

    // Für GoChat.init() ohne Auto-Start: Formatierung nachladen und neu zeichnen.
    if (!window.GoChatFormat) ensureFormat(server, function () { if (loaded.length || greeting) renderAll(); });

    updateNameUI();
    restore();
    if (target) connect();
    else if (opts.open === true || opts.open === "true") setOpen(true);
    else connect(); // im Hintergrund verbinden, damit der Ungelesen-Zähler funktioniert

    // Öffentliche API der Instanz
    return {
      open: function () { setOpen(true); },
      close: function () { setOpen(false); },
      toggle: function () { setOpen(!open); },
      setName: function (n) { return claim(String(n), false); },
      destroy: function () { if (es) es.close(); root.remove(); }
    };
  }

  // Öffentliche Schnittstelle: UCNG (früherer Name GoChat – derselbe Gegenstand, bleibt erhalten).
  window.GoChat = window.UCNG = window.UCNG || window.GoChat || {};
  window.GoChat.init = init;
  // Für UCNG.init(): erst Texte laden – UCNG.load({server, lang}, function () { UCNG.init({...}); })
  window.GoChat.load = function (o, cb) { o = o || {}; var srv = (o.server || defaultServer || "").replace(/\/$/, ""); ensureFormat(srv, function () { ensureI18n(srv, o.lang, cb || function () {}); }); };

  // Automatisch starten, wenn per <script data-...> eingebunden.
  if (script && script.dataset.manual !== "true") {
    var d = script.dataset;
    var server0 = (d.server || defaultServer || "").replace(/\/$/, "");
    var attrs = {
      server: d.server, room: d.room, title: d.title, color: d.color, position: d.position, target: d.target,
      name: d.name, open: d.open, css: d.css, theme: d.theme, greeting: d.greeting, hours: d.hours, lang: d.lang
    };
    var launch = function (base) {
      var o = {};
      for (var k in base) o[k] = base[k];
      for (var a in attrs) if (attrs[a] != null && attrs[a] !== "") o[a] = attrs[a];
      ensureFormat(server0, function () { ensureI18n(server0, o.lang, function () { window.GoChat.instance = init(o); }); });
    };
    var start = function () {
      if (!d.widget) { launch({}); return; }
      // Gespeichertes Widget: Einstellungen vom Server holen.
      fetch(server0 + "/api/widgets/" + encodeURIComponent(d.widget))
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (w) {
          launch({
            room: w.room, title: w.title, color: w.color, theme: w.theme, position: w.position,
            target: w.mode === "inline" ? w.target || "#gochat" : "", greeting: w.greeting, hours: w.showHours
          });
        }, function (e) {
          console.error(_t("widget.load_failed", { widget: d.widget }), e);
        });
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
    else start();
  }
})();
