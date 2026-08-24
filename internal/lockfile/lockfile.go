// Package lockfile provides process-scoped advisory locks for the supported
// macOS and Linux deployments. Lock files are retained to avoid inode races.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrLocked = errors.New("another process holds the lock")

type Lock struct {
	file *os.File
}

// Acquire waits for exclusive ownership of path.
func Acquire(path string) (*Lock, error) {
	return acquire(path, false)
}

// TryAcquire obtains exclusive ownership or returns ErrLocked immediately.
func TryAcquire(path string) (*Lock, error) {
	return acquire(path, true)
}

func acquire(path string, nonblocking bool) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure lock file: %w", err)
	}
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("release lock: %w", unlockErr)
	}
	return closeErr
}
