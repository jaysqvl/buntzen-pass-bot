package model

import (
	"slices"
	"testing"
)

func TestRuntimeTimingBounds(t *testing.T) {
	profile := Profile{Name: "Example", DefaultVehicle: "Example Vehicle", OTPSourceID: 1, DefaultTimeoutMS: 1000}
	if err := profile.Validate(); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	for _, timeout := range []int{999, 120_001} {
		candidate := profile
		candidate.DefaultTimeoutMS = timeout
		if err := candidate.Validate(); err == nil {
			t.Errorf("accepted browser timeout %d", timeout)
		}
	}
	booking := validBooking()
	if err := booking.Validate(); err != nil {
		t.Fatalf("valid booking: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*BookingRequest)
	}{
		{"poll deadline", func(r *BookingRequest) { r.PollDeadlineSeconds = 901 }},
		{"poll interval maximum", func(r *BookingRequest) { r.PollMaxSeconds = 60.1 }},
		{"poll interval minimum", func(r *BookingRequest) { r.PollMinSeconds = 0.049 }},
		{"preparation window", func(r *BookingRequest) { r.PrepMinutesBefore = MaxPrepMinutesBefore + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := booking
			test.change(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("accepted out-of-bounds timing")
			}
		})
	}
}

func TestPassOrder(t *testing.T) {
	booking := BookingRequest{CheckMorning: true, CheckAfternoon: true, CheckAllDay: true}
	if got := booking.PassOrder(); !slices.Equal(got, []PassType{PassAllDay, PassAfternoon, PassMorning}) {
		t.Fatalf("all enabled passes = %v", got)
	}
	booking.CheckAfternoon = false
	if got := booking.PassOrder(); !slices.Equal(got, []PassType{PassAllDay, PassMorning}) {
		t.Fatalf("enabled pass subset = %v", got)
	}
}

func validBooking() BookingRequest {
	return BookingRequest{
		Name: "Tomorrow", ProfileID: 1, TargetDate: "2030-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5,
		PollDeadlineSeconds: 120, PollMinSeconds: 0.05, PollMaxSeconds: 2,
		ConfirmationMode: RunModeManual, LoginProbeURL: "https://example.test/login",
		AllDayPassURL: "https://example.test:443/all", CheckAllDay: true,
	}
}

func TestBookingYodelOriginBoundary(t *testing.T) {
	booking := validBooking()
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
