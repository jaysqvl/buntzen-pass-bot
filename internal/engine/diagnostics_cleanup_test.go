package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestMaintenanceRemovesLegacyCompletedJobDiagnostics(t *testing.T) {
	f := newEngineTestFixture(t)
	ctx := context.Background()
	job, err := f.resources.EnqueueJob(ctx, store.EnqueueJobParams{BookingRequestID: &f.booking.ID, Command: model.CommandDryRun, RunMode: model.RunModeDryRun})
	if err != nil {
		t.Fatal(err)
	}
	job, err = f.store.SystemTransitionJob(ctx, job.ID, []model.JobStatus{model.JobQueued}, model.JobCancelled, store.JobTransition{})
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(f.engine.config.ArtifactsDir, fmt.Sprintf("job-%d", job.ID))
	unknownDir := filepath.Join(f.engine.config.ArtifactsDir, "operator-notes")
	for _, dir := range []string{artifactDir, unknownDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "trace-1.zip"), []byte("synthetic secret archive"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.engine.cleanupArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifactDir); !os.IsNotExist(err) {
		t.Fatalf("legacy capture retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unknownDir, "trace-1.zip")); err != nil {
		t.Fatalf("unmanaged operator file changed: %v", err)
	}
	if _, err := f.resources.GetJob(ctx, job.ID); err != nil {
		t.Fatalf("job record removed: %v", err)
	}
}
