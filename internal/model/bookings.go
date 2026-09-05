package model

import (
	"errors"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/origin"
)

type PassType string

const (
	PassAllDay    PassType = "all_day"
	PassAfternoon PassType = "afternoon"
	PassMorning   PassType = "morning"
)

type BookingRequest struct {
	ID                        int64
	UserID                    int64
	Name                      string
	ProfileID                 int64
	Enabled                   bool
	ScheduleEnabled           bool
	TargetDate                string
	Timezone                  string
	ReleaseTime               string
	PrepMinutesBefore         int
	AuthDeadlineMinutesBefore int
	PollDeadlineSeconds       int
	PollMinSeconds            float64
	PollMaxSeconds            float64
	ConfirmationMode          RunMode
	// LoginProbeURL retains the legacy database value for migration/history.
	// Authentication uses Profile.LoginProbeURL exclusively.
	LoginProbeURL   string
	AllDayPassURL   string
	HalfDayPassURL  string
	PreferredPasses []PassType
	// Legacy input flags are used only when PreferredPasses is nil.
	CheckAllDay    bool
	CheckAfternoon bool
	CheckMorning   bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (r BookingRequest) PassOrder() []PassType {
	if r.PreferredPasses != nil {
		return slices.Clone(r.PreferredPasses)
	}
	result := make([]PassType, 0, 3)
	if r.CheckAllDay {
		result = append(result, PassAllDay)
	}
	if r.CheckAfternoon {
		result = append(result, PassAfternoon)
	}
	if r.CheckMorning {
		result = append(result, PassMorning)
	}
	return result
}

func (r BookingRequest) Validate() error {
	var problems []string
	if strings.TrimSpace(r.Name) == "" {
		problems = append(problems, "name is required")
	} else if len(r.Name) > MaxResourceNameBytes {
		problems = append(problems, "name is too long")
	}
	if r.ProfileID <= 0 {
		problems = append(problems, "profile is required")
	}
	if _, err := time.Parse(time.DateOnly, r.TargetDate); err != nil {
		problems = append(problems, "target date must use YYYY-MM-DD")
	}
	if len(r.Timezone) > MaxTimezoneBytes {
		problems = append(problems, "timezone is too long")
	} else if _, err := time.LoadLocation(r.Timezone); err != nil {
		problems = append(problems, "timezone is invalid")
	}
	if _, err := time.Parse("15:04", r.ReleaseTime); err != nil {
		problems = append(problems, "release time must use HH:MM")
	}
	if r.PrepMinutesBefore < 0 || r.AuthDeadlineMinutesBefore < 0 {
		problems = append(problems, "preparation offsets cannot be negative")
	} else if r.PrepMinutesBefore > MaxPrepMinutesBefore {
		problems = append(problems, "preparation window cannot exceed 180 minutes")
	}
	if r.AuthDeadlineMinutesBefore > r.PrepMinutesBefore {
		problems = append(problems, "auth deadline must fall within the preparation window")
	}
	if r.PollDeadlineSeconds <= 0 || r.PollDeadlineSeconds > 900 ||
		r.PollMinSeconds < 0.05 || r.PollMinSeconds > 60 ||
		r.PollMaxSeconds < r.PollMinSeconds || r.PollMaxSeconds > 60 ||
		math.IsNaN(r.PollMinSeconds) || math.IsNaN(r.PollMaxSeconds) ||
		math.IsInf(r.PollMinSeconds, 0) || math.IsInf(r.PollMaxSeconds, 0) {
		problems = append(problems, "poll timing must fit the worker bounds")
	}
	if !r.ConfirmationMode.Valid() || r.ConfirmationMode == RunModeDryRun {
		problems = append(problems, "confirmation mode must be manual or auto")
	}
	passes := r.PassOrder()
	if len(passes) == 0 {
		problems = append(problems, "at least one pass preference is required")
	} else if len(passes) > 3 {
		problems = append(problems, "at most three pass preferences are allowed")
	}
	seen := make(map[PassType]bool, len(passes))
	for _, pass := range passes {
		if pass != PassAllDay && pass != PassAfternoon && pass != PassMorning {
			problems = append(problems, "pass preferences must be all-day, afternoon, or morning")
		} else if seen[pass] {
			problems = append(problems, "each pass preference can only be selected once")
		}
		seen[pass] = true
	}
	if seen[PassAllDay] {
		if err := validateHTTPURL(r.AllDayPassURL, "all-day pass URL"); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if seen[PassAfternoon] || seen[PassMorning] {
		if err := validateHTTPURL(r.HalfDayPassURL, "half-day pass URL"); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ValidateForOrigins applies the operator-controlled credential boundary on
// top of the persistence-level booking validation. Booking records may choose
// paths on an approved Yodel site, but they cannot choose which origin receives
// credentials or OTPs.
func (r BookingRequest) ValidateForOrigins(allowedOrigins []string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return validateYodelURLs(allowedOrigins,
		yodelURL{r.AllDayPassURL, "all-day pass URL"},
		yodelURL{r.HalfDayPassURL, "half-day pass URL"},
	)
}

type yodelURL struct{ value, label string }

func validateYodelURLs(allowedOrigins []string, checks ...yodelURL) error {
	approved := make(map[string]struct{}, len(allowedOrigins))
	for _, value := range allowedOrigins {
		canonical, err := origin.Canonical(value)
		if err != nil || !strings.HasPrefix(canonical, "https://") {
			return errors.New("configured Yodel origin is invalid")
		}
		approved[canonical] = struct{}{}
	}
	if len(approved) == 0 {
		return errors.New("no approved Yodel origin is configured")
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			continue
		}
		canonical, err := origin.FromURL(check.value)
		if err != nil || !strings.HasPrefix(canonical, "https://") {
			return errors.New(check.label + " must be an absolute HTTPS URL")
		}
		if _, ok := approved[canonical]; !ok {
			return errors.New(check.label + " must use an approved Yodel origin")
		}
	}
	return nil
}

func validateHTTPURL(value, label string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New(label + " is required")
	}
	if len(value) > 2048 {
		return errors.New(label + " is too long")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New(label + " must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return errors.New(label + " must not contain credentials")
	}
	return nil
}
