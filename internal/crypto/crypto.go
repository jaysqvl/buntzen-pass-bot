// Package crypto provides convenience encryption for secrets stored in SQLite.
// Keep the key separately from database backups. The legacy default beside the
// database protects only a copied database, not a copy of all of APPDATA_DIR.
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

	"golang.org/x/sys/unix"
)

const (
	keySize         = 32
	envelopeV1      = "v1"
	fileKeyPrefix   = "buntzen-key-v1:"
	maxKeyFileBytes = 128
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
	defer clear(key)
	return New(key)
}

// LoadExisting never creates a replacement key or its parent directory.
func LoadExisting(path string) (*Encryptor, error) {
	key, err := loadKey(path)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return New(key)
}

// LoadForDatabase permits automatic key generation only for a confidently new
// legacy installation. An explicit key path is always existing-key-only.
func LoadForDatabase(path, databasePath string, explicit bool) (*Encryptor, error) {
	if explicit {
		return LoadExisting(path)
	}
	box, err := LoadExisting(path)
	if !errors.Is(err, os.ErrNotExist) {
		return box, err
	}
	if _, statErr := os.Lstat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		if statErr != nil {
			return nil, fmt.Errorf("inspect database before key creation: %w", statErr)
		}
		return nil, errors.New("encryption key is missing for an existing database; restore its matching key")
	}
	return LoadOrCreate(path)
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
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open encryption key", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var metadata unix.Stat_t
	if err := unix.Fstat(fd, &metadata); err != nil {
		return nil, fmt.Errorf("inspect encryption key: %w", err)
	}
	if metadata.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("encryption key must be a regular file")
	}
	if metadata.Uid != uint32(os.Geteuid()) || metadata.Mode&0o077 != 0 {
		return nil, errors.New("encryption key must belong to the service user and be inaccessible to group and others")
	}
	if metadata.Size > maxKeyFileBytes {
		return nil, errors.New("encryption key file is too large")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read encryption key: %w", err)
	}
	defer clear(raw)
	if len(raw) > maxKeyFileBytes {
		return nil, errors.New("encryption key file is too large")
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, fileKeyPrefix) {
		return nil, errors.New("encryption key has an unsupported format")
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimPrefix(text, fileKeyPrefix))
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
	defer clear(data)
	file, err := os.CreateTemp(filepath.Dir(path), ".master-key-*")
	if err != nil {
		return nil, fmt.Errorf("create encryption key temporary file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if n, err := file.Write(data); err != nil || n != len(data) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, fmt.Errorf("write encryption key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync encryption key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close encryption key: %w", err)
	}
	// Publish only a complete file and never overwrite the winner of a
	// concurrent first start. A plain rename would replace the existing key.
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return loadKey(path)
		}
		return nil, fmt.Errorf("publish encryption key: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open encryption key directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return nil, fmt.Errorf("sync encryption key directory: %w", err)
	}
	return key, nil
}
