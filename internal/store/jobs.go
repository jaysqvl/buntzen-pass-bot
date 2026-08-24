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

func (s *Store) EnqueueJob(ctx context.Context, params EnqueueJobParams) (model.Job, error) {
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
	if params.BookingRequestID != nil {
		var bookingProfileID int64
		var bookingEnabled bool
		if err := tx.QueryRowContext(ctx,
			"SELECT profile_id, enabled FROM booking_requests WHERE id = ?", *params.BookingRequestID,
		).Scan(&bookingProfileID, &bookingEnabled); errors.Is(err, sql.ErrNoRows) {
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
	}
	if profileID <= 0 {
		return model.Job{}, errors.New("profile is required")
	}
	var sourceID int64
	if err := tx.QueryRowContext(ctx,
		"SELECT otp_source_id FROM profiles WHERE id = ? AND enabled = 1", profileID,
	).Scan(&sourceID); errors.Is(err, sql.ErrNoRows) {
		return model.Job{}, errors.New("profile does not exist or is disabled")
	} else if err != nil {
		return model.Job{}, fmt.Errorf("read profile OTP source: %w", err)
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(
			booking_request_id, profile_id, otp_source_id, command, run_mode, status,
			due_at, expires_at, dedup_key, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?)
	`, params.BookingRequestID, profileID, sourceID, params.Command, params.RunMode,
		formatTime(params.DueAt), optionalTimeValue(params.ExpiresAt), strings.TrimSpace(params.DedupKey),
		formatTime(now), formatTime(now))
	if err != nil {
		return model.Job{}, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Job{}, fmt.Errorf("read job id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Job{}, fmt.Errorf("commit enqueue transaction: %w", err)
	}
	return s.GetJob(ctx, id)
}

func (s *Store) ClaimNextDueJob(ctx context.Context, owner string) (model.Job, error) {
	return s.ClaimNextDueJobAt(ctx, owner, s.now())
}

// ClaimNextDueJobAt atomically selects one due job whose profile and OTP source
// are not in use, then leases both by moving the job to running.
func (s *Store) ClaimNextDueJobAt(ctx context.Context, owner string, now time.Time) (model.Job, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return model.Job{}, errors.New("job owner is required")
	}
	if _, err := s.ExpireStaleJobs(ctx, now); err != nil {
		return model.Job{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT candidate.id
			FROM jobs AS candidate
			JOIN profiles AS profile ON profile.id = candidate.profile_id
			LEFT JOIN booking_requests AS booking ON booking.id = candidate.booking_request_id
			WHERE candidate.status = 'queued' AND candidate.cancel_requested = 0 AND candidate.due_at <= ?
			AND (candidate.expires_at IS NULL OR candidate.expires_at > ?)
			AND profile.enabled = 1 AND profile.otp_source_id = candidate.otp_source_id
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
		UPDATE jobs SET status = 'running', owner = ?, started_at = COALESCE(started_at, ?), updated_at = ?
		WHERE id = (SELECT id FROM candidate) AND status = 'queued'
		RETURNING `+jobColumns, formatTime(now), formatTime(now), owner, formatTime(now), formatTime(now))
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

func (s *Store) TransitionJob(
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
	current, err := s.GetJob(ctx, id)
	if err != nil {
		return model.Job{}, err
	}
	if to == model.JobOutcomeUnknown && current.ConfirmationStartedAt == nil && !update.ConfirmationStarted {
		return model.Job{}, errors.New("outcome_unknown requires final confirmation to have started")
	}

	now := s.now()
	sets := []string{"status = ?", "message = ?", "exit_code = ?", "updated_at = ?"}
	args := []any{to, truncate(sanitizeText(update.Message), 1000), update.ExitCode, formatTime(now)}
	if update.ConfirmationStarted {
		sets = append(sets, "confirmation_started_at = COALESCE(confirmation_started_at, ?)")
		args = append(args, formatTime(now))
	}
	if to == model.JobRunning {
		sets = append(sets, "started_at = COALESCE(started_at, ?)")
		args = append(args, formatTime(now))
	}
	if to.Terminal() {
		sets = append(sets, "finished_at = ?", "owner = ''")
		args = append(args, formatTime(now))
	}
	placeholders := make([]string, len(expected))
	for index, status := range expected {
		placeholders[index] = "?"
		args = append(args, status)
	}
	query := "UPDATE jobs SET " + strings.Join(sets, ", ") +
		" WHERE id = ? AND status IN (" + strings.Join(placeholders, ",") + ")"
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
	return s.GetJob(ctx, id)
}

func (s *Store) MarkConfirmationStarted(ctx context.Context, id int64) error {
	now := formatTime(s.now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET confirmation_started_at = COALESCE(confirmation_started_at, ?), updated_at = ?
		WHERE id = ? AND status IN ('running', 'awaiting_approval')
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
// Queued work is cancelled immediately; an active owner observes the flag and
// cooperatively stops its Python/browser process.
func (s *Store) RequestJobCancellation(ctx context.Context, id int64) error {
	now := formatTime(s.now())
	var status model.JobStatus
	err := s.db.QueryRowContext(ctx, `
		UPDATE jobs SET
			cancel_requested = 1,
			status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
			message = CASE WHEN status = 'queued' THEN 'Cancelled before execution.' ELSE message END,
			finished_at = CASE WHEN status = 'queued' THEN ? ELSE finished_at END,
			updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running', 'awaiting_approval', 'cancelled')
		RETURNING status
	`, now, now, id).Scan(&status)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("request job cancellation: %w", err)
	}
	// Distinguish a missing job from an existing terminal job. The update itself
	// remains a single compare-and-set statement, so a concurrent claim or
	// completion cannot be overwritten based on a stale status read.
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM jobs WHERE id = ?", id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read cancellation target: %w", err)
	}
	return ErrTransitionConflict
}

func (s *Store) JobCancellationRequested(ctx context.Context, id int64) (bool, error) {
	var requested bool
	if err := s.db.QueryRowContext(ctx, "SELECT cancel_requested FROM jobs WHERE id = ?", id).Scan(&requested); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, fmt.Errorf("read cancellation request: %w", err)
	}
	return requested, nil
}

