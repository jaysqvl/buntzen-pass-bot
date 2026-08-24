package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

var (
	ErrSetupComplete            = errors.New("initial account setup is already complete")
	ErrSetupRequired            = errors.New("initial administrator setup is required")
	ErrProtectedAdmin           = errors.New("the permanent administrator cannot be disabled, demoted, or deleted")
	ErrPasswordUnchanged        = errors.New("new password must differ from the current password")
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

func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (model.User, bool, error) {
	user, _, ok, err := s.authenticatePassword(ctx, username, password)
	return user, ok, err
}

// AuthenticateAndCreateSession binds session issuance to the exact password
// hash that was verified. The conditional INSERT prevents an old password from
// minting a new session if a password change or host recovery commits between
// verification and session creation.
func (s *Store) AuthenticateAndCreateSession(
	ctx context.Context,
	username, password string,
	lifetime time.Duration,
) (model.User, model.SessionCredentials, bool, error) {
	user, passwordHash, ok, err := s.authenticatePassword(ctx, username, password)
	if err != nil || !ok {
		return model.User{}, model.SessionCredentials{}, ok, err
	}
	credentials, err := s.newSession(ctx, user.ID, lifetime, &passwordHash)
	if errors.Is(err, ErrNotFound) {
		return model.User{}, model.SessionCredentials{}, false, nil
	}
	if err != nil {
		return model.User{}, model.SessionCredentials{}, false, err
	}
	return user, credentials, true, nil
}

func (s *Store) authenticatePassword(ctx context.Context, username, password string) (model.User, string, bool, error) {
	normalized, err := auth.NormalizeUsername(username)
	if err != nil {
		auth.EqualizePasswordCheck(password)
		return model.User{}, "", false, nil
	}
	user, passwordHash, err := getUserWith(ctx, s.db, "username_normalized = ?", normalized)
	if errors.Is(err, ErrNotFound) {
		auth.EqualizePasswordCheck(password)
		return model.User{}, "", false, nil
	}
	if err != nil {
		return model.User{}, "", false, err
	}
	ok, err := auth.VerifyPassword(passwordHash, password)
	if err != nil {
		return model.User{}, "", false, fmt.Errorf("verify account password: %w", err)
	}
	if !ok || user.Status != model.UserActive {
		return model.User{}, "", false, nil
	}
	return user, passwordHash, true, nil
}

// ResetUserPassword is the administrator-managed reset operation. Every
// session for the target is revoked, including sessions on other browsers.
func (s *Store) ResetUserPassword(ctx context.Context, id int64, password string, mustChange bool) error {
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin password reset: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE users
		SET password_hash = ?, must_change_password = ?, updated_at = ?
		WHERE id = ?
	`, passwordHash, mustChange, formatTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("reset user password: %w", err)
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
		return fmt.Errorf("revoke sessions after password reset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit password reset: %w", err)
	}
	return nil
}

// ChangeUserPassword verifies the current password before replacing it. A
// successful change clears the reset marker and revokes every current session.
func (s *Store) ChangeUserPassword(ctx context.Context, id int64, currentPassword, newPassword string) (bool, error) {
	user, passwordHash, err := getUserWith(ctx, s.db, "id = ?", id)
	if err != nil {
		return false, err
	}
	if user.Status != model.UserActive {
		return false, nil
	}
	ok, err := auth.VerifyPassword(passwordHash, currentPassword)
	if err != nil {
		return false, fmt.Errorf("verify current password: %w", err)
	}
	if !ok {
		return false, nil
	}
	if len(currentPassword) == len(newPassword) &&
		subtle.ConstantTimeCompare([]byte(currentPassword), []byte(newPassword)) == 1 {
		return false, ErrPasswordUnchanged
	}
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin password change: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE users
		SET password_hash = ?, must_change_password = 0, updated_at = ?
		WHERE id = ? AND status = 'active' AND password_hash = ?
	`, newHash, formatTime(s.now()), id, passwordHash)
	if err != nil {
		return false, fmt.Errorf("change password: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read password change result: %w", err)
	}
	if count == 0 {
		// A concurrent reset/change or account disable won the race. Treat the
		// submitted current password as stale rather than overwriting it.
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", id); err != nil {
		return false, fmt.Errorf("revoke sessions after password change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit password change: %w", err)
	}
	return true, nil
}

