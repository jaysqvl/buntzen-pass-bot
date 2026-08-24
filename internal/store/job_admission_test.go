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

func TestJobAdmissionAtomicallyBoundsEveryEnqueuePath(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "shared.db")
	box := testEncryptor(t)
	database, err := OpenMigrated(ctx, databasePath, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	peer, err := OpenMigrated(ctx, databasePath, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	user, err := database.SetupAdmin(ctx, "admission-admin", "a strong admission test password")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != testUserID {
		t.Fatalf("test user ID=%d want=%d", user.ID, testUserID)
	}
	profile, _ := fixtureProfileAndBooking(t, database, "admission-cap")

	const attempts = MaxPendingJobsPerUser * 3
	start := make(chan struct{})
	results := make(chan error, attempts)
	for attempt := range attempts {
		go func() {
			<-start
			selected := database
			if attempt%2 == 1 {
				selected = peer
			}
			params := EnqueueJobParams{
				ProfileID: profile.ID, Command: model.CommandAuthCheck,
				RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
			}
			var err error
			switch attempt % 3 {
			case 0:
				_, err = selected.ForUser(testUserID).EnqueueJob(ctx, params)
			case 1:
				_, err = selected.EnqueueJob(ctx, testUserID, params)
			default:
				_, err = selected.SystemEnqueueJob(ctx, params)
			}
			results <- err
		}()
	}
	close(start)

	accepted, conflicts := 0, 0
	for range attempts {
		switch err := <-results; {
		case err == nil:
			accepted++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected enqueue error: %v", err)
		}
	}
	if accepted != MaxPendingJobsPerUser || conflicts != attempts-MaxPendingJobsPerUser {
		t.Fatalf("accepted=%d conflicts=%d, want %d/%d", accepted, conflicts,
			MaxPendingJobsPerUser, attempts-MaxPendingJobsPerUser)
	}

	var jobs, pending, events int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE user_id = ?", testUserID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs
		WHERE user_id = ? AND status IN ('queued', 'running', 'awaiting_approval')
	`, testUserID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM job_events WHERE user_id = ?", testUserID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if jobs != MaxPendingJobsPerUser || pending != MaxPendingJobsPerUser || events != 0 {
		t.Fatalf("jobs=%d pending=%d events=%d after rejected submissions", jobs, pending, events)
	}
	queued, err := database.ForUser(testUserID).ListJobs(ctx, MaxPendingJobsPerUser)
	if err != nil || len(queued) != MaxPendingJobsPerUser {
		t.Fatalf("queued jobs=%d err=%v", len(queued), err)
	}
	if err := database.ForUser(testUserID).RequestJobCancellation(ctx, queued[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.SystemEnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("enqueue after terminal job freed quota: %v", err)
	}
	if err := database.db.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs
		WHERE user_id = ? AND status IN ('queued', 'running', 'awaiting_approval')
	`, testUserID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != MaxPendingJobsPerUser {
		t.Fatalf("pending=%d after terminal requeue, want %d", pending, MaxPendingJobsPerUser)
	}
}

func TestEquivalentPendingBookingRunIsRejectedUntilTerminal(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "equivalent-run")
	bookingID := booking.ID
	params := EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandBook,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
	}

	first, err := database.ForUser(testUserID).EnqueueJob(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnqueueJob(ctx, testUserID, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("direct duplicate error=%v", err)
	}
	if _, err := database.SystemEnqueueJob(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("system duplicate error=%v", err)
	}

	if err := database.ForUser(testUserID).RequestJobCancellation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := database.SystemEnqueueJob(ctx, params)
	if err != nil {
		t.Fatalf("enqueue after terminal cancellation: %v", err)
	}
	second, err = database.SystemTransitionJob(ctx, second.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForUser(testUserID).EnqueueJob(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate of running job error=%v", err)
	}
	second, err = database.SystemTransitionJob(ctx, second.ID,
		[]model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemEnqueueJob(ctx, params); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate of awaiting-approval job error=%v", err)
	}
	second, err = database.SystemTransitionJob(ctx, second.ID,
		[]model.JobStatus{model.JobAwaitingApproval}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemTransitionJob(ctx, second.ID,
		[]model.JobStatus{model.JobRunning}, model.JobSucceeded, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnqueueJob(ctx, testUserID, params); err != nil {
		t.Fatalf("enqueue after successful terminal state: %v", err)
	}
}

func TestBoundedBacklogCannotPermanentlyStarveAnotherUser(t *testing.T) {
	ctx := context.Background()
	database, firstUserID, secondUserID := ownershipStore(t)
	_, firstProfile, _ := createOwnedResources(t, database, firstUserID, "queue-first")
	_, secondProfile, _ := createOwnedResources(t, database, secondUserID, "queue-second")
	now := time.Now().UTC()

	firstParams := EnqueueJobParams{
		ProfileID: firstProfile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: now,
	}
	for range MaxPendingJobsPerUser {
		if _, err := database.ForUser(firstUserID).EnqueueJob(ctx, firstParams); err != nil {
			t.Fatal(err)
		}
	}
	secondJob, err := database.ForUser(secondUserID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: secondProfile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: now.Add(time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}

	claimedSecond := false
	for claimNumber := 0; claimNumber < MaxPendingJobsPerUser+1; claimNumber++ {
		job, err := database.SystemClaimNextDueJobAt(ctx, fmt.Sprintf("fairness-worker-%d", claimNumber), now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if job.ID == secondJob.ID {
			claimedSecond = true
			break
		}
		if _, err := database.SystemTransitionJob(ctx, job.ID,
			[]model.JobStatus{model.JobRunning}, model.JobSucceeded, JobTransition{}); err != nil {
			t.Fatal(err)
		}
		// Simulate the first user continuously trying to replenish its queue.
		firstParams.DueAt = now.Add(2 * time.Millisecond)
		if _, err := database.ForUser(firstUserID).EnqueueJob(ctx, firstParams); err != nil {
			t.Fatal(err)
		}
	}
	if !claimedSecond {
		t.Fatalf("second user's job was not claimed within the bounded backlog")
	}
}

func TestTerminalJobChurnIsPrunedButCriticalRecordsSurvive(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "job-history.db")
	box := testEncryptor(t)
	database, err := OpenMigrated(ctx, databasePath, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	peer, err := OpenMigrated(ctx, databasePath, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	user, err := database.SetupAdmin(ctx, "history-admin", "a strong history test password")
	if err != nil {
		t.Fatal(err)
	}
	firstProfile, booking := fixtureProfileAndBooking(t, database, "history-first")
	secondProfile, _ := fixtureProfileAndBooking(t, database, "history-second")
	now := time.Now().UTC()

	outcome, err := database.ForUser(user.ID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: firstProfile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = database.SystemTransitionJob(ctx, outcome.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = database.SystemTransitionJob(ctx, outcome.ID,
		[]model.JobStatus{model.JobRunning}, model.JobOutcomeUnknown,
		JobTransition{ConfirmationStarted: true})
	if err != nil {
		t.Fatal(err)
	}

	expires := now.Add(time.Hour)
	scheduled, err := database.ForUser(user.ID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual,
		DueAt: now, ExpiresAt: &expires, DedupKey: "booking:history-first:2031-01-15",
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err = database.SystemTransitionJob(ctx, scheduled.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err = database.SystemTransitionJob(ctx, scheduled.ID,
		[]model.JobStatus{model.JobRunning}, model.JobSucceeded, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}

	active, err := database.ForUser(user.ID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: secondProfile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	var oldestPrunable int64
	for index := 0; index < MaxTerminalJobHistoryPerUser+32; index++ {
		selected := database
		if index%2 == 1 {
			selected = peer
		}
		job, err := selected.ForUser(user.ID).EnqueueJob(ctx, EnqueueJobParams{
			ProfileID: firstProfile.ID, Command: model.CommandAuthCheck,
			RunMode: model.RunModeManual, DueAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldestPrunable = job.ID
		}
		if err := selected.ForUser(user.ID).RequestJobCancellation(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}

	var total, prunable int
	if err := database.db.QueryRowContext(ctx, `
		SELECT count(*), sum(CASE WHEN status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
			AND (dedup_key = '' OR dedup_key GLOB 'pairing:*' OR (expires_at IS NOT NULL AND expires_at <= updated_at))
			THEN 1 ELSE 0 END)
		FROM jobs WHERE user_id = ?
	`, user.ID).Scan(&total, &prunable); err != nil {
		t.Fatal(err)
	}
	if prunable != MaxTerminalJobHistoryPerUser || total != MaxTerminalJobHistoryPerUser+3 {
		t.Fatalf("total=%d prunable=%d", total, prunable)
	}
	if _, err := database.SystemGetJob(ctx, oldestPrunable); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest prunable job error=%v", err)
	}
	for label, id := range map[string]int64{"outcome_unknown": outcome.ID, "scheduled": scheduled.ID, "active": active.ID} {
		if _, err := database.SystemGetJob(ctx, id); err != nil {
			t.Fatalf("%s job was pruned: %v", label, err)
		}
	}

	future := expires.Add(time.Hour)
	database.now = func() time.Time { return future }
	job, err := database.ForUser(user.ID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: firstProfile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: future,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ForUser(user.ID).RequestJobCancellation(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SystemGetJob(ctx, scheduled.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired scheduler dedup row error=%v", err)
	}
	if _, err := database.SystemGetJob(ctx, outcome.ID); err != nil {
		t.Fatalf("outcome_unknown was pruned after expiry cleanup: %v", err)
	}
	if _, err := database.SystemGetJob(ctx, active.ID); err != nil {
		t.Fatalf("active job was pruned after expiry cleanup: %v", err)
	}
}

func TestTerminalTransitionPreservesOldActiveJobAfterNewerHistory(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "old-active-history")
	now := time.Date(2031, time.January, 15, 12, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }

	oldActive, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldActive, err = database.SystemTransitionJob(ctx, oldActive.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}

	var oldestNewerTerminal int64
	for index := 0; index < MaxTerminalJobHistoryPerUser; index++ {
		job, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
			ProfileID: profile.ID, Command: model.CommandAuthCheck,
			RunMode: model.RunModeManual, DueAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldestNewerTerminal = job.ID
		}
		if err := database.ForUser(testUserID).RequestJobCancellation(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}

	finished, err := database.SystemTransitionJob(ctx, oldActive.ID,
		[]model.JobStatus{model.JobRunning}, model.JobSucceeded, JobTransition{})
	if err != nil {
		t.Fatalf("finish old active job: %v", err)
	}
	if finished.ID != oldActive.ID || finished.Status != model.JobSucceeded {
		t.Fatalf("finished job=%+v", finished)
	}
	if _, err := database.SystemGetJob(ctx, oldActive.ID); err != nil {
		t.Fatalf("just-finished old job was pruned: %v", err)
	}
	if _, err := database.SystemGetJob(ctx, oldestNewerTerminal); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest superseded terminal error=%v", err)
	}
}

func TestExpiredHistoryAtRetainedCapIsPrunedBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "expiry-cap")
	now := time.Date(2031, time.January, 15, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	database.now = func() time.Time { return now }
	protected := createFutureProtectedTerminalJobs(
		t, database, profile, "expiry-cap", MaxRetainedJobsPerUser, now, expires,
	)

	future := expires.Add(time.Hour)
	database.now = func() time.Time { return future }
	queued, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: future,
	})
	if err != nil {
		t.Fatalf("enqueue after protected history expired: %v", err)
	}
	if queued.Status != model.JobQueued {
		t.Fatalf("queued job status=%s", queued.Status)
	}

	var total, terminal int
	if err := database.db.QueryRowContext(ctx, `
		SELECT count(*), sum(CASE WHEN status IN ('succeeded', 'failed', 'cancelled', 'interrupted') THEN 1 ELSE 0 END)
		FROM jobs WHERE user_id = ?
	`, testUserID).Scan(&total, &terminal); err != nil {
		t.Fatal(err)
	}
	if total != MaxTerminalJobHistoryPerUser+1 || terminal != MaxTerminalJobHistoryPerUser {
		t.Fatalf("jobs after expiry admission: total=%d terminal=%d", total, terminal)
	}
	if _, err := database.SystemGetJob(ctx, protected[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest expired history error=%v", err)
	}
	if _, err := database.SystemGetJob(ctx, protected[len(protected)-1]); err != nil {
		t.Fatalf("newest expired history was pruned: %v", err)
	}
}

func TestFailedDuplicateAdmissionRollsBackExpiryPrune(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, booking := fixtureProfileAndBooking(t, database, "expiry-rollback")
	now := time.Date(2031, time.January, 15, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	database.now = func() time.Time { return now }
	protected := createFutureProtectedTerminalJobs(
		t, database, profile, "expiry-rollback", MaxTerminalJobHistoryPerUser+1, now, expires,
	)
	bookingID := booking.ID
	duplicate := EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandDryRun,
		RunMode: model.RunModeDryRun, DueAt: now,
	}
	if _, err := database.ForUser(testUserID).EnqueueJob(ctx, duplicate); err != nil {
		t.Fatal(err)
	}

	future := expires.Add(time.Hour)
	database.now = func() time.Time { return future }
	if _, err := database.ForUser(testUserID).EnqueueJob(ctx, duplicate); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate admission error=%v", err)
	}
	if _, err := database.SystemGetJob(ctx, protected[0]); err != nil {
		t.Fatalf("failed insert did not roll back history prune: %v", err)
	}
	var total int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE user_id = ?", testUserID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != MaxTerminalJobHistoryPerUser+2 {
		t.Fatalf("jobs after rolled-back admission=%d", total)
	}

	pruned, err := database.SystemPruneTerminalJobs(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("maintenance pruned=%d want=1", pruned)
	}
	if _, err := database.SystemGetJob(ctx, protected[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("maintenance oldest-history error=%v", err)
	}
}

func createFutureProtectedTerminalJobs(
	t *testing.T,
	database *Store,
	profile model.Profile,
	prefix string,
	count int,
	dueAt, expiresAt time.Time,
) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		expires := expiresAt
		job, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
			ProfileID: profile.ID, Command: model.CommandAuthCheck,
			RunMode: model.RunModeManual, DueAt: dueAt, ExpiresAt: &expires,
			DedupKey: fmt.Sprintf("booking:%s:%d", prefix, index),
		})
		if err != nil {
			t.Fatalf("enqueue protected terminal %d: %v", index, err)
		}
		if err := database.ForUser(testUserID).RequestJobCancellation(ctx, job.ID); err != nil {
			t.Fatalf("cancel protected terminal %d: %v", index, err)
		}
		ids = append(ids, job.ID)
	}
	return ids
}

func TestRetainedJobHardLimitCannotBeBypassedByCriticalHistory(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "hard-history")
	now := formatTime(time.Now().UTC())
	for index := 0; index < MaxRetainedJobsPerUser; index++ {
		if _, err := database.db.ExecContext(ctx, `
			INSERT INTO jobs(
				user_id, profile_id, otp_source_id, command, run_mode, status,
				due_at, created_at, updated_at
			) VALUES (?, ?, ?, 'auth-check', 'manual', 'outcome_unknown', ?, ?, ?)
		`, testUserID, profile.ID, profile.OTPSourceID, now, now, now); err != nil {
			t.Fatalf("insert critical history %d: %v", index, err)
		}
	}
	if _, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
	}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("hard retained-job limit error=%v", err)
	}
	var total int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE user_id = ?", testUserID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != MaxRetainedJobsPerUser {
		t.Fatalf("retained jobs=%d want=%d", total, MaxRetainedJobsPerUser)
	}
}

func TestJobEventsRotateWithinRetainedJob(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "event-history")
	job, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{
		ProfileID: profile.ID, Command: model.CommandAuthCheck,
		RunMode: model.RunModeManual, DueAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var firstEventID int64
	for index := 0; index < MaxJobEventsPerJob+32; index++ {
		event, err := database.SystemAppendJobEvent(ctx, JobEventInput{
			JobID: job.ID, Level: "info", Kind: "bounded.test", Message: fmt.Sprintf("event %d", index),
		})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstEventID = event.ID
		}
	}
	events, err := database.SystemListJobEvents(ctx, job.ID, 0, 1000)
	if err != nil || len(events) != MaxJobEventsPerJob {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	if events[0].ID == firstEventID {
		t.Fatal("oldest event was not rotated")
	}
}
