package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestLakeMigrationPreservesBookingsCredentialsAndReservation(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, booking := fixtureProfileAndBooking(t, database, "lake-upgrade")
	credentials, err := database.GetProfileCredentials(ctx, testUserID, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := database.SystemEnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the pre-selection schema from populated state. Remove only
	// the newer additions so all v6 constraints and execution records remain.
	if _, err := database.db.ExecContext(ctx, `
		DROP TRIGGER booking_profile_lake_insert;
		DROP TRIGGER booking_profile_lake_update;
		DROP TRIGGER profiles_lake_immutable;
		DROP TABLE lake_settings;
		DROP TABLE account_settings;
		ALTER TABLE profiles DROP COLUMN lake_id;
		ALTER TABLE booking_requests DROP COLUMN release_days_before;
		ALTER TABLE booking_requests DROP COLUMN lake_id;
		DELETE FROM schema_migrations WHERE version >= 7;
	`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := database.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	persisted, err := database.GetBookingRequest(ctx, testUserID, booking.ID)
	if err != nil || persisted.LakeID != destinations.DefaultLakeID || !reflect.DeepEqual(persisted, booking) {
		t.Fatalf("booking changed during upgrade: before=%+v after=%+v err=%v", booking, persisted, err)
	}
	gotCredentials, err := database.GetProfileCredentials(ctx, testUserID, profile.ID)
	if err != nil || !reflect.DeepEqual(gotCredentials, credentials) {
		t.Fatalf("profile credentials changed: %v", err)
	}
	gotProfile, err := database.GetProfile(ctx, testUserID, profile.ID)
	if err != nil || !reflect.DeepEqual(gotProfile, profile) {
		t.Fatalf("profile changed during upgrade: before=%+v after=%+v err=%v", profile, gotProfile, err)
	}
	gotJob, err := database.GetJob(ctx, testUserID, job.ID)
	if err != nil || !reflect.DeepEqual(gotJob, job) {
		t.Fatalf("job history changed: before=%+v after=%+v err=%v", job, gotJob, err)
	}
	conflict, err := database.BookingConflict(ctx, testUserID, booking.ID, model.CommandBook)
	if err != nil || !conflict.Reservation || conflict.Job == nil || conflict.Job.ID != job.ID {
		t.Fatalf("reservation was lost: %+v, %v", conflict, err)
	}
	if _, err := database.SystemEnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeAuto}); !errors.Is(err, ErrConflict) {
		t.Fatalf("upgrade permitted a duplicate booking: %v", err)
	}
}

func TestBookingLakeSelectionPersistsAndRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "lake-selection")
	if booking.LakeID != destinations.DefaultLakeID {
		t.Fatalf("legacy caller did not receive a persistent lake: %+v", booking)
	}
	booking.LakeID = "unknown-lake"
	if _, err := database.UpdateBookingRequest(ctx, testUserID, booking); err == nil {
		t.Fatal("unknown lake update was accepted")
	}
	booking.ID = 0
	booking.Name = "unknown lake"
	if _, err := database.CreateBookingRequest(ctx, testUserID, booking); err == nil {
		t.Fatal("unknown lake creation was accepted")
	}
}
