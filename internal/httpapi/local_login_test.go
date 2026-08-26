// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/session"
)

func TestLocalLoginService_Login_Success(t *testing.T) {
	store := newFakeUserStore()
	username := "jsmith"
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("auth.HashPassword() error: %v", err)
	}
	store.byLocalUsername[username] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Jane Smith", Tier: db.TierDeveloper}
	store.localHashByUsername[username] = hash

	svc := NewLocalLoginService(store, testSessionSecret)

	user, cookieValue, err := svc.Login(context.Background(), username, "hunter2")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if user.ID != "user-1" {
		t.Errorf("ID = %q, want %q", user.ID, "user-1")
	}

	sess, err := session.Verify(testSessionSecret, cookieValue)
	if err != nil {
		t.Fatalf("session.Verify() error: %v", err)
	}
	if sess.UserID != user.ID {
		t.Errorf("session UserID = %q, want %q", sess.UserID, user.ID)
	}
	if !sess.IsLocalAccount {
		t.Error("session IsLocalAccount = false, want true")
	}
}

func TestLocalLoginService_Login_UnknownUsername(t *testing.T) {
	store := newFakeUserStore()
	svc := NewLocalLoginService(store, testSessionSecret)

	_, _, err := svc.Login(context.Background(), "does-not-exist", "hunter2")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login() error = %v, want auth.ErrInvalidCredentials", err)
	}
}

func TestLocalLoginService_Login_WrongPassword(t *testing.T) {
	store := newFakeUserStore()
	username := "jsmith"
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatalf("auth.HashPassword() error: %v", err)
	}
	store.byLocalUsername[username] = &db.User{ID: "user-1", LocalUsername: &username, DisplayName: "Jane Smith", Tier: db.TierDeveloper}
	store.localHashByUsername[username] = hash

	svc := NewLocalLoginService(store, testSessionSecret)

	_, _, err = svc.Login(context.Background(), username, "wrong-password")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Login() error = %v, want auth.ErrInvalidCredentials", err)
	}
}
