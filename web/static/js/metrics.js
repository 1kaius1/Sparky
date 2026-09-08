// SPDX-License-Identifier: AGPL-3.0-or-later

// Initializes and live-updates the Metrics page's four chart panels - the
// one place htmx alone isn't enough (CLAUDE.md Frontend Conventions).
(function () {
  var charts = {}; // canvasId -> Chart instance, module-scoped so both
                    // initMetricsChart (full render) and
                    // sparkyMetricsLiveUpdate (in-place tick) can find them.
  var colors = ["#2f5fda", "#1a8a5f", "#b98900", "#c0342c", "#7a3fd1", "#0f8a9e"];

  // formatTimeOfDay renders a point's real Unix-milliseconds x value as a
  // "HH:MM:SS" string in the viewer's own local timezone, via plain JS
  // Date - no date-parsing library needed. Deliberately viewer-local, not
  // the server's timezone the old server-formatted string used - a real,
  // acknowledged behavior change (see chartPoint's own doc comment in
  // internal/httpapi/metrics.go).
  function formatTimeOfDay(ms) {
    var d = new Date(ms);
    function pad(n) { return n < 10 ? "0" + n : "" + n; }
    return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }

  // crosshairPlugin draws a single vertical line at the hovered x position
  // across every panel - Chart.js's own interaction/tooltip "x" mode
  // (see baseOptions below) already synchronizes the tooltip box across
  // series at that position, but draws no line of its own. Registered once,
  // globally, so it applies to every Chart instance this file creates with
  // no per-chart option needed.
  var crosshairPlugin = {
    id: "sparkyCrosshair",
    afterDatasetsDraw: function (chart) {
      var active = chart.tooltip && chart.tooltip._active;
      if (!active || !active.length) {
        return;
      }
      var x = active[0].element.x;
      var area = chart.chartArea;
      var ctx = chart.ctx;
      ctx.save();
      ctx.beginPath();
      ctx.moveTo(x, area.top);
      ctx.lineTo(x, area.bottom);
      ctx.lineWidth = 1;
      // Matches main.css's --color-text-muted - a CSS custom property
      // isn't reachable from a bare canvas 2D context, so the value is
      // duplicated here rather than read at runtime.
      ctx.strokeStyle = "#667085";
      ctx.stroke();
      ctx.restore();
    }
  };
  Chart.register(crosshairPlugin);

  // minPxPerSlot/minSlots/maxSlots are reasonable defaults for "readable
  // spacing," not measured values - same honesty precedent as this
  // project's other unmeasured-headroom constants (e.g.
  // internal/db.recentMetricsSafetyCap).
  var minPxPerSlot = 6;
  var minSlots = 20;
  var maxSlots = 180;

  // targetSlotCount computes how many points a panel's canvas can show
  // with reasonable spacing, from its own actual rendered width. Now used
  // purely as a decimation ceiling (see fitSeriesToWidth) - it no longer
  // determines a padding target, since the x-axis is a real linear time
  // scale, not a category scale needing every series to share one set of
  // slots.
  function targetSlotCount(canvasEl) {
    var width = canvasEl.clientWidth || 0;
    var slots = Math.floor(width / minPxPerSlot);
    return Math.max(minSlots, Math.min(maxSlots, slots || minSlots));
  }

  // fitSeriesToWidth stride-decimates one series' already-hour-bounded,
  // already-chronological real points down to targetCount when there are
  // more of them than a panel can usefully show - simple and
  // deterministic, not LTTB/min-max bucketing; readable spacing is the
  // goal here, not peak fidelity. A sparse series (fewer real points than
  // targetCount) is returned unchanged, with no padding - the x-axis is a
  // real linear time scale (see baseOptions), so a sparse series simply
  // plots its own real points at their own real x position, correctly
  // overlaid against any other series sharing the same panel by actual
  // time value, not by a shared, artificially-padded slot count. This
  // replaces an earlier category-axis design whose per-series padding
  // could not actually align two nodes' real, non-coincident timestamps -
  // see chartPoint's doc comment in internal/httpapi/metrics.go for the
  // real bug that caused.
  function fitSeriesToWidth(points, targetCount) {
    if (points.length <= targetCount) {
      return points;
    }
    var step = points.length / targetCount;
    var decimated = [];
    for (var j = 0; j < targetCount; j++) {
      decimated.push(points[Math.floor(j * step)]);
    }
    return decimated;
  }

  // yMax fixes the y-axis ceiling at 100 for a percentage panel (GPU/CPU
  // utilization); omitted (undefined) for an absolute-value panel (memory,
  // in MB) so Chart.js auto-scales to whatever the data actually spans -
  // a fixed 0-100 range would be meaningless once the unit isn't a
  // percentage.
  function baseOptions(yAxisLabel, yMax) {
    var y = { type: "linear", min: 0, title: { display: true, text: yAxisLabel } };
    if (typeof yMax === "number") {
      y.max = yMax;
    }
    return {
      // "x" mode (not "index") - matches every dataset's point nearest the
      // hovered pixel's real x value, rather than the point at the same
      // array index in each dataset. "index" mode was the right choice
      // back when every series was padded to share one identical set of
      // category slots (so "same index" and "same real time" always
      // agreed); now that the x-axis is a real linear time scale and a
      // sparse series is left unpadded (see fitSeriesToWidth), two
      // series' points at the same array index are not generally at the
      // same real time, so "index" mode would mismatch the tooltip.
      interaction: { mode: "x", intersect: false },
      plugins: { tooltip: { enabled: true, mode: "x", intersect: false } },
      scales: {
        x: {
          // Real linear time scale (Unix milliseconds, see chartPoint's
          // doc comment in internal/httpapi/metrics.go) - not "category".
          // A category scale can only align two series by identical label
          // values, which is exactly what caused the original bug: two
          // nodes polling telemetry on independent 5-second intervals
          // almost never produce identical formatted timestamps, so their
          // real, non-coincident readings rendered as two abutting blocks
          // instead of one interleaved timeline. A linear scale plots
          // every series' real x value directly, so multiple nodes'
          // series correctly overlay by actual time regardless of
          // whether their poll ticks ever coincide.
          type: "linear",
          title: { display: true, text: "Time" },
          ticks: {
            callback: function (value) { return formatTimeOfDay(value); }
          }
        },
        y: y
      }
    };
  }

  function buildDatasets(series, targetCount) {
    return series.map(function (s, i) {
      return {
        label: s.label,
        data: fitSeriesToWidth(s.points, targetCount),
        borderColor: colors[i % colors.length],
        backgroundColor: colors[i % colors.length],
        fill: false,
        tension: 0.2,
        pointRadius: 2
      };
    });
  }

  // initMetricsChart(canvasId, series, yAxisLabel, yMax) - called from
  // metrics.html's inline <script> once per panel, on both a full page
  // load and an htmx partial swap into it (htmx executes <script> tags in
  // swapped content by default - allowScriptTags in the vendored
  // htmx.min.js). Always tears down and recreates the Chart.js instance
  // bound to the (now newly-present, previously-detached) canvas element -
  // htmx's swap destroys and recreates the DOM node itself, so a stale
  // Chart object can never be validly updated in place here; that only
  // happens later, via sparkyMetricsLiveUpdate below, when the canvas is
  // known-still-alive.
  window.initMetricsChart = function (canvasId, series, yAxisLabel, yMax) {
    if (charts[canvasId]) {
      charts[canvasId].destroy();
    }
    var el = document.getElementById(canvasId);
    if (!el) {
      return;
    }
    var targetCount = targetSlotCount(el);
    charts[canvasId] = new Chart(el, { type: "line", data: { datasets: buildDatasets(series, targetCount) }, options: baseOptions(yAxisLabel, yMax) });
  };

  function updatePanel(canvasId, series) {
    var chart = charts[canvasId];
    var el = document.getElementById(canvasId);
    if (!chart || !el) {
      return;
    }
    chart.data.datasets = buildDatasets(series, targetSlotCount(el));
    chart.update("none"); // "none" mode = no animation, no visible redraw
  }

  // sparkyMetricsLiveUpdate - fetches /metrics/chart-data and updates every
  // stored chart in place, avoiding the visible redraw a full htmx page
  // refetch would cause on every ~5s telemetry tick (web/static/js/sse.js).
  // Defensive DOM check first, before even firing the fetch: this function
  // is defined globally regardless of which page is currently visible, and
  // sse.js's SSE connection stays open across htmx partial-swap navigation
  // (this app's sidebar/SSE-never-reload model), so a telemetry tick can
  // arrive while the user is no longer on the Metrics page. If the
  // expected canvases are gone, any charts[] entries are stale references
  // to detached canvases from the last time Metrics was open; clear them
  // (no .destroy() needed - the canvas is already gone, nothing left to
  // detach a listener from) so a later revisit's initMetricsChart call
  // doesn't find a bogus non-null entry and skip creating a fresh one.
  window.sparkyMetricsLiveUpdate = function () {
    var ids = ["metrics-gpu-util-chart", "metrics-gpu-mem-chart", "metrics-mem-chart", "metrics-cpu-chart"];
    var present = ids.every(function (id) { return document.getElementById(id) !== null; });
    if (!present) {
      charts = {};
      return;
    }
    fetch("/metrics/chart-data").then(function (resp) {
      if (!resp.ok) {
        return null;
      }
      return resp.json();
    }).then(function (data) {
      if (!data) {
        return;
      }
      updatePanel("metrics-gpu-util-chart", data.gpuUtilization);
      updatePanel("metrics-gpu-mem-chart", data.gpuMemory);
      updatePanel("metrics-mem-chart", data.systemMemory);
      updatePanel("metrics-cpu-chart", data.cpu);
    });
  };
})();
