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
	DefaultVehicle    string
	OTPSourceID       int64
	Headless          bool
	BrowserChannel    string
	BrowserExecutable string
	DefaultTimeoutMS  int
	Enabled           bool
	Credentials       *model.ProfileCredentials
}

// ErrYodelPhoneRequired is returned when a legacy profile has been migrated
// without copying its former email credential into Yodel's mobile-number
// field. The operator must explicitly provide a mobile number before the
// profile can be enabled or run again.
var ErrYodelPhoneRequired = errors.New("profile requires its Yodel mobile number to be re-entered before it can be enabled or run")

func (s *Store) CreateProfile(ctx context.Context, userID int64, input ProfileInput) (model.Profile, error) {
	if userID <= 0 {
		return model.Profile{}, ErrUserRequired
	}
	profile := profileFromInput(0, userID, input)
	if err := profile.Validate(); err != nil {
		return model.Profile{}, err
	}
	if input.Credentials == nil {
		return model.Profile{}, errors.New("Yodel mobile number is required when creating a profile")
	}
	phone, err := normalizeYodelPhone(input.Credentials.Phone)
	if err != nil {
		return model.Profile{}, err
	}
	encryptedPhone, err := s.encryptor.Encrypt([]byte(phone))
	if err != nil {
		return model.Profile{}, fmt.Errorf("encrypt Yodel mobile number: %w", err)
	}
	if len(encryptedPhone) > MaxYodelCredentialCiphertextBytes {
		return model.Profile{}, errors.New("encrypted Yodel mobile number is too large")
	}
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO profiles(
			user_id, name, default_vehicle, otp_source_id,
			yodel_phone_ciphertext,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, userID, profile.Name, profile.DefaultVehicle, profile.OTPSourceID,
		encryptedPhone, profile.Headless, profile.BrowserChannel, profile.BrowserExecutable,
		profile.DefaultTimeoutMS, profile.Enabled, formatTime(now), formatTime(now))
	if err != nil {
		return model.Profile{}, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Profile{}, fmt.Errorf("read profile id: %w", err)
	}
	return s.GetProfile(ctx, userID, id)
}

// UpdateProfile retains the existing Yodel mobile number when Credentials is nil.
// A non-nil value replaces the encrypted mobile number.
func (s *Store) UpdateProfile(ctx context.Context, userID, id int64, input ProfileInput) (model.Profile, error) {
	if userID <= 0 {
		return model.Profile{}, ErrUserRequired
	}
	profile := profileFromInput(id, userID, input)
	if err := profile.Validate(); err != nil {
		return model.Profile{}, err
	}
	if profile.Enabled && input.Credentials == nil {
		var hasPhone bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT yodel_phone_ciphertext <> '' FROM profiles WHERE id = ? AND user_id = ?
		`, id, userID).Scan(&hasPhone); errors.Is(err, sql.ErrNoRows) {
			return model.Profile{}, ErrNotFound
		} else if err != nil {
			return model.Profile{}, fmt.Errorf("check Yodel mobile number: %w", err)
		}
		if !hasPhone {
			return model.Profile{}, ErrYodelPhoneRequired
		}
	}
	args := []any{profile.Name, profile.DefaultVehicle,
		profile.OTPSourceID, profile.Headless, profile.BrowserChannel,
		profile.BrowserExecutable, profile.DefaultTimeoutMS, profile.Enabled}
	query := `UPDATE profiles SET name = ?, default_vehicle = ?,
		otp_source_id = ?, headless = ?, browser_channel = ?, browser_executable = ?,
		default_timeout_ms = ?, enabled = ?`
	if input.Credentials != nil {
		phone, err := normalizeYodelPhone(input.Credentials.Phone)
		if err != nil {
			return model.Profile{}, err
		}
		encryptedPhone, err := s.encryptor.Encrypt([]byte(phone))
		if err != nil {
			return model.Profile{}, fmt.Errorf("encrypt Yodel mobile number: %w", err)
		}
		if len(encryptedPhone) > MaxYodelCredentialCiphertextBytes {
			return model.Profile{}, errors.New("encrypted Yodel mobile number is too large")
		}
		query += ", yodel_phone_ciphertext = ?"
		args = append(args, encryptedPhone)
	}
	query += `, updated_at = ? WHERE id = ? AND user_id = ? AND NOT EXISTS (
		SELECT 1 FROM jobs WHERE profile_id = ?
		AND status IN ('queued', 'running', 'awaiting_approval')
	)`
	args = append(args, formatTime(s.now()), id, userID, id)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return model.Profile{}, mapWriteError(err)
	}
	if err := s.classifyOwnedGuardedUpdate(ctx, "profiles", userID, id, result); err != nil {
		return model.Profile{}, err
	}
	return s.GetProfile(ctx, userID, id)
}

func (s *Store) GetProfile(ctx context.Context, userID, id int64) (model.Profile, error) {
	if userID <= 0 {
		return model.Profile{}, ErrUserRequired
	}
	return scanProfile(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, default_vehicle, otp_source_id,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		FROM profiles WHERE id = ? AND user_id = ?
	`, id, userID))
}

