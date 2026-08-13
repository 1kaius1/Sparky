// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-server is the central Sparky application. See
// ARCHITECTURE.md Application Lifecycle for the full startup sequence.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/1kaius1/Sparky/internal/agentconn"
	"github.com/1kaius1/Sparky/internal/audit"
	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/httpapi"
	"github.com/1kaius1/Sparky/internal/lifecycle"
	"github.com/1kaius1/Sparky/internal/metrics"
	"github.com/1kaius1/Sparky/internal/nodes"
	"github.com/1kaius1/Sparky/internal/profiles"
	"github.com/1kaius1/Sparky/internal/rbac"
	"github.com/1kaius1/Sparky/internal/settings"
	"github.com/1kaius1/Sparky/internal/transfers"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests to finish before exiting anyway.
const shutdownTimeout = 10 * time.Second

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	// .env is a local development convenience only - see CLAUDE.md
	// Configuration and Environment Variables. It is never present in
	// production, so a missing file here is expected, not an error.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Printf("warning: failed to load .env: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	logger.Printf("configuration loaded (listen_port=%s log_level=%s)", cfg.ListenPort, cfg.LogLevel)

	ctx := context.Background()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetup(ctx, cfg, logger)
			return
		case "set-superadmin-password":
			runSetSuperAdminPassword(ctx, cfg, logger)
			return
		}
	}

	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer pool.Close()
	logger.Println("database connection pool established")

	identityProvider := auth.NewLDAPProvider(cfg.LDAPServerAddr, cfg.LDAPBindDN, cfg.LDAPBindPassword, cfg.LDAPBaseDN, cfg.LDAPAccessGroupDN)
	users := db.NewUserRepository(pool)
	loginService := httpapi.NewLoginService(identityProvider, users, cfg.SessionSecret)

	breakGlass := db.NewBreakGlassRepository(pool)
	breakGlassLoginService := httpapi.NewBreakGlassLoginService(breakGlass, cfg.SessionSecret)

	nodeRepo := db.NewNodeRepository(pool)
	nodeAuth := nodes.NewAuthService(nodeRepo)
	agentRegistry := agentconn.NewRegistry()
	// onMessage is nil - nothing dispatches a real command yet.
	// internal/transfers, internal/lifecycle, and internal/metrics each
	// have their own OnMessageFunc-shaped handler
	// (HandleTransferProgress/HandleInstanceResult/HandleTelemetry) but
	// agentconn.Handler only accepts one callback - combining the three
	// into a single dispatching OnMessageFunc is Dashboard UI Phase 3's
	// OnMessage-consolidation slice (PLANNING.md), deliberately not this
	// one - the Transfers page below only reads via ListTransfers, it
	// doesn't need this wired.
	agentConnHandler := agentconn.NewHandler(nodeAuth, nodeRepo, agentRegistry, logger, nil)

	auditRecorder := audit.NewRecorder(db.NewAuditRepository(pool), os.Stdout)
	engineRegistry := engines.NewRegistry()
	profileRepo := db.NewProfileRepository(pool)
	instanceRepo := db.NewRunningInstanceRepository(pool)

	transferRepo := db.NewModelTransferRepository(pool)
	inventoryRepo := db.NewNodeModelInventoryRepository(pool)
	overrideRepo := db.NewPermissionOverrideRepository(pool)

	// nodeService backs both the Nodes page's unguarded ListNodes read
	// and, as of Dashboard UI Phase 9, the registration form's own write
	// (RegisterNode) - the same value is passed twice below, once per
	// narrow interface httpapi expects (nodeLister, nodeRegistrar).
	nodeService := nodes.NewService(nodeRepo, auditRecorder)
	profileService := profiles.NewService(profileRepo, nodeRepo, engineRegistry, auditRecorder)
	lifecycleService := lifecycle.NewService(profileRepo, instanceRepo, engineRegistry, agentRegistry, auditRecorder, logger)
	// onMessage stays nil (see agentConnHandler above) - HandleTransferProgress
	// isn't wired in yet, so this Service currently only backs the read-only
	// Transfers page's ListTransfers, not a real InitiateTransfer caller.
	transferService := transfers.NewService(transferRepo, inventoryRepo, overrideRepo, agentRegistry, auditRecorder, logger)

	// rbacService backs both the Users & permissions page's RBAC-gated
	// roster read (ListUsers) and, as of Dashboard UI Phase 8, its
	// tier-change form's own write (ElevateTier) - the same value is
	// passed twice below, once per narrow interface httpapi expects
	// (userRoster, userElevator), since both were already this Service's
	// job before either had an HTTP caller.
	rbacService := rbac.NewService(users, auditRecorder)

	// settingsService backs the Settings page's read-only view of the two
	// singleton config rows - no write path exists yet in the Dashboard
	// UI (Phase 6 and beyond, PLANNING.md), so nothing has ever called
	// either repository's Set beyond what migrations 000012/000013 seed.
	settingsService := settings.NewService(db.NewMetricsExportConfigRepository(pool), db.NewAuditSettingsRepository(pool))

	// metricsService is constructed here for the first time - previously
	// only referenced in the agentConnHandler comment above as a future
	// OnMessageFunc caller. HandleTelemetry still isn't wired to that
	// callback (onMessage stays nil, same reasoning as transferService),
	// so in production this Service currently only backs the Metrics
	// page's two read methods; nothing yet actually ingests a real
	// telemetry reading end to end.
	metricsService := metrics.NewService(db.NewMetricsRepository(pool), instanceRepo, logger)

	// breakGlass is also the Setup Check's completeness signal - see
	// setup.go and internal/httpapi's setupGate.
	api, err := httpapi.New(loginService, breakGlassLoginService, breakGlass, cfg.SessionSecret, agentConnHandler,
		nodeService, nodeService, profileService, lifecycleService, transferService, users, auditRecorder, rbacService, rbacService, settingsService, metricsService, logger)
	if err != nil {
		logger.Fatalf("httpapi: %v", err)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.ListenPort,
		Handler: api.Router(),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Fatalf("server: %v", err)
		}
	case sig := <-sigCh:
		logger.Printf("received %s, shutting down", sig)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("graceful shutdown error: %v", err)
		}
	}

	logger.Println("sparky-server: stopped")
}
