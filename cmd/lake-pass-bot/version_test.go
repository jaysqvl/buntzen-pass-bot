package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/buildinfo"
)

func TestVersionCommandDoesNotLoadRuntimeConfigurationOrCreateAppdata(t *testing.T) {
	appdata := filepath.Join(t.TempDir(), "uninitialized-appdata")
	command := exec.Command(os.Args[0], "-test.run=^TestVersionCommandProcess$")
	command.Env = append(os.Environ(),
		"LAKE_PASS_VERSION_TEST_PROCESS=1",
		"APPDATA_DIR="+appdata,
		"MAX_CONCURRENT_JOBS=invalid-runtime-setting",
		"LAKE_PASS_VERSION=999.999.999",
		"LAKE_PASS_REVISION=runtime-overrides-do-not-identify-a-build",
	)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("version command: %v", err)
	}
	var report struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode version report %q: %v", output, err)
	}
	if report.Version != buildinfo.Version || report.Revision != buildinfo.Revision {
		t.Fatalf("version report = %+v; want build version %q revision %q", report, buildinfo.Version, buildinfo.Revision)
	}
	if _, err := os.Stat(appdata); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version command initialized appdata: %v", err)
	}
}

func TestVersionCommandRejectsArgumentsBeforeRuntimeSetup(t *testing.T) {
	t.Setenv("MAX_CONCURRENT_JOBS", "invalid-runtime-setting")
	if err := run(context.Background(), []string{"version", "unexpected"}); err == nil || err.Error() != "usage: lake-pass-bot version" {
		t.Fatalf("version with an argument: %v", err)
	}
}

func TestVersionCommandProcess(t *testing.T) {
	if os.Getenv("LAKE_PASS_VERSION_TEST_PROCESS") != "1" {
		return
	}
	if err := run(context.Background(), []string{"version"}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
