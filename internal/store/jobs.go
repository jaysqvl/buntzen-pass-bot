package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
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

type jobHistoryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
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

type JobTransition struct {
	Message             string
	ExitCode            *int
	ConfirmationStarted bool
}

func (s *Store) SystemTransitionJob(
	ctx context.Context,
	id int64,
	expected []model.JobStatus,
	to model.JobStatus,
	update JobTransition,
) (model.Job, error) {
	if len(expected) == 0 || !to.Valid() {
		return model.Job{}, errors.New("expected states and a valid destination state are required")
	}
	for _, from := range expected {
		if !from.Valid() || !model.CanTransition(from, to) {
			return model.Job{}, fmt.Errorf("invalid job transition %s -> %s", from, to)
		}
	}
	current, err := s.SystemGetJob(ctx, id)
	if err != nil {
		return model.Job{}, err
	}
	if to == model.JobOutcomeUnknown && current.ConfirmationStartedAt == nil && !update.ConfirmationStarted {
		return model.Job{}, errors.New("outcome_unknown requires final confirmation to have started")
	}

	now := s.now()
	message := truncate(sanitizeText(update.Message), 1000)
	sets := make([]string, 0, 8)
	args := make([]any, 0, 24)
	if to.Terminal() {
		// Resolve cancellation/disable races inside the same write that records
		// completion. If final confirmation has not started, a committed revoke
		// wins over a late worker result. Once confirmation may have been clicked,
		// a confirmed success remains success and every other result is unknown.
		const resolution = `CASE
			WHEN (confirmation_started_at IS NOT NULL OR ? = 1) AND ? <> 'succeeded' THEN %s
			WHEN confirmation_started_at IS NULL AND ? = 0 AND (
				cancel_requested = 1 OR NOT EXISTS (
					SELECT 1 FROM users WHERE users.id = jobs.user_id AND users.status = 'active'
				)
			) THEN %s
			ELSE %s END`
		confirmationStarting := 0
		if update.ConfirmationStarted {
			confirmationStarting = 1
		}
		appendResolution := func(column, unknownSQL, cancelledSQL, fallbackSQL string, unknown, cancelled, fallback any) {
			sets = append(sets, column+" = "+fmt.Sprintf(resolution, unknownSQL, cancelledSQL, fallbackSQL))
			args = append(args, confirmationStarting, to)
			if unknownSQL == "?" {
				args = append(args, unknown)
			}
			args = append(args, confirmationStarting)
			if cancelledSQL == "?" {
				args = append(args, cancelled)
			}
			if fallbackSQL == "?" {
				args = append(args, fallback)
			}
		}
		appendResolution(
			"status", "?", "?", "?",
			model.JobOutcomeUnknown, model.JobCancelled, to,
		)
		appendResolution(
			"message", "?", "?", "?",
			"The action ended after final confirmation may have started; booking outcome is unknown.",
			"Cancelled because cancellation was requested or the account was disabled.",
			message,
		)
		appendResolution("exit_code", "NULL", "NULL", "?", nil, nil, update.ExitCode)
	} else {
		sets = append(sets, "status = ?", "message = ?", "exit_code = ?")
		args = append(args, to, message, update.ExitCode)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, formatTime(now))
	if update.ConfirmationStarted {
		sets = append(sets, "confirmation_started_at = COALESCE(confirmation_started_at, ?)")
		args = append(args, formatTime(now))
	}
	if to == model.JobRunning {
		sets = append(sets, "started_at = COALESCE(started_at, ?)")
		args = append(args, formatTime(now))
	}
	if to.Terminal() {
		sets = append(sets, "finished_at = ?", "worker_owner = ''")
		args = append(args, formatTime(now))
	}
	placeholders := make([]string, len(expected))
	for index, status := range expected {
		placeholders[index] = "?"
		args = append(args, status)
	}
	query := "UPDATE jobs SET " + strings.Join(sets, ", ") +
		" WHERE id = ? AND status IN (" + strings.Join(placeholders, ",") + ")"
	if to == model.JobRunning || to == model.JobAwaitingApproval {
		query += " AND cancel_requested = 0" +
			" AND EXISTS (SELECT 1 FROM users WHERE users.id = jobs.user_id AND users.status = 'active')"
	}
	// The job id belongs before the expected-state values in the WHERE clause.
	stateArgs := append([]any(nil), args[len(args)-len(expected):]...)
	args = append(args[:len(args)-len(expected)], id)
	args = append(args, stateArgs...)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return model.Job{}, fmt.Errorf("transition job: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return model.Job{}, fmt.Errorf("read transition result: %w", err)
	}
	if count == 0 {
		return model.Job{}, ErrTransitionConflict
	}
	return s.SystemGetJob(ctx, id)
}

