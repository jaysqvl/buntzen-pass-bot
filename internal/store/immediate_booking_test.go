package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestImmediateBookingAdmissionRequiresManualModeAndBoundedExpiry(t *testing.T) {
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "immediate-policy")
	now := time.Now().UTC()
	expiry := now.Add(15 * time.Minute)
	valid := EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual, RunImmediately: true, DueAt: now, ExpiresAt: &expiry}
	for _, test := range []struct {
		name   string
		mutate func(*EnqueueJobParams)
	}{
		{"automatic", func(p *EnqueueJobParams) { p.RunMode = model.RunModeAuto }},
		{"auth check", func(p *EnqueueJobParams) { p.Command = model.CommandAuthCheck }},
		{"dry run", func(p *EnqueueJobParams) { p.Command = model.CommandDryRun }},
		{"no expiry", func(p *EnqueueJobParams) { p.ExpiresAt = nil }},
		{"extended expiry", func(p *EnqueueJobParams) { later := expiry.Add(time.Second); p.ExpiresAt = &later }},
		{"future due time", func(p *EnqueueJobParams) { p.DueAt = now.Add(time.Minute) }},
		{"expired", func(p *EnqueueJobParams) { p.DueAt = now.Add(-time.Minute); p.ExpiresAt = &now }},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := valid
			test.mutate(&params)
			if _, err := database.SystemEnqueueJob(context.Background(), params); err == nil {
				t.Fatal("invalid immediate job accepted")
			}
		})
	}
	job, err := database.EnqueueJob(context.Background(), testUserID, valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"run_mode = 'auto'", "command = 'auth-check'", "expires_at = NULL", "run_immediately = 2"} {
		if _, err := database.db.Exec("UPDATE jobs SET "+mutation+" WHERE id = ?", job.ID); err == nil {
			t.Fatalf("SQL accepted %s", mutation)
		}
	}
}

func TestImmediateDeadlineSurvivesReopenClaimAndRecovery(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "immediate-reopen")
	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(15 * time.Minute)
	job, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual, RunImmediately: true, DueAt: now, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := OpenMigrated(ctx, database.path, testEncryptor(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peer.now = func() time.Time { return now.Add(14 * time.Minute) }
	claimed, err := peer.SystemClaimNextDueJob(ctx, "reopened-worker")
	if err != nil || claimed.ID != job.ID || !claimed.RunImmediately || claimed.ExpiresAt == nil || !claimed.ExpiresAt.Equal(expiry) {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if _, err := peer.SystemRecoverInterruptedJobs(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := database.GetJob(ctx, testUserID, job.ID)
	if err != nil || recovered.Status != model.JobInterrupted || !recovered.RunImmediately || !recovered.ExpiresAt.Equal(expiry) {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if _, err := peer.SystemClaimNextDueJob(ctx, "retry-worker"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupted job was requeued: %v", err)
	}
}

func TestQueuedImmediateBookingExpiresWithoutBeingClaimed(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	_, booking := fixtureProfileAndBooking(t, database, "immediate-expiry")
	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(time.Minute)
	job, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{BookingRequestID: &booking.ID, Command: model.CommandBook, RunMode: model.RunModeManual, RunImmediately: true, DueAt: now, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	database.now = func() time.Time { return expiry }
	if _, err := database.SystemClaimNextDueJob(ctx, "late-worker"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired job claimed: %v", err)
	}
	stored, err := database.GetJob(ctx, testUserID, job.ID)
	if err != nil || stored.Status != model.JobFailed || !stored.RunImmediately || !stored.ExpiresAt.Equal(expiry) {
		t.Fatalf("expired=%+v err=%v", stored, err)
	}
}
