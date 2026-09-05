package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

var ErrArtifactLimit = errors.New("job diagnostics exceeded the storage limit")

const (
	maintenanceInterval     = time.Hour
	artifactMonitorInterval = time.Second
	maxJobArtifactBytes     = 64 << 20
	maxJobArtifactFiles     = 64
	managedProfileMarker    = ".buntzen-managed"
)

func (e *Engine) maintenanceLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		if purged, err := e.maintain(e.ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("control-plane maintenance failed", "error", err)
		} else if err == nil {
			slog.Debug("control-plane maintenance completed", "expired_sessions_purged", purged)
		}
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) maintain(ctx context.Context) (int64, error) {
	purged, err := e.store.PurgeExpiredSessions(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := e.store.SystemPruneTerminalJobs(ctx, time.Now().UTC()); err != nil {
		return purged, err
	}
	if err := e.cleanupArtifacts(ctx); err != nil {
		return purged, err
	}
	if err := e.cleanupProfiles(ctx); err != nil {
		return purged, err
	}
	return purged, nil
}

func (e *Engine) cleanupArtifacts(ctx context.Context) error {
	entries, err := os.ReadDir(e.config.ArtifactsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read artifact directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		jobID, err := strconv.ParseInt(strings.TrimPrefix(entry.Name(), "job-"), 10, 64)
		if err != nil || jobID <= 0 || entry.Name() != fmt.Sprintf("job-%d", jobID) {
			continue
		}
		path, err := safeChild(e.config.ArtifactsDir, entry.Name())
		if err != nil {
			return err
		}
		if job, err := e.store.SystemGetJob(ctx, jobID); err == nil {
			if job.Status.Terminal() {
				exceeded, footprintErr := artifactLimitExceeded(path)
				if footprintErr != nil {
					return fmt.Errorf("inspect retained job artifacts: %w", footprintErr)
				}
				if exceeded {
					if err := os.RemoveAll(path); err != nil {
						return fmt.Errorf("remove oversized job artifacts: %w", err)
					}
				}
			}
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("check retained artifact job: %w", err)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove expired job artifacts: %w", err)
		}
	}
	return nil
}

func (e *Engine) cleanupProfiles(ctx context.Context) error {
	profiles, err := e.store.SystemListProfiles(ctx)
	if err != nil {
		return fmt.Errorf("list retained browser profiles: %w", err)
	}
	return e.cleanupProfileEntries(ctx, profiles)
}

// ReconcileStorage removes managed browser and job-artifact directories whose
// durable owners no longer exist. It is safe to call after an administrator
// deletes a disabled member; periodic maintenance remains the retry path.
func (e *Engine) ReconcileStorage(ctx context.Context) error {
	if err := e.cleanupArtifacts(ctx); err != nil {
		return err
	}
	return e.cleanupProfiles(ctx)
}

func (e *Engine) cleanupProfileEntries(ctx context.Context, snapshot []model.Profile) error {
	retained := make(map[int64]model.Profile, len(snapshot))
	for _, profile := range snapshot {
		retained[profile.ID] = profile
	}
	entries, err := os.ReadDir(e.config.ProfilesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read browser profile directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		profileID, ok := managedProfileID(entry.Name())
		if !ok {
			continue
		}
		path, err := safeChild(e.config.ProfilesDir, entry.Name())
		if err != nil {
			continue
		}
		markerUserID, markerProfileID, err := readManagedProfileMarker(path)
		if err != nil || markerProfileID != profileID {
			// Unmarked, malformed, or mismatched directories may belong to the
			// host. Never delete them automatically.
			continue
		}
		profile, retainedNow := retained[profileID]
		if !retainedNow {
			// The initial snapshot can miss a profile created concurrently. An
			// exact recheck is sufficient because AUTOINCREMENT profile IDs are
			// immutable and never reused after deletion.
			profile, err = e.store.SystemGetProfile(ctx, profileID)
			if err == nil {
				retainedNow = true
			} else if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("recheck browser profile ownership: %w", err)
			}
		}
		if retainedNow {
			if profile.UserID != markerUserID {
				return fmt.Errorf("browser profile %d has a mismatched ownership marker", profileID)
			}
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove orphaned browser profile: %w", err)
		}
	}
	return nil
}

