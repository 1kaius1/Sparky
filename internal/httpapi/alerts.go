// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// alertManager is the subset of *alerts.Service this package needs. Every
// call is scoped to the actor: an alert the actor may not see is
// alerts.ErrNotFound, never a permission error, and the audited actions are
// audited inside the service.
type alertManager interface {
	List(ctx context.Context, actor rbac.Actor, view db.AlertListView, limit int) ([]*alerts.Item, error)
	Get(ctx context.Context, actor rbac.Actor, id string) (*alerts.Detail, error)
	UnreadCount(ctx context.Context, actor rbac.Actor) (int, error)
	MarkRead(ctx context.Context, actor rbac.Actor, id string) (bool, error)
	MarkUnread(ctx context.Context, actor rbac.Actor, id string) (bool, error)
	Dismiss(ctx context.Context, actor rbac.Actor, id string) (bool, error)
	MarkAllRead(ctx context.Context, actor rbac.Actor) (int, error)
	ResolveAlert(ctx context.Context, actor rbac.Actor, id string) (bool, error)

	// The retention setting on the Settings page (Admin).
	Settings(ctx context.Context, actor rbac.Actor) (*db.AlertSettings, error)
	UpdateRetention(ctx context.Context, actor rbac.Actor, months int) error
}

const (
	// maxAlertFormBytes bounds a bulk action's form: up to maxBulkAlerts
	// 36-character ids plus the field names.
	maxAlertFormBytes = 16 << 10

	// maxBulkAlerts is how many alerts one select-many action may name.
	maxBulkAlerts = 100
)

// alertTab is one of the inbox's filter tabs.
type alertTab struct {
	View   string
	Label  string
	Active bool
}

// alertRow is one alert in the inbox list.
type alertRow struct {
	ID            string
	Title         string
	SeverityClass string
	SeverityLabel string
	When          string
	Occurrences   int
	Read          bool
	Resolved      bool
	Selected      bool
}

// alertDetailView is the open alert's detail pane.
type alertDetailView struct {
	alertRow
	Body        string
	FirstRaised string
	LastRaised  string

	// ResolvedNote says when and how a resolved alert was closed; empty for
	// an open one.
	ResolvedNote string

	// ObjectURL and ObjectLabel link to the thing the alert is about; both
	// empty when the object has no page to link to.
	ObjectURL   string
	ObjectLabel string
	CanResolve  bool
}

type alertsPageData struct {
	View   string
	Tabs   []alertTab
	Rows   []alertRow
	Detail *alertDetailView

	// Error is shown above the list, e.g. when the alert named in the URL is
	// no longer available.
	Error string

	// CanMarkAllRead is false for the SuperAdmin, which has no read state.
	CanMarkAllRead bool
}

var alertViewTabs = []alertTab{
	{View: string(db.AlertViewUnread), Label: "Unread"},
	{View: string(db.AlertViewAll), Label: "All"},
	{View: string(db.AlertViewResolved), Label: "Resolved"},
}

// parseAlertView maps the ?view= value onto a known view, defaulting to
// unread so a hand-edited URL cannot ask for anything else.
func parseAlertView(s string) db.AlertListView {
	switch db.AlertListView(s) {
	case db.AlertViewAll:
		return db.AlertViewAll
	case db.AlertViewResolved:
		return db.AlertViewResolved
	default:
		return db.AlertViewUnread
	}
}

// alertsURL builds the inbox URL for a view and an optional open alert. Both
// parts come from validated values, and are escaped regardless.
func alertsURL(view db.AlertListView, selected string) string {
	u := "/alerts?view=" + url.QueryEscape(string(view))
	if selected != "" {
		u += "&selected=" + url.QueryEscape(selected)
	}
	return u
}

func severityPresentation(s db.AlertSeverity) (class, label string) {
	switch s {
	case db.AlertSeverityCritical:
		return "status-failed", "Critical"
	case db.AlertSeverityWarning:
		return "status-starting", "Warning"
	default:
		return "status-offline", "Info"
	}
}

func alertRowOf(it *alerts.Item, selected string) alertRow {
	class, label := severityPresentation(it.Severity)
	return alertRow{
		ID:            it.ID,
		Title:         it.Title,
		SeverityClass: class,
		SeverityLabel: label,
		When:          it.LastRaisedAt.Format("2006-01-02 15:04:05 MST"),
		Occurrences:   it.Occurrences,
		Read:          it.Read,
		Resolved:      it.ResolvedAt != nil,
		Selected:      it.ID == selected,
	}
}

