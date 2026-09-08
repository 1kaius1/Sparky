# Fix: SSE-driven full-page refresh closes open form controls

Assessment and implementation context for a real bug found during manual
end-to-end testing against two real Sparks (2026-09-08) - not a task list to
check off blindly, but everything needed to actually do this work correctly,
including the parts that still need a decision before writing code.

---

## Symptom (as reported)

A `<select>` dropdown on a form page (e.g. "New model transfer"'s
destination-node picker) opens and then closes almost immediately - even
left alone with no further interaction, it will always eventually close.
Observed while two real Sparks were actively connected and reporting
telemetry/transfer progress.

## Root cause

`web/static/js/sse.js` wires three SSE event types directly to an
unconditional, unscoped full-pane refresh, regardless of which page is open
or whether that event has anything to do with it:

```js
source.addEventListener("transfer_progress", scheduleRefresh);
source.addEventListener("engine_transfer_progress", scheduleRefresh);
source.addEventListener("instance_result", scheduleRefresh);
```

`scheduleRefresh` (same file) does:

```js
htmx.ajax("GET", window.location.pathname, { target: "#main-content", swap: "innerHTML" });
```

This destroys and rebuilds the entire `#main-content` subtree - including
whatever form and open `<select>` the user currently has on screen - on
*any* matching event from *anywhere in the fleet*, not just ones relevant to
the page being viewed. A native browser `<select>` closes immediately when
its underlying DOM node is removed, which is exactly the reported symptom.
It is not timing-sensitive or something a user can out-race: transfer
progress is reported periodically, not just at completion
(`docs/AGENT.md` Service Architecture Notes - "Progress is streamed back
periodically, not just on completion"), so the *next* tick within
`scheduleRefresh`'s 500ms debounce window will always eventually land and
close it.

`telemetry` events are **not** part of this bug - `metrics.js` is loaded
globally (`web/templates/layouts/base.html` line 11) and defines
`window.sparkyMetricsLiveUpdate` on every page; `sse.js`'s `telemetry`
listener always calls that function instead of `scheduleRefresh`, and that
function itself no-ops harmlessly on any page that isn't Metrics (its own
canvas-presence check fails and it returns early). Confirmed by reading
`web/static/js/metrics.js` end to end - not assumed.

### Why this matters beyond the dropdown

Even ignoring the dropdown-closing symptom, the current behavior is wrong on
its own terms: viewing an unrelated page (say, a create-profile form, or a
page for a completely different resource) gets a live re-render triggered by
an event about a transfer/instance the user isn't even looking at. The user
has already confirmed (in the conversation that produced this document) that
scoping refreshes to the currently relevant page is required independent of
the dropdown bug, not just a byproduct fix.

---

## The event pipeline today (for context, not guesswork)

Read directly from the code, not assumed:

- `internal/events.Event` is `{Type string}` - deliberately minimal. Its own
  doc comment states node/entity IDs were left out because the original
  design "refetches the current page rather than patching a specific
  row/entity," so carrying IDs "would be unused plumbing." That reasoning
  no longer holds once page-relevance scoping is added - see Open
  Questions below.
- Every `Publish` call site is `cmd/sparky-server/main.go`'s `onMessage`
  closure (lines ~178-198), one per `agentproto` message type:
  `TypeTransferProgress`, `TypeEngineTransferProgress`, `TypeInstanceResult`,
  `TypeInstanceHealth`, `TypeTelemetry`. **`nodeID` is already in scope at
  every one of these call sites** (it's `onMessage`'s own first parameter)
  but is currently discarded - only `env.Type` is published. Adding it to
  the event, if a future pass wants per-node (not just per-page-type)
  scoping, is a small, localized change - no protocol or agent change
  needed at all, this is purely server-internal plumbing.
- **`instance_health` is published to the broker but nothing subscribes to
  it in the browser** - `sse.js` has no `addEventListener("instance_health",
  ...)` at all today. This is a separate, real gap from the refresh-scoping
  bug: a live health-status change on the Profiles page currently requires a
  manual reload to see. Worth fixing in the same pass since defining
  Profiles' topic set properly requires deciding whether health updates are
  in scope (see the per-page table below).
