package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
	"golang.org/x/sys/unix"
)

func TestOversizedBrowserProfileIsRejectedBeforeWorkerLaunch(t *testing.T) {
	fixture := newEngineTestFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":200,"data":"pong"}`)) }))
	defer provider.Close()
	job := configureBudgetFixture(t, &fixture, provider.URL, model.CommandAuthCheck, "success")
	profile, err := fixture.resources.GetProfile(context.Background(), job.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := ensureManagedProfileDirectory(fixture.engine.config.ProfilesDir, profile)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(profilePath, "saved-session")
	if err := os.WriteFile(sentinel, []byte("synthetic saved login"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(profilePath, "oversized-cache"))
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file exercises the size boundary without allocating 512 MiB.
	if err := file.Truncate((512 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	_, err = fixture.engine.executeWithBudgets(context.Background(), job, 3*time.Second, checkoutExecutionGrace)
	if err == nil {
		t.Error("oversized profile reached worker launch")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.engine.config.PythonExecutable), "started")); !os.IsNotExist(err) {
		t.Error("worker started with oversized profile")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "synthetic saved login" {
		t.Fatal("profile login data removed on quota failure")
	}
}

func TestArtifactDirectoriesCannotBypassEntryBudget(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 65; index++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("empty-%d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exceeded, err := artifactLimitExceeded(root)
	if err != nil || !exceeded {
		t.Fatalf("empty directories bypassed artifact entry budget: exceeded=%v err=%v", exceeded, err)
	}
}

func TestStorageBudgetCountsEntriesWithoutFollowingLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "data"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside-canary"), []byte("larger than this profile budget"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if err := checkStorageBudget(context.Background(), root, storageBudget{maxBytes: 4, maxEntries: 3}); err != nil {
		t.Fatalf("healthy tree or Chromium-style symlink rejected: %v", err)
	}
	for _, budget := range []storageBudget{{maxBytes: 3, maxEntries: 3}, {maxBytes: 4, maxEntries: 2}} {
		if err := checkStorageBudget(context.Background(), root, budget); !errors.Is(err, errFootprintLimit) {
			t.Fatalf("budget not enforced: %v", err)
		}
	}
	if err := checkStorageBudget(context.Background(), filepath.Join(root, "SingletonLock"), storageBudget{maxBytes: 100, maxEntries: 100}); err == nil {
		t.Fatal("root symlink was followed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkStorageBudget(ctx, root, storageBudget{maxBytes: 100, maxEntries: 100}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan kept traversing: %v", err)
	}
}

func TestStorageBudgetStopsDeepAndWideTrees(t *testing.T) {
	for _, deep := range []bool{false, true} {
		root := t.TempDir()
		directory := root
		for index := 0; index < 130; index++ {
			path := filepath.Join(directory, fmt.Sprintf("d%d", index))
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if deep {
				directory = path
			}
		}
		budget := storageBudget{maxBytes: 1, maxEntries: 129}
		if deep {
			budget.maxEntries = 20000
		}
		if err := checkStorageBudget(context.Background(), root, budget); !errors.Is(err, errFootprintLimit) {
			t.Fatalf("deep=%t tree bypassed bound: %v", deep, err)
		}
	}
}

func TestProfileMarkersAreBoundedAndNeverFollowSpecialFiles(t *testing.T) {
	for _, kind := range []string{"oversized", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, managedProfileMarker)
			switch kind {
			case "oversized":
				file, err := os.Create(marker)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(1 << 20); err != nil {
					t.Fatal(err)
				}
				file.Close()
			case "symlink":
				target := filepath.Join(t.TempDir(), "marker")
				if err := os.WriteFile(target, []byte(managedProfileMarkerValue(1, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, marker); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(marker, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, _, err := readManagedProfileMarker(root); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe marker accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("marker read blocked")
			}
		})
	}
}

func TestGrowingProfileStopsWorkerAndPreservesLoginForRecovery(t *testing.T) {
	fixture := newEngineTestFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":200,"data":"pong"}`)) }))
	defer provider.Close()
	job := configureBudgetFixture(t, &fixture, provider.URL, model.CommandAuthCheck, "growth")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := fixture.engine.executeWithBudgets(ctx, job, 6*time.Second, checkoutExecutionGrace)
	if err != nil || result.Status != model.JobFailed || result.Message != profileLimitMessage {
		t.Fatalf("growth result=%+v err=%v", result, err)
	}
	if ctx.Err() != nil {
		t.Fatal("profile monitor relied on external timeout")
	}
	fixture.engine.finish(job.ID, result.Status, result.Message, &result.ExitCode)
	profilePath := filepath.Join(fixture.engine.config.ProfilesDir, fmt.Sprintf("profile-%d", job.ProfileID))
	if raw, err := os.ReadFile(filepath.Join(profilePath, "saved-session")); err != nil || string(raw) != "synthetic saved login" {
		t.Fatal("growth limit destroyed saved login")
	}
	// Only this simulated operator recovery removes the synthetic oversized cache.
	if err := os.Remove(filepath.Join(profilePath, "oversized-cache")); err != nil {
		t.Fatal(err)
	}
	retry := configureBudgetFixture(t, &fixture, provider.URL, model.CommandAuthCheck, "success")
	result, err = fixture.engine.executeWithBudgets(ctx, retry, 3*time.Second, checkoutExecutionGrace)
	if err != nil || result.Status != model.JobSucceeded {
		t.Fatalf("healthy recovered profile could not run: %+v %v", result, err)
	}
}

func TestProfileLimitAfterConfirmationRetainsReservation(t *testing.T) {
	for _, mode := range []string{"growth-confirm", "growth-completed"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":200,"data":"pong"}`)) }))
			defer provider.Close()
			job := configureBudgetFixture(t, &fixture, provider.URL, model.CommandBook, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			result, err := fixture.engine.executeWithBudgets(ctx, job, 6*time.Second, checkoutExecutionGrace)
			want := model.JobOutcomeUnknown
			if mode == "growth-completed" {
				want = model.JobSucceeded
			}
			if err != nil || result.Status != want || ctx.Err() != nil {
				t.Fatalf("post-confirmation storage limit result=%+v err=%v context=%v", result, err, ctx.Err())
			}
			fixture.engine.finish(job.ID, result.Status, result.Message, &result.ExitCode)
			if _, err := fixture.engine.QueueBookingNow(context.Background(), fixture.user.ID, fixture.booking.ID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("storage cancellation released reservation: %v", err)
			}
		})
	}
}
