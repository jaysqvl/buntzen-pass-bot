package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
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
