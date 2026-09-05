package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestBookingReservationIsAtomicAcrossRequestsModesAndProcesses(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "reservation-race")
	other := booking
	other.ID = 0
	other.Name = "Same day, another request"
	other, err := database.CreateBookingRequest(ctx, testUserID, other)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := OpenMigrated(ctx, database.path, testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, request := range []model.BookingRequest{booking, other} {
		go func() {
			<-start
			selected, mode := database, model.RunModeManual
			if index == 1 {
				selected, mode = peer, model.RunModeAuto
			}
			_, err := selected.SystemEnqueueJob(ctx, EnqueueJobParams{
				BookingRequestID: &request.ID, Command: model.CommandBook, RunMode: mode,
			})
			results <- err
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		} else if errors.Is(err, ErrConflict) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestBookingReservationReleasesOnlyUnconfirmedAttempts(t *testing.T) {
	for _, test := range []struct {
		status    model.JobStatus
		confirmed bool
		retryable bool
	}{
		{model.JobCancelled, false, true},
		{model.JobFailed, false, true},
		{model.JobInterrupted, false, true},
		{model.JobSucceeded, true, false},
		{model.JobOutcomeUnknown, true, false},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			ctx := context.Background()
			database := ownedTestStore(t)
			_, booking := fixtureProfileAndBooking(t, database, "reservation-outcome")
			params := EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual}
			job, err := database.SystemEnqueueJob(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, test.status,
				JobTransition{ConfirmationStarted: test.confirmed}); err != nil {
				t.Fatal(err)
			}
			params.RunMode = model.RunModeAuto
			_, err = database.SystemEnqueueJob(ctx, params)
			if test.retryable && err != nil || !test.retryable && !errors.Is(err, ErrConflict) {
				t.Fatalf("retryable=%v enqueue error=%v", test.retryable, err)
			}
		})
	}
}

func TestBookingReservationSurvivesHistoryRemovalAndRequestDateChanges(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "reservation-history")
	originalDate := booking.TargetDate
	params := EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual}
	job, err := database.SystemEnqueueJob(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobRunning}, model.JobSucceeded,
		JobTransition{ConfirmationStarted: true}); err != nil {
		t.Fatal(err)
	}
	// This is the same DELETE used by retention; the durable reservation must
	// remain even after its successful job is no longer retained.
	if _, err := database.db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemEnqueueJob(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry after history removal error=%v", err)
	}
	date, err := time.Parse(time.DateOnly, originalDate)
	if err != nil {
		t.Fatal(err)
	}
	booking.TargetDate = date.AddDate(0, 0, 1).Format(time.DateOnly)
	booking, err = database.UpdateBookingRequest(ctx, testUserID, booking)
	if err != nil {
		t.Fatal(err)
	}
	job, err = database.SystemEnqueueJob(ctx, params)
	if err != nil {
		t.Fatalf("different date was blocked: %v", err)
	}
	if err := database.SystemRequestJobCancellation(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	booking.TargetDate = originalDate
	if _, err := database.UpdateBookingRequest(ctx, testUserID, booking); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemEnqueueJob(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("changing the request date erased its original guard: %v", err)
	}
	if _, err := database.SystemPruneTerminalJobs(ctx, date.AddDate(0, 0, 3)); err != nil {
		t.Fatal(err)
	}
	var detached int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM booking_reservations WHERE job_id IS NULL").Scan(&detached); err != nil || detached != 0 {
		t.Fatalf("expired detached reservations=%d err=%v", detached, err)
	}
}

func TestBookingReservationMigrationCancelsLegacyDuplicates(t *testing.T) {
	for _, priorOutcome := range []model.JobStatus{model.JobQueued, model.JobRunning, model.JobSucceeded, model.JobOutcomeUnknown} {
		t.Run(string(priorOutcome), func(t *testing.T) {
			ctx := context.Background()
			database, err := Open(ctx, filepath.Join(t.TempDir(), "v2.db"), testEncryptor(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if _, err := database.db.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			for index, name := range []string{"0001_initial.sql", "0002_yodel_phone_login.sql"} {
				script, err := migrationFiles.ReadFile("migrations/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.applyMigration(ctx, index+1, name, script); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.SetupAdmin(ctx, "reservation-admin", "a strong reservation migration password"); err != nil {
				t.Fatal(err)
			}
			_, booking := fixtureProfileAndBooking(t, database, "migration-reservation")
			params := EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual}
			first, err := database.SystemEnqueueJob(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if priorOutcome != model.JobQueued {
				if _, err := database.SystemTransitionJob(ctx, first.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{}); err != nil {
					t.Fatal(err)
				}
				if priorOutcome != model.JobRunning {
					if _, err := database.SystemTransitionJob(ctx, first.ID, []model.JobStatus{model.JobRunning}, priorOutcome,
						JobTransition{ConfirmationStarted: true}); err != nil {
						t.Fatal(err)
					}
				}
			}
			params.RunMode = model.RunModeAuto
			second, err := database.SystemEnqueueJob(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			duplicateStatus := model.JobCancelled
			if priorOutcome == model.JobOutcomeUnknown {
				// An old worker may already have claimed the duplicate. Revoke it
				// without releasing its browser lease before cancellation finishes.
				if _, err := database.SystemTransitionJob(ctx, second.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{}); err != nil {
					t.Fatal(err)
				}
				duplicateStatus = model.JobRunning
			}
			if err := database.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			first, err = database.SystemGetJob(ctx, first.ID)
			if err != nil || first.Status != priorOutcome {
				t.Fatalf("original job=%+v err=%v", first, err)
			}
			second, err = database.SystemGetJob(ctx, second.ID)
			if err != nil || second.Status != duplicateStatus || !second.CancelRequested {
				t.Fatalf("duplicate job=%+v err=%v", second, err)
			}
			claimed, err := database.SystemClaimNextDueJob(ctx, "migration-check")
			if priorOutcome == model.JobQueued {
				if err != nil || claimed.ID != first.ID {
					t.Fatalf("claim=%+v err=%v", claimed, err)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatalf("claim after %s=%+v err=%v", priorOutcome, claimed, err)
			}
			assertNoForeignKeyViolations(t, database)
			if _, err := database.SystemEnqueueJob(ctx, params); !errors.Is(err, ErrConflict) {
				t.Fatal(fmt.Errorf("migration did not retain the booking guard: %w", err))
			}
		})
	}
}
