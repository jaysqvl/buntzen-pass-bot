package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("APPDATA_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv("MAX_CONCURRENT_JOBS", "")
	t.Setenv("SCHEDULES_ENABLED", "")
	t.Setenv("BUNTZEN_LISTEN", "")
	t.Setenv("BUNTZEN_ALLOWED_HOSTS", "")
	t.Setenv("BUNTZEN_ALLOWED_ORIGINS", "")
	t.Setenv("BUNTZEN_YODEL_ORIGINS", "")
	t.Setenv("BUNTZEN_SETUP_TOKEN", "")
	t.Setenv("BUNTZEN_LOG_LEVEL", "")
	t.Setenv("BUNTZEN_DEBUG", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentJobs != 2 || cfg.ListenAddress != ":8080" || cfg.SchedulesEnabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.LogLevel != "info" || cfg.EffectiveLogLevel() != "info" {
		t.Fatalf("default log level = %q", cfg.LogLevel)
	}
	if cfg.DatabasePath != filepath.Join(cfg.AppDataDir, "buntzen.db") {
		t.Fatalf("database path was %q", cfg.DatabasePath)
	}
	if len(cfg.YodelOrigins) != 1 || cfg.YodelOrigins[0] != DefaultYodelOrigin {
		t.Fatalf("default Yodel origins = %#v", cfg.YodelOrigins)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLogLevelAndDebugOverride(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_LOG_LEVEL", "warning")
	t.Setenv("BUNTZEN_DEBUG", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("warning log level = %q", cfg.LogLevel)
	}

	t.Setenv("BUNTZEN_LOG_LEVEL", "error")
	t.Setenv("BUNTZEN_DEBUG", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("debug override log level = %q", cfg.LogLevel)
	}
}

func TestLoadRejectsInvalidLoggingConfiguration(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_LOG_LEVEL", "trace")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid log level to be rejected")
	}

	t.Setenv("BUNTZEN_LOG_LEVEL", "info")
	t.Setenv("BUNTZEN_DEBUG", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid debug toggle to be rejected")
	}
}

func TestLoadTrustedYodelOrigins(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_YODEL_ORIGINS", "https://example.test:443,https://second.example.test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.test", "https://second.example.test"}
	if len(cfg.YodelOrigins) != len(want) || cfg.YodelOrigins[0] != want[0] || cfg.YodelOrigins[1] != want[1] {
		t.Fatalf("Yodel origins = %#v", cfg.YodelOrigins)
	}
}

func TestLoadRejectsInsecureYodelOrigin(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_YODEL_ORIGINS", "http://example.test")
	if _, err := Load(); err == nil {
		t.Fatal("expected insecure Yodel origin to be rejected")
	}
}

func TestLoadRejectsInvalidConcurrency(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("MAX_CONCURRENT_JOBS", "99")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid concurrency error")
	}
}

func TestLoadAllowedOrigins(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_ALLOWED_ORIGINS", "http://buntzen.example, https://buntzen.example")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(cfg.AllowedOrigins), 2; got != want || cfg.AllowedOrigins[0] != "http://buntzen.example" || cfg.AllowedOrigins[1] != "https://buntzen.example" {
		t.Fatalf("allowed origins = %#v", cfg.AllowedOrigins)
	}
	if got, want := cfg.AllowedHosts, []string{"buntzen.example"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("origin-derived allowed hosts = %#v", got)
	}
}

func TestLoadAllowedHostsAndSetupToken(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_ALLOWED_HOSTS", "Example.Test:8080, [::1]:8080")
	t.Setenv("BUNTZEN_SETUP_TOKEN", "operator-setup-token")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.AllowedHosts, []string{"example.test:8080", "[::1]:8080"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("allowed hosts = %#v", got)
	}
	if cfg.SetupToken != "operator-setup-token" {
		t.Fatalf("setup token = %q", cfg.SetupToken)
	}
}

func TestLoadRejectsEmptyAllowedOrigin(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_ALLOWED_ORIGINS", "http://buntzen.example,")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid allowed-origin list")
	}
}

func TestLoadRejectsInvalidAllowedOrigin(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_ALLOWED_ORIGINS", "https://buntzen.example/path")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid allowed origin")
	}
}

func TestLoadRejectsInvalidAllowedHost(t *testing.T) {
	t.Setenv("APPDATA_DIR", t.TempDir())
	t.Setenv("BUNTZEN_ALLOWED_HOSTS", "https://buntzen.example/path")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid allowed host")
	}
}
