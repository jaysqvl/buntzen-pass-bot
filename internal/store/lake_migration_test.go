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
	// Reconstruct the exact pre-selection schema from populated state. The
	// migration adds only this column, so all v6 constraints/triggers remain.
	if _, err := database.db.ExecContext(ctx, `ALTER TABLE booking_requests DROP COLUMN lake_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 7`); err != nil {
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