// ResetAdministratorPassword is the host recovery path. It targets the sole
// administrator by role, so recovery still works after an in-app rename.
func (s *Store) ResetAdministratorPassword(ctx context.Context, password string) (model.User, error) {
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return model.User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.User{}, fmt.Errorf("begin administrator recovery: %w", err)
	}
	defer tx.Rollback()
	admin, _, err := getUserWith(ctx, tx, "role = 'admin'")
	if err != nil {
		return model.User{}, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE users
		SET password_hash = ?, status = 'active', must_change_password = 0, updated_at = ?
		WHERE id = ? AND role = 'admin'
	`, passwordHash, formatTime(s.now()), admin.ID)
	if err != nil {
		return model.User{}, mapUserWriteError(err)
	}
	if err := requireAffected(result); err != nil {
		return model.User{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", admin.ID); err != nil {
		return model.User{}, fmt.Errorf("revoke administrator sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.User{}, fmt.Errorf("commit administrator recovery: %w", err)
	}
	return s.GetUser(ctx, admin.ID)
}

func (s *Store) NewSession(ctx context.Context, userID int64, lifetime time.Duration) (model.SessionCredentials, error) {
	return s.newSession(ctx, userID, lifetime, nil)
}

func (s *Store) newSession(
	ctx context.Context,
	userID int64,
	lifetime time.Duration,
	expectedPasswordHash *string,
) (model.SessionCredentials, error) {
	if lifetime <= 0 || lifetime > 31*24*time.Hour {
		return model.SessionCredentials{}, errors.New("session lifetime must be between zero and 31 days")
	}
	token, err := auth.NewToken()
	if err != nil {
		return model.SessionCredentials{}, err
	}
	csrf, err := auth.NewToken()
	if err != nil {
		return model.SessionCredentials{}, err
	}
	now := s.now()
	session := model.Session{
		ID: auth.HashToken(token), UserID: userID, CSRFTokenHash: auth.HashToken(csrf),
		ExpiresAt: now.Add(lifetime), CreatedAt: now, LastSeenAt: now,
	}
	query := `
		INSERT INTO sessions(id, user_id, csrf_token_hash, expires_at, created_at, last_seen_at)
		SELECT ?, id, ?, ?, ?, ? FROM users WHERE id = ? AND status = 'active'
	`
	arguments := []any{session.ID, session.CSRFTokenHash, formatTime(session.ExpiresAt),
		formatTime(session.CreatedAt), formatTime(session.LastSeenAt), session.UserID}
	if expectedPasswordHash != nil {
		query += " AND password_hash = ?"
		arguments = append(arguments, *expectedPasswordHash)
	}
	result, err := s.db.ExecContext(ctx, query, arguments...)
	if err != nil {
		return model.SessionCredentials{}, fmt.Errorf("create session: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return model.SessionCredentials{}, fmt.Errorf("read session creation result: %w", err)
	}
	if count == 0 {
		return model.SessionCredentials{}, ErrNotFound
	}
	return model.SessionCredentials{Token: token, CSRFToken: csrf, Session: session}, nil
}

func (s *Store) GetSession(ctx context.Context, token string) (model.AuthenticatedSession, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT sessions.id, sessions.user_id, sessions.csrf_token_hash, sessions.expires_at,
			sessions.created_at, sessions.last_seen_at,
			users.id, users.username, users.role, users.status, users.must_change_password,
			users.created_at, users.updated_at
		FROM sessions JOIN users ON users.id = sessions.user_id
		WHERE sessions.id = ? AND sessions.expires_at > ? AND users.status = 'active'
	`, auth.HashToken(token), formatTime(s.now()))
	var result model.AuthenticatedSession
	var sessionExpiry, sessionCreated, lastSeen, userCreated, userUpdated string
	if err := row.Scan(&result.Session.ID, &result.Session.UserID, &result.Session.CSRFTokenHash,
		&sessionExpiry, &sessionCreated, &lastSeen, &result.User.ID, &result.User.Username,
		&result.User.Role, &result.User.Status, &result.User.MustChangePassword,
		&userCreated, &userUpdated); errors.Is(err, sql.ErrNoRows) {
		return model.AuthenticatedSession{}, ErrNotFound
	} else if err != nil {
		return model.AuthenticatedSession{}, fmt.Errorf("read session: %w", err)
	}
	var err error
	if result.Session.ExpiresAt, err = parseTime(sessionExpiry); err != nil {
		return model.AuthenticatedSession{}, err
	}
	if result.Session.CreatedAt, err = parseTime(sessionCreated); err != nil {
		return model.AuthenticatedSession{}, err
	}
	if result.Session.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return model.AuthenticatedSession{}, err
	}
	if result.User.CreatedAt, err = parseTime(userCreated); err != nil {
		return model.AuthenticatedSession{}, err
	}
	if result.User.UpdatedAt, err = parseTime(userUpdated); err != nil {
		return model.AuthenticatedSession{}, err
	}
	return result, nil
}

