package model

import (
	"math"
	"slices"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
)

func TestLakeSettingsCopyOnlyDefaultsIntoBooking(t *testing.T) {
	lake, _ := destinations.Resolve("")
	settings := DefaultLakeSettings(lake)
	settings.ReleaseDaysBefore = 0
	settings.PreferredPasses = []PassType{PassAfternoon, PassAllDay}
	request := BookingRequest{
		ID: 4, UserID: 5, Name: "A visit", ProfileID: 6, TargetDate: "2030-06-20",
		Enabled: true, ScheduleEnabled: true, ConfirmationMode: RunModeAuto,
		PrepMinutesBefore: 45, AuthDeadlineMinutesBefore: 10,
		PollDeadlineSeconds: 300, PollMinSeconds: 2.5, PollMaxSeconds: 5,
	}
	updated := settings.ApplyTo(request)
	if updated.ID != request.ID || updated.UserID != request.UserID || updated.Name != request.Name ||
		updated.ProfileID != request.ProfileID || updated.TargetDate != request.TargetDate ||
		updated.Enabled != request.Enabled || updated.ScheduleEnabled != request.ScheduleEnabled ||
		updated.ConfirmationMode != request.ConfirmationMode ||
		updated.PrepMinutesBefore != request.PrepMinutesBefore ||
		updated.AuthDeadlineMinutesBefore != request.AuthDeadlineMinutesBefore ||
		updated.PollDeadlineSeconds != request.PollDeadlineSeconds ||
		updated.PollMinSeconds != request.PollMinSeconds || updated.PollMaxSeconds != request.PollMaxSeconds {
		t.Fatalf("applying defaults changed request-specific fields: %+v", updated)
	}
	if updated.ReleaseDaysBefore == nil || updated.EffectiveReleaseDaysBefore() != 0 ||
		!slices.Equal(updated.PassOrder(), settings.PreferredPasses) {
		t.Fatalf("defaults were not copied: %+v", updated)
	}
	settings.ReleaseDaysBefore = 7
	settings.PreferredPasses[0] = PassMorning
	if updated.EffectiveReleaseDaysBefore() != 0 || updated.PassOrder()[0] != PassAfternoon {
		t.Fatal("booking snapshot shares mutable settings state")
	}
}

func TestLakeSettingsValidationMatchesBookingBounds(t *testing.T) {
	lake, _ := destinations.Resolve("")
	for name, change := range map[string]func(*LakeSettings){
		"unknown lake":     func(s *LakeSettings) { s.LakeID = "unknown" },
		"negative days":    func(s *LakeSettings) { s.ReleaseDaysBefore = -1 },
		"excessive days":   func(s *LakeSettings) { s.ReleaseDaysBefore = MaxReleaseDaysBefore + 1 },
		"unknown timezone": func(s *LakeSettings) { s.Timezone = "No/SuchPlace" },
		"invalid time":     func(s *LakeSettings) { s.ReleaseTime = "25:00" },
		"empty order":      func(s *LakeSettings) { s.PreferredPasses = nil },
		"duplicate passes": func(s *LakeSettings) { s.PreferredPasses = []PassType{PassAllDay, PassAllDay} },
		"unsupported pass": func(s *LakeSettings) { s.PreferredPasses = []PassType{"camping"} },
	} {
		t.Run(name, func(t *testing.T) {
			settings := DefaultLakeSettings(lake)
			change(&settings)
			if err := settings.Validate(); err == nil {
				t.Fatal("invalid lake settings were accepted")
			}
		})
	}
	settings := DefaultLakeSettings(lake)
	settings.ReleaseDaysBefore = 0
	if err := settings.ValidateForOrigins([]string{"https://yodelportal.com"}); err != nil {
		t.Fatalf("valid same-day defaults: %v", err)
	}
	settings.AllDayPassURL = "https://unapproved.example/pass"
	if err := settings.ValidateForOrigins([]string{"https://yodelportal.com"}); err == nil {
		t.Fatal("unapproved credential origin was accepted")
	}
}

