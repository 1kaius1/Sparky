// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-server is the central Sparky application. See
// ARCHITECTURE.md Application Lifecycle for the full startup sequence this
// will grow into. The Setup Check step (refusing to serve normal routes
// until `sparky setup` has run) is not implemented yet.
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

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/config"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/httpapi"
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

	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer pool.Close()
	logger.Println("database connection pool established")

	identityProvider := auth.NewLDAPProvider(cfg.LDAPServerAddr, cfg.LDAPBindDN, cfg.LDAPBindPassword, cfg.LDAPBaseDN, cfg.LDAPAccessGroupDN)
	users := db.NewUserRepository(pool)
	loginService := httpapi.NewLoginService(identityProvider, users, cfg.SessionSecret)
	api := httpapi.New(loginService, cfg.SessionSecret)

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