func ValidateCSRF(session model.Session, token string) bool {
	actual := auth.HashToken(token)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(session.CSRFTokenHash)) == 1
}

func (s *Store) TouchSession(ctx context.Context, token string) error {
	now := formatTime(s.now())
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ?
		WHERE id = ? AND expires_at > ?
		AND EXISTS (SELECT 1 FROM users WHERE users.id = sessions.user_id AND users.status = 'active')
	`, now, auth.HashToken(token), now)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return requireAffected(result)
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", auth.HashToken(token))
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", formatTime(s.now()))
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) RecordLoginAttempt(ctx context.Context, rateKey string, succeeded bool) error {
	return s.RecordLoginAttempts(ctx, []string{rateKey}, succeeded)
}

func (s *Store) RecordLoginAttempts(ctx context.Context, rateKeys []string, succeeded bool) error {
	unique := make(map[string]struct{}, len(rateKeys))
	for _, rateKey := range rateKeys {
		rateKey = strings.TrimSpace(rateKey)
		if rateKey == "" || len(rateKey) > 256 {
			return errors.New("login rate key must contain at most 256 characters")
		}
		unique[rateKey] = struct{}{}
	}
	if len(unique) == 0 || len(unique) > 8 {
		return errors.New("between one and eight login rate keys are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin login-attempt transaction: %w", err)
	}
	defer tx.Rollback()
	if succeeded {
		for rateKey := range unique {
			if _, err := tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE rate_key = ?", rateKey); err != nil {
				return fmt.Errorf("clear login attempts: %w", err)
			}
		}
	} else {
		for rateKey := range unique {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO login_attempts(rate_key, succeeded, attempted_at) VALUES (?, 0, ?)
			`, rateKey, formatTime(s.now())); err != nil {
				return fmt.Errorf("record login attempt: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE attempted_at < ?",
		formatTime(s.now().Add(-24*time.Hour))); err != nil {
		return fmt.Errorf("purge old login attempts: %w", err)
	}
	return tx.Commit()
}

// LoginRateLimit reports whether another login may be attempted. retryAt is
// non-zero only when the key has reached maxFailures inside the rolling window.
func (s *Store) LoginRateLimit(
	ctx context.Context,
	rateKey string,
	now time.Time,
	window time.Duration,
	maxFailures int,
) (allowed bool, retryAt time.Time, err error) {
	if window <= 0 || maxFailures <= 0 {
		return false, time.Time{}, errors.New("rate-limit window and maximum must be positive")
	}
	var count int
	var oldest sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*), min(attempted_at) FROM login_attempts
		WHERE rate_key = ? AND succeeded = 0 AND attempted_at > ?
	`, strings.TrimSpace(rateKey), formatTime(now.Add(-window))).Scan(&count, &oldest); err != nil {
		return false, time.Time{}, fmt.Errorf("read login rate limit: %w", err)
	}
	if count < maxFailures || !oldest.Valid {
		return true, time.Time{}, nil
	}
	first, err := parseTime(oldest.String)
	if err != nil {
		return false, time.Time{}, err
	}
	return false, first.Add(window), nil
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