func (s *Store) ListProfiles(ctx context.Context, userID int64) ([]model.Profile, error) {
	if userID <= 0 {
		return nil, ErrUserRequired
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, default_vehicle, otp_source_id,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		FROM profiles WHERE user_id = ? ORDER BY name, id
	`, userID)
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

// SystemGetProfile is restricted to trusted queue/worker code that starts
// from a durable job. HTTP handlers must use ForUser.
func (s *Store) SystemGetProfile(ctx context.Context, id int64) (model.Profile, error) {
	return scanProfile(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, default_vehicle, otp_source_id,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		FROM profiles WHERE id = ?
	`, id))
}

func (s *Store) SystemListProfiles(ctx context.Context) ([]model.Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, default_vehicle, otp_source_id,
			headless, browser_channel, browser_executable, default_timeout_ms, enabled,
			created_at, updated_at
		FROM profiles ORDER BY user_id, name, id
	`)
	if err != nil {
		return nil, fmt.Errorf("system list profiles: %w", err)
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

func (s *Store) GetProfileCredentials(ctx context.Context, userID, id int64) (model.ProfileCredentials, error) {
	if userID <= 0 {
		return model.ProfileCredentials{}, ErrUserRequired
	}
	var encryptedPhone string
	if err := s.db.QueryRowContext(ctx, `
		SELECT yodel_phone_ciphertext FROM profiles WHERE id = ? AND user_id = ?
	`, id, userID).Scan(&encryptedPhone); errors.Is(err, sql.ErrNoRows) {
		return model.ProfileCredentials{}, ErrNotFound
	} else if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("read Yodel credentials: %w", err)
	}
	if encryptedPhone == "" {
		return model.ProfileCredentials{}, ErrYodelPhoneRequired
	}
	phone, err := s.encryptor.Decrypt(encryptedPhone)
	if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("decrypt Yodel mobile number: %w", err)
	}
	return model.ProfileCredentials{Phone: string(phone)}, nil
}

func (s *Store) SystemGetProfileCredentials(ctx context.Context, id int64) (model.ProfileCredentials, error) {
	var encryptedPhone string
	if err := s.db.QueryRowContext(ctx, `
		SELECT yodel_phone_ciphertext FROM profiles WHERE id = ?
	`, id).Scan(&encryptedPhone); errors.Is(err, sql.ErrNoRows) {
		return model.ProfileCredentials{}, ErrNotFound
	} else if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("read Yodel credentials: %w", err)
	}
	if encryptedPhone == "" {
		return model.ProfileCredentials{}, ErrYodelPhoneRequired
	}
	phone, err := s.encryptor.Decrypt(encryptedPhone)
	if err != nil {
		return model.ProfileCredentials{}, fmt.Errorf("decrypt Yodel mobile number: %w", err)
	}
	return model.ProfileCredentials{Phone: string(phone)}, nil
}

func (s *Store) DeleteProfile(ctx context.Context, userID, id int64) error {
	if userID <= 0 {
		return ErrUserRequired
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM profiles WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return fmt.Errorf("delete profile: %w", mapWriteError(err))
	}
	return requireAffected(result)
}

func profileFromInput(id, userID int64, input ProfileInput) model.Profile {
	return model.Profile{
		ID: id, UserID: userID, Name: strings.TrimSpace(input.Name),
		DefaultVehicle: strings.TrimSpace(input.DefaultVehicle), OTPSourceID: input.OTPSourceID,
		Headless: input.Headless, BrowserChannel: strings.TrimSpace(input.BrowserChannel),
		BrowserExecutable: strings.TrimSpace(input.BrowserExecutable), DefaultTimeoutMS: input.DefaultTimeoutMS,
		Enabled: input.Enabled,
	}
}

func normalizeYodelPhone(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("Yodel mobile number is required")
	}
	if len(value) > MaxYodelPhoneInputBytes {
		return "", errors.New("Yodel mobile number is too long")
	}
	hasInternationalPrefix := strings.HasPrefix(value, "+")
	var digits strings.Builder
	for index, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && index == 0:
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", errors.New("Yodel mobile number must contain only digits and common separators")
		}
	}
	normalized := digits.String()
	if hasInternationalPrefix && (len(normalized) != 11 || normalized[0] != '1') {
		return "", errors.New("Yodel mobile number international format must use the +1 country code")
	}
	if len(normalized) == 11 && normalized[0] == '1' {
		normalized = normalized[1:]
	}
	if len(normalized) != 10 {
		return "", errors.New("Yodel mobile number must contain 10 North American digits")
	}
	return normalized, nil
}

func scanProfile(scanner rowScanner) (model.Profile, error) {
	var profile model.Profile
	var created, updated string
	if err := scanner.Scan(&profile.ID, &profile.UserID, &profile.Name,
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
