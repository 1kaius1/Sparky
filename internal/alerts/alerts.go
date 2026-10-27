// SPDX-License-Identifier: AGPL-3.0-or-later

// Package alerts is the in-app alert inbox: durable, addressable notices
// about things that need a person's attention (an instance that died, later a
// stale container or an unsaved log). Producers inside the server call Raise
// when a condition appears and ResolveCondition when it clears; people read,
// dismiss and resolve alerts through the actor-facing methods, each of which
// is audited. See PLANNING.md's 2026-10-26 Decisions Log entry for the
// design and SCHEMA.md Alerts for the data model.
//
// An alert is addressed to a set of audiences (a user, a tier and above, or an
// application group - internal/groups).
// Read and dismissed state is per user; resolving closes the alert for
// everyone. internal/events only carries a signal with no history, so it
// cannot hold an alert - it is used here just to tell open browsers to
// refresh their unread count.
package alerts

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// EventAlertsChanged is the server event published whenever the set of
	// alerts, or any viewer's read state, changed. It carries no user: every
	// open browser refetches its own unread count.
	EventAlertsChanged = "alerts_changed"

	// MaxTitleLen and MaxBodyLen bound what a producer may store, in runes.
	MaxTitleLen = 200
	MaxBodyLen  = 2000

	// DefaultListLimit and MaxListLimit bound how many alerts a list returns.
	DefaultListLimit = 100
	MaxListLimit     = 200

	// zeroUUID stands in as the object id of an audit record that is about
	// no single alert (mark all read, the settings row, an expiry run);
	// audit_log.object_id is a real uuid.
	zeroUUID = "00000000-0000-0000-0000-000000000000"
)

var (
	// ErrNotFound is returned for an alert that does not exist or that the
	// viewer may not see - the two are deliberately indistinguishable.
	ErrNotFound = errors.New("alert not found")

	// ErrInvalidAlert is returned by Raise for input that breaks a rule.
	ErrInvalidAlert = errors.New("invalid alert")
)

