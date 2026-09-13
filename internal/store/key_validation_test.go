package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	secretcrypto "github.com/jaysqvl/lake-pass-bot/internal/crypto"
)

func TestWrongKeyFailsBeforeWritableOpen(t *testing.T) {
	database := ownedTestStore(t)
	fixtureProfileAndBooking(t, database, "key validation")
	path := database.path
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := secretcrypto.New(bytes.Repeat([]byte{0x99}, 32))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenMigrated(context.Background(), path, wrong)
	if err == nil {
		reopened.Close()
		t.Error("wrong key admitted existing encrypted data")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("wrong key changed database bytes")
	}
}

func TestKeyPreflightReadsCommittedWAL(t *testing.T) {
	database := ownedTestStore(t)
	if _, err := database.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	fixtureProfileAndBooking(t, database, "WAL credential")
	before, err := os.ReadFile(database.path + "-wal")
	if err != nil || len(before) == 0 {
		t.Fatalf("missing committed WAL: %v", err)
	}
	wrong, err := secretcrypto.New(bytes.Repeat([]byte{0x99}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyExistingDatabaseKey(context.Background(), database.path, wrong); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("WAL-only credential missed: %v", err)
	}
	after, err := os.ReadFile(database.path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only key preflight wrote WAL")
	}
	if err := verifyExistingDatabaseKey(context.Background(), database.path, testEncryptor(t)); err != nil {
		t.Fatalf("matching key rejected committed WAL: %v", err)
	}
}

func TestKeyPreflightChecksLaterRowsAndDisabledProfile(t *testing.T) {
	for _, column := range []string{"config_ciphertext", "yodel_phone_ciphertext"} {
		t.Run(column, func(t *testing.T) {
			database := ownedTestStore(t)
			fixtureProfileAndBooking(t, database, "first")
			later, _ := fixtureProfileAndBooking(t, database, "later")
			if _, err := database.db.Exec("UPDATE profiles SET enabled=0 WHERE id=?", later.ID); err != nil {
				t.Fatal(err)
			}
			table, id := "profiles", later.ID
			if column == "config_ciphertext" {
				table = "otp_sources"
				id = later.OTPSourceID
			}
			if _, err := database.db.Exec("UPDATE "+table+" SET "+column+"=? WHERE id=?", "invalid-encrypted-canary", id); err != nil {
				t.Fatal(err)
			}
			err := verifyExistingDatabaseKey(context.Background(), database.path, testEncryptor(t))
			if !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("later encrypted row missed: %v", err)
			}
			if strings.Contains(err.Error(), "invalid-encrypted-canary") {
				t.Fatal("ciphertext leaked in failure")
			}
		})
	}
}

func TestLegacyCredentialFailurePrecedesMigrationWrites(t *testing.T) {
	for _, column := range []string{"yodel_email_ciphertext", "yodel_password_ciphertext"} {
		t.Run(column, func(t *testing.T) {
			database := emptyTestStore(t)
			// Add the legacy column to a current fixture to exercise the same direct
			// migration entrypoint without allowing a migration to discard its value.
			if _, err := database.db.Exec("ALTER TABLE profiles ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil {
				t.Fatal(err)
			}
			admin, err := database.SetupAdmin(context.Background(), "test-admin", "a strong test password")
			if err != nil {
				t.Fatal(err)
			}
			if admin.ID != testUserID {
				t.Fatal("unexpected owner")
			}
			profile, _ := fixtureProfileAndBooking(t, database, "legacy")
			if _, err := database.db.Exec("UPDATE profiles SET "+column+"='invalid-legacy-canary' WHERE id=?", profile.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.Migrate(context.Background()); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("legacy encrypted column skipped: %v", err)
			}
			var retained string
			if err := database.db.QueryRow("SELECT "+column+" FROM profiles WHERE id=?", profile.ID).Scan(&retained); err != nil || retained != "invalid-legacy-canary" {
				t.Fatalf("legacy value mutated: %v", err)
			}
		})
	}
}
