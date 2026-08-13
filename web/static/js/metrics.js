// SPDX-License-Identifier: AGPL-3.0-or-later

// Initializes the Metrics page's GPU utilization chart - the one place
// htmx alone isn't enough (CLAUDE.md Frontend Conventions). Called
// directly from a <script> tag inside metrics.html's own content block,
// so it runs both on a full page load and on an htmx partial swap into
// it (htmx executes <script> tags in swapped content by default -
// allowScriptTags in the vendored htmx.min.js).
function initMetricsChart(canvasId, series) {
  var colors = ["#2f5fda", "#1a8a5f", "#b98900", "#c0342c", "#7a3fd1", "#0f8a9e"];
  var datasets = series.map(function (s, i) {
    return {
      label: s.nodeName,
      data: s.points,
      borderColor: colors[i % colors.length],
      backgroundColor: colors[i % colors.length],
      fill: false,
      tension: 0.2,
      pointRadius: 2
    };
  });

  new Chart(document.getElementById(canvasId), {
    type: "line",
    data: { datasets: datasets },
    options: {
      scales: {
        x: { type: "category", title: { display: true, text: "Time" } },
        y: { type: "linear", min: 0, max: 100, title: { display: true, text: "GPU utilization %" } }
      }
    }
  });
}
