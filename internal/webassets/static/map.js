/* MapGraph: the container diagram and the hint snippet of the /map page
   (docs/usage-and-diagnostics.md, «/map»). Vanilla JS on top of common.js
   (D) and the embedded elkjs (webassets.ElkPath); load it after both.

   A C4-like container diagram drawn as SVG: every repository is a block
   (name, role, endpoints, incoming / outgoing calls, unresolved calls),
   external systems are a row of blocks at the bottom (an ELK partition),
   one arrow per pair of repositories with its label ("HTTP · 31"; several
   protocols are parallel lines of one bundle, each in its protocol colour),
   calls linked by a topology rule are a thin solid arrow marked «по
   правилу». ELK (layered, top to bottom, orthogonal edges) places blocks,
   bends and labels, so labels never overlap.

   Interaction: hover or focus highlights a block's or an arrow's links;
   Tab walks blocks and arrows, Enter or a click opens the details
   (onNode / onEdge), Esc closes them (onClose).

   CSP-safe: every string from the data is inserted as SVG text nodes;
   geometry goes through SVG attributes, colours through classes (map.css);
   never a style attribute or an inline handler.

     MapGraph.draw(hostEl, {
       nodes: [{id, label, role, stats, warn, muted}],        // repositories
       ext: [{id, label, sub}],                                // external systems
       edges: [{from, to, counts: {http: n}, rule_counts: {}}], // repository pairs
       extEdges: [{from, to, total}],                          // repository -> external
       protos: {http: true, ...},                              // the protocols shown
       onNode(id), onEdge(edge), onClose()
     }).then(function (ok) {})                                 // ok=false: ELK is missing
     MapGraph.snippet(hints, key, repo)    // {text, lines: [{text, kind}]}
     MapGraph.yamlScalar(s)                // s as a YAML scalar (quoted when needed) */
