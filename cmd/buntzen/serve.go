package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	accountauth "github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/lockfile"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
	"github.com/jaysqvl/buntzen-pass-bot/internal/web"
)

func runServe(parent context.Context, cfg config.Config, database *store.Store) error {
	slog.Info("control plane starting",
		"listen", cfg.ListenAddress,
		"schedules_enabled", cfg.SchedulesEnabled,
		"max_concurrent_jobs", cfg.MaxConcurrentJobs,
		"log_level", cfg.EffectiveLogLevel(),
	)
	instanceLock, err := lockfile.TryAcquire(cfg.AppDataDir + "/control-plane.lock")
	if err != nil {
		if errors.Is(err, lockfile.ErrLocked) {
			return errors.New("another Buntzen control plane is already using this appdata directory")
		}
		return err
	}
	defer instanceLock.Close()
	// The recovery secret is never part of normal service operation and must
	// not be inherited by action-worker subprocesses.
	_ = os.Unsetenv("BUNTZEN_ADMIN_PASSWORD")
	hasUsers, err := database.HasUsers(parent)
	if err != nil {
		return err
	}
	if !hasUsers {
		if cfg.SetupToken == "" {
			cfg.SetupToken, err = accountauth.NewToken()
			if err != nil {
				return fmt.Errorf("generate first-run setup token: %w", err)
			}
			// First-run setup is an operator-action-required state. Keep this
			// record visible even when the configured threshold is error; without
			// the generated value there is no way to complete bootstrap.
			slog.Error("first-run setup required; one-time setup token: " + cfg.SetupToken)
		} else {
			slog.Info("first-run setup token loaded from BUNTZEN_SETUP_TOKEN")
		}
	}
	recovered, err := database.SystemRecoverInterruptedJobs(parent)
	if err != nil {
		return err
	}
	if recovered > 0 {
		slog.Warn("recovered interrupted jobs", "count", recovered)
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	hub := control.NewHub()
	jobEngine := engine.New(cfg, database, hub)
	ui, err := web.NewServer(cfg, database, jobEngine)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}
	defer listener.Close()
	jobEngine.Start(ctx)
	defer jobEngine.Stop()
	server := &http.Server{
		Addr: cfg.ListenAddress, Handler: ui.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 90 * time.Second,
	}
	go func() {
		<-ctx.Done()
		slog.Info("control plane shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("control plane listening", "address", listener.Addr().String())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		slog.Info("control plane stopped")
		return nil
	}
	return err
}
