package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

type OTPSourceInput struct {
	Name           string
	Provider       model.OTPProvider
	Identity       string
	ProviderConfig any
	// SecretProvided is set only when the current request supplied a new
	// provider secret. It prevents a retained BlueBubbles password from being
	// rebound to a different server identity by an edit path.
	SecretProvided  bool
	PairingChatGUID string
	PairingSender   string
	PairingService  string
}

func (s *Store) CreateOTPSource(ctx context.Context, userID int64, input OTPSourceInput) (model.OTPSource, error) {
	if userID <= 0 {
		return model.OTPSource{}, ErrUserRequired
	}
	if input.ProviderConfig == nil {
		return model.OTPSource{}, errors.New("provider configuration is required")
	}
	source := model.OTPSource{
		UserID:          userID,
		Name:            strings.TrimSpace(input.Name),
		Provider:        input.Provider,
		Identity:        strings.TrimSpace(input.Identity),
		PairingChatGUID: strings.TrimSpace(input.PairingChatGUID),
		PairingSender:   strings.TrimSpace(input.PairingSender),
		PairingService:  strings.TrimSpace(input.PairingService),
	}
	if err := source.Validate(); err != nil {
		return model.OTPSource{}, err
	}
	if err := validatePairing(source.Provider, source.PairingChatGUID, source.PairingSender, source.PairingService); err != nil {
		return model.OTPSource{}, err
	}
	ciphertext, err := s.encryptJSON(input.ProviderConfig)
	if err != nil {
		return model.OTPSource{}, fmt.Errorf("encrypt provider configuration: %w", err)
	}
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO otp_sources(
			user_id, name, provider, identity, config_ciphertext,
			pairing_chat_guid, pairing_sender, pairing_service, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, userID, source.Name, source.Provider, source.Identity, ciphertext,
		source.PairingChatGUID, source.PairingSender, source.PairingService,
		formatTime(now), formatTime(now))
	if err != nil {
		return model.OTPSource{}, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.OTPSource{}, fmt.Errorf("read OTP source id: %w", err)
	}
	return s.GetOTPSource(ctx, userID, id)
}

// UpdateOTPSource retains the existing encrypted configuration when
// ProviderConfig is nil. A non-nil value replaces it atomically.
func (s *Store) UpdateOTPSource(ctx context.Context, userID, id int64, input OTPSourceInput) (model.OTPSource, error) {
	if userID <= 0 {
		return model.OTPSource{}, ErrUserRequired
	}
	existing, err := s.GetOTPSource(ctx, userID, id)
	if err != nil {
		return model.OTPSource{}, err
	}
	source := model.OTPSource{
		ID: id, UserID: userID, Name: strings.TrimSpace(input.Name), Provider: input.Provider,
		Identity:        strings.TrimSpace(input.Identity),
		PairingChatGUID: strings.TrimSpace(input.PairingChatGUID),
		PairingSender:   strings.TrimSpace(input.PairingSender),
		PairingService:  strings.TrimSpace(input.PairingService),
	}
	if existing.Provider == model.OTPProviderBlueBubbles &&
		source.Provider == model.OTPProviderBlueBubbles && existing.Identity != source.Identity {
		if !input.SecretProvided {
			return model.OTPSource{}, errors.New("changing the BlueBubbles server requires a newly supplied password")
		}
		// A pairing fingerprint belongs to one Messages inbox. Password rotation
		// on the same identity may retain it; changing servers must re-pair.
		source.PairingChatGUID, source.PairingSender, source.PairingService = "", "", ""
	}
	if err := source.Validate(); err != nil {
		return model.OTPSource{}, err
	}
	if err := validatePairing(source.Provider, source.PairingChatGUID, source.PairingSender, source.PairingService); err != nil {
		return model.OTPSource{}, err
	}
	if input.ProviderConfig == nil {
		if existing.Provider != input.Provider {
			return model.OTPSource{}, errors.New("changing OTP provider requires replacement provider configuration")
		}
	}
	var result sql.Result
	if input.ProviderConfig == nil {
		result, err = s.db.ExecContext(ctx, `
			UPDATE otp_sources SET name = ?, provider = ?, identity = ?,
				pairing_chat_guid = ?, pairing_sender = ?, pairing_service = ?, updated_at = ?
			WHERE id = ? AND user_id = ?
			AND (? OR provider <> 'bluebubbles' OR ? <> 'bluebubbles' OR identity = ?)
			AND NOT EXISTS (
				SELECT 1 FROM jobs WHERE otp_source_id = otp_sources.id
				AND status IN ('queued', 'running', 'awaiting_approval')
			)
		`, source.Name, source.Provider, source.Identity, source.PairingChatGUID,
			source.PairingSender, source.PairingService, formatTime(s.now()), id, userID,
			input.SecretProvided, source.Provider, source.Identity)
	} else {
		var ciphertext string
		ciphertext, err = s.encryptJSON(input.ProviderConfig)
		if err == nil {
			result, err = s.db.ExecContext(ctx, `
				UPDATE otp_sources SET name = ?, provider = ?, identity = ?, config_ciphertext = ?,
					pairing_chat_guid = ?, pairing_sender = ?, pairing_service = ?, updated_at = ?
				WHERE id = ? AND user_id = ?
				AND (? OR provider <> 'bluebubbles' OR ? <> 'bluebubbles' OR identity = ?)
				AND NOT EXISTS (
					SELECT 1 FROM jobs WHERE otp_source_id = otp_sources.id
					AND status IN ('queued', 'running', 'awaiting_approval')
				)
			`, source.Name, source.Provider, source.Identity, ciphertext,
				source.PairingChatGUID, source.PairingSender, source.PairingService,
				formatTime(s.now()), id, userID, input.SecretProvided, source.Provider, source.Identity)
		}
	}
	if err != nil {
		return model.OTPSource{}, mapWriteError(err)
	}
	if err := s.classifyOwnedGuardedUpdate(ctx, "otp_sources", userID, id, result); err != nil {
		return model.OTPSource{}, err
	}
	return s.GetOTPSource(ctx, userID, id)
}