(function (global) {
  "use strict";
  var D = global.D;
  if (!D) return;
  var SVGNS = "http://www.w3.org/2000/svg";
  var PROTOS = ["http", "webhook", "ws", "sse"];
  var PROTO_PL = { http: "HTTP", webhook: "вебхуки", ws: "WebSocket", sse: "SSE" };
  var PROTO_SHORT = { http: "HTTP", webhook: "вебхук", ws: "WS", sse: "SSE" };
  var NODE_W = 216, NODE_H = 88, EXT_W = 176, EXT_H = 56;

  function s(tag, attrs, kids) {
    var e = document.createElementNS(SVGNS, tag);
    if (attrs) for (var k in attrs) {
      if (attrs[k] === null || attrs[k] === undefined) continue;
      if (k === "text") e.textContent = attrs[k]; else e.setAttribute(k, attrs[k]);
    }
    (kids || []).forEach(function (c) { if (c) e.appendChild(c); });
    return e;
  }
  function byStr(a, b) { return a < b ? -1 : a > b ? 1 : 0; }
  function fit(t, n) { t = String(t || ""); return t.length <= n ? t : t.slice(0, Math.max(1, n - 1)) + "…"; }
  function textW(t, px) { return Math.ceil(String(t).length * px * 0.6) + 14; }

  // edgeWidth: a line by the number of its calls (1.5 .. 4).
  function edgeWidth(n, max) { return 1.5 + 2.5 * Math.sqrt(n / Math.max(1, max)); }

  // offset shifts an orthogonal polyline sideways by d (the parallel lines
  // of one bundle).
  function offset(pts, d) {
    if (!d || pts.length < 2) return pts;
    var ns = [];
    for (var i = 0; i + 1 < pts.length; i++) {
      var dx = pts[i + 1].x - pts[i].x, dy = pts[i + 1].y - pts[i].y, l = Math.sqrt(dx * dx + dy * dy) || 1;
      ns.push({ x: -dy / l, y: dx / l });
    }
    return pts.map(function (p, i) {
      var a = ns[Math.max(0, i - 1)], b = ns[Math.min(ns.length - 1, i)];
      var same = Math.abs(a.x - b.x) < 1e-6 && Math.abs(a.y - b.y) < 1e-6;
      return { x: p.x + d * (same ? a.x : a.x + b.x), y: p.y + d * (same ? a.y : a.y + b.y) };
    });
  }
  function pathD(pts) {
    return pts.map(function (p, i) { return (i ? "L" : "M") + p.x.toFixed(1) + " " + p.y.toFixed(1); }).join(" ");
  }

  // edgeLabel: "HTTP · 31" or "HTTP 21 · WS 10" for the protocols shown.
  function edgeLabel(counts, ps) {
    if (ps.length === 1) return PROTO_SHORT[ps[0]] + " · " + counts[ps[0]];
    return ps.map(function (p) { return PROTO_SHORT[p] + " " + counts[p]; }).join(" · ");
  }

  // model builds the ELK graph and the drawing items for the shown protocols.
  function model(o) {
    var items = [], max = 1;
    var ids = {};
    o.nodes.forEach(function (n) { ids[n.id] = true; });
    o.edges.slice().sort(function (a, b) { return byStr(a.from + ">" + a.to, b.from + ">" + b.to); }).forEach(function (e, i) {
      if (!ids[e.from] || !ids[e.to] || e.from === e.to) return;
      var code = PROTOS.filter(function (p) { return e.counts && e.counts[p] && o.protos[p]; });
      var rule = PROTOS.filter(function (p) { return e.rule_counts && e.rule_counts[p] && o.protos[p]; });
      code.forEach(function (p) { max = Math.max(max, e.counts[p]); });
      if (code.length) items.push({ id: "e" + i, kind: "code", edge: e, from: e.from, to: e.to, ps: code, counts: e.counts, label: edgeLabel(e.counts, code) });
      if (rule.length) items.push({ id: "r" + i, kind: "rule", edge: e, from: e.from, to: e.to, ps: rule, counts: e.rule_counts, label: "по правилу · " + edgeLabel(e.rule_counts, rule) });
    });
    var ext = {};
    (o.ext || []).forEach(function (x) { ext[x.id] = true; });
    (o.extEdges || []).forEach(function (e, i) {
      if (ids[e.from] && ext[e.to]) items.push({ id: "x" + i, kind: "ext", edge: e, from: e.from, to: e.to, ps: [], label: String(e.total) });
    });
    var children = o.nodes.slice().sort(function (a, b) { return byStr(a.id, b.id); }).map(function (n) {
      return { id: n.id, width: NODE_W, height: NODE_H, layoutOptions: { "elk.partitioning.partition": "0" } };
    }).concat((o.ext || []).filter(function (x) { return items.some(function (it) { return it.to === x.id; }); }).map(function (x) {
      return { id: x.id, width: EXT_W, height: EXT_H, layoutOptions: { "elk.partitioning.partition": "1" } };
    }));
    var graph = {
      id: "root",
      layoutOptions: {
        "elk.algorithm": "layered", "elk.direction": "DOWN", "elk.edgeRouting": "ORTHOGONAL",
        "elk.partitioning.activate": "true", "elk.layered.nodePlacement.strategy": "NETWORK_SIMPLEX",
        "elk.layered.spacing.nodeNodeBetweenLayers": "72", "elk.spacing.nodeNode": "48", "elk.spacing.edgeEdge": "16",
        "elk.spacing.edgeLabel": "6", "elk.spacing.edgeNode": "22", "elk.layered.spacing.edgeNodeBetweenLayers": "22",
        "elk.edgeLabels.placement": "CENTER", "elk.padding": "[top=16,left=16,bottom=16,right=16]"
      },
      children: children,
      edges: items.map(function (it) {
        return { id: it.id, sources: [it.from], targets: [it.to], labels: [{ id: it.id + "l", text: it.label, width: textW(it.label, 11), height: 18 }] };
      })
    };
    return { graph: graph, items: items, max: max };
  }

  var state = {}; // per host: {svg, width, height}
  function resize(host) {
    var st = state[host.id];
    if (!st || !st.svg) return;
    var avail = Math.max(280, host.clientWidth - 2);
    var k = Math.min(1, Math.max(0.6, avail / st.width));
    st.svg.setAttribute("width", Math.round(st.width * k));
    st.svg.setAttribute("height", Math.round(st.height * k));
    host.classList.toggle("wide", st.width * k > avail + 1);
  }

  function draw(host, o) {
    if (typeof global.ELK !== "function") return Promise.resolve(false);
    var m = model(o);
    var st = state[host.id] = state[host.id] || { seq: 0 };
    var seq = ++st.seq;
    return new global.ELK().layout(m.graph).then(function (g) {
      if (st.seq !== seq) return true; // a newer draw won
      render(host, o, m, g);
      return true;
    });
  }

  function render(host, o, m, g) {
    var info = {}, ext = {};
    o.nodes.forEach(function (n) { info[n.id] = n; });
    (o.ext || []).forEach(function (x) { ext[x.id] = x; });
    var W = Math.ceil(g.width), H = Math.ceil(g.height);
    var svg = s("svg", { class: "c4", viewBox: "0 0 " + W + " " + H, role: "group", "aria-label": "Схема контейнеров: репозитории, связи и внешние системы" });
    var defs = s("defs");
    PROTOS.concat(["ext"]).forEach(function (k) {
      defs.appendChild(s("marker", { id: "c4a-" + k, viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, markerUnits: "userSpaceOnUse", orient: "auto-start-reverse" },
        [s("path", { d: "M0 1 L10 5 L0 9 z", class: "c4m c4m-" + k })]));
    });
    svg.appendChild(defs);
    var eg = s("g", { class: "c4-edges" }), lg = s("g", { class: "c4-labels" }), ng = s("g", { class: "c4-nodes" });
    var byId = {};
    m.items.forEach(function (it) { byId[it.id] = it; });
    var edgeEls = [], nodeEls = {};
    function highlight(on, ids) {
      svg.classList.toggle("hl", on);
      edgeEls.forEach(function (x) { x.el.classList.toggle("on", on && ids.edges[x.it.id] === true); x.lab.classList.toggle("on", on && ids.edges[x.it.id] === true); });
      Object.keys(nodeEls).forEach(function (id) { nodeEls[id].classList.toggle("on", on && ids.nodes[id] === true); });
    }
    function nodeLinks(id) {
      var r = { edges: {}, nodes: {} };
      r.nodes[id] = true;
      m.items.forEach(function (it) { if (it.from === id || it.to === id) { r.edges[it.id] = true; r.nodes[it.from] = true; r.nodes[it.to] = true; } });
      return r;
    }
    function edgeLinks(it) {
      var r = { edges: {}, nodes: {} };
      r.edges[it.id] = true; r.nodes[it.from] = true; r.nodes[it.to] = true;
      return r;
    }
    function activate(el, links, open, label) {
      el.setAttribute("tabindex", "0");
      el.setAttribute("role", "button");
      el.setAttribute("aria-label", label);
      el.addEventListener("mouseenter", function () { highlight(true, links()); });
      el.addEventListener("mouseleave", function () { if (document.activeElement !== el) highlight(false); });
      el.addEventListener("focus", function () { highlight(true, links()); });
      el.addEventListener("blur", function () { highlight(false); });
      el.addEventListener("click", function () { open(); });
      el.addEventListener("keydown", function (ev) {
        if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); open(); }
        if (ev.key === "Escape" && o.onClose) { ev.preventDefault(); o.onClose(); }
      });
    }

    (g.edges || []).forEach(function (ge) {
      var it = byId[ge.id], sec = ge.sections && ge.sections[0];
      if (!it || !sec) return;
      var pts = [sec.startPoint].concat(sec.bendPoints || [], [sec.endPoint]);
      var grp = s("g", { class: "c4-edge k-" + it.kind });
      grp.appendChild(s("path", { d: pathD(pts), class: "c4-hit" }));
      if (it.kind === "ext") {
        grp.appendChild(s("path", { d: pathD(pts), class: "c4-line c4-ext", "marker-end": "url(#c4a-ext)" }));
      } else {
        it.ps.forEach(function (p, k) {
          var d = (k - (it.ps.length - 1) / 2) * 4.5;
          var w = it.kind === "rule" ? 1.2 : edgeWidth(it.counts[p], m.max);
          grp.appendChild(s("path", { d: pathD(offset(pts, d)), class: "c4-line " + (it.kind === "rule" ? "c4-rule p-" + p : "p-" + p), "stroke-width": w.toFixed(2),
            "marker-end": "url(#c4a-" + p + ")" }));
        });
      }
      eg.appendChild(grp);
      var gl = (ge.labels || [])[0], lab = s("g", { class: "c4-elabel k-" + it.kind });
      if (gl) {
        lab.appendChild(s("rect", { x: gl.x, y: gl.y, width: gl.width, height: gl.height, rx: 9 }));
        lab.appendChild(s("text", { x: gl.x + gl.width / 2, y: gl.y + 12.5, "text-anchor": "middle", text: it.label }));
      }
      lg.appendChild(lab);
      edgeEls.push({ it: it, el: grp, lab: lab });
      var name = (info[it.from] || {}).label + " → " + ((info[it.to] || ext[it.to] || {}).label) + ": " + it.label;
      activate(grp, function () { return edgeLinks(it); }, function () { if (o.onEdge) o.onEdge(it); }, name);
      // The label is part of the arrow for the pointer (keyboard: the arrow).
      lab.addEventListener("mouseenter", function () { highlight(true, edgeLinks(it)); });
      lab.addEventListener("mouseleave", function () { highlight(false); });
      lab.addEventListener("click", function () { if (o.onEdge) o.onEdge(it); });
    });

    (g.children || []).forEach(function (gn) {
      var n = info[gn.id], x = ext[gn.id];
      var grp = s("g", { class: "c4-node" + (x ? " ext" : "") + (n && n.warn ? " warn" : "") + (n && n.muted ? " muted" : ""), transform: "translate(" + gn.x + "," + gn.y + ")" });
      grp.appendChild(s("rect", { width: gn.width, height: gn.height, rx: 8 }));
      if (x) {
        grp.appendChild(s("text", { class: "c4-kind", x: 12, y: 18, text: "внешняя система" }));
        grp.appendChild(s("text", { class: "c4-name", x: 12, y: 36, text: fit(x.label, 24) }));
        if (x.sub) grp.appendChild(s("text", { class: "c4-sub", x: 12, y: 50, text: fit(x.sub, 30) }));
      } else {
        grp.appendChild(s("text", { class: "c4-kind", x: 12, y: 18, text: n.role }));
        grp.appendChild(s("text", { class: "c4-name", x: 12, y: 38, text: fit(n.label, 26) }));
        (n.stats || []).forEach(function (line, i) { grp.appendChild(s("text", { class: "c4-sub", x: 12, y: 58 + i * 15, text: fit(line, 34) })); });
        if (n.warn) {
          var bt = n.warn, bw = textW(bt, 10);
          grp.appendChild(s("g", { class: "c4-badge" }, [s("rect", { x: gn.width - bw - 8, y: 7, width: bw, height: 16, rx: 8 }),
            s("text", { x: gn.width - bw / 2 - 8, y: 18.5, "text-anchor": "middle", text: bt })]));
        }
      }
      grp.appendChild(s("title", { text: gn.id }));
      nodeEls[gn.id] = grp;
      if (n) activate(grp, function () { return nodeLinks(gn.id); }, function () { if (o.onNode) o.onNode(gn.id); },
        n.label + ", " + n.role + ". " + (n.stats || []).join(". ") + (n.warn ? ". " + n.warn : ""));
      ng.appendChild(grp);
    });

    svg.appendChild(eg);
    svg.appendChild(ng);
    svg.appendChild(lg);
    D.clear(host);
    host.appendChild(svg);
    var st = state[host.id];
    st.svg = svg; st.width = W; st.height = H;
    resize(host);
    if (!st.observed && global.ResizeObserver) {
      st.observed = true;
      new ResizeObserver(function () { resize(host); }).observe(host);
    }
  }

  // ---- review.sysmap.hints snippet ----------------------------------------------
  // A plain YAML scalar when it is safe (letters, digits and _ . / - only,
  // not a word YAML reads as a bool or null), otherwise a JSON string, which
  // is a valid double-quoted YAML scalar.
  var RESERVED = /^(y|n|yes|no|true|false|on|off|null)$/i;
  function yamlScalar(v) {
    v = String(v);
    return /^[A-Za-z_][A-Za-z0-9_.\/-]*$/.test(v) && !RESERVED.test(v) ? v : JSON.stringify(v);
  }
  // snippet: the whole review.sysmap.hints block (the configured hints plus
  // key -> repo); the YAML map replaces the configured one, so it keeps them.
  function snippet(hints, key, repo) {
    var lines = [{ text: "review:" }, { text: "  sysmap:" }, { text: "    hints:" }];
    (hints || []).forEach(function (x) {
      if (x.key === key) return;
      lines.push({ text: "      " + yamlScalar(x.key) + ": " + yamlScalar(x.repo), kind: "old" });
    });
    lines.push({ text: "      " + yamlScalar(key) + ": " + yamlScalar(repo), kind: "new" });
    return { text: lines.map(function (l) { return l.text; }).join("\n") + "\n", lines: lines };
  }

  global.MapGraph = { draw: draw, model: model, offset: offset, snippet: snippet, yamlScalar: yamlScalar, edgeWidth: edgeWidth, PROTOS: PROTOS, PROTO_PL: PROTO_PL };
})(window);
