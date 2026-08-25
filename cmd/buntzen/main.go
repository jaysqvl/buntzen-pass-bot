// Command buntzen is the single entry point for the control plane and jobs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	accountauth "github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/lockfile"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/observability"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
	"github.com/jaysqvl/buntzen-pass-bot/internal/web"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := observability.Configure(cfg.EffectiveLogLevel()); err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	slog.Debug("runtime configuration loaded",
		"command", args[0],
		"log_level", cfg.EffectiveLogLevel(),
		"appdata_dir", cfg.AppDataDir,
	)
	if err := cfg.EnsureDirectories(); err != nil {
		return err
	}
	box, err := secretcrypto.LoadOrCreate(cfg.EncryptionKeyPath)
	if err != nil {
		return fmt.Errorf("load encryption key: %w", err)
	}
	database, err := store.OpenMigrated(ctx, cfg.DatabasePath, box)
	if err != nil {
		return err
	}
	defer database.Close()

	switch args[0] {
	case "migrate":
		version, err := database.SchemaVersion(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("database migrated to schema %04d\n", version)
		return nil
	case "doctor":
		return runDoctor(ctx, cfg, database)
	case "serve":
		return runServe(ctx, cfg, database)
	case "auth-check", "dry-run", "book":
		return runJobCommand(ctx, cfg, database, args[0], args[1:])
	case "admin-password":
		return adminPasswordCommand(ctx, cfg, database, args[1:])
	default:
		return usageError()
	}
}

