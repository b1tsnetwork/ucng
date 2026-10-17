// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Live-Transkription im Kanal: Hinweis-Leiste, solange sie läuft, ein Fenster mit dem
// Transkript (live, mit Sprecher und Uhrzeit) und die früheren Transkripte zum Herunterladen.
// Starten und beenden können Moderierende (bzw. der Besitzer eines temporären Kanals). Der
// Text kommt vom Chat-Server (eigene Spracherkennung) – sichtbar für alle, die den Kanal lesen,
// auch ohne dem Sprachkanal beizutreten.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const t = (k, v) => window._t(k, v);
  const D = () => window.UCNGVoice && UCNGVoice.deps();

  let room = "";
  let voiceRoom = false;
  let active = null; // laufendes Transkript (Kopfdaten)
  let lines = [];
  let items = [];
  let allowed = false;
  let canStart = false;
  let open = false;
  let loadSeq = 0;

  const pad = (x) => String(x).padStart(2, "0");
  const hhmm = (ms) => { const d = new Date(ms); return pad(d.getHours()) + ":" + pad(d.getMinutes()); };
  const hhmmss = (ms) => { const d = new Date(ms); return hhmm(ms) + ":" + pad(d.getSeconds()); };
  const dateTime = (ms) => new Date(ms).toLocaleString(window.GoChatI18n ? GoChatI18n.locale : "de-DE", { dateStyle: "short", timeStyle: "short" });

  // Kanal gewechselt (oder neu geladen): Stand vom Server holen.
  async function setRoom(name, info) {
    room = name || "";
    voiceRoom = !!(info && info.voice);
    active = null; lines = []; items = []; allowed = false; canStart = false;
    if (!voiceRoom || !D()) { render(); return; }
    const seq = ++loadSeq;
    try {
      const j = await D().api(`/api/rooms/${encodeURIComponent(room)}/transcripts`);
      if (seq !== loadSeq) return;
      active = j.active || null;
      lines = j.lines || [];
      items = j.items || [];
      allowed = !!j.allowed;
      canStart = !!j.canStart;
    } catch { /* ohne Transkripte */ }
    syncVoice();
    render();
  }

  // Ereignis „transcript“ aus dem Live-Stream des Kanals.
  function onEvent(ev) {
    if (!ev || ev.room !== room || !ev.transcript) return;
    const tr = ev.transcript;
    if (tr.info) {
      const was = active;
      if (tr.info.active) {
        active = tr.info; lines = [];
        if (!was) notify(t("voice.tr_started_toast", { name: tr.info.byName || "?" }));
      } else if (was && was.id === tr.info.id) {
        active = null;
        notify(t("voice.tr_ended_toast"));
      }
      upsertItem(tr.info);
      syncVoice();
      render();
      return;
    }
    if (tr.line && active && tr.id === active.id) {
      // Abschnitte werden parallel verschriftlicht: nach Beginn einsortieren
      let i = lines.length;
      while (i > 0 && lines[i - 1].at > tr.line.at) i--;
      lines.splice(i, 0, tr.line);
      active.lines = (active.lines || 0) + 1;
      renderLines(true);
      return;
    }
    if (tr.id && !tr.line) { // gelöscht (Admin)
      items = items.filter((x) => x.id !== tr.id);
      if (active && active.id === tr.id) { active = null; lines = []; syncVoice(); }
      render();
    }
  }

  // Hinweis höchstens einmal – Kanal-Stream und Sprachverbindung melden Start/Ende beide.
  let lastNote = 0;
  function notify(text, on) {
    const now = Date.now();
    if (now - lastNote < 4000) return;
    lastNote = now;
    D().toast(text);
  }

  function upsertItem(info) {
    const i = items.findIndex((x) => x.id === info.id);
    if (i >= 0) items[i] = info; else items.unshift(info);
  }

  // Sprachleiste (voice-extras.js) zeigt den Hinweis ebenfalls
  function syncVoice() {
    const S = window.UCNGVoice && UCNGVoice.session();
    if (S && S.cfg && S.room === room) { S.cfg.transcript = active; UCNGVoice.render(); }
  }

  async function start() {
    try {
      const info = await D().api(`/api/voice/${encodeURIComponent(room)}/transcript`, { method: "POST", body: JSON.stringify({ on: true }) });
      active = info; lines = []; upsertItem(info); open = true;
      syncVoice(); render();
    } catch (e) { D().toast(e.message); }
  }

  async function stop() {
    try {
      await D().api(`/api/voice/${encodeURIComponent(room)}/transcript`, { method: "POST", body: JSON.stringify({ on: false }) });
    } catch (e) { D().toast(e.message); }
  }

  async function download(id, fmt) {
    try {
      const res = await fetch(`/api/transcripts/${encodeURIComponent(id)}${fmt === "json" ? "?format=json" : ""}`, { headers: { Authorization: "Bearer " + D().token() } });
      if (!res.ok) throw new Error(t("voice.tr_download_failed"));
      const blob = await res.blob();
      const cd = res.headers.get("Content-Disposition") || "";
      const m = /filename="([^"]+)"/.exec(cd);
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = m ? m[1] : `transkript-${room}.txt`;
      document.body.append(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 60000);
    } catch (e) { D().toast(e.message); }
  }

  function btn(cls, label, fn) {
    const b = D().h("button", cls, label);
    b.type = "button";
    b.addEventListener("click", fn);
    return b;
  }

  function render() {
    const hb = $("transcriptBtn"), bar = $("transcriptBar"), panel = $("transcriptPanel");
    if (!hb || !bar || !panel) return;
    const show = voiceRoom && !!D() && (allowed || items.length > 0 || !!active);
    hb.hidden = !show;
    hb.classList.toggle("live", !!active);
    hb.setAttribute("aria-pressed", String(open && show));
    // Hinweis-Leiste: deutlich, solange es läuft
    bar.hidden = !active || !voiceRoom;
    if (active) {
      const txt = D().h("span", "tr-bar-text");
      txt.append(D().h("b", null, t("voice.tr_live")), document.createTextNode(" " + t("voice.tr_live_hint", { time: hhmm(active.started), name: active.byName || "?" })));
      const acts = [txt];
      if (!open) acts.push(btn("btn-quiet tr-bar-btn", t("voice.tr_show"), () => { open = true; render(); }));
      if (canStart) acts.push(btn("btn-quiet tr-bar-btn danger", t("voice.tr_stop"), stop));
      bar.replaceChildren(...acts);
    }
    panel.hidden = !open || !show;
    if (panel.hidden) return;
    $("trTitle").textContent = active ? t("voice.tr_panel_live", { n: lines.length }) : t("voice.tr_panel_title");
    const head = [];
    if (active) head.push(btn("btn-quiet", t("voice.tr_download"), () => download(active.id)));
    if (canStart && !active) head.push(btn("btn-primary tr-start", t("voice.tr_start"), start));
    if (canStart && active) head.push(btn("btn-quiet danger", t("voice.tr_stop"), stop));
    $("trActions").replaceChildren(...head);
    const note = $("trNote");
    if (!active && canStart) note.textContent = t("voice.tr_start_hint");
    else if (!active && allowed) note.textContent = t("voice.tr_nobody_hint");
    else if (!active) note.textContent = t("voice.tr_not_allowed");
    else note.textContent = "";
    note.hidden = !note.textContent;
    renderLines(false);
    const past = items.filter((x) => !active || x.id !== active.id);
    const list = $("trPastList");
    $("trPast").hidden = !past.length;
    list.replaceChildren(...past.map((x) => {
      const li = D().h("li", "tr-past-item");
      const mins = x.ended ? Math.max(1, Math.round((x.ended - x.started) / 60000)) : 0;
      li.append(D().h("span", null, t("voice.tr_past_line", { when: dateTime(x.started), min: mins, n: x.lines || 0, name: x.byName || "?" })));
      li.append(btn("btn-quiet", "TXT", () => download(x.id)), btn("btn-quiet", "JSON", () => download(x.id, "json")));
      return li;
    }));
  }

  function renderLines(append) {
    const box = $("trLines");
    if (!box || $("transcriptPanel").hidden) return;
    const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 40; // nur mitlaufen, wenn unten
    box.hidden = !active;
    if (!active) return;
    $("trTitle").textContent = t("voice.tr_panel_live", { n: lines.length });
    if (!lines.length) {
      box.replaceChildren(D().h("p", "tr-empty", t("voice.tr_waiting")));
      return;
    }
    const make = (l, prev) => {
      const p = D().h("p", "tr-line" + (prev && prev.u === l.u ? " same" : ""));
      p.append(D().h("time", "tr-time", hhmmss(l.at)));
      if (!prev || prev.u !== l.u) p.append(D().h("b", "tr-who", l.n));
      p.append(D().h("span", "tr-text", l.x));
      return p;
    };
    if (append && box.children.length === lines.length - 1 && lines[lines.length - 1] && box.querySelector(".tr-line")) {
      box.append(make(lines[lines.length - 1], lines[lines.length - 2]));
    } else {
      box.replaceChildren(...lines.map((l, i) => make(l, lines[i - 1])));
    }
    if (stick || !append) box.scrollTop = box.scrollHeight;
  }

  function toggle(on) {
    open = on === undefined ? !open : !!on;
    render();
  }

  function init() {
    const hb = $("transcriptBtn");
    if (hb) hb.addEventListener("click", () => toggle());
    const c = $("trClose");
    if (c) c.addEventListener("click", () => toggle(false));
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init); else init();

  window.UCNGTranscript = {
    setRoom, onEvent, start, stop, toggle, download, notify,
    get active() { return active; },
    get canStart() { return canStart; },
    get room() { return room; },
  };
})();
