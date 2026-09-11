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

**Decision: focus guard only** (see Decisions 5 below). Topic scoping
already fixes the reported bug outright - the form pages it happened on
declare no topics and so never auto-refresh. The focus guard is added as
forward-looking defence-in-depth for the topic-declaring pages; none of
them have an interactive control today. `hx-preserve` is deferred until a
concrete element needs to survive a refresh that *does* proceed.

---

## Implementation plan

1. Wrap the `content` block of the 5 topic-declaring templates
   (`transfers`, `engine_transfers`, `engine_inventory`, `dashboard`,
   `profiles`) in `<div data-sse-topics="...">...</div>` per the table
   below. No other template changes.
2. `web/static/js/sse.js`:
   - Add a `pageWantsTopic(topic)` helper: read
     `#main-content [data-sse-topics]`, split its value on whitespace,
     return whether `topic` is in the list; return `false` if no such
     element exists.
   - Wrap the three existing `transfer_progress` /
     `engine_transfer_progress` / `instance_result` listeners so they call
     `scheduleRefresh` only when `pageWantsTopic(<that type>)` is true.
   - Add a fourth listener for `instance_health`, gated the same way.
   - Leave the `telemetry` listener alone - it already routes through
     `sparkyMetricsLiveUpdate`, which self-scopes via its canvas-presence
     check.
   - Add the focus guard inside `scheduleRefresh`'s debounced callback:
     after the `visibilityState` check, `return` early if
     `document.activeElement` is an `INPUT`/`SELECT`/`TEXTAREA` contained
     in `#main-content`.
3. `PLANNING.md`: add a Known Issues row for the node online/offline
   live-update gap (Decision 4), and a Decisions Log entry for this fix.
4. `CHANGELOG.md`: a `### Fixed` entry.
5. Tests: `sse.js` has no existing test coverage (no JS test framework in
   this project - CLAUDE.md Frontend Conventions, "no build step").
   `go build ./... && go vet ./... && gofmt -l . && go test ./...` still
   applies (template parse errors surface via the `internal/httpapi`
   template-loading tests). Manual browser verification is the real gate,
   same precedent as the Metrics chart's own crosshair/right-justify
   claims (PLANNING.md's 2026-08-20 entry): open a create-form page,
   trigger a transfer/instance event elsewhere in the fleet, confirm the
   form's dropdown survives and unrelated pages no longer refresh; then
   confirm a topic-declaring page (Transfers) still does refresh on its
   own event type.

### Per-page topic assignment (final)

| Page template | `data-sse-topics` | Wrapper div added? | Notes |
|---|---|---|---|
| `transfers.html` | `transfer_progress` | yes | |
| `engine_transfers.html` | `engine_transfer_progress` | yes | |
| `engine_inventory.html` | `engine_transfer_progress` | yes | Decision 3 - refreshes on progress ticks too; harmless, page is form-free. |
| `dashboard.html` | `instance_result` | yes | The `OnlineNodes` tile still won't live-update - no event type for agent connect/disconnect (Decision 4, filed separately). |
| `profiles.html` | `instance_result instance_health` | yes | `instance_health` also needs the new `sse.js` listener (Decision 2). |
| `nodes.html` | *(none)* | no | Agent-status live-update filed as a separate follow-up (Decision 4). |
| `metrics.html` | *(none)* | no | Uses its own `sparkyMetricsLiveUpdate` in-place path for `telemetry`, not `scheduleRefresh` - untouched by this change. |
| `audit.html`, `users.html`, `settings.html` | *(none)* | no | No live SSE-driven data. |
| `register_node.html`, `profile_form.html`, `initiate_transfer.html`, `provision_engine.html`, `create_local_account.html`, `account.html` | *(none)* | no | Create/edit forms - the exact class of page the reported bug hit. Never auto-refresh. |
| `node_registered.html` | *(none)* | no | One-time bearer-token display - a re-fetch of this route does not re-show the token, so refreshing it would be actively harmful. |
| `forbidden.html` | *(none)* | no | Static. |

---

## Decisions (resolved 2026-09-10)

1. **Where the per-page topics marker lives.** A `data-sse-topics="..."`
   attribute on a plain `<div>` wrapping each topic-declaring page's
   `{{define "content"}}` block. Confirmed layout-safe: `.main` in
   `main.css` is `flex: 1; padding; min-width: 0` - a flex *item*, not a
   flex/grid *container* - and there are no `.main > *` /
   `#main-content > *` child-combinator selectors anywhere in `main.css`,
   so an inert wrapper `<div>` in normal block flow changes nothing
   visually. The wrapper is added only to pages that declare topics; every
   other page gets no wrapper and no attribute, and `sse.js` treats
   "no `[data-sse-topics]` element found inside `#main-content`" as "this
   page wants no live refresh" - the safe default. This works for both a
   full page load (the wrapper is in the server-rendered HTML) and an htmx
   partial swap (the wrapper is inside the swapped-in `content` block),
   without needing anything on `<main id="main-content">` itself, which a
   partial swap never re-renders.
2. **`instance_health` listener - in scope for this pass.** `sse.js` gains
   a fourth `addEventListener("instance_health", ...)`, topic-gated the
   same way as the other three. `profiles.html` declares
   `data-sse-topics="instance_result instance_health"`. This closes the
   pre-existing gap (health changes were published but nothing in the
   browser listened) in the same change that defines Profiles' topic set.
3. **`engine_inventory.html` live-updates.** Declares
   `data-sse-topics="engine_transfer_progress"`. It refreshes on every
   progress tick during a provisioning run (mostly redundant, but the
   500ms debounce collapses bursts and the page is form-free so a refresh
   is harmless), which is the only available signal that a run has
   completed and added a new inventory row.
4. **Node online/offline live-updating - filed, not fixed here.** Recorded
   as a PLANNING.md Known Issues row: the Nodes page and the Dashboard
   "Online" tile don't live-update on agent connect/disconnect because
   nothing publishes an SSE event for it (it's handled on the WebSocket
   lifecycle in `agentconn`, not through `onMessage`'s five message types).
   Fixing it needs new server-side work - a new event type plus a publish
   call on connect/disconnect - out of scope for this refresh-scoping fix.
   `nodes.html` therefore declares no topics; `dashboard.html` declares
   only `instance_result`.
5. **Input protection - focus guard only, no `hx-preserve`.** Topic scoping
   alone fully fixes the reported bug (the form pages it happened on
   declare no topics and so never auto-refresh). As defence-in-depth for
   the topic-declaring pages, `scheduleRefresh` gains a guard: when its
   debounced timer fires, if `document.activeElement` is an
   `INPUT`/`SELECT`/`TEXTAREA` inside `#main-content`, the refresh is
   *dropped* (not re-queued) - matching this file's own debounce-and-drop
   behaviour and `internal/events.Broker.Publish`'s full-buffer drop. The
   page catches up on the next event after the control loses focus. No
   topic-declaring page has such a control today, so this is purely
   forward-looking; `hx-preserve` is left for whenever a concrete element
   needs to survive a refresh that *does* proceed.