func (e *Engine) monitorArtifacts(ctx context.Context, jobID int64, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(artifactMonitorInterval)
	defer ticker.Stop()
	artifactDir, err := safeChild(e.config.ArtifactsDir, fmt.Sprintf("job-%d", jobID))
	if err != nil {
		cancel(ErrArtifactLimit)
		return
	}
	for {
		exceeded, err := artifactLimitExceeded(artifactDir)
		if err != nil {
			slog.Error("job artifact inspection failed", "job_id", jobID, "error", err)
			cancel(ErrArtifactLimit)
			return
		}
		if exceeded {
			cancel(ErrArtifactLimit)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) enforceArtifactLimit(jobID int64) error {
	artifactDir, err := safeChild(e.config.ArtifactsDir, fmt.Sprintf("job-%d", jobID))
	if err != nil {
		return err
	}
	exceeded, err := artifactLimitExceeded(artifactDir)
	if err != nil {
		return err
	}
	if !exceeded {
		return nil
	}
	if err := os.RemoveAll(artifactDir); err != nil {
		return fmt.Errorf("remove oversized job artifacts: %w", err)
	}
	return nil
}

func ensureManagedProfileDirectory(parent string, profile model.Profile) (string, error) {
	if profile.ID <= 0 || profile.UserID <= 0 {
		return "", errors.New("browser profile identity is invalid")
	}
	name := fmt.Sprintf("profile-%d", profile.ID)
	path, err := safeChild(parent, name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", fmt.Errorf("create browser profile: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return "", fmt.Errorf("inspect browser profile: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("browser profile path must be a real directory")
	}

	markerUserID, markerProfileID, err := readManagedProfileMarker(path)
	if err == nil {
		if markerUserID != profile.UserID || markerProfileID != profile.ID {
			return "", errors.New("browser profile directory belongs to a different profile")
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("inspect unmarked browser profile: %w", err)
	}
	if len(entries) != 0 {
		return "", errors.New("refusing to adopt an unmarked browser profile directory")
	}
	markerPath := filepath.Join(path, managedProfileMarker)
	markerFile, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create browser profile marker: %w", err)
	}
	_, writeErr := markerFile.WriteString(managedProfileMarkerValue(profile.UserID, profile.ID))
	closeErr := markerFile.Close()
	if writeErr != nil {
		return "", fmt.Errorf("write browser profile marker: %w", writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close browser profile marker: %w", closeErr)
	}
	return path, nil
}

func managedProfileID(name string) (int64, bool) {
	if !strings.HasPrefix(name, "profile-") {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(name, "profile-"), 10, 64)
	if err != nil || id <= 0 || name != fmt.Sprintf("profile-%d", id) {
		return 0, false
	}
	return id, true
}

func managedProfileMarkerValue(userID, profileID int64) string {
	return fmt.Sprintf("buntzen-profile-v1:%d:%d\n", userID, profileID)
}

func readManagedProfileMarker(path string) (int64, int64, error) {
	markerPath := filepath.Join(path, managedProfileMarker)
	marker, err := os.Lstat(markerPath)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect browser profile marker: %w", err)
	}
	if !marker.Mode().IsRegular() {
		return 0, 0, errors.New("browser profile marker must be a regular file")
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read browser profile marker: %w", err)
	}
	if len(raw) > 128 {
		return 0, 0, errors.New("browser profile marker is malformed")
	}
	var userID, profileID int64
	if count, err := fmt.Sscanf(string(raw), "buntzen-profile-v1:%d:%d\n", &userID, &profileID); err != nil || count != 2 ||
		userID <= 0 || profileID <= 0 || string(raw) != managedProfileMarkerValue(userID, profileID) {
		return 0, 0, errors.New("browser profile marker is malformed")
	}
	return userID, profileID, nil
}

func artifactLimitExceeded(path string) (bool, error) {
	var bytes int64
	files := 0
	err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		if files > maxJobArtifactFiles || bytes > maxJobArtifactBytes {
			return ErrArtifactLimit
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, ErrArtifactLimit) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func safeChild(parent, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", errors.New("child path must be one directory name")
	}
	path := filepath.Join(parent, name)
	return filepath.Abs(path)
}
