package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestExecutionRejectsPersistedUnknownLakeBeforeDecryptingCredentials(t *testing.T) {
	for _, command := range []model.JobCommand{model.CommandAuthCheck, model.CommandDryRun} {
		t.Run(string(command), func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			ctx := context.Background()
			job, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
				BookingRequestID: &fixture.booking.ID, Command: command, RunMode: model.RunModeManual,
			})
			if err != nil {
				t.Fatal(err)
			}
			database, err := sql.Open("sqlite", fixture.databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			// Bypass the write guard only in this isolated test to simulate
			// corrupted persisted state after queueing. Make decryption fail
			// if execution reaches either credential source.
			for _, statement := range []string{
				`DROP TRIGGER IF EXISTS booking_profile_lake_update`,
				`UPDATE booking_requests SET lake_id = 'unknown-lake'`,
				`UPDATE profiles SET yodel_phone_ciphertext = 'invalid-ciphertext'`,
				`UPDATE otp_sources SET config_ciphertext = 'invalid-ciphertext'`,
			} {
				if _, err := database.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.engine.execute(ctx, job); err == nil || !strings.Contains(err.Error(), "unsupported lake") {
				t.Fatalf("unsupported persisted destination was not rejected first: %v", err)
			}
		})
	}
}

func TestProfileOnlyExecutionRejectsUnknownProviderBeforeCredentials(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	job, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
		ProfileID: fixture.booking.ProfileID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Bypass immutability in this isolated database to verify the execution
	// boundary independently of the normal profile write guard.
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS profiles_provider_immutable`,
		`UPDATE profiles SET provider_id = 'future-provider', yodel_phone_ciphertext = 'invalid-ciphertext'`,
		`UPDATE otp_sources SET config_ciphertext = 'invalid-ciphertext'`,
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.engine.execute(ctx, job); err == nil || !strings.Contains(err.Error(), "unsupported sign-in provider") {
		t.Fatalf("profile-only execution did not validate the saved provider first: %v", err)
	}
}

func TestProfileAndBookingMustUseCompatibleProviders(t *testing.T) {
	for _, test := range []struct {
		name, profileLake, profileProvider, bookingLake string
		wantError                                       bool
	}{
		{name: "legacy defaults"},
		{name: "legacy profile", bookingLake: "buntzen"},
		{name: "legacy booking", profileLake: "buntzen"},
		{name: "global Yodel sign-in", profileLake: "future-lake", profileProvider: "yodel", bookingLake: "buntzen"},
		{name: "unknown lake", profileLake: "buntzen", bookingLake: "future-lake", wantError: true},
		{name: "different provider", profileProvider: "another-provider", bookingLake: "buntzen", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateProfileBookingProvider(model.Profile{LakeID: test.profileLake, ProviderID: test.profileProvider}, model.BookingRequest{LakeID: test.bookingLake})
			if (err != nil) != test.wantError {
				t.Fatalf("profile=%q booking=%q mismatch error=%v", test.profileLake, test.bookingLake, err)
			}
		})
	}
}
