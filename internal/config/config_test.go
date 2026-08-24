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

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentJobs != 2 || cfg.ListenAddress != ":8080" || cfg.SchedulesEnabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.DatabasePath != filepath.Join(cfg.AppDataDir, "buntzen.db") {
		t.Fatalf("database path was %q", cfg.DatabasePath)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatal(err)
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
