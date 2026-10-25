// SPDX-License-Identifier: AGPL-3.0-or-later

package containerlogs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

var admin = rbac.Actor{Tier: db.TierAdmin, UserID: "admin-1"}

func TestUpdateRetention_AdminOnlyAndRangeChecked(t *testing.T) {
	f := newFixture()
	ctx := context.Background()
	if err := f.svc.UpdateRetention(ctx, developer, 6); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v, want ErrNotPermitted", err)
	}
	for _, bad := range []int{0, -1, 25, 1000} {
		if err := f.svc.UpdateRetention(ctx, admin, bad); !errors.Is(err, ErrInvalidRetention) {
			t.Errorf("%d: %v, want ErrInvalidRetention", bad, err)
		}
	}
	if len(f.settings.updates) != 0 || len(f.audit.calls) != 0 {
		t.Errorf("a refused update changed something: %v / %d audit", f.settings.updates, len(f.audit.calls))
	}
	for _, ok := range []int{1, 12, 24} {
		if err := f.svc.UpdateRetention(ctx, admin, ok); err != nil {
			t.Errorf("%d: %v", ok, err)
		}
	}
}

func TestUpdateRetention_IsAuditedWithWhoAndWhat(t *testing.T) {
	f := newFixture()
	if err := f.svc.UpdateRetention(context.Background(), admin, 6); err != nil {
		t.Fatal(err)
	}
	if len(f.audit.calls) != 1 {
		t.Fatalf("%d audit records, want 1", len(f.audit.calls))
	}
	c := f.audit.calls[0]
	if c.action != "updated_container_log_retention" || c.actorID == nil || *c.actorID != "admin-1" || c.superAdmin || c.detail["retention_months"] != 6 || c.detail["previous_retention_months"] != 12 {
		t.Errorf("audit = %+v", c)
	}

	f2 := newFixture()
	if err := f2.svc.UpdateRetention(context.Background(), rbac.Actor{IsSuperAdmin: true}, 3); err != nil {
		t.Fatal(err)
	}
	if c := f2.audit.calls[0]; c.actorID != nil || !c.superAdmin {
		t.Errorf("break-glass audit = %+v, want a nil actor with the super-admin flag", c)
	}
}

func TestUpdateRetention_AFailedAuditWriteRevertsTheChange(t *testing.T) {
	f := newFixture()
	f.audit.err = errors.New("audit down")
	if err := f.svc.UpdateRetention(context.Background(), admin, 3); err == nil {
		t.Fatal("UpdateRetention succeeded without an audit record")
	}
	if f.settings.cfg.RetentionMonths != 12 {
		t.Errorf("retention is %d after a failed audit, want it reverted to 12", f.settings.cfg.RetentionMonths)
	}
}

func TestSettings_AdminOnly(t *testing.T) {
	f := newFixture()
	if _, err := f.svc.Settings(context.Background(), developer); !errors.Is(err, rbac.ErrNotPermitted) {
		t.Errorf("developer: %v", err)
	}
	cfg, err := f.svc.Settings(context.Background(), admin)
	if err != nil || cfg.RetentionMonths != 12 {
		t.Errorf("admin: %+v, %v", cfg, err)
	}
}

func TestExpireOnce_DeletesPastTheCutoffAndAuditsAsTheSystem(t *testing.T) {
	f := newFixture()
	f.settings.cfg.RetentionMonths = 6
	f.store.deleted = 4
	n, err := f.svc.ExpireOnce(context.Background())
	if err != nil || n != 4 {
		t.Fatalf("ExpireOnce() = %d, %v", n, err)
	}
	want := f.now.AddDate(0, -6, 0)
	if len(f.store.cutoffs) != 1 || !f.store.cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v (six months before now)", f.store.cutoffs, want)
	}
	if len(f.audit.calls) != 1 {
		t.Fatalf("%d audit records, want 1", len(f.audit.calls))
	}
	c := f.audit.calls[0]
	if c.action != "expired_container_logs" || c.actorID != nil || c.superAdmin || c.detail["deleted"] != int64(4) || c.detail["retention_months"] != 6 {
		t.Errorf("audit = %+v, want a system record (nil actor, not break-glass)", c)
	}
}

func TestExpireOnce_NothingExpiredWritesNoAuditRecord(t *testing.T) {
	f := newFixture()
	if n, err := f.svc.ExpireOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("ExpireOnce() = %d, %v", n, err)
	}
	if len(f.audit.calls) != 0 {
		t.Errorf("%d audit records for a run that deleted nothing", len(f.audit.calls))
	}
}

func TestExpireOnce_ErrorsAreReturned(t *testing.T) {
	f := newFixture()
	f.settings.getErr = errors.New("db down")
	if _, err := f.svc.ExpireOnce(context.Background()); err == nil {
		t.Error("settings failure swallowed")
	}

	f = newFixture()
	f.store.deleted = 2
	f.audit.err = errors.New("audit down")
	n, err := f.svc.ExpireOnce(context.Background())
	if err == nil || n != 2 {
		t.Errorf("ExpireOnce() = %d, %v, want the count and the audit failure reported", n, err)
	}
}

func TestRunRetention_StopsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.svc.RunRetention(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRetention did not stop")
	}
	if len(f.store.cutoffs) != 0 {
		t.Error("the first run should wait for its start-up delay, not run immediately")
	}
}
