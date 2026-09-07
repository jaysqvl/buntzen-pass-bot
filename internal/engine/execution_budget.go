package engine

import (
	"errors"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
)

var ErrExecutionBudget = errors.New("job execution time limit exceeded")

const (
	interactiveExecutionBudget = 15 * time.Minute
	checkoutExecutionGrace     = 15 * time.Minute
	executionInputBudget       = 15 * time.Second
	executionBudgetMessage     = "The job exceeded its execution time limit."
)

func jobExecutionDeadline(job model.Job, booking model.BookingRequest, started time.Time, interactive, checkout time.Duration) (time.Time, error) {
	if err := job.ValidateImmediateRun(); err != nil {
		return time.Time{}, err
	}
	if interactive <= 0 || checkout <= 0 {
		return time.Time{}, errors.New("job execution budgets must be positive")
	}
	if job.RunImmediately {
		return *job.ExpiresAt, nil
	}
	if job.Command == model.CommandBook {
		window, err := scheduler.WindowFor(booking)
		if err != nil {
			return time.Time{}, err
		}
		return window.PollEndsAt.Add(checkout), nil
	}
	return started.Add(interactive), nil
}
