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

  // liveUpdateTimer is a separate debounce timer from refreshTimer above,
  // not a shared one - a topic-relevant transfer_progress/instance_result/
  // instance_health/node_status event still triggers scheduleRefresh's
  // full-page htmx refetch, and could in principle land in the same window
  // as a telemetry event on a page that also has an in-place updater;
  // sharing one timer variable between two different actions (a full-page
  // refetch vs. an in-place redraw) would let one silently cancel or starve
  // the other.
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

  // setConnectionStatus reflects the EventSource's own connection state in
  // the sidebar footer indicator (#sse-status, rendered by base.html and
  // never re-rendered by an htmx partial swap, so this element is stable
  // for the life of the page). EventSource reconnects on its own; its
  // readyState is CONNECTING while retrying and CLOSED only if it has given
  // up entirely (a non-2xx response or wrong content-type from /events).
  function setConnectionStatus(state, label) {
    var el = document.getElementById("sse-status");
    if (!el) {
      return;
    }
    el.setAttribute("data-state", state);
    var labelEl = el.querySelector(".sse-status-label");
    if (labelEl) {
      labelEl.textContent = label;
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    var source = new EventSource("/events");
    source.addEventListener("open", function () {
      setConnectionStatus("live", "Live");
    });
    source.addEventListener("error", function () {
      if (source.readyState === EventSource.CLOSED) {
        setConnectionStatus("down", "Disconnected");
      } else {
        setConnectionStatus("reconnecting", "Reconnecting");
      }
    });
    // Each of these full-page-refetch events is gated on the current page's
    // declared topics - see refreshIfRelevant. instance_health and
    // node_status are published by cmd/sparky-server but had no browser
    // listener before 2026-09-08: Profiles declares instance_health so a
    // live health change shows without a reload, and Nodes / Dashboard
    // declare node_status so a node coming online or dropping does too
    // (agentconn emits it on every agent_status transition).
    source.addEventListener("transfer_progress", refreshIfRelevant("transfer_progress"));
    source.addEventListener("engine_transfer_progress", refreshIfRelevant("engine_transfer_progress"));
    source.addEventListener("instance_result", refreshIfRelevant("instance_result"));
    source.addEventListener("instance_health", refreshIfRelevant("instance_health"));
    source.addEventListener("node_status", refreshIfRelevant("node_status"));
    // A telemetry tick drives whichever pages have an in-place updater
    // (Metrics charts, Dashboard load strips) rather than a full-page
    // refetch - see PLANNING.md's Decisions Log for why those pages
    // deliberately diverge. Falls back to scheduleRefresh only if neither
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
