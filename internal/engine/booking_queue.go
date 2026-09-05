package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

var (
	ErrBookingNotReleased = errors.New("parking passes have not been released for this target date")
	ErrBookingDatePassed  = errors.New("the booking target date has passed")
)

func (e *Engine) QueueBooking(ctx context.Context, userID, bookingID int64, command model.JobCommand, mode model.RunMode) (model.Job, error) {
	resources := e.store.ForUser(userID)
	booking, err := resources.GetBookingRequest(ctx, bookingID)
	if err != nil {
		return model.Job{}, err
	}
	if err := booking.ValidateForOrigins(e.config.YodelOrigins); err != nil {
		return model.Job{}, err
	}
	params, err := bookingEnqueueParams(booking, command, mode, time.Now().UTC())
	if err != nil {
		return model.Job{}, err
	}
	return resources.EnqueueJob(ctx, params)
}

// QueueBookingNow checks currently available passes and always stops for the
// operator's approval before final confirmation.
func (e *Engine) QueueBookingNow(ctx context.Context, userID, bookingID int64) (model.Job, error) {
	resources := e.store.ForUser(userID)
	booking, err := resources.GetBookingRequest(ctx, bookingID)
	if err != nil {
		return model.Job{}, err
	}
	if err := booking.ValidateForOrigins(e.config.YodelOrigins); err != nil {
		return model.Job{}, err
	}
	params, err := immediateBookingEnqueueParams(booking, time.Now().UTC())
	if err != nil {
		return model.Job{}, err
	}
	return resources.EnqueueJob(ctx, params)
}

func immediateBookingEnqueueParams(booking model.BookingRequest, now time.Time) (store.EnqueueJobParams, error) {
	if err := validateImmediateBookingDate(booking, now); err != nil {
		return store.EnqueueJobParams{}, err
	}
	expiresAt := now.Add(model.MaxImmediateBookingLifetime).UTC()
	return store.EnqueueJobParams{
		BookingRequestID: &booking.ID,
		Command:          model.CommandBook,
		RunMode:          model.RunModeManual,
		RunImmediately:   true,
		DueAt:            now.UTC(),
		ExpiresAt:        &expiresAt,
	}, nil
}

func validateImmediateBookingDate(booking model.BookingRequest, now time.Time) error {
	window, err := scheduler.WindowFor(booking)
	if err != nil {
		return err
	}
	if now.Before(window.ReleaseAt) {
		return ErrBookingNotReleased
	}
	if now.In(window.ReleaseAt.Location()).Format(time.DateOnly) > booking.TargetDate {
		return ErrBookingDatePassed
	}
	return nil
}

// bookingStartTiming checks admission again immediately before launching the
// browser, since provider setup and queueing can consume the remaining window.
func bookingStartTiming(job model.Job, booking model.BookingRequest, now time.Time) (map[string]any, error) {
	if err := job.ValidateImmediateRun(); err != nil {
		return nil, err
	}
	if job.Command != model.CommandBook {
		return nil, nil
	}
	timing := make(map[string]any)
	if job.RunImmediately {
		if err := validateImmediateBookingDate(booking, now); err != nil {
			return nil, err
		}
		if now.Before(job.DueAt) {
			return nil, errors.New("the immediate booking became runnable before its enqueue time")
		}
		timing["auth_deadline_at"] = job.ExpiresAt.Format(time.RFC3339Nano)
	} else {
		window, err := scheduler.WindowFor(booking)
		if err != nil {
			return nil, err
		}
		if now.Before(window.PrepAt) {
			return nil, errors.New("the booking job became runnable before its bounded preparation window")
		}
		if !now.Before(window.PollEndsAt) {
			return nil, errors.New("the booking release window ended before the action could start")
		}
		if now.Before(window.ReleaseAt) {
			timing["release_at"] = window.ReleaseAt.Format(time.RFC3339)
		}
		timing["auth_deadline_at"] = window.AuthDeadlineAt.Format(time.RFC3339)
	}
	if job.ExpiresAt != nil {
		remaining := int(math.Ceil(job.ExpiresAt.Sub(now).Seconds()))
		if remaining < 1 {
			return nil, errors.New("the booking window expired before the action could start")
		}
		if remaining < booking.PollDeadlineSeconds {
			timing["poll_deadline_seconds"] = remaining
		}
	}
	return timing, nil
}

