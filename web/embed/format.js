// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
/* UCNG – einfache Formatierung für Nachrichten (Weboberfläche und Widget).
 *
 *   **fett**   *kursiv* oder _kursiv_   ~~durchgestrichen~~   `Code`   ```Codeblock```
 *   Links (https://… und www.…) werden klickbar, @Name wird hervorgehoben,
 *   :name: wird zum Emote (opts.emotes: Map name → {url, size}). Besteht eine Nachricht nur
 *   aus Emotes, erscheinen sie in voller Größe (32 bzw. 64 px), im Text höchstens zeilenhoch.
 *
 * Es wird nie HTML aus dem Text übernommen – alles entsteht über DOM-Knoten.
 * Nutzung: GoChatFormat.render(element, text, { me: "Mein Name", cls: "präfix-" })
 */
(function (global) {
  "use strict";

  var INLINE = new RegExp(
    [
      "(`[^`\\n]+`)",                                         // 1 Code
      "(\\*\\*(?=\\S)[^*\\n]*?\\S\\*\\*)",                    // 2 fett
      "(~~(?=\\S)[^~\\n]*?\\S~~)",                            // 3 durchgestrichen
      "((?:^|(?<=[\\s(\"'„]))\\*(?=[^\\s*])[^*\\n]*?[^\\s*]\\*(?![\\p{L}\\p{N}]))", // 4 kursiv *
      "((?:^|(?<=[\\s(\"'„]))_(?=[^\\s_])[^_\\n]*?[^\\s_]_(?![\\p{L}\\p{N}]))",     // 5 kursiv _
      "((?:https?:\\/\\/|www\\.)[^\\s<>\"']+)",               // 6 Link
      "(@[\\p{L}\\p{N}_.\\-]+(?: [\\p{Lu}][\\p{L}\\p{N}_.\\-]*)?)", // 7 Erwähnung (ein oder zwei Wörter)
      "((?<![\\p{L}\\p{N}:]):[a-z0-9_]{2,32}:(?![\\p{L}\\p{N}:]))", // 8 Emote
      "(\\*{3,})"                                              // 9 Sternchen (z. B. vom Wortfilter) bleiben, wie sie sind
    ].join("|"),
    "gu"
  );

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function link(url, cls) {
    // Satzzeichen am Ende gehören meist nicht zum Link.
    var trail = "";
    var m = /[.,;:!?)\]}"'»]+$/.exec(url);
    if (m) { trail = m[0]; url = url.slice(0, -trail.length); }
    var a = el("a", null, url);
    a.href = /^www\./i.test(url) ? "https://" + url : url;
    a.target = "_blank";
    a.rel = "noopener noreferrer nofollow";
    return [a, trail];
  }

  function inline(target, text, o, depth) {
    var re = new RegExp(INLINE.source, "gu"), last = 0, m;
    while ((m = re.exec(text))) {
      if (m.index > last) target.append(text.slice(last, m.index));
      last = m.index + m[0].length;
      var s = m[0];
      if (m[9]) target.append(s);
      else if (m[1]) target.append(el("code", o.cls + "code", s.slice(1, -1)));
      else if (m[2] && depth < 3) { var b = el("strong"); inline(b, s.slice(2, -2), o, depth + 1); target.append(b); }
      else if (m[3] && depth < 3) { var d = el("del"); inline(d, s.slice(2, -2), o, depth + 1); target.append(d); }
      else if ((m[4] || m[5]) && depth < 3) { var i = el("em"); inline(i, s.slice(1, -1), o, depth + 1); target.append(i); }
      else if (m[6]) { var l = link(s, o.cls); target.append(l[0]); if (l[1]) target.append(l[1]); }
      else if (m[8]) {
        var em = o.emotes && o.emotes.get(s.slice(1, -1));
        if (em) target.append(emoteImg(em, s, o, o.jumbo));
        else target.append(s);
      }
      else if (m[7]) {
        var name = s.slice(1), lower = name.toLowerCase(), me = (o.me || "").toLowerCase();
        // "@Tom Bauer": zweites Wort nur mitnehmen, wenn der volle Name gemeint ist
        if (name.indexOf(" ") > 0 && !(me && lower === me) && !(o.names && o.names.has(lower)) && !(o.meAlso && o.meAlso.indexOf(lower) >= 0)) {
          var first = s.split(" ")[0];
          last = m.index + first.length;
          name = first.slice(1); lower = name.toLowerCase();
          s = first;
        }
        var mine = (me && lower === me) || (o.meAlso && o.meAlso.indexOf(lower) >= 0);
        var cls = o.cls + "mention" + (mine ? " " + o.cls + "mention-me" : "");
        target.append(el("span", cls, s));
      } else target.append(s);
    }
    if (last < text.length) target.append(text.slice(last));
  }

  function emoteImg(em, alt, o, jumbo) {
    var img = el("img", o.cls + "emote" + (jumbo ? " " + o.cls + "emote-jumbo" : ""));
    var size = jumbo ? em.size : Math.min(em.size, 32);
    img.src = (o.base || "") + em.url;
    img.alt = alt;
    img.title = alt;
    img.width = size;
    img.height = size;
    img.loading = "lazy";
    img.decoding = "async";
    img.draggable = false;
    return img;
  }

  // Nur Emotes (und Leerzeichen)? Dann groß darstellen.
  var ONLY_EMOTES = /^(\s*:[a-z0-9_]{2,32}:\s*){1,12}$/;
  function onlyEmotes(text, emotes) {
    if (!emotes || !ONLY_EMOTES.test(text)) return false;
    var names = text.match(/:[a-z0-9_]{2,32}:/g) || [];
    for (var i = 0; i < names.length; i++) if (!emotes.has(names[i].slice(1, -1))) return false;
    return true;
  }

  function render(target, text, opts) {
    var o = { me: "", cls: "", names: null, emotes: null, base: "" };
    for (var k in opts || {}) o[k] = opts[k];
    o.jumbo = onlyEmotes(text, o.emotes);
    if (o.jumbo) {
      var parts = text.match(/:[a-z0-9_]{2,32}:/g);
      var wrap = el("span", o.cls + "emote-line");
      for (var j = 0; j < parts.length; j++) wrap.append(emoteImg(o.emotes.get(parts[j].slice(1, -1)), parts[j], o, true));
      target.append(wrap);
      return;
    }
    var re = /```(?:[a-z0-9+-]*\n)?([\s\S]*?)```/gi, last = 0, m;
    while ((m = re.exec(text))) {
      if (m.index > last) inline(target, text.slice(last, m.index), o, 0);
      var pre = el("pre", o.cls + "pre");
      pre.append(el("code", null, m[1].replace(/\n$/, "")));
      target.append(pre);
      last = m.index + m[0].length;
    }
    if (last < text.length) inline(target, text.slice(last), o, 0);
  }

  // Erwähnt der Text @name? (gleiche Regel wie auf dem Server)
  function mentions(text, name) {
    if (!name) return false;
    var lt = text.toLowerCase(), needle = "@" + name.toLowerCase(), from = 0, i;
    while ((i = lt.indexOf(needle, from)) >= 0) {
      var next = lt.slice(i + needle.length).charAt(0);
      if (!next || !/[\p{L}\p{N}_-]/u.test(next)) return true;
      from = i + needle.length;
    }
    return false;
  }

  // Formatierung entfernen (für Vorschauen und Benachrichtigungen).
  function plain(text) {
    return text.replace(/```(?:[a-z0-9+-]*\n)?([\s\S]*?)```/gi, "$1")
      .replace(/\*\*(.+?)\*\*/g, "$1").replace(/~~(.+?)~~/g, "$1")
      .replace(/`([^`]+)`/g, "$1").replace(/\s+/g, " ").trim();
  }

  // Einzeilige Vorschau (Antwort-Zitat, Eingabeleiste): ohne Formatierung,
  // aber bekannte :emotes: als kleine Bilder. max kürzt den Text.
  function preview(target, text, opts) {
    var o = { cls: "", emotes: null, base: "", max: 0 };
    for (var k in opts || {}) o[k] = opts[k];
    var t = plain(text || "");
    if (o.max && t.length > o.max) t = t.slice(0, o.max).replace(/:[a-z0-9_]*$/, "") + " …";
    var re = /:([a-z0-9_]{2,32}):/g, last = 0, m;
    while ((m = re.exec(t))) {
      var em = o.emotes && o.emotes.get(m[1]);
      if (!em) continue;
      if (m.index > last) target.append(t.slice(last, m.index));
      var img = emoteImg(em, m[0], o, false);
      img.className = o.cls + "emote " + o.cls + "emote-mini";
      img.width = img.height = 18;
      target.append(img);
      last = m.index + m[0].length;
    }
    if (last < t.length) target.append(t.slice(last));
  }

  global.GoChatFormat = { render: render, mentions: mentions, plain: plain, preview: preview };
})(window);
