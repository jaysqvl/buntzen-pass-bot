package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestBookingConflictFindsPendingCommandsBeyondRecentHistory(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	resources := database.ForUser(testUserID)
	for _, command := range []model.JobCommand{model.CommandAuthCheck, model.CommandDryRun, model.CommandBook} {
		t.Run(string(command), func(t *testing.T) {
			_, booking := fixtureProfileAndBooking(t, database, string(command))
			job, err := resources.EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: command, RunMode: model.RunModeAuto})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.db.ExecContext(ctx, `
				WITH RECURSIVE history(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM history WHERE n < 51)
				INSERT INTO jobs (user_id, profile_id, otp_source_id, command, run_mode, status, due_at, created_at, updated_at, finished_at)
				SELECT user_id, profile_id, otp_source_id, 'auth-check', 'manual', 'succeeded', created_at, created_at, updated_at, updated_at
				FROM jobs CROSS JOIN history WHERE jobs.id = ?`, job.ID); err != nil {
				t.Fatal(err)
			}
			recent, err := resources.ListJobs(ctx, 50)
			if err != nil || len(recent) != 50 || recent[len(recent)-1].ID <= job.ID {
				t.Fatalf("job was not outside recent history: len=%d err=%v", len(recent), err)
			}
			conflict, err := resources.BookingConflict(ctx, booking.ID, command)
			if err != nil || conflict.Job == nil || conflict.Job.ID != job.ID || conflict.Reservation != (command == model.CommandBook) {
				t.Fatalf("pending %s conflict=%+v err=%v", command, conflict, err)
			}
			otherCommand := model.CommandBook
			if command == model.CommandBook {
				otherCommand = model.CommandDryRun
			}
			if conflict, err := resources.BookingConflict(ctx, booking.ID, otherCommand); err != nil || conflict.Job != nil || conflict.Reservation {
				t.Fatalf("different command reported conflict=%+v err=%v", conflict, err)
			}
			if err := resources.RequestJobCancellation(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			if conflict, err := resources.BookingConflict(ctx, booking.ID, command); err != nil || conflict.Job != nil || conflict.Reservation {
				t.Fatalf("cancelled-before-confirmation attempt still blocks: %+v err=%v", conflict, err)
			}
		})
	}
}

func TestBookingConflictUsesImmutableReservationAcrossRequestsAndPrunedHistory(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	resources := database.ForUser(testUserID)
	_, booking := fixtureProfileAndBooking(t, database, "original")
	other := booking
	other.ID, other.Name = 0, "same date, another request"
	other, err := resources.CreateBookingRequest(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	job, err := resources.EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resources.EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &other.ID, Command: model.CommandBook, RunMode: model.RunModeManual}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second request was not rejected: %v", err)
	}
	for _, status := range []model.JobStatus{model.JobQueued, model.JobRunning, model.JobAwaitingApproval} {
		if status != job.Status {
			job, err = database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{job.Status}, status, JobTransition{})
			if err != nil {
				t.Fatal(err)
			}
		}
		conflict, err := resources.BookingConflict(ctx, other.ID, model.CommandBook)
		if err != nil || !conflict.Reservation || conflict.Job == nil || conflict.Job.ID != job.ID || conflict.Job.Status != status {
			t.Fatalf("cross-request %s conflict=%+v err=%v", status, conflict, err)
		}
	}
	job, err = database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval}, model.JobOutcomeUnknown, JobTransition{ConfirmationStarted: true})
	if err != nil {
		t.Fatal(err)
	}
	// Editing the original request must not move the date protected by its job.
	booking.TargetDate = "2030-01-16"
	if _, err := resources.UpdateBookingRequest(ctx, booking); err != nil {
		t.Fatal(err)
	}
	if conflict, err := resources.BookingConflict(ctx, booking.ID, model.CommandBook); err != nil || conflict.Job != nil || conflict.Reservation {
		t.Fatalf("original request's new date inherited old guard: %+v err=%v", conflict, err)
	}
	conflict, err := resources.BookingConflict(ctx, other.ID, model.CommandBook)
	if err != nil || !conflict.Reservation || conflict.Job == nil || conflict.Job.ID != job.ID || conflict.Job.Status != model.JobOutcomeUnknown || conflict.Job.ConfirmationStartedAt == nil {
		t.Fatalf("immutable original date lost confirmation context: %+v err=%v", conflict, err)
	}
	// A separate successful booking is eligible for history retention. Deleting
	// that history must leave a guard without inventing a job link.
	successful, err := resources.EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemTransitionJob(ctx, successful.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemTransitionJob(ctx, successful.ID, []model.JobStatus{model.JobRunning}, model.JobSucceeded, JobTransition{ConfirmationStarted: true}); err != nil {
		t.Fatal(err)
	}
	conflict, err = resources.BookingConflict(ctx, booking.ID, model.CommandBook)
	if err != nil || !conflict.Reservation || conflict.Job == nil || conflict.Job.Status != model.JobSucceeded {
		t.Fatalf("successful reservation lost its outcome: %+v err=%v", conflict, err)
	}
	if _, err := database.db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", successful.ID); err != nil {
		t.Fatal(err)
	}
	conflict, err = resources.BookingConflict(ctx, booking.ID, model.CommandBook)
	if err != nil || !conflict.Reservation || conflict.Job != nil {
		t.Fatalf("detached reservation should have no job link: %+v err=%v", conflict, err)
	}
}

func TestBookingConflictRespectsRequestedBookingAndJobOwnership(t *testing.T) {
	ctx := context.Background()
	database, ownerID, otherID := ownershipStore(t)
	_, _, booking := createOwnedResources(t, database, ownerID, "conflict-owner")
	_, _, foreign := createOwnedResources(t, database, otherID, "conflict-foreign")
	owner := database.ForUser(ownerID)
	job, err := owner.EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	foreignJob, err := database.ForUser(otherID).EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &foreign.ID, Command: model.CommandBook, RunMode: model.RunModeManual})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{foreign.ID, 999999} {
		if conflict, err := owner.BookingConflict(ctx, id, model.CommandBook); !errors.Is(err, ErrNotFound) || conflict.Job != nil || conflict.Reservation {
			t.Fatalf("unowned request returned conflict=%+v err=%v", conflict, err)
		}
	}
	if conflict, err := owner.BookingConflict(ctx, booking.ID, model.CommandBook); err != nil || conflict.Job == nil || conflict.Job.ID != job.ID {
		t.Fatalf("owned control conflict=%+v err=%v", conflict, err)
	}
	if _, err := database.BookingConflict(ctx, 0, booking.ID, model.CommandBook); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("unscoped lookup error=%v", err)
	}
	// A reservation's job FK does not itself enforce matching owners. Even an
	// inconsistent historical link must not expose another user's job metadata.
	if _, err := database.db.ExecContext(ctx, "UPDATE booking_reservations SET job_id = NULL WHERE job_id = ?", foreignJob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE booking_reservations SET job_id = ? WHERE job_id = ?", foreignJob.ID, job.ID); err != nil {
		t.Fatal(err)
	}
	if conflict, err := owner.BookingConflict(ctx, booking.ID, model.CommandBook); err != nil || !conflict.Reservation || conflict.Job != nil {
		t.Fatalf("foreign job link exposed: %+v err=%v", conflict, err)
	}
}
