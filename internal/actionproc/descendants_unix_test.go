//go:build darwin || linux

package actionproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalWorkerExitStopsProbeDescendants(t *testing.T) {
	const helper = "probe-descendants"
	switch os.Getenv("BUNTZEN_ACTIONPROC_HELPER") {
	case helper + "-child":
		time.Sleep(time.Second)
		_ = os.WriteFile(os.Args[len(os.Args)-1], []byte("descendant survived"), 0600)
		os.Exit(0)
	case helper:
		child := exec.Command(os.Args[0], "-test.run=^TestNormalWorkerExitStopsProbeDescendants$", "--", os.Args[len(os.Args)-1])
		child.Env = []string{"BUNTZEN_ACTIONPROC_HELPER=" + helper + "-child"}
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, `{"v":2,"type":"run.complete","status":"failed"}`)
		os.Exit(0)
	}
	marker := filepath.Join(t.TempDir(), "survived")
	session, err := Start(t.Context(), Config{Executable: os.Args[0], Args: []string{"-test.run=^TestNormalWorkerExitStopsProbeDescendants$", "--", marker}, Environment: []string{"BUNTZEN_ACTIONPROC_HELPER=" + helper}})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Events() {
	}
	if result := <-session.Done(); result.Err != nil {
		t.Fatal(result.Err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker left a running probe descendant: %v", err)
	}
}
