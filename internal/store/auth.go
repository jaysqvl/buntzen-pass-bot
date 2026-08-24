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

func (s *Store) BootstrapAdmin(ctx context.Context, username, password string) (model.Admin, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		username = "admin"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Admin{}, false, fmt.Errorf("begin administrator bootstrap: %w", err)
	}
	defer tx.Rollback()
	admin, _, err := getAdminWith(ctx, tx, username)
	if err == nil {
		return admin, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return model.Admin{}, false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM admins").Scan(&count); err != nil {
		return model.Admin{}, false, fmt.Errorf("count administrators: %w", err)
	}
	if count != 0 {
		return model.Admin{}, false, errors.New("the built-in administrator already exists under another username")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return model.Admin{}, false, err
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO admins(username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)
	`, username, hash, formatTime(now), formatTime(now))
	if err != nil {
		return model.Admin{}, false, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Admin{}, false, fmt.Errorf("read administrator id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Admin{}, false, fmt.Errorf("commit administrator bootstrap: %w", err)
	}
	admin = model.Admin{ID: id, Username: username, CreatedAt: now, UpdatedAt: now}
	return admin, true, nil
}

func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) (model.Admin, bool, error) {
	admin, passwordHash, err := getAdminWith(ctx, s.db, strings.TrimSpace(username))
	if errors.Is(err, ErrNotFound) {
		// Perform equivalent Argon2 work to reduce username-existence timing leakage.
		auth.EqualizePasswordCheck(password)
		return model.Admin{}, false, nil
	}
	if err != nil {
		return model.Admin{}, false, err
	}
	ok, err := auth.VerifyPassword(passwordHash, password)
	if err != nil {
		return model.Admin{}, false, fmt.Errorf("verify administrator password: %w", err)
	}
	return admin, ok, nil
}

func (s *Store) ResetAdminPassword(ctx context.Context, username, newPassword string) error {
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin password reset: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE admins SET password_hash = ?, updated_at = ? WHERE username = ?
	`, hash, formatTime(s.now()), strings.TrimSpace(username))
	if err != nil {
		return fmt.Errorf("reset administrator password: %w", err)
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	// A password reset invalidates every outstanding session.
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
		return fmt.Errorf("invalidate sessions after password reset: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit password reset: %w", err)
	}
	return nil
}

func (s *Store) NewSession(ctx context.Context, adminID int64, lifetime time.Duration) (model.SessionCredentials, error) {
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
		ID: auth.HashToken(token), AdminID: adminID, CSRFTokenHash: auth.HashToken(csrf),
		ExpiresAt: now.Add(lifetime), CreatedAt: now, LastSeenAt: now,
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(id, admin_id, csrf_token_hash, expires_at, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, session.ID, session.AdminID, session.CSRFTokenHash, formatTime(session.ExpiresAt),
		formatTime(session.CreatedAt), formatTime(session.LastSeenAt)); err != nil {
		return model.SessionCredentials{}, fmt.Errorf("create session: %w", err)
	}
	return model.SessionCredentials{Token: token, CSRFToken: csrf, Session: session}, nil
}

func (s *Store) GetSession(ctx context.Context, token string) (model.AuthenticatedSession, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT sessions.id, sessions.admin_id, sessions.csrf_token_hash, sessions.expires_at,
			sessions.created_at, sessions.last_seen_at,
			admins.id, admins.username, admins.created_at, admins.updated_at
		FROM sessions JOIN admins ON admins.id = sessions.admin_id
		WHERE sessions.id = ? AND sessions.expires_at > ?
	`, auth.HashToken(token), formatTime(s.now()))
	var result model.AuthenticatedSession
	var sessionExpiry, sessionCreated, lastSeen, adminCreated, adminUpdated string
	if err := row.Scan(&result.Session.ID, &result.Session.AdminID, &result.Session.CSRFTokenHash,
		&sessionExpiry, &sessionCreated, &lastSeen, &result.Admin.ID, &result.Admin.Username,
		&adminCreated, &adminUpdated); errors.Is(err, sql.ErrNoRows) {
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
	if result.Admin.CreatedAt, err = parseTime(adminCreated); err != nil {
		return model.AuthenticatedSession{}, err
	}
	if result.Admin.UpdatedAt, err = parseTime(adminUpdated); err != nil {
		return model.AuthenticatedSession{}, err
	}
	return result, nil
}

func ValidateCSRF(session model.Session, token string) bool {
	actual := auth.HashToken(token)
	return subtle.ConstantTimeCompare([]byte(actual), []byte(session.CSRFTokenHash)) == 1
}

func (s *Store) TouchSession(ctx context.Context, token string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ? WHERE id = ? AND expires_at > ?
	`, formatTime(s.now()), auth.HashToken(token), formatTime(s.now()))
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
	rateKey = strings.TrimSpace(rateKey)
	if rateKey == "" {
		return errors.New("login rate key is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin login-attempt transaction: %w", err)
	}
	defer tx.Rollback()
	if succeeded {
		if _, err := tx.ExecContext(ctx, "DELETE FROM login_attempts WHERE rate_key = ?", rateKey); err != nil {
			return fmt.Errorf("clear login attempts: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `
		INSERT INTO login_attempts(rate_key, succeeded, attempted_at) VALUES (?, 0, ?)
	`, rateKey, formatTime(s.now())); err != nil {
		return fmt.Errorf("record login attempt: %w", err)
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

func getAdminWith(ctx context.Context, queryer queryRower, username string) (model.Admin, string, error) {
	var admin model.Admin
	var passwordHash string
	var created, updated string
	if err := queryer.QueryRowContext(ctx, `
		SELECT id, username, password_hash, created_at, updated_at FROM admins WHERE username = ?
	`, username).Scan(&admin.ID, &admin.Username, &passwordHash, &created, &updated); errors.Is(err, sql.ErrNoRows) {
		return model.Admin{}, "", ErrNotFound
	} else if err != nil {
		return model.Admin{}, "", fmt.Errorf("read administrator: %w", err)
	}
	var err error
	if admin.CreatedAt, err = parseTime(created); err != nil {
		return model.Admin{}, "", err
	}
	if admin.UpdatedAt, err = parseTime(updated); err != nil {
		return model.Admin{}, "", err
	}
	return admin, passwordHash, nil
}
