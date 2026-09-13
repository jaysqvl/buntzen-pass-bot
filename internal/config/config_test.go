package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	isolateEnvironment(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentJobs != 2 || cfg.ListenAddress != ":8080" || cfg.SchedulesEnabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !cfg.HostCheckEnabled {
		t.Fatal("host checks must be enabled by default")
	}
	if cfg.LogLevel != "info" || cfg.EffectiveLogLevel() != "info" {
		t.Fatalf("default log level = %q", cfg.LogLevel)
	}
	if cfg.DatabasePath != filepath.Join(cfg.AppDataDir, "lake-pass-bot.db") {
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
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_LOG_LEVEL", "warning")
	t.Setenv("LAKE_PASS_DEBUG", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("warning log level = %q", cfg.LogLevel)
	}

	t.Setenv("LAKE_PASS_LOG_LEVEL", "error")
	t.Setenv("LAKE_PASS_DEBUG", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("debug override log level = %q", cfg.LogLevel)
	}
}

func TestLoadRejectsInvalidLoggingConfiguration(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_LOG_LEVEL", "trace")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid log level to be rejected")
	}

	t.Setenv("LAKE_PASS_LOG_LEVEL", "info")
	t.Setenv("LAKE_PASS_DEBUG", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid debug toggle to be rejected")
	}
}

func TestLoadTrustedYodelOrigins(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_YODEL_ORIGINS", "https://example.test:443,https://second.example.test")
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
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_YODEL_ORIGINS", "http://example.test")
	if _, err := Load(); err == nil {
		t.Fatal("expected insecure Yodel origin to be rejected")
	}
}

func TestLoadRejectsInvalidConcurrency(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("MAX_CONCURRENT_JOBS", "99")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid concurrency error")
	}
}

func TestLoadAllowedOrigins(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_ALLOWED_ORIGINS", "http://lake-pass.example, https://lake-pass.example")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(cfg.AllowedOrigins), 2; got != want || cfg.AllowedOrigins[0] != "http://lake-pass.example" || cfg.AllowedOrigins[1] != "https://lake-pass.example" {
		t.Fatalf("allowed origins = %#v", cfg.AllowedOrigins)
	}
	if got, want := cfg.AllowedHosts, []string{"lake-pass.example"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("origin-derived allowed hosts = %#v", got)
	}
}

func TestLoadAllowedHostsAndSetupToken(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_ALLOWED_HOSTS", "Example.Test:8080, [::1]:8080")
	t.Setenv("LAKE_PASS_SETUP_TOKEN", "operator-setup-token")
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

func TestLoadHostCheckEnabled(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		unset bool
		want  bool
	}{
		{name: "unset", unset: true, want: true},
		{name: "empty", want: true},
		{name: "enabled", value: "true", want: true},
		{name: "disabled", value: "false", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironment(t)
			if test.unset {
				unsetForTest(t, "LAKE_PASS_HOST_CHECK_ENABLED")
			} else {
				t.Setenv("LAKE_PASS_HOST_CHECK_ENABLED", test.value)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HostCheckEnabled != test.want {
				t.Fatalf("host check enabled = %t, want %t", cfg.HostCheckEnabled, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidHostCheckEnabled(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_HOST_CHECK_ENABLED", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LAKE_PASS_HOST_CHECK_ENABLED must be true or false") {
		t.Fatalf("invalid host check setting error = %v", err)
	}
}

func TestLoadRejectsEmptyAllowedOrigin(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_ALLOWED_ORIGINS", "http://lake-pass.example,")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid allowed-origin list")
	}
}

func TestLoadRejectsInvalidAllowedOrigin(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_ALLOWED_ORIGINS", "https://lake-pass.example/path")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid allowed origin")
	}
}

func TestLoadRejectsInvalidAllowedHost(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("LAKE_PASS_ALLOWED_HOSTS", "https://lake-pass.example/path")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid allowed host")
	}
}

// Each test starts from production defaults, regardless of the developer's shell.
func isolateEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LAKE_PASS_LISTEN", "MAX_CONCURRENT_JOBS", "SCHEDULES_ENABLED",
		"LAKE_PASS_DEBUG", "LAKE_PASS_LOG_LEVEL", "LAKE_PASS_PYTHON",
		"LAKE_PASS_ACTIONS_MODULE", "LAKE_PASS_BROWSER_EXECUTABLE", "BLUEBUBBLES_URL", "LAKE_PASS_BLUEBUBBLES_ENDPOINTS", "LAKE_PASS_ALLOWED_ORIGINS",
		"LAKE_PASS_YODEL_ORIGINS", "LAKE_PASS_ALLOWED_HOSTS", "LAKE_PASS_SETUP_TOKEN", "LAKE_PASS_MASTER_KEY_FILE",
		"LAKE_PASS_HOST_CHECK_ENABLED", "BUNTZEN_HOST_CHECK_ENABLED",
		"LAKE_PASS_PUBLIC_ORIGIN", "LAKE_PASS_TRUSTED_PROXIES",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("APPDATA_DIR", filepath.Join(t.TempDir(), "state"))
}

func TestOperatorBrowserExecutable(t *testing.T) {
	isolateEnvironment(t)
	for _, value := range []string{"chrome", "../chrome"} {
		t.Setenv("LAKE_PASS_BROWSER_EXECUTABLE", value)
		if _, err := Load(); err == nil {
			t.Fatalf("accepted relative executable %q", value)
		}
	}
	want := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	t.Setenv("LAKE_PASS_BROWSER_EXECUTABLE", want)
	cfg, err := Load()
	if err != nil || cfg.BrowserExecutable != want {
		t.Fatalf("operator browser path = %q, %v", cfg.BrowserExecutable, err)
	}
}

func TestExplicitMasterKeyPath(t *testing.T) {
	isolateEnvironment(t)
	for _, path := range []string{"master.key", "../master.key", "/" + strings.Repeat("x", 2048)} {
		t.Setenv("LAKE_PASS_MASTER_KEY_FILE", path)
		if _, err := Load(); err == nil {
			t.Fatal("invalid explicit key path accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "existing.key")
	t.Setenv("LAKE_PASS_MASTER_KEY_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MasterKeyExplicit || cfg.EncryptionKeyPath != path {
		t.Fatal("explicit key path lost")
	}
	t.Setenv("LAKE_PASS_MASTER_KEY_FILE", "")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterKeyExplicit || cfg.EncryptionKeyPath != filepath.Join(cfg.AppDataDir, "master.key") {
		t.Fatal("legacy key default changed")
	}
}