func (s *Store) SystemMarkConfirmationStarted(ctx context.Context, id int64) error {
	now := formatTime(s.now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET confirmation_started_at = COALESCE(confirmation_started_at, ?), updated_at = ?
		WHERE id = ? AND status IN ('running', 'awaiting_approval') AND cancel_requested = 0
		AND EXISTS (SELECT 1 FROM users WHERE users.id = jobs.user_id AND users.status = 'active')
	`, now, now, id)
	if err != nil {
		return fmt.Errorf("mark final confirmation: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read final confirmation result: %w", err)
	}
	if count == 0 {
		return ErrTransitionConflict
	}
	return nil
}

// RequestJobCancellation is the durable cross-process cancellation signal.
// Queued work is cancelled immediately; an active worker observes the flag and
// cooperatively stops its Python/browser process.
func (s *Store) RequestJobCancellation(ctx context.Context, userID, id int64) error {
	if userID <= 0 {
		return ErrUserRequired
	}
	return s.requestJobCancellation(ctx, userID, id)
}

func (s *Store) SystemRequestJobCancellation(ctx context.Context, id int64) error {
	return s.requestJobCancellation(ctx, 0, id)
}

func (s *Store) requestJobCancellation(ctx context.Context, actorUserID, id int64) error {
	now := formatTime(s.now())
	var status model.JobStatus
	query := `
		UPDATE jobs SET
			cancel_requested = 1,
			status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
			message = CASE WHEN status = 'queued' THEN 'Cancelled before execution.' ELSE message END,
			finished_at = CASE WHEN status = 'queued' THEN ? ELSE finished_at END,
			updated_at = ?
		WHERE id = ?`
	args := []any{now, now, id}
	if actorUserID > 0 {
		query += " AND user_id = ?"
		args = append(args, actorUserID)
	}
	query += ` AND status IN ('queued', 'running', 'awaiting_approval', 'cancelled')
		RETURNING status
	`
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&status)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("request job cancellation: %w", err)
	}
	// Distinguish a missing job from an existing terminal job. The update itself
	// remains a single compare-and-set statement, so a concurrent claim or
	// completion cannot be overwritten based on a stale status read.
	readQuery := "SELECT status FROM jobs WHERE id = ?"
	readArgs := []any{id}
	if actorUserID > 0 {
		readQuery += " AND user_id = ?"
		readArgs = append(readArgs, actorUserID)
	}
	if err := s.db.QueryRowContext(ctx, readQuery, readArgs...).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read cancellation target: %w", err)
	}
	return ErrTransitionConflict
}

func (s *Store) SystemJobCancellationRequested(ctx context.Context, id int64) (bool, error) {
	var requested bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT jobs.cancel_requested OR users.status <> 'active'
		FROM jobs JOIN users ON users.id = jobs.user_id WHERE jobs.id = ?
	`, id).Scan(&requested); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, fmt.Errorf("read cancellation request: %w", err)
	}
	return requested, nil
}

func (s *Store) GetJob(ctx context.Context, userID, id int64) (model.Job, error) {
	if userID <= 0 {
		return model.Job{}, ErrUserRequired
	}
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ? AND user_id = ?", id, userID))
}