// SystemQueueBooking supports the host-authorized CLI and scheduler path. The
// persisted owner is derived from the booking request, never supplied here.
func (e *Engine) SystemQueueBooking(ctx context.Context, bookingID int64, command model.JobCommand, mode model.RunMode) (model.Job, error) {
	booking, err := e.store.SystemGetBookingRequest(ctx, bookingID)
	if err != nil {
		return model.Job{}, err
	}
	if err := booking.ValidateForOrigins(e.config.YodelOrigins); err != nil {
		return model.Job{}, err
	}
	params, err := bookingEnqueueParams(booking, command, mode, time.Now().UTC())
	if err != nil {
		return model.Job{}, err
	}
	return e.store.SystemEnqueueJob(ctx, params)
}

func bookingEnqueueParams(
	booking model.BookingRequest,
	command model.JobCommand,
	mode model.RunMode,
	now time.Time,
) (store.EnqueueJobParams, error) {
	switch command {
	case model.CommandDryRun:
		mode = model.RunModeDryRun
	case model.CommandAuthCheck:
		mode = model.RunModeManual
	case model.CommandBook:
		if mode == "" {
			mode = booking.ConfirmationMode
		}
		if mode != model.RunModeManual && mode != model.RunModeAuto {
			return store.EnqueueJobParams{}, errors.New("book run mode must be manual or auto")
		}
	default:
		return store.EnqueueJobParams{}, fmt.Errorf("invalid job command %q", command)
	}
	params := store.EnqueueJobParams{
		BookingRequestID: &booking.ID,
		Command:          command,
		RunMode:          mode,
		DueAt:            now.UTC(),
	}
	if command != model.CommandBook {
		return params, nil
	}
	window, err := scheduler.WindowFor(booking)
	if err != nil {
		return store.EnqueueJobParams{}, err
	}
	if !now.Before(window.PollEndsAt) {
		return store.EnqueueJobParams{}, errors.New("the booking release window has ended")
	}
	if now.Before(window.PrepAt) {
		params.DueAt = window.PrepAt.UTC()
	}
	expiresAt := window.PollEndsAt.UTC()
	params.ExpiresAt = &expiresAt
	return params, nil
}

func (e *Engine) scheduleLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		e.queueScheduled(e.ctx, time.Now())
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) queueScheduled(ctx context.Context, now time.Time) {
	requests, err := e.store.SystemListScheduledBookingRequests(ctx)
	if err != nil {
		slog.Error("scheduled booking scan failed", "error", err)
		return
	}
	for _, request := range requests {
		if err := request.ValidateForOrigins(e.config.YodelOrigins); err != nil {
			slog.Warn("scheduled booking has an invalid Yodel origin policy", "booking_id", request.ID)
			continue
		}
		window, err := scheduler.WindowFor(request)
		if err != nil || !scheduler.ShouldQueue(now, window) {
			continue
		}
		_, err = e.store.SystemEnqueueJob(ctx, store.EnqueueJobParams{
			BookingRequestID: &request.ID, Command: model.CommandBook,
			RunMode: request.ConfirmationMode, DueAt: now.UTC(), ExpiresAt: &window.PollEndsAt,
			DedupKey: scheduler.DedupKey(request),
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			slog.Error("scheduled booking could not be queued", "booking_id", request.ID, "error", err)
		} else if err == nil {
			slog.Info("scheduled booking queued", "booking_id", request.ID)
		}
	}
}