// SystemPersistOTPSourcePairing commits the operator's supervised selection
// only while the exact pairing job is still active and uncancelled. The SQL
// predicate is the revocation linearization point: an account disable or job
// cancellation that commits first prevents this durable write.
func (s *Store) SystemPersistOTPSourcePairing(ctx context.Context, jobID, sourceID int64, chatGUID, sender, service string) error {
	if jobID <= 0 || sourceID <= 0 {
		return errors.New("pairing job and source are required")
	}
	source, err := s.SystemGetOTPSource(ctx, sourceID)
	if err != nil {
		return err
	}
	chatGUID = strings.TrimSpace(chatGUID)
	sender = strings.TrimSpace(sender)
	service = strings.TrimSpace(service)
	if err := validatePairing(source.Provider, chatGUID, sender, service); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE otp_sources SET pairing_chat_guid = ?, pairing_sender = ?, pairing_service = ?, updated_at = ?
		WHERE id = ? AND provider = 'bluebubbles' AND EXISTS (
			SELECT 1 FROM jobs
			JOIN users ON users.id = jobs.user_id
			WHERE jobs.id = ?
			AND jobs.user_id = otp_sources.user_id
			AND jobs.otp_source_id = otp_sources.id
			AND jobs.command = 'auth-check'
			AND jobs.status = 'running'
			AND jobs.cancel_requested = 0
			AND jobs.dedup_key GLOB ('pairing:' || otp_sources.id || ':*')
			AND users.status = 'active'
		)
	`, chatGUID, sender, service, formatTime(s.now()), sourceID, jobID)
	if err != nil {
		return fmt.Errorf("persist OTP source pairing: %w", mapWriteError(err))
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect OTP source pairing update: %w", err)
	}
	if rows != 1 {
		return ErrTransitionConflict
	}
	return nil
}

func (s *Store) GetOTPSource(ctx context.Context, userID, id int64) (model.OTPSource, error) {
	if userID <= 0 {
		return model.OTPSource{}, ErrUserRequired
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, provider, identity, pairing_chat_guid, pairing_sender,
			pairing_service, created_at, updated_at
		FROM otp_sources WHERE id = ? AND user_id = ?
	`, id, userID)
	return scanOTPSource(row)
}

