package model

import "testing"

func TestRuntimeBoundsMatchPythonWorker(t *testing.T) {
	profile := Profile{
		Name: "Example", DefaultVehicle: "Example Vehicle",
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
	booking.PollMaxSeconds = 2
	booking.PrepMinutesBefore = MaxPrepMinutesBefore + 1
	if err := booking.Validate(); err == nil {
		t.Fatal("unbounded preparation window was accepted")
	}
}

func TestBookingYodelOriginBoundary(t *testing.T) {
	booking := BookingRequest{
		Name: "Tomorrow", ProfileID: 1, TargetDate: "2030-01-15",
		Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5,
		PollDeadlineSeconds: 120, PollMinSeconds: 1, PollMaxSeconds: 2,
		ConfirmationMode: RunModeManual, LoginProbeURL: "https://example.test/login",
		AllDayPassURL: "https://example.test:443/all", CheckAllDay: true,
	}
	if err := booking.ValidateForOrigins([]string{"https://example.test"}); err != nil {
		t.Fatalf("approved Yodel URLs rejected: %v", err)
	}

	tests := []struct {
		name    string
		login   string
		origins []string
	}{
		{"arbitrary HTTPS host", "https://attacker.example/login", []string{"https://example.test"}},
		{"lookalike subdomain", "https://example.test.attacker.example/login", []string{"https://example.test"}},
		{"userinfo confusion", "https://example.test" + "@attacker.example/login", []string{"https://example.test"}},
		{"plaintext HTTP", "http://example.test/login", []string{"https://example.test"}},
		{"empty policy", "https://example.test/login", nil},
		{"invalid policy path", "https://example.test/login", []string{"https://example.test/path"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := booking
			candidate.LoginProbeURL = test.login
			if err := candidate.ValidateForOrigins(test.origins); err == nil {
				t.Fatal("unsafe Yodel origin was accepted")
			}
		})
	}
}
