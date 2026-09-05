package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type engineTestFixture struct {
	engine    *Engine
	store     *store.Store
	resources store.UserStore
	user      model.User
	booking   model.BookingRequest
}

func newEngineTestFixture(t *testing.T) engineTestFixture {
	t.Helper()
	ctx := context.Background()
	runtimeRoot := t.TempDir()
	box, err := secretcrypto.New(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenMigrated(ctx, filepath.Join(t.TempDir(), "buntzen.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	user, err := database.SetupAdmin(ctx, "owner", "engine test administrator password")
	if err != nil {
		t.Fatal(err)
	}
	resources := database.ForUser(user.ID)
	source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Example inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:engine-test",
		ProviderConfig: map[string]string{"auth_token": "synthetic-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{
		Name: "Example profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := resources.CreateBookingRequest(ctx, model.BookingRequest{
		Name: "Example booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2031-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeAuto,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all-day",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := control.NewHub()
	return engineTestFixture{
		engine: New(config.Config{
			MaxConcurrentJobs: 1,
			YodelOrigins:      []string{"https://example.test"},
			ProfilesDir:       filepath.Join(runtimeRoot, "profiles"),
			ArtifactsDir:      filepath.Join(runtimeRoot, "artifacts"),
		}, database, hub),
		store: database, resources: resources, user: user, booking: booking,
	}
}

func TestQueueBookingRejectsExplicitInvalidMode(t *testing.T) {
	fixture := newEngineTestFixture(t)
	invalid := model.RunMode("manul")
	if _, err := fixture.engine.QueueBooking(context.Background(), fixture.user.ID,
		fixture.booking.ID, model.CommandBook, invalid); err == nil || !strings.Contains(err.Error(), "manual or auto") {
		t.Fatalf("user queue invalid mode error = %v", err)
	}
	if _, err := fixture.engine.SystemQueueBooking(context.Background(), fixture.booking.ID,
		model.CommandBook, invalid); err == nil || !strings.Contains(err.Error(), "manual or auto") {
		t.Fatalf("system queue invalid mode error = %v", err)
	}
	if jobs, err := fixture.resources.ListJobs(context.Background(), 10); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs after invalid modes = %+v err=%v", jobs, err)
	}
}

func TestUserAndSystemQueuePathsSharePendingDeduplication(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	first, err := fixture.engine.QueueBooking(ctx, fixture.user.ID,
		fixture.booking.ID, model.CommandDryRun, model.RunModeDryRun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.SystemQueueBooking(ctx, fixture.booking.ID,
		model.CommandDryRun, model.RunModeDryRun); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("system duplicate error=%v", err)
	}
	if err := fixture.resources.RequestJobCancellation(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.SystemQueueBooking(ctx, fixture.booking.ID,
		model.CommandDryRun, model.RunModeDryRun); err != nil {
		t.Fatalf("system queue after terminal job: %v", err)
	}
	if _, err := fixture.engine.QueueBooking(ctx, fixture.user.ID,
		fixture.booking.ID, model.CommandDryRun, model.RunModeDryRun); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("user duplicate error=%v", err)
	}
}

func TestSchedulerCannotRepeatManualBookingAcrossModesOrOutcomes(t *testing.T) {
	for _, status := range []model.JobStatus{model.JobQueued, model.JobSucceeded, model.JobOutcomeUnknown} {
		t.Run(string(status), func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			ctx := context.Background()
			booking := fixture.booking
			booking.ScheduleEnabled = true
			booking.ConfirmationMode = model.RunModeAuto
			booking, err := fixture.resources.UpdateBookingRequest(ctx, booking)
			if err != nil {
				t.Fatal(err)
			}
			first, err := fixture.engine.QueueBooking(ctx, fixture.user.ID, booking.ID, model.CommandBook, model.RunModeManual)
			if err != nil {
				t.Fatal(err)
			}
			if status != model.JobQueued {
				if _, err := fixture.store.SystemTransitionJob(ctx, first.ID, []model.JobStatus{model.JobQueued}, model.JobRunning, store.JobTransition{}); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.store.SystemTransitionJob(ctx, first.ID, []model.JobStatus{model.JobRunning}, status,
					store.JobTransition{ConfirmationStarted: true}); err != nil {
					t.Fatal(err)
				}
			}
			window, err := scheduler.WindowFor(booking)
			if err != nil {
				t.Fatal(err)
			}
			fixture.engine.queueScheduled(ctx, window.ReleaseAt)
			fixture.engine.queueScheduled(ctx, window.ReleaseAt.Add(15*time.Second))
			jobs, err := fixture.resources.ListJobs(ctx, 10)
			if err != nil || len(jobs) != 1 || jobs[0].ID != first.ID || jobs[0].Status != status {
				t.Fatalf("scheduler duplicated %s manual booking: jobs=%+v err=%v", status, jobs, err)
			}
		})
	}
}

func TestBookAdmissionWaitsForTheBoundedPrepWindowAndCarriesExpiry(t *testing.T) {
	fixture := newEngineTestFixture(t)
	window, err := scheduler.WindowFor(fixture.booking)
	if err != nil {
		t.Fatal(err)
	}
	farBeforePrep := window.PrepAt.Add(-48 * time.Hour)
	params, err := bookingEnqueueParams(
		fixture.booking, model.CommandBook, model.RunModeManual, farBeforePrep,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !params.DueAt.Equal(window.PrepAt) {
		t.Fatalf("book due=%s want prep=%s", params.DueAt, window.PrepAt)
	}
	if params.ExpiresAt == nil || !params.ExpiresAt.Equal(window.PollEndsAt) {
		t.Fatalf("book expiry=%v want=%s", params.ExpiresAt, window.PollEndsAt)
	}

	duringPrep := window.PrepAt.Add(time.Minute)
	params, err = bookingEnqueueParams(
		fixture.booking, model.CommandBook, model.RunModeAuto, duringPrep,
	)
	if err != nil || !params.DueAt.Equal(duringPrep) {
		t.Fatalf("in-window params=%+v err=%v", params, err)
	}
	if _, err := bookingEnqueueParams(
		fixture.booking, model.CommandBook, model.RunModeManual,
		window.PollEndsAt,
	); err == nil || !strings.Contains(err.Error(), "window has ended") {
		t.Fatalf("expired booking error=%v", err)
	}

	authParams, err := bookingEnqueueParams(
		fixture.booking, model.CommandAuthCheck, model.RunModeManual, farBeforePrep,
	)
	if err != nil || !authParams.DueAt.Equal(farBeforePrep) || authParams.ExpiresAt != nil {
		t.Fatalf("auth-check timing=%+v err=%v", authParams, err)
	}
}

func TestManagedProfileDirectoriesUseImmutableOwnedIdentity(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	profile, err := fixture.resources.GetProfile(ctx, fixture.booking.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := ensureManagedProfileDirectory(fixture.engine.config.ProfilesDir, profile)
	if err != nil {
		t.Fatal(err)
	}
	sessionSentinel := filepath.Join(profilePath, "authenticated-session")
	if err := os.WriteFile(sessionSentinel, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(fixture.engine.config.ProfilesDir, "operator-notes")
	if err := os.MkdirAll(unknownPath, 0o700); err != nil {
		t.Fatal(err)
	}

	member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{
		Username: "other-member", Password: "another strong synthetic password",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberStore := fixture.store.ForUser(member.ID)
	source, err := memberStore.CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Other inbox", Provider: model.OTPProviderTwilio,
		Identity:       "twilio:other-profile-owner",
		ProviderConfig: map[string]string{"auth_token": "synthetic-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherProfile, err := memberStore.CreateProfile(ctx, store.ProfileInput{
		Name: "Other profile", DefaultVehicle: "Other Vehicle", OTPSourceID: source.ID,
		Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherPath, err := ensureManagedProfileDirectory(fixture.engine.config.ProfilesDir, otherProfile)
	if err != nil {
		t.Fatal(err)
	}
	if otherPath == profilePath {
		t.Fatal("different database profiles shared a browser directory")
	}
	if _, err := os.Stat(filepath.Join(otherPath, "authenticated-session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("other user inherited the first user's session: %v", err)
	}
	if _, err := ensureManagedProfileDirectory(fixture.engine.config.ProfilesDir, model.Profile{
		ID: profile.ID, UserID: member.ID,
	}); err == nil || !strings.Contains(err.Error(), "different profile") {
		t.Fatalf("mismatched profile owner marker error=%v", err)
	}

	orphan := model.Profile{ID: 999_999, UserID: 999_999}
	orphanPath, err := ensureManagedProfileDirectory(fixture.engine.config.ProfilesDir, orphan)
	if err != nil {
		t.Fatal(err)
	}
	// An empty snapshot simulates maintenance racing with newly created
	// profiles. Exact immutable-ID rechecks must preserve both live profiles.
	if err := fixture.engine.cleanupProfileEntries(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned managed profile error=%v", err)
	}
	for label, path := range map[string]string{
		"existing retained": profilePath,
		"newly retained":    otherPath,
		"unmanaged":         unknownPath,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s profile directory was removed: %v", label, err)
		}
	}
}

func TestArtifactQuotaRemovesOversizedRetainedDiagnostics(t *testing.T) {
	fixture := newEngineTestFixture(t)
	artifactDir := filepath.Join(fixture.engine.config.ArtifactsDir, "job-42")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= maxJobArtifactFiles; index++ {
		path := filepath.Join(artifactDir, fmt.Sprintf("artifact-%03d", index))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exceeded, err := artifactLimitExceeded(artifactDir)
	if err != nil || !exceeded {
		t.Fatalf("artifact footprint exceeded=%v err=%v", exceeded, err)
	}
	if err := fixture.engine.enforceArtifactLimit(42); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized artifact directory error=%v", err)
	}
}

func TestStartSchedulesSessionAndArtifactMaintenance(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	fixture.engine.config.MaxConcurrentJobs = 0
	fixture.engine.config.ArtifactsDir = filepath.Join(t.TempDir(), "artifacts")
	if _, err := fixture.store.NewSession(ctx, fixture.user.ID, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	job, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &fixture.booking.ID, Command: model.CommandDryRun,
		RunMode: model.RunModeDryRun, DueAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	retainedDir := filepath.Join(fixture.engine.config.ArtifactsDir, fmt.Sprintf("job-%d", job.ID))
	staleDir := filepath.Join(fixture.engine.config.ArtifactsDir, "job-999999")
	unknownDir := filepath.Join(fixture.engine.config.ArtifactsDir, "operator-notes")
	for _, directory := range []string{retainedDir, staleDir, unknownDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	fixture.engine.Start(ctx)
	t.Cleanup(fixture.engine.Stop)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(staleDir)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled artifact maintenance did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if purged, err := fixture.store.PurgeExpiredSessions(ctx); err != nil || purged != 0 {
		t.Fatalf("expired session remained after scheduled maintenance: purged=%d err=%v", purged, err)
	}
	for label, directory := range map[string]string{"retained": retainedDir, "unknown": unknownDir} {
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("%s artifact directory was removed: %v", label, err)
		}
	}
}

func TestCredentialDecryptionFailsClosedOnPersistedUnapprovedOrigin(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	booking := fixture.booking
	booking.LoginProbeURL = "https://attacker.example/login"
	booking.AllDayPassURL = "https://attacker.example/pass"
	booking, err := fixture.resources.UpdateBookingRequest(ctx, booking)
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	job, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandAuthCheck, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = fixture.store.SystemClaimNextDueJobAt(ctx, "origin-test", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.execute(ctx, job); err == nil || !strings.Contains(err.Error(), "approved Yodel origin") {
		t.Fatalf("execute unapproved origin error = %v", err)
	}
}

func TestFinishPublishesTheAtomicallyClassifiedTerminalState(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	bookingID := fixture.booking.ID
	job, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandDryRun, RunMode: model.RunModeDryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err = fixture.store.SystemTransitionJob(ctx, job.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, store.JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.resources.RequestJobCancellation(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	live, unsubscribe := fixture.engine.Hub().Subscribe(strconv.FormatInt(job.ID, 10))
	defer unsubscribe()
	zero := 0
	fixture.engine.finish(job.ID, model.JobSucceeded, "worker reported success", &zero)

	finished, err := fixture.resources.GetJob(ctx, job.ID)
	if err != nil || finished.Status != model.JobCancelled {
		t.Fatalf("classified job = %+v err=%v", finished, err)
	}
	events, err := fixture.resources.ListJobEvents(ctx, job.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Kind != "job.cancelled" || events[0].Message != finished.Message {
		t.Fatalf("classified events = %+v err=%v", events, err)
	}
	select {
	case event := <-live:
		data, ok := event.Data.(map[string]any)
		if event.Kind != "complete" || !ok || data["status"] != model.JobCancelled || data["message"] != finished.Message {
			t.Fatalf("classified live event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("classified completion event was not published")
	}
}
