// Package scheduler computes booking release windows without owning a timer loop.
package scheduler

import (
	"fmt"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

type Window struct {
	ReleaseAt      time.Time
	PrepAt         time.Time
	AuthDeadlineAt time.Time
	PollEndsAt     time.Time
}

func WindowFor(request model.BookingRequest) (Window, error) {
	if err := request.Validate(); err != nil {
		return Window{}, err
	}
	location, err := time.LoadLocation(request.Timezone)
	if err != nil {
		return Window{}, fmt.Errorf("load booking timezone: %w", err)
	}
	target, err := time.ParseInLocation(time.DateOnly, request.TargetDate, location)
	if err != nil {
		return Window{}, fmt.Errorf("parse target date: %w", err)
	}
	clock, err := time.Parse("15:04", request.ReleaseTime)
	if err != nil {
		return Window{}, fmt.Errorf("parse release time: %w", err)
	}
	releaseDate := target.AddDate(0, 0, -1)
	release := time.Date(releaseDate.Year(), releaseDate.Month(), releaseDate.Day(),
		clock.Hour(), clock.Minute(), 0, 0, location)
	return Window{
		ReleaseAt:      release,
		PrepAt:         release.Add(-time.Duration(request.PrepMinutesBefore) * time.Minute),
		AuthDeadlineAt: release.Add(-time.Duration(request.AuthDeadlineMinutesBefore) * time.Minute),
		PollEndsAt:     release.Add(time.Duration(request.PollDeadlineSeconds) * time.Second),
	}, nil
}

func ShouldQueue(now time.Time, window Window) bool {
	return !now.Before(window.PrepAt) && now.Before(window.PollEndsAt)
}

func DedupKey(request model.BookingRequest) string {
	return fmt.Sprintf("booking:%d:%s", request.ID, request.TargetDate)
}
