// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/alerts"
	"github.com/1kaius1/Sparky/internal/db"
)

// fakeAlertRaiser implements alertRaiser for tests, recording what the
// service asked for.
type fakeAlertRaiser struct {
	raised      []alerts.RaiseParams
	resolved    []string
	raiseErr    error
	resolveErr  error
	resolveHits int
}

func (f *fakeAlertRaiser) Raise(_ context.Context, p alerts.RaiseParams) (*db.RaiseResult, error) {
	f.raised = append(f.raised, p)
	if f.raiseErr != nil {
		return nil, f.raiseErr
	}
	return &db.RaiseResult{Alert: &db.Alert{ID: "alert-1"}, Created: true}, nil
}

func (f *fakeAlertRaiser) ResolveCondition(_ context.Context, key string) (int, error) {
	f.resolved = append(f.resolved, key)
	return f.resolveHits, f.resolveErr
}

const deadUserID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

// deadAlertService builds a Service whose instance store reports one
// instance (previously prevHealth) started by deadUserID.
func deadAlertService(prevHealth db.InstanceHealthStatus, startedBy *string) (*Service, *fakeInstanceStore, *fakeAlertRaiser) {
	instances := &fakeInstanceStore{findByIDResult: &db.RunningInstance{
		ID: "instance-1", ProfileID: "profile-1", StartedBy: startedBy,
		Status: db.RunningInstanceStatusRunning, HealthStatus: prevHealth,
	}}
	raiser := &fakeAlertRaiser{}
	profiles := &fakeProfileLookup{profile: &db.Profile{ID: "profile-1", Name: "Qwen3-8B"}}
	svc := NewService(profiles, instances, &fakeAdapterRegistry{}, &fakeDispatcher{}, &fakeAuditRecorder{}, raiser, testLogger())
	return svc, instances, raiser
}

func healthReport(t *testing.T, status string) agentproto.Envelope {
	t.Helper()
	return newInstanceHealthEnvelope(t, agentproto.InstanceHealth{InstanceID: "instance-1", Status: status, CheckedAt: time.Now()})
}

func TestHandleInstanceHealth_Dead_RaisesAnAlertForTheStarterAndAdmins(t *testing.T) {
	user := deadUserID
	svc, instances, raiser := deadAlertService(db.InstanceHealthHealthy, &user)

	svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))

	if len(instances.healthCalls) != 1 {
		t.Fatalf("UpdateHealth called %d times, want 1", len(instances.healthCalls))
	}
	if len(raiser.raised) != 1 {
		t.Fatalf("Raise called %d times, want 1", len(raiser.raised))
	}
	p := raiser.raised[0]
	if p.Kind != "instance_dead" || p.Severity != db.AlertSeverityCritical {
		t.Errorf("kind/severity = %q/%q", p.Kind, p.Severity)
	}
	if p.Title != "Qwen3-8B is no longer running" {
		t.Errorf("title = %q", p.Title)
	}
	if p.DedupeKey != "instance_dead:instance-1" || p.ObjectType != "running_instance" || p.ObjectID != "instance-1" {
		t.Errorf("dedupe/object = %q %q %q", p.DedupeKey, p.ObjectType, p.ObjectID)
	}
	var sawAdmin, sawStarter bool
	for _, a := range p.Audiences {
		switch {
		case a.Type == db.AlertAudienceTier && a.MinTier == db.TierAdmin:
			sawAdmin = true
		case a.Type == db.AlertAudienceUser && a.UserID == deadUserID:
			sawStarter = true
		}
	}
	if !sawAdmin || !sawStarter || len(p.Audiences) != 2 {
		t.Errorf("audiences = %+v, want the Admin tier and the starter", p.Audiences)
	}
	if len(raiser.resolved) != 0 {
		t.Errorf("a dead report tried to close alerts: %v", raiser.resolved)
	}
}

func TestHandleInstanceHealth_Dead_NoStarterMeansAdminsOnly(t *testing.T) {
	svc, _, raiser := deadAlertService(db.InstanceHealthHealthy, nil)

	svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))

	if len(raiser.raised) != 1 {
		t.Fatalf("Raise called %d times, want 1", len(raiser.raised))
	}
	if auds := raiser.raised[0].Audiences; len(auds) != 1 || auds[0].Type != db.AlertAudienceTier || auds[0].MinTier != db.TierAdmin {
		t.Errorf("audiences = %+v, want only the Admin tier", auds)
	}
}

