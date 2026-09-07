package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

var (
	errFootprintLimit    = errors.New("storage footprint limit exceeded")
	ErrProfileLimit      = errors.New("browser profile storage limit exceeded")
	ErrProfileInspection = errors.New("browser profile storage could not be inspected")
)

const (
	maxProfileBytes          int64 = 512 << 20
	maxProfileEntries              = 20000
	maxStorageDepth                = 64
	storageReadBatch               = 128
	profileMonitorInterval         = 2 * time.Second
	profileInspectionBudget        = 5 * time.Second
	profileLimitMessage            = "Browser profile storage exceeded its limit. Ask the operator to review its storage before retrying."
	profileInspectionMessage       = "Browser profile storage could not be inspected safely. Ask the operator to review it before retrying."
)

type storageBudget struct {
	maxBytes   int64
	maxEntries int
}

// checkStorageBudget enumerates in bounded batches through directory descriptors.
// It never follows symlinks or opens file contents. Counting all entries also
// bounds an attack consisting only of empty directories or symbolic links.
func checkStorageBudget(ctx context.Context, path string, budget storageBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget.maxBytes < 0 || budget.maxEntries < 0 {
		return errFootprintLimit
	}
	root, err := openStorageDirectory(unix.AT_FDCWD, path)
	if err != nil {
		return err
	}
	defer root.Close()
	var totalBytes int64
	var totalEntries int
	var visit func(*os.File, int) error
	visit = func(directory *os.File, depth int) error {
		if depth > maxStorageDepth {
			return errFootprintLimit
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			names, readErr := directory.Readdirnames(storageReadBatch)
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					return err
				}
				totalEntries++
				if totalEntries > budget.maxEntries {
					return errFootprintLimit
				}
				var metadata unix.Stat_t
				if err := unix.Fstatat(int(directory.Fd()), name, &metadata, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return err
				}
				switch metadata.Mode & unix.S_IFMT {
				case unix.S_IFREG:
					if metadata.Size < 0 || metadata.Size > budget.maxBytes-totalBytes {
						return errFootprintLimit
					}
					totalBytes += metadata.Size
				case unix.S_IFDIR:
					child, err := openStorageDirectory(int(directory.Fd()), name)
					if err != nil {
						// Cache entries may disappear or change type between stat and
						// open. No-follow ensures replacements cannot escape the root.
						if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
							continue
						}
						return err
					}
					err = visit(child, depth+1)
					child.Close()
					if err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return visit(root, 0)
}

func openStorageDirectory(parent int, path string) (*os.File, error) {
	fd, err := unix.Openat(parent, path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func inspectProfileStorage(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, profileInspectionBudget)
	defer cancel()
	err := checkStorageBudget(ctx, path, storageBudget{maxBytes: maxProfileBytes, maxEntries: maxProfileEntries})
	if errors.Is(err, errFootprintLimit) {
		return ErrProfileLimit
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProfileInspection, err)
	}
	return nil
}

func monitorProfileStorage(parent context.Context, path string, cancelJob context.CancelCauseFunc) func() {
	ctx, stop := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(profileMonitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			err := inspectProfileStorage(ctx, path)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Warn("browser profile storage check stopped job", "error", err)
				if errors.Is(err, ErrProfileLimit) {
					cancelJob(ErrProfileLimit)
				} else {
					cancelJob(ErrProfileInspection)
				}
				return
			}
		}
	}()
	return func() { stop(); <-done }
}

func executionLimitMessage(cause error) string {
	switch {
	case errors.Is(cause, ErrExecutionBudget):
		return executionBudgetMessage
	case errors.Is(cause, ErrProfileLimit):
		return profileLimitMessage
	case errors.Is(cause, ErrProfileInspection):
		return profileInspectionMessage
	default:
		return ""
	}
}
