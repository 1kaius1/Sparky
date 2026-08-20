// SPDX-License-Identifier: AGPL-3.0-or-later

// Live-refresh client for GET /events (Dashboard UI Phase 11) - the
// Server-Sent Events channel ARCHITECTURE.md commits to for live telemetry
// and transfer progress. Deliberately minimal: rather than patching
// specific DOM nodes per event type (a Chart.js point, a progress bar's
// width, a status badge), a relevant event simply triggers an htmx refetch
// of whatever page is currently visible - see PLANNING.md's Decisions Log
// for this phase. Plain vanilla JS, no htmx extension - CLAUDE.md Frontend
// Conventions' "minimal vanilla JS - no framework" rule, and EventSource's
// own built-in reconnect-with-retry already covers what a hand-rolled
// reconnect loop would otherwise need to.
(function () {
  var refreshTimer = null;
  var refreshDebounceMs = 500;

  function scheduleRefresh() {
    if (refreshTimer !== null) {
      return;
    }
    refreshTimer = window.setTimeout(function () {
      refreshTimer = null;
      // A backgrounded tab still receives SSE messages - no reason to
      // spend a request re-rendering content nobody is looking at.
      if (document.visibilityState !== "visible") {
        return;
      }
      var main = document.getElementById("main-content");
      if (!main) {
        return;
      }
      htmx.ajax("GET", window.location.pathname, { target: "#main-content", swap: "innerHTML" });
    }, refreshDebounceMs);
  }

  // metricsUpdateTimer is a separate debounce timer from refreshTimer
  // above, not a shared one - transfer_progress/instance_result still
  // trigger scheduleRefresh's full-page htmx refetch unconditionally, and
  // could in principle land in the same window as a telemetry event;
  // sharing one timer variable between two different actions (a full-page
  // refetch vs. an in-place chart update) would let one silently cancel
  // or starve the other.
  var metricsUpdateTimer = null;

  function scheduleMetricsLiveUpdate() {
    if (metricsUpdateTimer !== null) {
      return;
    }
    metricsUpdateTimer = window.setTimeout(function () {
      metricsUpdateTimer = null;
      if (document.visibilityState !== "visible") {
        return;
      }
      window.sparkyMetricsLiveUpdate();
    }, refreshDebounceMs);
  }

  document.addEventListener("DOMContentLoaded", function () {
    var source = new EventSource("/events");
    source.addEventListener("transfer_progress", scheduleRefresh);
    source.addEventListener("engine_transfer_progress", scheduleRefresh);
    source.addEventListener("instance_result", scheduleRefresh);
    // The Metrics page's own live-update path (web/static/js/metrics.js)
    // replaces just its chart data in place instead of the full-page
    // refetch every other page still uses for this event - see
    // PLANNING.md's Decisions Log for why this page's live-update
    // mechanism deliberately diverges. Falls back to scheduleRefresh when
    // metrics.js hasn't defined sparkyMetricsLiveUpdate (every other
    // page's unchanged behavior).
    source.addEventListener("telemetry", function () {
      if (typeof window.sparkyMetricsLiveUpdate === "function") {
        scheduleMetricsLiveUpdate();
      } else {
        scheduleRefresh();
      }
    });
  });
})();
