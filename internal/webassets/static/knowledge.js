/* KnowledgePage: the /knowledge page on top of its server-rendered HTML
   (internal/knowledgeweb/page.html). Without this file the page works through
   its forms and links; with it:
     - the accumulated precision chart is drawn as SVG from the JSON of
       #k-series (one point per decision; hover, focus or tap shows it);
     - outcome bars of «По проектам» show their counts in a tooltip;
     - a decision row opens its saved text;
     - rule checkboxes drive the group actions (plain POST form #kb-bulk;
       the checkboxes belong to it through the form attribute), with the
       limit of ids per request;
     - «скрыть проекты без решений» hides the empty rows.
   CSP-safe: no network, no inline styles or handlers (positions go through
   CSSOM style.setProperty), text only via textContent. Needs filters.js and
   common.js, loaded before. */
(function (global) {
  "use strict";

  var doc = document;
  var SVG = "http://www.w3.org/2000/svg";
  function all(sel, root) { return Array.prototype.slice.call((root || doc).querySelectorAll(sel)); }
  function one(sel, root) { return (root || doc).querySelector(sel); }
  function svg(tag, attrs, parent) {
    var n = doc.createElementNS(SVG, tag);
    Object.keys(attrs || {}).forEach(function (k) { n.setAttribute(k, String(attrs[k])); });
    if (parent) parent.appendChild(n);
    return n;
  }

  /* ---- tooltip ---------------------------------------------------------- */
  var tip = null;
  /* lines: [{text, cls}] — the first one is the heading */
  function showTip(el, lines) {
    tip = tip || one("#k-tip");
    if (!tip) return;
    while (tip.firstChild) tip.removeChild(tip.firstChild);
    lines.forEach(function (l, i) {
      var d = doc.createElement("div");
      d.className = i === 0 ? "h" : (l.cls || "");
      d.textContent = l.text;
      tip.appendChild(d);
    });
    tip.hidden = false;
    var r = el.getBoundingClientRect(), w = tip.offsetWidth, h = tip.offsetHeight;
    var vw = doc.documentElement.clientWidth;
    var x = r.left + r.width / 2 - w / 2, y = r.top - h - 8;
    x = Math.max(8, Math.min(x, vw - w - 8));
    if (y < 8) y = r.bottom + 8;
    tip.style.setProperty("left", Math.round(x + global.scrollX) + "px");
    tip.style.setProperty("top", Math.round(y + global.scrollY) + "px");
  }
  function hideTip() { if (tip) tip.hidden = true; }

  /* ---- precision chart -------------------------------------------------- */
  var LABEL = { fixed: "принято", dismissed: "отклонено" };
  function pct(v) { return Math.round(v * 100) + "%"; }

  function readSeries() {
    var s = one("#k-series");
    if (!s) return [];
    try {
      var v = JSON.parse(s.textContent || "[]");
      return Array.isArray(v) ? v : [];
    } catch (e) { return []; }
  }

  function drawChart(host, list) {
    var W = 640, H = 180, L = 38, R = 12, T = 12, B = 24, iw = W - L - R, ih = H - T - B, n = list.length;
    var root = svg("svg", { viewBox: "0 0 " + W + " " + H, role: "img", "aria-label": "Накопленная точность по мере решений команды" });
    function y(v) { return T + ih - v * ih; }
    function x(i) { return L + (n === 1 ? iw / 2 : i * iw / (n - 1)); }
    [0, 0.5, 1].forEach(function (g) {
      svg("line", { "class": "gl", x1: L, x2: W - R, y1: y(g), y2: y(g) }, root);
      var t = svg("text", { "class": "ax", x: L - 6, y: y(g) + 4, "text-anchor": "end" }, root);
      t.textContent = Math.round(g * 100) + "%";
    });
    var fixed = 0, pts = list.map(function (o, i) {
      if (o.s === "fixed") fixed++;
      return { o: o, i: i, f: fixed, v: fixed / (i + 1), x: x(i) };
    });
    var d = "";
    pts.forEach(function (p, i) { d += (i ? "H" + p.x.toFixed(1) + "V" : "M" + p.x.toFixed(1) + " ") + y(p.v).toFixed(1); });
    svg("path", { "class": "area", d: d + "V" + (T + ih) + "H" + pts[0].x.toFixed(1) + "Z" }, root);
    svg("path", { "class": "ln", d: d }, root);
    var r = n > 120 ? 2 : n > 40 ? 3 : 4.5;
    var dots = pts.map(function (p) {
      return svg("circle", { "class": "pt c-" + (p.o.s === "fixed" ? "fixed" : "dismissed"), cx: p.x.toFixed(1), cy: y(p.v).toFixed(1), r: r }, root);
    });
    /* x labels: at most ~7, no repeats of the same day */
    /* all on one day: label the time instead */
    var oneDay = list[0].x === list[n - 1].x;
    var step = Math.max(1, Math.ceil(n / 7)), last = "";
    pts.forEach(function (p, i) {
      var lab = oneDay ? p.o.h : p.o.x;
      if ((i % step !== 0 && i !== n - 1) || lab === last) return;
      last = lab;
      var anchor = n === 1 ? "middle" : i === 0 ? "start" : i === n - 1 ? "end" : "middle";
      var t = svg("text", { "class": "ax", x: p.x.toFixed(1), y: H - 6, "text-anchor": anchor }, root);
      t.textContent = lab;
    });
    var hw = n === 1 ? iw : Math.max(4, iw / (n - 1));
    var cur = -1;
    function lines(p) {
      return [
        { text: (p.i + 1) + "-е решение: " + (LABEL[p.o.s] || p.o.s) },
        { text: p.o.t },
        { text: p.o.w + " · " + p.o.d, cls: "m" },
        { text: "точность после него: " + pct(p.v) + " (" + p.f + " из " + (p.i + 1) + ")", cls: "m" }
      ];
    }
    function focus(i, el) {
      if (cur >= 0) dots[cur].classList.remove("on");
      cur = i;
      dots[i].classList.add("on");
      showTip(el || dots[i], lines(pts[i]));
    }
    function blur() { if (cur >= 0) dots[cur].classList.remove("on"); cur = -1; hideTip(); }
    pts.forEach(function (p) {
      var hit = svg("rect", { "class": "hit", x: (p.x - hw / 2).toFixed(1), y: T, width: hw.toFixed(1), height: ih, "data-i": p.i }, root);
      hit.addEventListener("mouseenter", function () { focus(p.i, dots[p.i]); });
      hit.addEventListener("click", function () { focus(p.i, dots[p.i]); });
      hit.addEventListener("mouseleave", blur);
    });
    /* keyboard: the chart is one tab stop, arrows move between decisions */
    host.setAttribute("tabindex", "0");
    host.addEventListener("keydown", function (e) {
      var k = e.key, i = cur;
      if (k === "ArrowRight" || k === "ArrowDown") i = Math.min(n - 1, cur + 1);
      else if (k === "ArrowLeft" || k === "ArrowUp") i = Math.max(0, cur < 0 ? 0 : cur - 1);
      else if (k === "Home") i = 0;
      else if (k === "End") i = n - 1;
      else if (k === "Escape") { blur(); return; }
      else return;
      e.preventDefault();
      focus(i);
    });
    host.addEventListener("focus", function () { if (cur < 0) focus(n - 1); });
    host.addEventListener("blur", blur);
    host.appendChild(root);
  }

  function initChart() {
    var host = one("[data-chart]");
    var list = readSeries();
    if (host && list.length) drawChart(host, list);
  }

  /* ---- outcome bars ----------------------------------------------------- */
  function initBars() {
    all("[data-tip]").forEach(function (el) {
      function show() {
        var parts = (el.getAttribute("data-tip") || "").split("\n");
        showTip(el, parts.map(function (t, i) { return { text: t, cls: i ? "" : "h" }; }));
      }
      el.addEventListener("mouseenter", show);
      el.addEventListener("focus", show);
      el.addEventListener("mouseleave", hideTip);
      el.addEventListener("blur", hideTip);
    });
  }

  /* ---- decisions -------------------------------------------------------- */
  function toggleDecision(tb) {
    var open = !tb.classList.contains("open");
    tb.classList.toggle("open", open);
    var b = one("[data-fx]", tb);
    if (b) b.setAttribute("aria-expanded", String(open));
  }
  function initDecisions() {
    doc.addEventListener("click", function (e) {
      var fx = e.target.closest("[data-fx]");
      if (fx) { toggleDecision(fx.closest("tbody")); return; }
      var row = e.target.closest(".k-dec tr.r");
      if (row && !e.target.closest("a, button, input, label")) {
        var sel = global.getSelection && global.getSelection();
        if (sel && String(sel).length) return; /* selecting text, not opening */
        toggleDecision(row.closest("tbody"));
      }
    });
  }

  /* ---- selection and group actions -------------------------------------- */
  var CB = 'input[type="checkbox"][name="id"][form="kb-bulk"]';
  function boxes() { return all(CB); }
  function updateBulk() {
    var bulk = one("[data-bulk]");
    if (!bulk) return;
    var max = parseInt(bulk.getAttribute("data-max"), 10) || 200;
    var on = boxes().filter(function (c) {
      var tr = c.closest("tr");
      if (tr) tr.classList.toggle("sel", c.checked);
      return c.checked;
    });
    var noProj = on.some(function (c) { return c.hasAttribute("data-noproj"); });
    bulk.classList.toggle("on", on.length > 0);
    var label = one("[data-bulk-n]", bulk);
    if (label) label.textContent = on.length > max ? "Выбрано: " + on.length + " — за раз можно не больше " + max : "Выбрано: " + on.length;
    all("[data-bulk-btn]", bulk).forEach(function (b) {
      b.disabled = on.length === 0 || on.length > max || (b.hasAttribute("data-needs-project") && noProj);
      b.title = b.hasAttribute("data-needs-project") && noProj ? "У части выбранных правил нет проекта" : "";
    });
    var sa = one("[data-select-all]");
    if (sa) {
      var list = boxes();
      sa.checked = list.length > 0 && on.length === list.length;
      sa.indeterminate = on.length > 0 && on.length < list.length;
    }
  }
  function initSelection() {
    doc.addEventListener("change", function (e) {
      var t = e.target;
      if (!t.matches) return;
      if (t.matches("[data-select-all]")) {
        boxes().forEach(function (c) { c.checked = t.checked; });
        updateBulk();
      } else if (t.matches(CB)) updateBulk();
    });
    var clear = one("[data-bulk-clear]");
    if (clear) clear.addEventListener("click", function () {
      boxes().forEach(function (c) { c.checked = false; });
      updateBulk();
    });
    updateBulk();
  }

  /* ---- «скрыть пустые» -------------------------------------------------- */
  function initHideEmpty() {
    var cb = one("[data-hide-empty]"), table = one("[data-proj]"), count = one("[data-proj-count]");
    if (!cb || !table) return;
    var rows = all("tbody tr", table), empty = all("tbody tr[data-empty]", table);
    function apply() {
      table.classList.toggle("hide-empty", cb.checked);
      if (count) count.textContent = cb.checked ? (rows.length - empty.length) + " / " + rows.length : String(rows.length);
    }
    cb.addEventListener("change", apply);
    apply();
  }

  function init() {
    initChart();
    initBars();
    initDecisions();
    initSelection();
    initHideEmpty();
    global.addEventListener("scroll", hideTip, { passive: true });
  }
  global.KnowledgePage = { init: init, version: "1" };
  init();
})(window);
