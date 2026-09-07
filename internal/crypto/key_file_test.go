package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKeyLoaderRejectsSymlinkAndOversizedFile(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "valid.key")
	if _, err := LoadOrCreate(valid); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.key")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(link); err == nil {
		t.Error("symlink key accepted")
	}
	large := filepath.Join(directory, "large.key")
	if err := os.WriteFile(large, append([]byte(strings.Repeat(" ", 1<<20)), raw...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(large); err == nil {
		t.Error("oversized key file accepted")
	}
}

func TestExistingKeyPrivateModesAndNonregularFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "master.key")
	box, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Encrypt([]byte("synthetic key relocation control"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o400, 0o600} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		opened, err := LoadExisting(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := opened.Decrypt(encrypted); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatal("loader rewrote key permissions")
		}
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExisting(path); err == nil {
		t.Fatal("group-readable key accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("loader changed key bytes")
	}
	fifo := filepath.Join(directory, "pipe")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, nonregular := range []string{directory, fifo, "/dev/null"} {
		done := make(chan error, 1)
		go func() { _, err := LoadExisting(nonregular); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("nonregular key accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("nonregular key blocked loader")
		}
	}
}

func TestConcurrentKeyCreationPublishesOneCompleteKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	const starters = 32
	results := make(chan *Encryptor, starters)
	errorsFound := make(chan error, starters)
	var group sync.WaitGroup
	for i := 0; i < starters; i++ {
		group.Add(1)
		go func() { defer group.Done(); box, err := LoadOrCreate(path); results <- box; errorsFound <- err }()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent creator saw partial state: %v", err)
		}
	}
	canonical, err := LoadExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	for box := range results {
		encrypted, err := box.Encrypt([]byte("synthetic concurrent key canary"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := canonical.Decrypt(encrypted); err != nil {
			t.Fatal("creators used different keys")
		}
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".master-key-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files remain: %d %v", len(matches), err)
	}
}
