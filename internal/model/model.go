// Package model contains the persistence-neutral control-plane domain types.
package model

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

type OTPProvider string

const (
	OTPProviderBlueBubbles OTPProvider = "bluebubbles"
	OTPProviderTwilio      OTPProvider = "twilio"
)

func (p OTPProvider) Valid() bool {
	return p == OTPProviderBlueBubbles || p == OTPProviderTwilio
}

type PassType string

const (
	PassAllDay    PassType = "all_day"
	PassAfternoon PassType = "afternoon"
	PassMorning   PassType = "morning"
)

var CanonicalPassOrder = []PassType{PassAllDay, PassAfternoon, PassMorning}

type RunMode string

const (
	RunModeDryRun RunMode = "dry-run"
	RunModeManual RunMode = "manual"
	RunModeAuto   RunMode = "auto"
)

func (m RunMode) Valid() bool {
	return m == RunModeDryRun || m == RunModeManual || m == RunModeAuto
}

type JobCommand string

const (
	CommandAuthCheck JobCommand = "auth-check"
	CommandDryRun    JobCommand = "dry-run"
	CommandBook      JobCommand = "book"
)

func (c JobCommand) Valid() bool {
	return c == CommandAuthCheck || c == CommandDryRun || c == CommandBook
}

type JobStatus string

const (
	JobQueued           JobStatus = "queued"
	JobRunning          JobStatus = "running"
	JobAwaitingApproval JobStatus = "awaiting_approval"
	JobSucceeded        JobStatus = "succeeded"
	JobFailed           JobStatus = "failed"
	JobCancelled        JobStatus = "cancelled"
	JobInterrupted      JobStatus = "interrupted"
	JobOutcomeUnknown   JobStatus = "outcome_unknown"
)

func (s JobStatus) Valid() bool {
	switch s {
	case JobQueued, JobRunning, JobAwaitingApproval, JobSucceeded, JobFailed,
		JobCancelled, JobInterrupted, JobOutcomeUnknown:
		return true
	default:
		return false
	}
}

func (s JobStatus) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled, JobInterrupted, JobOutcomeUnknown:
		return true
	default:
		return false
	}
}

func CanTransition(from, to JobStatus) bool {
	switch from {
	case JobQueued:
		return to == JobRunning || to == JobCancelled
	case JobRunning:
		return to == JobAwaitingApproval || to == JobSucceeded || to == JobFailed ||
			to == JobCancelled || to == JobInterrupted || to == JobOutcomeUnknown
	case JobAwaitingApproval:
		return to == JobRunning || to == JobCancelled || to == JobFailed ||
			to == JobInterrupted || to == JobOutcomeUnknown
	default:
		return false
	}
}

