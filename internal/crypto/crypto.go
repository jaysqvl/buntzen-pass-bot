// Package crypto provides convenience encryption for secrets stored in SQLite.
// The key lives beside the database, so this protects a copied database rather
// than an attacker who can copy all of APPDATA_DIR.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	keySize       = 32
	envelopeV1    = "v1"
	fileKeyPrefix = "buntzen-key-v1:"
)

type Encryptor struct {
	aead cipher.AEAD
}

func LoadOrCreate(path string) (*Encryptor, error) {
	key, err := loadKey(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err = createKey(path)
	}
	if err != nil {
		return nil, err
	}
	return New(key)
}

func New(key []byte) (*Encryptor, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("encryption key must be %d bytes", keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &Encryptor{aead: aead}, nil
}

func (e *Encryptor) Encrypt(plaintext []byte) (string, error) {
	if e == nil || e.aead == nil {
		return "", errors.New("encryptor is not initialized")
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := e.aead.Seal(nil, nonce, plaintext, []byte(envelopeV1))
	payload := append(nonce, sealed...)
	return envelopeV1 + ":" + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (e *Encryptor) Decrypt(envelope string) ([]byte, error) {
	if e == nil || e.aead == nil {
		return nil, errors.New("encryptor is not initialized")
	}
	version, encoded, ok := strings.Cut(envelope, ":")
	if !ok || version != envelopeV1 {
		return nil, errors.New("unsupported encrypted value")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("encrypted value is malformed")
	}
	if len(payload) < e.aead.NonceSize() {
		return nil, errors.New("encrypted value is truncated")
	}
	nonce, ciphertext := payload[:e.aead.NonceSize()], payload[e.aead.NonceSize():]
	plaintext, err := e.aead.Open(nil, nonce, ciphertext, []byte(envelopeV1))
	if err != nil {
		return nil, errors.New("encrypted value failed authentication")
	}
	return plaintext, nil
}

func loadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("encryption key %s must not be accessible to group or others", path)
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, fileKeyPrefix) {
		return nil, errors.New("encryption key has an unsupported format")
	}
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(text, fileKeyPrefix))
	if err != nil || len(key) != keySize {
		return nil, errors.New("encryption key is malformed")
	}
	return key, nil
}

func createKey(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}
	data := []byte(fileKeyPrefix + base64.RawStdEncoding.EncodeToString(key) + "\n")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create encryption key: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return nil, fmt.Errorf("write encryption key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync encryption key: %w", err)
	}
	return key, nil
}