// alertObjectLink names where an alert's object can be seen. Only an
// instance has a page today; an unknown type simply has no link.
func alertObjectLink(objectType *string) (linkURL, label string) {
	if objectType != nil && *objectType == "running_instance" {
		return "/dashboard", "View the Dashboard"
	}
	return "", ""
}

func (a *API) alertDetailViewOf(ctx context.Context, d *alerts.Detail) *alertDetailView {
	view := &alertDetailView{
		alertRow:    alertRowOf(d.Item, d.ID),
		Body:        d.Body,
		FirstRaised: d.FirstRaisedAt.Format("2006-01-02 15:04:05 MST"),
		LastRaised:  d.LastRaisedAt.Format("2006-01-02 15:04:05 MST"),
		CanResolve:  d.CanResolve,
	}
	view.ObjectURL, view.ObjectLabel = alertObjectLink(d.ObjectType)
	if d.ResolvedAt != nil {
		when := d.ResolvedAt.Format("2006-01-02 15:04:05 MST")
		switch {
		case d.Resolution != nil && *d.Resolution == db.AlertResolutionCleared:
			view.ResolvedNote = "Resolved " + when + ": the condition cleared."
		case d.ResolvedBy != nil:
			view.ResolvedNote = "Resolved " + when + " by " + a.resolveUserName(ctx, d.ResolvedBy) + "."
		default:
			view.ResolvedNote = "Resolved " + when + " by the break-glass administrator."
		}
	}
	return view
}

// alertActor resolves the viewer, answering the request itself on failure.
// ok is false when the handler should stop.
func (a *API) alertActor(w http.ResponseWriter, r *http.Request) (rbac.Actor, bool) {
	ctx := r.Context()
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// RequireSession already guarantees this - defensive only.
		writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "no session")
		return rbac.Actor{}, false
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for alerts: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return rbac.Actor{}, false
	}
	return actor, true
}

// alertUnreadCount is the sidebar badge's number. Any failure shows no badge
// rather than failing the page, the same "display nicety" stance as
// canViewAdminNav.
func (a *API) alertUnreadCount(ctx context.Context) int {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return 0
	}
	actor, err := a.actorFromIdentity(ctx, identity)
	if err != nil {
		a.logger.Printf("httpapi: resolve actor for the alerts badge: %v", err)
		return 0
	}
	n, err := a.alerts.UnreadCount(ctx, actor)
	if err != nil {
		a.logger.Printf("httpapi: count unread alerts: %v", err)
		return 0
	}
	return n
}

