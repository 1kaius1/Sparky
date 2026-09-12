// SPDX-License-Identifier: AGPL-3.0-or-later

// In-place live updater for the Dashboard's "Running instances" load strips
// - the second page (after Metrics) with its own telemetry-driven update
// path instead of the full page refetch every ~5s tick would otherwise
// cause. See web/static/js/sse.js's telemetry listener, and
// web/static/js/metrics.js for the same "self-scoping window.sparky*Update
// function" pattern. Plain vanilla JS - CLAUDE.md Frontend Conventions.
(function () {
  // Bar geometry - deliberately small; this is an at-a-glance strip, not a
  // chart. MAX_BARS matches the server's dashboardLoadBars.
  var BAR_W = 4, BAR_GAP = 1, STRIP_H = 26, MAX_BARS = 24;

  // stripColors reads the active theme's own accent colors live, the same
  // way metrics.js's themeSeriesColors() does - one of 8 user-selectable
  // presets may be active, so these can't be hardcoded literals. Read on
  // each redraw rather than cached: cheap (one redraw per ~5s tick, not per
  // animation frame like metrics.js's crosshair color), and correct
  // immediately if the viewer switches theme without a reload.
  function stripColors() {
    var s = getComputedStyle(document.documentElement);
    return {
      util: s.getPropertyValue("--color-primary").trim(),
      mem: s.getPropertyValue("--color-status-running").trim()
    };
  }

  function clampPct(v) {
    if (typeof v !== "number" || isNaN(v)) return 0;
    return v < 0 ? 0 : v > 100 ? 100 : v;
  }

  // stripSVG renders one metric's recent values as a run of bottom-anchored
  // bars, oldest on the left. Returns an em-dash placeholder when there is
  // no data yet (a just-started instance before its first correlated
  // telemetry tick).
  function stripSVG(values, color) {
    if (!values || !values.length) {
      return '<span class="load-empty">&mdash;</span>';
    }
    var vals = values.length > MAX_BARS ? values.slice(values.length - MAX_BARS) : values;
    var w = vals.length * (BAR_W + BAR_GAP) - BAR_GAP;
    var bars = "";
    for (var i = 0; i < vals.length; i++) {
      var h = Math.max(1, Math.round(clampPct(vals[i]) / 100 * STRIP_H));
      bars += '<rect x="' + (i * (BAR_W + BAR_GAP)) + '" y="' + (STRIP_H - h) +
        '" width="' + BAR_W + '" height="' + h + '" fill="' + color + '"></rect>';
    }
    return '<svg width="' + w + '" height="' + STRIP_H + '" viewBox="0 0 ' + w + ' ' + STRIP_H +
      '" aria-hidden="true"><rect x="0" y="0" width="' + w + '" height="' + STRIP_H +
      '" class="load-strip-track"></rect>' + bars + '</svg>';
  }

  function latestLabel(values) {
    if (!values || !values.length) return "-";
    return Math.round(clampPct(values[values.length - 1])) + "%";
  }

  function rowFor(instanceID) {
    var sel = (window.CSS && CSS.escape) ? CSS.escape(instanceID) : instanceID.replace(/"/g, '\\"');
    return document.querySelector('#dashboard-running tr[data-instance-id="' + sel + '"]');
  }

  function applyRow(inst, colors) {
    var tr = rowFor(inst.id);
    if (!tr) return;
    var us = tr.querySelector('[data-role="util-strip"]');
    var uv = tr.querySelector('[data-role="util-val"]');
    var ms = tr.querySelector('[data-role="mem-strip"]');
    var mv = tr.querySelector('[data-role="mem-val"]');
    var up = tr.querySelector('[data-role="uptime"]');
    if (us) us.innerHTML = stripSVG(inst.gpuUtil, colors.util);
    if (uv) uv.textContent = latestLabel(inst.gpuUtil);
    if (ms) ms.innerHTML = stripSVG(inst.gpuMem, colors.mem);
    if (mv) mv.textContent = latestLabel(inst.gpuMem);
    if (up && inst.uptime) up.textContent = inst.uptime;
  }

  // sparkyDashboardRenderAll - (re)draw every row from a full payload (same
  // shape as GET /dashboard/live-data). Called by dashboard.html's own
  // inline <script> on load/swap to seed the strips, and by
  // sparkyDashboardLiveUpdate on each telemetry tick.
  window.sparkyDashboardRenderAll = function (data) {
    if (!data || !data.instances) return;
    var colors = stripColors();
    data.instances.forEach(function (inst) { applyRow(inst, colors); });
  };

  // sparkyDashboardLiveUpdate - fetch current load data and apply it in
  // place. Self-scoping: a no-op on any page without the table (this file
  // is loaded globally, like metrics.js). If the set of instance rows has
  // changed since the page was rendered (a load/unload landed between the
  // instance_result event and this tick), fall through to a morph-based
  // refetch so the table structure catches up - instance_result already
  // drives that refetch, this is just a safety net for a tick that beats
  // it.
  window.sparkyDashboardLiveUpdate = function () {
    var table = document.getElementById("dashboard-running");
    if (!table) return;
    fetch("/dashboard/live-data").then(function (resp) {
      return resp.ok ? resp.json() : null;
    }).then(function (data) {
      if (!data || !data.instances) return;
      var domIDs = [].slice.call(table.querySelectorAll("tr[data-instance-id]"))
        .map(function (el) { return el.getAttribute("data-instance-id"); }).sort().join(",");
      var dataIDs = data.instances.map(function (i) { return i.id; }).sort().join(",");
      if (domIDs !== dataIDs) {
        if (typeof htmx !== "undefined") {
          htmx.ajax("GET", window.location.pathname, { target: "#main-content", swap: "morph:innerHTML" });
        }
        return;
      }
      window.sparkyDashboardRenderAll(data);
    });
  };
})();