func (s *Store) ListJobs(ctx context.Context, userID int64, limit int) ([]model.Job, error) {
	if userID <= 0 {
		return nil, ErrUserRequired
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+jobColumns+" FROM jobs WHERE user_id = ? ORDER BY id DESC LIMIT ?", userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var result []model.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *Store) SystemGetJob(ctx context.Context, id int64) (model.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id))
}

func (s *Store) SystemListJobs(ctx context.Context, limit int) ([]model.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+jobColumns+" FROM jobs ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("system list jobs: %w", err)
	}
	defer rows.Close()
	var result []model.Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func (s *Store) SystemRecoverInterruptedJobs(ctx context.Context) (int64, error) {
	now := formatTime(s.now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET
			status = CASE WHEN confirmation_started_at IS NULL THEN 'interrupted' ELSE 'outcome_unknown' END,
			message = CASE WHEN confirmation_started_at IS NULL
				THEN 'Interrupted by control-plane restart.'
				ELSE 'Control plane restarted after final confirmation began; outcome requires review.' END,
			worker_owner = '', finished_at = ?, updated_at = ?
		WHERE status IN ('running', 'awaiting_approval')
	`, now, now)
	if err != nil {
		return 0, fmt.Errorf("recover interrupted jobs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read recovery result: %w", err)
	}
	return count, nil
}

func (s *Store) SystemExpireStaleJobs(ctx context.Context, now time.Time) (int64, error) {
	formatted := formatTime(now)
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'failed', message = 'The booking window expired before this job could start.',
			worker_owner = '', finished_at = ?, updated_at = ?
		WHERE status = 'queued' AND expires_at IS NOT NULL AND expires_at <= ?
	`, formatted, formatted, formatted)
	if err != nil {
		return 0, fmt.Errorf("expire stale jobs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read stale-job result: %w", err)
	}
	return count, nil
}

