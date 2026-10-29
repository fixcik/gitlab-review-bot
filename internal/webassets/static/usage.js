/* UsagePage: the /usage page (docs/usage-and-diagnostics.md, "/usage — расход
   токенов"). Renders /usage/data.json: KPIs with the previous period, the
   "На что обратить внимание" signals and the monthly budget (both computed
   by the bot), sortable tables, the chart (ECharts) and the filter bar
   (ReviewFilters), and keeps the URL in sync (period, since, until, user,
   project, kind, model, mr, cmp).
   Needs, loaded before it: echarts (optional: without it the page shows the
   tables), filters.js and the shared helpers D (webui common.js).
   CSP: DOM is built with createElement/textContent only; sizes and colours
   of data marks go through CSSOM setProperty; ECharts tooltips are
   markup with classes only and every data string escaped. */
(function () {
  "use strict";
  var D = window.D;
  if (!D) return;
  var $ = D.$, h = D.h, add = D.add, clear = D.clear, each = D.each;
  var root = document.documentElement;
  var reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var NS = "http://www.w3.org/2000/svg";
  D.initTheme();

  var state = { period: "30d", since: "", until: "", user: null, project: "", kind: "", model: null, mr: "", cmp: true, metric: "tokens", sort: {}, all: {} };
  var data = null, seq = 0;

  // ---- formatting ---------------------------------------------------------------
  var nf = new Intl.NumberFormat("ru-RU");
  var MON = ["янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"];
  var MONL = ["январь", "февраль", "март", "апрель", "май", "июнь", "июль", "август", "сентябрь", "октябрь", "ноябрь", "декабрь"];
  var WD = ["вс", "пн", "вт", "ср", "чт", "пт", "сб"];
  function full(n) { return nf.format(Math.round(n || 0)); }
  function dec(x) { return x.toFixed(1).replace(/\.0$/, "").replace(".", ","); }
  function short(n) {
    n = n || 0; var a = Math.abs(n);
    if (a >= 1e9) return dec(n / 1e9) + " млрд";
    if (a >= 1e6) return dec(n / 1e6) + " млн";
    if (a >= 1e4) return dec(n / 1e3) + " тыс.";
    return full(n);
  }
  function usd(n) {
    n = n || 0;
    if (n !== 0 && Math.abs(n) < 0.01) return "<$0.01";
    if (Math.abs(n) >= 10000) return "$" + Math.round(n).toLocaleString("en-US");
    return "$" + n.toFixed(2);
  }
  function usd3(n) { n = n || 0; return "$" + (Math.abs(n) < 0.1 ? n.toFixed(3) : n.toFixed(2)); }
  function pct(p) { p = (p || 0) * 100; if (p === 0) return "0%"; if (p < 0.1) return "<0,1%"; return (p < 10 ? dec(p) : String(Math.round(p))) + "%"; }
  function plural(n, a, b, c) { return D.plural(n, a, b, c); }
  function pad(x) { return x < 10 ? "0" + x : "" + x; }
  function hm(d) { return pad(d.getHours()) + ":" + pad(d.getMinutes()); }
  function dm(d, y) { return d.getDate() + " " + MON[d.getMonth()] + (y ? " " + d.getFullYear() : ""); }
  function dur(ms) { var s = Math.round((ms || 0) / 1000); return s < 60 ? s + " с" : Math.floor(s / 60) + " мин " + (s % 60) + " с"; }
  function safeURL(u) { return typeof u === "string" && /^https?:\/\//i.test(u) ? u : ""; }
  function repoName(p) { return D.repoName(p); }
  function nsOf(p) { var i = (p || "").lastIndexOf("/"); return i < 0 ? "" : p.slice(0, i); }
  function hash(str) { var x = 0; for (var i = 0; i < str.length; i++) x = (x * 31 + str.charCodeAt(i)) | 0; return Math.abs(x); }
  function esc(v) {
    return String(v === null || v === undefined ? "" : v).replace(/[&<>"'`]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;", "`": "&#96;" }[c];
    });
  }
  function svg(d, size) {
    var s = document.createElementNS(NS, "svg");
    s.setAttribute("width", size || 10); s.setAttribute("height", size || 10); s.setAttribute("viewBox", "0 0 16 16");
    s.setAttribute("fill", "none"); s.setAttribute("stroke", "currentColor"); s.setAttribute("stroke-width", "2.2");
    s.setAttribute("stroke-linecap", "round"); s.setAttribute("stroke-linejoin", "round"); s.setAttribute("aria-hidden", "true");
    var p = document.createElementNS(NS, "path"); p.setAttribute("d", d); s.appendChild(p);
    return s;
  }
  var ICON = { up: "M8 13V3M4 7l4-4 4 4", dn: "M8 3v10M4 9l4 4 4-4", warn: "M8 3.5v6M8 12.5v.01", info: "M8 7v5.5M8 3.5v.01",
    ext: "M9 2h5v5M14 2L7 9M12 10v3.5a.5.5 0 01-.5.5h-9a.5.5 0 01-.5-.5v-9a.5.5 0 01.5-.5H6" };
  function width(el, frac) { el.style.setProperty("width", Math.max(0, Math.min(100, frac * 100)) + "%"); return el; }

  // ---- kinds, labels -------------------------------------------------------------
  var KIND_TOK = { review: "k1", verification: "k2", ci: "k3", chat: "k4", map: "k5", learning: "k6", memory: "k7", other: "k8" };
  function kindTok(k) { return KIND_TOK[k] || "k8"; }
  function userLabel(k) { return k === "-" ? "без пользователя" : "@" + k; }
  function modelValue(k) { return k === "" ? "-" : k; }
  var PERIOD_NOUN = { "24h": "24 ч", "7d": "7 дн.", "30d": "30 дн.", "90d": "90 дн." };
  var PARTS = [
    { name: "Вход", key: "input_tokens", cls: "c-in", v: "s-in" }, { name: "Выход", key: "output_tokens", cls: "c-out", v: "s-out" },
    { name: "Кэш, чтение", key: "cache_read_tokens", cls: "c-cr", v: "s-cr" }, { name: "Кэш, запись", key: "cache_write_tokens", cls: "c-cw", v: "s-cw" }
  ];

  // ---- URL state -----------------------------------------------------------------
  function readURL() {
    var q = new URLSearchParams(location.search);
    state.since = q.get("since") || ""; state.until = q.get("until") || "";
    state.period = state.since || state.until ? "custom" : (PERIOD_NOUN[q.get("period")] ? q.get("period") : "30d");
    state.project = q.get("project") || "";
    state.user = q.has("user") ? q.get("user") : null;
    state.kind = q.get("kind") || "";
    state.model = q.get("model") ? q.get("model") : null;
    state.mr = q.get("mr") || "";
    state.cmp = q.get("cmp") !== "0";
  }
  function query(forData) {
    var q = new URLSearchParams();
    if (state.period === "custom") { if (state.since) q.set("since", state.since); if (state.until) q.set("until", state.until); }
    else q.set("period", state.period);
    if (state.user !== null) q.set("user", state.user);
    if (state.project) q.set("project", state.project);
    if (state.kind) q.set("kind", state.kind);
    if (state.model !== null) q.set("model", state.model);
    if (state.mr) q.set("mr", state.mr);
    if (!forData && !state.cmp) q.set("cmp", "0");
    return q.toString();
  }
  function filtered() { return state.user !== null || !!state.project || !!state.kind || state.model !== null || !!state.mr; }
  window.addEventListener("popstate", function () { readURL(); load(false); });

  // ---- filter bar ------------------------------------------------------------------
  var FIELDS = [
    { key: "period", label: "Период", type: "date-range", default: "30d", clearable: false,
      options: [{ value: "24h", label: "24 часа" }, { value: "7d", label: "7 дней" }, { value: "30d", label: "30 дней" }, { value: "90d", label: "90 дней" }] },
    { key: "user", label: "Кто", type: "search-select", allLabel: "все пользователи", options: [] },
    { key: "project", label: "Репозиторий", type: "search-select", allLabel: "все репозитории", options: [] },
    { key: "kind", label: "Тип", type: "select", allLabel: "все типы", options: [] },
    { key: "model", label: "Модель", type: "select", allLabel: "все модели", options: [] },
    { key: "mr", label: "MR", type: "search-select", allLabel: "все MR", options: [] }
  ];
  function barValues() {
    return { period: state.period === "custom" ? state.since + ".." + state.until : state.period,
      user: state.user === null ? "" : (state.user === "" ? "-" : state.user), project: state.project, kind: state.kind,
      model: state.model === null ? "" : state.model, mr: state.mr };
  }
  var bar = D.filters.mount($("bar"), { fields: FIELDS, values: barValues(), ariaLabel: "Фильтры расхода", onChange: function (v) {
    var p = v.period || "30d";
    if (p.indexOf("..") >= 0) { var s = p.split(".."); state.period = "custom"; state.since = s[0]; state.until = s[1]; }
    else { state.period = p; state.since = state.until = ""; }
    state.user = v.user ? v.user : null;
    state.project = v.project || ""; state.kind = v.kind || ""; state.model = v.model ? v.model : null; state.mr = v.mr || "";
    load(true);
  } });
  function facetOptions(key, list) {
    return (list || []).map(function (f) {
      var o = { value: f.value, label: f.label, count: f.count };
      if (f.short) o.shortLabel = f.short;
      if (key === "kind") o.swatch = kindTok(f.value);
      return o;
    });
  }
  function syncBar(d) {
    if (!bar) return;
    var fc = (d && d.facets) || {}, o = {};
    ["user", "project", "kind", "model", "mr"].forEach(function (k) { o[k] = facetOptions(k, fc[k]); });
    bar.refresh(o);
    bar.set(barValues());
  }
  // setFilter toggles one field (a row, a slice or a signal) and reloads.
  function setFilter(key, value) {
    var cur = { user: state.user, project: state.project, kind: state.kind, model: state.model, mr: state.mr }[key];
    var on = cur === value;
    if (key === "user") state.user = on ? null : value;
    else if (key === "model") state.model = on ? null : value;
    else state[key] = on ? "" : value;
    load(true);
  }

  // ---- controls ----------------------------------------------------------------------
  $("cmp").checked = state.cmp;
  $("cmp").addEventListener("change", function () { state.cmp = $("cmp").checked; history.replaceState(null, "", location.pathname + "?" + query(false)); if (data) render(data); });
  $("metric").addEventListener("click", function (e) {
    var b = e.target.closest("button"); if (!b) return;
    state.metric = b.dataset.m; syncControls(); if (data) { dayChart(data); dayTable(data); }
  });
  $("reload").addEventListener("click", function () { load(false); });
  $("retry").addEventListener("click", function () { load(false); });
  [["more-user", "user"], ["more-proj", "proj"], ["more-mr", "mr"]].forEach(function (x) {
    $(x[0]).addEventListener("click", function () { state.all[x[1]] = !state.all[x[1]]; if (data) render(data); });
  });
  function syncControls() {
    each($("metric").children, function (b) { b.setAttribute("aria-pressed", String(b.dataset.m === state.metric)); });
    $("cmp").checked = state.cmp;
  }

  // ---- loading -------------------------------------------------------------------------
  function load(push) {
    var url = location.pathname + "?" + query(false);
    if (push) history.pushState(null, "", url); else history.replaceState(null, "", url);
    syncControls();
    var my = ++seq;
    document.body.classList.add("busy"); $("main").setAttribute("aria-busy", "true");
    fetch("/usage/data.json?" + query(true), { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (r) {
        if (!r.ok) return r.text().then(function (t) { var e = new Error(t.trim()); e.status = r.status; throw e; });
        return r.json();
      })
      .then(function (d) { if (my !== seq) return; data = d; done(); render(d); })
      .catch(function (err) { if (my !== seq) return; done(); fail(err); });
  }
  function done() { document.body.classList.remove("busy"); $("main").setAttribute("aria-busy", "false"); }
  function fail(err) {
    $("error").hidden = false;
    var bad = err && err.status === 400;
    $("error-title").textContent = bad ? "Неверные параметры" : err && err.status === 401 ? "Нужен токен" : "Не удалось загрузить данные";
    $("error-text").textContent = (err && err.message) || "Сеть недоступна или бот не отвечает.";
    $("range").textContent = "—";
  }

  // ---- render ----------------------------------------------------------------------------
  function render(d) {
    $("error").hidden = true;
    syncBar(d);
    var since = new Date(d.since), until = new Date(d.until), last = new Date(until.getTime() - 1);
    var p = d.filter && d.filter.period;
    var rangeTxt = p === "custom" ? "Свой период" : ({ "24h": "Последние 24 часа", "7d": "Последние 7 дней", "30d": "Последние 30 дней", "90d": "Последние 90 дней" })[p] || "Период";
    $("range").textContent = rangeTxt + " · " + (p === "24h" || until - since <= 48 * 3600e3 ? dm(since) + ", " + hm(since) + " — " + dm(until) + ", " + hm(until) : dm(since) + " — " + dm(last, true));
    $("partial").hidden = !d.partial;
    $("partial").textContent = d.partial ? "Запусков слишком много: сигналы, график, прошлый период и счётчики фильтров посчитаны по последним 200 000 запускам." : "";

    var t = d.totals || {}, empty = !t.runs;
    $("empty").hidden = !empty; $("dash").hidden = empty;
    if (empty) { renderEmpty(); return; }
    $("dash").classList.add("ready");
    kpis(d);
    signals(d);
    budget(d);
    users(d);
    projects(d);
    kinds(d);
    models(d);
    mrs(d);
    dayTable(d);
    charts(d);
  }
  function renderEmpty() {
    $("empty-text").textContent = filtered()
      ? "С текущими фильтрами за выбранный период ничего не найдено. Снимите фильтр или выберите период длиннее."
      : "Бот не запускал Claude в выбранном окне. Выберите период длиннее.";
    var act = $("empty-act"); clear(act);
    if (filtered()) act.appendChild(h("button", { type: "button", "class": "btn", text: "Сбросить фильтры", on: { click: function () {
      state.user = null; state.project = state.kind = state.mr = ""; state.model = null; load(true);
    } } }));
    if (state.period !== "90d") act.appendChild(h("button", { type: "button", "class": "btn primary", text: "Показать 90 дней", on: { click: function () {
      state.period = "90d"; state.since = state.until = ""; load(true);
    } } }));
  }

  // ---- deltas ------------------------------------------------------------------------------
  function periodNoun(d) { var p = d.filter && d.filter.period; return PERIOD_NOUN[p] ? "к пред. " + PERIOD_NOUN[p] : "к пред. периоду"; }
  // delta fills el with the change against the previous period.
  function delta(el, d, cur, prev, money, compact) {
    clear(el); el.className = "delta"; el.hidden = !state.cmp; el.removeAttribute("title");
    if (!state.cmp) return;
    var pv = d.previous || {}, cov = pv.coverage;
    if (cov === "none") { el.classList.add("na"); el.textContent = compact ? "—" : "нет данных за прошлый период"; el.title = "Запуски за прошлый период не записаны (история начинается позже)."; return; }
    var fmt = money ? usd : short;
    var note = cov === "partial" ? " · прошлый период неполный" : "";
    if (cov === "partial") el.title = "История начинается " + dm(new Date(pv.history_from)) + ", " + hm(new Date(pv.history_from)) + ": прошлый период записан не целиком.";
    if (!prev && !cur) { el.textContent = compact ? "—" : "как в прошлом периоде"; return; }
    if (!prev) { el.textContent = compact ? "новое" : "в прошлом периоде — 0" + note; return; }
    var r = (cur - prev) / prev, up = r >= 0;
    el.title = (el.title ? el.title + " " : "") + "Прошлый период: " + fmt(prev) + ".";
    add(el, [svg(up ? ICON.up : ICON.dn), h("b", { text: (up ? "+" : "−") + pct(Math.abs(r)) }), compact ? null : " " + periodNoun(d) + note]);
  }

  // ---- KPIs -----------------------------------------------------------------------------------
  function kpis(d) {
    var t = d.totals, pv = (d.previous && d.previous.totals) || {}, rc = d.review_per_mr || {}, prc = (d.previous && d.previous.review_per_mr) || {};
    $("k-runs").textContent = full(t.runs);
    var sub = $("k-runs-sub");
    sub.textContent = t.failed ? full(t.failed) + " с ошибкой · " + pct(t.failed / t.runs) : "без ошибок";
    sub.className = "sub" + (t.failed ? " bad" : "");
    delta($("d-runs"), d, t.runs, pv.runs);

    var tok = $("k-tok"); tok.textContent = short(t.total_tokens); tok.title = full(t.total_tokens) + " токенов";
    var inAll = (t.input_tokens || 0) + (t.cache_read_tokens || 0) + (t.cache_write_tokens || 0);
    $("k-tok-sub").textContent = inAll ? "из кэша прочитано " + pct((t.cache_read_tokens || 0) / inAll) + " входа" : "";
    var split = $("k-split"), leg = $("k-legend"); clear(split); clear(leg);
    PARTS.forEach(function (p) {
      var v = t[p.key] || 0, share = t.total_tokens ? v / t.total_tokens : 0;
      if (v > 0) { var i = h("i", { "class": p.cls }); i.style.setProperty("flex-grow", String(v)); i.style.setProperty("flex-basis", "0"); split.appendChild(i); }
      leg.appendChild(h("li", { title: p.name + ": " + full(v) + " (" + pct(share) + ")" }, [
        h("span", { "class": "dot " + p.cls, "aria-hidden": "true" }), p.name + " ", h("b", { text: short(v) }), h("span", { "class": "sr", text: ", " + pct(share) })
      ]));
    });
    delta($("d-tok"), d, t.total_tokens, pv.total_tokens);

    var cost = $("k-cost"); cost.textContent = usd(t.cost_usd); cost.title = "$" + (t.cost_usd || 0).toFixed(4);
    delta($("d-cost"), d, t.cost_usd, pv.cost_usd, true);

    $("k-mr").textContent = rc.mrs ? usd3(rc.per_mr_usd) : "—";
    $("k-mr-sub").textContent = rc.mrs ? "ревью + перепроверки · " + rc.mrs + " MR" + (rc.median_run_usd ? " · медиана ревью " + usd3(rc.median_run_usd) : "") : "за период нет ревью MR";
    if (rc.mrs) delta($("d-mr"), d, rc.per_mr_usd, prc.mrs ? prc.per_mr_usd : 0, true); else { clear($("d-mr")); $("d-mr").hidden = true; }

    var avg = t.runs ? t.total_tokens / t.runs : 0;
    var av = $("k-avg"); clear(av); add(av, [short(avg), h("small", { text: "ток." })]); av.title = full(avg) + " токенов на запуск";
    $("k-avg-sub").textContent = usd3(t.cost_usd / t.runs) + " · " + dur(t.duration_ms / t.runs) + (d.avg_turns ? " · " + dec(d.avg_turns) + " " + plural(Math.round(d.avg_turns), "ход", "хода", "ходов") : "");
    delta($("d-avg"), d, t.cost_usd / t.runs, pv.runs ? pv.cost_usd / pv.runs : 0, true);
  }

  // ---- signals ------------------------------------------------------------------------------------
  function activeValue(key) { return { user: state.user, project: state.project, kind: state.kind, model: state.model, mr: state.mr }[key]; }
  function signals(d) {
    var list = d.signals || [], box = $("signals"); clear(box);
    $("n-sig").textContent = list.length;
    if (!list.length) {
      box.appendChild(h("p", { "class": "quiet", text: "За период ничего подозрительного: ни повторных ревью, ни выбросов по стоимости, ни холодного кэша, ни ошибок, ни запусков без пользователя." }));
    } else {
      var ul = h("ul", { "class": "ins" });
      list.forEach(function (s) {
        var f = s.filter, on = f && activeValue(f.key) === f.value;
        var inner = [
          h("span", { "class": "ic " + s.level, "aria-hidden": "true" }, svg(s.level === "info" ? ICON.info : ICON.warn, 12)),
          h("span", null, [h("span", { "class": "tt", text: s.title }), h("span", { "class": "dd", text: s.detail }),
            f ? h("span", { "class": "go", text: (on ? "Снять фильтр: " : "Показать: ") + f.label }) : null]),
          h("span", { "class": "amt" }, [usd(s.cost_usd), h("small", { text: s.cost_note })])
        ];
        var row = f ? h("button", { type: "button", "class": "row", "aria-pressed": String(!!on), on: { click: function () { setFilter(f.key, f.value); } } }, inner)
          : h("div", { "class": "row" }, inner);
        ul.appendChild(h("li", null, row));
      });
      box.appendChild(ul);
    }
    var r = d.rules || {};
    $("rules").textContent = "Правила: повторные ревью — от " + r.rereview_min + " ревью одного MR; выброс — дороже " + r.outlier_factor +
      "× медианы успешных запусков своего типа (медиана — если таких запусков не меньше " + r.outlier_min_samples + ", запуски дешевле " + usd3(r.outlier_min_usd) +
      " не считаются); холодный кэш — вход от " + Math.round(r.cold_min_input / 1000) + " тыс. токенов, из кэша меньше " + Math.round(r.cold_max_share * 100) + "%. Считается по запускам с текущими фильтрами.";
  }

  // ---- budget ---------------------------------------------------------------------------------------
  function budget(d) {
    var b = d.budget, card = $("budget-card");
    card.hidden = !b; $("row-sig").classList.toggle("one", !b);
    if (!b) return;
    var box = $("budget"); clear(box);
    var ms = new Date(b.month_start);
    box.appendChild(h("div", { "class": "top" }, [h("span", { "class": "big", text: usd(b.spent_usd) }),
      h("span", { "class": "muted", text: "из " + usd(b.budget_usd) + " · " + MONL[ms.getMonth()] + " " + ms.getFullYear() })]));
    var g = h("div", { "class": "gauge" + (b.spent_share > 1 ? " over" : ""), role: "img", "aria-label": "Потрачено " + pct(b.spent_share) + " бюджета, прогноз " + pct(b.forecast_share) });
    g.appendChild(width(h("i", { "class": "fc" }), b.forecast_share));
    g.appendChild(width(h("i"), b.spent_share));
    box.appendChild(g);
    box.appendChild(h("ul", { "class": "legend2" }, [
      h("li", null, [h("span", { "class": "sw bar1", "aria-hidden": "true" }), "потрачено " + pct(b.spent_share)]),
      h("li", null, [h("span", { "class": "sw fc", "aria-hidden": "true" }), "прогноз " + pct(b.forecast_share)])
    ]));
    box.appendChild(h("dl", null, [
      h("dt", { text: "Темп (среднее за 7 дней)" }), h("dd", { text: usd(b.rate_per_day_usd) + " / день" }),
      h("dt", { text: "Прогноз на конец месяца" }), h("dd", { text: usd(b.forecast_usd) }),
      h("dt", { text: "Осталось дней" }), h("dd", { text: dec(b.days_left) })
    ]));
    if (b.forecast_usd > b.budget_usd) box.appendChild(h("p", { "class": "fine bad", text: "Прогноз выше бюджета на " + usd(b.forecast_usd - b.budget_usd) + "." }));
    box.appendChild(h("p", { "class": "fine", text: "Прогноз = потрачено с начала месяца + темп × оставшиеся дни; темп — расход последних 7 дней, делённый на 7. Фильтры страницы на бюджет не влияют. Бюджет — настройка usage.budget_usd, только для информации: бот ничего по нему не ограничивает." +
      (b.rough ? " История — " + dec(b.history_days) + " дн., меньше 7: дни без записей считаются нулём, прогноз грубый." : "") }));
  }

  // ---- tables ------------------------------------------------------------------------------------------
  // cols: {k, label, cls, th, num, sort: fn(row) | false, cell: fn(row, i)}.
  // opt: {sort: {k, dir}, limit, more: [footId, btnId, allKey, noun], onRow, rowClass, groups: fn(rows) -> [{label, rows}], emptyText, caption}.
  function table(id, cols, rows, opt) {
    opt = opt || {};
    var t = $(id); clear(t);
    if (opt.caption) t.appendChild(h("caption", { "class": "sr", text: opt.caption }));
    var s = state.sort[id] || opt.sort, col = null;
    cols.forEach(function (c) { if (c.k === s.k) col = c; });
    var sorter = function (list) {
      if (!col || col.sort === false) return list;
      var sv = col.sort || function (r) { return r[col.k]; };
      return list.slice().sort(function (a, b) {
        var x = sv(a), y = sv(b), r = typeof x === "string" ? x.localeCompare(y, "ru") : (x || 0) - (y || 0);
        return s.dir === "asc" ? r : -r;
      });
    };
    var tr = h("tr");
    cols.forEach(function (c) {
      var sorted = c.k === s.k && c.sort !== false;
      var th = h("th", { scope: "col", "class": (c.num ? "num " : "") + (c.cls || "") + (c.th ? " " + c.th : "") });
      if (sorted) th.setAttribute("aria-sort", s.dir === "asc" ? "ascending" : "descending");
      if (c.sort === false) th.textContent = c.label;
      else th.appendChild(h("button", { type: "button", "class": "sort", title: "Сортировать", on: { click: function () {
        var cur = state.sort[id] || opt.sort;
        state.sort[id] = { k: c.k, dir: cur.k === c.k ? (cur.dir === "asc" ? "desc" : "asc") : (c.num ? "desc" : "asc") };
        table(id, cols, rows, opt);
      } } }, [c.label, sorted ? svg(s.dir === "asc" ? ICON.up : ICON.dn) : null]));
      tr.appendChild(th);
    });
    t.appendChild(h("thead", null, tr));
    var tb = h("tbody"), groups = opt.groups ? opt.groups(rows) : [{ rows: rows }], idx = 0, total = rows.length;
    var lim = opt.limit && !(opt.more && state.all[opt.more[2]]) ? opt.limit : Infinity;
    groups.forEach(function (g) {
      if (idx >= lim) return;
      if (g.label) tb.appendChild(h("tr", { "class": "grp" }, h("td", { colspan: String(cols.length), text: g.label })));
      sorter(g.rows).forEach(function (r) {
        if (idx >= lim) return;
        var i = idx++;
        var row = h("tr", { "class": opt.rowClass ? opt.rowClass(r) : null });
        cols.forEach(function (c) {
          var td = h("td", { "class": (c.num ? "num " : "") + (c.cls || "") });
          add(td, c.cell(r, i));
          row.appendChild(td);
        });
        if (opt.onRow && (!opt.canClick || opt.canClick(r))) {
          row.tabIndex = 0;
          row.addEventListener("click", function (e) { if (e.target.closest("a")) return; opt.onRow(r); });
          row.addEventListener("keydown", function (e) { if (e.target === row && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); opt.onRow(r); } });
        }
        tb.appendChild(row);
      });
    });
    if (!total) tb.appendChild(h("tr", { "class": "empty-row" }, h("td", { colspan: String(cols.length), "class": "muted", text: opt.emptyText || "Нет данных за период" })));
    t.appendChild(tb);
    if (opt.more) {
      var foot = $(opt.more[0]), btn = $(opt.more[1]);
      foot.hidden = total <= opt.limit;
      btn.textContent = state.all[opt.more[2]] ? "Свернуть" : "Показать все " + total + " " + opt.more[3];
    }
  }
  function shareCell(v, max, total) {
    return h("div", { "class": "share" }, [h("div", { "class": "track" }, width(h("div", { "class": "fill" }), max ? Math.max(0.01, v / max) : 0)),
      h("span", { "class": "p", text: pct(total ? v / total : 0) })]);
  }
  function maxOf(rows, k) { return rows.reduce(function (m, r) { return Math.max(m, r[k] || 0); }, 0); }
  function sel(cur, v) { return "click" + (cur === v ? " sel" : ""); }
  function fails(r) { return r.failed ? h("span", { "class": "fail", text: full(r.failed) }) : h("span", { "class": "dim", text: "—" }); }
  var tokCol = { k: "total_tokens", label: "Токены", num: true, cls: "strong", cell: function (r) { return short(r.total_tokens); } };
  var runsCol = { k: "runs", label: "Запуски", num: true, cell: function (r) { return full(r.runs); } };
  var costCol = { k: "cost_usd", label: "Стоимость", num: true, cell: function (r) { return usd(r.cost_usd); } };
  var perRunCol = { k: "per_run", label: "За запуск", num: true, cls: "hide-s", sort: function (r) { return r.runs ? r.cost_usd / r.runs : 0; }, cell: function (r) { return r.runs ? usd3(r.cost_usd / r.runs) : "—"; } };

  function users(d) {
    var rows = d.by_user || [], total = d.totals.total_tokens, mx = maxOf(rows, "total_tokens");
    var showDelta = state.cmp && d.previous && d.previous.coverage !== "none";
    $("n-user").textContent = rows.length;
    var cols = [
      { k: "rank", label: "#", sort: false, th: "rank", cls: "rank", cell: function (r, i) { return String(i + 1); } },
      { k: "label", label: "Пользователь", cls: "name", sort: function (r) { return r.key; }, cell: function (r) {
        var none = r.key === "-";
        return h("span", { "class": "who" }, [h("span", { "class": "ava " + (none ? "none" : "a" + hash(r.key) % 7), "aria-hidden": "true", text: none ? "—" : r.key.charAt(0) }),
          h("span", { "class": "t", text: userLabel(r.key), title: userLabel(r.key) })]);
      } },
      { k: "share", label: "Доля токенов", cls: "share-cell hide-s", sort: function (r) { return r.total_tokens; }, cell: function (r) { return shareCell(r.total_tokens, mx, total); } },
      tokCol, runsCol, perRunCol,
      { k: "failed", label: "Ошибки", num: true, cls: "hide-s", cell: fails },
      costCol
    ];
    if (showDelta) cols.push({ k: "dcost", label: "Δ стоимости", num: true, cls: "hide-s", sort: function (r) { return (r.cost_usd || 0) - (r.prev_cost_usd || 0); }, cell: function (r) {
      var el = h("span"); delta(el, d, r.cost_usd, r.prev_cost_usd || 0, true, true); return el;
    } });
    table("t-user", cols, rows, { caption: "Расход по пользователям", sort: { k: "cost_usd", dir: "desc" }, limit: 10, more: ["f-user", "more-user", "user", "пользователей"],
      rowClass: function (r) { return sel(state.user === "" ? "-" : state.user, r.key); }, onRow: function (r) { setFilter("user", r.key); } });
  }

  function projects(d) {
    var rows = d.by_project || [], total = d.totals.total_tokens, mx = maxOf(rows, "total_tokens");
    $("n-proj").textContent = rows.length;
    var spaces = {};
    rows.forEach(function (r) { if (r.key) spaces[nsOf(r.key)] = (spaces[nsOf(r.key)] || 0) + (r.cost_usd || 0); });
    var names = Object.keys(spaces), one = names.length <= 1;
    $("proj-hint").textContent = one && names[0] ? "группа " + names[0] + "/ · клик — фильтр" : "клик — фильтр";
    var groups = one ? null : function (list) {
      var by = {};
      list.forEach(function (r) { var n = r.key ? nsOf(r.key) : "\u0000"; (by[n] = by[n] || []).push(r); });
      return Object.keys(by).sort(function (a, b) { return (spaces[b] || 0) - (spaces[a] || 0) || a.localeCompare(b); }).map(function (n) {
        return { label: n === "\u0000" ? "без репозитория" : n === "" ? "без группы" : n + "/", rows: by[n] };
      });
    };
    table("t-project", [
      { k: "label", label: "Репозиторий", cls: "strong", sort: function (r) { return r.key; }, cell: function (r) {
        // the group is shown once (card header or group row), the name in full
        return h("span", { text: r.key ? repoName(r.key) : "—", title: r.key || "запуски без репозитория" });
      } },
      { k: "share", label: "Доля", cls: "share-cell hide-s", sort: function (r) { return r.total_tokens; }, cell: function (r) { return shareCell(r.total_tokens, mx, total); } },
      tokCol, runsCol, costCol
    ], rows, { caption: "Расход по репозиториям", sort: { k: "cost_usd", dir: "desc" }, groups: groups, limit: 8, more: ["f-proj", "more-proj", "proj", "репозиториев"],
      rowClass: function (r) { return r.key ? sel(state.project, r.key) : ""; }, canClick: function (r) { return !!r.key; }, onRow: function (r) { setFilter("project", r.key); } });
  }

  function kinds(d) {
    var rows = d.by_kind || [];
    table("t-kind", [
      { k: "label", label: "Тип", sort: function (r) { return r.label; }, cell: function (r) { return h("span", { "class": "kd" }, [h("i", { "class": "dot " + kindTok(r.key), "aria-hidden": "true" }), r.label]); } },
      runsCol,
      { k: "per_run", label: "За запуск", num: true, sort: function (r) { return r.runs ? r.cost_usd / r.runs : 0; }, cell: function (r) { return r.runs ? usd3(r.cost_usd / r.runs) : "—"; } },
      costCol
    ], rows, { caption: "Расход по типам запуска", sort: { k: "cost_usd", dir: "desc" }, rowClass: function (r) { return sel(state.kind, r.key); }, onRow: function (r) { setFilter("kind", r.key); } });
    var total = d.totals.cost_usd || 0;
    $("kind-sum").textContent = rows.length ? "Доли стоимости: " + rows.map(function (r) { return r.label + " " + pct(total ? r.cost_usd / total : 0); }).join(", ") + "." : "Нет данных.";
  }

  function models(d) {
    var rows = d.by_model || [], total = rows.reduce(function (a, r) { return a + (r.total_tokens || 0); }, 0), mx = maxOf(rows, "total_tokens");
    table("t-model", [
      { k: "label", label: "Модель", sort: function (r) { return r.label; }, cell: function (r) {
        return [h("span", { "class": "strong", text: r.label }), r.failed && r.failed === r.runs && !r.total_tokens ? h("span", { "class": "badge bad", text: "только ошибки" }) : null];
      } },
      { k: "share", label: "Доля токенов", cls: "share-cell hide-s", sort: function (r) { return r.total_tokens; }, cell: function (r) { return shareCell(r.total_tokens, mx, total); } },
      runsCol, costCol
    ], rows, { caption: "Расход по моделям", sort: { k: "share", dir: "desc" }, rowClass: function (r) { return sel(state.model, modelValue(r.key)); }, onRow: function (r) { setFilter("model", modelValue(r.key)); } });
  }

  function mrs(d) {
    var rows = d.top_mrs || [], total = d.totals.cost_usd, mx = maxOf(rows, "cost_usd"), many = (d.rules && d.rules.rereview_min) || 5;
    $("n-mr").textContent = rows.length;
    table("t-mr", [
      { k: "rank", label: "#", sort: false, cls: "rank", th: "rank", cell: function (r, i) { return String(i + 1); } },
      { k: "mr", label: "Merge request", cls: "mr", sort: function (r) { return r.key; }, cell: function (r) {
        var name = repoName(r.project_path) + "!" + r.mr_iid, path = r.project_path + "!" + r.mr_iid, url = safeURL(r.url);
        var a = url ? h("a", { href: url, target: "_blank", rel: "noopener noreferrer", title: path + " — открыть в GitLab" }, [name, svg(ICON.ext, 11)]) : h("span", { "class": "strong", text: name, title: path });
        if (url) a.lastChild.setAttribute("class", "ext");
        return [a, h("span", { "class": "title", text: r.title || "без названия", title: r.title || null })];
      } },
      { k: "share", label: "Доля стоимости", cls: "share-cell hide-s", sort: function (r) { return r.cost_usd; }, cell: function (r) { return shareCell(r.cost_usd, mx, total); } },
      { k: "reviews", label: "Ревью", num: true, cell: function (r) { return r.reviews >= many ? h("span", { "class": "badge warn", title: "Повторные ревью", text: "×" + r.reviews }) : String(r.reviews || 0); } },
      runsCol,
      { k: "total_tokens", label: "Токены", num: true, cls: "hide-s", cell: function (r) { return short(r.total_tokens); } },
      { k: "cost_usd", label: "Стоимость", num: true, cls: "strong", cell: function (r) { return usd(r.cost_usd); } }
    ], rows, { caption: "Самые дорогие merge request’ы", sort: { k: "cost_usd", dir: "desc" }, limit: 10, more: ["f-mr", "more-mr", "mr", "MR"], emptyText: "За период нет запусков, привязанных к MR",
      rowClass: function (r) { return sel(state.mr, r.key); }, onRow: function (r) { setFilter("mr", r.key); } });
  }

  // ---- chart data -----------------------------------------------------------------------------------------------
  function buckets(d) { return (d.series && d.series.buckets) || []; }
  function stepMs(d) { var s = d.series && d.series.step; return s === "15m" ? 15 * 60e3 : s === "30m" ? 30 * 60e3 : 0; }
  function bLabel(d, b) { var t = new Date(b.start); return stepMs(d) ? hm(t) : dm(t); }
  function bLong(d, b) {
    var t = new Date(b.start), st = stepMs(d);
    return st ? dm(t) + ", " + hm(t) + "–" + hm(new Date(t.getTime() + st)) : WD[t.getDay()] + ", " + dm(t, true);
  }
  function stepWord(d) { var s = d.series && d.series.step; return s === "15m" ? "по 15 минут" : s === "30m" ? "по 30 минут" : "по дням"; }
  function mval(x) { return state.metric === "cost" ? x.cost_usd || 0 : state.metric === "runs" ? x.runs || 0 : x.total_tokens || 0; }
  function mfmt(v) { return state.metric === "cost" ? usd(v) : state.metric === "runs" ? full(v) : short(v); }
  function prevOn(d) { return state.cmp && d.previous && d.previous.coverage !== "none" && buckets(d).some(function (b) { return b.prev && b.prev.runs > 0; }); }

  // seriesDefs: what the bars are stacked by — the user's repositories
  // (user filter), token kinds (tokens) or run kinds (cost, runs).
  function seriesDefs(d) {
    var bs = buckets(d), keys = (d.series && d.series.split_keys) || [];
    if (keys.length) {
      var defs = keys.map(function (k, i) { return { name: repoName(k), cls: "p" + i, color: T["c" + (i + 1)], vals: bs.map(function (b) { return mval(b.split[i] || {}); }) }; });
      var other = bs.map(function (b, j) { var v = mval(b); defs.forEach(function (df) { v -= df.vals[j]; }); return v > (state.metric === "cost" ? 0.00005 : 0.5) ? v : 0; });
      if (other.some(function (v) { return v > 0; })) defs.push({ name: "другие репозитории", cls: "px", color: T["c-other"], vals: other });
      return { by: "repo", defs: defs };
    }
    if (state.metric === "tokens") return { by: "tokens", defs: PARTS.map(function (p) { return { name: p.name, cls: p.cls, color: T[p.v], vals: bs.map(function (b) { return b[p.key] || 0; }) }; }) };
    var ks = {};
    bs.forEach(function (b) { for (var k in b.kinds || {}) ks[k] = (ks[k] || 0) + mval(b.kinds[k]); });
    var order = Object.keys(ks).sort(function (a, b) { return ks[b] - ks[a]; });
    var label = {}; (d.by_kind || []).forEach(function (r) { label[r.key] = r.label; });
    return { by: "kind", defs: order.map(function (k) { return { name: label[k] || k, cls: kindTok(k), color: T[kindTok(k)], vals: bs.map(function (b) { return mval((b.kinds || {})[k] || {}); }) }; }) };
  }

  function dayTable(d) {
    var bs = buckets(d).filter(function (b) { return b.runs || (state.cmp && b.prev && b.prev.runs); }).slice().reverse();
    var cols = [
      { k: "start", label: "Интервал", cls: "strong", cell: function (r) { return bLong(d, r); } },
      runsCol,
      { k: "input_tokens", label: "Вход", num: true, cls: "hide-s", cell: function (r) { return full(r.input_tokens); } },
      { k: "output_tokens", label: "Выход", num: true, cls: "hide-s", cell: function (r) { return full(r.output_tokens); } },
      { k: "cache_read_tokens", label: "Кэш, чтение", num: true, cls: "hide-s", cell: function (r) { return full(r.cache_read_tokens); } },
      { k: "cache_write_tokens", label: "Кэш, запись", num: true, cls: "hide-s", cell: function (r) { return full(r.cache_write_tokens); } },
      { k: "total_tokens", label: "Токены", num: true, cls: "strong", cell: function (r) { return full(r.total_tokens); } },
      costCol,
      { k: "failed", label: "Ошибки", num: true, cell: fails }
    ];
    if (prevOn(d)) cols.push({ k: "prev", label: "Пред. период", num: true, sort: function (r) { return mval(r.prev || {}); }, cell: function (r) { return mfmt(mval(r.prev || {})); } });
    table("t-day", cols, bs, { caption: "Расход по интервалам, полные числа", sort: { k: "start", dir: "desc" } });
  }

  // ---- charts (Apache ECharts, embedded in the binary, see /assets) ---------------------------------------------
  var EC = typeof window.echarts === "object" && window.echarts && typeof window.echarts.init === "function" ? window.echarts : null;
  var insts = {}, lastData = null, T = {};
  var ro = EC && window.ResizeObserver ? new ResizeObserver(function (entries) {
    entries.forEach(function (en) { var c = insts[en.target.id]; if (c && !c.isDisposed()) c.resize(); });
  }) : null;
  function chartsOff() {
    document.body.classList.add("no-charts");
    $("ec-note").hidden = false;
    $("day-alt").open = true;
    Object.keys(insts).forEach(function (k) { try { insts[k].dispose(); } catch (e) { /* ignore */ } delete insts[k]; });
    EC = null;
  }
  if (!EC) chartsOff();
  function cssv(n) { return getComputedStyle(root).getPropertyValue(n).trim(); }
  function readTheme() {
    ["surface", "line", "border", "text-strong", "text", "text-2", "text-3", "hover", "subtle", "accent", "accent-soft", "accent-line", "bar", "tip-bg", "tip-text", "tip-muted", "prev",
     "s-in", "s-out", "s-cr", "s-cw", "c1", "c2", "c3", "c4", "c5", "c6", "c-other", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"].forEach(function (n) { T[n] = cssv("--" + n); });
    T.font = getComputedStyle(document.body).fontFamily;
    var axis = { axisLine: { lineStyle: { color: T.border } }, axisTick: { show: false }, axisLabel: { color: T["text-3"], fontSize: 11 }, splitLine: { lineStyle: { color: T.line } } };
    EC.registerTheme("usage", {
      color: [T.c1, T.c2, T.c3, T.c4, T.c5, T.c6], backgroundColor: "transparent",
      textStyle: { fontFamily: T.font, color: T["text-2"] }, categoryAxis: axis, valueAxis: axis,
      tooltip: { backgroundColor: T["tip-bg"], borderColor: T["tip-bg"], borderWidth: 0, textStyle: { color: T["tip-text"], fontSize: 12, fontFamily: T.font } }
    });
  }
  function chartFor(id) {
    var c = insts[id];
    if (c && !c.isDisposed()) return c;
    c = insts[id] = EC.init($(id), "usage", { renderer: "canvas", locale: "EN" });
    if (ro) ro.observe($(id));
    return c;
  }
  function tipBase(extra) {
    var o = { className: "ectip", padding: [8, 10], confine: true, transitionDuration: reduceMotion ? 0 : 0.2,
      extraCssText: "box-shadow:" + cssv("--shadow") + ";border-radius:6px;line-height:18px;min-width:200px;max-width:340px;" };
    for (var k in extra) o[k] = extra[k];
    return o;
  }
  function tipRow(label, value, dotCls, top) {
    return '<div class="r' + (top ? " top" : "") + '"><span>' + (dotCls ? '<i class="dot ' + esc(dotCls) + '"></i>' : "") + esc(label) + "</span><span>" + esc(value) + "</span></div>";
  }
  function charts(d) {
    lastData = d;
    if (!EC) return;
    try { readTheme(); dayChart(d); kindChart(d); } catch (e) { chartsOff(); if (window.console) console.warn("charts disabled:", e); }
  }
  function rebuild() {
    if (!EC) return;
    Object.keys(insts).forEach(function (k) { insts[k].dispose(); delete insts[k]; });
    if (lastData && !$("dash").hidden) charts(lastData);
  }

  var dayState = { n: 0, active: -1 };
  function dayChart(d) {
    var bs = buckets(d), n = bs.length;
    var sd = EC ? seriesDefs(d) : { by: "", defs: [] }, defs = sd.defs, withPrev = prevOn(d);
    $("day-hint").textContent = stepWord(d) + (sd.by === "repo" ? " · по репозиториям пользователя" : sd.by === "tokens" ? " · по видам токенов" : sd.by === "kind" ? " · по типам запуска" : "");
    var leg = $("chart-legend"); clear(leg);
    defs.forEach(function (df) { leg.appendChild(h("li", null, [h("span", { "class": "dot " + df.cls, "aria-hidden": "true" }), df.name])); });
    if (withPrev) leg.appendChild(h("li", null, [h("span", { "class": "dash-sw", "aria-hidden": "true" }), "Предыдущий период (итого)"]));
    var totals = bs.map(mval), prev = bs.map(function (b) { return mval(b.prev || {}); });

    var sum = $("chart-sum"); clear(sum);
    var maxV = Math.max.apply(null, totals.concat([0])), peakI = totals.indexOf(maxV), act = totals.filter(function (v) { return v > 0; }).length;
    var unit = state.metric === "tokens" ? " токенов" : state.metric === "runs" ? " " + plural(maxV, "запуск", "запуска", "запусков") : "";
    if (maxV > 0) add(sum, ["Пик — ", h("b", { text: bLong(d, bs[peakI]) }), ": " + mfmt(maxV) + unit + ". Активность в " + act + " из " + n + " " + plural(n, "интервала", "интервалов", "интервалов") + " " + stepWord(d) + "."]);
    else sum.textContent = "За период ничего.";
    if (!EC) return;

    var series = defs.map(function (df, k) {
      return { type: "bar", name: df.name, stack: "t", data: df.vals, barMaxWidth: 32, barCategoryGap: "24%",
        itemStyle: { color: df.color, borderRadius: k === defs.length - 1 ? [2, 2, 0, 0] : 0 }, emphasis: { disabled: true }, blur: { itemStyle: { opacity: 1 } } };
    });
    if (withPrev) series.push({ type: "line", name: "Предыдущий период", data: prev, step: "middle", symbol: "none", z: 5, silent: true,
      lineStyle: { color: T.prev, type: [4, 3], width: 1.5 }, emphasis: { disabled: true } });
    var zoom = !stepMs(d) && n > 45, startV = zoom && $("c-day").clientWidth < 640 ? n - 45 : 0;
    var opt = {
      animation: !reduceMotion, animationDuration: 300, animationDurationUpdate: 200, aria: { enabled: false },
      grid: { left: 4, right: 12, top: 14, bottom: zoom ? 44 : 4, containLabel: true },
      xAxis: { type: "category", data: bs.map(function (b) { return bLabel(d, b); }), axisLabel: { color: T["text-3"], fontSize: 11, hideOverlap: true },
        axisLine: { lineStyle: { color: T.border } } },
      yAxis: { type: "value", splitNumber: 4, axisLabel: { color: T["text-3"], fontSize: 11, formatter: function (v) { return mfmt(v); } }, splitLine: { lineStyle: { color: T.line } } },
      tooltip: tipBase({ trigger: "axis", axisPointer: { type: "shadow", z: 0, shadowStyle: { color: T["accent-soft"], opacity: 0.7 } }, formatter: function (ps) {
        var p = Array.isArray(ps) ? ps[0] : ps; return p ? dayTip(d, p.dataIndex, defs, withPrev) : "";
      } }),
      series: series
    };
    if (zoom) opt.dataZoom = [
      { type: "inside", startValue: startV, endValue: n - 1, zoomOnMouseWheel: "ctrl", moveOnMouseWheel: false, moveOnMouseMove: true, minValueSpan: 7 },
      { type: "slider", startValue: startV, endValue: n - 1, height: 18, bottom: 6, left: 56, right: 16, minValueSpan: 7, borderColor: T.border, backgroundColor: "transparent",
        fillerColor: T["accent-soft"], textStyle: { color: T["text-3"], fontSize: 11 }, labelFormatter: function (v, s) { return s || ""; },
        dataBackground: { lineStyle: { color: T["text-3"], width: 0.5 }, areaStyle: { color: T.subtle } },
        selectedDataBackground: { lineStyle: { color: T.accent, width: 0.5 }, areaStyle: { color: T["accent-line"] } },
        handleStyle: { color: T.surface, borderColor: T["text-3"] }, moveHandleStyle: { color: T.subtle }, brushSelect: false }
    ];
    var c = chartFor("c-day");
    c.setOption(opt, { notMerge: true });
    dayState = { n: n, active: -1, d: d, zoom: zoom ? [startV, n - 1] : null };
    c.off("datazoom");
    if (zoom) c.on("datazoom", function () { var o = c.getOption().dataZoom; if (o && o[0]) dayState.zoom = [o[0].startValue, o[0].endValue]; });
  }
  function dayTip(d, i, defs, withPrev) {
    var b = buckets(d)[i]; if (!b) return "";
    var out = '<div class="h">' + esc(bLong(d, b) + (b.runs ? "" : " — запусков нет")) + "</div>";
    if (b.runs) {
      var lbl = { tokens: "Токены", cost: "Стоимость", runs: "Запуски" }[state.metric];
      out += tipRow(lbl, state.metric === "cost" ? usd(mval(b)) : full(mval(b)), null, true);
      defs.forEach(function (df) { if (df.vals[i]) out += tipRow(df.name, state.metric === "cost" ? usd(df.vals[i]) : full(df.vals[i]), df.cls); });
      out += '<div class="sep"></div>';
      if (state.metric !== "tokens") out += tipRow("Токены", full(b.total_tokens));
      if (state.metric !== "runs") out += tipRow("Запуски", full(b.runs) + (b.failed ? " (ошибок: " + full(b.failed) + ")" : ""));
      if (state.metric !== "cost") out += tipRow("Стоимость", usd(b.cost_usd));
    }
    if (withPrev) out += tipRow("Предыдущий период", state.metric === "cost" ? usd(mval(b.prev || {})) : full(mval(b.prev || {})));
    return out;
  }
  function dayActivate(i) {
    var st = dayState, c = insts["c-day"]; if (!st.n || !c) return;
    i = Math.max(0, Math.min(st.n - 1, i)); st.active = i;
    if (st.zoom && (i < st.zoom[0] || i > st.zoom[1])) {
      var span = st.zoom[1] - st.zoom[0], s0 = i < st.zoom[0] ? i : i - span;
      c.dispatchAction({ type: "dataZoom", startValue: s0, endValue: s0 + span }); st.zoom = [s0, s0 + span];
    }
    c.dispatchAction({ type: "showTip", seriesIndex: 0, dataIndex: i });
    var b = buckets(st.d)[i];
    $("chart-live").textContent = bLong(st.d, b) + ": " + short(b.total_tokens) + " токенов, " + full(b.runs) + " " + plural(b.runs, "запуск", "запуска", "запусков") + ", " + usd(b.cost_usd);
  }
  function dayHide() { var c = insts["c-day"]; if (c) c.dispatchAction({ type: "hideTip" }); dayState.active = -1; }
  $("chartwrap").addEventListener("blur", dayHide);
  $("chartwrap").addEventListener("keydown", function (e) {
    var n = dayState.n; if (!n) return;
    var a = dayState.active, i;
    if (e.key === "ArrowLeft") i = a < 0 ? n - 1 : a - 1;
    else if (e.key === "ArrowRight") i = a < 0 ? n - 1 : a + 1;
    else if (e.key === "Home") i = 0;
    else if (e.key === "End") i = n - 1;
    else if (e.key === "Escape") { dayHide(); return; }
    else return;
    e.preventDefault(); dayActivate(i);
  });

  // run kinds: donut of the cost share; a click on a slice sets the kind filter
  function kindChart(d) {
    var rows = (d.by_kind || []).filter(function (r) { return r.cost_usd > 0; }), total = d.totals.cost_usd || 0;
    var c = chartFor("c-kind");
    c.setOption({
      animation: !reduceMotion, animationDuration: 300, aria: { enabled: false },
      tooltip: tipBase({ trigger: "item", confine: false, appendTo: "body", formatter: function (p) {
        var r = rows[p.dataIndex]; if (!r) return "";
        return '<div class="h"><i class="dot ' + kindTok(r.key) + '"></i>' + esc(r.label) + "</div>" + tipRow("Доля стоимости", pct(total ? r.cost_usd / total : 0), null, true) +
          tipRow("Стоимость", usd(r.cost_usd)) + tipRow("Запуски", full(r.runs)) + tipRow("Токены", full(r.total_tokens));
      } }),
      graphic: [{ type: "text", left: "center", top: "middle", silent: true, style: { text: usd(total) + "\nстоимость", fill: T["text-strong"], font: "600 13px " + T.font, align: "center", lineHeight: 16 } }],
      series: [{ type: "pie", radius: ["62%", "96%"], center: ["50%", "50%"], startAngle: 90, avoidLabelOverlap: false, label: { show: false }, labelLine: { show: false },
        itemStyle: { borderColor: T.surface, borderWidth: 2 }, emphasis: { scale: true, scaleSize: 3, label: { show: false } },
        data: rows.map(function (r) { return { name: r.label, value: r.cost_usd, itemStyle: { color: T[kindTok(r.key)], opacity: state.kind && state.kind !== r.key ? 0.3 : 1 } }; }) }]
    }, { notMerge: true });
    c.off("click");
    c.on("click", function (p) { var r = rows[p.dataIndex]; if (r) setFilter("kind", r.key); });
  }

  D.onTheme(function () { if (lastData) rebuild(); });
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-reduced-motion: reduce)");
    var onMQ = function (m, fn) { if (m.addEventListener) m.addEventListener("change", fn); else if (m.addListener) m.addListener(fn); };
    onMQ(mq, function () { reduceMotion = mq.matches; rebuild(); });
  }

  readURL();
  if (bar) bar.set(barValues());
  load(false);
})();
