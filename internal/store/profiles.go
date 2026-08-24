package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

type ProfileInput struct {
	Name              string
	BrowserProfile    string
	DefaultVehicle    string
	OTPSourceID       int64
	Headless          bool
	BrowserChannel    string
	BrowserExecutable string
	DefaultTimeoutMS  int
	Enabled           bool
	Credentials       *model.ProfileCredentials
}

func (s *Store) CreateProfile(ctx context.Context, input ProfileInput) (model.Profile, error) {
	profile := profileFromInput(0, input)
	if err := model.ValidateProfile(profile); err != nil {
		return model.Profile{}, err
	}
	if input.Credentials == nil {
		return model.Profile{}, errors.New("Yodel credentials are required when creating a profile")
	}
	if strings.TrimSpace(input.Credentials.Email) == "" || input.Credentials.Password == "" {
		return model.Profile{}, errors.New("Yodel email and password are required")
	}
	email, err := s.encryptor.Encrypt([]byte(strings.TrimSpace(input.Credentials.Email)))
	if err != nil {
		return model.Profile{}, fmt.Errorf("encrypt Yodel email: %w", err)
	}
	password, err := s.encryptor.Encrypt([]byte(input.Credentials.Password))
	if err != nil {
		return model.Profile{}, fmt.Errorf("encrypt Yodel password: %w", err)
	}
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO profiles(
			name, browser_profile, default_vehicle, otp_source_id,
			yodel_email_ciphertext, yodel_password_ciphertext,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, profile.Name, profile.BrowserProfile, profile.DefaultVehicle, profile.OTPSourceID,
		email, password, profile.Headless, profile.BrowserChannel, profile.BrowserExecutable,
		profile.DefaultTimeoutMS, profile.Enabled, formatTime(now), formatTime(now))
	if err != nil {
		return model.Profile{}, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Profile{}, fmt.Errorf("read profile id: %w", err)
	}
	return s.GetProfile(ctx, id)
}

// UpdateProfile retains existing Yodel credentials when Credentials is nil.
// A non-nil value replaces both encrypted fields, including with empty values.
func (s *Store) UpdateProfile(ctx context.Context, id int64, input ProfileInput) (model.Profile, error) {
	profile := profileFromInput(id, input)
	if err := model.ValidateProfile(profile); err != nil {
		return model.Profile{}, err
	}
	args := []any{profile.Name, profile.BrowserProfile, profile.DefaultVehicle,
		profile.OTPSourceID, profile.Headless, profile.BrowserChannel,
		profile.BrowserExecutable, profile.DefaultTimeoutMS, profile.Enabled}
	query := `UPDATE profiles SET name = ?, browser_profile = ?, default_vehicle = ?,
		otp_source_id = ?, headless = ?, browser_channel = ?, browser_executable = ?,
		default_timeout_ms = ?, enabled = ?`
	if input.Credentials != nil {
		if strings.TrimSpace(input.Credentials.Email) == "" || input.Credentials.Password == "" {
			return model.Profile{}, errors.New("Yodel email and password are required")
		}
		email, err := s.encryptor.Encrypt([]byte(strings.TrimSpace(input.Credentials.Email)))
		if err != nil {
			return model.Profile{}, fmt.Errorf("encrypt Yodel email: %w", err)
		}
		password, err := s.encryptor.Encrypt([]byte(input.Credentials.Password))
		if err != nil {
			return model.Profile{}, fmt.Errorf("encrypt Yodel password: %w", err)
		}
		query += ", yodel_email_ciphertext = ?, yodel_password_ciphertext = ?"
		args = append(args, email, password)
	}
	query += `, updated_at = ? WHERE id = ? AND NOT EXISTS (
		SELECT 1 FROM jobs WHERE profile_id = ?
		AND status IN ('queued', 'running', 'awaiting_approval')
	)`
	args = append(args, formatTime(s.now()), id, id)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return model.Profile{}, mapWriteError(err)
	}
	if err := s.classifyGuardedUpdate(ctx, "profiles", id, result); err != nil {
		return model.Profile{}, err
	}
	return s.GetProfile(ctx, id)
}

func (s *Store) GetProfile(ctx context.Context, id int64) (model.Profile, error) {
	return scanProfile(s.db.QueryRowContext(ctx, `
		SELECT id, name, browser_profile, default_vehicle, otp_source_id,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		FROM profiles WHERE id = ?
	`, id))
}

func (s *Store) ListProfiles(ctx context.Context) ([]model.Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, browser_profile, default_vehicle, otp_source_id,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		FROM profiles ORDER BY name, id
	`)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer rows.Close()
	var result []model.Profile
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, profile)
	}
	return result, rows.Err()
}

func (s *Store) GetProfileCredentials(ctx context.Context, id int64) (model.ProfileCredentials, error) {
	var encryptedEmail, encryptedPassword string
	if err := s.db.QueryRowContext(ctx, `
		SELECT yodel_email_ciphertext, yodel_password_ciphertext FROM profiles WHERE id = ?
	`, id).Scan(&encryptedEmail, &encryptedPassword); errors.Is(err, sql.ErrNoRows) {
		return model.ProfileCredentials{}, ErrNotFound
	} else if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("read Yodel credentials: %w", err)
	}
	email, err := s.encryptor.Decrypt(encryptedEmail)
	if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("decrypt Yodel email: %w", err)
	}
	password, err := s.encryptor.Decrypt(encryptedPassword)
	if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("decrypt Yodel password: %w", err)
	}
	return model.ProfileCredentials{Email: string(email), Password: string(password)}, nil
}

func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM profiles WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete profile: %w", mapWriteError(err))
	}
	return requireAffected(result)
}

func profileFromInput(id int64, input ProfileInput) model.Profile {
	return model.Profile{
		ID: id, Name: strings.TrimSpace(input.Name), BrowserProfile: strings.TrimSpace(input.BrowserProfile),
		DefaultVehicle: strings.TrimSpace(input.DefaultVehicle), OTPSourceID: input.OTPSourceID,
		Headless: input.Headless, BrowserChannel: strings.TrimSpace(input.BrowserChannel),
		BrowserExecutable: strings.TrimSpace(input.BrowserExecutable), DefaultTimeoutMS: input.DefaultTimeoutMS,
		Enabled: input.Enabled,
	}
}

func scanProfile(scanner rowScanner) (model.Profile, error) {
	var profile model.Profile
	var created, updated string
	if err := scanner.Scan(&profile.ID, &profile.Name, &profile.BrowserProfile,
		&profile.DefaultVehicle, &profile.OTPSourceID, &profile.Headless,
		&profile.BrowserChannel, &profile.BrowserExecutable, &profile.DefaultTimeoutMS,
		&profile.Enabled, &created, &updated); errors.Is(err, sql.ErrNoRows) {
		return model.Profile{}, ErrNotFound
	} else if err != nil {
		return model.Profile{}, fmt.Errorf("scan profile: %w", err)
	}
	var err error
	if profile.CreatedAt, err = parseTime(created); err != nil {
		return model.Profile{}, err
	}
	if profile.UpdatedAt, err = parseTime(updated); err != nil {
		return model.Profile{}, err
	}
	return profile, nil
}