var (
	kindPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	objectTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// store is the subset of *db.AlertRepository this package needs.
type store interface {
	Raise(ctx context.Context, in db.NewAlert) (*db.RaiseResult, error)
	ResolveByDedupeKey(ctx context.Context, dedupeKey string) ([]string, error)
	ResolveByKeyPrefixExcept(ctx context.Context, prefix string, keep []string) ([]string, error)
	ResolveByUser(ctx context.Context, id string, resolvedBy *string) (bool, error)
	Reopen(ctx context.Context, id string) error
	FindVisible(ctx context.Context, viewer db.AlertViewer, id string) (*db.AlertView, error)
	ListVisible(ctx context.Context, viewer db.AlertViewer, view db.AlertListView, limit int) ([]*db.AlertView, error)
	CountUnread(ctx context.Context, viewer db.AlertViewer) (int, error)
	Audiences(ctx context.Context, alertID string) ([]db.AlertAudience, error)
	MarkRead(ctx context.Context, userID, alertID string) (bool, error)
	MarkUnread(ctx context.Context, userID, alertID string) (bool, error)
	Dismiss(ctx context.Context, userID, alertID string) (bool, error)
	RestoreDismissed(ctx context.Context, userID, alertID string) error
	MarkAllRead(ctx context.Context, viewer db.AlertViewer) ([]string, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// settingsStore is the subset of *db.AlertSettingsRepository this package
// needs: the retention period.
type settingsStore interface {
	Get(ctx context.Context) (*db.AlertSettings, error)
	Update(ctx context.Context, retentionMonths int, updatedBy *string) error
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// publisher is the subset of *events.Broker this package needs.
type publisher interface {
	Publish(evt events.Event)
}

// Service raises, resolves, lists and acts on alerts.
type Service struct {
	store    store
	settings settingsStore
	audit    auditRecorder
	pub      publisher
	logger   *log.Logger
	now      func() time.Time
	notifier Notifier
}

// Notifier is told about each alert worth a person's attention as it is
// raised: a new alert, or an open one that became more severe (escalated). It
// is the email service; it has no way to fail the raise, and so no error to
// return.
type Notifier interface {
	AlertRaised(ctx context.Context, a *db.Alert, escalated bool)
}

// NewService constructs a Service. pub may be nil (no live refresh), which
// only tests do.
func NewService(store store, settings settingsStore, audit auditRecorder, pub publisher, logger *log.Logger) *Service {
	return &Service{store: store, settings: settings, audit: audit, pub: pub, logger: logger, now: time.Now}
}

// WithNotifier sets who is told when an alert is raised or escalates, and
// returns the service for chaining. Unset, nobody is (alerts stay in the
// inbox only).
func (s *Service) WithNotifier(n Notifier) *Service {
	s.notifier = n
	return s
}

// RaiseParams describes an alert a producer wants raised.
type RaiseParams struct {
	// Kind is a machine-readable slug naming what sort of alert this is,
	// e.g. "instance_dead". New producers add kinds without a schema change.
	Kind     string
	Severity db.AlertSeverity
	Title    string
	Body     string

	// ObjectType and ObjectID optionally name the thing the alert is about
	// (e.g. "running_instance" and its id), so the inbox can link to it.
	ObjectType string
	ObjectID   string

	// DedupeKey, when set, makes a repeat of this condition update the open
	// alert instead of adding another. Pass the same key to ResolveCondition
	// when the condition clears.
	DedupeKey string

	// Audiences is who the alert is for; at least one.
	Audiences []db.AlertAudience
}

// Validate reports whether Raise would accept p. It is exported so a producer's
// tests can hold a fake to the same rules as the real service, instead of
// passing on an alert the inbox would refuse.
func (p RaiseParams) Validate() error {
	if !kindPattern.MatchString(p.Kind) {
		return fmt.Errorf("%w: kind must be a lowercase slug", ErrInvalidAlert)
	}
	if p.Severity.Rank() == 0 {
		return fmt.Errorf("%w: unknown severity %q", ErrInvalidAlert, p.Severity)
	}
	if strings.TrimSpace(p.Title) == "" || utf8.RuneCountInString(p.Title) > MaxTitleLen {
		return fmt.Errorf("%w: title must be 1 to %d characters", ErrInvalidAlert, MaxTitleLen)
	}
	if utf8.RuneCountInString(p.Body) > MaxBodyLen {
		return fmt.Errorf("%w: body must be at most %d characters", ErrInvalidAlert, MaxBodyLen)
	}
	if (p.ObjectType == "") != (p.ObjectID == "") {
		return fmt.Errorf("%w: object type and id go together", ErrInvalidAlert)
	}
	if p.ObjectType != "" && (!objectTypePattern.MatchString(p.ObjectType) || len(p.ObjectID) > 64) {
		return fmt.Errorf("%w: malformed object reference", ErrInvalidAlert)
	}
	if len(p.DedupeKey) > 200 {
		return fmt.Errorf("%w: dedupe key is too long", ErrInvalidAlert)
	}
	if len(p.Audiences) == 0 {
		return fmt.Errorf("%w: an alert needs at least one audience", ErrInvalidAlert)
	}
	for _, a := range p.Audiences {
		switch a.Type {
		case db.AlertAudienceUser:
			if !uuidPattern.MatchString(a.UserID) {
				return fmt.Errorf("%w: user audience needs a user id", ErrInvalidAlert)
			}
		case db.AlertAudienceGroup:
			if !uuidPattern.MatchString(a.AppGroupID) {
				return fmt.Errorf("%w: group audience needs a group id", ErrInvalidAlert)
			}
		case db.AlertAudienceTier:
			switch a.MinTier {
			case db.TierReadOnly, db.TierDeveloper, db.TierPowerDev, db.TierAdmin:
			default:
				return fmt.Errorf("%w: unknown tier %q", ErrInvalidAlert, a.MinTier)
			}
		default:
			return fmt.Errorf("%w: unknown audience type %q", ErrInvalidAlert, a.Type)
		}
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Raise records an alert, or updates the open one with the same dedupe key.
// It is for producers inside the server: there is no actor, and a raise is
// not itself audited - the alert row is the record, and the event that
// caused it is usually audited already (PLANNING.md, 2026-10-26). Browsers
// are told to refresh only when something a viewer would notice changed: a
// new alert, or one that got more severe.
func (s *Service) Raise(ctx context.Context, p RaiseParams) (*db.RaiseResult, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	res, err := s.store.Raise(ctx, db.NewAlert{
		Kind: p.Kind, Severity: p.Severity, Title: p.Title, Body: p.Body,
		ObjectType: optional(p.ObjectType), ObjectID: optional(p.ObjectID),
		DedupeKey: optional(p.DedupeKey), Audiences: p.Audiences,
	})
	if err != nil {
		return nil, fmt.Errorf("raise alert: %w", err)
	}
	if res.Created || res.Escalated {
		s.changed()
		if s.notifier != nil {
			// Detached from the caller's cancellation: the alert exists now, and
			// a caller that gives up must not leave the people it concerns
			// unmailed. The notifier bounds its own time.
			s.notifier.AlertRaised(context.WithoutCancel(ctx), res.Alert, res.Escalated)
		}
	}
	return res, nil
}

// ResolveCondition closes the open alert with this dedupe key because its
// condition cleared, and returns how many it closed (0 when none was open,
// which is the normal case and is cheap). Like Raise it is not audited.
func (s *Service) ResolveCondition(ctx context.Context, dedupeKey string) (int, error) {
	ids, err := s.store.ResolveByDedupeKey(ctx, dedupeKey)
	if err != nil {
		return 0, fmt.Errorf("resolve alert condition: %w", err)
	}
	if len(ids) > 0 {
		s.changed()
	}
	return len(ids), nil
}

// ResolveConditionsExcept closes every open alert whose dedupe key starts with
// prefix and is not one of keep, because the thing each names is gone, and
// returns how many it closed. For a producer that raises one alert per thing
// (a container) and cannot enumerate the ones it raised earlier. Not audited,
// like ResolveCondition.
func (s *Service) ResolveConditionsExcept(ctx context.Context, prefix string, keep []string) (int, error) {
	if prefix == "" {
		// An empty prefix would match every alert; refuse it rather than
		// close the whole inbox on a producer's bug.
		return 0, fmt.Errorf("%w: a key prefix is required", ErrInvalidAlert)
	}
	ids, err := s.store.ResolveByKeyPrefixExcept(ctx, prefix, keep)
	if err != nil {
		return 0, fmt.Errorf("resolve alert conditions: %w", err)
	}
	if len(ids) > 0 {
		s.changed()
	}
	return len(ids), nil
}

func (s *Service) changed() {
	if s.pub != nil {
		s.pub.Publish(events.Event{Type: EventAlertsChanged})
	}
}

// viewerOf maps an actor to who is looking. The break-glass SuperAdmin is not
// a users row: it sees tier audiences as Admin and has no user id.
func viewerOf(actor rbac.Actor) db.AlertViewer {
	if actor.IsSuperAdmin {
		return db.AlertViewer{Tier: db.TierAdmin}
	}
	return db.AlertViewer{UserID: actor.UserID, Tier: actor.Tier}
}

// Item is an alert as one viewer sees it. Read is always true for the
// SuperAdmin, which has no read state of its own.
type Item struct {
	db.Alert
	Read bool
}

func itemOf(actor rbac.Actor, v *db.AlertView) *Item {
	return &Item{Alert: v.Alert, Read: actor.IsSuperAdmin || v.ReadAt != nil}
}

// List returns the actor's alerts for view (unread when empty), newest
// activity first. Listing is not audited: like reading a mail subject line
// without opening the message (PLANNING.md, 2026-10-26).
func (s *Service) List(ctx context.Context, actor rbac.Actor, view db.AlertListView, limit int) ([]*Item, error) {
	switch view {
	case "":
		view = db.AlertViewUnread
	case db.AlertViewUnread, db.AlertViewAll, db.AlertViewResolved:
	default:
		view = db.AlertViewUnread
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	// The SuperAdmin has no read state, so "unread" would list everything
	// open as unread forever; there is nothing unread for it.
	if actor.IsSuperAdmin && view == db.AlertViewUnread {
		return nil, nil
	}
	views, err := s.store.ListVisible(ctx, viewerOf(actor), view, limit)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	items := make([]*Item, 0, len(views))
	for _, v := range views {
		items = append(items, itemOf(actor, v))
	}
	return items, nil
}

// UnreadCount is the number for the sidebar badge: open alerts the actor has
// not read. Never audited.
func (s *Service) UnreadCount(ctx context.Context, actor rbac.Actor) (int, error) {
	n, err := s.store.CountUnread(ctx, viewerOf(actor))
	if err != nil {
		return 0, fmt.Errorf("count unread alerts: %w", err)
	}
	return n, nil
}

// Detail is one alert for its detail pane.
type Detail struct {
	*Item

	// CanResolve is whether the actor may close this alert for everyone: it
	// is open, and the actor is an Admin or its sole addressee.
	CanResolve bool
}

func (s *Service) visible(ctx context.Context, actor rbac.Actor, id string) (*db.AlertView, error) {
	if !uuidPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	v, err := s.store.FindVisible(ctx, viewerOf(actor), id)
	if errors.Is(err, db.ErrAlertNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find alert: %w", err)
	}
	return v, nil
}

// mayResolve reports whether actor may close alert for everyone: an Admin
// always, or the person it is addressed to alone.
func (s *Service) mayResolve(ctx context.Context, actor rbac.Actor, alertID string) (bool, error) {
	if rbac.CanResolveAnyAlert(actor) {
		return true, nil
	}
	if actor.IsSuperAdmin {
		return false, nil
	}
	auds, err := s.store.Audiences(ctx, alertID)
	if err != nil {
		return false, fmt.Errorf("look up alert audiences: %w", err)
	}
	return len(auds) == 1 && auds[0].Type == db.AlertAudienceUser && auds[0].UserID == actor.UserID, nil
}

// Get returns one alert the actor may see. Viewing it here does not change
// read state: marking it read is the separate MarkRead call, so that a
// state change is never hidden inside a GET.
func (s *Service) Get(ctx context.Context, actor rbac.Actor, id string) (*Detail, error) {
	v, err := s.visible(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{Item: itemOf(actor, v)}
	if v.ResolvedAt == nil {
		d.CanResolve, err = s.mayResolve(ctx, actor, v.ID)
		if err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (s *Service) record(ctx context.Context, actor rbac.Actor, action, objectType, objectID string, detail map[string]any) error {
	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	return s.audit.Record(ctx, actorID, actor.IsSuperAdmin, action, objectType, objectID, detail)
}

// userStateChange applies one per-user state change to an alert the actor may
// see, audits it only if it changed something, and undoes it if the audit
// write fails - the project's rule that an unaudited change must not stay.
func (s *Service) userStateChange(
	ctx context.Context, actor rbac.Actor, id, action string,
	apply func(ctx context.Context, userID, alertID string) (bool, error),
	revert func(ctx context.Context, userID, alertID string) error,
) (bool, error) {
	v, err := s.visible(ctx, actor, id)
	if err != nil {
		return false, err
	}
	// The SuperAdmin has no per-user state to change.
	if actor.IsSuperAdmin {
		return false, nil
	}
	changed, err := apply(ctx, actor.UserID, v.ID)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := s.record(ctx, actor, action, "alert", v.ID, map[string]any{"kind": v.Kind}); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := revert(ctx, actor.UserID, v.ID); revertErr != nil {
			return false, fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return false, auditErr
	}
	s.changed()
	return true, nil
}

// MarkRead marks an alert read for the actor and reports whether that
// changed anything. Audited as read_alert, and only on the unread-to-read
// transition, so opening an already-read alert again is not logged.
func (s *Service) MarkRead(ctx context.Context, actor rbac.Actor, id string) (bool, error) {
	return s.userStateChange(ctx, actor, id, "read_alert", s.store.MarkRead,
		func(ctx context.Context, userID, alertID string) error {
			_, err := s.store.MarkUnread(ctx, userID, alertID)
			return err
		})
}

// MarkUnread clears the actor's read mark. Audited as marked_alert_unread.
func (s *Service) MarkUnread(ctx context.Context, actor rbac.Actor, id string) (bool, error) {
	return s.userStateChange(ctx, actor, id, "marked_alert_unread", s.store.MarkUnread,
		func(ctx context.Context, userID, alertID string) error {
			_, err := s.store.MarkRead(ctx, userID, alertID)
			return err
		})
}

// Dismiss hides an alert from the actor only. Audited as dismissed_alert.
func (s *Service) Dismiss(ctx context.Context, actor rbac.Actor, id string) (bool, error) {
	return s.userStateChange(ctx, actor, id, "dismissed_alert", s.store.Dismiss, s.store.RestoreDismissed)
}

// MarkAllRead marks every open unread alert the actor can see as read and
// returns how many it changed. Audited as one marked_all_alerts_read entry
// carrying the count, not one entry per alert.
func (s *Service) MarkAllRead(ctx context.Context, actor rbac.Actor) (int, error) {
	if actor.IsSuperAdmin {
		return 0, nil
	}
	ids, err := s.store.MarkAllRead(ctx, viewerOf(actor))
	if err != nil {
		return 0, fmt.Errorf("mark all alerts read: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.record(ctx, actor, "marked_all_alerts_read", "alert", zeroUUID, map[string]any{"count": len(ids)}); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		var revertErr error
		for _, id := range ids {
			if _, err := s.store.MarkUnread(ctx, actor.UserID, id); err != nil {
				revertErr = err
			}
		}
		if revertErr != nil {
			return 0, fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return 0, auditErr
	}
	s.changed()
	return len(ids), nil
}

// ResolveAlert closes an alert for everyone. Allowed to an Admin, or to the
// one person an alert is addressed to alone; anyone else gets
// rbac.ErrNotPermitted. Audited as resolved_alert. It reports whether the
// alert changed (false when it was already resolved).
func (s *Service) ResolveAlert(ctx context.Context, actor rbac.Actor, id string) (bool, error) {
	v, err := s.visible(ctx, actor, id)
	if err != nil {
		return false, err
	}
	ok, err := s.mayResolve(ctx, actor, v.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, rbac.ErrNotPermitted
	}
	var resolvedBy *string
	if !actor.IsSuperAdmin {
		resolvedBy = &actor.UserID
	}
	changed, err := s.store.ResolveByUser(ctx, v.ID, resolvedBy)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := s.record(ctx, actor, "resolved_alert", "alert", v.ID, map[string]any{"kind": v.Kind}); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.store.Reopen(ctx, v.ID); revertErr != nil {
			return false, fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return false, auditErr
	}
	s.changed()
	return true, nil
}
