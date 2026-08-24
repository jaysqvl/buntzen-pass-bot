package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func (s *Store) CreateBookingRequest(ctx context.Context, userID int64, request model.BookingRequest) (model.BookingRequest, error) {
	if userID <= 0 {
		return model.BookingRequest{}, ErrUserRequired
	}
	request.UserID = userID
	request = normalizeBooking(request)
	if err := request.Validate(); err != nil {
		return model.BookingRequest{}, err
	}
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO booking_requests(
			user_id, name, profile_id, enabled, schedule_enabled, target_date, timezone, release_time,
			prep_minutes_before, auth_deadline_minutes_before, poll_deadline_seconds,
			poll_min_seconds, poll_max_seconds, confirmation_mode, login_probe_url,
			all_day_pass_url, half_day_pass_url, check_all_day, check_afternoon, check_morning,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, userID, request.Name, request.ProfileID, request.Enabled, request.ScheduleEnabled,
		request.TargetDate, request.Timezone, request.ReleaseTime, request.PrepMinutesBefore,
		request.AuthDeadlineMinutesBefore, request.PollDeadlineSeconds, request.PollMinSeconds,
		request.PollMaxSeconds, request.ConfirmationMode, request.LoginProbeURL,
		request.AllDayPassURL, request.HalfDayPassURL, request.CheckAllDay,
		request.CheckAfternoon, request.CheckMorning, formatTime(now), formatTime(now))
	if err != nil {
		return model.BookingRequest{}, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.BookingRequest{}, fmt.Errorf("read booking request id: %w", err)
	}
	return s.GetBookingRequest(ctx, userID, id)
}

