//go:build integration

package integration_test

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
}

func pythonCommand(t *testing.T, repoRoot string, args ...string) (string, []string) {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("BUNTZEN_E2E_PYTHON")); configured != "" {
		return configured, args
	}
	venvPython := filepath.Join(repoRoot, "actions", ".venv", "bin", "python")
	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() {
		return venvPython, args
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Fatal("browser integration requires actions/.venv or uv on PATH")
	}
	return uv, append([]string{"run", "--project", filepath.Join(repoRoot, "actions"), "--locked", "python"}, args...)
}

func browserPath() string {
	if configured := strings.TrimSpace(os.Getenv("BUNTZEN_E2E_BROWSER_EXECUTABLE")); configured != "" {
		return configured
	}
	for _, candidate := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	// The pinned Playwright runtime resolves its own bundled Chromium in CI.
	return ""
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func assertTreeExcludesValues(t *testing.T, root string, forbidden ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
			archive, err := zip.OpenReader(path)
			if err != nil {
				return err
			}
			defer archive.Close()
			for _, item := range archive.File {
				reader, err := item.Open()
				if err != nil {
					return err
				}
				contents, err := io.ReadAll(io.LimitReader(reader, 16<<20))
				_ = reader.Close()
				if err != nil {
					return err
				}
				assertExcludesValues(t, contents, filepath.Base(path)+":"+item.Name, forbidden...)
			}
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertExcludesValues(t, contents, filepath.Base(path), forbidden...)
		return nil
	})
	if err != nil {
		t.Fatalf("inspect integration artifacts: %v", err)
	}
}

func assertExcludesValues(t *testing.T, contents []byte, label string, forbidden ...string) {
	t.Helper()
	for _, secret := range forbidden {
		if strings.Contains(string(contents), secret) {
			t.Errorf("%s retained synthetic secret %q", label, secret)
		}
	}
}
