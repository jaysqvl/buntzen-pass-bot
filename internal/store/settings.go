package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func (u UserStore) GetLakeSettings(ctx context.Context, lakeID string) (model.LakeSettings, error) {
	if u.userID <= 0 {
		return model.LakeSettings{}, ErrUserRequired
	}
	lake, err := destinations.Resolve(lakeID)
	if err != nil {
		return model.LakeSettings{}, err
	}
	var settings model.LakeSettings
	var passOrder, updated string
	err = u.store.db.QueryRowContext(ctx, `
		SELECT user_id, lake_id, timezone, release_time, release_days_before,
			all_day_pass_url, half_day_pass_url, pass_order, vehicle_keyword, updated_at
		FROM lake_settings WHERE user_id = ? AND lake_id = ?
	`, u.userID, lake.ID).Scan(&settings.UserID, &settings.LakeID, &settings.Timezone,
		&settings.ReleaseTime, &settings.ReleaseDaysBefore, &settings.AllDayPassURL,
		&settings.HalfDayPassURL, &passOrder, &settings.VehicleKeyword, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.LakeSettings{}, ErrNotFound
	}
	if err != nil {
		return model.LakeSettings{}, fmt.Errorf("read lake settings: %w", err)
	}
	for _, pass := range strings.Split(passOrder, ",") {
		settings.PreferredPasses = append(settings.PreferredPasses, model.PassType(pass))
	}
	settings.UpdatedAt, err = parseTime(updated)
	return settings, err
}

func (u UserStore) SaveLakeSettings(ctx context.Context, settings model.LakeSettings) (model.LakeSettings, error) {
	if u.userID <= 0 {
		return model.LakeSettings{}, ErrUserRequired
	}
	lake, err := destinations.Resolve(settings.LakeID)
	if err != nil {
		return model.LakeSettings{}, err
	}
	settings.UserID = u.userID
	settings.LakeID = lake.ID
	settings.VehicleKeyword = strings.TrimSpace(settings.VehicleKeyword)
	settings.Timezone = strings.TrimSpace(settings.Timezone)
	settings.ReleaseTime = strings.TrimSpace(settings.ReleaseTime)
	settings.AllDayPassURL = strings.TrimSpace(settings.AllDayPassURL)
	settings.HalfDayPassURL = strings.TrimSpace(settings.HalfDayPassURL)
	if err := settings.Validate(); err != nil {
		return model.LakeSettings{}, err
	}
	_, err = u.store.db.ExecContext(ctx, `
		INSERT INTO lake_settings(user_id, lake_id, timezone, release_time,
			release_days_before, all_day_pass_url, half_day_pass_url, pass_order, vehicle_keyword, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, lake_id) DO UPDATE SET
			timezone = excluded.timezone, release_time = excluded.release_time,
			release_days_before = excluded.release_days_before,
			all_day_pass_url = excluded.all_day_pass_url, half_day_pass_url = excluded.half_day_pass_url,
			pass_order = excluded.pass_order, vehicle_keyword = excluded.vehicle_keyword,
			updated_at = excluded.updated_at
	`, u.userID, settings.LakeID, settings.Timezone, settings.ReleaseTime,
		settings.ReleaseDaysBefore, settings.AllDayPassURL, settings.HalfDayPassURL,
		passOrderCSV(settings.PreferredPasses), settings.VehicleKeyword, formatTime(u.store.now()))
	if err != nil {
		return model.LakeSettings{}, mapWriteError(err)
	}
	return u.GetLakeSettings(ctx, lake.ID)
}

func (u UserStore) ResetLakeSettings(ctx context.Context, lakeID string) error {
	if u.userID <= 0 {
		return ErrUserRequired
	}
	lake, err := destinations.Resolve(lakeID)
	if err != nil {
		return err
	}
	_, err = u.store.db.ExecContext(ctx, `DELETE FROM lake_settings WHERE user_id = ? AND lake_id = ?`, u.userID, lake.ID)
	if err != nil {
		return fmt.Errorf("reset lake settings: %w", err)
	}
	return nil
}

func (u UserStore) GetAccountSettings(ctx context.Context) (model.AccountSettings, error) {
	if u.userID <= 0 {
		return model.AccountSettings{}, ErrUserRequired
	}
	var settings model.AccountSettings
	var updated string
	err := u.store.db.QueryRowContext(ctx, `
		SELECT user_id, headless, browser_channel, default_timeout_ms,
			prep_minutes_before, auth_deadline_minutes_before, poll_deadline_seconds,
			poll_min_seconds, poll_max_seconds, updated_at
		FROM account_settings WHERE user_id = ?
	`, u.userID).Scan(&settings.UserID, &settings.Headless, &settings.BrowserChannel,
		&settings.DefaultTimeoutMS, &settings.PrepMinutesBefore,
		&settings.AuthDeadlineMinutesBefore, &settings.PollDeadlineSeconds,
		&settings.PollMinSeconds, &settings.PollMaxSeconds, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		settings = model.DefaultAccountSettings()
		settings.UserID = u.userID
		return settings, nil
	}
	if err != nil {
		return model.AccountSettings{}, fmt.Errorf("read account settings: %w", err)
	}
	settings.UpdatedAt, err = parseTime(updated)
	return settings, err
}

func (u UserStore) SaveAccountSettings(ctx context.Context, settings model.AccountSettings) (model.AccountSettings, error) {
	if u.userID <= 0 {
		return model.AccountSettings{}, ErrUserRequired
	}
	settings.UserID = u.userID
	settings.BrowserChannel = strings.ToLower(strings.TrimSpace(settings.BrowserChannel))
	if err := settings.Validate(); err != nil {
		return model.AccountSettings{}, err
	}
	_, err := u.store.db.ExecContext(ctx, `
		INSERT INTO account_settings(user_id, headless, browser_channel, default_timeout_ms,
			prep_minutes_before, auth_deadline_minutes_before, poll_deadline_seconds,
			poll_min_seconds, poll_max_seconds, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET headless = excluded.headless,
			browser_channel = excluded.browser_channel, default_timeout_ms = excluded.default_timeout_ms,
			prep_minutes_before = excluded.prep_minutes_before,
			auth_deadline_minutes_before = excluded.auth_deadline_minutes_before,
			poll_deadline_seconds = excluded.poll_deadline_seconds,
			poll_min_seconds = excluded.poll_min_seconds, poll_max_seconds = excluded.poll_max_seconds,
			updated_at = excluded.updated_at
	`, u.userID, settings.Headless, settings.BrowserChannel, settings.DefaultTimeoutMS,
		settings.PrepMinutesBefore, settings.AuthDeadlineMinutesBefore, settings.PollDeadlineSeconds,
		settings.PollMinSeconds, settings.PollMaxSeconds, formatTime(u.store.now()))
	if err != nil {
		return model.AccountSettings{}, mapWriteError(err)
	}
	return u.GetAccountSettings(ctx)
}
