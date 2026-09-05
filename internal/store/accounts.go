package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

var (
	ErrSetupComplete            = errors.New("initial account setup is already complete")
	ErrSetupRequired            = errors.New("initial administrator setup is required")
	ErrProtectedAdmin           = errors.New("the permanent administrator cannot be disabled, demoted, or deleted")
	ErrMemberMustBeDisabled     = errors.New("member account must be disabled before deletion")
	ErrMemberDeleteConfirmation = errors.New("member deletion confirmation does not match")
	ErrMemberHasActiveJobs      = errors.New("member account still has active jobs")
)

type CreateUserInput struct {
	Username           string
	Password           string
	MustChangePassword bool
}

type UserUpdateInput struct {
	Username string
	Status   model.UserStatus
}

func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users)").Scan(&exists); err != nil {
		return false, fmt.Errorf("check account setup: %w", err)
	}
	return exists, nil
}

// SetupAdmin creates the first and only administrator. The INSERT predicate is
// deliberately atomic so concurrent setup submissions cannot create a member
// first or replace the permanent administrator.
func (s *Store) SetupAdmin(ctx context.Context, username, password string) (model.User, error) {
	hasUsers, err := s.HasUsers(ctx)
	if err != nil {
		return model.User{}, err
	}
	if hasUsers {
		return model.User{}, ErrSetupComplete
	}
	display, normalized, err := normalizeUsername(username)
	if err != nil {
		return model.User{}, err
	}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return model.User{}, err
	}
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO users(
			username, username_normalized, password_hash, role, status,
			must_change_password, created_at, updated_at
		)
		SELECT ?, ?, ?, 'admin', 'active', 0, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM users)
	`, display, normalized, passwordHash, formatTime(now), formatTime(now))
	if err != nil {
		if errors.Is(mapWriteError(err), ErrConflict) {
			return model.User{}, ErrSetupComplete
		}
		return model.User{}, fmt.Errorf("create initial administrator: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return model.User{}, fmt.Errorf("read initial administrator result: %w", err)
	}
	if count == 0 {
		return model.User{}, ErrSetupComplete
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.User{}, fmt.Errorf("read initial administrator id: %w", err)
	}
	return s.GetUser(ctx, id)
}

func (s *Store) CreateMember(ctx context.Context, input CreateUserInput) (model.User, error) {
	display, normalized, err := normalizeUsername(input.Username)
	if err != nil {
		return model.User{}, err
	}
	passwordHash, err := auth.HashPassword(input.Password)
	if err != nil {
		return model.User{}, err
	}
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO users(
			username, username_normalized, password_hash, role, status,
			must_change_password, created_at, updated_at
		)
		SELECT ?, ?, ?, 'member', 'active', ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM users WHERE role = 'admin')
	`, display, normalized, passwordHash, input.MustChangePassword, formatTime(now), formatTime(now))
	if err != nil {
		return model.User{}, mapWriteError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return model.User{}, fmt.Errorf("read member creation result: %w", err)
	}
	if count == 0 {
		return model.User{}, ErrSetupRequired
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.User{}, fmt.Errorf("read member id: %w", err)
	}
	return s.GetUser(ctx, id)
}

func (s *Store) GetUser(ctx context.Context, id int64) (model.User, error) {
	user, _, err := getUserWith(ctx, s.db, "id = ?", id)
	return user, err
}

func (s *Store) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, role, status, must_change_password, created_at, updated_at
		FROM users
		ORDER BY CASE role WHEN 'admin' THEN 0 ELSE 1 END, username_normalized, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var result []model.User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return result, nil
}

