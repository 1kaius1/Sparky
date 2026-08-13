// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/1kaius1/Sparky/internal/events"
	"github.com/1kaius1/Sparky/internal/session"
)

func newEventsTestServer(t *testing.T, broker *events.Broker) *httptest.Server {
	t.Helper()
	api := newTestDashboardAPIWithEvents(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, &fakeProfileEditor{}, &fakeInstanceLister{}, &fakeInstanceLauncher{}, &fakeTransferLister{}, newFakeUserLister(), &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{}, broker)
	srv := httptest.NewServer(api.Router())
	t.Cleanup(srv.Close)
	return srv
}

func authenticatedEventsRequest(t *testing.T, srv *httptest.Server, userID string) *http.Request {
	t.Helper()
	cookieValue, err := session.Sign(testSessionSecret, session.New(userID, sessionDuration))
	if err != nil {
		t.Fatalf("session.Sign() error: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookieValue})
	return req
}

// TestHandleEvents_StreamsPublishedEvents exercises the real network path
// (httptest.NewServer, a real http.Client) rather than
// httptest.NewRecorder - a streaming handler and a concurrently-read
// response body over a plain in-memory recorder would race on its
// unsynchronized bytes.Buffer, which a real connection doesn't have.
// handleEvents subscribes before its first flush (see its own doc
// comment/body), so by the time http.Client.Do returns a response, the
// subscription is already registered - Publish afterward is guaranteed
// deliverable, not a race to publish before Subscribe runs.
func TestHandleEvents_StreamsPublishedEvents(t *testing.T) {
	broker := events.NewBroker()
	srv := newEventsTestServer(t, broker)

	// A bounded request context, not a bare read loop with a wall-clock
	// deadline check - if nothing arrives, bufio.Reader.ReadString blocks
	// on the underlying connection with nothing else polling it; canceling
	// the request context is what actually unblocks that read (with an
	// error), giving a clean timeout instead of an indefinitely hung test.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := authenticatedEventsRequest(t, srv, "user-1").WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", got, "text/event-stream")
	}

	broker.Publish(events.Event{Type: "telemetry"})

	reader := bufio.NewReader(resp.Body)
	line1, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() error: %v", err)
	}
	line2, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() error: %v", err)
	}

	got := line1 + line2
	if got != "event: telemetry\ndata: {}\n" {
		t.Errorf("streamed event = %q, want %q", got, "event: telemetry\ndata: {}\n")
	}
}

func TestHandleEvents_Unauthenticated(t *testing.T) {
	srv := newEventsTestServer(t, events.NewBroker())

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestHandleEvents_CloseUnsubscribes confirms a client disconnect actually
// releases the handler goroutine (and, with it, its broker subscription) -
// httptest.Server.Close (via t.Cleanup) blocks until every in-flight
// connection's handler returns, so a bounded-time Close after the client
// closes its side proves handleEvents observed the cancellation rather
// than leaking the goroutine/subscription forever.
func TestHandleEvents_CloseUnsubscribes(t *testing.T) {
	broker := events.NewBroker()
	srv := newEventsTestServer(t, broker)

	resp, err := http.DefaultClient.Do(authenticatedEventsRequest(t, srv, "user-1"))
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not close within the timeout - handler likely leaked")
	}
}
