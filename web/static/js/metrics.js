// SPDX-License-Identifier: AGPL-3.0-or-later

// Initializes and live-updates the Metrics page's four chart panels - the
// one place htmx alone isn't enough (CLAUDE.md Frontend Conventions).
(function () {
  var charts = {}; // canvasId -> Chart instance, module-scoped so both
                    // initMetricsChart (full render) and
                    // sparkyMetricsLiveUpdate (in-place tick) can find them.
  var colors = ["#2f5fda", "#1a8a5f", "#b98900", "#c0342c", "#7a3fd1", "#0f8a9e"];

  function baseOptions(yAxisLabel) {
    return {
      // A shared crosshair-style tooltip across every line at the hovered
      // x position, not just the one line directly under the cursor -
      // the "dynamic overlay showing precise value" the Metrics page asks
      // for.
      interaction: { mode: "index", intersect: false },
      plugins: { tooltip: { enabled: true, mode: "index", intersect: false } },
      scales: {
        x: { type: "category", title: { display: true, text: "Time" } },
        y: { type: "linear", min: 0, max: 100, title: { display: true, text: yAxisLabel } }
      }
    };
  }

  function buildDatasets(series) {
    return series.map(function (s, i) {
      return {
        label: s.label,
        data: s.points,
        borderColor: colors[i % colors.length],
        backgroundColor: colors[i % colors.length],
        fill: false,
        tension: 0.2,
        pointRadius: 2
      };
    });
  }

  // initMetricsChart(canvasId, series, yAxisLabel) - called from
  // metrics.html's inline <script> once per panel, on both a full page
  // load and an htmx partial swap into it (htmx executes <script> tags in
  // swapped content by default - allowScriptTags in the vendored
  // htmx.min.js). Always tears down and recreates the Chart.js instance
  // bound to the (now newly-present, previously-detached) canvas element -
  // htmx's swap destroys and recreates the DOM node itself, so a stale
  // Chart object can never be validly updated in place here; that only
  // happens later, via sparkyMetricsLiveUpdate below, when the canvas is
  // known-still-alive.
  window.initMetricsChart = function (canvasId, series, yAxisLabel) {
    if (charts[canvasId]) {
      charts[canvasId].destroy();
    }
    var el = document.getElementById(canvasId);
    if (!el) {
      return;
    }
    charts[canvasId] = new Chart(el, { type: "line", data: { datasets: buildDatasets(series) }, options: baseOptions(yAxisLabel) });
  };

  function updatePanel(canvasId, series) {
    var chart = charts[canvasId];
    if (!chart || !document.getElementById(canvasId)) {
      return;
    }
    chart.data.datasets = buildDatasets(series);
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