func TestAccountSettingsValidateBrowserPolicy(t *testing.T) {
	settings := DefaultAccountSettings()
	if !settings.Headless || settings.BrowserChannel != "" || settings.DefaultTimeoutMS != 15000 {
		t.Fatalf("unexpected account defaults: %+v", settings)
	}
	for _, channel := range []string{"", "chrome", "chrome-beta", "chrome-dev", "chrome-canary"} {
		settings.BrowserChannel = channel
		if err := settings.Validate(); err != nil {
			t.Fatalf("supported channel %q: %v", channel, err)
		}
	}
	settings.BrowserChannel = "/tmp/custom-browser"
	if err := settings.Validate(); err == nil {
		t.Fatal("account defaults accepted a browser executable")
	}
	settings = DefaultAccountSettings()
	settings.DefaultTimeoutMS = 999
	if err := settings.Validate(); err == nil {
		t.Fatal("account defaults accepted an unbounded timeout")
	}
}

func TestAccountSettingsValidateGlobalTimingPolicy(t *testing.T) {
	for name, change := range map[string]func(*AccountSettings){
		"negative prep":     func(s *AccountSettings) { s.PrepMinutesBefore = -1 },
		"unbounded prep":    func(s *AccountSettings) { s.PrepMinutesBefore = MaxPrepMinutesBefore + 1 },
		"negative auth":     func(s *AccountSettings) { s.AuthDeadlineMinutesBefore = -1 },
		"auth outside prep": func(s *AccountSettings) { s.AuthDeadlineMinutesBefore = s.PrepMinutesBefore + 1 },
		"empty window":      func(s *AccountSettings) { s.PollDeadlineSeconds = 0 },
		"unbounded window":  func(s *AccountSettings) { s.PollDeadlineSeconds = 901 },
		"nan min":           func(s *AccountSettings) { s.PollMinSeconds = math.NaN() },
		"nan max":           func(s *AccountSettings) { s.PollMaxSeconds = math.NaN() },
		"infinite min":      func(s *AccountSettings) { s.PollMinSeconds = math.Inf(-1) },
		"infinite max":      func(s *AccountSettings) { s.PollMaxSeconds = math.Inf(1) },
		"reversed poll":     func(s *AccountSettings) { s.PollMaxSeconds = s.PollMinSeconds - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			settings := DefaultAccountSettings()
			change(&settings)
			if err := settings.Validate(); err == nil {
				t.Fatal("invalid account timing was accepted")
			}
		})
	}
	settings := DefaultAccountSettings()
	settings.PrepMinutesBefore, settings.AuthDeadlineMinutesBefore = 0, 0
	if err := settings.Validate(); err != nil {
		t.Fatalf("explicit zero preparation offsets: %v", err)
	}
}

func TestAccountSettingsCopyTimingWithoutChangingLakeOrBookingChoices(t *testing.T) {
	lake, _ := destinations.Resolve("")
	request := DefaultLakeSettings(lake).ApplyTo(BookingRequest{
		Name: "Existing visit", ProfileID: 5, TargetDate: "2030-06-20",
		ConfirmationMode: RunModeAuto, Enabled: true, ScheduleEnabled: true,
	})
	settings := DefaultAccountSettings()
	updated := settings.ApplyToBooking(request)
	if updated.PrepMinutesBefore != 30 || updated.AuthDeadlineMinutesBefore != 5 ||
		updated.PollDeadlineSeconds != 120 || updated.PollMinSeconds != 1.4 || updated.PollMaxSeconds != 3.6 {
		t.Fatalf("global timing defaults were not copied: %+v", updated)
	}
	if updated.LakeID != request.LakeID || updated.Timezone != request.Timezone ||
		updated.ReleaseTime != request.ReleaseTime || updated.EffectiveReleaseDaysBefore() != request.EffectiveReleaseDaysBefore() ||
		updated.AllDayPassURL != request.AllDayPassURL || updated.HalfDayPassURL != request.HalfDayPassURL ||
		!slices.Equal(updated.PreferredPasses, request.PreferredPasses) || updated.Name != request.Name ||
		updated.ProfileID != request.ProfileID || updated.TargetDate != request.TargetDate ||
		updated.ConfirmationMode != request.ConfirmationMode || updated.Enabled != request.Enabled ||
		updated.ScheduleEnabled != request.ScheduleEnabled {
		t.Fatalf("global defaults changed lake or request choices: %+v", updated)
	}
	settings.PrepMinutesBefore = 90
	if updated.PrepMinutesBefore != 30 {
		t.Fatal("copied request timing changed when defaults changed")
	}
}
