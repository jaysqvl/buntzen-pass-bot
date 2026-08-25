package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestUserResourcesAreIsolatedAndNamesArePerUser(t *testing.T) {
	ctx := context.Background()
	database, firstID, secondID := ownershipStore(t)
	firstSource, firstProfile, firstBooking := createOwnedResources(t, database, firstID, "first")
	secondSource, secondProfile, secondBooking := createOwnedResources(t, database, secondID, "second")

	for label, pair := range map[string]struct{ got, want int64 }{
		"first source":   {firstSource.UserID, firstID},
		"first profile":  {firstProfile.UserID, firstID},
		"first booking":  {firstBooking.UserID, firstID},
		"second source":  {secondSource.UserID, secondID},
		"second profile": {secondProfile.UserID, secondID},
		"second booking": {secondBooking.UserID, secondID},
	} {
		if pair.got != pair.want {
			t.Fatalf("%s user_id=%d want=%d", label, pair.got, pair.want)
		}
	}

	first := database.ForUser(firstID)
	second := database.ForUser(secondID)
	if sources, err := first.ListOTPSources(ctx); err != nil || len(sources) != 1 || sources[0].ID != firstSource.ID {
		t.Fatalf("first sources=%+v err=%v", sources, err)
	}
	if profiles, err := second.ListProfiles(ctx); err != nil || len(profiles) != 1 || profiles[0].ID != secondProfile.ID {
		t.Fatalf("second profiles=%+v err=%v", profiles, err)
	}
	if bookings, err := first.ListBookingRequests(ctx); err != nil || len(bookings) != 1 || bookings[0].ID != firstBooking.ID {
		t.Fatalf("first bookings=%+v err=%v", bookings, err)
	}

	assertNotFound(t, func() error { _, err := second.GetOTPSource(ctx, firstSource.ID); return err })
	assertNotFound(t, func() error { _, err := second.GetProfile(ctx, firstProfile.ID); return err })
	assertNotFound(t, func() error { _, err := second.GetBookingRequest(ctx, firstBooking.ID); return err })
	assertNotFound(t, func() error {
		var config map[string]any
		return second.GetOTPSourceConfig(ctx, firstSource.ID, &config)
	})
	assertNotFound(t, func() error { _, err := second.GetProfileCredentials(ctx, firstProfile.ID); return err })
	assertNotFound(t, func() error { return second.DeleteOTPSource(ctx, firstSource.ID) })
	assertNotFound(t, func() error { return second.DeleteProfile(ctx, firstProfile.ID) })
	assertNotFound(t, func() error { return second.DeleteBookingRequest(ctx, firstBooking.ID) })

	if _, err := database.SystemGetOTPSource(ctx, firstSource.ID); err != nil {
		t.Fatalf("trusted worker could not resolve source: %v", err)
	}
	if _, err := database.SystemGetProfile(ctx, secondProfile.ID); err != nil {
		t.Fatalf("trusted worker could not resolve profile: %v", err)
	}
	if _, err := database.SystemGetBookingRequest(ctx, secondBooking.ID); err != nil {
		t.Fatalf("trusted worker could not resolve booking: %v", err)
	}
}

