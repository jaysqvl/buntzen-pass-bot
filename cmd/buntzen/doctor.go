package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/observability"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

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
