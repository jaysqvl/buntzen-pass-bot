package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func TestDeletedMemberStorageReconciliationIsOwnerIsolated(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()

	retainedProfile, err := fixture.resources.GetProfile(ctx, fixture.booking.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	retainedProfilePath, err := ensureManagedProfileDirectory(
		fixture.engine.config.ProfilesDir, retainedProfile,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retainedProfilePath, "retained-session"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	retainedBookingID := fixture.booking.ID
	retainedJob, err := fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &retainedBookingID,
		Command:          model.CommandDryRun,
		RunMode:          model.RunModeDryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.resources.RequestJobCancellation(ctx, retainedJob.ID); err != nil {
		t.Fatal(err)
	}
	retainedArtifactPath := filepath.Join(
		fixture.engine.config.ArtifactsDir, "job-"+strconv.FormatInt(retainedJob.ID, 10),
	)
	if err := os.MkdirAll(retainedArtifactPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retainedArtifactPath, "retained.txt"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}

	member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{
		Username: "deletion-cleanup-member",
		Password: "a strong deletion cleanup password",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberStore := fixture.store.ForUser(member.ID)
	source, err := memberStore.CreateOTPSource(ctx, store.OTPSourceInput{
		Name:           "Deletion cleanup inbox",
		Provider:       model.OTPProviderTwilio,
		Identity:       "twilio:deletion-cleanup-member",
		ProviderConfig: map[string]string{"auth_token": "synthetic-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	memberProfile, err := memberStore.CreateProfile(ctx, store.ProfileInput{
		Name: "Deletion cleanup profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := memberStore.CreateBookingRequest(ctx, model.BookingRequest{
		Name: "Deletion cleanup booking", ProfileID: memberProfile.ID, Enabled: true,
		TargetDate: "2031-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all-day",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bookingID := booking.ID
	memberJob, err := memberStore.EnqueueJob(ctx, store.EnqueueJobParams{
		BookingRequestID: &bookingID,
		Command:          model.CommandDryRun,
		RunMode:          model.RunModeDryRun,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := memberStore.RequestJobCancellation(ctx, memberJob.ID); err != nil {
		t.Fatal(err)
	}
	memberProfilePath, err := ensureManagedProfileDirectory(
		fixture.engine.config.ProfilesDir, memberProfile,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memberProfilePath, "member-session"), []byte("deleted"), 0o600); err != nil {
		t.Fatal(err)
	}
	memberArtifactPath := filepath.Join(
		fixture.engine.config.ArtifactsDir, "job-"+strconv.FormatInt(memberJob.ID, 10),
	)
	if err := os.MkdirAll(memberArtifactPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memberArtifactPath, "member.txt"), []byte("deleted"), 0o600); err != nil {
		t.Fatal(err)
	}

	unmarkedProfilePath := filepath.Join(fixture.engine.config.ProfilesDir, "profile-999999")
	unmanagedProfilePath := filepath.Join(fixture.engine.config.ProfilesDir, "operator-notes")
	unmanagedArtifactPath := filepath.Join(fixture.engine.config.ArtifactsDir, "job-not-managed")
	for _, path := range []string{unmarkedProfilePath, unmanagedProfilePath, unmanagedArtifactPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	member, err = fixture.store.UpdateUser(ctx, member.ID, store.UserUpdateInput{
		Username: member.Username,
		Status:   model.UserDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.DeleteMember(ctx, member.ID, member.Username); err != nil {
		t.Fatal(err)
	}
	if err := fixture.engine.ReconcileStorage(ctx); err != nil {
		t.Fatal(err)
	}

	for label, path := range map[string]string{
		"deleted member profile":   memberProfilePath,
		"deleted member artifacts": memberArtifactPath,
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains after reconciliation: %v", label, err)
		}
	}
	for label, path := range map[string]string{
		"retained owner profile":   retainedProfilePath,
		"retained owner artifacts": retainedArtifactPath,
		"unmarked profile":         unmarkedProfilePath,
		"unmanaged profile":        unmanagedProfilePath,
		"unmanaged artifacts":      unmanagedArtifactPath,
	} {
		if _, err := os.Stat(filepath.Join(path, map[string]string{
			"retained owner profile":   "retained-session",
			"retained owner artifacts": "retained.txt",
			"unmarked profile":         "keep.txt",
			"unmanaged profile":        "keep.txt",
			"unmanaged artifacts":      "keep.txt",
		}[label])); err != nil {
			t.Fatalf("%s was changed: %v", label, err)
		}
	}
}