func (s *Store) ListOTPSources(ctx context.Context, userID int64) ([]model.OTPSource, error) {
	if userID <= 0 {
		return nil, ErrUserRequired
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, provider, identity, pairing_chat_guid, pairing_sender,
			pairing_service, created_at, updated_at
		FROM otp_sources WHERE user_id = ? ORDER BY name, id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list OTP sources: %w", err)
	}
	defer rows.Close()
	var result []model.OTPSource
	for rows.Next() {
		source, err := scanOTPSource(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

// SystemGetOTPSource is for trusted queue workers that already hold a durable
// job reference. HTTP handlers must use ForUser instead.
func (s *Store) SystemGetOTPSource(ctx context.Context, id int64) (model.OTPSource, error) {
	return scanOTPSource(s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, provider, identity, pairing_chat_guid, pairing_sender,
			pairing_service, created_at, updated_at
		FROM otp_sources WHERE id = ?
	`, id))
}

func (s *Store) SystemListOTPSources(ctx context.Context) ([]model.OTPSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, provider, identity, pairing_chat_guid, pairing_sender,
			pairing_service, created_at, updated_at
		FROM otp_sources ORDER BY user_id, name, id
	`)
	if err != nil {
		return nil, fmt.Errorf("system list OTP sources: %w", err)
	}
	defer rows.Close()
	var result []model.OTPSource
	for rows.Next() {
		source, err := scanOTPSource(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

func (s *Store) GetOTPSourceConfig(ctx context.Context, userID, id int64, destination any) error {
	if userID <= 0 {
		return ErrUserRequired
	}
	if destination == nil {
		return errors.New("provider configuration destination is required")
	}
	var ciphertext string
	if err := s.db.QueryRowContext(ctx,
		"SELECT config_ciphertext FROM otp_sources WHERE id = ? AND user_id = ?", id, userID,
	).Scan(&ciphertext); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read provider configuration: %w", err)
	}
	plaintext, err := s.encryptor.Decrypt(ciphertext)
	if err != nil {
		return fmt.Errorf("decrypt provider configuration: %w", err)
	}
	if err := json.Unmarshal(plaintext, destination); err != nil {
		return fmt.Errorf("decode provider configuration: %w", err)
	}
	return nil
}

func (s *Store) SystemGetOTPSourceConfig(ctx context.Context, id int64, destination any) error {
	if destination == nil {
		return errors.New("provider configuration destination is required")
	}
	var ciphertext string
	if err := s.db.QueryRowContext(ctx,
		"SELECT config_ciphertext FROM otp_sources WHERE id = ?", id,
	).Scan(&ciphertext); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read provider configuration: %w", err)
	}
	plaintext, err := s.encryptor.Decrypt(ciphertext)
	if err != nil {
		return fmt.Errorf("decrypt provider configuration: %w", err)
	}
	if err := json.Unmarshal(plaintext, destination); err != nil {
		return fmt.Errorf("decode provider configuration: %w", err)
	}
	return nil
}

func (s *Store) DeleteOTPSource(ctx context.Context, userID, id int64) error {
	if userID <= 0 {
		return ErrUserRequired
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM otp_sources WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return fmt.Errorf("delete OTP source: %w", mapWriteError(err))
	}
	return requireAffected(result)
}

func scanOTPSource(scanner rowScanner) (model.OTPSource, error) {
	var source model.OTPSource
	var created, updated string
	if err := scanner.Scan(&source.ID, &source.UserID, &source.Name, &source.Provider, &source.Identity,
		&source.PairingChatGUID, &source.PairingSender, &source.PairingService,
		&created, &updated); errors.Is(err, sql.ErrNoRows) {
		return model.OTPSource{}, ErrNotFound
	} else if err != nil {
		return model.OTPSource{}, fmt.Errorf("scan OTP source: %w", err)
	}
	var err error
	if source.CreatedAt, err = parseTime(created); err != nil {
		return model.OTPSource{}, err
	}
	if source.UpdatedAt, err = parseTime(updated); err != nil {
		return model.OTPSource{}, err
	}
	return source, nil
}

func (s *Store) encryptJSON(value any) (string, error) {
	if value == nil {
		value = struct{}{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode JSON: %w", err)
	}
	if len(encoded) > MaxProviderConfigJSONBytes {
		return "", errors.New("provider configuration is too large")
	}
	ciphertext, err := s.encryptor.Encrypt(encoded)
	if err != nil {
		return "", err
	}
	if len(ciphertext) > MaxProviderConfigCiphertextBytes {
		return "", errors.New("encrypted provider configuration is too large")
	}
	return ciphertext, nil
}

func validatePairing(provider model.OTPProvider, chatGUID, sender, service string) error {
	if len(chatGUID) > model.MaxPairingChatGUIDBytes || len(sender) > model.MaxPairingSenderBytes ||
		len(service) > model.MaxPairingServiceBytes {
		return errors.New("BlueBubbles pairing fingerprint is too long")
	}
	populated := 0
	for _, value := range []string{chatGUID, sender, service} {
		if strings.TrimSpace(value) != "" {
			populated++
		}
	}
	if provider != model.OTPProviderBlueBubbles && populated != 0 {
		return errors.New("pairing fingerprints are only valid for BlueBubbles")
	}
	if provider == model.OTPProviderBlueBubbles && populated != 0 && populated != 3 {
		return errors.New("BlueBubbles pairing requires chat, sender, and service together")
	}
	return nil
}