func TestHandleInstanceHealth_Dead_AlreadyDeadRaisesNothingNew(t *testing.T) {
	user := deadUserID
	svc, instances, raiser := deadAlertService(db.InstanceHealthDead, &user)

	svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))

	// The health is still recorded (its check time moves), but the minute-by-
	// minute reports of an instance that stays dead must not re-raise.
	if len(instances.healthCalls) != 1 {
		t.Errorf("UpdateHealth called %d times, want 1", len(instances.healthCalls))
	}
	if len(raiser.raised) != 0 {
		t.Errorf("Raise called %d times for an instance that was already dead", len(raiser.raised))
	}
}

func TestHandleInstanceHealth_Dead_FallsBackWhenTheProfileCannotBeRead(t *testing.T) {
	user := deadUserID
	svc, _, raiser := deadAlertService(db.InstanceHealthHealthy, &user)
	svc.profiles = &fakeProfileLookup{err: errors.New("db down")}

	svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))

	if len(raiser.raised) != 1 || raiser.raised[0].Title != "An instance is no longer running" {
		t.Errorf("raised = %+v, want a generic title", raiser.raised)
	}
}

func TestHandleInstanceHealth_Dead_AlertTroubleNeverBlocksHealth(t *testing.T) {
	t.Run("instance lookup fails", func(t *testing.T) {
		svc, instances, raiser := deadAlertService(db.InstanceHealthHealthy, nil)
		instances.findByIDErr = errors.New("db down")
		svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))
		if len(instances.healthCalls) != 1 {
			t.Errorf("UpdateHealth called %d times, want 1", len(instances.healthCalls))
		}
		if len(raiser.raised) != 0 {
			t.Error("an alert was raised without being able to read the instance")
		}
	})
	t.Run("raise fails", func(t *testing.T) {
		svc, instances, raiser := deadAlertService(db.InstanceHealthHealthy, nil)
		raiser.raiseErr = errors.New("alerts down")
		svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))
		if len(instances.healthCalls) != 1 {
			t.Errorf("UpdateHealth called %d times, want 1", len(instances.healthCalls))
		}
	})
	t.Run("health update fails", func(t *testing.T) {
		svc, instances, raiser := deadAlertService(db.InstanceHealthHealthy, nil)
		instances.updateHealthErr = errors.New("db down")
		svc.HandleInstanceHealth("node-1", healthReport(t, agentproto.InstanceHealthStatusDead))
		if len(raiser.raised) != 0 {
			t.Error("an alert was raised although the dead state was not recorded")
		}
	})
}

func TestHandleInstanceHealth_NotDead_ClosesTheDeadAlert(t *testing.T) {
	for _, status := range []string{agentproto.InstanceHealthStatusHealthy, agentproto.InstanceHealthStatusUnhealthy} {
		t.Run(status, func(t *testing.T) {
			svc, _, raiser := deadAlertService(db.InstanceHealthDead, nil)
			svc.HandleInstanceHealth("node-1", healthReport(t, status))
			if len(raiser.resolved) != 1 || raiser.resolved[0] != "instance_dead:instance-1" {
				t.Errorf("resolved = %v, want the instance's dead-alert key", raiser.resolved)
			}
			if len(raiser.raised) != 0 {
				t.Error("a live report raised an alert")
			}
		})
	}
}

func TestHandleInstanceResult_StoppedOrFailedClosesTheDeadAlert(t *testing.T) {
	cases := []struct {
		status string
		closes bool
	}{
		{agentproto.InstanceStatusStopped, true},
		{agentproto.InstanceStatusFailed, true},
		{agentproto.InstanceStatusRunning, false},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			svc, _, raiser := deadAlertService(db.InstanceHealthDead, nil)
			svc.HandleInstanceResult("node-1", newInstanceResultEnvelope(t, agentproto.InstanceResult{InstanceID: "instance-1", Status: c.status}))
			if got := len(raiser.resolved) == 1; got != c.closes {
				t.Errorf("closed = %v, want %v (resolved=%v)", got, c.closes, raiser.resolved)
			}
		})
	}

	// A status write that failed must not close the alert: the row still says
	// the instance is there.
	svc, instances, raiser := deadAlertService(db.InstanceHealthDead, nil)
	instances.setStatusErr = errors.New("db down")
	svc.HandleInstanceResult("node-1", newInstanceResultEnvelope(t, agentproto.InstanceResult{InstanceID: "instance-1", Status: agentproto.InstanceStatusStopped}))
	if len(raiser.resolved) != 0 {
		t.Error("the alert was closed although the status write failed")
	}
}
