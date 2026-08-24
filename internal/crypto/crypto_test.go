package crypto

import (
	"bytes"
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
	plaintext, err := box.Decrypt(ciphertext)
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
	last := ciphertext[len(ciphertext)-1]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	ciphertext = ciphertext[:len(ciphertext)-1] + string(replacement)
	if _, err := box.Decrypt(ciphertext); err == nil {
		t.Fatal("expected authenticated decryption to fail")
	}
}
