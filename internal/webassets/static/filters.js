/* ReviewFilters: the shared filter bar of the bot's pages
   (docs/usage-and-diagnostics.md, «Панель фильтров»).
   Vanilla JS, no dependencies, no network. CSP-safe: no inline style
   attributes and no inline handlers; dynamic sizes and colours go through
   element.style.setProperty on custom properties (CSSOM). Styles:
   internal/webui/filters.css (inlined into every page).

   Two ways to use it:
     ReviewFilters.mount(root, { fields: [...], values, onChange })   - fields from JS
     ReviewFilters.enhance(root, { onChange })                         - fields from the
       native <select>/<input> inside root; they stay in the DOM (hidden) and keep the
       value, so a GET form submits them as before. Inside a GET form a change submits
       the form (data-submit="false" turns it off). Pages without their own JS get it
       automatically: every [data-review-filters] is enhanced on DOMContentLoaded.

   Strings from options are inserted with textContent only. */
(function (global) {
  "use strict";

  var uid = 0;
  var SVGNS = "http://www.w3.org/2000/svg";
  var PATHS = {
    x: "M4.5 4.5l7 7M11.5 4.5l-7 7",
    caret: "M4.5 6.5l3.5 3.5 3.5-3.5",
    check: "M3.5 8.5l3 3 6-7",
    search: "M7 12A5 5 0 1 0 7 2a5 5 0 0 0 0 10zM10.6 10.6L14 14",
    filter: "M2.5 4.5h11M4.5 8h7M6.5 11.5h3"
  };
  var DATE_PRESETS = [
    { value: "24h", label: "24 часа" }, { value: "7d", label: "7 дней" },
    { value: "30d", label: "30 дней" }, { value: "90d", label: "90 дней" }
  ];
  var TOKEN = /^[a-z][a-z0-9-]*$/i;

  function el(tag, cls, attrs) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (attrs) for (var k in attrs) {
      if (attrs[k] == null) continue;
      if (k === "text") e.textContent = attrs[k]; else e.setAttribute(k, attrs[k]);
    }
    return e;
  }
  function icon(name) {
    var s = document.createElementNS(SVGNS, "svg");
    s.setAttribute("viewBox", "0 0 16 16");
    s.setAttribute("aria-hidden", "true");
    s.setAttribute("focusable", "false");
    s.setAttribute("class", "rf-ico rf-ico--" + name);
    var p = document.createElementNS(SVGNS, "path");
    p.setAttribute("d", PATHS[name]);
    s.appendChild(p);
    return s;
  }
  /* swatch: a colour token of the page ("s-in" -> var(--s-in)), set through CSSOM. */
  function swatch(token) {
    var s = el("span", "rf-swatch", { "aria-hidden": "true" });
    if (token && TOKEN.test(token)) s.style.setProperty("--rf-sw", "var(--" + token + ")");
    return s;
  }
  function isEmpty(v) { return v == null || v === "" || (Array.isArray(v) && v.length === 0); }
  function clone(o) { var r = {}; for (var k in o) r[k] = Array.isArray(o[k]) ? o[k].slice() : o[k]; return r; }
  function same(a, b) { return JSON.stringify(a) === JSON.stringify(b); }
  function plural(n, one, few, many) {
    var m10 = n % 10, m100 = n % 100;
    if (m10 === 1 && m100 !== 11) return one;
    if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
    return many;
  }
  function cap(s) { return s ? s.charAt(0).toUpperCase() + s.slice(1) : "Все"; }
  function emptyOf(f) { return f.type === "multi" ? [] : ""; }
  /* The value "×" and "Сбросить всё" return to: field.default, or empty. */
  function defOf(f) { return f.default != null ? normValue(f, f.default) : emptyOf(f); }
  function normValue(f, v) {
    if (f.type === "multi") {
      if (v == null || v === "") return [];
      return (Array.isArray(v) ? v : String(v).split(",")).map(String).filter(Boolean);
    }
    if (f.type === "toggle") return (v === true || v === "1" || v === "true" || v === 1) ? "1" : "";
    return v == null ? "" : String(v);
  }
  function fmtDate(s) {
    var m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s || "");
    return m ? m[3] + "." + m[2] + "." + m[1].slice(2) : s;
  }
  function normOption(x) {
    if (typeof x !== "object" || x === null) return { value: String(x), label: String(x) };
    var o = {};
    for (var k in x) o[k] = x[k];
    o.value = String(o.value == null ? "" : o.value);
    o.label = o.label == null ? o.value : String(o.label);
    return o;
  }
  function normField(f) {
    var o = {};
    for (var k in f) o[k] = f[k];
    o.type = o.type || "select";
    o.label = o.label || o.key;
    o.options = (o.options || []).map(normOption);
    if (o.type === "date-range" && !o.options.length) o.options = DATE_PRESETS.slice();
    if (o.placeholder == null && o.allLabel) o.placeholder = o.allLabel;
    if (o.placeholder == null) o.placeholder = o.type === "text" ? "" : o.type === "date-range" ? "всё время" : "все";
    return o;
  }

  /* ---------- native controls (enhance) ---------- */
  function nativeControls(root) {
    var out = [], list = root.querySelectorAll("select, input");
    for (var i = 0; i < list.length; i++) {
      var n = list[i];
      if (n.hasAttribute("data-rf-skip") || n.type === "hidden" || n.type === "submit" || n.type === "button") continue;
      if (!(n.dataset.key || n.name || n.id)) continue;
      out.push(n);
    }
    return out;
  }
  /* The text of the <label> around a control, without the control's own text. */
  function wrapLabel(n) {
    var lab = n.closest("label");
    if (!lab) return "";
    var t = "";
    for (var c = lab.firstChild; c; c = c.nextSibling) if (c.nodeType === 3) t += c.nodeValue;
    return t.trim();
  }
  function readOptions(sel) {
    var opts = [], all = null;
    for (var j = 0; j < sel.options.length; j++) {
      var op = sel.options[j], d = op.dataset;
      if (op.value === "" && !sel.multiple) { all = op.textContent.trim(); continue; }
      var o = { value: op.value, label: op.textContent.trim() };
      var og = op.parentNode && op.parentNode.tagName === "OPTGROUP" ? op.parentNode : null;
      if (d.group) o.group = d.group; else if (og) o.group = og.label;
      if (d.shortLabel) o.shortLabel = d.shortLabel;
      if (d.swatch) o.swatch = d.swatch;
      if (d.count != null && d.count !== "") o.count = +d.count;
      opts.push(o);
    }
    return { options: opts, all: all };
  }
  function readNative(n) {
    if (n.tagName === "SELECT") {
      if (n.multiple) {
        var v = [];
        for (var i = 0; i < n.options.length; i++) if (n.options[i].selected) v.push(n.options[i].value);
        return v;
      }
      return n.value;
    }
    if (n.type === "checkbox") return n.checked ? "1" : "";
    return n.value;
  }
  function writeNative(n, v) {
    if (n.tagName === "SELECT") {
      if (n.multiple) for (var i = 0; i < n.options.length; i++) n.options[i].selected = v.indexOf(n.options[i].value) >= 0;
      else n.value = v;
    } else if (n.type === "checkbox") n.checked = v === "1";
    else n.value = v;
  }
  function fieldFromNative(n) {
    var d = n.dataset, f = { key: d.key || n.name || n.id, label: d.label || n.getAttribute("aria-label") || wrapLabel(n) };
    if (n.tagName === "SELECT") {
      var r = readOptions(n);
      f.type = d.type || (n.multiple ? "multi" : "select");
      f.options = r.options;
      if (d.allLabel != null) f.allLabel = d.allLabel; else if (r.all != null) f.allLabel = r.all;
      /* No empty <option>: the list offers no "все" (the select cannot hold "");
         without a default "×" would have nothing to return to either. */
      if (r.all == null && !n.multiple) { f.noEmpty = true; if (d.default == null) f.clearable = false; }
    } else if (n.type === "checkbox") {
      f.type = "toggle";
    } else {
      f.type = d.type || "text";
      if (n.placeholder) f.placeholder = n.placeholder;
      if (n.inputMode) f.inputmode = n.inputMode;
      var w = +(d.width || n.getAttribute("size") || 0);
      if (w) f.width = w;
    }
    if (d.prefix) f.prefix = d.prefix;
    if (d.default != null) f.default = d.default;
    if (d.clearable === "false") f.clearable = false;
    if (d.search === "true") f.search = true;
    if (d.placeholder) f.placeholder = d.placeholder;
    if (d.debounce) f.debounce = +d.debounce;
    f.value = readNative(n);
    return f;
  }

  /* ---------- the bar ---------- */
  function build(root, opts, natives) {
    var ds = root.dataset || {};
    var rawFields = natives ? natives.map(fieldFromNative) : (opts.fields || []);
    var fields = rawFields.map(normField);
    var byKey = {}, nativeByKey = {};
    fields.forEach(function (f, i) { byKey[f.key] = f; if (natives) nativeByKey[f.key] = natives[i]; });
    var collapse = opts.collapse != null ? opts.collapse !== false : ds.collapse !== "false";
    var listeners = { change: [] };
    if (opts.onChange) listeners.change.push(opts.onChange);

    var start = opts.values || {};
    var values = {};
    fields.forEach(function (f, i) {
      var v = start[f.key] !== undefined ? start[f.key] : rawFields[i].value;
      values[f.key] = v === undefined ? defOf(f) : normValue(f, v);
    });

    /* enhance: keep the native controls (and fallback buttons) in a hidden holder */
    var holder = null;
    if (natives) {
      holder = el("div", "rf-native", { hidden: "" });
      while (root.firstChild) holder.appendChild(root.firstChild);
    } else root.textContent = "";
    root.classList.add("rf");
    root.classList.toggle("rf--fixed", !collapse);
    if (!root.getAttribute("role") && root.tagName !== "FORM") root.setAttribute("role", "search");
    if (!root.getAttribute("aria-label")) root.setAttribute("aria-label", opts.ariaLabel || "Фильтры");

    var id = "rf" + (++uid);
    var toggle = null, toggleCount = null;
    if (collapse) {
      toggle = el("button", "rf-toggle", { type: "button", "aria-expanded": "false", "aria-controls": id + "-fields" });
      toggle.appendChild(icon("filter"));
      toggle.appendChild(el("span", null, { text: "Фильтры" }));
      toggleCount = el("span", "rf-badge");
      toggle.appendChild(toggleCount);
      toggle.addEventListener("click", function () {
        var on = toggle.getAttribute("aria-expanded") !== "true";
        toggle.setAttribute("aria-expanded", String(on));
      });
      root.appendChild(toggle);
    }
    var fieldsWrap = el("div", "rf-fields", { id: id + "-fields" });
    var tail = el("div", "rf-tail");
    var countEl = el("span", "rf-count", { "aria-live": "polite" });
    var resetAll = el("button", "rf-reset-all", { type: "button", text: "Сбросить всё" });
    tail.appendChild(countEl);
    tail.appendChild(resetAll);
    root.appendChild(fieldsWrap);
    root.appendChild(tail);
    if (holder) root.appendChild(holder);

    resetAll.addEventListener("click", function () {
      api.reset();
      if (toggle && toggle.offsetParent !== null) toggle.focus(); else if (ctls[0]) ctls[0].focus();
    });

    var openCtl = null;
    function onDocDown(e) { if (openCtl && !openCtl.wrap.contains(e.target)) openCtl.close(false); }
    document.addEventListener("mousedown", onDocDown, true);
    document.addEventListener("focusin", onDocDown, true);

    var form = natives && natives.length ? natives[0].form : null;
    var submit = !!form && (form.getAttribute("method") || "get").toLowerCase() === "get" &&
      (opts.submit != null ? opts.submit !== false : ds.submit !== "false");

    function emit() {
      var vals = clone(values);
      listeners.change.forEach(function (fn) { try { fn(clone(vals)); } catch (err) { setTimeout(function () { throw err; }); } });
      root.dispatchEvent(new CustomEvent("rf:change", { detail: vals, bubbles: true }));
      if (submit) { if (form.requestSubmit) form.requestSubmit(); else form.submit(); }
    }
    function toNative(key, fire) {
      var n = nativeByKey[key];
      if (!n) return;
      writeNative(n, values[key]);
      if (fire) n.dispatchEvent(new Event("change", { bubbles: true }));
    }
    /* set one field from the UI: native control, render, one rf:change */
    function commit(key, value) {
      var v = normValue(byKey[key], value);
      if (same(v, values[key])) return;
      values[key] = v;
      toNative(key, true);
      ctlByKey[key].render();
      updateTail();
      emit();
    }
    function activeCount() {
      var n = 0;
      fields.forEach(function (f) { if (f.clearable !== false && !same(values[f.key], defOf(f))) n++; });
      return n;
    }
    function updateTail() {
      var n = activeCount();
      countEl.textContent = n ? n + " " + plural(n, "фильтр", "фильтра", "фильтров") : "";
      tail.hidden = n === 0;
      if (toggleCount) { toggleCount.textContent = n ? String(n) : ""; toggleCount.hidden = n === 0; }
      root.classList.toggle("has-active", n > 0);
    }

    /* ---------- field shell ---------- */
    function shell(f) {
      var wrap = el("div", "rf-field rf-field--" + f.type + (f.clearable === false ? " rf-field--noclear" : ""), { "data-key": f.key });
      var clear = el("button", "rf-clear", { type: "button", "aria-label": "Сбросить: " + f.label, title: "Сбросить" });
      clear.appendChild(icon("x"));
      var ctl = { f: f, wrap: wrap, clear: clear };
      clear.addEventListener("click", function () { commit(f.key, defOf(f)); ctl.focus(); });
      return ctl;
    }
    function setState(ctl, on) {
      if (ctl.f.clearable === false) on = false; /* pickers: no ×, no highlight */
      ctl.wrap.classList.toggle("is-set", on);
      ctl.clear.hidden = !on;
    }

    /* ---------- text ---------- */
    function textField(f) {
      var ctl = shell(f);
      var lab = el("label", "rf-trigger rf-trigger--text");
      lab.appendChild(el("span", "rf-label", { text: f.label }));
      if (f.prefix) lab.appendChild(el("span", "rf-prefix", { text: f.prefix, "aria-hidden": "true" }));
      var input = el("input", "rf-input", {
        type: "text", autocomplete: "off", spellcheck: "false",
        placeholder: f.placeholder || null, inputmode: f.inputmode || null
      });
      input.style.setProperty("--rf-w", (f.width || Math.max(8, (f.placeholder || "").length + 2)) + "ch");
      lab.appendChild(input);
      ctl.wrap.appendChild(lab);
      ctl.wrap.appendChild(ctl.clear);
      var timer = null;
      function clean(v) {
        v = v.trim();
        if (f.prefix && v.indexOf(f.prefix) === 0) v = v.slice(f.prefix.length);
        return v;
      }
      function flush() { clearTimeout(timer); timer = null; commit(f.key, clean(input.value)); }
      input.addEventListener("input", function () {
        setState(ctl, clean(input.value) !== defOf(f));
        clearTimeout(timer);
        timer = setTimeout(flush, f.debounce != null ? f.debounce : 300);
      });
      input.addEventListener("keydown", function (e) {
        if (e.key === "Enter") { e.preventDefault(); flush(); }
        else if (e.key === "Escape" && input.value) { e.preventDefault(); input.value = ""; flush(); }
      });
      input.addEventListener("blur", function () { if (timer) flush(); });
      ctl.render = function () {
        var v = values[f.key];
        if (clean(input.value) !== v) input.value = v;
        setState(ctl, v !== defOf(f));
      };
      ctl.focus = function () { input.focus(); };
      ctl.close = function () {};
      return ctl;
    }

    /* ---------- toggle ---------- */
    function toggleField(f) {
      var ctl = shell(f);
      var btn = el("button", "rf-trigger rf-trigger--toggle", { type: "button", "aria-pressed": "false" });
      btn.appendChild(el("span", "rf-switch", { "aria-hidden": "true" }));
      btn.appendChild(el("span", "rf-value", { text: f.label }));
      btn.addEventListener("click", function () { commit(f.key, values[f.key] ? "" : "1"); });
      ctl.wrap.appendChild(btn);
      ctl.wrap.appendChild(ctl.clear);
      ctl.render = function () {
        btn.setAttribute("aria-pressed", String(values[f.key] === "1"));
        setState(ctl, values[f.key] !== defOf(f));
      };
      ctl.focus = function () { btn.focus(); };
      ctl.close = function () {};
      return ctl;
    }

    /* ---------- select / search-select / multi / date-range ---------- */
    function listField(f) {
      var ctl = shell(f);
      var multi = f.type === "multi";
      var isDate = f.type === "date-range";
      var searchable = function () { return f.type === "search-select" || f.search === true || (multi && f.search !== false && f.options.length > 7); };
      var listId = id + "-" + f.key + "-list";
      var trig = el("button", "rf-trigger", { type: "button", "aria-haspopup": "listbox", "aria-expanded": "false", "aria-controls": listId });
      var valSw = el("span", "rf-swatch", { "aria-hidden": "true", hidden: "" });
      var valEl = el("span", "rf-value");
      trig.appendChild(el("span", "rf-label", { text: f.label }));
      trig.appendChild(valSw);
      trig.appendChild(valEl);
      trig.appendChild(icon("caret"));
      ctl.wrap.appendChild(trig);
      ctl.wrap.appendChild(ctl.clear);

      var pop = el("div", "rf-pop", { hidden: "" });
      var sw = el("div", "rf-search");
      sw.appendChild(icon("search"));
      var search = el("input", "rf-search-input", {
        type: "text", role: "combobox", "aria-expanded": "true", "aria-controls": listId,
        "aria-autocomplete": "list", "aria-label": "Поиск: " + f.label,
        placeholder: f.searchPlaceholder || "Поиск…", autocomplete: "off", spellcheck: "false"
      });
      sw.appendChild(search);
      pop.appendChild(sw);
      var list = el("ul", "rf-list", { id: listId, role: "listbox", tabindex: "-1", "aria-label": f.label });
      if (multi) list.setAttribute("aria-multiselectable", "true");
      pop.appendChild(list);
      var empty = el("div", "rf-empty", { text: "Ничего не найдено", hidden: "" });
      pop.appendChild(empty);

      var fromIn, toIn;
      if (isDate) {
        var custom = el("div", "rf-range");
        custom.appendChild(el("div", "rf-range-title", { text: "Свой период" }));
        var row = el("div", "rf-range-row");
        fromIn = el("input", "rf-date", { type: "date", "aria-label": "С" });
        toIn = el("input", "rf-date", { type: "date", "aria-label": "По" });
        row.appendChild(fromIn);
        row.appendChild(el("span", "rf-range-sep", { text: "–", "aria-hidden": "true" }));
        row.appendChild(toIn);
        custom.appendChild(row);
        var ok = el("button", "rf-range-ok", { type: "button", text: "Готово" });
        var applyRange = function () {
          var a = fromIn.value, b = toIn.value;
          if (a && b && a > b) { var t = a; a = b; b = t; }
          close(true);
          commit(f.key, a || b ? a + ".." + b : "");
        };
        ok.addEventListener("click", applyRange);
        /* Enter in a date input: apply the range (inside a form it would submit it first) */
        [fromIn, toIn].forEach(function (inp) {
          inp.addEventListener("keydown", function (e) { if (e.key === "Enter") { e.preventDefault(); applyRange(); } });
        });
        custom.appendChild(ok);
        pop.appendChild(custom);
      }
      ctl.wrap.appendChild(pop);

      var shown = [], optEls = [], active = -1, q = "", hasSearch = false;
      function optsAll() {
        var o = f.options.slice();
        if (!multi && !q && !f.noEmpty && !(f.clearable === false && !f.allLabel)) o.unshift({ value: "", label: cap(f.allLabel || f.placeholder), _all: true });
        return o;
      }
      function isSel(v) {
        var cur = values[f.key];
        return multi ? cur.indexOf(v) >= 0 : cur === v;
      }
      function find(x) { for (var i = 0; i < f.options.length; i++) if (f.options[i].value === x) return f.options[i]; return null; }
      function renderList() {
        var needle = q.trim().toLowerCase();
        shown = optsAll().filter(function (o) {
          return !needle || o.label.toLowerCase().indexOf(needle) >= 0 || o.value.toLowerCase().indexOf(needle) >= 0 ||
            (o.group || "").toLowerCase().indexOf(needle) >= 0;
        });
        /* options of one group together, groups in order of first appearance */
        var gOrder = {}, gi = 0;
        shown.forEach(function (o) { var g = o.group || ""; if (!(g in gOrder)) gOrder[g] = o._all ? -1 : gi++; });
        shown = shown.map(function (o, i) { return [o, i]; }).sort(function (a, b) {
          return (gOrder[a[0].group || ""] - gOrder[b[0].group || ""]) || (a[1] - b[1]);
        }).map(function (x) { return x[0]; });
        list.textContent = "";
        optEls = [];
        var lastGroup = null;
        shown.forEach(function (o, i) {
          var g = o.group || null;
          if (g && g !== lastGroup) list.appendChild(el("li", "rf-group", { role: "presentation", text: g }));
          lastGroup = g;
          var li = el("li", "rf-opt" + (o._all ? " rf-opt--all" : ""), { role: "option", id: listId + "-" + i, "aria-selected": String(isSel(o.value)) });
          var ck = el("span", multi ? "rf-check rf-check--box" : "rf-check");
          ck.appendChild(icon("check"));
          li.appendChild(ck);
          if (o.swatch) li.appendChild(swatch(o.swatch));
          li.appendChild(el("span", "rf-opt-label", { text: o.label, title: o.label }));
          if (o.count != null) li.appendChild(el("span", "rf-opt-count", { text: String(o.count) }));
          li.addEventListener("mousedown", function (e) { e.preventDefault(); });
          li.addEventListener("click", function () { active = i; choose(o); });
          li.addEventListener("mousemove", function () { if (active !== i) { active = i; paintActive(); } });
          list.appendChild(li);
          optEls.push(li);
        });
        empty.hidden = shown.length > 0;
        if (active >= shown.length) active = shown.length - 1;
        paintActive();
      }
      function paintActive() {
        for (var i = 0; i < optEls.length; i++) optEls[i].classList.toggle("is-active", i === active);
        var host = hasSearch ? search : list;
        if (active >= 0 && optEls[active]) {
          host.setAttribute("aria-activedescendant", optEls[active].id);
          var it = optEls[active], prev = it.previousElementSibling;
          var top = prev && prev.classList.contains("rf-group") ? prev.offsetTop : it.offsetTop;
          if (top < list.scrollTop) list.scrollTop = top;
          else if (it.offsetTop + it.offsetHeight > list.scrollTop + list.clientHeight) list.scrollTop = it.offsetTop + it.offsetHeight - list.clientHeight;
        } else host.removeAttribute("aria-activedescendant");
      }
      function choose(o) {
        if (multi) {
          var cur = values[f.key].slice(), at = cur.indexOf(o.value);
          if (at >= 0) cur.splice(at, 1); else cur.push(o.value);
          commit(f.key, cur);
          renderList();
        } else {
          close(true);
          commit(f.key, o.value);
        }
      }
      function open() {
        if (openCtl && openCtl !== ctl) openCtl.close(false);
        openCtl = ctl;
        q = "";
        hasSearch = searchable();
        sw.hidden = !hasSearch;
        search.value = "";
        pop.hidden = false;
        ctl.wrap.classList.add("is-open");
        trig.setAttribute("aria-expanded", "true");
        if (isDate) {
          var m = /^(.*)\.\.(.*)$/.exec(values[f.key] || "");
          fromIn.value = m ? m[1] : ""; toIn.value = m ? m[2] : "";
        }
        renderList();
        active = 0;
        for (var i = 0; i < shown.length; i++) if (isSel(shown[i].value) && !(multi && shown[i]._all)) { active = i; break; }
        paintActive();
        pop.classList.remove("rf-pop--end");
        var r = pop.getBoundingClientRect();
        if (r.right > document.documentElement.clientWidth - 8) pop.classList.add("rf-pop--end");
        (hasSearch ? search : list).focus({ preventScroll: true });
      }
      function close(refocus) {
        if (pop.hidden) return;
        pop.hidden = true;
        ctl.wrap.classList.remove("is-open");
        trig.setAttribute("aria-expanded", "false");
        if (openCtl === ctl) openCtl = null;
        if (refocus) trig.focus();
      }
      var typed = "", typedAt = 0;
      function onKey(e) {
        var k = e.key;
        if (k === "ArrowDown") { e.preventDefault(); active = Math.min(shown.length - 1, active + 1); paintActive(); }
        else if (k === "ArrowUp") { e.preventDefault(); active = Math.max(0, active - 1); paintActive(); }
        else if (k === "Home" && !hasSearch) { e.preventDefault(); active = 0; paintActive(); }
        else if (k === "End" && !hasSearch) { e.preventDefault(); active = shown.length - 1; paintActive(); }
        else if (k === "Enter" || (k === " " && !hasSearch)) { e.preventDefault(); if (shown[active]) choose(shown[active]); }
        else if (k === "Escape") { e.preventDefault(); e.stopPropagation(); close(true); }
        else if (k === "Tab") close(false);
        else if (!hasSearch && k.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) {
          var now = Date.now();
          typed = (now - typedAt > 700 ? "" : typed) + k.toLowerCase();
          typedAt = now;
          for (var i = 0; i < shown.length; i++) {
            if (shown[i].label.toLowerCase().indexOf(typed) === 0) { active = i; paintActive(); break; }
          }
        }
      }
      list.addEventListener("keydown", onKey);
      search.addEventListener("keydown", onKey);
      search.addEventListener("input", function () { q = search.value; active = 0; renderList(); });
      trig.addEventListener("click", function () { if (pop.hidden) open(); else close(true); });
      trig.addEventListener("keydown", function (e) {
        if ((e.key === "ArrowDown" || e.key === "ArrowUp") && pop.hidden) { e.preventDefault(); open(); }
      });
      pop.addEventListener("keydown", function (e) { if (e.key === "Escape") { e.preventDefault(); close(true); } });

      /* the text in the field: shortLabel when the option has one */
      function display() {
        var v = values[f.key];
        if (multi) {
          if (!v.length) return null;
          if (v.length === 1) { var o1 = find(v[0]); return o1 ? (o1.shortLabel || o1.label) : v[0]; }
          return v.length + " выбрано";
        }
        if (v === "") return null;
        var o = find(v);
        if (o) return o.shortLabel || o.label;
        if (isDate && v.indexOf("..") >= 0) {
          var p = v.split("..");
          if (p[0] && p[1]) return fmtDate(p[0]) + " – " + fmtDate(p[1]);
          return p[0] ? "с " + fmtDate(p[0]) : "по " + fmtDate(p[1]);
        }
        return v;
      }
      ctl.render = function () {
        var d = display(), v = values[f.key];
        valEl.textContent = d == null ? f.placeholder : d;
        valEl.classList.toggle("is-placeholder", d == null);
        var one = multi ? (v.length === 1 ? find(v[0]) : null) : find(v);
        var tok = one && one.swatch && TOKEN.test(one.swatch) ? one.swatch : "";
        valSw.hidden = !tok;
        if (tok) valSw.style.setProperty("--rf-sw", "var(--" + tok + ")"); else valSw.style.removeProperty("--rf-sw");
        /* the long text as a tooltip when the field shows a shorter one */
        var full = multi && v.length > 1 ? v.map(function (x) { var o2 = find(x); return o2 ? o2.label : x; }).join(", ")
          : one && one.shortLabel ? one.label : "";
        if (full) trig.title = full; else trig.removeAttribute("title");
        setState(ctl, !same(v, defOf(f)));
        if (!pop.hidden) renderList();
      };
      ctl.focus = function () { trig.focus(); };
      ctl.close = close;
      ctl.open = open;
      return ctl;
    }

    var ctls = [], ctlByKey = {};
    fields.forEach(function (f) {
      var ctl = f.type === "text" ? textField(f) : f.type === "toggle" ? toggleField(f) : listField(f);
      ctl.render();
      ctls.push(ctl);
      ctlByKey[f.key] = ctl;
      fieldsWrap.appendChild(ctl.wrap);
    });
    updateTail();

    function renderAll() { ctls.forEach(function (c) { c.render(); }); updateTail(); }
    var api = {
      fields: fields,
      root: root,
      /* current values: string per field, array for multi */
      get: function () { return clone(values); },
      /* set values (merged by key); no change event unless {emit:true} */
      set: function (vals, o) {
        vals = vals || {};
        var changed = false;
        for (var k in vals) if (byKey[k]) {
          var v = normValue(byKey[k], vals[k]);
          if (!same(v, values[k])) { values[k] = v; changed = true; }
          toNative(k, false);
        }
        renderAll();
        if (changed && o && o.emit) emit();
        return api;
      },
      /* every clearable field back to its default; one change event */
      reset: function (o) {
        var changed = false;
        fields.forEach(function (f) {
          if (f.clearable === false || same(values[f.key], defOf(f))) return;
          values[f.key] = defOf(f);
          toNative(f.key, true);
          changed = true;
        });
        renderAll();
        if (changed && !(o && o.silent)) emit();
        return api;
      },
      on: function (name, fn) {
        (listeners[name] = listeners[name] || []).push(fn);
        return function () { var a = listeners[name], i = a.indexOf(fn); if (i >= 0) a.splice(i, 1); };
      },
      /* new options: refresh({key: options}); enhanced bars: refresh() re-reads
         the options and values of the native controls */
      refresh: function (optionsByKey) {
        if (optionsByKey) {
          for (var k in optionsByKey) if (byKey[k]) byKey[k].options = (optionsByKey[k] || []).map(normOption);
        } else {
          for (var key in nativeByKey) {
            var n = nativeByKey[key], f = byKey[key];
            if (n.tagName === "SELECT") f.options = readOptions(n).options;
            values[key] = normValue(f, readNative(n));
          }
        }
        renderAll();
        return api;
      },
      open: function (key) { var c = ctlByKey[key]; if (c && c.open) c.open(); return api; },
      destroy: function () {
        document.removeEventListener("mousedown", onDocDown, true);
        document.removeEventListener("focusin", onDocDown, true);
        root.textContent = "";
        if (holder) while (holder.firstChild) root.appendChild(holder.firstChild);
        root.classList.remove("rf", "rf--fixed", "has-active");
        delete root.__reviewFilters;
      }
    };
    root.__reviewFilters = api;
    return api;
  }

  function resolve(root) {
    if (typeof root === "string") root = document.querySelector(root);
    if (!root) throw new Error("ReviewFilters: root element not found");
    return root;
  }
  function mount(root, opts) {
    root = resolve(root);
    return root.__reviewFilters || build(root, opts || {}, null);
  }
  function enhance(root, opts) {
    root = resolve(root);
    if (root.__reviewFilters) {
      if (opts && opts.onChange) root.__reviewFilters.on("change", opts.onChange);
      return root.__reviewFilters;
    }
    return build(root, opts || {}, nativeControls(root));
  }

  /* ---------- URL helpers ---------- */
  function toQuery(values, fields, base) {
    var p = new URLSearchParams(base || "");
    (fields || Object.keys(values).map(function (k) { return { key: k }; })).forEach(function (f) {
      var k = f.param || f.key, v = values[f.key];
      if (v === undefined) return; /* not in values: leave the parameter as it is */
      p.delete(k);
      if (f.default != null) {
        if (same(v, defOf(f))) return;
      } else if (isEmpty(v)) return;
      p.set(k, Array.isArray(v) ? v.join(",") : String(v == null ? "" : v));
    });
    return p;
  }
  function fromQuery(fields, search) {
    var p = new URLSearchParams(search == null ? location.search : search);
    var out = {};
    fields.forEach(function (f) {
      var raw = p.get(f.param || f.key);
      if (raw != null) out[f.key] = f.type === "multi" ? raw.split(",").filter(Boolean) : raw;
    });
    return out;
  }
  /* Two-way sync of a bar with location.search (history.replaceState unless {push:true}). */
  function urlSync(inst, o) {
    o = o || {};
    inst.set(fromQuery(inst.fields, location.search));
    var off = inst.on("change", function (vals) {
      var s = toQuery(vals, inst.fields, location.search).toString();
      var url = location.pathname + (s ? "?" + s : "") + location.hash;
      if (o.push) history.pushState(null, "", url); else history.replaceState(null, "", url);
    });
    function onPop() { inst.reset({ silent: true }); inst.set(fromQuery(inst.fields, location.search), { emit: true }); }
    window.addEventListener("popstate", onPop);
    return function () { off(); window.removeEventListener("popstate", onPop); };
  }

  function autoEnhance(scope) {
    var nodes = (scope || document).querySelectorAll("[data-review-filters]"), out = [];
    for (var i = 0; i < nodes.length; i++) out.push(enhance(nodes[i]));
    return out;
  }

  global.ReviewFilters = { mount: mount, enhance: enhance, autoEnhance: autoEnhance, urlSync: urlSync, toQuery: toQuery, fromQuery: fromQuery, version: "1" };

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", function () { autoEnhance(); });
  else autoEnhance();
})(window);
