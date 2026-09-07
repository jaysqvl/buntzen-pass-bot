package store

import (
	"context"
	"strings"
	"testing"
)

func TestProfileExecutableRejectedOnWritesAndLegacyReadValidation(t *testing.T) {
	database := ownedTestStore(t)
	ctx := context.Background()
	profile, _ := fixtureProfileAndBooking(t, database, "browser-boundary")
	input := ProfileInput{Name: profile.Name, DefaultVehicle: profile.DefaultVehicle, LoginProbeURL: profile.LoginProbeURL,
		OTPSourceID: profile.OTPSourceID, DefaultTimeoutMS: profile.DefaultTimeoutMS, BrowserExecutable: "/tmp/member-program"}
	for _, write := range []func() error{
		func() error { _, err := database.CreateProfile(ctx, testUserID, input); return err },
		func() error { _, err := database.UpdateProfile(ctx, testUserID, profile.ID, input); return err },
	} {
		if err := write(); err == nil || !strings.Contains(err.Error(), "operator-controlled") {
			t.Fatalf("profile write executable rejection = %v", err)
		}
	}
	if _, err := database.db.ExecContext(ctx, "UPDATE profiles SET browser_executable = ? WHERE id = ?", "/tmp/legacy-program", profile.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := database.SystemGetProfile(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.ValidateForOrigins([]string{"https://example.test"}); err == nil || !strings.Contains(err.Error(), "operator-controlled") {
		t.Fatalf("legacy execution validation = %v", err)
	}
	input.BrowserExecutable = ""
	input.BrowserChannel = "chrome"
	if _, err := database.UpdateProfile(ctx, testUserID, profile.ID, input); err != nil {
		t.Fatalf("clearing legacy override rejected: %v", err)
	}
}
