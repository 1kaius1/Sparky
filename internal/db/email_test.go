// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEmailSettingsRepository_GetUpdateRestoreAndChecks(t *testing.T) {
	pool := newTestPool(t)
	repo := NewEmailSettingsRepository(pool)
	ctx := context.Background()

	original, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Restore(ctx, original) })

	good := EmailSettings{Enabled: true, Host: "relay.example.com", Port: 587, TLSMode: EmailTLSStartTLS, Username: "svc", FromAddress: "sparky@example.com", FromName: "Sparky"}
	if err := repo.Update(ctx, good); err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	got, _ := repo.Get(ctx)
	if !got.Enabled || got.Host != "relay.example.com" || got.Port != 587 || got.TLSMode != EmailTLSStartTLS || got.Username != "svc" || got.FromAddress != "sparky@example.com" || got.UpdatedBy != nil {
		t.Fatalf("after update: %+v", got)
	}

	bad := map[string]func(*EmailSettings){
		"enabled with no host":         func(s *EmailSettings) { s.Host = "" },
		"enabled with no from address": func(s *EmailSettings) { s.FromAddress = "" },
		"port 0":                       func(s *EmailSettings) { s.Port = 0 },
		"port 65536":                   func(s *EmailSettings) { s.Port = 65536 },
		"unknown TLS mode":             func(s *EmailSettings) { s.TLSMode = "ssl" },
	}
	for name, mutate := range bad {
		s := good
		mutate(&s)
		if err := repo.Update(ctx, s); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Off needs no relay.
	off := EmailSettings{Enabled: false, Port: 25, TLSMode: EmailTLSNone}
	if err := repo.Update(ctx, off); err != nil {
		t.Errorf("email off with no relay: %v", err)
	}

	if err := repo.Restore(ctx, got); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}
	back, _ := repo.Get(ctx)
	if back.Host != got.Host || back.Enabled != got.Enabled || !back.UpdatedAt.Equal(got.UpdatedAt) {
		t.Errorf("restored %+v, want %+v", back, got)
	}
	if err := repo.Restore(ctx, nil); err == nil {
		t.Error("Restore(nil) was accepted")
	}
}

