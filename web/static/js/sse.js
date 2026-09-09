// SPDX-License-Identifier: AGPL-3.0-or-later

// Live-refresh client for GET /events (Dashboard UI Phase 11) - the
// Server-Sent Events channel ARCHITECTURE.md commits to for live telemetry
// and transfer progress. Rather than hand-patching individual DOM nodes per
// event type (a Chart.js point, a progress bar's width, a status badge), a
// relevant event refetches the current page's HTML and morphs it into
// #main-content via idiomorph's htmx "morph" swap extension (vendored in
// base.html, enabled by hx-ext="morph" on <body>) - so the refetch keeps
// scroll position, focus, text selection, and open <details> / <select>
// state instead of destroying and rebuilding the subtree. See PLANNING.md's
// Decisions Log for this phase and its 2026-09-08 "Level A + Tier 0" entry.
// Plain vanilla JS otherwise - CLAUDE.md Frontend Conventions' "minimal
// vanilla JS - no framework" rule, and EventSource's own built-in
// reconnect-with-retry already covers what a hand-rolled reconnect loop
// would otherwise need to.
//
// A refetch only fires when the page currently in #main-content declares
// the incoming event's type in its own data-sse-topics marker (rendered
// inside each page's {{define "content"}} block - internal/httpapi/render.go
// never re-renders <main id="main-content"> on an htmx partial swap, only
// its innerHTML). A page with no marker - every create/edit form, and the
// static pages - never auto-refetches, which is the point: an unrelated
// transfer's progress tick must not blow away a form the user is filling in
// (a native <select> closes the instant its DOM node is removed). See
// PLANNING.md's 2026-09-08 Decisions Log entry.
(function () {
  var refreshTimer = null;
  var refreshDebounceMs = 500;

  // currentPageTopics reads the live-event types the page now in
  // #main-content displays data for, from its data-sse-topics marker
  // (space- or comma-separated). Absent or empty marker -> no topics, so
  // this page never triggers scheduleRefresh.
  function currentPageTopics() {
    var main = document.getElementById("main-content");
    var marker = main ? main.querySelector("[data-sse-topics]") : null;
    var raw = marker ? (marker.getAttribute("data-sse-topics") || "") : "";
    return raw.split(/[\s,]+/).filter(function (topic) {
      return topic.length > 0;
    });
  }

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
      // Drop - not reschedule - the refetch while the user has focus in a
      // form control inside the content area. idiomorph preserves the active
      // element's focus and caret across a morph, but a morph can still
      // churn sibling fields and disrupt an in-progress edit, so skip it
      // entirely until they're done. Matches internal/events.Broker's own
      // "drop rather than block" behavior on a full subscriber buffer.
      var active = document.activeElement;
      if (active && main.contains(active) &&
          (active.tagName === "INPUT" || active.tagName === "SELECT" || active.tagName === "TEXTAREA")) {
        return;
      }
      // morph:innerHTML diffs the response against the live subtree and
      // patches only what changed - see this file's header comment.
      htmx.ajax("GET", window.location.pathname, { target: "#main-content", swap: "morph:innerHTML" });
    }, refreshDebounceMs);
  }

  // refreshIfRelevant returns an SSE listener that calls scheduleRefresh
  // only when the current page declares eventType in its data-sse-topics
  // marker - so a page refetches when its own live data changes and stays
  // put for events about resources it isn't showing.
  function refreshIfRelevant(eventType) {
    return function () {
      if (currentPageTopics().indexOf(eventType) !== -1) {
        scheduleRefresh();
      }
    };
  }

  // metricsUpdateTimer is a separate debounce timer from refreshTimer
  // above, not a shared one - a topic-relevant transfer_progress /
  // instance_result / instance_health event still triggers scheduleRefresh's
  // full-page htmx refetch, and could in principle land in the same window
  // as a telemetry event; sharing one timer variable between two different
  // actions (a full-page refetch vs. an in-place chart update) would let
  // one silently cancel or starve the other.
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
    // Each of these full-page-refetch events is gated on the current page's
    // declared topics - see refreshIfRelevant. instance_health is published
    // by cmd/sparky-server's onMessage but had no browser listener before
    // 2026-09-08; the Profiles page declares it so a live health-status
    // change shows without a manual reload.
    source.addEventListener("transfer_progress", refreshIfRelevant("transfer_progress"));
    source.addEventListener("engine_transfer_progress", refreshIfRelevant("engine_transfer_progress"));
    source.addEventListener("instance_result", refreshIfRelevant("instance_result"));
    source.addEventListener("instance_health", refreshIfRelevant("instance_health"));
    // The Metrics page's own live-update path (web/static/js/metrics.js)
    // replaces just its chart data in place instead of a full-page refetch
    // - see PLANNING.md's Decisions Log for why this page's live-update
    // mechanism deliberately diverges. Falls back to scheduleRefresh when
    // metrics.js hasn't defined sparkyMetricsLiveUpdate; the Metrics page
    // itself carries no data-sse-topics marker, so that fallback only ever
    // no-ops off-page anyway.
    source.addEventListener("telemetry", function () {
      if (typeof window.sparkyMetricsLiveUpdate === "function") {
        scheduleMetricsLiveUpdate();
      } else {
        scheduleRefresh();
      }
    });
  });
})();
