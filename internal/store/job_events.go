package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

var (
	digitSecretPattern = regexp.MustCompile(`\b[0-9]{4,8}\b`)
	querySecretPattern = regexp.MustCompile(`(?i)(password|token|secret|otp|code)=([^&\s]+)`)
)

type JobEventInput struct {
	JobID   int64
	Level   string
	Kind    string
	Message string
	Data    map[string]any
}

func (s *Store) SystemAppendJobEvent(ctx context.Context, input JobEventInput) (model.JobEvent, error) {
	level := strings.ToLower(strings.TrimSpace(input.Level))
	if level == "" {
		level = "info"
	}
	if level != "debug" && level != "info" && level != "warning" && level != "error" {
		return model.JobEvent{}, errors.New("invalid job event level")
	}
	kind := truncate(sanitizeText(strings.TrimSpace(input.Kind)), 80)
	if kind == "" {
		return model.JobEvent{}, errors.New("job event kind is required")
	}
	knownSecrets, err := s.durableEventSecrets(ctx, input.JobID)
	if err != nil {
		return model.JobEvent{}, err
	}
	data, err := normalizeAndSanitize(input.Data)
	if err != nil {
		return model.JobEvent{}, err
	}
	data = redactKnownValue(data, knownSecrets)
	encoded, err := json.Marshal(data)
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("encode job event data: %w", err)
	}
	if len(encoded) > 16*1024 {
		return model.JobEvent{}, errors.New("job event data exceeds 16 KiB")
	}
	now := s.now()
	message := truncate(redactKnownText(sanitizeText(input.Message), knownSecrets), 2000)
	kind = truncate(redactKnownText(kind, knownSecrets), 80)
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO job_events(user_id, job_id, level, kind, message, data_json, created_at)
		SELECT user_id, id, ?, ?, ?, ?, ? FROM jobs WHERE id = ?
	`, level, kind, message, string(encoded), formatTime(now), input.JobID)
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("append job event: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return model.JobEvent{}, fmt.Errorf("read job event result: %w", err)
	} else if count == 0 {
		return model.JobEvent{}, ErrNotFound
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.JobEvent{}, fmt.Errorf("read job event id: %w", err)
	}
	job, err := s.SystemGetJob(ctx, input.JobID)
	if err != nil {
		return model.JobEvent{}, err
	}
	return model.JobEvent{ID: id, UserID: job.UserID, JobID: input.JobID, Level: level, Kind: kind,
		Message: message, DataJSON: string(encoded), CreatedAt: now}, nil
}

func (s *Store) ListJobEvents(ctx context.Context, userID, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if userID <= 0 {
		return nil, ErrUserRequired
	}
	if _, err := s.GetJob(ctx, userID, jobID); err != nil {
		return nil, err
	}
	return s.listJobEvents(ctx, userID, jobID, afterID, limit)
}

func (s *Store) SystemListJobEvents(ctx context.Context, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if _, err := s.SystemGetJob(ctx, jobID); err != nil {
		return nil, err
	}
	return s.listJobEvents(ctx, 0, jobID, afterID, limit)
}

func (s *Store) listJobEvents(ctx context.Context, userID, jobID int64, afterID int64, limit int) ([]model.JobEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, user_id, job_id, level, kind, message, data_json, created_at
		FROM job_events WHERE job_id = ? AND id > ?`
	args := []any{jobID, afterID}
	if userID > 0 {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list job events: %w", err)
	}
	defer rows.Close()
	var result []model.JobEvent
	for rows.Next() {
		var event model.JobEvent
		var created string
		if err := rows.Scan(&event.ID, &event.UserID, &event.JobID, &event.Level, &event.Kind,
			&event.Message, &event.DataJSON, &created); err != nil {
			return nil, fmt.Errorf("scan job event: %w", err)
		}
		event.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func sanitizeText(value string) string {
	value = querySecretPattern.ReplaceAllString(value, "$1=[REDACTED]")
	return digitSecretPattern.ReplaceAllString(value, "[REDACTED]")
}

func sanitizeValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "token") ||
				strings.Contains(lower, "secret") || strings.Contains(lower, "otp") ||
				strings.Contains(lower, "code") {
				result[key] = "[REDACTED]"
			} else {
				result[key] = sanitizeValue(child)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = sanitizeValue(child)
		}
		return result
	case string:
		return sanitizeText(typed)
	case int:
		if typed >= 1_000 && typed <= 99_999_999 {
			return "[REDACTED]"
		}
		return typed
	case int64:
		if typed >= 1_000 && typed <= 99_999_999 {
			return "[REDACTED]"
		}
		return typed
	case float64:
		if typed >= 1_000 && typed <= 99_999_999 && typed == float64(int64(typed)) {
			return "[REDACTED]"
		}
		return typed
	case json.Number:
		if numeric, err := typed.Int64(); err == nil && numeric >= 1_000 && numeric <= 99_999_999 {
			return "[REDACTED]"
		}
		return typed
	default:
		return value
	}
}

// normalizeAndSanitize first passes arbitrary caller values through JSON so
// concrete maps, slices, and structs cannot bypass recursive secret redaction.
func normalizeAndSanitize(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode job event data: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("normalize job event data: %w", err)
	}
	return sanitizeValue(normalized), nil
}

func (s *Store) durableEventSecrets(ctx context.Context, jobID int64) ([]string, error) {
	var encryptedPhone, encryptedProvider string
	if err := s.db.QueryRowContext(ctx, `
		SELECT profile.yodel_phone_ciphertext, source.config_ciphertext
		FROM jobs AS job
		JOIN profiles AS profile ON profile.id = job.profile_id
		JOIN otp_sources AS source ON source.id = job.otp_source_id
		WHERE job.id = ?
	`, jobID).Scan(&encryptedPhone, &encryptedProvider); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	secrets := make([]string, 0, 8)
	if encryptedPhone != "" {
		plaintext, err := s.encryptor.Decrypt(encryptedPhone)
		if err != nil {
			return nil, fmt.Errorf("load event redaction material: %w", err)
		}
		if value := strings.TrimSpace(string(plaintext)); len(value) >= 4 {
			secrets = append(secrets, value)
		}
	}
	providerJSON, err := s.encryptor.Decrypt(encryptedProvider)
	if err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(providerJSON)))
	decoder.UseNumber()
	var providerData any
	if err := decoder.Decode(&providerData); err != nil {
		return nil, fmt.Errorf("load event redaction material: %w", err)
	}
	collectStringLeaves(providerData, &secrets)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets, nil
}

func collectStringLeaves(value any, destination *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			collectStringLeaves(child, destination)
		}
	case []any:
		for _, child := range typed {
			collectStringLeaves(child, destination)
		}
	case string:
		if trimmed := strings.TrimSpace(typed); len(trimmed) >= 4 {
			*destination = append(*destination, trimmed)
		}
	}
}

func redactKnownValue(value any, secrets []string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = redactKnownValue(child, secrets)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = redactKnownValue(child, secrets)
		}
		return typed
	case string:
		return redactKnownText(typed, secrets)
	default:
		return value
	}
}

func redactKnownText(value string, secrets []string) string {
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
