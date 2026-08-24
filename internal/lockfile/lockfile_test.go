package lockfile

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.lock")
	first, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := TryAcquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
