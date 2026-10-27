// SPDX-License-Identifier: AGPL-3.0-or-later

package schedule

import (
	"context"
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

var (
	admin     = rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}
	developer = rbac.Actor{Tier: db.TierDeveloper, UserID: "dev-1"}
)

type fakeSettings struct {
	cfg     db.ScheduleSettings
	updates []string
}

func (f *fakeSettings) Get(context.Context) (*db.ScheduleSettings, error) {
	c := f.cfg
	return &c, nil
}

func (f *fakeSettings) Update(_ context.Context, tz string, by *string) error {
	f.cfg.Timezone = tz
	f.cfg.UpdatedBy = by
	f.updates = append(f.updates, tz)
	return nil
}

type auditCall struct {
	actorID    *string
	superAdmin bool
	action     string
	objectType string
	detail     map[string]any
}

type fakeAudit struct {
	calls []auditCall
	err   error
}

func (f *fakeAudit) Record(_ context.Context, actorID *string, superAdmin bool, action, objectType, _ string, detail map[string]any) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, auditCall{actorID, superAdmin, action, objectType, detail})
	return nil
}

func newTZService() (*Service, *fakeSettings, *fakeAudit) {
	st := &fakeSettings{cfg: db.ScheduleSettings{Timezone: "UTC"}}
	au := &fakeAudit{}
	return NewService(st, au), st, au
}

func TestParseTimezone(t *testing.T) {
	for _, ok := range []string{"UTC", "America/New_York", "Europe/London", "Asia/Kolkata", "Etc/GMT+5", "America/Argentina/Buenos_Aires"} {
		if _, err := ParseTimezone(ok); err != nil {
			if errors.Is(err, ErrInvalidTimezone) && ok != "UTC" {
				t.Logf("%s not in this machine's zone database: %v", ok, err)
				continue
			}
			t.Errorf("ParseTimezone(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Local", " UTC", "UTC ", "../etc/passwd", "/etc/localtime", "America/Nowhere", "a b", "America//New_York", "Mars/Olympus_Mons", string(make([]byte, 100))} {
		if _, err := ParseTimezone(bad); !errors.Is(err, ErrInvalidTimezone) {
			t.Errorf("ParseTimezone(%q) = %v, want ErrInvalidTimezone", bad, err)
		}
	}
}

func TestUpdateTimezone_AdminOnlyAndChecked(t *testing.T) {
	svc, st, au := newTZService()
	ctx := context.Background()
	if err := svc.UpdateTimezone(ctx, developer, "UTC"); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v, want ErrNotPermitted", err)
	}
	for _, bad := range []string{"", "Local", "Nowhere/Land"} {
		if err := svc.UpdateTimezone(ctx, admin, bad); !errors.Is(err, ErrInvalidTimezone) {
			t.Errorf("%q: %v, want ErrInvalidTimezone", bad, err)
		}
	}
	if len(st.updates) != 0 || len(au.calls) != 0 {
		t.Errorf("a refused update changed something: %v, %d audit", st.updates, len(au.calls))
	}
}

func TestUpdateTimezone_IsAuditedWithWhoAndWhat(t *testing.T) {
	svc, st, au := newTZService()
	if err := svc.UpdateTimezone(context.Background(), admin, "America/New_York"); err != nil {
		if errors.Is(err, ErrInvalidTimezone) {
			t.Skip("no America/New_York in this machine's zone database")
		}
		t.Fatal(err)
	}
	if st.cfg.Timezone != "America/New_York" || st.cfg.UpdatedBy == nil || *st.cfg.UpdatedBy != "admin-1" {
		t.Errorf("settings = %+v", st.cfg)
	}
	if len(au.calls) != 1 {
		t.Fatalf("%d audit records, want 1", len(au.calls))
	}
	c := au.calls[0]
	if c.action != "updated_schedule_timezone" || c.objectType != "schedule_settings" || c.actorID == nil || *c.actorID != "admin-1" || c.superAdmin ||
		c.detail["timezone"] != "America/New_York" || c.detail["previous_timezone"] != "UTC" {
		t.Errorf("audit = %+v", c)
	}

	svc2, st2, au2 := newTZService()
	if err := svc2.UpdateTimezone(context.Background(), rbac.Actor{IsSuperAdmin: true}, "UTC"); err != nil {
		t.Fatal(err)
	}
	if c := au2.calls[0]; c.actorID != nil || !c.superAdmin {
		t.Errorf("break-glass audit = %+v, want a nil actor with the super-admin flag", c)
	}
	if st2.cfg.UpdatedBy != nil {
		t.Errorf("break-glass UpdatedBy = %v, want nil", st2.cfg.UpdatedBy)
	}
}

func TestUpdateTimezone_AFailedAuditWriteRevertsTheChange(t *testing.T) {
	svc, st, au := newTZService()
	au.err = errors.New("audit down")
	if err := svc.UpdateTimezone(context.Background(), admin, "Etc/UTC"); err == nil {
		t.Fatal("UpdateTimezone succeeded without an audit record")
	}
	if st.cfg.Timezone != "UTC" {
		t.Errorf("timezone is %q after a failed audit, want it reverted to UTC", st.cfg.Timezone)
	}
}

func TestSettings_AdminOnly(t *testing.T) {
	svc, _, _ := newTZService()
	if _, err := svc.Settings(context.Background(), developer); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v", err)
	}
	cfg, err := svc.Settings(context.Background(), admin)
	if err != nil || cfg.Timezone != "UTC" {
		t.Errorf("admin: %+v, %v", cfg, err)
	}
}

func TestLocation(t *testing.T) {
	svc, st, _ := newTZService()
	loc, err := svc.Location(context.Background())
	if err != nil || loc.String() != "UTC" {
		t.Fatalf("default: %v, %v", loc, err)
	}
	st.cfg.Timezone = "Nowhere/Land"
	if _, err := svc.Location(context.Background()); !errors.Is(err, ErrInvalidTimezone) {
		t.Errorf("an unloadable stored zone: %v, want ErrInvalidTimezone, not a quiet UTC", err)
	}
}

// The runner reads the zone through this interface; the compiler, not a
// test, should be what notices the Service ever stops satisfying it.
var _ ZoneSource = (*Service)(nil)
