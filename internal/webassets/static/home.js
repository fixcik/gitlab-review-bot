// HomePage: the main page "/" (internal/homeweb). The page is rendered on
// the server and works without this script: each hour of a sparkline is an
// SVG group with a <title> (the browser's tooltip) and a cursor that CSS
// shows on hover. The script only writes the hovered hour next to the
// figure's label (.at), as text, and clears it when the pointer leaves.
(function () {
  "use strict";
  function label(svg) {
    var k = svg.closest(".hkpi");
    return k ? k.querySelector(".at") : null;
  }
  function show(e) {
    var g = e.target.closest ? e.target.closest(".spark .h") : null;
    if (!g) return;
    var at = label(g.ownerSVGElement);
    if (at) at.textContent = g.getAttribute("data-label") || "";
  }
  function hide(e) {
    var at = label(e.currentTarget);
    if (at) at.textContent = "";
  }
  Array.prototype.forEach.call(document.querySelectorAll("svg.spark"), function (svg) {
    svg.addEventListener("pointerover", show);
    svg.addEventListener("pointerdown", show);
    svg.addEventListener("pointerleave", hide);
  });
  window.HomePage = { show: show, hide: hide };
})();