func (s *Store) UpdateBookingRequest(ctx context.Context, userID int64, request model.BookingRequest) (model.BookingRequest, error) {
	if userID <= 0 {
		return model.BookingRequest{}, ErrUserRequired
	}
	if request.ID <= 0 {
		return model.BookingRequest{}, errors.New("booking request id is required")
	}
	request.UserID = userID
	request = normalizeBooking(request)
	if err := request.Validate(); err != nil {
		return model.BookingRequest{}, err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE booking_requests SET
			name = ?, profile_id = ?, enabled = ?, schedule_enabled = ?, target_date = ?,
			timezone = ?, release_time = ?, prep_minutes_before = ?,
			auth_deadline_minutes_before = ?, poll_deadline_seconds = ?, poll_min_seconds = ?,
			poll_max_seconds = ?, confirmation_mode = ?, login_probe_url = ?,
			all_day_pass_url = ?, half_day_pass_url = ?, check_all_day = ?,
			check_afternoon = ?, check_morning = ?, updated_at = ?
		WHERE id = ? AND user_id = ? AND NOT EXISTS (
			SELECT 1 FROM jobs WHERE booking_request_id = booking_requests.id
			AND status IN ('queued', 'running', 'awaiting_approval')
		)
	`, request.Name, request.ProfileID, request.Enabled, request.ScheduleEnabled,
		request.TargetDate, request.Timezone, request.ReleaseTime, request.PrepMinutesBefore,
		request.AuthDeadlineMinutesBefore, request.PollDeadlineSeconds, request.PollMinSeconds,
		request.PollMaxSeconds, request.ConfirmationMode, request.LoginProbeURL,
		request.AllDayPassURL, request.HalfDayPassURL, request.CheckAllDay,
		request.CheckAfternoon, request.CheckMorning, formatTime(s.now()), request.ID, userID)
	if err != nil {
		return model.BookingRequest{}, mapWriteError(err)
	}
	if err := s.classifyOwnedGuardedUpdate(ctx, "booking_requests", userID, request.ID, result); err != nil {
		return model.BookingRequest{}, err
	}
	return s.GetBookingRequest(ctx, userID, request.ID)
}

func (s *Store) GetBookingRequest(ctx context.Context, userID, id int64) (model.BookingRequest, error) {
	if userID <= 0 {
		return model.BookingRequest{}, ErrUserRequired
	}
	return scanBooking(s.db.QueryRowContext(ctx, bookingSelect+" WHERE id = ? AND user_id = ?", id, userID))
}

func (s *Store) ListBookingRequests(ctx context.Context, userID int64) ([]model.BookingRequest, error) {
	if userID <= 0 {
		return nil, ErrUserRequired
	}
	rows, err := s.db.QueryContext(ctx, bookingSelect+" WHERE user_id = ? ORDER BY name, id", userID)
	if err != nil {
		return nil, fmt.Errorf("list booking requests: %w", err)
	}
	defer rows.Close()
	var result []model.BookingRequest
	for rows.Next() {
		request, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, rows.Err()
}

// SystemGetBookingRequest is for trusted scheduler/worker code. HTTP handlers
// must use ForUser so a cross-owner ID remains indistinguishable from missing.
func (s *Store) SystemGetBookingRequest(ctx context.Context, id int64) (model.BookingRequest, error) {
	return scanBooking(s.db.QueryRowContext(ctx, bookingSelect+" WHERE id = ?", id))
}

func (s *Store) SystemListBookingRequests(ctx context.Context) ([]model.BookingRequest, error) {
	rows, err := s.db.QueryContext(ctx, bookingSelect+" ORDER BY user_id, name, id")
	if err != nil {
		return nil, fmt.Errorf("system list booking requests: %w", err)
	}
	defer rows.Close()
	var result []model.BookingRequest
	for rows.Next() {
		request, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, rows.Err()
}

func (s *Store) SystemListScheduledBookingRequests(ctx context.Context) ([]model.BookingRequest, error) {
	rows, err := s.db.QueryContext(ctx, bookingSelect+`
		WHERE enabled = 1 AND schedule_enabled = 1
		AND EXISTS (SELECT 1 FROM users WHERE users.id = booking_requests.user_id AND users.status = 'active')
		AND EXISTS (SELECT 1 FROM profiles WHERE profiles.id = booking_requests.profile_id AND profiles.enabled = 1)
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list scheduled booking requests: %w", err)
	}
	defer rows.Close()
	var result []model.BookingRequest
	for rows.Next() {
		request, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, rows.Err()
}

func (s *Store) DeleteBookingRequest(ctx context.Context, userID, id int64) error {
	if userID <= 0 {
		return ErrUserRequired
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM booking_requests WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return fmt.Errorf("delete booking request: %w", mapWriteError(err))
	}
	return requireAffected(result)
}

const bookingSelect = `
	SELECT id, user_id, name, profile_id, enabled, schedule_enabled, target_date, timezone, release_time,
		prep_minutes_before, auth_deadline_minutes_before, poll_deadline_seconds,
		poll_min_seconds, poll_max_seconds, confirmation_mode, login_probe_url,
		all_day_pass_url, half_day_pass_url, check_all_day, check_afternoon, check_morning,
		created_at, updated_at
	FROM booking_requests`

func scanBooking(scanner rowScanner) (model.BookingRequest, error) {
	var request model.BookingRequest
	var created, updated string
	if err := scanner.Scan(&request.ID, &request.UserID, &request.Name, &request.ProfileID, &request.Enabled,
		&request.ScheduleEnabled, &request.TargetDate, &request.Timezone, &request.ReleaseTime,
		&request.PrepMinutesBefore, &request.AuthDeadlineMinutesBefore,
		&request.PollDeadlineSeconds, &request.PollMinSeconds, &request.PollMaxSeconds,
		&request.ConfirmationMode, &request.LoginProbeURL, &request.AllDayPassURL,
		&request.HalfDayPassURL, &request.CheckAllDay, &request.CheckAfternoon,
		&request.CheckMorning, &created, &updated); errors.Is(err, sql.ErrNoRows) {
		return model.BookingRequest{}, ErrNotFound
	} else if err != nil {
		return model.BookingRequest{}, fmt.Errorf("scan booking request: %w", err)
	}
	var err error
	if request.CreatedAt, err = parseTime(created); err != nil {
		return model.BookingRequest{}, err
	}
	if request.UpdatedAt, err = parseTime(updated); err != nil {
		return model.BookingRequest{}, err
	}
	return request, nil
}

func normalizeBooking(request model.BookingRequest) model.BookingRequest {
	request.Name = strings.TrimSpace(request.Name)
	request.TargetDate = strings.TrimSpace(request.TargetDate)
	request.Timezone = strings.TrimSpace(request.Timezone)
	request.ReleaseTime = strings.TrimSpace(request.ReleaseTime)
	request.LoginProbeURL = strings.TrimSpace(request.LoginProbeURL)
	request.AllDayPassURL = strings.TrimSpace(request.AllDayPassURL)
	request.HalfDayPassURL = strings.TrimSpace(request.HalfDayPassURL)
	return request
}
