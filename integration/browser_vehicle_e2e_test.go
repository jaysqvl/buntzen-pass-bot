//go:build integration

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPythonBrowserVehicleSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("real-browser integration test")
	}
	repoRoot := repositoryRoot(t)
	python, args := pythonCommand(t, repoRoot, filepath.Join(repoRoot, "integration", "browser_vehicle_cases.py"))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, args...)
	command.Dir = repoRoot
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(repoRoot, "actions", "src"),
		"LAKE_PASS_E2E_BROWSER_EXECUTABLE="+browserPath(),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("vehicle browser regressions: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
