package config

import (
	"os"
	"path/filepath"
	"testing"
)

func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyConfigurationAndNeutralPrecedence(t *testing.T) {
	isolateEnvironment(t)
	for _, suffix := range []string{"LISTEN", "ACTIONS_MODULE", "SETUP_TOKEN", "ALLOWED_HOSTS", "PUBLIC_ORIGIN", "TRUSTED_PROXIES"} {
		unsetForTest(t, "LAKE_PASS_"+suffix)
	}
	t.Setenv("BUNTZEN_LISTEN", "127.0.0.1:8097")
	t.Setenv("BUNTZEN_ACTIONS_MODULE", "buntzen_actions")
	t.Setenv("BUNTZEN_SETUP_TOKEN", "legacy-token")
	t.Setenv("BUNTZEN_ALLOWED_HOSTS", "lake-pass.example")
	t.Setenv("BUNTZEN_PUBLIC_ORIGIN", "https://lake-pass.example")
	t.Setenv("BUNTZEN_TRUSTED_PROXIES", "127.0.0.1/32")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != "127.0.0.1:8097" || cfg.PythonModule != "lake_pass_actions" || cfg.SetupToken != "legacy-token" || cfg.PublicOrigin != "https://lake-pass.example" || len(cfg.TrustedProxies) != 1 {
		t.Fatal("legacy deployment settings were not retained")
	}
	t.Setenv("LAKE_PASS_LISTEN", "127.0.0.1:8098")
	t.Setenv("LAKE_PASS_SETUP_TOKEN", "")
	cfg, err = Load()
	if err != nil || cfg.ListenAddress != "127.0.0.1:8098" || cfg.SetupToken != "" {
		t.Fatal("new names must win, including an explicitly empty value")
	}
}

func TestDatabaseDiscoveryPreservesExistingState(t *testing.T) {
	for _, test := range []struct {
		name, existing, want string
	}{
		{"new install", "", "lake-pass-bot.db"},
		{"legacy install", "buntzen.db", "buntzen.db"},
		{"renamed install", "lake-pass-bot.db", "lake-pass-bot.db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironment(t)
			directory := t.TempDir()
			t.Setenv("APPDATA_DIR", directory)
			if test.existing != "" {
				if err := os.WriteFile(filepath.Join(directory, test.existing), []byte("existing state"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load()
			if err != nil || cfg.DatabasePath != filepath.Join(directory, test.want) {
				t.Fatalf("database = %q, err = %v", cfg.DatabasePath, err)
			}
			if test.existing != "" {
				raw, err := os.ReadFile(cfg.DatabasePath)
				if err != nil || string(raw) != "existing state" {
					t.Fatal("existing database was modified")
				}
			}
		})
	}
}

func TestDatabaseDiscoveryRefusesAmbiguousOrUnsafePaths(t *testing.T) {
	for _, test := range []string{"both databases", "symlink", "directory"} {
		t.Run(test, func(t *testing.T) {
			directory := t.TempDir()
			legacy := filepath.Join(directory, "buntzen.db")
			switch test {
			case "both databases":
				for _, path := range []string{legacy, filepath.Join(directory, "lake-pass-bot.db")} {
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(directory, "missing"), legacy); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(legacy, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := databasePath(directory); err == nil {
				t.Fatal("unsafe database selection was accepted")
			}
		})
	}
}
