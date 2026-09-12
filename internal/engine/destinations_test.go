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
				`DROP TRIGGER booking_profile_lake_update`,
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

func TestProfileOnlyExecutionRejectsUnknownProfileLakeBeforeCredentials(t *testing.T) {
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
		`DROP TRIGGER profiles_lake_immutable`,
		`UPDATE profiles SET lake_id = 'future-lake', yodel_phone_ciphertext = 'invalid-ciphertext'`,
		`UPDATE otp_sources SET config_ciphertext = 'invalid-ciphertext'`,
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.engine.execute(ctx, job); err == nil || !strings.Contains(err.Error(), "unsupported lake") {
		t.Fatalf("profile-only execution did not validate the saved profile lake first: %v", err)
	}
}

func TestProfileAndBookingMustSelectTheSameLake(t *testing.T) {
	for _, test := range []struct {
		name, profileLake, bookingLake string
		wantError                      bool
	}{
		{name: "legacy defaults"},
		{name: "legacy profile", bookingLake: "buntzen"},
		{name: "legacy booking", profileLake: "buntzen"},
		{name: "same lake", profileLake: "future-lake", bookingLake: "future-lake"},
		{name: "different lakes", profileLake: "buntzen", bookingLake: "future-lake", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateProfileBookingLake(model.Profile{LakeID: test.profileLake}, model.BookingRequest{LakeID: test.bookingLake})
			if (err != nil) != test.wantError {
				t.Fatalf("profile=%q booking=%q mismatch error=%v", test.profileLake, test.bookingLake, err)
			}
		})
	}
}
