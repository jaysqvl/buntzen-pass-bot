package crypto

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appdata", "master.key")
	box, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Encrypt([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, "correct horse") {
		t.Fatal("ciphertext contains plaintext")
	}
	// Reopening must reuse the persisted key, as it does after a service restart.
	reopened, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := reopened.Decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, []byte("correct horse battery staple")) {
		t.Fatalf("got %q", plaintext)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("key mode = %o", got)
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{1}, keySize))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, ok := strings.Cut(ciphertext, ":")
	if !ok {
		t.Fatal("ciphertext is missing its version prefix")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 1
	ciphertext = envelopeV1 + ":" + base64.RawURLEncoding.EncodeToString(payload)
	if _, err := box.Decrypt(ciphertext); err == nil {
		t.Fatal("expected authenticated decryption to fail")
	}
}