// SystemPruneTerminalJobs removes terminal history that has become eligible for
// retention since its last status transition. In particular, scheduler rows
// become prunable only after their deduplication window expires.
func (s *Store) SystemPruneTerminalJobs(ctx context.Context, now time.Time) (int64, error) {
	// Completed booking guards outlive pruned job rows so history cleanup cannot
	// permit another confirmation. Once the target day has passed everywhere,
	// detached guards are no longer needed by the bounded booking window.
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM booking_reservations
		WHERE job_id IS NULL AND target_date < date(?, '-1 day')
	`, formatTime(now)); err != nil {
		return 0, fmt.Errorf("prune past booking reservations: %w", err)
	}
	count, err := pruneJobHistory(ctx, s.db, 0, now)
	if err != nil {
		return 0, fmt.Errorf("prune job history: %w", err)
	}
	return count, nil
}

func pruneJobHistory(
	ctx context.Context,
	executor jobHistoryExecer,
	userID int64,
	now time.Time,
) (int64, error) {
	result, err := executor.ExecContext(ctx, `
		DELETE FROM jobs WHERE id IN (
			SELECT id FROM (
				SELECT id, row_number() OVER (
					PARTITION BY user_id ORDER BY updated_at DESC, id DESC
				) AS retention_rank
				FROM jobs
				WHERE status IN ('succeeded', 'failed', 'cancelled', 'interrupted')
				AND (
					dedup_key = '' OR dedup_key GLOB 'pairing:*' OR
					(expires_at IS NOT NULL AND expires_at <= ?)
				)
				AND (? = 0 OR user_id = ?)
			) AS ranked_terminal_history
			WHERE retention_rank > ?
		)
	`, formatTime(now), userID, userID, MaxTerminalJobHistoryPerUser)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

const jobColumns = `id, user_id, booking_request_id, profile_id, otp_source_id, command, run_mode,
	status, due_at, expires_at, dedup_key, worker_owner, cancel_requested, message, exit_code, confirmation_started_at,
	created_at, updated_at, started_at, finished_at`

func scanJob(scanner rowScanner) (model.Job, error) {
	var job model.Job
	var bookingID sql.NullInt64
	var exitCode sql.NullInt64
	var dueAt, createdAt, updatedAt string
	var expiresAt, confirmationAt, startedAt, finishedAt sql.NullString
	if err := scanner.Scan(&job.ID, &job.UserID, &bookingID, &job.ProfileID, &job.OTPSourceID,
		&job.Command, &job.RunMode, &job.Status, &dueAt, &expiresAt, &job.DedupKey, &job.WorkerOwner,
		&job.CancelRequested, &job.Message, &exitCode, &confirmationAt, &createdAt, &updatedAt,
		&startedAt, &finishedAt); errors.Is(err, sql.ErrNoRows) {
		return model.Job{}, ErrNotFound
	} else if err != nil {
		return model.Job{}, fmt.Errorf("scan job: %w", err)
	}
	if bookingID.Valid {
		job.BookingRequestID = &bookingID.Int64
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		job.ExitCode = &code
	}
	var err error
	if job.DueAt, err = parseTime(dueAt); err != nil {
		return model.Job{}, err
	}
	if job.ExpiresAt, err = parseOptionalTime(expiresAt); err != nil {
		return model.Job{}, err
	}
	if job.CreatedAt, err = parseTime(createdAt); err != nil {
		return model.Job{}, err
	}
	if job.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return model.Job{}, err
	}
	if job.ConfirmationStartedAt, err = parseOptionalTime(confirmationAt); err != nil {
		return model.Job{}, err
	}
	if job.StartedAt, err = parseOptionalTime(startedAt); err != nil {
		return model.Job{}, err
	}
	if job.FinishedAt, err = parseOptionalTime(finishedAt); err != nil {
		return model.Job{}, err
	}
	return job, nil
}

type JobEventInput struct {
	JobID   int64
	Level   string
	Kind    string
	Message string
	Data    map[string]any
}

func (s *Store) SystemAppendJobEvent(ctx context.Context, input JobEventInput) (model.JobEvent, error) {
	level := strings.ToLower(strings.TrimSpace(input.Level))
	if level == "" {
		level = "info"
	}
	if level != "debug" && level != "info" && level != "warning" && level != "error" {
		return model.JobEvent{}, errors.New("invalid job event level")
	}
	kind := truncate(sanitizeText(strings.TrimSpace(input.Kind)), 80)
	if kind == "" {
		return model.JobEvent{}, errors.New("job event kind is required")
	}
	knownSecrets, err := s.durableEventSecrets(ctx, input.JobID)
	if err != nil {
		return model.JobEvent{}, err
	}
	data, err := normalizeAndSanitize(input.Data)
	if err != nil {
		return model.JobEvent{}, err
	}
	data = redactKnownValue(data, knownSecrets)
	encoded, err := json.Marshal(data)
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("encode job event data: %w", err)
	}
	if len(encoded) > 16*1024 {
		return model.JobEvent{}, errors.New("job event data exceeds 16 KiB")
	}
	now := s.now()
	message := truncate(redactKnownText(sanitizeText(input.Message), knownSecrets), 2000)
	kind = truncate(redactKnownText(kind, knownSecrets), 80)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO job_events(user_id, job_id, level, kind, message, data_json, created_at)
		SELECT user_id, id, ?, ?, ?, ?, ? FROM jobs WHERE id = ?
	`, level, kind, message, string(encoded), formatTime(now), input.JobID)
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("append job event: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return model.JobEvent{}, fmt.Errorf("read job event result: %w", err)
	} else if count == 0 {
		return model.JobEvent{}, ErrNotFound
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("read job event id: %w", err)
	}
	job, err := s.SystemGetJob(ctx, input.JobID)
	if err != nil {
		return model.JobEvent{}, err
	}
	return model.JobEvent{ID: id, UserID: job.UserID, JobID: input.JobID, Level: level, Kind: kind,
		Message: message, DataJSON: string(encoded), CreatedAt: now}, nil
}

