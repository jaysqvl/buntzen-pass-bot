package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

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
