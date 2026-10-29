/* ProblemsPage: the /problems page on top of its server-rendered HTML
   (internal/problemsweb/page.html). Without this file the page works through
   its forms; with it:
     - the filter bar does not reload the page for the text search («Поиск»
       hides the rows that do not match; the other fields submit the GET form);
     - «Как исправить» opens the instructions of a row;
     - row checkboxes drive «Скрыть выбранные» / «Вернуть выбранные» (plain
       POST forms; the checkboxes belong to them through the form attribute);
     - «Копировать» copies a snippet.
   CSP-safe: no network, no inline styles or handlers, text only via
   textContent. Needs filters.js (ReviewFilters) and common.js, loaded before. */
(function (global) {
  "use strict";

  var doc = document;
  function all(sel, root) { return Array.prototype.slice.call((root || doc).querySelectorAll(sel)); }
  function one(sel, root) { return (root || doc).querySelector(sel); }
  /* row checkboxes (the hide/return forms of a row also carry a hidden
     input named "fingerprint") */
  var CB = 'input[type="checkbox"][name="fingerprint"]';

  /* ---- search without a reload ---------------------------------------- */
  function setParam(href, key, value) {
    var u = new URL(href, location.href);
    if (value) u.searchParams.set(key, value); else u.searchParams.delete(key);
    return u.pathname + (u.search || "");
  }
  function setBackParam(raw, key, value) {
    var p = new URLSearchParams(raw);
    if (value) p.set(key, value); else p.delete(key);
    return p.toString();
  }
  function applySearch(q) {
    q = (q || "").trim();
    var ql = q.toLowerCase(), visible = 0;
    all("tbody.p").forEach(function (tb) {
      var hide = !!ql && (tb.getAttribute("data-q") || "").indexOf(ql) < 0;
      tb.hidden = hide;
      if (hide) all(CB, tb).forEach(function (c) { c.checked = false; });
      else visible++;
    });
    var total = one("[data-total]");
    if (total) total.textContent = String(visible);
    var empty = one("[data-qempty]");
    if (empty) empty.hidden = visible > 0 || !all("tbody.p").length;
    /* keep the search in the address, the links and the "back" of every form */
    try { history.replaceState(null, "", setParam(location.href, "q", q)); } catch (e) { /* ignore */ }
    all('main a[href^="/problems"]').forEach(function (a) { a.setAttribute("href", setParam(a.getAttribute("href"), "q", q)); });
    all('input[name="back"]').forEach(function (i) { i.value = setBackParam(i.value, "q", q); });
    updateBulk();
  }

  function initFilters() {
    var form = one("form.filters");
    if (!form || !global.ReviewFilters) return;
    var bar = global.ReviewFilters.enhance(form, { submit: false });
    var prev = bar.get();
    bar.on("change", function (vals) {
      var others = false;
      Object.keys(vals).forEach(function (k) {
        if (k !== "q" && JSON.stringify(vals[k]) !== JSON.stringify(prev[k])) others = true;
      });
      var qChanged = vals.q !== prev.q;
      prev = vals;
      if (others) { if (form.requestSubmit) form.requestSubmit(); else form.submit(); }
      else if (qChanged) applySearch(vals.q);
    });
  }

  /* ---- selection ------------------------------------------------------- */
  function boxes() { return all("tbody.p:not([hidden]) " + CB); }
  function updateBulk() {
    var bulk = one("[data-bulk]");
    var n = {}, total = 0;
    all("tbody.p " + CB).forEach(function (c) {
      var tb = c.closest("tbody");
      if (tb) tb.classList.toggle("sel", c.checked);
      if (!c.checked) return;
      var f = c.getAttribute("form");
      n[f] = (n[f] || 0) + 1;
      total++;
    });
    if (bulk) {
      bulk.classList.toggle("on", total > 0);
      var label = one("[data-bulk-n]", bulk);
      if (label) label.textContent = "Выбрано: " + total;
      all("[data-bulk-btn]", bulk).forEach(function (b) { b.disabled = !n[b.getAttribute("data-bulk-btn")]; });
    }
    var sa = one("[data-select-all]");
    if (sa) {
      var vis = boxes(), on = vis.filter(function (c) { return c.checked; }).length;
      sa.disabled = vis.length === 0;
      sa.checked = vis.length > 0 && on === vis.length;
      sa.indeterminate = on > 0 && on < vis.length;
    }
  }
  function initSelection() {
    doc.addEventListener("change", function (e) {
      var t = e.target;
      if (t.matches && t.matches("[data-select-all]")) {
        boxes().forEach(function (c) { c.checked = t.checked; });
        updateBulk();
      } else if (t.matches && t.matches("tbody.p " + CB)) updateBulk();
    });
    var clear = one("[data-bulk-clear]");
    if (clear) clear.addEventListener("click", function () {
      all("tbody.p " + CB).forEach(function (c) { c.checked = false; });
      updateBulk();
    });
    updateBulk();
  }

  /* ---- «Как исправить» and «Копировать» --------------------------------- */
  function copied(btn) {
    btn.classList.add("done");
    btn.textContent = "Скопировано";
    setTimeout(function () { btn.classList.remove("done"); btn.textContent = "Копировать"; }, 1600);
  }
  function copyFallback(pre, btn) {
    /* plain http (no navigator.clipboard): select the text and copy it */
    var r = doc.createRange();
    r.selectNodeContents(pre);
    var s = global.getSelection();
    s.removeAllRanges();
    s.addRange(r);
    var ok = false;
    try { ok = doc.execCommand("copy"); } catch (e) { ok = false; }
    if (ok) { s.removeAllRanges(); copied(btn); } else btn.textContent = "Выделено: Ctrl+C";
  }
  function initClicks() {
    doc.addEventListener("click", function (e) {
      var fix = e.target.closest("[data-fix]");
      if (fix) {
        var tb = fix.closest("tbody");
        var open = !tb.classList.contains("open");
        tb.classList.toggle("open", open);
        fix.setAttribute("aria-expanded", String(open));
        return;
      }
      var cp = e.target.closest("[data-copy]");
      if (cp) {
        var pre = one("pre", cp.closest(".snip"));
        if (!pre) return;
        var text = pre.textContent;
        if (global.navigator.clipboard && global.isSecureContext) {
          global.navigator.clipboard.writeText(text).then(function () { copied(cp); }, function () { copyFallback(pre, cp); });
        } else copyFallback(pre, cp);
      }
    });
  }

  function init() {
    initFilters();
    initSelection();
    initClicks();
  }
  global.ProblemsPage = { init: init, applySearch: applySearch, version: "1" };
  init();
})(window);