- `internal/httpapi/render.go`'s `render` helper: for a plain navigation,
  the full `base` template renders (sidebar, `<main id="main-content">`,
  everything); for an `HX-Request: true` request (what `htmx.ajax` sends,
  including `scheduleRefresh`'s own calls), **only the page's own `content`
  block renders** - `<main id="main-content">` itself, defined in
  `base.html`, is never part of that response. This matters directly for
  where a page-relevance marker can live - see Open Questions.

---

## Proposed fix

Two sub-problems, each with a standard, idiomatic solution - not the same
problem, both need addressing:

### 1. Page-relevance scoping ("only refresh when relevant")

**Chosen approach: per-page topic tagging**, not per-resource-ID matching.
A page declares which event *types* it displays live data for; `sse.js`
only calls `scheduleRefresh` when the current page's declared topics
include the incoming event's type. This matches the project's existing
"coarse full-page refetch, not fine-grained patching" philosophy (see
`internal/events.Event`'s own doc comment) rather than introducing a
heavier per-node/per-resource-ID design - a create/edit **form** page
simply declares no topics at all, since it displays no live event-driven
data regardless of which node or transfer the event concerns.

Rejected alternative: per-node/resource-ID matching (tagging events with
`nodeID` and matching against a page's displayed node(s)). More precise,
but doesn't actually solve the reported case on its own - a "New model
transfer" *form* has no relationship to any specific transfer's progress at
all, node-scoped or not, so form pages still need to be excluded entirely
either way. Kept as a future refinement if a list page ever needs to ignore
irrelevant *rows* within a topic it otherwise cares about (e.g. Transfers
list ignoring another node's transfer specifically) - not needed to close
this bug.

### 2. Protecting active user input from a *relevant* refresh

Not usually solved with per-field async calls (that's the autosave/inline-
validation pattern, solving a different problem: persisting partial input,
not protecting live DOM state from an external refresh). The standard
solutions for this exact problem in a server-rendered/htmx app:

- **`hx-preserve`** (htmx's own attribute) - marks an element to be moved
  intact (with its live DOM state: focus, open dropdown, typed text, scroll
  position) from the old tree into the new one during a swap, instead of
  being replaced. The htmx-idiomatic answer.
- **A focus guard in `scheduleRefresh`** - skip (drop, don't queue) the
  scheduled refresh if `document.activeElement` is a form control
  (`INPUT`/`SELECT`/`TEXTAREA`) inside `#main-content`. Matches this
  codebase's existing "drop rather than block/queue" philosophy already
  used in `internal/events.Broker.Publish` (a full subscriber buffer drops
  the event rather than blocking the publisher).

**Recommendation: do both.** They cover different cases - `hx-preserve`
protects a specific known-important element across a swap that does
proceed; the focus guard prevents the swap from happening at all while the
user is actively engaged with any control, which is simpler and broader but
means the rest of the page also stays stale until they're done. Given list
pages (the only pages that will have any topics at all) don't generally
have persistent open dropdowns the way create/edit forms do, the focus
guard alone may be sufficient in practice - but `hx-preserve` is cheap
insurance for the one case it doesn't cover (a list page's own filter/sort
control, if one is ever added).

---

## Implementation plan

1. **Decide where the per-page topics marker lives** (see Open Questions -
   this must be resolved before writing template changes, since
   `<main id="main-content">` in `base.html` is never re-rendered by the
   htmx partial swap `scheduleRefresh` performs - only its innerHTML is
   replaced with each page's own `content` block).
2. Add the topics marker to every page template's `content` block per the
   table below.
3. `web/static/js/sse.js`:
   - Add a helper reading the current page's declared topics from the
     marker.
   - Change each of the three `addEventListener` calls to check topics
     before calling `scheduleRefresh`, instead of calling it unconditionally.
   - Add the missing `instance_health` listener (see per-page table -
     needed if Profiles is to correctly reflect live health changes).
   - Add the `document.activeElement` focus guard inside `scheduleRefresh`.
4. Apply `hx-preserve` to any page element judged worth it once the topic
   tagging is in place (likely none needed immediately - re-evaluate after
   step 3, since most of the risk is already removed once forms declare no
   topics).
5. Update `CHANGELOG.md` (`### Fixed`) and `PLANNING.md` (Decisions Log +
   removing/narrowing the relevant Known Issues row, if one gets filed
   before this lands) per this project's normal change process.
6. Tests: `sse.js` has no existing test coverage (no JS test framework in
   this project - CLAUDE.md Frontend Conventions, "no build step"). Manual
   browser verification is the applicable gate here, same precedent as the
   Metrics chart's own crosshair/right-justify claims (PLANNING.md's
   2026-08-20 entry) - plan to verify by hand: open a create-form page,
   trigger a transfer/instance event elsewhere in the fleet, confirm the
   form's dropdown survives and the rest of the page does not unexpectedly
   refresh.

### Per-page topic assignment

Based on what each page actually displays; entries marked "needs
confirmation" are a best-effort read of the template, not confirmed against
the user's own intent - confirm before implementing.

| Page template | Topics | Notes |
|---|---|---|
| `dashboard.html` | `instance_result` | Shows running-instance counts and a recent-instances table. Node online/offline counts (`OnlineNodes`) have no corresponding published event type today - agent connect/disconnect isn't one of `onMessage`'s five cases - so this page still won't live-update on that specific stat. Existing gap, out of scope for this fix. |
| `nodes.html` | none today | Same agent-connect/disconnect gap as above - `agent_status` changes aren't broadcast as an event type at all currently. Out of scope unless the user wants it added as part of this pass. |
| `profiles.html` | `instance_result`, `instance_health` | Health requires adding the missing `instance_health` `sse.js` listener (see above) - needs confirmation this is wanted in the same pass, since it's a real feature gap, not just a scoping fix. |
| `transfers.html` | `transfer_progress` | |
| `engine_transfers.html` | `engine_transfer_progress` | |
| `engine_inventory.html` | `engine_transfer_progress` (needs confirmation) | A completed engine transfer changes what this page lists; worth confirming this is actually wanted live vs. acceptable to require a manual revisit. |
| `metrics.html` | handled separately | Already uses its own `sparkyMetricsLiveUpdate` in-place update path (`telemetry` event), not `scheduleRefresh` - no topics tag needed. |
| `audit.html`, `users.html`, `settings.html` | none | No live SSE-driven data. |
| `register_node.html`, `profile_form.html`, `initiate_transfer.html`, `provision_engine.html`, `create_local_account.html`, `account.html` | none | All create/edit forms - the exact class of page the reported bug hit. Never auto-refresh, regardless of event type. |
| `node_registered.html` | none | One-time bearer-token display page - refreshing it would be actively harmful even if it were otherwise safe, since the token is shown only once and a re-fetch of this route does not re-show it. |
| `forbidden.html` | none | Static. |

---

## Open questions (resolve before implementing)

1. **Where does the per-page topics marker actually live?** Confirmed via
   `internal/httpapi/render.go`: for an `HX-Request: true` request (what
   `scheduleRefresh` sends), only the page's own `{{define "content"}}`
   block renders - `<main id="main-content">` in `base.html` is not
   re-rendered by a partial swap, only its innerHTML replaced. So the
   marker must live inside each page's own `content` block (e.g. a
   `data-sse-topics="..."` attribute on that block's own root wrapper
   element, or a small dedicated marker element at its top), not on
   `#main-content` itself. Several page templates (e.g.
   `initiate_transfer.html`) currently start directly with an `<h1>` and no
   wrapping element - adding one is a small structural change to most page
   templates, worth doing consistently rather than ad hoc per page.
2. **Is extending `sse.js` to also listen for `instance_health` in scope
   for this pass**, or should Profiles' topic list omit it and this gets
   filed as its own separate follow-up? Recommended: include it, since
   defining Profiles' topic set at all means confronting this gap directly,
   but confirm before implementing since it's a small feature addition, not
   purely a bug fix.
3. **`engine_inventory.html`'s topic** - confirm whether a completed engine
   transfer should live-update this page or whether a manual revisit is
   acceptable.
4. **Node online/offline live-updating** (`dashboard.html`/`nodes.html`) -
   confirmed out of scope for this pass (no event type exists for it today),
   but worth deciding whether to file it as its own follow-up item now
   while it's fresh, or leave it undiscovered until someone notices.
