package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

// BookingConflict describes existing work after enqueueing was rejected. A
// reservation can outlive its job history, so Reservation may be true with no Job.
type BookingConflict struct {
	Job         *model.Job
	Reservation bool
}

// BookingConflict is a read-only explanation, not an admission check. Enqueueing
// still relies on transactional constraints to prevent concurrent duplicates.
func (s *Store) BookingConflict(ctx context.Context, userID, bookingID int64, command model.JobCommand) (BookingConflict, error) {
	if userID <= 0 {
		return BookingConflict{}, ErrUserRequired
	}
	if !command.Valid() {
		return BookingConflict{}, fmt.Errorf("invalid job command %q", command)
	}
	booking, err := s.GetBookingRequest(ctx, userID, bookingID)
	if err != nil {
		return BookingConflict{}, err
	}
	if command == model.CommandBook {
		var jobID sql.NullInt64
		err := s.db.QueryRowContext(ctx, `
			SELECT reservation.job_id FROM booking_reservations AS reservation
			JOIN profiles AS profile ON profile.id = reservation.profile_id
			WHERE profile.user_id = ? AND reservation.profile_id = ? AND reservation.target_date = ?
		`, userID, booking.ProfileID, booking.TargetDate).Scan(&jobID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return BookingConflict{}, fmt.Errorf("read booking reservation: %w", err)
		}
		if err == nil {
			conflict := BookingConflict{Reservation: true}
			if jobID.Valid {
				job, err := s.GetJob(ctx, userID, jobID.Int64)
				if err != nil && !errors.Is(err, ErrNotFound) {
					return BookingConflict{}, err
				}
				if err == nil {
					conflict.Job = &job
				}
			}
			return conflict, nil
		}
	}
	job, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+` FROM jobs
		WHERE user_id = ? AND booking_request_id = ? AND command = ?
		AND status IN ('queued', 'running', 'awaiting_approval')
		ORDER BY id LIMIT 1`, userID, bookingID, command))
	if errors.Is(err, ErrNotFound) {
		return BookingConflict{}, nil
	}
	if err != nil {
		return BookingConflict{}, fmt.Errorf("read pending booking job: %w", err)
	}
	return BookingConflict{Job: &job}, nil
}
