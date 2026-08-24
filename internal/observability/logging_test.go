package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerLevelAndRedaction(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewLogger(&output, "info")
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("hidden debug event")
	logger.Error(
		`provider failed at http://mac.test:1234/api/v1/ping?password=provider-secret "password":"yodel-secret" otp=654321 authorization: Bearer inline-token`,
		"authorization", "Bearer private-token",
		"exit_code", 64,
	)
	logged := output.String()
	for _, forbidden := range []string{"hidden debug event", "provider-secret", "yodel-secret", "654321", "inline-token", "private-token", "?password="} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log output leaked %q: %s", forbidden, logged)
		}
	}
	for _, expected := range []string{"[REDACTED]", "http://mac.test:1234/api/v1/ping", `"exit_code":64`} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log output missing %q: %s", expected, logged)
		}
	}
}

func TestChildDiagnosticRedactsKnownSecretsCodesAndAuthenticatedURLs(t *testing.T) {
	raw := "ERROR worker: account@example.test private-password 482913 https://user:pass@provider.test/messages?password=provider-password"
	clean := SanitizeChildDiagnostic(raw, "account@example.test", "private-password")
	for _, forbidden := range []string{"account@example.test", "private-password", "482913", "user:pass", "provider-password", "?password="} {
		if strings.Contains(clean, forbidden) {
			t.Fatalf("child diagnostic leaked %q: %s", forbidden, clean)
		}
	}
	if !strings.Contains(clean, "https://provider.test/messages") || !strings.Contains(clean, "[REDACTED-CODE]") {
		t.Fatalf("unexpected sanitized diagnostic: %s", clean)
	}
}

func TestLoggerSupportsGroupsAndDebug(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewLogger(&output, "debug")
	if err != nil {
		t.Fatal(err)
	}
	logger.LogAttrs(context.Background(), slog.LevelDebug, "worker event",
		slog.Group("detail", slog.String("password", "secret-value"), slog.String("phase", "starting")))
	logged := output.String()
	if !strings.Contains(logged, `"level":"DEBUG"`) || !strings.Contains(logged, `"phase":"starting"`) || strings.Contains(logged, "secret-value") {
		t.Fatalf("unexpected grouped debug log: %s", logged)
	}
}

func TestNewLoggerRejectsUnsupportedLevel(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, "trace"); err == nil {
		t.Fatal("expected unsupported log level error")
	}
}

func TestActionDiagnosticCarriesCorrelationAndMapsLevel(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewLogger(&output, "debug")
	if err != nil {
		t.Fatal(err)
	}
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	LogActionDiagnostic(context.Background(), "42", "WARNING buntzen_actions.worker: code=482913")
	logged := output.String()
	if !strings.Contains(logged, `"level":"WARN"`) || !strings.Contains(logged, `"job_id":"42"`) {
		t.Fatalf("missing correlated action diagnostic: %s", logged)
	}
	if strings.Contains(logged, "482913") {
		t.Fatalf("action diagnostic leaked OTP: %s", logged)
	}
}
