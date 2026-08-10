// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"net/http"
	"sync/atomic"
)

// setupGate blocks every route until the break-glass credential has been
// set (see cmd/sparky-server's setup.go for why that specific state is
// the setup-complete signal) - ARCHITECTURE.md Application Lifecycle's
// Setup Check: "if minimal config doesn't exist, refuse to serve normal
// routes; direct operator to `sparky setup`."
//
// complete is checked against the database only until it first observes
// setup as done, then cached forever - setup is a one-time action with no
// "unset" path, so there's no reason to keep re-querying on every request
// once it has been observed complete. This also means an operator running
// `sparky-server setup` in another terminal while the server is already up
// takes effect on the very next request, without a restart.
type setupGate struct {
	store    breakGlassStore
	complete atomic.Bool
}

func newSetupGate(store breakGlassStore) *setupGate {
	return &setupGate{store: store}
}

func (g *setupGate) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.complete.Load() {
			if _, err := g.store.Get(r.Context()); err != nil {
				writeError(w, r, http.StatusServiceUnavailable, "SETUP_REQUIRED", "run `sparky-server setup` before using this server")
				return
			}
			g.complete.Store(true)
		}
		next.ServeHTTP(w, r)
	})
}
