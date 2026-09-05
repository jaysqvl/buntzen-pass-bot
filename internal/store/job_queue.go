package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

type EnqueueJobParams struct {
	BookingRequestID *int64
	ProfileID        int64
	Command          model.JobCommand
	RunMode          model.RunMode
	DueAt            time.Time
	ExpiresAt        *time.Time
	DedupKey         string
}

func (s *Store) EnqueueJob(ctx context.Context, userID int64, params EnqueueJobParams) (model.Job, error) {
	if userID <= 0 {
		return model.Job{}, ErrUserRequired
	}
	return s.enqueueJob(ctx, userID, params)
}

// SystemEnqueueJob is used by the scheduler. Ownership is always derived from
// the selected booking/profile; callers cannot supply a job owner.
func (s *Store) SystemEnqueueJob(ctx context.Context, params EnqueueJobParams) (model.Job, error) {
	return s.enqueueJob(ctx, 0, params)
}

func (s *Store) enqueueJob(ctx context.Context, actorUserID int64, params EnqueueJobParams) (model.Job, error) {
	if !params.Command.Valid() {
		return model.Job{}, fmt.Errorf("invalid job command %q", params.Command)
	}
	switch params.Command {
	case model.CommandAuthCheck:
		params.RunMode = model.RunModeManual
	case model.CommandDryRun:
		params.RunMode = model.RunModeDryRun
	case model.CommandBook:
		if params.RunMode != model.RunModeManual && params.RunMode != model.RunModeAuto {
			return model.Job{}, errors.New("book run mode must be manual or auto")
		}
	}
	if !params.RunMode.Valid() {
		return model.Job{}, fmt.Errorf("invalid run mode %q", params.RunMode)
	}
	if params.Command != model.CommandAuthCheck && params.BookingRequestID == nil {
		return model.Job{}, errors.New("dry-run and book jobs require a booking request")
	}
	if params.DueAt.IsZero() {
		params.DueAt = s.now()
	}
	if params.ExpiresAt != nil && !params.ExpiresAt.After(params.DueAt) {
		return model.Job{}, errors.New("job expiry must be after its due time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Job{}, fmt.Errorf("begin enqueue transaction: %w", err)
	}
	defer tx.Rollback()

	profileID := params.ProfileID
	var jobUserID int64
	if params.BookingRequestID != nil {
		var bookingProfileID int64
		var bookingUserID int64
		var bookingEnabled bool
		query := "SELECT profile_id, user_id, enabled FROM booking_requests WHERE id = ?"
		args := []any{*params.BookingRequestID}
		if actorUserID > 0 {
			query += " AND user_id = ?"
			args = append(args, actorUserID)
		}
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&bookingProfileID, &bookingUserID, &bookingEnabled); errors.Is(err, sql.ErrNoRows) {
			return model.Job{}, ErrNotFound
		} else if err != nil {
			return model.Job{}, fmt.Errorf("read booking request profile: %w", err)
		}
		if !bookingEnabled {
			return model.Job{}, errors.New("booking request is disabled")
		}
		if profileID != 0 && profileID != bookingProfileID {
			return model.Job{}, errors.New("booking request does not belong to the selected profile")
		}
		profileID = bookingProfileID
		jobUserID = bookingUserID
	}
	if profileID <= 0 {
		return model.Job{}, errors.New("profile is required")
	}
	var sourceID, profileUserID int64
	profileQuery := "SELECT otp_source_id, user_id FROM profiles WHERE id = ? AND enabled = 1"
	profileArgs := []any{profileID}
	if actorUserID > 0 {
		profileQuery += " AND user_id = ?"
		profileArgs = append(profileArgs, actorUserID)
	}
	if err := tx.QueryRowContext(ctx, profileQuery, profileArgs...).Scan(&sourceID, &profileUserID); errors.Is(err, sql.ErrNoRows) {
		if actorUserID > 0 {
			return model.Job{}, ErrNotFound
		}
		return model.Job{}, errors.New("profile does not exist or is disabled")
	} else if err != nil {
		return model.Job{}, fmt.Errorf("read profile OTP source: %w", err)
	}
	if jobUserID != 0 && jobUserID != profileUserID {
		return model.Job{}, fmt.Errorf("%w: booking request and profile owners differ", ErrConflict)
	}
	jobUserID = profileUserID
	now := s.now()
	if _, err := pruneJobHistory(ctx, tx, jobUserID, now); err != nil {
		return model.Job{}, fmt.Errorf("prune job history before enqueue: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(
			user_id, booking_request_id, profile_id, otp_source_id, command, run_mode, status,
			due_at, expires_at, dedup_key, created_at, updated_at
		)
		SELECT ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?
		FROM users AS account
		WHERE account.id = ? AND account.status = 'active'
		AND (
			SELECT count(*) FROM jobs AS pending
			WHERE pending.user_id = account.id
			AND pending.status IN ('queued', 'running', 'awaiting_approval')
		) < ?
		AND (SELECT count(*) FROM jobs AS retained WHERE retained.user_id = account.id) < ?
	`, jobUserID, params.BookingRequestID, profileID, sourceID, params.Command, params.RunMode,
		formatTime(params.DueAt), optionalTimeValue(params.ExpiresAt), strings.TrimSpace(params.DedupKey),
		formatTime(now), formatTime(now), jobUserID, MaxPendingJobsPerUser, MaxRetainedJobsPerUser)
	if err != nil {
		return model.Job{}, mapWriteError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return model.Job{}, fmt.Errorf("read enqueue result: %w", err)
	}
	if count == 0 {
		var active int
		if err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM users WHERE id = ? AND status = 'active'", jobUserID,
		).Scan(&active); err != nil {
			return model.Job{}, fmt.Errorf("classify job admission: %w", err)
		}
		if active == 0 {
			return model.Job{}, ErrNotFound
		}
		var pending, retained int
		if err := tx.QueryRowContext(ctx, `
			SELECT
				(SELECT count(*) FROM jobs WHERE user_id = ? AND status IN ('queued', 'running', 'awaiting_approval')),
				(SELECT count(*) FROM jobs WHERE user_id = ?)
		`, jobUserID, jobUserID).Scan(&pending, &retained); err != nil {
			return model.Job{}, fmt.Errorf("classify job admission limit: %w", err)
		}
		if retained >= MaxRetainedJobsPerUser {
			return model.Job{}, fmt.Errorf("%w: retained job history limit reached", ErrResourceLimit)
		}
		return model.Job{}, fmt.Errorf("%w: pending job limit reached (%d/%d)", ErrResourceLimit, pending, MaxPendingJobsPerUser)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Job{}, fmt.Errorf("read job id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Job{}, fmt.Errorf("commit enqueue transaction: %w", err)
	}
	if actorUserID > 0 {
		return s.GetJob(ctx, actorUserID, id)
	}
	return s.SystemGetJob(ctx, id)
}

func (s *Store) SystemClaimNextDueJob(ctx context.Context, workerOwner string) (model.Job, error) {
	return s.SystemClaimNextDueJobAt(ctx, workerOwner, s.now())
}

// SystemClaimNextDueJobAt atomically selects one due job whose profile and OTP source
// are not in use, then leases both by moving the job to running.
func (s *Store) SystemClaimNextDueJobAt(ctx context.Context, workerOwner string, now time.Time) (model.Job, error) {
	workerOwner = strings.TrimSpace(workerOwner)
	if workerOwner == "" {
		return model.Job{}, errors.New("job worker owner is required")
	}
	if _, err := s.SystemExpireStaleJobs(ctx, now); err != nil {
		return model.Job{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT candidate.id
			FROM jobs AS candidate
			JOIN users AS account ON account.id = candidate.user_id AND account.status = 'active'
			JOIN profiles AS profile ON profile.id = candidate.profile_id
			LEFT JOIN booking_requests AS booking ON booking.id = candidate.booking_request_id
			WHERE candidate.status = 'queued' AND candidate.cancel_requested = 0 AND candidate.due_at <= ?
			AND (candidate.expires_at IS NULL OR candidate.expires_at > ?)
			AND profile.enabled = 1 AND profile.otp_source_id = candidate.otp_source_id
			AND (candidate.command <> 'book' OR EXISTS (
				SELECT 1 FROM booking_reservations WHERE job_id = candidate.id
				AND profile_id = candidate.profile_id
			))
			AND (
				candidate.booking_request_id IS NULL OR
				(booking.enabled = 1 AND booking.profile_id = candidate.profile_id)
			)
			AND NOT EXISTS (
				SELECT 1 FROM jobs AS active
				WHERE active.status IN ('running', 'awaiting_approval')
				AND (active.profile_id = candidate.profile_id OR active.otp_source_id = candidate.otp_source_id)
			)
			ORDER BY candidate.due_at, candidate.id
			LIMIT 1
		)
		UPDATE jobs SET status = 'running', worker_owner = ?, started_at = COALESCE(started_at, ?), updated_at = ?
		WHERE id = (SELECT id FROM candidate) AND status = 'queued'
		RETURNING `+jobColumns, formatTime(now), formatTime(now), workerOwner, formatTime(now), formatTime(now))
	job, err := scanJob(row)
	if errors.Is(err, ErrNotFound) {
		return model.Job{}, ErrNotFound
	}
	return job, err
}