func (s *Store) ListJobEvents(ctx context.Context, userID, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if userID <= 0 {
		return nil, ErrUserRequired
	}
	if _, err := s.GetJob(ctx, userID, jobID); err != nil {
		return nil, err
	}
	return s.listJobEvents(ctx, userID, jobID, afterID, limit)
}

func (s *Store) SystemListJobEvents(ctx context.Context, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if _, err := s.SystemGetJob(ctx, jobID); err != nil {
		return nil, err
	}
	return s.listJobEvents(ctx, 0, jobID, afterID, limit)
}

func (s *Store) listJobEvents(ctx context.Context, userID, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, user_id, job_id, level, kind, message, data_json, created_at
		FROM job_events WHERE job_id = ? AND id > ?`
	args := []any{jobID, afterID}
	if userID > 0 {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list job events: %w", err)
	}
	defer rows.Close()
	var result []model.JobEvent
	for rows.Next() {
		var event model.JobEvent
		var created string
		if err := rows.Scan(&event.ID, &event.UserID, &event.JobID, &event.Level, &event.Kind,
			&event.Message, &event.DataJSON, &created); err != nil {
			return nil, fmt.Errorf("scan job event: %w", err)
		}
		event.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) RecordJobDecision(ctx context.Context, userID, jobID int64, decision model.ApprovalDecision) (model.JobDecision, error) {
	if userID <= 0 {
		return model.JobDecision{}, ErrUserRequired
	}
	if !decision.Valid() {
		return model.JobDecision{}, errors.New("invalid job decision")
	}
	// The INSERT is the state check and unique-decision race in one SQLite
	// statement. Concurrent approve/cancel requests therefore select exactly one
	// durable winner instead of racing through separate reads in deferred
	// transactions.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO job_decisions(job_id, user_id, decision, created_at)
		SELECT jobs.id, jobs.user_id, ?, ? FROM jobs
		JOIN users ON users.id = jobs.user_id AND users.status = 'active'
		WHERE jobs.id = ? AND jobs.user_id = ? AND jobs.status = 'awaiting_approval'
		ON CONFLICT(job_id) DO NOTHING
	`, decision, formatTime(s.now()), jobID, userID); err != nil {
		return model.JobDecision{}, fmt.Errorf("record job decision: %w", err)
	}
	recorded, err := s.GetJobDecision(ctx, userID, jobID)
	if err == nil && recorded.Decision != decision {
		return model.JobDecision{}, ErrDecisionConflict
	}
	if err == nil {
		return recorded, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return model.JobDecision{}, err
	}
	var status model.JobStatus
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM jobs WHERE id = ? AND user_id = ?", jobID, userID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return model.JobDecision{}, ErrNotFound
	} else if err != nil {
		return model.JobDecision{}, fmt.Errorf("read decision job: %w", err)
	}
	return model.JobDecision{}, ErrTransitionConflict
}

func (s *Store) GetJobDecision(ctx context.Context, userID, jobID int64) (model.JobDecision, error) {
	if userID <= 0 {
		return model.JobDecision{}, ErrUserRequired
	}
	var decision model.JobDecision
	var created string
	if err := s.db.QueryRowContext(ctx,
		"SELECT job_id, user_id, decision, created_at FROM job_decisions WHERE job_id = ? AND user_id = ?", jobID, userID,
	).Scan(&decision.JobID, &decision.UserID, &decision.Decision, &created); errors.Is(err, sql.ErrNoRows) {
		return model.JobDecision{}, ErrNotFound
	} else if err != nil {
		return model.JobDecision{}, fmt.Errorf("read job decision: %w", err)
	}
	var err error
	decision.CreatedAt, err = parseTime(created)
	return decision, err
}

