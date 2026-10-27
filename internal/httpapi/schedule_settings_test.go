// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/schedule"
	"github.com/1kaius1/Sparky/internal/session"
)

// fakeSchedule implements scheduleManager with the same tier rule the real
// service applies, so these tests exercise the handlers' handling of
// ErrNotPermitted and ErrInvalidTimezone rather than a rule of their own.
type fakeSchedule struct {
	timezone  string
	updateErr error
	updatedTo []string
}

func (f *fakeSchedule) Settings(_ context.Context, actor rbac.Actor) (*db.ScheduleSettings, error) {
	if !rbac.CanViewSettings(actor) {
		return nil, rbac.ErrNotPermitted
	}
	tz := f.timezone
	if tz == "" {
		tz = "UTC"
	}
	return &db.ScheduleSettings{Timezone: tz, UpdatedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeSchedule) UpdateTimezone(_ context.Context, actor rbac.Actor, name string) error {
	if !rbac.CanViewSettings(actor) {
		return rbac.ErrNotPermitted
	}
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updatedTo = append(f.updatedTo, name)
	f.timezone = name
	return nil
}

// scheduleSettingsAPI is an API whose Settings page has real rows behind it,
// with fake as its schedule service.
func scheduleSettingsAPI(t *testing.T, tier db.Tier, fake *fakeSchedule) (*API, string) {
	t.Helper()
	api, uid := settingsAPI(t, tier, &fakeContainerLogs{})
	api.schedule = fake
	return api, uid
}

func TestSettings_ScheduleTimezoneIsShownAndUpdatableByAnAdmin(t *testing.T) {
	fake := &fakeSchedule{timezone: "Europe/London"}
	api, uid := scheduleSettingsAPI(t, db.TierAdmin, fake)
	body := getAs(t, api, uid, "/settings").Body.String()
	for _, want := range []string{"Schedule time zone", `name="timezone"`, `value="Europe/London"`, `action="/settings/schedule"`, "The time there now"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page is missing %q", want)
		}
	}

	rec := postFormAs(t, api, uid, "/settings/schedule", url.Values{"timezone": {"  America/New_York "}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Time zone updated") {
		t.Errorf("update: status %d body %.200s", rec.Code, rec.Body.String())
	}
	if len(fake.updatedTo) != 1 || fake.updatedTo[0] != "America/New_York" {
		t.Errorf("updated to %v, want [America/New_York] with the surrounding spaces trimmed", fake.updatedTo)
	}
}

func TestSettings_ScheduleTimezoneRejectsAnUnknownZoneWithAReason(t *testing.T) {
	fake := &fakeSchedule{updateErr: fmt.Errorf("%w: %q is not a time zone this server knows", schedule.ErrInvalidTimezone, "Mars/Base")}
	api, uid := scheduleSettingsAPI(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/settings/schedule", url.Values{"timezone": {"Mars/Base"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not a time zone this server knows") {
		t.Errorf("status %d body %.300s", rec.Code, rec.Body.String())
	}
	if len(fake.updatedTo) != 0 {
		t.Errorf("a refused zone was stored: %v", fake.updatedTo)
	}
}

func TestSettings_ScheduleTimezoneHidesAnInternalFailure(t *testing.T) {
	fake := &fakeSchedule{updateErr: fmt.Errorf("update schedule settings: pq: connection refused at 10.0.0.5")}
	api, uid := scheduleSettingsAPI(t, db.TierAdmin, fake)
	rec := postFormAs(t, api, uid, "/settings/schedule", url.Values{"timezone": {"UTC"}})
	body := rec.Body.String()
	if !strings.Contains(body, "failed to update the time zone") || strings.Contains(body, "10.0.0.5") {
		t.Errorf("status %d, body leaks or lacks the generic message: %.300s", rec.Code, body)
	}
}

func TestSettings_ScheduleTimezoneIsForbiddenBelowAdmin(t *testing.T) {
	fake := &fakeSchedule{}
	api, uid := scheduleSettingsAPI(t, db.TierPowerDev, fake)
	if rec := postFormAs(t, api, uid, "/settings/schedule", url.Values{"timezone": {"UTC"}}); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(fake.updatedTo) != 0 {
		t.Error("a non-admin changed the time zone")
	}
}

func TestSettings_ScheduleTimezoneNeedsACSRFToken(t *testing.T) {
	fake := &fakeSchedule{}
	api, uid := scheduleSettingsAPI(t, db.TierAdmin, fake)
	cookieValue, err := session.Sign(testSessionSecret, session.New(uid, sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	// A session but no CSRF token: the form content type is what a real
	// <form> or htmx submission sends, and what RequireCSRF checks.
	req := httptest.NewRequest(http.MethodPost, "/settings/schedule", strings.NewReader(url.Values{"timezone": {"UTC"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(fake.updatedTo) != 0 {
		t.Errorf("status %d, updated %v; want a refused request with no change", rec.Code, fake.updatedTo)
	}
}
