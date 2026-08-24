package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func TestAdminPasswordCommandRecoversSoleAdminAndRevokesSessions(t *testing.T) {
	ctx := context.Background()
	box, err := secretcrypto.New(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenMigrated(ctx, filepath.Join(t.TempDir(), "buntzen.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	admin, err := database.SetupAdmin(ctx, "renamed-owner", "original administrator password")
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.NewSession(ctx, admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("BUNTZEN_ADMIN_PASSWORD", "host recovered password")
	if err := adminPasswordCommand(ctx, config.Config{}, database, []string{"reset"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := database.AuthenticateUser(ctx, "renamed-owner", "host recovered password"); err != nil || !ok {
		t.Fatalf("recovered authentication ok=%v err=%v", ok, err)
	}
	if _, err := database.GetSession(ctx, session.Token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after recovery error = %v", err)
	}
}

func TestAdminPasswordCommandRequiresExplicitRecoverySecret(t *testing.T) {
	t.Setenv("BUNTZEN_ADMIN_PASSWORD", "")
	err := adminPasswordCommand(context.Background(), config.Config{}, nil, []string{"reset"})
	if err == nil || err.Error() != "BUNTZEN_ADMIN_PASSWORD must contain the new password" {
		t.Fatalf("error = %v", err)
	}
}

func TestBookCommandRejectsAnExplicitInvalidModeBeforeQueueing(t *testing.T) {
	err := runJobCommand(context.Background(), config.Config{}, nil, "book", []string{
		"--booking", "1", "--mode", "manul",
	})
	if err == nil || err.Error() != "book --mode must be manual or auto" {
		t.Fatalf("invalid mode error = %v", err)
	}
}
