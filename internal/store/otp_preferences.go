package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func (u UserStore) GetDefaultOTPSource(ctx context.Context) (model.OTPSource, error) {
	if u.userID <= 0 {
		return model.OTPSource{}, ErrUserRequired
	}
	var sourceID int64
	if err := u.store.db.QueryRowContext(ctx, `SELECT source_id FROM user_otp_preferences WHERE user_id = ?`, u.userID).Scan(&sourceID); errors.Is(err, sql.ErrNoRows) {
		return model.OTPSource{}, ErrNotFound
	} else if err != nil {
		return model.OTPSource{}, fmt.Errorf("read default OTP source: %w", err)
	}
	return u.GetOTPSource(ctx, sourceID)
}

func (u UserStore) SetDefaultOTPSource(ctx context.Context, sourceID int64) error {
	if u.userID <= 0 {
		return ErrUserRequired
	}
	result, err := u.store.db.ExecContext(ctx, `
		INSERT INTO user_otp_preferences(user_id, source_id)
		SELECT user_id, id FROM otp_sources WHERE id = ? AND user_id = ?
		ON CONFLICT(user_id) DO UPDATE SET source_id = excluded.source_id
	`, sourceID, u.userID)
	if err != nil {
		return mapWriteError(err)
	}
	return requireAffected(result)
}
