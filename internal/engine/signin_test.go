package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestHomeSignInSnapshotsDefaultOTPAndRejectsDuplicate(t *testing.T) {
	for _, provider := range []model.OTPProvider{model.OTPProviderBlueBubbles, model.OTPProviderTwilio} {
		t.Run(string(provider), func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			ctx := context.Background()
			var source model.OTPSource
			var err error
			if provider == model.OTPProviderBlueBubbles {
				source = createPairingTestSource(t, fixture.resources, "http://127.0.0.1:2234")
			} else {
				source, err = fixture.resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: "New default inbox", Provider: provider, Identity: "twilio:shared-sign-in", ProviderConfig: map[string]string{"auth_token": "synthetic-secret"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.resources.SetDefaultOTPSource(ctx, source.ID); err != nil {
				t.Fatal(err)
			}
			job, err := fixture.engine.QueueProfileSignIn(ctx, fixture.user.ID, fixture.booking.ProfileID)
			if err != nil {
				t.Fatal(err)
			}
			if job.BookingRequestID != nil || job.ProfileID != fixture.booking.ProfileID || job.OTPSourceID != source.ID || job.Command != model.CommandAuthCheck || job.RunMode != model.RunModeManual {
				t.Fatalf("Home sign-in lost identity or selected source: %+v", job)
			}
			if strings.HasPrefix(job.DedupKey, "pairing:") != (provider == model.OTPProviderBlueBubbles) {
				t.Fatalf("wrong OTP pairing mode: %+v", job)
			}
			if _, err := fixture.engine.QueueProfileSignIn(ctx, fixture.user.ID, fixture.booking.ProfileID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate Home sign-in was not rejected: %v", err)
			}
			profile, err := fixture.resources.GetProfile(ctx, fixture.booking.ProfileID)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.resources.SetDefaultOTPSource(ctx, profile.OTPSourceID); err != nil {
				t.Fatal(err)
			}
			saved, err := fixture.resources.GetJob(ctx, job.ID)
			if err != nil || saved.OTPSourceID != source.ID {
				t.Fatalf("later default changed queued sign-in: %+v err=%v", saved, err)
			}
			if _, err := fixture.engine.QueueProfileSignIn(ctx, fixture.user.ID+100, profile.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("foreign identity became accessible: %v", err)
			}
		})
	}
}

func TestGlobalPairingRequiresChoiceWhenSignInsAreAmbiguous(t *testing.T) {
	fixture := newEngineTestFixture(t)
	ctx := context.Background()
	oldProfile, err := fixture.resources.GetProfile(ctx, fixture.booking.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.resources.CreateProfile(ctx, store.ProfileInput{Name: "Second Yodel sign-in", ProviderID: "yodel", LoginProbeURL: "https://example.test/login", OTPSourceID: oldProfile.OTPSourceID, Headless: true, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
	if err != nil {
		t.Fatal(err)
	}
	if second.DefaultVehicle != "" {
		t.Fatal("new global sign-in unexpectedly required a vehicle")
	}
	source := createPairingTestSource(t, fixture.resources, "http://127.0.0.1:2234")
	if _, err := fixture.engine.CheckPairingSetup(ctx, fixture.user.ID, source.ID); !errors.Is(err, ErrPairingProfileAmbiguous) {
		t.Fatalf("ambiguous identities were silently selected: %v", err)
	}
	setup, err := fixture.engine.CheckPairingSetup(ctx, fixture.user.ID, source.ID, second.ID)
	if err != nil || setup.ProfileID != second.ID {
		t.Fatalf("explicit global sign-in was not accepted: %+v err=%v", setup, err)
	}
	job, err := fixture.engine.QueuePairing(ctx, fixture.user.ID, source.ID, second.ID)
	if err != nil || job.ProfileID != second.ID || job.OTPSourceID != source.ID {
		t.Fatalf("pairing lost explicit sign-in/source: %+v err=%v", job, err)
	}
	if _, err := fixture.engine.CheckPairingSetup(ctx, fixture.user.ID, source.ID, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unowned selected sign-in was not rejected: %v", err)
	}
}
