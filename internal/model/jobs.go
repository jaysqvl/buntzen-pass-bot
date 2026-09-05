package model

import (
	"time"
)

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

type Job struct {
	ID                    int64
	UserID                int64
	BookingRequestID      *int64
	ProfileID             int64
	OTPSourceID           int64
	Command               JobCommand
	RunMode               RunMode
	Status                JobStatus
	DueAt                 time.Time
	ExpiresAt             *time.Time
	DedupKey              string
	WorkerOwner           string
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
	UserID    int64
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
	UserID    int64
	Decision  ApprovalDecision
	CreatedAt time.Time
}
