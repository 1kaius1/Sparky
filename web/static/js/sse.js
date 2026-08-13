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

  document.addEventListener("DOMContentLoaded", function () {
    var source = new EventSource("/events");
    source.addEventListener("transfer_progress", scheduleRefresh);
    source.addEventListener("instance_result", scheduleRefresh);
    source.addEventListener("telemetry", scheduleRefresh);
  });
})();
