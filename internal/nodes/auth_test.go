// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"context"
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
)

type fakeCredentialStore struct {
	cred    *db.NodeCredential
	findErr error
}

func (f *fakeCredentialStore) FindCredentialByName(_ context.Context, name string) (*db.NodeCredential, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.cred == nil || f.cred.Node.Name != name {
		return nil, db.ErrNodeNotFound
	}
	return f.cred, nil
}

func TestAuthService_Authenticate_Success(t *testing.T) {
	token, err := auth.GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	store := &fakeCredentialStore{
		cred: &db.NodeCredential{
			Node:            db.Node{ID: "node-1", Name: "spark-1"},
			BearerTokenHash: auth.HashNodeToken(token),
		},
	}
	svc := NewAuthService(store)

	node, err := svc.Authenticate(context.Background(), "spark-1", token)
	if err != nil {
		t.Fatalf("Authenticate() error: %v", err)
	}
	if node.ID != "node-1" {
		t.Errorf("ID = %q, want %q", node.ID, "node-1")
	}
}

func TestAuthService_Authenticate_UnknownNode(t *testing.T) {
	store := &fakeCredentialStore{}
	svc := NewAuthService(store)

	_, err := svc.Authenticate(context.Background(), "no-such-node", "any-token")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthService_Authenticate_WrongToken(t *testing.T) {
	token, err := auth.GenerateNodeToken()
	if err != nil {
		t.Fatalf("GenerateNodeToken() error: %v", err)
	}
	store := &fakeCredentialStore{
		cred: &db.NodeCredential{
			Node:            db.Node{ID: "node-1", Name: "spark-1"},
			BearerTokenHash: auth.HashNodeToken(token),
		},
	}
	svc := NewAuthService(store)

	_, err = svc.Authenticate(context.Background(), "spark-1", "wrong-token")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Authenticate() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthService_Authenticate_StoreError(t *testing.T) {
	store := &fakeCredentialStore{findErr: errors.New("database unreachable")}
	svc := NewAuthService(store)

	_, err := svc.Authenticate(context.Background(), "spark-1", "any-token")
	if err == nil {
		t.Fatal("Authenticate() succeeded despite a store failure")
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Error("Authenticate() returned ErrInvalidCredentials for an infrastructure failure, want a distinct error")
	}
}
