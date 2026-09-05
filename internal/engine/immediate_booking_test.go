package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/twilio"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func TestBookNowAcceptsReleasedAvailabilityWithFixedManualDeadline(t *testing.T) {
	fixture := newEngineTestFixture(t)
	window, err := scheduler.WindowFor(fixture.booking)
	if err != nil {
		t.Fatal(err)
	}
	late := window.PollEndsAt.Add(time.Hour)
	if _, err := bookingEnqueueParams(fixture.booking, model.CommandBook, "", late); err == nil {
		t.Fatal("release booking unexpectedly accepted after its window")
	}
	params, err := immediateBookingEnqueueParams(fixture.booking, late)
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{
		Command: params.Command, RunMode: params.RunMode, RunImmediately: params.RunImmediately,
		DueAt: params.DueAt, ExpiresAt: params.ExpiresAt,
	}
	if !job.RunImmediately || job.Command != model.CommandBook || job.RunMode != model.RunModeManual || !job.DueAt.Equal(late) || job.ExpiresAt == nil || !job.ExpiresAt.Equal(late.Add(15*time.Minute)) {
		t.Fatalf("immediate persisted job = %+v", job)
	}
	// Delayed start receives the original deadline, not a new 15-minute window.
	timing, err := bookingStartTiming(job, fixture.booking, late.Add(14*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := timing["release_at"]; present || timing["auth_deadline_at"] != job.ExpiresAt.Format(time.RFC3339Nano) || timing["poll_deadline_seconds"] != 60 {
		t.Fatalf("late immediate timing = %+v", timing)
	}
	if _, err := bookingStartTiming(job, fixture.booking, *job.ExpiresAt); err == nil {
		t.Fatal("expired immediate job allowed to start")
	}
}

func TestImmediateExpiryCancelsProviderSetup(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	booking := fixture.booking
	booking.TargetDate = time.Now().UTC().Format(time.DateOnly)
	booking, err := fixture.resources.UpdateBookingRequest(ctx, booking)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := fixture.resources.GetProfile(ctx, booking.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	if _, err := fixture.resources.UpdateOTPSource(ctx, profile.OTPSourceID, store.OTPSourceInput{
		Name: "Deadline inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:engine-test",
		ProviderConfig: twilio.Config{AccountSID: "synthetic-account", AuthToken: "synthetic-token", ToNumber: "+15559876543", BaseURL: server.URL},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expiry := now.Add(2 * time.Second)
	job := model.Job{
		ProfileID: profile.ID, OTPSourceID: profile.OTPSourceID, BookingRequestID: &booking.ID,
		Command: model.CommandBook, RunMode: model.RunModeManual, RunImmediately: true,
		DueAt: now, ExpiresAt: &expiry,
	}
	finished := make(chan error, 1)
	go func() {
		_, err := fixture.engine.execute(ctx, job)
		finished <- err
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("execution stopped before provider setup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("provider setup never started")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("provider setup did not honor fixed job expiry: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job expiry did not cancel blocked provider setup")
	}
}

func TestBookNowRejectsUnreleasedAndPastTargetDates(t *testing.T) {
	fixture := newEngineTestFixture(t)
	fixture.booking.Timezone = "America/Vancouver"
	window, err := scheduler.WindowFor(fixture.booking)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		now       time.Time
		wantError string
	}{
		{"before release", window.ReleaseAt.Add(-time.Second), "not been released"},
		{"at release", window.ReleaseAt, ""},
		{"late target day", window.ReleaseAt.Add(40 * time.Hour), ""},
		{"following day", window.ReleaseAt.Add(41 * time.Hour), "target date has passed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := immediateBookingEnqueueParams(fixture.booking, test.now.UTC())
			if test.wantError == "" && err != nil || test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("admission error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestReleaseBookingRetainsPreparationAndAuthenticationTiming(t *testing.T) {
	fixture := newEngineTestFixture(t)
	window, err := scheduler.WindowFor(fixture.booking)
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{
		Command: model.CommandBook, RunMode: model.RunModeAuto,
		DueAt: window.PrepAt, ExpiresAt: &window.PollEndsAt,
	}
	if _, err := bookingStartTiming(job, fixture.booking, window.PrepAt.Add(-time.Second)); err == nil {
		t.Fatal("release booking started before preparation window")
	}
	timing, err := bookingStartTiming(job, fixture.booking, window.PrepAt)
	if err != nil || timing["release_at"] != window.ReleaseAt.Format(time.RFC3339) || timing["auth_deadline_at"] != window.AuthDeadlineAt.Format(time.RFC3339) {
		t.Fatalf("release preparation timing = %+v, err = %v", timing, err)
	}
	timing, err = bookingStartTiming(job, fixture.booking, window.ReleaseAt.Add(time.Minute))
	if err != nil || timing["poll_deadline_seconds"] != 60 || timing["auth_deadline_at"] != window.AuthDeadlineAt.Format(time.RFC3339) {
		t.Fatalf("release polling timing = %+v, err = %v", timing, err)
	}
	if _, exists := timing["release_at"]; exists {
		t.Fatal("already-released job retained a release wait")
	}
	if _, err := bookingStartTiming(job, fixture.booking, window.PollEndsAt); err == nil {
		t.Fatal("release booking started after polling window")
	}
}

func TestBookNowCannotBypassProfileDateReservation(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	booking := fixture.booking
	booking.TargetDate = time.Now().UTC().Format(time.DateOnly)
	booking, err := fixture.resources.UpdateBookingRequest(ctx, booking)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fixture.engine.QueueBookingNow(ctx, fixture.user.ID, booking.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.QueueBookingNow(ctx, fixture.user.ID+1, booking.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign immediate booking error = %v", err)
	}
	other := booking
	other.Name = "Another request for the same date"
	other, err = fixture.resources.CreateBookingRequest(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.JobStatus{model.JobQueued, model.JobSucceeded} {
		if status == model.JobSucceeded {
			if _, err := fixture.store.SystemTransitionJob(ctx, first.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, store.JobTransition{}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.SystemTransitionJob(ctx, first.ID, []model.JobStatus{model.JobRunning}, status, store.JobTransition{ConfirmationStarted: true}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := fixture.engine.QueueBookingNow(ctx, fixture.user.ID, other.ID); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate of %s immediate job error = %v", status, err)
		}
		if _, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{BookingRequestID: &other.ID, Command: model.CommandBook, RunMode: model.RunModeAuto}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("scheduled duplicate of %s immediate job error = %v", status, err)
		}
	}
}

func TestImmediateWorkerRejectsInvalidModeBeforeLoadingSecrets(t *testing.T) {
	fixture := newEngineTestFixture(t)
	now := time.Now().UTC()
	expiry := now.Add(time.Minute)
	for _, test := range []struct {
		command model.JobCommand
		mode    model.RunMode
	}{
		{model.CommandBook, model.RunModeAuto},
		{model.CommandAuthCheck, model.RunModeManual},
		{model.CommandDryRun, model.RunModeDryRun},
	} {
		_, err := fixture.engine.execute(context.Background(), model.Job{Command: test.command, RunMode: test.mode, RunImmediately: true, DueAt: now, ExpiresAt: &expiry})
		if err == nil || !strings.Contains(err.Error(), "manual approval") {
			t.Fatalf("command %s mode %s error = %v", test.command, test.mode, err)
		}
	}
}
