package model

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
)

const MaxReleaseDaysBefore = 365

// LakeSettings are one account's defaults. ApplyTo copies them into a booking;
// changing these defaults never changes an existing request or queued job.
type LakeSettings struct {
	UserID                    int64
	LakeID                    string
	Timezone                  string
	ReleaseTime               string
	ReleaseDaysBefore         int
	AllDayPassURL             string
	HalfDayPassURL            string
	PreferredPasses           []PassType
	PrepMinutesBefore         int
	AuthDeadlineMinutesBefore int
	PollDeadlineSeconds       int
	PollMinSeconds            float64
	PollMaxSeconds            float64
	UpdatedAt                 time.Time
}

func DefaultLakeSettings(lake destinations.Lake) LakeSettings {
	settings := LakeSettings{
		LakeID: lake.ID, Timezone: lake.Timezone, ReleaseTime: lake.ReleaseTime,
		ReleaseDaysBefore: lake.ReleaseDaysBefore,
		AllDayPassURL:     lake.AllDayPassURL, HalfDayPassURL: lake.HalfDayPassURL,
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5,
		PollDeadlineSeconds: 120, PollMinSeconds: 1.4, PollMaxSeconds: 3.6,
	}
	for _, pass := range lake.SupportedPasses {
		settings.PreferredPasses = append(settings.PreferredPasses, PassType(pass))
	}
	return settings
}

func (s LakeSettings) ApplyTo(request BookingRequest) BookingRequest {
	days := s.ReleaseDaysBefore
	request.LakeID = s.LakeID
	request.Timezone = s.Timezone
	request.ReleaseTime = s.ReleaseTime
	request.ReleaseDaysBefore = &days
	request.AllDayPassURL = s.AllDayPassURL
	request.HalfDayPassURL = s.HalfDayPassURL
	request.PreferredPasses = slices.Clone(s.PreferredPasses)
	request.PrepMinutesBefore = s.PrepMinutesBefore
	request.AuthDeadlineMinutesBefore = s.AuthDeadlineMinutesBefore
	request.PollDeadlineSeconds = s.PollDeadlineSeconds
	request.PollMinSeconds = s.PollMinSeconds
	request.PollMaxSeconds = s.PollMaxSeconds
	return request
}

func (s LakeSettings) validationRequest() BookingRequest {
	// Reuse the same destination, timing, pass-order and URL validation as
	// bookings, supplying only the unrelated per-request required fields.
	return s.ApplyTo(BookingRequest{
		Name: "Lake defaults", ProfileID: 1, TargetDate: "2000-01-01",
		ConfirmationMode: RunModeManual,
	})
}

func (s LakeSettings) Validate() error {
	return s.validationRequest().Validate()
}

func (s LakeSettings) ValidateForOrigins(allowedOrigins []string) error {
	return s.validationRequest().ValidateForOrigins(allowedOrigins)
}

// AccountSettings are browser defaults copied into newly created profiles.
// Profiles retain their own values after these defaults change.
type AccountSettings struct {
	UserID           int64
	Headless         bool
	BrowserChannel   string
	DefaultTimeoutMS int
	UpdatedAt        time.Time
}

func DefaultAccountSettings() AccountSettings {
	return AccountSettings{Headless: true, DefaultTimeoutMS: 15_000}
}

func (s AccountSettings) Validate() error {
	if len(s.BrowserChannel) > MaxBrowserChannelBytes {
		return errors.New("browser selection is too long")
	}
	switch strings.ToLower(strings.TrimSpace(s.BrowserChannel)) {
	case "", "chrome", "chrome-beta", "chrome-dev", "chrome-canary":
	default:
		return errors.New("choose bundled Chromium or a supported Chrome channel")
	}
	if s.DefaultTimeoutMS < 1_000 || s.DefaultTimeoutMS > 120_000 {
		return errors.New("default timeout must be between 1000 and 120000 milliseconds")
	}
	return nil
}
