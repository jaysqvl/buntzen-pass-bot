package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/observability"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
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
