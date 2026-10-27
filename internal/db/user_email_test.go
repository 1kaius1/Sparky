// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func createTestLocalUser(t *testing.T, users *UserRepository, name string) *User {
	t.Helper()
	u, err := users.CreateLocal(context.Background(), name, "hash", "Local "+name, TierDeveloper)
	if err != nil {
		t.Fatalf("create local user: %v", err)
	}
	t.Cleanup(func() { _, _ = users.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID) })
	return u
}

func str(s string) *string { return &s }

func TestUserEmailRepository_DirectoryAddressForAnADAccountOnly(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	ctx := context.Background()
	ad := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-ad", t.Name()))
	local := createTestLocalUser(t, users, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))

	got, err := repo.Get(ctx, ad.ID)
	if err != nil || !got.Directory || got.Email != nil || got.Source != nil {
		t.Fatalf("a new AD user: %+v, %v", got, err)
	}
	if changed, err := repo.SetDirectory(ctx, ad.ID, str("ana@example.org")); err != nil || !changed {
		t.Fatalf("SetDirectory() = %v, %v", changed, err)
	}
	got, _ = repo.Get(ctx, ad.ID)
	if got.Email == nil || *got.Email != "ana@example.org" || got.Source == nil || *got.Source != EmailSourceDirectory {
		t.Fatalf("after SetDirectory: %+v", got)
	}
	// Setting the same address again changes nothing; AD dropping the address
	// clears it.
	if changed, _ := repo.SetDirectory(ctx, ad.ID, str("ana@example.org")); changed {
		t.Error("an unchanged address reported a change")
	}
	if changed, err := repo.SetDirectory(ctx, ad.ID, nil); err != nil || !changed {
		t.Fatalf("clearing: %v, %v", changed, err)
	}
	got, _ = repo.Get(ctx, ad.ID)
	if got.Email != nil || got.Source != nil {
		t.Errorf("after clearing: %+v", got)
	}

	// The directory's address never lands on a local account, and a local
	// address never lands on an AD one.
	if changed, err := repo.SetDirectory(ctx, local.ID, str("x@example.org")); err != nil || changed {
		t.Errorf("SetDirectory on a local account = %v, %v; want no change", changed, err)
	}
	if changed, err := repo.SetLocal(ctx, ad.ID, str("x@example.org"), EmailSourceSelf); err != nil || changed {
		t.Errorf("SetLocal on an AD account = %v, %v; want no change", changed, err)
	}
	if got, _ := repo.Get(ctx, local.ID); got.Email != nil || got.Directory {
		t.Errorf("local account = %+v", got)
	}
	if _, err := repo.Get(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestUserEmailRepository_LocalAddressAndTheDatabaseChecks(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	ctx := context.Background()
	ad := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-ad", t.Name()))
	local := createTestLocalUser(t, users, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))

	if changed, err := repo.SetLocal(ctx, local.ID, str("bo@example.org"), EmailSourceSelf); err != nil || !changed {
		t.Fatalf("SetLocal() = %v, %v", changed, err)
	}
	got, _ := repo.Get(ctx, local.ID)
	if got.Directory || got.Email == nil || *got.Email != "bo@example.org" || *got.Source != EmailSourceSelf {
		t.Fatalf("after SetLocal: %+v", got)
	}
	if changed, _ := repo.SetLocal(ctx, local.ID, str("bo@example.org"), EmailSourceAdmin); !changed {
		t.Error("changing only the source was not a change")
	}
	if changed, err := repo.SetLocal(ctx, local.ID, nil, EmailSourceAdmin); err != nil || !changed {
		t.Errorf("clearing: %v, %v", changed, err)
	}

	// The CHECKs behind it: an address needs a source, and a source must match
	// the kind of account.
	bad := map[string]string{
		"address without a source":           fmt.Sprintf(`UPDATE users SET email = 'x@example.org', email_source = NULL WHERE id = '%s'`, local.ID),
		"source without an address":          fmt.Sprintf(`UPDATE users SET email = NULL, email_source = 'self' WHERE id = '%s'`, local.ID),
		"a directory address on a local one": fmt.Sprintf(`UPDATE users SET email = 'x@example.org', email_source = 'directory' WHERE id = '%s'`, local.ID),
		"a self address on an AD account":    fmt.Sprintf(`UPDATE users SET email = 'x@example.org', email_source = 'self' WHERE id = '%s'`, ad.ID),
		"an admin address on an AD account":  fmt.Sprintf(`UPDATE users SET email = 'x@example.org', email_source = 'admin' WHERE id = '%s'`, ad.ID),
	}
	for name, q := range bad {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was accepted by the database", name)
		}
	}
}

