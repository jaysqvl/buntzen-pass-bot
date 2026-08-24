package model

import "testing"

func TestRuntimeBoundsMatchPythonWorker(t *testing.T) {
	profile := Profile{
		Name: "Example", BrowserProfile: "example", DefaultVehicle: "Example Vehicle",
		OTPSourceID: 1, DefaultTimeoutMS: 999,
	}
	if err := ValidateProfile(profile); err == nil {
		t.Fatal("sub-second browser timeout was accepted")
	}
	profile.DefaultTimeoutMS = 120_001
	if err := ValidateProfile(profile); err == nil {
		t.Fatal("browser timeout above worker maximum was accepted")
	}

	booking := BookingRequest{
		Name: "Tomorrow", ProfileID: 1, TargetDate: "2030-01-15",
		Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5,
		PollDeadlineSeconds: 901, PollMinSeconds: 1, PollMaxSeconds: 2,
		ConfirmationMode: RunModeManual, LoginProbeURL: "https://example.test/login",
		AllDayPassURL: "https://example.test/all", CheckAllDay: true,
	}
	if err := booking.Validate(); err == nil {
		t.Fatal("poll deadline above worker maximum was accepted")
	}
	booking.PollDeadlineSeconds = 120
	booking.PollMaxSeconds = 60.1
	if err := booking.Validate(); err == nil {
		t.Fatal("poll interval above worker maximum was accepted")
	}
}
