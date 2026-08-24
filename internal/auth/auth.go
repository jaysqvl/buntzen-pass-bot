// Package auth implements local account password and session primitives.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonVersion = 19
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
	tokenBytes   = 32
)

// Argon2id intentionally uses 64 MiB per operation. Keep process-wide memory
// bounded even if several account, login, and recovery paths are exercised at
// once. Web handlers add stricter per-account admission and rate controls.
var passwordWork = make(chan struct{}, 2)

func beginPasswordWork() func() {
	passwordWork <- struct{}{}
	return func() { <-passwordWork }
}

func HashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	release := beginPasswordWork()
	defer release()
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("password hash has an unsupported format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argonVersion {
		return false, errors.New("password hash has an unsupported version")
	}
	var memory uint32
	var iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, errors.New("password hash parameters are malformed")
	}
	if memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || threads < 1 || threads > 8 {
		return false, errors.New("password hash parameters are outside safe bounds")
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, errors.New("password hash salt is malformed")
	}
	expected, err := b64.DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return false, errors.New("password hash is malformed")
	}
	release := beginPasswordWork()
	defer release()
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func NewToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func HashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// NormalizeUsername returns the stable login key for a display username.
// Usernames are deliberately restricted to a small ASCII set so case folding
// cannot vary across Go, SQLite, browsers, or future operating systems.
func NormalizeUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", errors.New("username is required")
	}
	if len(username) > 128 {
		return "", errors.New("username is too long")
	}
	for _, character := range username {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("._@+-", character) {
			continue
		}
		return "", errors.New("username may use only letters, digits, dot, underscore, at, plus, and hyphen")
	}
	return strings.ToLower(username), nil
}

// EqualizePasswordCheck performs the same Argon2 work as one password
// verification without requiring a real account hash.
func EqualizePasswordCheck(password string) {
	release := beginPasswordWork()
	defer release()
	_ = argon2.IDKey([]byte(password), []byte("buntzen-dummy-v1"), argonTime, argonMemory, argonThreads, argonKeyLen)
}

func validatePassword(password string) error {
	if len(strings.TrimSpace(password)) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	if len(password) > 1024 {
		return errors.New("password is too long")
	}
	return nil
}
