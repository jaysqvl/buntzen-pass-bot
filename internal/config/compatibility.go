package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env gives the neutral name precedence, including an explicitly empty value.
// Old deployment settings remain usable until an operator replaces them.
func Env(name string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	if suffix, ok := strings.CutPrefix(name, "LAKE_PASS_"); ok {
		return os.Getenv("BUNTZEN_" + suffix)
	}
	return ""
}

// Keep an existing database in place: moving SQLite without its WAL or opening
// a second database could lose state or bypass outstanding reservations.
func databasePath(directory string) (string, error) {
	current := filepath.Join(directory, "lake-pass-bot.db")
	legacy := filepath.Join(directory, "buntzen.db")
	currentExists, err := databaseExists(current)
	if err != nil {
		return "", err
	}
	legacyExists, err := databaseExists(legacy)
	if err != nil {
		return "", err
	}
	if currentExists && legacyExists {
		return "", errors.New("both lake-pass-bot.db and the legacy database exist; choose the intended appdata directory before starting")
	}
	if legacyExists {
		return legacy, nil
	}
	return current, nil
}

func databaseExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("database path must be a regular file")
	}
	return true, nil
}
