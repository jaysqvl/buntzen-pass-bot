package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestDurableResourceCountsAreBoundedAcrossDatabaseHandles(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "resource-limits.db")
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
	user, err := database.SetupAdmin(ctx, "limits-admin", "a strong resource limit password")
	if err != nil {
		t.Fatal(err)
	}

	const sourceAttempts = MaxOTPSourcesPerUser * 3
	start := make(chan struct{})
	results := make(chan error, sourceAttempts)
	for attempt := range sourceAttempts {
		go func() {
			<-start
			selected := database
			if attempt%2 == 1 {
				selected = peer
			}
			_, err := selected.ForUser(user.ID).CreateOTPSource(ctx, OTPSourceInput{
				Name: fmt.Sprintf("source-%03d", attempt), Provider: model.OTPProviderTwilio,
				Identity:       fmt.Sprintf("twilio:resource-limit-%03d", attempt),
				ProviderConfig: map[string]string{"auth_token": "synthetic-token"},
			})
			results <- err
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for range sourceAttempts {
		switch err := <-results; {
		case err == nil:
			accepted++
		case errors.Is(err, ErrResourceLimit):
			rejected++
		default:
			t.Fatalf("unexpected source creation error: %v", err)
		}
	}
	if accepted != MaxOTPSourcesPerUser || rejected != sourceAttempts-MaxOTPSourcesPerUser {
		t.Fatalf("sources accepted=%d rejected=%d", accepted, rejected)
	}
	sources, err := database.ForUser(user.ID).ListOTPSources(ctx)
	if err != nil || len(sources) != MaxOTPSourcesPerUser {
		t.Fatalf("sources=%d err=%v", len(sources), err)
	}
	if err := database.ForUser(user.ID).DeleteOTPSource(ctx, sources[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ForUser(user.ID).CreateOTPSource(ctx, OTPSourceInput{
		Name: "replacement-source", Provider: model.OTPProviderTwilio,
		Identity:       "twilio:resource-limit-replacement",
		ProviderConfig: map[string]string{"auth_token": "synthetic-token"},
	}); err != nil {
		t.Fatalf("source slot was not reusable: %v", err)
	}
	sources, err = database.ForUser(user.ID).ListOTPSources(ctx)
	if err != nil || len(sources) != MaxOTPSourcesPerUser {
		t.Fatalf("sources after replacement=%d err=%v", len(sources), err)
	}

	profiles := make([]model.Profile, 0, MaxProfilesPerUser)
	for index := 0; index < MaxProfilesPerUser; index++ {
		profile, err := database.ForUser(user.ID).CreateProfile(ctx, ProfileInput{
			Name:           fmt.Sprintf("profile-%02d", index),
			DefaultVehicle: "Example Vehicle", OTPSourceID: sources[index].ID,
			Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
			Credentials: &model.ProfileCredentials{Email: fmt.Sprintf("profile-%02d@example.test", index), Password: "synthetic-password"},
		})
		if err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, profile)
	}
	if _, err := peer.ForUser(user.ID).CreateProfile(ctx, ProfileInput{
		Name:           "profile-over-limit",
		DefaultVehicle: "Example Vehicle", OTPSourceID: sources[MaxProfilesPerUser].ID,
		Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "over-limit@example.test", Password: "synthetic-password"},
	}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("profile limit error=%v", err)
	}

	bookingInput := func(name string) model.BookingRequest {
		return model.BookingRequest{
			Name: name, ProfileID: profiles[0].ID, Enabled: true,
			TargetDate: "2031-01-15", Timezone: "UTC", ReleaseTime: "07:00",
			PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
			PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
			LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all-day",
			CheckAllDay: true,
		}
	}
	var firstBooking model.BookingRequest
	for index := 0; index < MaxBookingRequestsPerUser; index++ {
		booking, err := database.ForUser(user.ID).CreateBookingRequest(ctx, bookingInput(fmt.Sprintf("booking-%02d", index)))
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstBooking = booking
		}
	}
	if _, err := peer.ForUser(user.ID).CreateBookingRequest(ctx, bookingInput("booking-over-limit")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("booking limit error=%v", err)
	}
	if err := database.ForUser(user.ID).DeleteBookingRequest(ctx, firstBooking.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.ForUser(user.ID).CreateBookingRequest(ctx, bookingInput("booking-replacement")); err != nil {
		t.Fatalf("booking slot was not reusable: %v", err)
	}
}

func TestResourceFieldsAndEncryptedConfigurationAreBounded(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	resources := database.ForUser(testUserID)
	if _, err := resources.CreateOTPSource(ctx, OTPSourceInput{
		Name: "oversized config", Provider: model.OTPProviderTwilio,
		Identity:       "twilio:oversized-config",
		ProviderConfig: map[string]string{"auth_token": strings.Repeat("x", MaxProviderConfigJSONBytes)},
	}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized provider configuration error=%v", err)
	}
	var sourceCount int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM otp_sources").Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if sourceCount != 0 {
		t.Fatalf("oversized configuration created %d source rows", sourceCount)
	}

	profile, _ := fixtureProfileAndBooking(t, database, "bounded-fields")
	credentials, err := resources.GetProfileCredentials(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resources.UpdateProfile(ctx, profile.ID, ProfileInput{
		Name: profile.Name, DefaultVehicle: profile.DefaultVehicle,
		OTPSourceID: profile.OTPSourceID, Headless: profile.Headless,
		BrowserChannel: profile.BrowserChannel, BrowserExecutable: profile.BrowserExecutable,
		DefaultTimeoutMS: profile.DefaultTimeoutMS, Enabled: profile.Enabled,
		Credentials: &model.ProfileCredentials{Email: credentials.Email, Password: strings.Repeat("p", MaxYodelPasswordBytes+1)},
	})
	if err == nil || !strings.Contains(err.Error(), "password is too long") {
		t.Fatalf("oversized Yodel password error=%v", err)
	}
	unchanged, err := resources.GetProfileCredentials(ctx, profile.ID)
	if err != nil || unchanged != credentials {
		t.Fatalf("credentials changed after rejected update: before=%+v after=%+v err=%v", credentials, unchanged, err)
	}
}

func TestSessionsRotateAtPerUserLimitAndExpiredRowsPurge(t *testing.T) {
	ctx := context.Background()
	database := ownedTestStore(t)
	tokens := make([]string, 0, MaxSessionsPerUser+4)
	for range MaxSessionsPerUser + 4 {
		credentials, err := database.NewSession(ctx, testUserID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, credentials.Token)
		time.Sleep(time.Microsecond)
	}
	var sessions int
	if err := database.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE user_id = ?", testUserID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != MaxSessionsPerUser {
		t.Fatalf("sessions=%d want=%d", sessions, MaxSessionsPerUser)
	}
	for _, token := range tokens[:len(tokens)-MaxSessionsPerUser] {
		if _, err := database.GetSession(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("evicted session error=%v", err)
		}
	}
	for _, token := range tokens[len(tokens)-MaxSessionsPerUser:] {
		if _, err := database.GetSession(ctx, token); err != nil {
			t.Fatalf("retained session error=%v", err)
		}
	}

	if _, err := database.NewSession(ctx, testUserID, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	purged, err := database.PurgeExpiredSessions(ctx)
	if err != nil || purged != 1 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
}