// queueFixture queues emails for a test and removes them afterwards.
func queueFixture(t *testing.T) (*EmailQueueRepository, func(addr string) *Email) {
	t.Helper()
	pool := newTestPool(t)
	repo := NewEmailQueueRepository(pool)
	prefix := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM email_queue WHERE to_address LIKE $1`, prefix+"%")
	})
	return repo, func(addr string) *Email {
		e, err := repo.Enqueue(context.Background(), NewEmail{Kind: EmailKindAlert, ToAddress: prefix + addr, Subject: "s " + addr, Body: "b"})
		if err != nil {
			t.Fatalf("Enqueue() error: %v", err)
		}
		return e
	}
}

func TestEmailQueueRepository_EnqueueClaimAndLease(t *testing.T) {
	repo, enqueue := queueFixture(t)
	ctx := context.Background()

	a := enqueue("a@x.com")
	if a.Status != EmailPending || a.Attempts != 0 || a.Kind != EmailKindAlert || a.SentAt != nil {
		t.Fatalf("enqueued = %+v", a)
	}

	claimed, err := repo.Claim(ctx, 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var mine *Email
	for _, c := range claimed {
		if c.ID == a.ID {
			mine = c
		}
	}
	if mine == nil || mine.Attempts != 1 || !mine.NextAttemptAt.After(time.Now().Add(30*time.Second)) {
		t.Fatalf("claimed = %+v, want it with one attempt counted and the next try a lease away", mine)
	}

	// While leased it is not handed out again.
	again, _ := repo.Claim(ctx, 1000, time.Minute)
	for _, c := range again {
		if c.ID == a.ID {
			t.Error("an email under lease was claimed a second time")
		}
	}
}

func TestEmailQueueRepository_ConcurrentClaimsNeverShareARow(t *testing.T) {
	repo, enqueue := queueFixture(t)
	ctx := context.Background()
	ids := map[string]bool{}
	for i := 0; i < 30; i++ {
		ids[enqueue(fmt.Sprintf("u%d@x.com", i)).ID] = true
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch, err := repo.Claim(ctx, 5, time.Minute)
				if err != nil {
					t.Errorf("Claim() error: %v", err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, e := range batch {
					seen[e.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for id := range ids {
		if seen[id] != 1 {
			t.Errorf("email %s was claimed %d times, want exactly once", id, seen[id])
		}
	}
}

func TestEmailQueueRepository_SentRetryFailedAndTheirTransitions(t *testing.T) {
	repo, enqueue := queueFixture(t)
	ctx := context.Background()
	pool := repo.pool
	state := func(id string) (EmailStatus, int, *string) {
		var st EmailStatus
		var n int
		var le *string
		if err := pool.QueryRow(ctx, `SELECT status, attempts, last_error FROM email_queue WHERE id = $1`, id).Scan(&st, &n, &le); err != nil {
			t.Fatal(err)
		}
		return st, n, le
	}

	sent, retry, failed := enqueue("sent@x.com"), enqueue("retry@x.com"), enqueue("failed@x.com")
	if _, err := repo.Claim(ctx, 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSent(ctx, sent.ID); err != nil {
		t.Fatal(err)
	}
	if st, _, le := state(sent.ID); st != EmailSent || le != nil {
		t.Errorf("sent: %s %v", st, le)
	}
	next := time.Now().Add(-time.Second) // already due again
	if err := repo.MarkRetry(ctx, retry.ID, next, "connection refused"); err != nil {
		t.Fatal(err)
	}
	if st, n, le := state(retry.ID); st != EmailPending || n != 1 || le == nil || *le != "connection refused" {
		t.Errorf("retry: %s attempts %d error %v", st, n, le)
	}
	if err := repo.MarkFailed(ctx, failed.ID, "550 no such user"); err != nil {
		t.Fatal(err)
	}
	if st, _, le := state(failed.ID); st != EmailFailed || le == nil || *le != "550 no such user" {
		t.Errorf("failed: %s %v", st, le)
	}

	// A retry that is due is claimed again, with the attempt counted; a sent
	// or failed one never is.
	claimed, _ := repo.Claim(ctx, 1000, time.Minute)
	var gotRetry bool
	for _, c := range claimed {
		if c.ID == retry.ID && c.Attempts == 2 {
			gotRetry = true
		}
		if c.ID == sent.ID || c.ID == failed.ID {
			t.Errorf("a finished email was claimed again: %s", c.ID)
		}
	}
	if !gotRetry {
		t.Error("the retry was not claimed again with its second attempt counted")
	}

	// Marking a finished email as failed or retried does nothing.
	if err := repo.MarkFailed(ctx, sent.ID, "late"); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := state(sent.ID); st != EmailSent {
		t.Errorf("a sent email was changed to %s", st)
	}

	// The database refuses a sent email with no sent time.
	if _, err := pool.Exec(ctx, `UPDATE email_queue SET status = 'sent', sent_at = NULL WHERE id = $1`, retry.ID); err == nil {
		t.Error("the CHECK accepted status sent with no sent_at")
	}
}

func TestEmailQueueRepository_AgingStatsAndRecent(t *testing.T) {
	repo, enqueue := queueFixture(t)
	ctx := context.Background()
	pool := repo.pool

	oldPending, oldSent, fresh, failed := enqueue("oldp@x.com"), enqueue("olds@x.com"), enqueue("fresh@x.com"), enqueue("fail@x.com")
	if _, err := repo.Claim(ctx, 1000, time.Minute); err != nil {
		t.Fatal(err)
	}
	_ = repo.MarkSent(ctx, oldSent.ID)
	_ = repo.MarkFailed(ctx, failed.ID, "relay said no")
	for _, id := range []string{oldPending.ID, oldSent.ID} {
		if _, err := pool.Exec(ctx, `UPDATE email_queue SET created_at = now() - interval '40 days' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := repo.Stats(ctx)
	if err != nil || stats.Pending < 2 || stats.FailedLastWeek < 1 || stats.SentLastDay < 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if stats.LastError == "" || stats.LastErrorAt.IsZero() {
		t.Errorf("stats has no last error though one failed: %+v", stats)
	}

	// Old pending mail is given up on, with the reason; fresh mail is not.
	n, err := repo.FailOlderThan(ctx, time.Now().Add(-24*time.Hour), "too old to send")
	if err != nil || n < 1 {
		t.Fatalf("FailOlderThan() = %d, %v", n, err)
	}
	var st EmailStatus
	_ = pool.QueryRow(ctx, `SELECT status FROM email_queue WHERE id = $1`, oldPending.ID).Scan(&st)
	if st != EmailFailed {
		t.Errorf("old pending email is %s, want failed", st)
	}
	_ = pool.QueryRow(ctx, `SELECT status FROM email_queue WHERE id = $1`, fresh.ID).Scan(&st)
	if st != EmailPending {
		t.Errorf("fresh email is %s, want it left pending", st)
	}

	// Purging removes old finished mail and never a pending one.
	n, err = repo.DeleteOlderThan(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n < 2 {
		t.Fatalf("DeleteOlderThan() = %d, %v", n, err)
	}
	var remaining int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM email_queue WHERE id = ANY($1)`, []string{oldPending.ID, oldSent.ID, fresh.ID, failed.ID}).Scan(&remaining)
	if remaining != 2 {
		t.Errorf("%d of the four remain, want the fresh pending one and the recent failed one", remaining)
	}

	recent, err := repo.Recent(ctx, 2)
	if err != nil || len(recent) != 2 || recent[0].CreatedAt.Before(recent[1].CreatedAt) {
		t.Errorf("Recent(2) = %d rows, %v; want 2 newest first", len(recent), err)
	}
}
