package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestDefaultOTPOwnershipAndNewJobsSnapshotChoice(t *testing.T) {
	ctx := context.Background()
	database, ownerID, otherID := ownershipStore(t)
	owner, other := database.ForUser(ownerID), database.ForUser(otherID)
	if _, err := owner.GetDefaultOTPSource(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty default: %v", err)
	}
	source, profile, _ := createOwnedResources(t, database, ownerID, "default-owner")
	otherSource, _, _ := createOwnedResources(t, database, otherID, "default-other")
	if got, err := owner.GetDefaultOTPSource(ctx); err != nil || got.ID != source.ID {
		t.Fatalf("first source not default: %+v %v", got, err)
	}
	if err := owner.SetDefaultOTPSource(ctx, otherSource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner default: %v", err)
	}
	if err := owner.SetDefaultOTPSource(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing default: %v", err)
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE user_otp_preferences SET source_id = ? WHERE user_id = ?", otherSource.ID, ownerID); err == nil {
		t.Fatal("SQL allowed cross-owner default")
	}
	second, err := owner.CreateOTPSource(ctx, OTPSourceInput{Name: "Second inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:second-default", ProviderConfig: map[string]string{"token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	firstJob, err := owner.EnqueueJob(ctx, EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck, DueAt: now})
	if err != nil || firstJob.OTPSourceID != source.ID {
		t.Fatalf("initial job source: %+v %v", firstJob, err)
	}
	if err := owner.SetDefaultOTPSource(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	secondJob, err := owner.EnqueueJob(ctx, EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck, DueAt: now})
	if err != nil || secondJob.OTPSourceID != second.ID {
		t.Fatalf("new default not snapshotted: %+v %v", secondJob, err)
	}
	retained, err := owner.GetJob(ctx, firstJob.ID)
	if err != nil || !reflect.DeepEqual(retained, firstJob) {
		t.Fatalf("default change altered old job: %+v %v", retained, err)
	}
	if _, err := owner.EnqueueJob(ctx, EnqueueJobParams{ProfileID: profile.ID, OTPSourceID: otherSource.ID, Command: model.CommandAuthCheck}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner override: %v", err)
	}
	if _, err := owner.EnqueueJob(ctx, EnqueueJobParams{ProfileID: profile.ID, OTPSourceID: second.ID, Command: model.CommandBook}); err == nil {
		t.Fatal("booking accepted explicit source override")
	}
	if err := owner.RequestJobCancellation(ctx, firstJob.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := database.SystemClaimNextDueJobAt(ctx, "shared-identity", now.Add(time.Second))
	if err != nil || claimed.ID != secondJob.ID || claimed.OTPSourceID != second.ID {
		t.Fatalf("claim rejected source differing from legacy profile: %+v %v", claimed, err)
	}
	if _, err := other.GetDefaultOTPSource(ctx); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { _, err := database.ForUser(0).GetDefaultOTPSource(ctx); return err },
		func() error { return database.ForUser(0).SetDefaultOTPSource(ctx, source.ID) },
	} {
		if err := operation(); !errors.Is(err, ErrUserRequired) {
			t.Fatalf("invalid owner: %v", err)
		}
	}
}

func TestGlobalSignInsShareSourceWhileJobsRetainResourceLocks(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	owner := database.ForUser(testUserID)
	profile, _ := fixtureProfileAndBooking(t, database, "shared-signin")
	second, err := owner.CreateProfile(ctx, ProfileInput{Name: "Second global sign-in", ProviderID: "yodel", LoginProbeURL: profile.LoginProbeURL, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Phone: "5559876544"}})
	if err != nil || second.OTPSourceID != profile.OTPSourceID || second.DefaultVehicle != "" {
		t.Fatalf("global identity sharing source: %+v %v", second, err)
	}
	now := time.Now().UTC()
	firstJob, err := owner.EnqueueJob(ctx, EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck, DueAt: now})
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := owner.EnqueueJob(ctx, EnqueueJobParams{ProfileID: second.ID, Command: model.CommandAuthCheck, DueAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if job, err := database.SystemClaimNextDueJobAt(ctx, "one", now.Add(time.Second)); err != nil || job.ID != firstJob.ID {
		t.Fatalf("first claim: %+v %v", job, err)
	}
	if _, err := database.SystemClaimNextDueJobAt(ctx, "two", now.Add(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared inbox used concurrently: %v", err)
	}
	if _, err := database.SystemTransitionJob(ctx, firstJob.ID, []model.JobStatus{model.JobRunning}, model.JobFailed, JobTransition{}); err != nil {
		t.Fatal(err)
	}
	if job, err := database.SystemClaimNextDueJobAt(ctx, "two", now.Add(time.Second)); err != nil || job.ID != secondJob.ID {
		t.Fatalf("released inbox could not be claimed: %+v %v", job, err)
	}
}

func TestProfileSignInAdmissionDeduplicatesConcurrentRequests(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	profile, _ := fixtureProfileAndBooking(t, database, "signin-dedupe")
	var wg sync.WaitGroup
	errorsFound := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := database.ForUser(testUserID).EnqueueJob(ctx, EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck, DeduplicateProfileSignIn: true})
			errorsFound <- err
		}()
	}
	wg.Wait()
	close(errorsFound)
	created, conflicts := 0, 0
	for err := range errorsFound {
		if err == nil {
			created++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("concurrent admission: created=%d conflicts=%d", created, conflicts)
	}
}

func TestLakeVehicleIsCopiedIntoBookingsAndLegacyUpdatesPreserveSnapshot(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	owner := database.ForUser(testUserID)
	profile, legacy := fixtureProfileAndBooking(t, database, "vehicle-snapshot")
	if legacy.VehicleKeyword != profile.DefaultVehicle {
		t.Fatalf("legacy vehicle not snapshotted: %+v", legacy)
	}
	lake, _ := destinations.Resolve("")
	settings := model.DefaultLakeSettings(lake)
	settings.VehicleKeyword = "Lake vehicle"
	settings, err := owner.SaveLakeSettings(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	request := settings.ApplyTo(legacy)
	request.ID = 0
	request.Name = "Vehicle choice"
	booking, err := owner.CreateBookingRequest(ctx, request)
	if err != nil || booking.VehicleKeyword != "Lake vehicle" {
		t.Fatalf("lake vehicle snapshot: %+v %v", booking, err)
	}
	settings.VehicleKeyword = "Different car"
	if _, err := owner.SaveLakeSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	booking.VehicleKeyword = ""
	updated, err := owner.UpdateBookingRequest(ctx, booking)
	if err != nil || updated.VehicleKeyword != "Lake vehicle" {
		t.Fatalf("legacy edit rewrote snapshot: %+v %v", updated, err)
	}
	if err := owner.ResetLakeSettings(ctx, lake.ID); err != nil {
		t.Fatal(err)
	}
	got, err := owner.GetBookingRequest(ctx, updated.ID)
	if err != nil || got.VehicleKeyword != "Lake vehicle" {
		t.Fatalf("reset changed snapshot: %+v %v", got, err)
	}
}