func runDoctor(ctx context.Context, cfg config.Config, database *store.Store) error {
	slog.Debug("doctor checks started")
	if err := database.Ping(ctx); err != nil {
		return err
	}
	version, err := database.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	pythonOK, pythonError := doctorPython(ctx, cfg)
	providerReports := make([]map[string]any, 0)
	sources, err := database.SystemListOTPSources(ctx)
	if err != nil {
		return err
	}
	for _, source := range sources {
		entry := map[string]any{"id": source.ID, "name": source.Name, "provider": source.Provider, "ok": false}
		provider, providerErr := engine.ProviderForSource(ctx, database, source)
		if providerErr == nil {
			healthCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			providerErr = provider.Health(healthCtx)
			cancel()
		}
		if providerErr == nil {
			entry["ok"] = true
			slog.Debug("OTP provider health check succeeded", "source_id", source.ID, "provider", source.Provider)
		} else {
			entry["error"] = providerErr.Error()
			slog.Warn("OTP provider health check failed", "source_id", source.ID, "provider", source.Provider, "error", providerErr)
		}
		providerReports = append(providerReports, entry)
	}
	report := map[string]any{
		"ok":                  true,
		"schema_version":      version,
		"action_protocol":     actionproc.ProtocolVersion,
		"appdata_dir":         cfg.AppDataDir,
		"database_path":       cfg.DatabasePath,
		"profiles_dir":        cfg.ProfilesDir,
		"artifacts_dir":       cfg.ArtifactsDir,
		"python_executable":   cfg.PythonExecutable,
		"python_module":       cfg.PythonModule,
		"python_ready":        pythonOK,
		"log_level":           cfg.EffectiveLogLevel(),
		"schedules_enabled":   cfg.SchedulesEnabled,
		"max_concurrent_jobs": cfg.MaxConcurrentJobs,
		"otp_sources":         providerReports,
	}
	if pythonError != "" {
		report["ok"] = false
		report["python_error"] = pythonError
	}
	for _, entry := range providerReports {
		if ok, _ := entry["ok"].(bool); !ok {
			report["ok"] = false
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return err
	}
	if ok, _ := report["ok"].(bool); !ok {
		return errors.New("doctor checks failed")
	}
	return nil
}

func doctorPython(ctx context.Context, cfg config.Config) (bool, string) {
	checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	session, err := actionproc.Start(checkCtx, actionproc.Config{
		Executable: cfg.PythonExecutable, Args: []string{"-m", cfg.PythonModule},
		Environment: []string{"PYTHONUNBUFFERED=1", "BUNTZEN_ACTION_LOG_LEVEL=" + cfg.EffectiveLogLevel()}, CancelGrace: 2 * time.Second,
		OnStderr: func(line string) {
			observability.LogActionDiagnostic(checkCtx, "doctor", line)
		},
	})
	if err != nil {
		return false, "Python action worker could not start"
	}
	select {
	case frame, open := <-session.Events():
		protocol, protocolOK := frame.Payload["protocol"].(float64)
		if !open || frame.Type != "worker.ready" || frame.Payload["action"] != "yodel" ||
			!protocolOK || protocol != float64(actionproc.ProtocolVersion) {
			session.Cancel(time.Second)
			return false, "Python action worker did not negotiate protocol v2"
		}
		session.Cancel(time.Second)
		select {
		case <-session.Done():
		case <-time.After(2 * time.Second):
		}
		return true, ""
	case <-checkCtx.Done():
		session.Cancel(time.Second)
		return false, "Python action worker readiness timed out"
	}
}

// runServe provides the process lifecycle and health endpoint. The composed
// UI/job handler replaces the fallback root response during application wiring.
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

func runJobCommand(parent context.Context, cfg config.Config, database *store.Store, commandName string, args []string) error {
	flags := flag.NewFlagSet(commandName, flag.ContinueOnError)
	bookingID := flags.Int64("booking", 0, "booking request ID")
	mode := flags.String("mode", "", "manual or auto override for book")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *bookingID <= 0 {
		return errors.New(commandName + " requires --booking ID")
	}
	command := model.JobCommand(commandName)
	runMode := model.RunMode("")
	requestedMode := strings.TrimSpace(*mode)
	if command == model.CommandDryRun {
		if requestedMode != "" {
			return errors.New("--mode is only valid for book")
		}
		runMode = model.RunModeDryRun
	} else if command == model.CommandBook && requestedMode != "" {
		runMode = model.RunMode(requestedMode)
		if runMode != model.RunModeManual && runMode != model.RunModeAuto {
			return errors.New("book --mode must be manual or auto")
		}
	} else if command != model.CommandBook && requestedMode != "" {
		return errors.New("--mode is only valid for book")
	}
	instanceLock, lockErr := lockfile.TryAcquire(cfg.AppDataDir + "/control-plane.lock")
	ownsControlPlane := lockErr == nil
	if lockErr != nil && !errors.Is(lockErr, lockfile.ErrLocked) {
		return lockErr
	}
	if !ownsControlPlane && !servingControlPlaneHealthy(cfg) {
		return errors.New("another one-shot command owns this appdata directory; wait for it to finish before queueing another job")
	}
	if ownsControlPlane {
		defer instanceLock.Close()
		if command == model.CommandBook && runMode != model.RunModeAuto {
			booking, err := database.SystemGetBookingRequest(parent, *bookingID)
			if err != nil {
				return err
			}
			if runMode == "" {
				runMode = booking.ConfirmationMode
			}
			if runMode == model.RunModeManual {
				return errors.New("manual book requires the running web control plane for approval; start serve or use --mode auto")
			}
		}
		if _, err := database.SystemRecoverInterruptedJobs(parent); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	jobEngine := engine.New(cfg, database, control.NewHub())
	if ownsControlPlane {
		jobEngine.Start(ctx)
		defer jobEngine.Stop()
	}
	job, err := jobEngine.SystemQueueBooking(ctx, *bookingID, command, runMode)
	if err != nil {
		return err
	}
	fmt.Printf("queued job %d\n", job.ID)
	slog.Info("one-shot command queued job", "job_id", job.ID, "command", command, "mode", runMode)
	finished, err := jobEngine.SystemWait(ctx, job.ID)
	if err != nil {
		if ctx.Err() != nil {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = database.SystemRequestJobCancellation(cancelCtx, job.ID)
			cancel()
			return errors.New("interrupted; durable job cancellation was requested")
		}
		return err
	}
	fmt.Printf("job %d finished: %s\n", finished.ID, finished.Status)
	slog.Info("one-shot command finished", "job_id", finished.ID, "status", finished.Status)
	if finished.Status != model.JobSucceeded {
		return fmt.Errorf("job %d did not succeed: %s", finished.ID, finished.Message)
	}
	return nil
}

func servingControlPlaneHealthy(cfg config.Config) bool {
	address := cfg.ListenAddress
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	target := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(target)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func adminPasswordCommand(ctx context.Context, cfg config.Config, database *store.Store, args []string) error {
	if len(args) != 1 || args[0] != "reset" {
		return errors.New("usage: buntzen admin-password reset")
	}
	password := os.Getenv("BUNTZEN_ADMIN_PASSWORD")
	if password == "" {
		return errors.New("BUNTZEN_ADMIN_PASSWORD must contain the new password")
	}
	admin, err := database.ResetAdministratorPassword(ctx, password)
	if err != nil {
		return err
	}
	fmt.Printf("administrator %q password reset; existing sessions were revoked\n", admin.Username)
	return nil
}

func usageError() error {
	return errors.New("usage: buntzen {serve|doctor|migrate|auth-check|dry-run|book|admin-password reset}; runtime commands require --booking 1")
}
