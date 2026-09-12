package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	secretcrypto "github.com/jaysqvl/lake-pass-bot/internal/crypto"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestMissingKeyCannotBeReplacedForExistingDatabase(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("APPDATA_DIR", directory)
	t.Setenv("LAKE_PASS_MASTER_KEY_FILE", "")
	keyPath := filepath.Join(directory, "master.key")
	dbPath := filepath.Join(directory, "buntzen.db")
	box, err := secretcrypto.LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMigrated(context.Background(), dbPath, box)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.SetupAdmin(context.Background(), "owner", "long-owner-password")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateOTPSource(context.Background(), admin.ID, store.OTPSourceInput{Name: "test source", Provider: model.OTPProviderTwilio, Identity: "synthetic-inbox", ProviderConfig: map[string]string{"auth_token": "synthetic-provider-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"migrate"}); err == nil {
		t.Error("missing key silently replaced for existing data")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Errorf("replacement key created: %v", err)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("existing database modified after key failure")
	}
}

func TestExplicitMissingKeyCannotFallBackToLegacyKey(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("APPDATA_DIR", directory)
	if _, err := secretcrypto.LoadOrCreate(filepath.Join(directory, "master.key")); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "absent", "master.key")
	t.Setenv("LAKE_PASS_MASTER_KEY_FILE", explicit)
	if err := run(context.Background(), []string{"migrate"}); err == nil {
		t.Fatal("explicit missing key fell back to colocated key")
	}
	if _, err := os.Stat(filepath.Dir(explicit)); !os.IsNotExist(err) {
		t.Fatalf("external key directory created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "lake-pass-bot.db")); !os.IsNotExist(err) {
		t.Fatalf("database initialized despite explicit key failure: %v", err)
	}
}

func TestRelocatedExistingKeyPreservesData(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "encrypted records"}[populated], func(t *testing.T) {
			testRelocatedExistingKey(t, populated)
		})
	}
}

func testRelocatedExistingKey(t *testing.T, populated bool) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("APPDATA_DIR", directory)
	t.Setenv("LAKE_PASS_MASTER_KEY_FILE", "")
	if err := run(context.Background(), []string{"migrate"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "master.key")
	var ownerID, sourceID int64
	if populated {
		box, err := secretcrypto.LoadExisting(path)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.OpenMigrated(context.Background(), filepath.Join(directory, "lake-pass-bot.db"), box)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := db.SetupAdmin(context.Background(), "owner", "long-owner-password")
		if err != nil {
			t.Fatal(err)
		}
		source, err := db.CreateOTPSource(context.Background(), owner.ID, store.OTPSourceInput{Name: "relocation", Provider: model.OTPProviderTwilio, Identity: "synthetic-inbox", ProviderConfig: map[string]string{"auth_token": "synthetic-relocation-secret"}})
		if err != nil {
			t.Fatal(err)
		}
		ownerID, sourceID = owner.ID, source.ID
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	relocated := filepath.Join(t.TempDir(), "read-only.key")
	if err := os.WriteFile(relocated, original, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAKE_PASS_MASTER_KEY_FILE", relocated)
	if err := run(context.Background(), []string{"migrate"}); err != nil {
		t.Fatalf("relocated key rejected: %v", err)
	}
	after, err := os.ReadFile(relocated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("relocation rotated key")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("colocated key recreated")
	}
	if populated {
		box, err := secretcrypto.LoadExisting(relocated)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.OpenMigrated(context.Background(), filepath.Join(directory, "lake-pass-bot.db"), box)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var provider map[string]string
		if err := db.GetOTPSourceConfig(context.Background(), ownerID, sourceID, &provider); err != nil {
			t.Fatal(err)
		}
		if provider["auth_token"] != "synthetic-relocation-secret" {
			t.Fatal("relocated key did not recover original credential")
		}
	}
}
