// Package config loads the single environment-backed control-plane configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/origin"
)

const (
	defaultListenAddress = ":8080"
	defaultMaxJobs       = 2
	DefaultYodelOrigin   = "https://yodelportal.com"
)

type Config struct {
	AppDataDir        string
	DatabasePath      string
	EncryptionKeyPath string
	ProfilesDir       string
	ArtifactsDir      string
	ListenAddress     string
	MaxConcurrentJobs int
	SchedulesEnabled  bool
	PythonExecutable  string
	PythonModule      string
	BlueBubblesURL    string
	YodelOrigins      []string
	AllowedOrigins    []string
	AllowedHosts      []string
	SetupToken        string
	LogLevel          string
}

func Load() (Config, error) {
	appData := strings.TrimSpace(os.Getenv("APPDATA_DIR"))
	if appData == "" {
		appData = "./appdata"
	}
	abs, err := filepath.Abs(appData)
	if err != nil {
		return Config{}, fmt.Errorf("resolve APPDATA_DIR: %w", err)
	}

	listen := strings.TrimSpace(os.Getenv("BUNTZEN_LISTEN"))
	if listen == "" {
		listen = defaultListenAddress
	}
	maxJobs, err := boundedInt("MAX_CONCURRENT_JOBS", defaultMaxJobs, 1, 8)
	if err != nil {
		return Config{}, err
	}
	schedules, err := boolValue("SCHEDULES_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	debug, err := boolValue("BUNTZEN_DEBUG", false)
	if err != nil {
		return Config{}, err
	}
	logLevel, err := logLevelValue(os.Getenv("BUNTZEN_LOG_LEVEL"), debug)
	if err != nil {
		return Config{}, err
	}
	python := strings.TrimSpace(os.Getenv("BUNTZEN_PYTHON"))
	if python == "" {
		python = "python3"
	}
	module := strings.TrimSpace(os.Getenv("BUNTZEN_ACTIONS_MODULE"))
	if module == "" {
		module = "buntzen_actions"
	}
	blueBubblesURL := strings.TrimSpace(os.Getenv("BLUEBUBBLES_URL"))
	if blueBubblesURL == "" {
		blueBubblesURL = "http://127.0.0.1:1234"
	}
	allowedOrigins, err := originList("BUNTZEN_ALLOWED_ORIGINS")
	if err != nil {
		return Config{}, err
	}
	yodelOrigins, err := yodelOriginList("BUNTZEN_YODEL_ORIGINS")
	if err != nil {
		return Config{}, err
	}
	allowedHosts, err := hostList("BUNTZEN_ALLOWED_HOSTS")
	if err != nil {
		return Config{}, err
	}
	seenHosts := make(map[string]struct{}, len(allowedHosts)+len(allowedOrigins))
	for _, host := range allowedHosts {
		seenHosts[host] = struct{}{}
	}
	for _, allowedOrigin := range allowedOrigins {
		parsed, _ := url.Parse(allowedOrigin)
		host, err := origin.Host(parsed.Host)
		if err != nil {
			return Config{}, err
		}
		if _, exists := seenHosts[host]; !exists {
			allowedHosts = append(allowedHosts, host)
			seenHosts[host] = struct{}{}
		}
	}
	return Config{
		AppDataDir:        abs,
		DatabasePath:      filepath.Join(abs, "buntzen.db"),
		EncryptionKeyPath: filepath.Join(abs, "master.key"),
		ProfilesDir:       filepath.Join(abs, "profiles"),
		ArtifactsDir:      filepath.Join(abs, "artifacts"),
		ListenAddress:     listen,
		MaxConcurrentJobs: maxJobs,
		SchedulesEnabled:  schedules,
		PythonExecutable:  python,
		PythonModule:      module,
		BlueBubblesURL:    blueBubblesURL,
		YodelOrigins:      yodelOrigins,
		AllowedOrigins:    allowedOrigins,
		AllowedHosts:      allowedHosts,
		SetupToken:        strings.TrimSpace(os.Getenv("BUNTZEN_SETUP_TOKEN")),
		LogLevel:          logLevel,
	}, nil
}

// EffectiveLogLevel returns the validated level used by both the Go control
// plane and its isolated Python workers. Config values assembled directly in
// tests retain the production-safe info default.
func (c Config) EffectiveLogLevel() string {
	level, err := logLevelValue(c.LogLevel, false)
	if err != nil {
		return "info"
	}
	return level
}

func logLevelValue(raw string, debug bool) (string, error) {
	if debug {
		return "debug", nil
	}
	level := strings.ToLower(strings.TrimSpace(raw))
	if level == "" {
		return "info", nil
	}
	if level == "warning" {
		level = "warn"
	}
	switch level {
	case "debug", "info", "warn", "error":
		return level, nil
	default:
		return "", errors.New("BUNTZEN_LOG_LEVEL must be debug, info, warn, or error")
	}
}

// yodelOriginList returns the exact HTTPS origins that may receive Yodel
// credentials. The operator may override the production default for a trusted
// test deployment, but booking records cannot expand this boundary.
func yodelOriginList(name string) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return []string{DefaultYodelOrigin}, nil
	}
	values, err := parseOriginList(name, raw)
	if err != nil {
		return nil, err
	}
	for _, value := range values {
		if !strings.HasPrefix(value, "https://") {
			return nil, errors.New(name + " may contain only HTTPS origins")
		}
	}
	return values, nil
}

func hostList(name string) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, errors.New(name + " must be a comma-separated list of hosts")
		}
		canonical, err := origin.Host(value)
		if err != nil {
			return nil, fmt.Errorf("%s contains an invalid host: %w", name, err)
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		values = append(values, canonical)
	}
	return values, nil
}

// originList parses an optional comma-separated list of exact browser origins
// trusted when a reverse proxy changes the Host header seen by Buntzen.
func originList(name string) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	return parseOriginList(name, raw)
}

func parseOriginList(name, raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, errors.New(name + " must be a comma-separated list of origins")
		}
		canonical, err := origin.Canonical(value)
		if err != nil {
			return nil, fmt.Errorf("%s contains an invalid origin: %w", name, err)
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		values = append(values, canonical)
	}
	return values, nil
}

func (c Config) EnsureDirectories() error {
	for _, path := range []string{c.AppDataDir, c.ProfilesDir, c.ArtifactsDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", path, err)
		}
	}
	return nil
}

func boundedInt(name string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", name, min, max)
	}
	return value, nil
}

func boolValue(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New(name + " must be true or false")
	}
	return value, nil
}