func TestPhysicalIdentitiesRemainGloballyExclusive(t *testing.T) {
	ctx := context.Background()
	database, firstID, secondID := ownershipStore(t)
	firstSource, _, _ := createOwnedResources(t, database, firstID, "first")

	if _, err := database.ForUser(secondID).CreateOTPSource(ctx, OTPSourceInput{
		Name: "different display name", Provider: firstSource.Provider, Identity: firstSource.Identity,
		ProviderConfig: map[string]string{"password": "second-secret"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate physical inbox error=%v", err)
	}

	_, err := database.ForUser(secondID).CreateOTPSource(ctx, OTPSourceInput{
		Name: "same resource name", Provider: model.OTPProviderBlueBubbles,
		Identity: "http://second-unique.test:1234", ProviderConfig: map[string]string{"password": "second-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForUser(firstID).CreateOTPSource(ctx, OTPSourceInput{
		Name: "aliased messages inbox", Provider: model.OTPProviderBlueBubbles,
		Identity: "http://different-alias.test:1234", ProviderConfig: map[string]string{"password": "alias-secret"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second BlueBubbles inbox error=%v", err)
	}
	if _, err := database.ForUser(secondID).CreateProfile(ctx, ProfileInput{
		Name:           "cross-owner source",
		DefaultVehicle: "Example Vehicle", OTPSourceID: firstSource.ID, DefaultTimeoutMS: 15_000,
		Enabled: true, Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-owner source association error=%v", err)
	}
}

func TestJobOwnershipIsDerivedAndAllChildrenAreScoped(t *testing.T) {
	ctx := context.Background()
	database, firstID, secondID := ownershipStore(t)
	_, _, firstBooking := createOwnedResources(t, database, firstID, "first")
	_, _, secondBooking := createOwnedResources(t, database, secondID, "second")

	firstBookingID := firstBooking.ID
	firstJob, err := database.ForUser(firstID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &firstBookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstJob.UserID != firstID {
		t.Fatalf("job user_id=%d want=%d", firstJob.UserID, firstID)
	}
	if _, err := database.ForUser(secondID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &firstBookingID, Command: model.CommandBook, RunMode: model.RunModeManual,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner enqueue error=%v", err)
	}

	secondBookingID := secondBooking.ID
	secondJob, err := database.SystemEnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &secondBookingID, Command: model.CommandDryRun, RunMode: model.RunModeDryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondJob.UserID != secondID {
		t.Fatalf("system-enqueued job user_id=%d want=%d", secondJob.UserID, secondID)
	}

	firstJobs, err := database.ForUser(firstID).ListJobs(ctx, 100)
	if err != nil || len(firstJobs) != 1 || firstJobs[0].ID != firstJob.ID {
		t.Fatalf("first jobs=%+v err=%v", firstJobs, err)
	}
	secondJobs, err := database.ForUser(secondID).ListJobs(ctx, 100)
	if err != nil || len(secondJobs) != 1 || secondJobs[0].ID != secondJob.ID {
		t.Fatalf("second jobs=%+v err=%v", secondJobs, err)
	}
	assertNotFound(t, func() error { _, err := database.ForUser(secondID).GetJob(ctx, firstJob.ID); return err })

	firstJob, err = database.SystemTransitionJob(ctx, firstJob.ID,
		[]model.JobStatus{model.JobQueued}, model.JobRunning, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	firstJob, err = database.SystemTransitionJob(ctx, firstJob.ID,
		[]model.JobStatus{model.JobRunning}, model.JobAwaitingApproval, JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	event, err := database.SystemAppendJobEvent(ctx, JobEventInput{
		JobID: firstJob.ID, Level: "info", Kind: "approval.waiting", Message: "Waiting for approval.",
	})
	if err != nil || event.UserID != firstID {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	events, err := database.ForUser(firstID).ListJobEvents(ctx, firstJob.ID, 0, 100)
	if err != nil || len(events) != 1 || events[0].UserID != firstID {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	assertNotFound(t, func() error {
		_, err := database.ForUser(secondID).ListJobEvents(ctx, firstJob.ID, 0, 100)
		return err
	})
	assertNotFound(t, func() error { return database.ForUser(secondID).RequestJobCancellation(ctx, firstJob.ID) })
	assertNotFound(t, func() error {
		_, err := database.ForUser(secondID).RecordJobDecision(ctx, firstJob.ID, model.DecisionApprove)
		return err
	})

	decision, err := database.ForUser(firstID).RecordJobDecision(ctx, firstJob.ID, model.DecisionApprove)
	if err != nil || decision.UserID != firstID {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	assertNotFound(t, func() error {
		_, err := database.ForUser(secondID).GetJobDecision(ctx, firstJob.ID)
		return err
	})
}

func TestInvalidUserScopeFailsClosed(t *testing.T) {
	database := testStore(t)
	ctx := context.Background()
	invalid := database.ForUser(0)
	if !errors.Is(func() error { _, err := invalid.ListProfiles(ctx); return err }(), ErrUserRequired) {
		t.Fatal("invalid user scope did not fail closed")
	}
	if _, err := database.CreateOTPSource(ctx, 0, OTPSourceInput{}); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("direct create with zero owner error=%v", err)
	}
}

func ownershipStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	database := testStore(t)
	admin, err := database.SetupAdmin(context.Background(), "owner-admin", "a strong admin password")
	if err != nil {
		t.Fatal(err)
	}
	member, err := database.CreateMember(context.Background(), CreateUserInput{
		Username: "owner-member", Password: "a strong member password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return database, admin.ID, member.ID
}

func createOwnedResources(t *testing.T, database *Store, userID int64, unique string) (model.OTPSource, model.Profile, model.BookingRequest) {
	t.Helper()
	ctx := context.Background()
	resources := database.ForUser(userID)
	source, err := resources.CreateOTPSource(ctx, OTPSourceInput{
		Name: "shared source name", Provider: model.OTPProviderTwilio,
		Identity: "twilio:" + unique, ProviderConfig: map[string]string{"auth_token": unique + "-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(ctx, ProfileInput{
		Name: "shared profile name", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := resources.CreateBookingRequest(ctx, model.BookingRequest{
		UserID: 999_999, // Deliberately ignored in favor of the bound actor.
		Name:   "shared booking name", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2031-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all-day",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, profile, booking
}

func assertNotFound(t *testing.T, operation func() error) {
	t.Helper()
	if err := operation(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner operation error=%v, want ErrNotFound", err)
	}
}

func TestJobWorkerOwnerIsOperationalMetadata(t *testing.T) {
	ctx := context.Background()
	database, userID, _ := ownershipStore(t)
	_, _, booking := createOwnedResources(t, database, userID, "worker-owner")
	bookingID := booking.ID
	due := time.Now().UTC()
	if _, err := database.ForUser(userID).EnqueueJob(ctx, EnqueueJobParams{
		BookingRequestID: &bookingID, Command: model.CommandDryRun, RunMode: model.RunModeDryRun, DueAt: due,
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := database.SystemClaimNextDueJobAt(ctx, "worker-a", due.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claimed.UserID != userID || claimed.WorkerOwner != "worker-a" {
		t.Fatalf("claimed job=%+v", claimed)
	}
}
