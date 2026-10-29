/* RunDetail: the detail of one diagnostics run, shared by the page /runs/<id>
   and the side panel of /runs (docs/usage-and-diagnostics.md, «/runs»).
   Vanilla JS on top of common.js (D) and filters.js (D.filters); load it after
   both. Data: GET /runs/<id>/data.json (the run record), the raw transcripts
   GET /runs/<id>/claude-<n>.jsonl and, for «Этот MR» on the page,
   GET /runs/data.json?project=…&mr=….
   CSP-safe: every string from the data is inserted as text (textContent),
   links go through D.safeHref, sizes and positions go through CSSOM
   (element.style), never through style attributes. Styles: internal/diagweb/diag.css.

     var rd = RunDetail.create(rootEl, {
       layout: "page" | "drawer",
       mrRuns: function (job) { return rows | null },  // drawer: rows of the list of the same MR
       onPick: function (id) {},                        // a run of «Этот MR» was chosen
       onLoad: function (run, data) {}                  // after a run is rendered (title, crumbs)
     });
     rd.show(id, { anchor: "s22" | "claude-2" });     // anchor: open a step / a Claude session */
(function (global) {
  "use strict";
  var D = global.D;
  if (!D) return;
  var h = D.h, clear = D.clear, add = D.add;

  var SK = { queue: "очередь", gitlab: "gitlab", checkout: "checkout", git: "git", setup: "setup", exec: "exec", claude: "claude", limiter: "лимит", log: "лог", step: "шаг" };
  var PURPOSE = { review: "ревью", chat: "ответ в треде", verification: "проверка замечаний", learning: "обучение", other: "прочее", ci: "CI", memory: "память", map: "карта контрактов" };
  var CLASS = { timeout: "таймаут", canceled: "отменён", error: "ошибка" };
  var PREVIEW = 1500, TR_PAGE = 300;

  // ---- small helpers -------------------------------------------------------
  function purposeLabel(p) { return PURPOSE[p] || p || "claude"; }
  function off(ms) { return "+" + (ms < 1000 ? Math.round(ms) + " мс" : (ms / 1000).toFixed(ms < 10000 ? 2 : 1).replace(".", ",") + " с"); }
  function pn(n, a, b, c) { return D.full(n) + " " + D.plural(n, a, b, c); }
  function pretty(s) { try { return JSON.stringify(JSON.parse(s), null, 2); } catch (e) { return s; } }
  function tok(u) { u = u || {}; return (u.input || 0) + (u.output || 0) + (u.cache_read || 0) + (u.cache_write || 0); }
  function cacheShare(u) { u = u || {}; var all = (u.input || 0) + (u.cache_read || 0) + (u.cache_write || 0); return all ? Math.round((u.cache_read || 0) * 100 / all) + "%" : ""; }
  function errTail(e) { var p = String(e).split(": "); return p[p.length - 1]; }
  function runURL(id) { return "/runs/" + encodeURIComponent(id); }
  function permalink(id, anchor) { return location.origin + runURL(id) + (anchor ? "#" + anchor : ""); }

  var SVGNS = "http://www.w3.org/2000/svg";
  var ICONS = {
    ext: "M9 3h4v4M13 3 7.5 8.5M11 9.5V13H3V5h3.5",
    thread: "M2.5 3.5h11v7h-6l-3 2.5v-2.5h-2z",
    link: "M6.5 9.5l3-3M7 4.5l1-1a2.5 2.5 0 0 1 3.5 3.5l-1 1M9 11.5l-1 1A2.5 2.5 0 0 1 4.5 9l1-1",
    alert: "M8 2 14.5 13.5h-13zM8 6.5v3M8 11.6v.1",
    chev: "M6 3l5 5-5 5",
    dl: "M8 2.5v8M4.5 7 8 10.5 11.5 7M3 13.5h10",
    copy: "M5.5 5.5h7v7h-7zM3.5 10.5v-7h7"
  };
  function icon(name) {
    var s = document.createElementNS(SVGNS, "svg"), p = document.createElementNS(SVGNS, "path");
    s.setAttribute("viewBox", "0 0 16 16"); s.setAttribute("width", "14"); s.setAttribute("height", "14");
    s.setAttribute("aria-hidden", "true"); s.setAttribute("focusable", "false"); s.setAttribute("class", "ico ico-" + name);
    p.setAttribute("d", ICONS[name]); p.setAttribute("fill", "none"); p.setAttribute("stroke", "currentColor");
    p.setAttribute("stroke-width", "1.6"); p.setAttribute("stroke-linecap", "round"); p.setAttribute("stroke-linejoin", "round");
    s.appendChild(p);
    return s;
  }

  // ---- copy & toast (shared by the pages: RunDetail.copy / RunDetail.toast) --
  var toastEl = null, toastT = 0;
  function toast(msg) {
    if (!toastEl) { toastEl = h("div", { class: "rd-toast", role: "status", "aria-live": "polite", hidden: true }); document.body.appendChild(toastEl); }
    toastEl.textContent = msg; toastEl.hidden = false;
    clearTimeout(toastT); toastT = setTimeout(function () { toastEl.hidden = true; }, 1800);
  }
  function copy(text, msg) {
    function fallback() {
      var ta = h("textarea", { class: "rd-offscreen", "aria-hidden": "true" }); ta.value = text; document.body.appendChild(ta); ta.select();
      var ok = false; try { ok = document.execCommand("copy"); } catch (e) { /* no clipboard */ }
      ta.remove(); toast(ok ? (msg || "Скопировано") : "Не удалось скопировать");
    }
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(function () { toast(msg || "Скопировано"); }, fallback);
    else fallback();
  }
  function shaBtn(sha, n, title) {
    return h("button", { type: "button", class: "sha", title: title || "Скопировать " + sha, text: sha.slice(0, n || 12), on: { click: function (e) { e.stopPropagation(); copy(sha, "Скопировано: " + sha.slice(0, 12)); } } });
  }

  // ---- data ------------------------------------------------------------------
  var cache = {}, trCache = {};
  function getJSON(url) {
    return fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } }).then(function (r) {
      if (r.status === 404) throw new Error("Запуск не найден (возможно, он старше срока хранения).");
      if (!r.ok) return r.text().then(function (t) { throw new Error("HTTP " + r.status + (t ? ": " + t.trim() : "")); });
      return r.json();
    });
  }
  function loadRun(id, fresh) {
    if (!fresh && cache[id]) return Promise.resolve(cache[id]);
    return getJSON(runURL(id) + "/data.json").then(function (d) { var m = model(id, d); cache[id] = m; return m; });
  }
  function loadTranscript(id, c) {
    var key = id + "/" + c.n;
    if (trCache[key] != null) return Promise.resolve(trCache[key]);
    return fetch(transcriptURL(id, c), { credentials: "same-origin" }).then(function (r) {
      if (r.status === 404) return "";
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.text();
    }).then(function (t) { trCache[key] = t; return t; });
  }
  // The file name is claude-<n>.jsonl (diag.TranscriptName); the route serves only that.
  function transcriptURL(id, c) { return runURL(id) + "/claude-" + c.n + ".jsonl"; }

  function kindCoverage(steps, total) {
    var by = {};
    steps.forEach(function (s) { if (s.kind === "log" || !s.duration_ms) return; (by[s.kind] = by[s.kind] || []).push([s.off, s.end]); });
    var out = Object.keys(by).map(function (k) {
      var iv = by[k].sort(function (a, b) { return a[0] - b[0]; }), sum = 0, ca = -1, cb = -1;
      iv.forEach(function (x) { if (x[0] > cb) { if (cb > ca) sum += cb - ca; ca = x[0]; cb = x[1]; } else cb = Math.max(cb, x[1]); });
      if (cb > ca) sum += cb - ca;
      return { k: k, ms: sum, n: by[k].length, pc: total ? sum / total : 0 };
    });
    out.sort(function (a, b) { return b.ms - a.ms; });
    return out;
  }

  function model(id, d) {
    var r = d.run, j = r.job || {};
    var T0 = Date.parse(D.zero(j.enqueued_at) ? r.started_at : j.enqueued_at);
    var steps = (r.steps || []).map(function (s) {
      var o = Math.max(0, Date.parse(s.start) - T0);
      var x = {}; for (var k in s) x[k] = s[k];
      x.off = o; x.end = o + (s.duration_ms || 0);
      return x;
    });
    var lastEnd = steps.reduce(function (a, s) { return Math.max(a, s.end); }, 0);
    var Tend = D.zero(r.finished_at) ? lastEnd : Math.max(lastEnd, Date.parse(r.finished_at) - T0);
    var queue = steps.filter(function (s) { return s.kind === "queue"; })[0];
    var qEnd = Math.max(queue ? queue.end : 0, Date.parse(r.started_at) - T0);
    var cl = r.claude || [], tk = 0, cost = 0;
    cl.forEach(function (c) { c._tok = tok(c.usage); tk += c._tok; cost += c.cost_usd || 0; });
    var errs = steps.filter(function (s) { return s.status === "error" && s.kind !== "log"; });
    var warns = steps.filter(function (s) { return s.status === "warn" || s.status === "denied" || (s.kind === "log" && s.status === "error"); });
    var done = steps.filter(function (s) { return s.name === "INFO: review: done"; })[0];
    var m = {
      id: id, d: d, r: r, j: j, T0: T0, total: Tend, steps: steps, claude: cl, tok: tk, cost: cost, errs: errs, warns: warns,
      failed: r.status === "failed" || r.status === "interrupted", findings: done && done.fields ? done.fields.findings : null,
      queueMs: qEnd, runMs: r.duration_ms || Math.max(0, Tend - qEnd), sizes: d.transcript_sizes || []
    };
    m.kinds = kindCoverage(steps, Tend);
    m.mrLabel = (j.project_path ? D.repoName(j.project_path) : "—") + (j.mr_iid ? "!" + j.mr_iid : "");
    m.mrHref = D.safeHref(j.mr_url);
    m.threadHref = m.mrHref && j.note_id ? m.mrHref + "#note_" + j.note_id : null;
    m.traceHref = D.safeHref(d.trace_url);
    return m;
  }

  // «Что пошло не так»: facts of the record only, no guesses.
  function diagnose(m) {
    if (!m.failed) return null;
    var r = m.r, c = m.claude.filter(function (x) { return x.status === "error"; })[0];
    var cls = r.error_class || (c && c.error_class) || "";
    var title = r.status === "interrupted" ? "Задача прервана" : "Задача упала";
    var headline, hint = null;
    if (cls === "timeout" && c) {
      headline = "Claude («" + purposeLabel(c.purpose) + "») прерван по таймауту через " + D.dur(c.duration_ms);
      if (c.timeout) hint = "Таймаут этого запуска Claude — " + c.timeout + ".";
    } else if (c && /model/i.test(c.error || "")) {
      headline = "Claude не запустился: модель «" + (c.model || "?") + "» недоступна";
      hint = "CLI завершился" + (c.exit_code != null ? " с кодом " + c.exit_code : "") + " за " + D.dur(c.duration_ms) + ".";
    } else if (cls === "canceled" || /context canceled/.test(r.error || "")) {
      var first = m.errs[0];
      headline = "Контекст задачи отменён — дальше шаги падали с «context canceled»";
      hint = r.reason || (first ? "Первым отмену получил шаг «" + first.name + "»." : null);
    } else {
      headline = r.error || r.reason || "Ошибка без текста";
    }
    return { title: title, headline: headline, raw: r.error || "", hint: hint };
  }

  // ---- shared pieces -----------------------------------------------------------
  function longText(text, opts) {
    opts = opts || {};
    text = text == null ? "" : String(text);
    var pre = h("pre", { class: "box" }), bar = h("div", { class: "boxbar" }), wrap = h("div", { class: "boxwrap" });
    if (opts.title) bar.appendChild(h("span", { text: opts.title }));
    if (text.length > PREVIEW && !opts.full) {
      pre.textContent = text.slice(0, PREVIEW) + "…";
      var more = h("button", { type: "button", class: "linkbtn", text: "показать всё (" + D.bytes(text.length) + ")" });
      more.addEventListener("click", function () { pre.textContent = text; more.remove(); });
      bar.appendChild(more);
    } else pre.textContent = text;
    if (opts.copy !== false && text) bar.appendChild(h("button", { type: "button", class: "linkbtn", text: "копировать", on: { click: function () { copy(text); } } }));
    if (bar.firstChild) wrap.appendChild(bar);
    wrap.appendChild(pre);
    return wrap;
  }
  var LONG = { argv: 1, process_argv: 1 };
  function fmtVal(v) {
    if (typeof v === "string" && /^[0-9a-f]{40}$/.test(v)) return shaBtn(v);
    if (Array.isArray(v)) return v.join(" ");
    if (v && typeof v === "object") return JSON.stringify(v);
    return String(v);
  }
  function kvList(fields) {
    if (!fields) return null;
    var dl = h("dl", { class: "kv" }), n = 0;
    Object.keys(fields).forEach(function (k) {
      var v = fields[k];
      if (LONG[k] || v === "" || v === null || v === undefined || (Array.isArray(v) && !v.length)) return;
      dl.appendChild(h("dt", { text: k })); dl.appendChild(h("dd", null, fmtVal(v))); n++;
    });
    return n ? dl : null;
  }
  function kvPreview(fields) {
    if (!fields) return "";
    var out = [];
    Object.keys(fields).forEach(function (k) {
      var v = fields[k];
      if (LONG[k] || v === "" || v == null) return;
      if (Array.isArray(v)) v = v.join(" "); else if (typeof v === "object") v = JSON.stringify(v);
      out.push(k + ": " + v);
    });
    return out.join(" · ");
  }
  function stepExtra(s) {
    var f = s.fields || {}, out = [];
    if (f.argv) out.push(longText([].concat(f.argv).join(" "), { title: "командная строка" + (f.process_argv ? " (внутри контейнера)" : "") }));
    if (f.process_argv) out.push(longText([].concat(f.process_argv).join(" "), { title: f.argv ? "полная команда" : "командная строка" }));
    if (s.stdout) out.push(longText(s.stdout, { title: "stdout (хвост)" }));
    if (s.stderr) out.push(longText(s.stderr, { title: "stderr (хвост)" }));
    return out;
  }
  function sect(title, extra, content) {
    var s = h("div", { class: "sect" }, h("h3", null, [title, extra ? h("span", { class: "muted", text: extra }) : null]));
    add(s, content);
    return s;
  }
  function ticks(T, n) {
    var raw = T / n, mag = Math.pow(10, Math.floor(Math.log10(raw || 1)));
    var step = [1, 2, 5, 10].map(function (k) { return k * mag; }).filter(function (s) { return s >= raw; })[0] || raw || 1;
    var out = []; for (var t = 0; t <= T + 1 && out.length < 12; t += step) out.push(t);
    return out;
  }
  function pct(x) { return Math.max(0, Math.min(100, x)) + "%"; }
  function badgeOf(st) { return h("span", { class: "badge b-" + st, text: CLASS[st] || D.STATUS[st] || st }); }
  function tabsBar(tabs, cur, onPick) {
    var bar = h("div", { class: "tabs", role: "tablist" });
    tabs.forEach(function (t) {
      var b = h("button", { type: "button", role: "tab", "aria-selected": String(t.id === cur), "data-tab": t.id }, [t.t, t.n ? h("span", { class: "count", text: String(t.n) }) : null]);
      b.addEventListener("click", function () { D.each(bar.children, function (x) { x.setAttribute("aria-selected", String(x === b)); }); onPick(t.id); });
      bar.appendChild(b);
    });
    return bar;
  }

  // ---- the renderer --------------------------------------------------------------
  function create(root, opts) {
    opts = opts || {};
    var page = opts.layout === "page";
    var seq = 0, cur = null, st = null, bars = [];
    root.classList.add("rd", page ? "rd--page" : "rd--drawer");

    function mount(host, fields, values, onChange) {
      var b = D.filters.mount(host, { fields: fields, values: values, onChange: onChange, ariaLabel: "Фильтры" });
      if (b) bars.push(b);
      return b;
    }
    function dropBars(scope) {
      bars = bars.filter(function (b) { if (!scope || scope.contains(b.root)) { b.destroy(); return false; } return true; });
    }
    function freshState(id, keepTab) { return { run: id, tab: keepTab || "wf", kind: "", probs: false, log: false, q: "", sort: "n", dir: 1, open: {}, cl: {}, ctab: {} }; }

    function show(id, o) {
      o = o || {};
      var my = ++seq;
      if (!st || st.run !== id) st = freshState(id, st && st.tab);
      if (!cache[id]) { dropBars(); clear(root); root.appendChild(h("p", { class: "muted rd-loading", text: "Загрузка запуска…" })); }
      return loadRun(id, o.fresh).then(function (m) {
        if (my !== seq) return;
        cur = m;
        applyAnchor(o.anchor);
        render();
        if (opts.onLoad) opts.onLoad(m.r, m.d);
        if (o.anchor) scrollToAnchor(o.anchor);
      }, function (e) {
        if (my !== seq) return;
        dropBars(); clear(root);
        root.appendChild(h("div", { class: "banner", role: "alert" }, h("div", null, [h("h2", { text: "Не удалось загрузить запуск" }), h("p", { text: String(e.message || e) })])));
        if (opts.onError) opts.onError(e);
      });
    }
    function applyAnchor(a) {
      var mm;
      if (!a) return;
      if ((mm = /^s(\d+)$/.exec(a))) { st.tab = "wf"; st.open = {}; st.open[+mm[1]] = true; st.kind = ""; st.probs = false; st.q = ""; st.log = cur.steps.some(function (s) { return s.n === +mm[1] && s.kind === "log"; }); }
      else if ((mm = /^claude-(\d+)$/.exec(a))) { st.tab = "cl"; st.cl = {}; st.cl[+mm[1]] = true; }
    }
    function scrollToAnchor(a) {
      setTimeout(function () {
        var mm = /^s(\d+)$/.exec(a) || /^claude-(\d+)$/.exec(a);
        if (!mm) return;
        var el = document.getElementById(pid(a));
        if (el) el.scrollIntoView({ block: "center" });
      }, 0);
    }
    // Ids are prefixed in the drawer so they never clash with the list page.
    function pid(x) { return page ? x : "rd-" + x; }
    function goStep(n) {
      st.tab = "wf"; st.open = {}; st.open[n] = true; st.kind = ""; st.probs = false; st.q = "";
      st.log = cur.steps.some(function (s) { return s.n === n && s.kind === "log"; });
      render(); scrollToAnchor("s" + n);
    }
    function goClaude(n) { st.tab = "cl"; st.cl = {}; st.cl[n] = true; render(); scrollToAnchor("claude-" + n); }

    // ---- top: numbers, MR, «что пошло не так» ----
    function factsLine(m) {
      var r = m.r, j = m.j;
      function f2(v, l) { return h("span", null, [h("b", { text: v }), l ? " " + l : ""]); }
      return h("div", { class: "rd-facts" }, [
        f2(D.zero(r.finished_at) && r.status === "running" ? "идёт" : D.dur(m.runMs), "работа"),
        h("span", null, ["очередь ", h("b", { text: D.dur(m.queueMs) })]),
        h("span", null, ["Claude ", h("b", { text: String(m.claude.length) })]),
        m.tok ? f2(D.short(m.tok), "ток.") : null, m.cost ? f2(D.usd(m.cost)) : null,
        m.findings != null ? h("span", null, ["замечаний ", h("b", { text: String(m.findings) })]) : null,
        r.attempt > 1 ? h("span", null, ["попытка ", h("b", { text: String(r.attempt) })]) : null,
        h("span", { text: D.dateTime(r.started_at) }), j.username ? h("span", { text: "@" + j.username }) : null
      ]);
    }
    function mrLine(m) {
      var j = m.j;
      return h("p", { class: "rd-mrt" }, [m.mrHref ? h("a", { href: m.mrHref, target: "_blank", rel: "noopener noreferrer", text: m.mrLabel }) : h("b", { text: m.mrLabel }),
        j.mr_title ? " — " + j.mr_title : ""]);
    }
    function diagBox(m) {
      var dg = diagnose(m);
      if (!dg) return null;
      var d = h("section", { class: "diag", "aria-label": "Что пошло не так" }, [h("h2", null, [icon("alert"), "Что пошло не так"]),
        h("p", { class: "cause" }, [h("b", { text: dg.title + ": " }), dg.headline]), dg.hint ? h("p", { class: "meta2", text: dg.hint }) : null]);
      var pl = h("div", { class: "rd-probs" });
      var probs = m.errs.concat(m.warns.filter(function (s) { return s.kind !== "log"; })).sort(function (a, b) { return a.n - b.n; });
      probs.slice(0, 6).forEach(function (s) {
        pl.appendChild(h("button", { type: "button", title: "Открыть шаг #" + s.n, on: { click: function () { goStep(s.n); } } },
          [h("span", { class: "sdot " + s.status }), h("span", { class: "pn", text: "#" + s.n + " " + s.name }), h("span", { class: "x", text: off(s.off) })]));
      });
      if (probs.length > 6) pl.appendChild(h("span", { class: "muted", text: "и ещё " + (probs.length - 6) + " — фильтр «проблемы» в шагах" }));
      if (pl.firstChild) d.appendChild(pl);
      if (dg.raw) d.appendChild(longText(dg.raw + (m.r.error_class ? " [" + m.r.error_class + "]" : ""), { title: "текст ошибки" }));
      return d;
    }
    function pageHead(m) {
      var r = m.r, j = m.j;
      return h("div", { class: "rd-head" }, [h("span", { class: "badge lg b-" + r.status, text: D.STATUS[r.status] || r.status }),
        h("h1", null, [D.kindLabel(j.kind), j.mr_iid || j.project_path ? " · " : "", m.mrHref ? h("a", { href: m.mrHref, target: "_blank", rel: "noopener noreferrer", text: m.mrLabel }) : (j.project_path ? m.mrLabel : "")]),
        factsLine(m),
        h("div", { class: "acts" }, [
          m.mrHref ? h("a", { class: "btn sm", href: m.mrHref, target: "_blank", rel: "noopener noreferrer" }, [icon("ext"), "MR"]) : null,
          m.threadHref ? h("a", { class: "btn sm", href: m.threadHref, target: "_blank", rel: "noopener noreferrer" }, [icon("thread"), "Тред"]) : null,
          h("button", { type: "button", class: "btn sm icon", title: "Скопировать ссылку на запуск", "aria-label": "Скопировать ссылку на запуск", on: { click: function () { copy(permalink(m.id), "Ссылка на запуск скопирована"); } } }, icon("link"))
        ])]);
    }

    // ---- passport, kinds, Claude summary, MR history ----
    function passport(m) {
      var r = m.r, j = m.j, dl = h("dl", { class: "kv rd-pass" });
      [j.mr_title ? ["MR", j.mr_title] : null, ["Проект", (j.project_path || "—") + (j.project_id ? " (#" + j.project_id + ")" : "")],
        ["Автор MR / запуск", (j.mr_author ? "@" + j.mr_author : "—") + " / " + (j.username ? "@" + j.username : "автоматически")],
        j.question ? ["Команда", "«" + j.question + "»"] : null,
        ["Тип задачи", D.kindLabel(j.kind) + " (" + j.kind + ")"],
        ["Head", j.head_sha ? shaBtn(j.head_sha) : "—"],
        j.discussion_id ? ["Тред", [shaBtn(j.discussion_id, 10), j.note_id ? " · note " + j.note_id : ""]] : null,
        r.route ? ["Маршрут", ({ light: "лёгкий", heavy: "тяжёлый" }[r.route.route] || "обычный") + (r.route.model ? " · " + r.route.model : "")] : null,
        r.route && r.route.reason ? ["почему", r.route.reason] : null,
        D.zero(j.enqueued_at) ? null : ["В очереди с", D.dateTime(j.enqueued_at) + " (" + D.time(j.enqueued_at) + ")"],
        ["Начало → конец", D.time(r.started_at) + " → " + (D.zero(r.finished_at) ? "…" : D.time(r.finished_at))],
        r.status === "postponed" && !D.zero(r.retry_at) ? ["Повтор в", D.dateTime(r.retry_at)] : null,
        r.reason ? ["Причина", r.reason] : null,
        ["Попытка", String(r.attempt || 1)],
        r.trace_id ? ["Трейс", m.traceHref ? h("a", { href: m.traceHref, target: "_blank", rel: "noopener noreferrer", text: r.trace_id }) : r.trace_id] : null,
        r.truncated ? ["Обрезано", "длинные поля сокращены (diag.max_run_mb)"] : null,
        ["ID", h("button", { type: "button", class: "sha", text: m.id, title: "Скопировать ID", on: { click: function () { copy(m.id, "ID скопирован"); } } })]
      ].forEach(function (x) { if (!x) return; dl.appendChild(h("dt", { text: x[0] })); dl.appendChild(h("dd", null, x[1])); });
      return dl;
    }
    function kindsView(m) {
      var kd = h("div", { class: "rd-kinds" });
      m.kinds.forEach(function (k) {
        var bar = h("span", { class: "bar" }, h("i", { class: "k-" + k.k, css: { width: pct(Math.max(1, k.pc * 100)) } }));
        kd.appendChild(h("div", { class: "r", title: pn(k.n, "шаг", "шага", "шагов") + " · " + D.dur(k.ms) }, [h("span", null, [h("span", { class: "kdot k-" + k.k }), " " + (SK[k.k] || k.k)]), bar,
          h("b", { text: D.dur(k.ms) }), h("span", { class: "pc", text: Math.round(k.pc * 100) + "%" })]));
      });
      if (!kd.firstChild) kd.appendChild(h("span", { class: "muted", text: "Шагов с длительностью нет." }));
      return kd;
    }
    function claudeTable(m) {
      var t = h("table", { class: "rd-ct" }, h("thead", null, h("tr", null, [h("th", { text: "#" }), h("th", { text: "задача" }), h("th", { class: "n", text: "время" }), h("th", { class: "n", text: "ток." }), h("th", { class: "n", text: "$" })])));
      var tb = h("tbody");
      m.claude.forEach(function (c) {
        tb.appendChild(h("tr", { class: "click", tabindex: "0", on: { click: function () { goClaude(c.n); }, keydown: function (e) { if (e.key === "Enter") goClaude(c.n); } } }, [
          h("td", { text: String(c.n) }), h("td", null, [purposeLabel(c.purpose), c.status !== "ok" ? h("span", { class: "errline", text: " · " + (CLASS[c.error_class] || c.error_class || "ошибка") }) : null]),
          h("td", { class: "n", text: D.dur(c.duration_ms) }), h("td", { class: "n", text: D.short(c._tok) }), h("td", { class: "n", text: D.usd(c.cost_usd) })]));
      });
      t.appendChild(tb);
      t.appendChild(h("tfoot", null, h("tr", null, [h("td"), h("td", { text: "итого" }), h("td", { class: "n", text: D.dur(m.claude.reduce(function (a, c) { return a + (c.duration_ms || 0); }, 0)) }),
        h("td", { class: "n", text: D.short(m.tok) }), h("td", { class: "n", text: D.usd(m.cost) })])));
      return t;
    }
    var mrCache = {};
    function mrRows(m) {
      var j = m.j;
      if (!j.mr_iid || !j.project_path) return Promise.resolve([]);
      var own = opts.mrRuns ? opts.mrRuns(j) : null;
      if (own) return Promise.resolve(own);
      var key = j.project_path + "!" + j.mr_iid;
      if (mrCache[key]) return Promise.resolve(mrCache[key]);
      // The server's project filter is a substring match: keep the exact project.
      return getJSON("/runs/data.json?project=" + encodeURIComponent(j.project_path) + "&mr=" + j.mr_iid).then(function (d) {
        var rows = (d.runs || []).filter(function (x) { return x.project_path === j.project_path && x.mr_iid === j.mr_iid; });
        mrCache[key] = rows; return rows;
      });
    }
    function historyView(m, box) {
      clear(box);
      box.appendChild(h("p", { class: "muted", text: "Загрузка…" }));
      mrRows(m).then(function (rows) {
        clear(box);
        if (!rows.length) { box.appendChild(h("p", { class: "muted", text: "Других запусков этого MR за окно нет." })); return; }
        var list = h("div", { class: "hist" });
        rows.slice().sort(function (a, b) { return Date.parse(a.started_at) - Date.parse(b.started_at); }).forEach(function (x) {
          var here = x.id === m.id;
          var b = h(here || !opts.onPick ? "span" : "button", { type: here || !opts.onPick ? null : "button", class: "hi" + (here ? " here" : ""), "aria-current": here ? "true" : null,
            title: D.kindLabel(x.kind) + " · " + (D.STATUS[x.status] || x.status) + " · " + D.dateTime(x.started_at) + (x.error ? " · " + x.error.slice(0, 120) : x.reason ? " · " + x.reason.slice(0, 120) : "") }, [
            h("span", { class: "h1" }, [h("span", { class: "sdot " + x.status }), D.kindLabel(x.kind)]),
            h("span", { class: "h2", text: D.time(x.started_at) + " · " + D.dur(x.duration_ms) + (x.claude_runs ? " · " + x.claude_runs + " Cl" : "") })]);
          if (!here && opts.onPick) b.addEventListener("click", function () { opts.onPick(x.id); });
          if (!here && !opts.onPick) b = h("a", { class: "hi", href: runURL(x.id), title: b.title }, Array.prototype.slice.call(b.childNodes));
          list.appendChild(b);
        });
        box.appendChild(list);
      }, function (e) { clear(box); box.appendChild(h("p", { class: "fail", text: "Не удалось загрузить запуски MR: " + (e.message || e) })); });
      return box;
    }

    // ---- waterfall of steps ----
    function waterfall(m) {
      var box = h("div", { class: "rd-wf" }), bar = h("div", { class: "rd-bar" }), host = h("div", { class: "frow" }), cnt = h("span", { class: "muted rd-cnt" });
      var kc = {}; m.steps.forEach(function (s) { kc[s.kind] = (kc[s.kind] || 0) + 1; });
      bar.appendChild(host); bar.appendChild(cnt); box.appendChild(bar);
      var T = m.total || 1, tk = ticks(T, page ? 4 : 3);
      var tbl = h("table", { class: "rd-wft" });
      tbl.appendChild(h("colgroup", null, page ? [h("col", { class: "c-n" }), h("col", { class: "c-k" }), h("col"), h("col", { class: "c-t" }), h("col", { class: "c-d" }), h("col", { class: "c-g" })]
        : [h("col", { class: "c-n" }), h("col"), h("col", { class: "c-d" }), h("col", { class: "c-g" })]));
      var axis = h("div", { class: "rd-axis" });
      tk.forEach(function (t, i, arr) { axis.appendChild(h("span", { class: i === arr.length - 1 && i ? "last" : null, text: t === 0 ? "0" : D.dur(t), css: { left: pct(t * 100 / T) } })); });
      function sh(key, label, cls) {
        var on = st.sort === key;
        var b = h("button", { type: "button", "data-dir": on ? String(st.dir) : null, text: label + (on ? (st.dir > 0 ? " ↑" : " ↓") : "") });
        b.addEventListener("click", function () { if (st.sort === key) st.dir = -st.dir; else { st.sort = key; st.dir = key === "d" ? -1 : 1; } renderPanel(); });
        return h("th", { class: cls || null, "aria-sort": on ? (st.dir > 0 ? "ascending" : "descending") : null }, b);
      }
      tbl.appendChild(h("thead", null, h("tr", null, page
        ? [sh("n", "#"), sh("k", "тип", "k"), h("th", { text: "шаг" }), h("th", { class: "t n", text: "старт" }), sh("d", "длит.", "n"), h("th", { class: "gt" }, axis)]
        : [h("th", { text: "#" }), h("th", { text: "шаг" }), h("th", { class: "n", text: "длит." }), h("th", { class: "gt" }, axis)])));
      var tb = h("tbody"); tbl.appendChild(tb);
      box.appendChild(h("div", { class: "rd-wfscroll" }, tbl));
      var ncol = page ? 6 : 4;
      function drawRows() {
        clear(tb);
        var rows = m.steps.filter(function (s) {
          var prob = s.status === "error" || s.status === "warn" || s.status === "denied";
          if (s.kind === "log" && !st.log && !st.open[s.n] && !(st.probs && prob)) return false;
          if (st.kind && s.kind !== st.kind) return false;
          if (st.probs && !prob) return false;
          if (st.q && (s.name + " " + (s.detail || "") + " " + (s.error || "") + " " + kvPreview(s.fields)).toLowerCase().indexOf(st.q) < 0) return false;
          return true;
        });
        rows.sort(function (a, b) {
          var x = st.sort === "d" ? (a.duration_ms || 0) - (b.duration_ms || 0) : st.sort === "k" ? (a.kind < b.kind ? -1 : a.kind > b.kind ? 1 : a.n - b.n) : a.n - b.n;
          return x * st.dir;
        });
        rows.forEach(function (s) {
          var lane = h("div", { class: "lane", title: s.name + " · " + (SK[s.kind] || s.kind) + " · " + (D.STATUS[s.status] || s.status) + " · старт " + off(s.off) + " · " + D.dur(s.duration_ms) });
          tk.forEach(function (t) { lane.appendChild(h("span", { class: "gl", css: { left: pct(t * 100 / T) } })); });
          var gb = h("span", { class: "gb k-" + s.kind + (s.duration_ms ? "" : " pt") + (s.status === "error" ? " err" : ""), css: { left: pct(Math.min(99.5, s.off * 100 / T)) } });
          if (s.duration_ms) gb.style.width = pct(Math.max(page ? 0.3 : 0.4, s.duration_ms * 100 / T));
          lane.appendChild(gb);
          var open = !!st.open[s.n];
          var cells = page
            ? [h("td", { class: "t", text: String(s.n) }), h("td", { class: "k" }, [h("span", { class: "kdot k-" + s.kind }), SK[s.kind] || s.kind]),
              h("td", { class: "nm", title: s.name, text: s.name }), h("td", { class: "t n", text: off(s.off) }),
              h("td", { class: "n", text: s.kind === "log" ? "" : D.dur(s.duration_ms) }), h("td", { class: "gt" }, lane)]
            : [h("td", { class: "t", text: String(s.n) }), h("td", { class: "nm", title: s.name }, [h("span", { class: "kdot k-" + s.kind, title: SK[s.kind] || s.kind }), " ", s.name]),
              h("td", { class: "n", text: s.kind === "log" ? "" : D.dur(s.duration_ms) }), h("td", { class: "gt" }, lane)];
          var tr = h("tr", { class: "r s-" + s.status + " k-" + s.kind + (open ? " open" : ""), id: pid("s" + s.n), tabindex: "0", "aria-expanded": String(open) }, cells);
          function toggle() { st.open[s.n] = !st.open[s.n]; drawRows(); var el = document.getElementById(pid("s" + s.n)); if (el) el.focus(); }
          tr.addEventListener("click", toggle);
          tr.addEventListener("keydown", function (e) { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); e.stopPropagation(); toggle(); } });
          tb.appendChild(tr);
          if (open) {
            var det = h("div", { class: "rd-det" }, [
              h("div", { class: "kvline" }, [h("span", null, ["тип ", h("b", { text: SK[s.kind] || s.kind })]), h("span", null, ["статус ", h("b", { text: D.STATUS[s.status] || s.status })]),
                h("span", null, ["начало ", h("b", { text: D.time(s.start) })]), h("span", null, ["смещение ", h("b", { text: off(s.off) })]), h("span", null, ["конец ", h("b", { text: off(s.end) })]),
                s.claude ? h("button", { type: "button", class: "linkbtn", text: "→ Claude #" + s.claude, on: { click: function (e) { e.stopPropagation(); goClaude(s.claude); } } }) : null,
                h("button", { type: "button", class: "linkbtn", text: "ссылка на шаг", on: { click: function (e) { e.stopPropagation(); copy(permalink(m.id, "s" + s.n), "Ссылка на шаг скопирована"); } } })]),
              s.detail ? h("div", { class: "sub1", text: s.detail }) : null, s.error ? h("div", { class: "errline", text: s.error }) : null,
              kvList(s.fields) || (!s.detail && !s.error ? h("span", { class: "muted", text: "Полей нет." }) : null)]);
            stepExtra(s).forEach(function (x) { det.appendChild(x); });
            var dtr = h("tr", { class: "det" }, h("td", { colspan: String(ncol) }, det));
            dtr.addEventListener("click", function (e) { e.stopPropagation(); });
            tb.appendChild(dtr);
          }
        });
        if (!rows.length) tb.appendChild(h("tr", null, h("td", { colspan: String(ncol), class: "muted", text: "Под фильтр ничего не попало." })));
        cnt.textContent = rows.length + " из " + m.steps.length;
      }
      var nprob = m.errs.length + m.warns.length;
      mount(host, [
        { key: "kind", label: "Тип", type: "select", allLabel: "все", options: Object.keys(kc).map(function (k) { return { value: k, label: SK[k] || k, count: kc[k] }; }) },
        { key: "probs", label: "проблемы (" + nprob + ")", type: "toggle" },
        { key: "log", label: "строки лога", type: "toggle" },
        { key: "q", label: "Поиск", type: "text", placeholder: "шаг или значение", width: 16 }
      ], { kind: st.kind, probs: st.probs ? "1" : "", log: st.log ? "1" : "", q: st.q }, function (v) {
        st.kind = v.kind || ""; st.probs = v.probs === "1"; st.log = v.log === "1" || st.kind === "log"; st.q = (v.q || "").trim().toLowerCase(); drawRows();
      });
      drawRows();
      return box;
    }

    // ---- Claude sessions ----
    function answerView(c) {
      var box = h("div");
      if (!c.output) { box.appendChild(h("p", { class: "muted", text: c.status === "error" ? "Ответа нет: запуск упал." : "Ответа нет." })); return box; }
      var obj = null; try { obj = JSON.parse(c.output); } catch (e) { /* plain text */ }
      if (obj && typeof obj === "object" && !Array.isArray(obj)) {
        ["summary", "reply", "answer"].forEach(function (k) { if (typeof obj[k] === "string" && obj[k]) box.appendChild(h("p", { class: "ans-sum", text: obj[k] })); });
        Object.keys(obj).forEach(function (k) {
          var v = obj[k];
          if (!Array.isArray(v) || !v.length || typeof v[0] !== "object" || v[0] === null) return;
          var cols = [];
          v.forEach(function (row) { Object.keys(row || {}).forEach(function (x) { if (cols.indexOf(x) < 0 && row[x] !== "" && row[x] != null) cols.push(x); }); });
          cols = cols.slice(0, 7);
          var t = h("table", { class: "ans-tbl" }, [h("thead", null, h("tr", null, cols.map(function (x) { return h("th", { text: x }); }))),
            h("tbody", null, v.map(function (row) {
              return h("tr", null, cols.map(function (x) {
                var y = (row || {})[x];
                if (x === "verdict" && typeof y === "string") return h("td", null, h("span", { class: "badge " + (y === "confirm" ? "b-ok" : y === "reject" ? "b-error" : "b-info"), text: y }));
                return h("td", { text: y == null ? "" : typeof y === "object" ? JSON.stringify(y) : String(y) });
              }));
            }))]);
          box.appendChild(h("div", { class: "ans-cap", text: k + " · " + v.length }));
          box.appendChild(h("div", { class: "ans-scroll" }, t));
        });
      }
      if (!box.firstChild) { box.appendChild(longText(pretty(c.output))); return box; }
      var raw = h("details", null, h("summary", { text: "JSON целиком (" + D.bytes(c.output.length) + ")" }));
      raw.appendChild(longText(pretty(c.output), { full: true }));
      box.appendChild(raw);
      return box;
    }
    function paramsView(c) {
      var u = c.usage || {}, box = h("div");
      var info = { "модель": c.model || "по умолчанию", "длительность": D.dur(c.duration_ms), "по данным CLI": c.cli_duration_ms ? D.dur(c.cli_duration_ms) : "", "ходов": c.num_turns,
        "вход": D.full(u.input), "выход": D.full(u.output), "кэш: чтение": D.full(u.cache_read), "кэш: запись": D.full(u.cache_write), "доля входа из кэша": cacheShare(u),
        "стоимость (усл.)": D.usd(c.cost_usd), "таймаут": c.timeout || "", "код выхода": c.exit_code == null ? "" : c.exit_code, "каталог": c.dir || "", "без инструментов": c.no_tools ? "да" : "" };
      box.appendChild(sect("Итог", "", kvList(info)));
      if (c.models) {
        var mm = {};
        Object.keys(c.models).forEach(function (k) { var x = c.models[k]; mm[k] = D.short(tok(x)) + " токенов, " + D.usd(x.cost_usd); });
        box.appendChild(sect("По моделям", "", kvList(mm)));
      }
      function chips(arr, lim) {
        arr = arr || [];
        var d = h("div", { class: "chips2" });
        arr.slice(0, lim || arr.length).forEach(function (t) { d.appendChild(h("code", { text: t })); });
        if (lim && arr.length > lim) {
          var b = h("button", { type: "button", class: "linkbtn", text: "ещё " + (arr.length - lim) });
          b.addEventListener("click", function () { b.remove(); arr.slice(lim).forEach(function (t) { d.appendChild(h("code", { text: t })); }); });
          d.appendChild(b);
        }
        return d.firstChild ? d : h("span", { class: "muted", text: "—" });
      }
      box.appendChild(sect("Разрешённые инструменты", "--allowedTools", chips(c.allowed_tools)));
      box.appendChild(sect("Запрещённые", "--disallowedTools · " + (c.disallowed_tools || []).length, chips(c.disallowed_tools, 8)));
      if (c.mcp_tools && c.mcp_tools.length) box.appendChild(sect("MCP-инструменты бота", c.mcp_url || "", chips(c.mcp_tools)));
      if (c.context_dirs && c.context_dirs.length) box.appendChild(sect("Контекстные репозитории", String(c.context_dirs.length), chips(c.context_dirs.map(function (x) { return x.name + "@" + (x.sha || "").slice(0, 8); }), 6)));
      if (c.argv && c.argv.length) box.appendChild(sect("Командная строка", "промпты заменены ссылками, токены передаются по имени", longText(c.argv.join(" "))));
      if (c.stderr) box.appendChild(sect("stderr (хвост)", "", longText(c.stderr)));
      return box;
    }

    // Transcript: tool call + its result as one collapsed line.
    function parseTranscript(txt) {
      var items = [], byId = {}, svc = 0, init = null, final = null;
      (txt || "").split("\n").forEach(function (l) {
        if (!l.trim()) return;
        var ev; try { ev = JSON.parse(l); } catch (e) { items.push({ t: "svc", text: l }); svc++; return; }
        var t = ev.type;
        if (t === "system" && ev.subtype === "init") { init = ev; return; }
        if (t === "assistant" || t === "user") {
          var c = (ev.message || {}).content; if (typeof c === "string") c = [{ type: "text", text: c }];
          (Array.isArray(c) ? c : []).forEach(function (b) {
            if (b.type === "text" && b.text) items.push({ t: "text", who: t, text: b.text, sub: !!ev.parent_tool_use_id });
            else if (b.type === "thinking") items.push({ t: "think", text: b.thinking || "" });
            else if (b.type === "tool_use") { var it = { t: "tool", name: b.name, input: b.input, res: null, sub: !!ev.parent_tool_use_id }; byId[b.id] = it; items.push(it); }
            else if (b.type === "tool_result") {
              var rt = typeof b.content === "string" ? b.content : (Array.isArray(b.content) ? b.content : []).map(function (x) { return x.type === "text" ? x.text : "[" + x.type + "]"; }).join("\n");
              if (byId[b.tool_use_id]) byId[b.tool_use_id].res = { text: rt, err: !!b.is_error };
              else items.push({ t: "tool", name: "?", input: null, res: { text: rt, err: !!b.is_error } });
            }
          });
          if (ev.error) items.push({ t: "svc", text: "ошибка: " + ev.error, bad: true });
          return;
        }
        if (t === "result") { final = ev; return; }
        svc++; items.push({ t: "svc", text: (t || "событие") + (ev.subtype ? " · " + ev.subtype : ""), bad: t === "reviewbot_truncated" || t === "reviewbot_dropped" });
      });
      return { items: items, init: init, final: final, svc: svc };
    }
    function toolShort(n) { return (n || "").replace(/^mcp__reviewbot__/, "reviewbot·"); }
    function argSummary(input) {
      if (!input || typeof input !== "object") return "";
      var v = input.command || input.file_path || input.pattern || input.path || input.description || input.title || "";
      if (input.pattern && input.path) v = input.pattern + "  в " + input.path;
      if (!v) { var ks = Object.keys(input); v = ks.length ? ks.join(", ") : ""; }
      v = String(v);
      return v.length > 160 ? v.slice(0, 160) + "…" : v;
    }
    function transcriptView(m, c, idx) {
      var box = h("div", { class: "rd-tr" }), size = m.sizes[idx] || 0;
      var head = h("div", { class: "tr-bar" });
      var info = h("span", { class: "muted", text: (c.events || 0) + " событий" + (size ? " · " + D.bytes(size) : "") + (c.transcript_truncated ? " · обрезан (diag.max_run_mb)" : "") });
      head.appendChild(info);
      // The raw file is served by GET /runs/<id>/claude-<n>.jsonl (attachment).
      if (size > 0) head.appendChild(h("a", { class: "linkbtn", href: transcriptURL(m.id, c), download: m.id + "-claude-" + c.n + ".jsonl" }, [icon("dl"), " скачать .jsonl"]));
      box.appendChild(head);
      var body = h("div", null, h("p", { class: "muted", text: size > 0 ? "Загрузка транскрипта…" : "Транскрипт не записан (Claude не успел начать сессию)." }));
      box.appendChild(body);
      if (!size) return box;
      loadTranscript(m.id, c).then(function (txt) { clear(body); body.appendChild(transcriptBody(txt)); },
        function (e) { clear(body); body.appendChild(h("p", { class: "fail", text: "Не удалось загрузить транскрипт: " + (e.message || e) })); });
      return box;
    }
    function transcriptBody(txt) {
      var P = parseTranscript(txt), box = h("div");
      if (!txt) { box.appendChild(h("p", { class: "muted", text: "Транскрипт пуст: Claude не успел начать сессию." })); return box; }
      var tst = { q: "", tool: "", svc: false, openAll: false, limit: TR_PAGE };
      var counts = {}; P.items.forEach(function (it) { if (it.t === "tool") counts[it.name] = (counts[it.name] || 0) + 1; });
      var fields = [{ key: "q", label: "Поиск", type: "text", placeholder: "текст, путь, аргумент", width: 20 }];
      var tk = Object.keys(counts).sort(function (a, b) { return counts[b] - counts[a]; });
      if (tk.length) fields.push({ key: "tool", label: "Инструмент", type: "select", allLabel: "все", search: tk.length > 6, options: tk.map(function (n) { return { value: n, label: toolShort(n), count: counts[n] }; }) });
      if (P.svc) fields.push({ key: "svc", label: "служебные (" + P.svc + ")", type: "toggle" });
      var host = h("div", { class: "frow" });
      box.appendChild(host);
      var bar = h("div", { class: "tr-bar" });
      var exp = h("button", { type: "button", class: "linkbtn", text: "развернуть все" });
      exp.addEventListener("click", function () { tst.openAll = !tst.openAll; exp.textContent = tst.openAll ? "свернуть все" : "развернуть все"; draw(); });
      bar.appendChild(exp);
      var shownEl = h("span", { class: "muted" }); bar.appendChild(shownEl);
      box.appendChild(bar);
      if (P.init) {
        var i = P.init;
        box.appendChild(h("div", { class: "tr-init" }, [h("span", null, ["сессия ", h("b", { text: i.model || "" })]), h("span", null, ["cwd ", h("b", { text: i.cwd || "" })]),
          h("span", null, ["инструментов ", h("b", { text: String((i.tools || []).length) })]), h("span", null, ["права ", h("b", { text: i.permissionMode || "" })]),
          h("span", null, ["CLI ", h("b", { text: i.claude_code_version || "" })])]));
      }
      var list = h("div", { class: "tr" });
      box.appendChild(list);
      function hit(it) {
        if (tst.tool && !(it.t === "tool" && it.name === tst.tool)) return false;
        if (!tst.q) return true;
        var hay = (it.text || "") + " " + (it.name || "") + " " + (it.input ? JSON.stringify(it.input) : "") + " " + (it.res ? it.res.text : "");
        return hay.toLowerCase().indexOf(tst.q) >= 0;
      }
      function callNode(name, arg, res, inputText, r, cls) {
        var wrap = h("div", { class: "tr-call " + (cls || "") + (tst.openAll ? " open" : "") });
        var hd = h("button", { type: "button", class: "hd", "aria-expanded": String(tst.openAll) }, [h("span", { class: "chev" }, icon("chev")), h("span", { class: "tn", text: name }),
          h("span", { class: "ta", text: arg }), res ? h("span", { class: "tr-r", text: res }) : null]);
        wrap.appendChild(hd);
        var bd = null;
        function build() {
          bd = h("div", { class: "bd" });
          if (inputText) bd.appendChild(longText(inputText, { title: cls === "think" ? "" : "вызов" }));
          if (r) bd.appendChild(longText(r.text, { title: r.err ? "результат — ошибка" : "результат" }));
          wrap.appendChild(bd);
        }
        if (tst.openAll) build();
        hd.addEventListener("click", function () {
          var o = !wrap.classList.contains("open");
          wrap.classList.toggle("open", o); hd.setAttribute("aria-expanded", String(o));
          if (o && !bd) build();
          if (bd) bd.hidden = !o;
        });
        return wrap;
      }
      function draw() {
        clear(list);
        var hits = P.items.filter(function (it) { return (it.t !== "svc" || tst.svc) && hit(it); });
        hits.slice(0, tst.limit).forEach(function (it) {
          if (it.t === "text") list.appendChild(h("div", { class: "tr-msg " + (it.who === "assistant" ? "a" : "u") + (it.sub ? " sub" : "") }, [h("span", { class: "who", "aria-hidden": "true", text: it.who === "assistant" ? "C" : "U" }), h("div", { class: "txt", text: it.text })]));
          else if (it.t === "think") list.appendChild(it.text ? callNode("размышления", D.short(it.text.length) + " симв.", "", it.text, null, "think") : h("div", { class: "tr-svc", text: "размышления · текст в stream-json не сохраняется" }));
          else if (it.t === "tool") {
            var r = it.res, lines = r ? r.text.split("\n").length : 0;
            var rinfo = r ? (r.err ? "ошибка" : (lines > 1 ? pn(lines, "строка", "строки", "строк") + " · " : "") + D.bytes(r.text.length)) : "нет ответа";
            list.appendChild(callNode(toolShort(it.name), argSummary(it.input), rinfo, it.input ? JSON.stringify(it.input, null, 2) : "", r, (r && r.err ? "err" : "") + (it.sub ? " sub" : "")));
          } else list.appendChild(h("div", { class: "tr-svc" + (it.bad ? " errline" : ""), text: it.text }));
        });
        if (hits.length > tst.limit) {
          var more = h("button", { type: "button", class: "btn sm", text: "показать ещё " + Math.min(TR_PAGE, hits.length - tst.limit) + " из " + (hits.length - tst.limit) });
          more.addEventListener("click", function () { tst.limit += TR_PAGE; draw(); });
          list.appendChild(more);
        }
        shownEl.textContent = tst.q || tst.tool ? "найдено " + hits.length : "";
        if (!hits.length) list.appendChild(h("p", { class: "muted", text: tst.q || tst.tool ? "Ничего не найдено." : P.svc ? "Claude не сделал ни одного хода — в транскрипте только служебные события (" + P.svc + ")." : "Сессия открылась, но до обрыва модель не ответила ни разу." }));
        if (P.final && !tst.q && !tst.tool) {
          var f = P.final, bad = f.is_error || (f.subtype && f.subtype !== "success");
          list.appendChild(h("div", { class: "tr-final" + (bad ? " bad" : "") }, [h("b", { text: "итог: " + (f.subtype || "") }),
            f.num_turns != null ? h("span", { text: pn(f.num_turns, "ход", "хода", "ходов") }) : null, f.duration_ms ? h("span", { text: D.dur(f.duration_ms) }) : null,
            f.total_cost_usd ? h("span", { text: D.usd(f.total_cost_usd) }) : null, f.stop_reason ? h("span", { text: "stop: " + f.stop_reason }) : null,
            f.permission_denials && f.permission_denials.length ? h("span", { text: "отказов в правах: " + f.permission_denials.length }) : null]));
          if (f.result && bad) list.appendChild(longText(f.result));
          if (f.permission_denials && f.permission_denials.length) list.appendChild(longText(JSON.stringify(f.permission_denials, null, 2), { title: "отказы в правах (permission_denials)" }));
        }
      }
      mount(host, fields, {}, function (v) { tst.q = (v.q || "").trim().toLowerCase(); tst.tool = v.tool || ""; tst.svc = v.svc === "1"; tst.limit = TR_PAGE; draw(); });
      draw();
      return box;
    }
    function claudeTabs(m, c, idx) {
      var tabs = [
        { id: "ans", t: "Ответ", f: function () { return answerView(c); } },
        { id: "tr", t: "Транскрипт", n: c.events, f: function () { return transcriptView(m, c, idx); } },
        { id: "pr", t: "Промпт", f: function () { return longText(c.prompt, { title: D.bytes((c.prompt || "").length) }); } },
        c.system_prompt ? { id: "sys", t: "Системный промпт", f: function () { return longText(c.system_prompt, { title: "--append-system-prompt · " + D.bytes(c.system_prompt.length) }); } } : null,
        c.json_schema ? { id: "sch", t: "Схема", f: function () { return longText(pretty(c.json_schema), { title: "--json-schema" }); } } : null,
        { id: "par", t: "Параметры", f: function () { return paramsView(c); } }
      ].filter(Boolean);
      var box = h("div");
      if (c.error) box.appendChild(h("div", { class: "rd-cerr" }, [h("b", { text: (CLASS[c.error_class] || c.error_class || "ошибка") + ": " }), c.error]));
      var cur = st.ctab[c.n] || (c.status === "error" ? "tr" : "ans");
      var panel = h("div", { class: "tabpanel", role: "tabpanel" });
      function showTab() { dropBars(panel); clear(panel); var t = tabs.filter(function (x) { return x.id === cur; })[0] || tabs[0]; panel.appendChild(t.f()); }
      box.appendChild(tabsBar(tabs, cur, function (id) { cur = id; st.ctab[c.n] = id; showTab(); }));
      box.appendChild(panel); showTab();
      return box;
    }
    function claudeList(m) {
      var box = h("div", { class: "rd-cls" });
      if (page) box.appendChild(h("div", { class: "rd-clh" }, [h("span"), h("span", { text: "#" }), h("span", { text: "задача" }), h("span", { text: "статус" }), h("span", { class: "n", text: "время" }),
        h("span", { class: "n opt", text: "ходы" }), h("span", { class: "n opt", text: "токены" }), h("span", { class: "n opt", text: "кэш" }), h("span", { class: "n", text: "$" })]));
      m.claude.forEach(function (c, idx) {
        var it = h("div", { class: "rd-cl" + (st.cl[c.n] ? " open" : ""), id: pid("claude-" + c.n) });
        var status = c.status === "ok" ? badgeOf("ok") : badgeOf(c.error_class || "error");
        var kids = page
          ? [h("span", { class: "chev" }, icon("chev")), h("span", { class: "muted", text: String(c.n) }), h("span", { class: "nm2", text: purposeLabel(c.purpose) + " · " + (c.model || "модель по умолчанию") }), status,
            h("span", { class: "n", text: D.dur(c.duration_ms) }), h("span", { class: "n opt", text: String(c.num_turns || 0) }), h("span", { class: "n opt", text: D.short(c._tok) }),
            h("span", { class: "n opt", text: cacheShare(c.usage) || "—" }), h("span", { class: "n", text: D.usd(c.cost_usd) })]
          : [h("span", { class: "chev" }, icon("chev")), h("span", { class: "nm2", text: "#" + c.n + " " + purposeLabel(c.purpose) + " · " + (c.model || "") }), status,
            h("span", { class: "n", text: D.dur(c.duration_ms) }), h("span", { class: "n opt", text: D.short(c._tok) + " ток." }), h("span", { class: "n opt", text: D.usd(c.cost_usd) })];
        var hd = h("button", { type: "button", class: "hd", "aria-expanded": String(!!st.cl[c.n]), title: (c.num_turns || 0) + " ходов · кэш " + (cacheShare(c.usage) || "—") }, kids);
        var body = null;
        function sync() {
          var o = !!st.cl[c.n];
          it.classList.toggle("open", o); hd.setAttribute("aria-expanded", String(o));
          if (o && !body) { body = h("div", { class: "body" }, claudeTabs(m, c, idx)); it.appendChild(body); }
          else if (!o && body) { dropBars(body); body.remove(); body = null; }
        }
        hd.addEventListener("click", function () { st.cl[c.n] = !st.cl[c.n]; sync(); });
        it.appendChild(hd);
        box.appendChild(it);
        sync();
      });
      if (!m.claude.length) box.appendChild(h("p", { class: "muted rd-pad", text: "В этой задаче Claude не запускался." }));
      return box;
    }

    // ---- layout ----
    var panelBox = null, tabsEl = null;
    function tabList(m) {
      var t = [{ id: "wf", t: "Шаги", n: m.steps.length }];
      if (m.claude.length) t.push({ id: "cl", t: "Claude", n: m.claude.length });
      if (!page) {
        if (m.j.mr_iid) t.push({ id: "mr", t: "Этот MR" });
        t.push({ id: "pass", t: "Паспорт" });
      }
      return t;
    }
    function renderPanel() {
      var m = cur;
      dropBars(panelBox); clear(panelBox);
      if (st.tab === "wf") panelBox.appendChild(waterfall(m));
      else if (st.tab === "cl") panelBox.appendChild(claudeList(m));
      else if (st.tab === "mr") panelBox.appendChild(h("div", { class: "rd-pad" }, [h("p", { class: "muted rd-hint", text: "Запуски этого MR за окно списка. Клик — открыть в панели." }), historyView(m, h("div"))]));
      else panelBox.appendChild(h("div", { class: "rd-pad" }, [sect("Паспорт", "", passport(m)), sect("Время по типам шагов", "покрытие " + D.dur(m.total), kindsView(m))]));
    }
    function render() {
      var m = cur, tabs = tabList(m);
      if (!tabs.some(function (t) { return t.id === st.tab; })) st.tab = "wf";
      dropBars(); clear(root);
      var card = h("section", { class: "rd-card" });
      tabsEl = tabsBar(tabs, st.tab, function (id) { st.tab = id; renderPanel(); });
      panelBox = h("div", { class: "rd-panel" });
      card.appendChild(tabsEl); card.appendChild(panelBox);
      if (page) {
        root.appendChild(pageHead(m));
        var dg = diagBox(m); if (dg) root.appendChild(dg);
        var side = h("aside", { class: "rd-side", "aria-label": "Сводка" });
        side.appendChild(h("section", { class: "card" }, [h("div", { class: "card-h" }, h("h2", { text: "Паспорт" })), h("div", { class: "rd-pad" }, passport(m))]));
        side.appendChild(h("section", { class: "card" }, [h("div", { class: "card-h" }, [h("h2", { text: "Время по типам" }), h("span", { class: "hint", text: "покрытие " + D.dur(m.total) })]), h("div", { class: "rd-pad" }, kindsView(m))]));
        if (m.claude.length) side.appendChild(h("section", { class: "card" }, [h("div", { class: "card-h" }, [h("h2", { text: "Claude" }), h("span", { class: "hint", text: "клик — подробности" })]), h("div", { class: "rd-pad" }, claudeTable(m))]));
        if (m.j.mr_iid) side.appendChild(h("section", { class: "card" }, [h("div", { class: "card-h" }, [h("h2", { text: "Этот MR" }), h("span", { class: "hint", text: "запуски за окно" })]), historyView(m, h("div", { class: "rd-pad" }))]));
        root.appendChild(h("div", { class: "rd-grid" }, [h("div", { class: "rd-main" }, card), side]));
      } else {
        root.appendChild(factsLine(m));
        root.appendChild(mrLine(m));
        var dg2 = diagBox(m); if (dg2) root.appendChild(dg2);
        root.appendChild(card);
      }
      renderPanel();
    }

    return {
      show: show,
      id: function () { return cur ? cur.id : null; },
      run: function () { return cur ? cur.r : null; },
      refresh: function () { if (cur) return show(cur.id, { fresh: true }); },
      clear: function () { seq++; cur = null; st = null; dropBars(); clear(root); }
    };
  }

  global.RunDetail = { create: create, copy: copy, toast: toast, version: "1" };
})(window);
