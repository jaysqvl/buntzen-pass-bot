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
