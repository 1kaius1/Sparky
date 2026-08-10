// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

func newGateTestServer(store breakGlassStore) *httptest.Server {
	gate := newSetupGate(store)
	handler := gate.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// gate.middleware alone doesn't set X-Request-ID, but writeError reads
	// it from context via chi's middleware.GetReqID, which returns "" if
	// absent - fine for these tests, which only check status/body.
	return httptest.NewServer(handler)
}

func TestSetupGate_NotConfigured_Blocks(t *testing.T) {
	srv := newGateTestServer(newFakeBreakGlassStore())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestSetupGate_Configured_Allows(t *testing.T) {
	srv := newGateTestServer(newConfiguredFakeBreakGlassStore())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestSetupGate_StoreError_Blocks(t *testing.T) {
	srv := newGateTestServer(&fakeBreakGlassStore{err: errors.New("database unreachable")})
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestSetupGate_CachesCompleteState(t *testing.T) {
	// A store that starts configured, then would report an error on any
	// subsequent call - if the gate correctly caches "complete" after the
	// first successful check, it must never call Get again, so this
	// second request must still succeed despite the store now failing.
	store := &onceThenErrorStore{cred: &db.BreakGlassCredential{PasswordHash: "x"}}
	srv := newGateTestServer(store)
	defer srv.Close()

	for i, wantStatus := range []int{http.StatusOK, http.StatusOK} {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET #%d error: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Errorf("GET #%d status = %d, want %d", i, resp.StatusCode, wantStatus)
		}
	}

	if store.calls != 1 {
		t.Errorf("store.Get() was called %d times, want exactly 1 (gate should cache after first success)", store.calls)
	}
}

// onceThenErrorStore returns cred on its first call and errors on every
// call after that, so a test can prove the gate stops querying once it
// has observed setup as complete.
type onceThenErrorStore struct {
	cred  *db.BreakGlassCredential
	calls int
}

func (s *onceThenErrorStore) Get(_ context.Context) (*db.BreakGlassCredential, error) {
	s.calls++
	if s.calls > 1 {
		return nil, errors.New("should not be called again after setup is observed complete")
	}
	return s.cred, nil
}
