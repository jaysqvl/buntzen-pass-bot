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
	Name            string
	Provider        model.OTPProvider
	Identity        string
	ProviderConfig  any
	PairingChatGUID string
	PairingSender   string
	PairingService  string
}

func (s *Store) CreateOTPSource(ctx context.Context, input OTPSourceInput) (model.OTPSource, error) {
	if input.ProviderConfig == nil {
		return model.OTPSource{}, errors.New("provider configuration is required")
	}
	source := model.OTPSource{
		Name:            strings.TrimSpace(input.Name),
		Provider:        input.Provider,
		Identity:        strings.TrimSpace(input.Identity),
		PairingChatGUID: strings.TrimSpace(input.PairingChatGUID),
		PairingSender:   strings.TrimSpace(input.PairingSender),
		PairingService:  strings.TrimSpace(input.PairingService),
	}
	if err := model.ValidateOTPSource(source); err != nil {
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
			name, provider, identity, config_ciphertext,
			pairing_chat_guid, pairing_sender, pairing_service, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, source.Name, source.Provider, source.Identity, ciphertext,
		source.PairingChatGUID, source.PairingSender, source.PairingService,
		formatTime(now), formatTime(now))
	if err != nil {
		return model.OTPSource{}, mapWriteError(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.OTPSource{}, fmt.Errorf("read OTP source id: %w", err)
	}
	return s.GetOTPSource(ctx, id)
}

// UpdateOTPSource retains the existing encrypted configuration when
// ProviderConfig is nil. A non-nil value replaces it atomically.
func (s *Store) UpdateOTPSource(ctx context.Context, id int64, input OTPSourceInput) (model.OTPSource, error) {
	existing, err := s.GetOTPSource(ctx, id)
	if err != nil {
		return model.OTPSource{}, err
	}
	source := model.OTPSource{
		ID: id, Name: strings.TrimSpace(input.Name), Provider: input.Provider,
		Identity:        strings.TrimSpace(input.Identity),
		PairingChatGUID: strings.TrimSpace(input.PairingChatGUID),
		PairingSender:   strings.TrimSpace(input.PairingSender),
		PairingService:  strings.TrimSpace(input.PairingService),
	}
	if existing.Provider == model.OTPProviderBlueBubbles &&
		source.Provider == model.OTPProviderBlueBubbles && existing.Identity != source.Identity {
		// A pairing fingerprint belongs to one Messages inbox. Password rotation
		// on the same identity may retain it; changing servers must re-pair.
		source.PairingChatGUID, source.PairingSender, source.PairingService = "", "", ""
	}
	if err := model.ValidateOTPSource(source); err != nil {
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
			WHERE id = ? AND NOT EXISTS (
				SELECT 1 FROM jobs WHERE otp_source_id = otp_sources.id
				AND status IN ('queued', 'running', 'awaiting_approval')
			)
		`, source.Name, source.Provider, source.Identity, source.PairingChatGUID,
			source.PairingSender, source.PairingService, formatTime(s.now()), id)
	} else {
		var ciphertext string
		ciphertext, err = s.encryptJSON(input.ProviderConfig)
		if err == nil {
			result, err = s.db.ExecContext(ctx, `
				UPDATE otp_sources SET name = ?, provider = ?, identity = ?, config_ciphertext = ?,
					pairing_chat_guid = ?, pairing_sender = ?, pairing_service = ?, updated_at = ?
				WHERE id = ? AND NOT EXISTS (
					SELECT 1 FROM jobs WHERE otp_source_id = otp_sources.id
					AND status IN ('queued', 'running', 'awaiting_approval')
				)
			`, source.Name, source.Provider, source.Identity, ciphertext,
				source.PairingChatGUID, source.PairingSender, source.PairingService,
				formatTime(s.now()), id)
		}
	}
	if err != nil {
		return model.OTPSource{}, mapWriteError(err)
	}
	if err := s.classifyGuardedUpdate(ctx, "otp_sources", id, result); err != nil {
		return model.OTPSource{}, err
	}
	return s.GetOTPSource(ctx, id)
}

func (s *Store) UpdateOTPSourcePairing(ctx context.Context, id int64, chatGUID, sender, service string) error {
	source, err := s.GetOTPSource(ctx, id)
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
		WHERE id = ? AND NOT EXISTS (
			SELECT 1 FROM jobs WHERE otp_source_id = otp_sources.id
			AND status IN ('queued', 'running', 'awaiting_approval')
			AND command <> 'auth-check'
		)
	`, chatGUID, sender, service, formatTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("update OTP source pairing: %w", err)
	}
	return s.classifyGuardedUpdate(ctx, "otp_sources", id, result)
}

func (s *Store) GetOTPSource(ctx context.Context, id int64) (model.OTPSource, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, provider, identity, pairing_chat_guid, pairing_sender,
			pairing_service, created_at, updated_at
		FROM otp_sources WHERE id = ?
	`, id)
	return scanOTPSource(row)
}

func (s *Store) ListOTPSources(ctx context.Context) ([]model.OTPSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, provider, identity, pairing_chat_guid, pairing_sender,
			pairing_service, created_at, updated_at
		FROM otp_sources ORDER BY name, id
	`)
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

func (s *Store) GetOTPSourceConfig(ctx context.Context, id int64, destination any) error {
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

func (s *Store) DeleteOTPSource(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM otp_sources WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete OTP source: %w", mapWriteError(err))
	}
	return requireAffected(result)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOTPSource(scanner rowScanner) (model.OTPSource, error) {
	var source model.OTPSource
	var created, updated string
	if err := scanner.Scan(&source.ID, &source.Name, &source.Provider, &source.Identity,
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
	return s.encryptor.Encrypt(encoded)
}

func requireAffected(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected row count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func validatePairing(provider model.OTPProvider, chatGUID, sender, service string) error {
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
