// SPDX-License-Identifier: AGPL-3.0-or-later

// Live-refresh client for GET /events (Dashboard UI Phase 11) - the
// Server-Sent Events channel ARCHITECTURE.md commits to for live telemetry
// and transfer progress. Deliberately minimal: rather than patching
// specific DOM nodes per event type (a Chart.js point, a progress bar's
// width, a status badge), a relevant event triggers an htmx refetch of the
// currently visible page - see PLANNING.md's Decisions Log for this phase.
// Plain vanilla JS, no htmx extension - CLAUDE.md Frontend Conventions'
// "minimal vanilla JS - no framework" rule, and EventSource's own built-in
// reconnect-with-retry already covers what a hand-rolled reconnect loop
// would otherwise need to.
//
// "Relevant" is per-page: a page's content-block wrapper carries a
// data-sse-topics attribute listing the event types whose data that page
// actually displays (see web/templates/pages/*.html - only list pages have
// one; a create/edit form page has none and so never auto-refreshes). An
// event whose type isn't in the current page's list is ignored here. See
// FIX_REFRESH.md for the full rationale - this closed a real bug where any
// transfer/instance event anywhere in the fleet refreshed every open page,
// including a form the user was mid-way through, closing its open <select>.
(function () {
  var refreshTimer = null;
  var refreshDebounceMs = 500;

  // pageWantsTopic reports whether the currently rendered page has declared
  // it displays live data for this event type. No data-sse-topics element
  // present (the common case - every form page, and several read pages)
  // means "wants nothing", the safe default.
  function pageWantsTopic(topic) {
    var marker = document.querySelector("#main-content [data-sse-topics]");
    if (!marker) {
      return false;
    }
    return marker.getAttribute("data-sse-topics").split(/\s+/).indexOf(topic) !== -1;
  }

  // isEditingFormControl reports whether the user currently has focus in a
  // text/select control inside the main pane - swapping #main-content out
  // from under an open <select> or a half-typed field is hostile. A focused
  // <button> is deliberately not counted: a Load/Unload button holding
  // focus should not block its own list from updating.
  function isEditingFormControl() {
    var el = document.activeElement;
    if (!el) {
      return false;
    }
    var tag = el.tagName;
    if (tag !== "INPUT" && tag !== "SELECT" && tag !== "TEXTAREA") {
      return false;
    }
    var main = document.getElementById("main-content");
    return main !== null && main.contains(el);
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
      // Drop (do not re-queue) a refresh that would land while the user is
      // mid-interaction with a form control - same debounce-and-drop
      // behavior this file already uses, and internal/events.Broker's own
      // full-buffer drop. The page catches up on the next event once the
      // control loses focus.
      if (isEditingFormControl()) {
        return;
      }
      var main = document.getElementById("main-content");
      if (!main) {
        return;
      }
      htmx.ajax("GET", window.location.pathname, { target: "#main-content", swap: "innerHTML" });
    }, refreshDebounceMs);
  }

  // refreshIfRelevant is the topic-gated entry point every event listener
  // below uses - scheduleRefresh itself stays topic-agnostic so the
  // telemetry path (which never calls it directly) is unaffected.
  function refreshIfRelevant(topic) {
    return function () {
      if (pageWantsTopic(topic)) {
        scheduleRefresh();
      }
    };
  }

  // liveUpdateTimer is a separate debounce timer from refreshTimer above,
  // not a shared one - a topic-relevant transfer_progress/instance_result/
  // instance_health event still triggers scheduleRefresh's full-page htmx
  // refetch, and could in principle land in the same window as a telemetry
  // event on a page that also has an in-place updater; sharing one timer
  // variable between two different actions (a full-page refetch vs. an
  // in-place redraw) would let one silently cancel or starve the other.
  var liveUpdateTimer = null;

  // liveUpdaters are the in-place, per-page telemetry consumers -
  // window.sparkyMetricsLiveUpdate (Metrics page charts) and
  // window.sparkyDashboardLiveUpdate (Dashboard load strips). Each is
  // defined globally (its script is loaded on every page) and self-scopes
  // by checking for its own DOM anchor, so calling every one that exists
  // on each tick is safe - the one whose page isn't showing no-ops.
  function liveUpdaters() {
    return [window.sparkyMetricsLiveUpdate, window.sparkyDashboardLiveUpdate].filter(function (fn) {
      return typeof fn === "function";
    });
  }

  function scheduleLiveUpdate() {
    if (liveUpdateTimer !== null) {
      return;
    }
    liveUpdateTimer = window.setTimeout(function () {
      liveUpdateTimer = null;
      if (document.visibilityState !== "visible") {
        return;
      }
      liveUpdaters().forEach(function (fn) { fn(); });
    }, refreshDebounceMs);
  }

  document.addEventListener("DOMContentLoaded", function () {
    var source = new EventSource("/events");
    source.addEventListener("transfer_progress", refreshIfRelevant("transfer_progress"));
    source.addEventListener("engine_transfer_progress", refreshIfRelevant("engine_transfer_progress"));
    source.addEventListener("instance_result", refreshIfRelevant("instance_result"));
    // instance_health is published server-side (cmd/sparky-server/main.go's
    // onMessage) but had no listener here until this topic-scoping work -
    // the Model profiles page's Health column now updates live, gated the
    // same per-page way as the events above.
    source.addEventListener("instance_health", refreshIfRelevant("instance_health"));
    // A telemetry tick drives whichever pages have an in-place updater
    // (Metrics charts, Dashboard load strips) rather than a full-page
    // refetch - see PLANNING.md's Decisions Log for why those pages
    // deliberately diverge. Falls back to scheduleRefresh only if no
    // in-place updater is defined at all (not expected - both scripts load
    // globally - but keeps a page that lists telemetry-derived data
    // self-updating if one is ever removed).
    source.addEventListener("telemetry", function () {
      if (liveUpdaters().length > 0) {
        scheduleLiveUpdate();
      } else {
        scheduleRefresh();
      }
    });
  });
})();