func (s *Store) SystemGetJobDecision(ctx context.Context, jobID int64) (model.JobDecision, error) {
	var decision model.JobDecision
	var created string
	if err := s.db.QueryRowContext(ctx,
		"SELECT job_id, user_id, decision, created_at FROM job_decisions WHERE job_id = ?", jobID,
	).Scan(&decision.JobID, &decision.UserID, &decision.Decision, &created); errors.Is(err, sql.ErrNoRows) {
		return model.JobDecision{}, ErrNotFound
	} else if err != nil {
		return model.JobDecision{}, fmt.Errorf("read job decision: %w", err)
	}
	var err error
	decision.CreatedAt, err = parseTime(created)
	return decision, err
}

var (
	digitSecretPattern = regexp.MustCompile(`\b[0-9]{4,8}\b`)
	querySecretPattern = regexp.MustCompile(`(?i)(password|token|secret|otp|code)=([^&\s]+)`)
)

func sanitizeText(value string) string {
	value = querySecretPattern.ReplaceAllString(value, "$1=[REDACTED]")
	return digitSecretPattern.ReplaceAllString(value, "[REDACTED]")
}

func sanitizeValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "token") ||
				strings.Contains(lower, "secret") || strings.Contains(lower, "otp") ||
				strings.Contains(lower, "code") {
				result[key] = "[REDACTED]"
			} else {
				result[key] = sanitizeValue(child)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = sanitizeValue(child)
		}
		return result
	case string:
		return sanitizeText(typed)
	case int:
		if typed >= 1_000 && typed <= 99_999_999 {
			return "[REDACTED]"
		}
		return typed
	case int64:
		if typed >= 1_000 && typed <= 99_999_999 {
			return "[REDACTED]"
		}
		return typed
	case float64:
		if typed >= 1_000 && typed <= 99_999_999 && typed == float64(int64(typed)) {
			return "[REDACTED]"
		}
		return typed
	case json.Number:
		if numeric, err := typed.Int64(); err == nil && numeric >= 1_000 && numeric <= 99_999_999 {
			return "[REDACTED]"
		}
		return typed
	default:
		return value
	}
}

// normalizeAndSanitize first passes arbitrary caller values through JSON so
// concrete maps, slices, and structs cannot bypass recursive secret redaction.
func normalizeAndSanitize(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode job event data: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("normalize job event data: %w", err)
	}
	return sanitizeValue(normalized), nil
}

func (s *Store) durableEventSecrets(ctx context.Context, jobID int64) ([]string, error) {
	var encryptedPhone, encryptedProvider string
	if err := s.db.QueryRowContext(ctx, `
		SELECT profile.yodel_phone_ciphertext, source.config_ciphertext
		FROM jobs AS job
		JOIN profiles AS profile ON profile.id = job.profile_id
		JOIN otp_sources AS source ON source.id = job.otp_source_id
		WHERE job.id = ?
	`, jobID).Scan(&encryptedPhone, &encryptedProvider); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	secrets := make([]string, 0, 8)
	if encryptedPhone != "" {
		plaintext, err := s.encryptor.Decrypt(encryptedPhone)
		if err != nil {
			return nil, fmt.Errorf("load event redaction material: %w", err)
		}
		if value := strings.TrimSpace(string(plaintext)); len(value) >= 4 {
			secrets = append(secrets, value)
		}
	}
	providerJSON, err := s.encryptor.Decrypt(encryptedProvider)
	if err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(providerJSON)))
	decoder.UseNumber()
	var providerData any
	if err := decoder.Decode(&providerData); err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	collectStringLeaves(providerData, &secrets)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets, nil
}

func collectStringLeaves(value any, destination *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			collectStringLeaves(child, destination)
		}
	case []any:
		for _, child := range typed {
			collectStringLeaves(child, destination)
		}
	case string:
		if trimmed := strings.TrimSpace(typed); len(trimmed) >= 4 {
			*destination = append(*destination, trimmed)
		}
	}
}

func redactKnownValue(value any, secrets []string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = redactKnownValue(child, secrets)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = redactKnownValue(child, secrets)
		}
		return typed
	case string:
		return redactKnownText(typed, secrets)
	default:
		return value
	}
}

func redactKnownText(value string, secrets []string) string {
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