// handleAlerts is GET /alerts, the inbox. ?view= picks the tab and
// ?selected= opens one alert beside the list. Viewing an alert here changes
// nothing: marking it read is a separate POST the opened pane fires, so a
// state change never hides inside a GET.
func (a *API) handleAlerts(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.alertActor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	view := parseAlertView(r.URL.Query().Get("view"))
	selected := r.URL.Query().Get("selected")

	items, err := a.alerts.List(ctx, actor, view, 0)
	if err != nil {
		a.logger.Printf("httpapi: list alerts: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := alertsPageData{View: string(view), CanMarkAllRead: !actor.IsSuperAdmin}
	for _, tab := range alertViewTabs {
		tab.Active = tab.View == string(view)
		data.Tabs = append(data.Tabs, tab)
	}
	for _, it := range items {
		data.Rows = append(data.Rows, alertRowOf(it, selected))
	}

	if selected != "" {
		detail, err := a.alerts.Get(ctx, actor, selected)
		switch {
		case errors.Is(err, alerts.ErrNotFound):
			data.Error = "That alert is no longer available."
		case err != nil:
			a.logger.Printf("httpapi: get alert %s: %v", selected, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		default:
			data.Detail = a.alertDetailViewOf(ctx, detail)
		}
	}

	a.render(w, r, "alerts", "Alerts", data)
}

// handleAlertsBadge is GET /alerts/badge, the sidebar badge fragment the page
// refetches when an alerts_changed event arrives.
func (a *API) handleAlertsBadge(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.alertActor(w, r)
	if !ok {
		return
	}
	n, err := a.alerts.UnreadCount(r.Context(), actor)
	if err != nil {
		a.logger.Printf("httpapi: count unread alerts: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.renderPartial(w, "alerts_badge", n)
}

// writeAlertError answers a failed alert action and reports whether the
// caller may carry on (it may, when err is nil).
func (a *API) writeAlertError(w http.ResponseWriter, r *http.Request, what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, alerts.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "alert not found")
	case errors.Is(err, rbac.ErrNotPermitted):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "only an Admin, or the person an alert is addressed to alone, may resolve it")
	default:
		a.logger.Printf("httpapi: %s: %v", what, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
	return false
}

// alertActionSpec describes one single-alert POST.
type alertActionSpec struct {
	name string
	do   func(ctx context.Context, actor rbac.Actor, id string) (bool, error)

	// keepOpen leaves the alert open in the detail pane after the action;
	// a dismissed alert is gone, so its pane is not reopened.
	keepOpen bool

	// quiet answers 204 with no redirect, for the pane's automatic mark-read:
	// the alerts_changed event it causes refreshes the page.
	quiet bool
}

func (a *API) handleAlertAction(spec alertActionSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.alertActor(w, r)
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
			return
		}
		id := chi.URLParam(r, "id")
		if _, err := spec.do(r.Context(), actor, id); !a.writeAlertError(w, r, spec.name+" "+id, err) {
			return
		}
		if !spec.quiet {
			selected := ""
			if spec.keepOpen {
				selected = id
			}
			w.Header().Set("HX-Redirect", alertsURL(parseAlertView(r.PostForm.Get("view")), selected))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) handleAlertRead() http.HandlerFunc {
	return a.handleAlertAction(alertActionSpec{name: "mark alert read", do: a.alerts.MarkRead, keepOpen: true, quiet: true})
}

func (a *API) handleAlertUnread() http.HandlerFunc {
	return a.handleAlertAction(alertActionSpec{name: "mark alert unread", do: a.alerts.MarkUnread, keepOpen: true})
}

func (a *API) handleAlertDismiss() http.HandlerFunc {
	return a.handleAlertAction(alertActionSpec{name: "dismiss alert", do: a.alerts.Dismiss})
}

func (a *API) handleAlertResolve() http.HandlerFunc {
	return a.handleAlertAction(alertActionSpec{name: "resolve alert", do: a.alerts.ResolveAlert, keepOpen: true})
}

// handleAlertsMarkAllRead is POST /alerts/mark-all-read.
func (a *API) handleAlertsMarkAllRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.alertActor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}
	if _, err := a.alerts.MarkAllRead(r.Context(), actor); !a.writeAlertError(w, r, "mark all alerts read", err) {
		return
	}
	w.Header().Set("HX-Redirect", alertsURL(parseAlertView(r.PostForm.Get("view")), ""))
	w.WriteHeader(http.StatusNoContent)
}

// handleAlertsBulk is POST /alerts/bulk, the inbox's select-many action: mark
// the chosen alerts read, or dismiss them. Each alert is audited on its own,
// exactly as if it had been acted on singly.
func (a *API) handleAlertsBulk(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.alertActor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "malformed form")
		return
	}
	ids := r.PostForm["ids"]
	if len(ids) == 0 {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "select at least one alert")
		return
	}
	if len(ids) > maxBulkAlerts {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "select at most "+strconv.Itoa(maxBulkAlerts)+" alerts at a time")
		return
	}

	var do func(context.Context, rbac.Actor, string) (bool, error)
	switch strings.TrimSpace(r.PostForm.Get("action")) {
	case "read":
		do = a.alerts.MarkRead
	case "dismiss":
		do = a.alerts.Dismiss
	default:
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "action must be read or dismiss")
		return
	}

	for _, id := range ids {
		_, err := do(r.Context(), actor, id)
		// An alert that vanished since the page loaded (expired, or dismissed
		// in another tab) is not worth failing the rest of the selection for.
		if errors.Is(err, alerts.ErrNotFound) {
			continue
		}
		if !a.writeAlertError(w, r, "bulk alert action", err) {
			return
		}
	}
	w.Header().Set("HX-Redirect", alertsURL(parseAlertView(r.PostForm.Get("view")), ""))
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateAlertRetention sets how long alerts are kept - see
// alerts.Service.UpdateRetention. A value outside 1 to 24 redisplays the page
// with the reason, never a 500.
func (a *API) handleUpdateAlertRetention(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.alertActor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{AlertError: "invalid form submission"})
		return
	}
	months, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("retention_months")))
	if err != nil {
		a.renderSettingsPage(w, r, actor, settingsPageData{AlertError: "retention must be a whole number of months"})
		return
	}

	err = a.alerts.UpdateRetention(r.Context(), actor, months)
	switch {
	case errors.Is(err, rbac.ErrNotPermitted):
		a.renderForbidden(w, r, actor.Tier)
		return
	case errors.Is(err, alerts.ErrInvalidRetention):
		a.renderSettingsPage(w, r, actor, settingsPageData{AlertError: err.Error()})
		return
	case err != nil:
		a.logger.Printf("httpapi: update alert retention: %v", err)
		a.renderSettingsPage(w, r, actor, settingsPageData{AlertError: "failed to update retention"})
		return
	}

	a.renderSettingsPage(w, r, actor, settingsPageData{AlertSuccess: true})
}