func TestUserEmailRepository_Confirmations(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	ctx := context.Background()
	local := createTestLocalUser(t, users, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	admin := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-admin", t.Name()))

	if _, err := repo.PendingConfirmation(ctx, local.ID); !errors.Is(err, ErrEmailConfirmationNotFound) {
		t.Fatalf("nothing pending: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := EmailConfirmation{UserID: local.ID, Email: "one@example.org", TokenHash: "hash-one-" + local.ID, Source: EmailSourceSelf, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := repo.PutConfirmation(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ConfirmationByTokenHash(ctx, first.TokenHash)
	if err != nil || got.UserID != local.ID || got.Email != "one@example.org" || got.Source != EmailSourceSelf || got.RequestedBy != nil || !got.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("by token: %+v, %v", got, err)
	}

	// A new request replaces the old one: the old token stops working.
	second := EmailConfirmation{UserID: local.ID, Email: "two@example.org", TokenHash: "hash-two-" + local.ID, Source: EmailSourceAdmin, RequestedBy: &admin.ID, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := repo.PutConfirmation(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ConfirmationByTokenHash(ctx, first.TokenHash); !errors.Is(err, ErrEmailConfirmationNotFound) {
		t.Errorf("the replaced token still works: %v", err)
	}
	pending, err := repo.PendingConfirmation(ctx, local.ID)
	if err != nil || pending.Email != "two@example.org" || pending.Source != EmailSourceAdmin || pending.RequestedBy == nil || *pending.RequestedBy != admin.ID {
		t.Fatalf("pending: %+v, %v", pending, err)
	}

	// The database refuses a source that is not self or admin, and a token hash
	// shared by two users.
	other := createTestLocalUser(t, users, fmt.Sprintf("%s-other-%d", t.Name(), time.Now().UnixNano()))
	if err := repo.PutConfirmation(ctx, EmailConfirmation{UserID: other.ID, Email: "x@example.org", TokenHash: "h", Source: EmailSourceDirectory, CreatedAt: now, ExpiresAt: now}); err == nil {
		t.Error("a directory-sourced confirmation was accepted")
	}
	if err := repo.PutConfirmation(ctx, EmailConfirmation{UserID: other.ID, Email: "x@example.org", TokenHash: second.TokenHash, Source: EmailSourceSelf, CreatedAt: now, ExpiresAt: now}); err == nil {
		t.Error("two users shared a token hash")
	}

	// Expired ones are cleaned up; deleting reports whether there was one.
	if err := repo.PutConfirmation(ctx, EmailConfirmation{UserID: other.ID, Email: "old@example.org", TokenHash: "old-" + other.ID, Source: EmailSourceSelf, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.DeleteExpiredConfirmations(ctx, now); err != nil || n < 1 {
		t.Fatalf("DeleteExpiredConfirmations() = %d, %v", n, err)
	}
	if _, err := repo.PendingConfirmation(ctx, other.ID); !errors.Is(err, ErrEmailConfirmationNotFound) {
		t.Error("the expired confirmation survived")
	}
	if _, err := repo.PendingConfirmation(ctx, local.ID); err != nil {
		t.Errorf("an unexpired confirmation was removed: %v", err)
	}
	if ok, err := repo.DeleteConfirmation(ctx, local.ID); err != nil || !ok {
		t.Errorf("DeleteConfirmation() = %v, %v", ok, err)
	}
	if ok, _ := repo.DeleteConfirmation(ctx, local.ID); ok {
		t.Error("deleting nothing reported a deletion")
	}

	// Deleting the user takes the pending change with it.
	if err := repo.PutConfirmation(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ConfirmationByTokenHash(ctx, first.TokenHash); !errors.Is(err, ErrEmailConfirmationNotFound) {
		t.Error("a deleted user's confirmation survived")
	}
}

func TestUserEmailRepository_ListAll(t *testing.T) {
	pool := newTestPool(t)
	users, repo := NewUserRepository(pool), NewUserEmailRepository(pool)
	ctx := context.Background()
	ad := createTestUser(t, users, fmt.Sprintf("S-1-TEST-%s-ad", t.Name()))
	local := createTestLocalUser(t, users, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()))
	_, _ = repo.SetDirectory(ctx, ad.ID, str("ad@example.org"))

	all, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a := all[ad.ID]; a == nil || !a.Directory || a.Email == nil || *a.Email != "ad@example.org" {
		t.Errorf("AD user = %+v", a)
	}
	if l := all[local.ID]; l == nil || l.Directory || l.Email != nil {
		t.Errorf("local user = %+v", l)
	}
}
