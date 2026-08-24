// Package observability configures bounded, structured, secret-safe runtime
// logging for the control plane and its isolated action workers.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const MaxLogMessageBytes = 16 * 1024

var (
	urlPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	// Named values cover common error and exception formats without treating
	// every occurrence of the word "token" as secret. In particular, the
	// deliberately operator-visible one-time setup-token startup line remains
	// available on an empty installation.
	sensitiveValuePattern = regexp.MustCompile(`(?i)\b(password|passwd|auth_token|access_token|refresh_token|api_key|secret|otp|passcode|code)\b(["']?)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
	authorizationPattern  = regexp.MustCompile(`(?i)\bauthorization\b(["']?)(\s*[:=]\s*)[^\r\n,;]+`)
	standaloneCodePattern = regexp.MustCompile(`(^|[^0-9])([0-9]{4,8})([^0-9]|$)`)
)

// Configure installs a JSON logger on stderr. Docker and Portainer retain one
// event per line, and slog.SetDefault also routes legacy log.Printf calls
// through the same redacting handler.
func Configure(level string) error {
	logger, err := NewLogger(os.Stderr, level)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	return nil
}

// NewLogger is exported for deterministic redaction and level tests.
func NewLogger(writer io.Writer, level string) (*slog.Logger, error) {
	parsed, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: parsed})
	return slog.New(&redactingHandler{next: handler}), nil
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}

// Sanitize removes dynamically known values, authenticated URL material, and
// named credential/OTP values from a diagnostic string.
func Sanitize(value string, secrets ...string) string {
	value = replaceKnownSecrets(value, secrets)
	value = urlPattern.ReplaceAllStringFunc(value, sanitizeURL)
	value = authorizationPattern.ReplaceAllString(value, `authorization${1}${2}[REDACTED]`)
	value = sensitiveValuePattern.ReplaceAllString(value, `${1}${2}${3}[REDACTED]`)
	return truncate(value, MaxLogMessageBytes)
}

// SanitizeChildDiagnostic is intentionally stricter than ordinary control-
// plane logging. Child stderr is never protocol data, so any standalone 4-8
// digit sequence is suppressed defensively in case a third-party exception
// renders an OTP before Python's in-memory redactor sees it.
func SanitizeChildDiagnostic(value string, secrets ...string) string {
	value = Sanitize(value, secrets...)
	urls := make([]string, 0, 2)
	value = urlPattern.ReplaceAllStringFunc(value, func(safeURL string) string {
		token := fmt.Sprintf("__BUNTZEN_SAFE_URL_%x__", len(urls))
		urls = append(urls, safeURL)
		return token
	})
	for {
		updated := standaloneCodePattern.ReplaceAllString(value, `${1}[REDACTED-CODE]${3}`)
		if updated == value {
			break
		}
		value = updated
	}
	for index, safeURL := range urls {
		token := fmt.Sprintf("__BUNTZEN_SAFE_URL_%x__", index)
		value = strings.ReplaceAll(value, token, safeURL)
	}
	return truncate(value, MaxLogMessageBytes)
}

// LogActionDiagnostic maps the Python logging prefix onto the corresponding Go
// level while adding the durable job correlation used in container logs.
func LogActionDiagnostic(ctx context.Context, jobID string, value string, secrets ...string) {
	value = SanitizeChildDiagnostic(strings.TrimSpace(value), secrets...)
	if value == "" {
		return
	}
	level := slog.LevelInfo
	for _, candidate := range []struct {
		prefix string
		level  slog.Level
	}{
		{"DEBUG ", slog.LevelDebug},
		{"INFO ", slog.LevelInfo},
		{"WARNING ", slog.LevelWarn},
		{"WARN ", slog.LevelWarn},
		{"ERROR ", slog.LevelError},
		{"CRITICAL ", slog.LevelError},
	} {
		if strings.HasPrefix(value, candidate.prefix) {
			level = candidate.level
			value = strings.TrimSpace(strings.TrimPrefix(value, candidate.prefix))
			break
		}
	}
	slog.Log(ctx, level, "python action diagnostic",
		"component", "python",
		"job_id", jobID,
		"detail", value,
	)
}

func replaceKnownSecrets(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func sanitizeURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[REDACTED-URL]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "...[truncated]"
}

type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, Sanitize(record.Message), record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		clean.AddAttrs(sanitizeAttr(attribute))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attributes))
	for _, attribute := range attributes {
		clean = append(clean, sanitizeAttr(attribute))
	}
	return &redactingHandler{next: h.next.WithAttrs(clean)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func sanitizeAttr(attribute slog.Attr) slog.Attr {
	attribute.Value = attribute.Value.Resolve()
	if sensitiveKey(attribute.Key) {
		return slog.String(attribute.Key, "[REDACTED]")
	}
	switch attribute.Value.Kind() {
	case slog.KindString:
		return slog.String(attribute.Key, Sanitize(attribute.Value.String()))
	case slog.KindAny:
		return slog.String(attribute.Key, Sanitize(fmt.Sprint(attribute.Value.Any())))
	case slog.KindGroup:
		group := attribute.Value.Group()
		clean := make([]slog.Attr, 0, len(group))
		for _, nested := range group {
			clean = append(clean, sanitizeAttr(nested))
		}
		return slog.Group(attribute.Key, attrsToAny(clean)...)
	default:
		return attribute
	}
}

func attrsToAny(attributes []slog.Attr) []any {
	values := make([]any, len(attributes))
	for index := range attributes {
		values[index] = attributes[index]
	}
	return values
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "password", "passwd", "auth_token", "access_token", "refresh_token",
		"api_key", "authorization", "secret", "otp", "passcode", "code",
		"setup_token", "session", "session_token", "csrf", "csrf_token":
		return true
	default:
		return false
	}
}
