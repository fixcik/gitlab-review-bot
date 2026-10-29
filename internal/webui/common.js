// Shared helpers of every page of the bot (webui.Assets). Every string from
// the data is inserted with textContent / createTextNode, never as HTML.
var D = (function () {
  "use strict";
  var root = document.documentElement;
  var $ = function (id) { return document.getElementById(id); };

  // ---- theme (same storage key as /usage, so both pages agree) -------------
  var THEME_KEY = "reviewbot-usage-theme";
  var themeListeners = [];
  function getTheme() { try { var t = localStorage.getItem(THEME_KEY); return t === "light" || t === "dark" ? t : "system"; } catch (e) { return "system"; } }
  function setTheme(t) {
    try { if (t === "system") localStorage.removeItem(THEME_KEY); else localStorage.setItem(THEME_KEY, t); } catch (e) { /* storage blocked */ }
    if (t === "system") root.removeAttribute("data-theme"); else root.setAttribute("data-theme", t);
    syncTheme(t);
    themeListeners.forEach(function (f) { f(); });
  }
  function syncTheme(t) { each($("theme").children, function (b) { b.setAttribute("aria-pressed", String(b.dataset.t === t)); }); }
  function initTheme() {
    $("theme").addEventListener("click", function (e) { var b = e.target.closest("button"); if (b) setTheme(b.dataset.t); });
    syncTheme(getTheme());
    if (window.matchMedia) {
      var mq = window.matchMedia("(prefers-color-scheme: dark)");
      var f = function () { themeListeners.forEach(function (g) { g(); }); };
      if (mq.addEventListener) mq.addEventListener("change", f); else if (mq.addListener) mq.addListener(f);
    }
  }
  function onTheme(f) { themeListeners.push(f); }

  // ---- DOM ------------------------------------------------------------------
  function each(list, fn) { Array.prototype.forEach.call(list, fn); }
  function h(tag, props, kids) {
    var e = document.createElement(tag);
    if (props) for (var k in props) {
      var v = props[k];
      if (v === null || v === undefined || v === false) continue;
      if (k === "class") e.className = v;
      else if (k === "text") e.textContent = v;
      else if (k === "on") for (var ev in v) e.addEventListener(ev, v[ev]);
      // css: inline sizes and colours through CSSOM (allowed by the CSP), never a style attribute.
      else if (k === "css") for (var s in v) e.style[s] = v[s];
      else e.setAttribute(k, v === true ? "" : v);
    }
    add(e, kids);
    return e;
  }
  function add(e, kids) {
    if (kids === null || kids === undefined) return;
    (Array.isArray(kids) ? kids : [kids]).forEach(function (c) {
      if (c === null || c === undefined || c === false) return;
      e.appendChild(typeof c === "object" ? c : document.createTextNode(String(c)));
    });
  }
  function clear(e) { while (e.firstChild) e.removeChild(e.firstChild); }
  // safeHref keeps only http(s) and same-origin relative links.
  function safeHref(u) {
    if (typeof u !== "string" || !u) return null;
    if (u.charAt(0) === "/" && u.charAt(1) !== "/") return u;
    try { var p = new URL(u); return p.protocol === "https:" || p.protocol === "http:" ? p.href : null; } catch (e) { return null; }
  }

  // ---- formatting -------------------------------------------------------------
  var nf = new Intl.NumberFormat("ru-RU");
  function full(n) { return nf.format(n || 0); }
  function short(n) {
    n = n || 0;
    if (n >= 1e9) return (n / 1e9).toFixed(n >= 1e10 ? 0 : 1).replace(".", ",") + " млрд";
    if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1).replace(".", ",") + " млн";
    if (n >= 1e4) return Math.round(n / 1e3) + " тыс.";
    return full(n);
  }
  function usd(v) { v = v || 0; return "$" + (v >= 100 ? Math.round(v) : v >= 1 ? v.toFixed(2) : v.toFixed(3)); }
  function dur(ms) {
    ms = ms || 0;
    if (ms < 1000) return ms + " мс";
    var s = ms / 1000;
    if (s < 60) return (s < 10 ? s.toFixed(1).replace(".", ",") : Math.round(s)) + " с";
    var m = Math.floor(s / 60), r = Math.round(s % 60);
    if (m < 60) return m + " мин" + (r ? " " + r + " с" : "");
    return Math.floor(m / 60) + " ч " + (m % 60) + " мин";
  }
  function bytes(n) {
    n = n || 0;
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1).replace(".", ",") + " МБ";
    if (n >= 1024) return Math.round(n / 1024) + " КБ";
    return n + " Б";
  }
  function pad(x) { return x < 10 ? "0" + x : "" + x; }
  function time(s) { var d = new Date(s); return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds()); }
  function dateTime(s) {
    var d = new Date(s), now = new Date();
    var t = pad(d.getHours()) + ":" + pad(d.getMinutes());
    if (d.toDateString() === now.toDateString()) return "сегодня, " + t;
    var y = new Date(now); y.setDate(now.getDate() - 1);
    if (d.toDateString() === y.toDateString()) return "вчера, " + t;
    return pad(d.getDate()) + "." + pad(d.getMonth() + 1) + ", " + t;
  }
  function zero(s) { return !s || s.indexOf("0001-01-01") === 0; }
  function plural(n, one, few, many) {
    var n10 = n % 10, n100 = n % 100;
    if (n10 === 1 && n100 !== 11) return one;
    if (n10 >= 2 && n10 <= 4 && (n100 < 10 || n100 >= 20)) return few;
    return many;
  }

  var STATUS = { ok: "успешно", skipped: "пропущено", postponed: "отложено", failed: "ошибка", running: "выполняется", interrupted: "прервано",
    error: "ошибка", warn: "внимание", denied: "отказ", info: "инфо" };
  // Generated from core.AllJobKinds() (diagweb.kindLabels).
  var KIND = {{KIND_LABELS}};
  function badge(status) { return h("span", { class: "badge b-" + status, text: STATUS[status] || status }); }
  function kindLabel(k) { return KIND[k] || k; }

  // ---- ECharts theming from CSS variables -------------------------------------
  function cssVar(name) { return getComputedStyle(root).getPropertyValue(name).trim(); }
  function chartTheme() {
    return {
      text: cssVar("--text-2"), strong: cssVar("--text-strong"), line: cssVar("--line"), border: cssVar("--border"),
      tipBg: cssVar("--tip-bg"), tipText: cssVar("--tip-text"), surface: cssVar("--surface"),
      bar: cssVar("--bar"), danger: cssVar("--danger"), warn: cssVar("--warn"), ok: cssVar("--ok"), muted: cssVar("--text-3"),
      sIn: cssVar("--s-in"), sOut: cssVar("--s-out"), sCr: cssVar("--s-cr"), sCw: cssVar("--s-cw")
    };
  }
  // tooltipBox builds the tooltip as DOM (ECharts accepts an HTMLElement
  // from the formatter), so no data string is parsed as HTML.
  function tooltipBox(title, rows) {
    var box = h("div", { class: "tip-in" }, h("div", { class: "h", text: title }));
    rows.forEach(function (r) {
      box.appendChild(h("div", { class: "r" + (r.top ? " top" : "") }, [
        h("span", null, [r.color ? h("i", { class: "dot", "data-c": r.color }) : null, r.label]), h("span", { text: r.value })
      ]));
    });
    // Colors via CSSOM (allowed by the CSP), not via style attributes.
    each(box.querySelectorAll("i[data-c]"), function (i) { i.style.background = i.getAttribute("data-c"); });
    return box;
  }

  // ---- filter bar (ReviewFilters, webassets.FiltersPath;
  // docs/usage-and-diagnostics.md, «Панель фильтров») --------------------------
  // mount(el, opts): fields from JS; enhance(el, opts): fields from the native
  // controls inside el (they keep the value, GET forms submit as before).
  // Both return the bar, or null when the library did not load (the native
  // controls then stay as they are).
  var filters = {
    mount: function (el, opts) { return window.ReviewFilters ? window.ReviewFilters.mount(el, opts) : null; },
    enhance: function (el, opts) { return window.ReviewFilters ? window.ReviewFilters.enhance(el, opts) : null; }
  };

  // windowLabel turns a window like "48h" or "7d" into "48 ч" / "7 дн.".
  function windowLabel(w) {
    var m = /^(\d+)([hd])$/.exec(w || "");
    return m ? m[1] + (m[2] === "h" ? " ч" : " дн.") : (w || "");
  }

  // repoName is the last segment of a project path ("group/sub/api" -> "api").
  function repoName(p) { var i = (p || "").lastIndexOf("/"); return i < 0 ? (p || "") : p.slice(i + 1); }

  return { $: $, h: h, windowLabel: windowLabel, repoName: repoName, add: add, clear: clear, each: each, safeHref: safeHref, full: full, short: short, usd: usd, dur: dur,
    bytes: bytes, time: time, dateTime: dateTime, zero: zero, plural: plural, badge: badge, kindLabel: kindLabel,
    STATUS: STATUS, filters: filters, initTheme: initTheme, onTheme: onTheme, chartTheme: chartTheme, tooltipBox: tooltipBox };
})();