func (s *Store) UpdateUser(ctx context.Context, id int64, input UserUpdateInput) (model.User, error) {
	display, normalized, err := normalizeUsername(input.Username)
	if err != nil {
		return model.User{}, err
	}
	if !input.Status.Valid() {
		return model.User{}, errors.New("account status must be active or disabled")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.User{}, fmt.Errorf("begin user update: %w", err)
	}
	defer tx.Rollback()
	user, _, err := getUserWith(ctx, tx, "id = ?", id)
	if err != nil {
		return model.User{}, err
	}
	if user.Role == model.RoleAdmin && input.Status != model.UserActive {
		return model.User{}, ErrProtectedAdmin
	}
	now := formatTime(s.now())
	result, err := tx.ExecContext(ctx, `
		UPDATE users
		SET username = ?, username_normalized = ?, status = ?, updated_at = ?
		WHERE id = ?
	`, display, normalized, input.Status, now, id)
	if err != nil {
		return model.User{}, mapUserWriteError(err)
	}
	if err := requireAffected(result); err != nil {
		return model.User{}, err
	}
	if input.Status == model.UserDisabled {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
			return model.User{}, fmt.Errorf("revoke disabled user sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE booking_requests SET schedule_enabled = 0, updated_at = ?
			WHERE user_id = ? AND schedule_enabled = 1
		`, now, id); err != nil {
			return model.User{}, fmt.Errorf("disable user schedules: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET
				cancel_requested = 1,
				status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
				message = CASE WHEN status = 'queued' THEN 'Cancelled because the account was disabled.' ELSE message END,
				finished_at = CASE WHEN status = 'queued' THEN ? ELSE finished_at END,
				updated_at = ?
			WHERE user_id = ? AND status IN ('queued', 'running', 'awaiting_approval')
		`, now, now, id); err != nil {
			return model.User{}, fmt.Errorf("cancel disabled user jobs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return model.User{}, fmt.Errorf("commit user update: %w", err)
	}
	return s.GetUser(ctx, id)
}

// DeleteMember permanently removes a disabled non-administrator and all of
// their owned state. The immediate transaction serializes the status/job
// checks against worker claims, account updates, and terminal transitions.
func (s *Store) DeleteMember(ctx context.Context, id int64, confirmedUsername string) error {
	if id <= 0 {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin member deletion: %w", err)
	}
	defer tx.Rollback()
	user, _, err := getUserWith(ctx, tx, "id = ?", id)
	if err != nil {
		return err
	}
	if user.Role == model.RoleAdmin {
		return ErrProtectedAdmin
	}
	if user.Status != model.UserDisabled {
		return ErrMemberMustBeDisabled
	}
	if confirmedUsername != user.Username {
		return ErrMemberDeleteConfirmation
	}
	var activeJobs bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM jobs
			WHERE user_id = ? AND status IN ('queued', 'running', 'awaiting_approval')
		)
	`, id).Scan(&activeJobs); err != nil {
		return fmt.Errorf("check member jobs before deletion: %w", err)
	}
	if activeJobs {
		return ErrMemberHasActiveJobs
	}
	for _, statement := range []string{
		"DELETE FROM job_decisions WHERE user_id = ?",
		"DELETE FROM job_events WHERE user_id = ?",
		"DELETE FROM jobs WHERE user_id = ?",
		"DELETE FROM booking_requests WHERE user_id = ?",
		"DELETE FROM profiles WHERE user_id = ?",
		"DELETE FROM otp_sources WHERE user_id = ?",
		"DELETE FROM sessions WHERE user_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return fmt.Errorf("delete member-owned state: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM users
		WHERE id = ? AND role = 'member' AND status = 'disabled' AND username = ?
	`, id, confirmedUsername)
	if err != nil {
		return fmt.Errorf("delete member account: %w", mapUserWriteError(err))
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit member deletion: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getUserWith(ctx context.Context, queryer queryRower, predicate string, arguments ...any) (model.User, string, error) {
	row := queryer.QueryRowContext(ctx, `
		SELECT id, username, role, status, must_change_password, created_at, updated_at, password_hash
		FROM users WHERE `+predicate, arguments...)
	var user model.User
	var passwordHash, created, updated string
	if err := row.Scan(&user.ID, &user.Username, &user.Role, &user.Status, &user.MustChangePassword,
		&created, &updated, &passwordHash); errors.Is(err, sql.ErrNoRows) {
		return model.User{}, "", ErrNotFound
	} else if err != nil {
		return model.User{}, "", fmt.Errorf("read user: %w", err)
	}
	var err error
	if user.CreatedAt, err = parseTime(created); err != nil {
		return model.User{}, "", err
	}
	if user.UpdatedAt, err = parseTime(updated); err != nil {
		return model.User{}, "", err
	}
	return user, passwordHash, nil
}

func scanUser(scanner rowScanner) (model.User, error) {
	var user model.User
	var created, updated string
	if err := scanner.Scan(&user.ID, &user.Username, &user.Role, &user.Status,
		&user.MustChangePassword, &created, &updated); errors.Is(err, sql.ErrNoRows) {
		return model.User{}, ErrNotFound
	} else if err != nil {
		return model.User{}, fmt.Errorf("scan user: %w", err)
	}
	var err error
	if user.CreatedAt, err = parseTime(created); err != nil {
		return model.User{}, err
	}
	if user.UpdatedAt, err = parseTime(updated); err != nil {
		return model.User{}, err
	}
	return user, nil
}

func normalizeUsername(username string) (display, normalized string, err error) {
	display = strings.TrimSpace(username)
	normalized, err = auth.NormalizeUsername(display)
	return display, normalized, err
}

func mapUserWriteError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "permanent administrator") {
		return fmt.Errorf("%w: %v", ErrProtectedAdmin, err)
	}
	return mapWriteError(err)
}
