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

type jobHistoryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
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
