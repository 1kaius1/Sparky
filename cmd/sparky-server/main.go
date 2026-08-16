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
	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/audit"
	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/engineprovision"
	"github.com/1kaius1/Sparky/internal/engines"
	"github.com/1kaius1/Sparky/internal/events"
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

	auditRecorder := audit.NewRecorder(db.NewAuditRepository(pool), os.Stdout)
	engineRegistry := engines.NewRegistry()
	profileRepo := db.NewProfileRepository(pool)
	instanceRepo := db.NewRunningInstanceRepository(pool)

	transferRepo := db.NewModelTransferRepository(pool)
	inventoryRepo := db.NewNodeModelInventoryRepository(pool)
	overrideRepo := db.NewPermissionOverrideRepository(pool)

	engineTransferRepo := db.NewEngineTransferRepository(pool)
	engineInventoryRepo := db.NewNodeEngineInventoryRepository(pool)

	// nodeService backs both the Nodes page's unguarded ListNodes read
	// and, as of Dashboard UI Phase 9, the registration form's own write
	// (RegisterNode) - the same value is passed twice below, once per
	// narrow interface httpapi expects (nodeLister, nodeRegistrar).
	nodeService := nodes.NewService(nodeRepo, auditRecorder)
	// profileService backs both the Model profiles page's unguarded
	// ListProfiles read and, as of Dashboard UI Phase 10, the create/edit
	// form's own writes (CreateProfile/UpdateProfile/GetProfile) - same
	// twice-passed-value reasoning as nodeService above.
	profileService := profiles.NewService(profileRepo, nodeRepo, engineRegistry, auditRecorder)
	// lifecycleService backs both the Model profiles page's unguarded
	// ListInstances read and, as of Dashboard UI Phase 11, the Load/Unload
	// controls' own writes (LoadInstance/UnloadInstance) - same
	// twice-passed-value reasoning as nodeService/profileService above.
	lifecycleService := lifecycle.NewService(profileRepo, instanceRepo, engineRegistry, agentRegistry, auditRecorder, logger)
	transferService := transfers.NewService(transferRepo, inventoryRepo, overrideRepo, agentRegistry, auditRecorder, logger)
	// engineProvisionService has no HTTP caller yet - same "logic layer
	// ahead of HTTP wiring" precedent as RBAC Phase B, the audit log, and
	// the node registry originally shipped with (PLANNING.md), so it is
	// not passed to httpapi.New below (unlike transferService, which
	// already backs the Transfers page's read view). It is still
	// constructed and wired into the onMessage dispatch below - the
	// engine_transfer_progress handling direction is real infrastructure
	// independent of whether anything can trigger a provisioning run via
	// HTTP yet. See PLANNING.md's 2026-08-15 Decisions Log entry.
	engineProvisionService := engineprovision.NewService(engineTransferRepo, engineInventoryRepo, agentRegistry, auditRecorder, logger)

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

	// metricsService backs the Metrics page's two read methods and, as of
	// Dashboard UI Phase 11 (via the onMessage dispatch below),
	// HandleTelemetry - the first time this Service actually ingests a
	// real telemetry reading end to end, not just serves the page.
	metricsService := metrics.NewService(db.NewMetricsRepository(pool), instanceRepo, logger)

	// eventsBroker fans out a live signal to every open GET /events (SSE)
	// connection - see internal/events' own doc comment. Fed by the
	// onMessage dispatch closure below, consumed by internal/httpapi's
	// handleEvents.
	eventsBroker := events.NewBroker()

	// onMessage is the OnMessage-consolidation slice PLANNING.md's
	// Dashboard UI Phase 11 names as the hard prerequisite for both the
	// Load/Unload controls (an unhandled TypeInstanceResult would leave a
	// running_instances row stuck in starting/stopping forever) and SSE
	// (nothing to broadcast otherwise). internal/transfers,
	// internal/lifecycle, and internal/metrics each already have their own
	// OnMessageFunc-shaped handler; agentconn.Handler only accepts one
	// callback, so this switches on the envelope's own type rather than
	// calling all three unconditionally on every message.
	onMessage := func(nodeID string, env agentproto.Envelope) {
		switch env.Type {
		case agentproto.TypeTransferProgress:
			transferService.HandleTransferProgress(nodeID, env)
			eventsBroker.Publish(events.Event{Type: string(env.Type)})
		case agentproto.TypeEngineTransferProgress:
			engineProvisionService.HandleEngineTransferProgress(nodeID, env)
			eventsBroker.Publish(events.Event{Type: string(env.Type)})
		case agentproto.TypeInstanceResult:
			lifecycleService.HandleInstanceResult(nodeID, env)
			eventsBroker.Publish(events.Event{Type: string(env.Type)})
		case agentproto.TypeTelemetry:
			metricsService.HandleTelemetry(nodeID, env)
			eventsBroker.Publish(events.Event{Type: string(env.Type)})
		default:
			logger.Printf("agentconn: node %s sent an unhandled message type %q", nodeID, env.Type)
		}
	}
	agentConnHandler := agentconn.NewHandler(nodeAuth, nodeRepo, agentRegistry, logger, onMessage, lifecycleService.ReconcileNode)

	// breakGlass is also the Setup Check's completeness signal - see
	// setup.go and internal/httpapi's setupGate.
	api, err := httpapi.New(loginService, breakGlassLoginService, breakGlass, cfg.BreakGlassAllowedIPs, cfg.SessionSecret, agentConnHandler,
		nodeService, nodeService, profileService, profileService, lifecycleService, lifecycleService, transferService, users, auditRecorder, rbacService, rbacService, settingsService, metricsService, eventsBroker, logger)
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