type OTPSource struct {
	ID              int64
	Name            string
	Provider        OTPProvider
	Identity        string
	PairingChatGUID string
	PairingSender   string
	PairingService  string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Profile struct {
	ID                int64
	Name              string
	BrowserProfile    string
	DefaultVehicle    string
	OTPSourceID       int64
	Headless          bool
	BrowserChannel    string
	BrowserExecutable string
	DefaultTimeoutMS  int
	Enabled           bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ProfileCredentials struct {
	Email    string
	Password string
}

type BookingRequest struct {
	ID                        int64
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
	LoginProbeURL             string
	AllDayPassURL             string
	HalfDayPassURL            string
	CheckAllDay               bool
	CheckAfternoon            bool
	CheckMorning              bool
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

func (r BookingRequest) PassOrder() []PassType {
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
	}
	if r.ProfileID <= 0 {
		problems = append(problems, "profile is required")
	}
	if _, err := time.Parse(time.DateOnly, r.TargetDate); err != nil {
		problems = append(problems, "target date must use YYYY-MM-DD")
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		problems = append(problems, "timezone is invalid")
	}
	if _, err := time.Parse("15:04", r.ReleaseTime); err != nil {
		problems = append(problems, "release time must use HH:MM")
	}
	if r.PrepMinutesBefore < 0 || r.AuthDeadlineMinutesBefore < 0 {
		problems = append(problems, "preparation offsets cannot be negative")
	}
	if r.AuthDeadlineMinutesBefore > r.PrepMinutesBefore {
		problems = append(problems, "auth deadline must fall within the preparation window")
	}
	if r.PollDeadlineSeconds <= 0 || r.PollDeadlineSeconds > 900 ||
		r.PollMinSeconds <= 0 || r.PollMinSeconds > 60 ||
		r.PollMaxSeconds < r.PollMinSeconds || r.PollMaxSeconds > 60 ||
		math.IsNaN(r.PollMinSeconds) || math.IsNaN(r.PollMaxSeconds) ||
		math.IsInf(r.PollMinSeconds, 0) || math.IsInf(r.PollMaxSeconds, 0) {
		problems = append(problems, "poll timing must fit the worker bounds")
	}
	if !r.ConfirmationMode.Valid() || r.ConfirmationMode == RunModeDryRun {
		problems = append(problems, "confirmation mode must be manual or auto")
	}
	if len(r.PassOrder()) == 0 {
		problems = append(problems, "at least one pass preference is required")
	}
	if err := validateHTTPURL(r.LoginProbeURL, "login probe URL"); err != nil {
		problems = append(problems, err.Error())
	}
	if r.CheckAllDay {
		if err := validateHTTPURL(r.AllDayPassURL, "all-day pass URL"); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if r.CheckAfternoon || r.CheckMorning {
		if err := validateHTTPURL(r.HalfDayPassURL, "half-day pass URL"); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
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

type Job struct {
	ID                    int64
	BookingRequestID      *int64
	ProfileID             int64
	OTPSourceID           int64
	Command               JobCommand
	RunMode               RunMode
	Status                JobStatus
	DueAt                 time.Time
	ExpiresAt             *time.Time
	DedupKey              string
	Owner                 string
	CancelRequested       bool
	Message               string
	ExitCode              *int
	ConfirmationStartedAt *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
}

type JobEvent struct {
	ID        int64
	JobID     int64
	Level     string
	Kind      string
	Message   string
	DataJSON  string
	CreatedAt time.Time
}

type ApprovalDecision string

const (
	DecisionApprove ApprovalDecision = "approve"
	DecisionCancel  ApprovalDecision = "cancel"
)

func (d ApprovalDecision) Valid() bool {
	return d == DecisionApprove || d == DecisionCancel
}

type JobDecision struct {
	JobID     int64
	Decision  ApprovalDecision
	CreatedAt time.Time
}

type Admin struct {
	ID        int64
	Username  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Session struct {
	ID            string
	AdminID       int64
	CSRFTokenHash string
	ExpiresAt     time.Time
	CreatedAt     time.Time
	LastSeenAt    time.Time
}

type SessionCredentials struct {
	Token     string
	CSRFToken string
	Session   Session
}

type AuthenticatedSession struct {
	Session Session
	Admin   Admin
}

func ValidateProfile(p Profile) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("profile name is required")
	}
	if strings.TrimSpace(p.BrowserProfile) == "" {
		return errors.New("browser profile is required")
	}
	if p.BrowserProfile == "." || p.BrowserProfile == ".." || strings.ContainsAny(p.BrowserProfile, `/\\`) {
		return errors.New("browser profile must be a single directory name")
	}
	for _, character := range p.BrowserProfile {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return errors.New("browser profile must use only lowercase letters, digits, hyphens, and underscores")
		}
	}
	if strings.TrimSpace(p.DefaultVehicle) == "" {
		return errors.New("default vehicle is required")
	}
	if p.OTPSourceID <= 0 {
		return errors.New("OTP source is required")
	}
	if p.DefaultTimeoutMS < 1_000 || p.DefaultTimeoutMS > 120_000 {
		return errors.New("default timeout must be between 1000 and 120000 milliseconds")
	}
	return nil
}

func ValidateOTPSource(s OTPSource) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("OTP source name is required")
	}
	if !s.Provider.Valid() {
		return fmt.Errorf("unsupported OTP provider %q", s.Provider)
	}
	if strings.TrimSpace(s.Identity) == "" {
		return errors.New("OTP source identity is required")
	}
	return nil
}
