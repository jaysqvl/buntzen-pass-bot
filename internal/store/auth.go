package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

var ErrPasswordUnchanged = errors.New("new password must differ from the current password")

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
