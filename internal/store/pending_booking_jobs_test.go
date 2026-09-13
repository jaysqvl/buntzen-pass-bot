package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestPendingBookingJobsStayVisibleBeyondHistoryAndRespectOwnership(t *testing.T) {
	ctx := context.Background()
	database, ownerID, otherID := ownershipStore(t)
	_, profile, booking := createOwnedResources(t, database, ownerID, "pending-owner")
	_, _, foreignBooking := createOwnedResources(t, database, otherID, "pending-foreign")
	job, err := database.ForUser(ownerID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeAuto,
		DueAt: time.Now().Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForUser(otherID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &foreignBooking.ID, Command: model.CommandBook, RunMode: model.RunModeManual,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForUser(ownerID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
	}); err != nil {
		t.Fatal(err)
	}
	// Historical test runs can outnumber the recent-history limit while a
	// future booking is still waiting. Seed completed history for that case.
	if _, err := database.db.ExecContext(ctx, `
		WITH RECURSIVE history(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM history WHERE n < 51)
		INSERT INTO jobs (user_id, profile_id, otp_source_id, command, run_mode, status, due_at, created_at, updated_at, finished_at)
		SELECT user_id, profile_id, otp_source_id, 'auth-check', 'manual', 'succeeded', created_at, created_at, updated_at, updated_at
		FROM jobs CROSS JOIN history WHERE jobs.id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	recent, err := database.ForUser(ownerID).ListJobs(ctx, 50)
	if err != nil || len(recent) != 50 || recent[len(recent)-1].ID <= job.ID {
		t.Fatalf("history fixture did not push booking out of recent jobs: count=%d err=%v", len(recent), err)
	}
	for _, status := range []model.JobStatus{model.JobQueued, model.JobRunning, model.JobAwaitingApproval} {
		if job.Status != status {
			job, err = database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{job.Status}, status, JobTransition{})
			if err != nil {
				t.Fatal(err)
			}
		}
		pending, err := database.ForUser(ownerID).ListPendingBookingJobs(ctx)
		if err != nil || len(pending) != 1 || pending[0].ID != job.ID || pending[0].Status != status {
			t.Fatalf("pending %s jobs=%+v err=%v", status, pending, err)
		}
	}
	if _, err := database.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobAwaitingApproval}, model.JobCancelled, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := database.ForUser(ownerID).ListPendingBookingJobs(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("completed booking still shown: %+v err=%v", pending, err)
	}
	if _, err := database.ListPendingBookingJobs(ctx, 0); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("unscoped pending jobs: %v", err)
	}
}