func (s *Store) GetJob(ctx context.Context, id int64) (model.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id))
}

func (s *Store) ListJobs(ctx context.Context, limit int) ([]model.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+jobColumns+" FROM jobs ORDER BY id DESC LIMIT ?", limit)
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

func (s *Store) RecoverInterruptedJobs(ctx context.Context) (int64, error) {
	now := formatTime(s.now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET
			status = CASE WHEN confirmation_started_at IS NULL THEN 'interrupted' ELSE 'outcome_unknown' END,
			message = CASE WHEN confirmation_started_at IS NULL
				THEN 'Interrupted by control-plane restart.'
				ELSE 'Control plane restarted after final confirmation began; outcome requires review.' END,
			owner = '', finished_at = ?, updated_at = ?
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

func (s *Store) ExpireStaleJobs(ctx context.Context, now time.Time) (int64, error) {
	formatted := formatTime(now)
	result, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'failed', message = 'The booking window expired before this job could start.',
			owner = '', finished_at = ?, updated_at = ?
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

const jobColumns = `id, booking_request_id, profile_id, otp_source_id, command, run_mode,
	status, due_at, expires_at, dedup_key, owner, cancel_requested, message, exit_code, confirmation_started_at,
	created_at, updated_at, started_at, finished_at`

func scanJob(scanner rowScanner) (model.Job, error) {
	var job model.Job
	var bookingID sql.NullInt64
	var exitCode sql.NullInt64
	var dueAt, createdAt, updatedAt string
	var expiresAt, confirmationAt, startedAt, finishedAt sql.NullString
	if err := scanner.Scan(&job.ID, &bookingID, &job.ProfileID, &job.OTPSourceID,
		&job.Command, &job.RunMode, &job.Status, &dueAt, &expiresAt, &job.DedupKey, &job.Owner,
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

func (s *Store) AppendJobEvent(ctx context.Context, input JobEventInput) (model.JobEvent, error) {
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
		INSERT INTO job_events(job_id, level, kind, message, data_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, input.JobID, level, kind, message, string(encoded), formatTime(now))
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("append job event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("read job event id: %w", err)
	}
	return model.JobEvent{ID: id, JobID: input.JobID, Level: level, Kind: kind,
		Message: message, DataJSON: string(encoded), CreatedAt: now}, nil
}

func (s *Store) ListJobEvents(ctx context.Context, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, level, kind, message, data_json, created_at
		FROM job_events WHERE job_id = ? AND id > ? ORDER BY id LIMIT ?
	`, jobID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list job events: %w", err)
	}
	defer rows.Close()
	var result []model.JobEvent
	for rows.Next() {
		var event model.JobEvent
		var created string
		if err := rows.Scan(&event.ID, &event.JobID, &event.Level, &event.Kind,
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

func (s *Store) RecordJobDecision(ctx context.Context, jobID int64, decision model.ApprovalDecision) (model.JobDecision, error) {
	if !decision.Valid() {
		return model.JobDecision{}, errors.New("invalid job decision")
	}
	// The INSERT is the state check and unique-decision race in one SQLite
	// statement. Concurrent approve/cancel requests therefore select exactly one
	// durable winner instead of racing through separate reads in deferred
	// transactions.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO job_decisions(job_id, decision, created_at)
		SELECT ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM jobs WHERE id = ? AND status = 'awaiting_approval'
		)
		ON CONFLICT(job_id) DO NOTHING
	`, jobID, decision, formatTime(s.now()), jobID); err != nil {
		return model.JobDecision{}, fmt.Errorf("record job decision: %w", err)
	}
	recorded, err := s.GetJobDecision(ctx, jobID)
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
	if err := s.db.QueryRowContext(ctx, "SELECT status FROM jobs WHERE id = ?", jobID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return model.JobDecision{}, ErrNotFound
	} else if err != nil {
		return model.JobDecision{}, fmt.Errorf("read decision job: %w", err)
	}
	return model.JobDecision{}, ErrTransitionConflict
}

func (s *Store) GetJobDecision(ctx context.Context, jobID int64) (model.JobDecision, error) {
	var decision model.JobDecision
	var created string
	if err := s.db.QueryRowContext(ctx,
		"SELECT job_id, decision, created_at FROM job_decisions WHERE job_id = ?", jobID,
	).Scan(&decision.JobID, &decision.Decision, &created); errors.Is(err, sql.ErrNoRows) {
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
	var encryptedEmail, encryptedPassword, encryptedProvider string
	if err := s.db.QueryRowContext(ctx, `
		SELECT profile.yodel_email_ciphertext, profile.yodel_password_ciphertext, source.config_ciphertext
		FROM jobs AS job
		JOIN profiles AS profile ON profile.id = job.profile_id
		JOIN otp_sources AS source ON source.id = job.otp_source_id
		WHERE job.id = ?
	`, jobID).Scan(&encryptedEmail, &encryptedPassword, &encryptedProvider); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	secrets := make([]string, 0, 8)
	for _, encrypted := range []string{encryptedEmail, encryptedPassword} {
		plaintext, err := s.encryptor.Decrypt(encrypted)
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
