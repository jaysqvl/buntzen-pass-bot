package actionproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProcessRoundTripAndCancellation(t *testing.T) {
	if os.Getenv("BUNTZEN_ACTIONPROC_HELPER") == "1" {
		helperProcess()
		os.Exit(0)
	}
	session, err := Start(context.Background(), Config{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestProcessRoundTripAndCancellation"},
		Environment: []string{
			"BUNTZEN_ACTIONPROC_HELPER=1",
		},
		CancelGrace: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := <-session.Events()
	if ready.Type != "worker.ready" {
		t.Fatalf("first event = %q", ready.Type)
	}
	if err := session.Send("run.start", map[string]any{"run_id": "test"}); err != nil {
		t.Fatal(err)
	}
	started := <-session.Events()
	if started.Type != "run.status" {
		t.Fatalf("second event = %q", started.Type)
	}
	session.Cancel(5 * time.Second)
	completed := <-session.Events()
	if completed.Type != "run.complete" || completed.Payload["status"] != "cancelled" {
		t.Fatalf("terminal event = %#v", completed)
	}
	result := <-session.Done()
	if result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestDecodeRejectsMalformedAndOversizedFrames(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"v":2,"type":"worker.ready"}`),
		[]byte(`{"v":1}`),
	} {
		if _, err := decodeFrame(raw); err == nil {
			t.Fatalf("expected %q to fail", raw)
		}
	}
	_, err := decodeFrame([]byte(strings.Repeat("x", MaxFrameBytes+1)))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestChildEnvironmentDoesNotInheritControlPlaneSecrets(t *testing.T) {
	t.Setenv("BUNTZEN_ADMIN_PASSWORD", "admin-secret")
	t.Setenv("BUNTZEN_MASTER_KEY", "master-secret")
	t.Setenv("TWILIO_AUTH_TOKEN", "provider-secret")
	t.Setenv("HTTP_PROXY", "http://proxy-with-credentials.example")
	t.Setenv("PLAYWRIGHT_BROWSERS_PATH", "/safe/playwright")

	environment, err := childEnvironment([]string{"PYTHONUNBUFFERED=1"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{
		"BUNTZEN_ADMIN_PASSWORD=",
		"BUNTZEN_MASTER_KEY=",
		"TWILIO_AUTH_TOKEN=",
		"HTTP_PROXY=",
		"admin-secret",
		"master-secret",
		"provider-secret",
		"proxy-with-credentials",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("child environment contains %q", forbidden)
		}
	}
	for _, expected := range []string{"PLAYWRIGHT_BROWSERS_PATH=/safe/playwright", "PYTHONUNBUFFERED=1"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("child environment missing %q: %q", expected, joined)
		}
	}
}

func TestChildEnvironmentRejectsUnapprovedOverrides(t *testing.T) {
	for _, override := range []string{"BUNTZEN_ADMIN_PASSWORD=secret", "HTTP_PROXY=http://proxy", "MALFORMED"} {
		if _, err := childEnvironment([]string{override}); err == nil {
			t.Fatalf("expected override %q to be rejected", override)
		}
	}
}

func TestSendRejectsOversizedFrame(t *testing.T) {
	if os.Getenv("BUNTZEN_ACTIONPROC_HELPER") == "1" {
		helperProcess()
		os.Exit(0)
	}
	session, err := Start(context.Background(), Config{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestSendRejectsOversizedFrame"},
		Environment: []string{
			"BUNTZEN_ACTIONPROC_HELPER=1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Cancel(100 * time.Millisecond)
	<-session.Events()
	err = session.Send("run.start", map[string]any{"padding": strings.Repeat("x", MaxFrameBytes)})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("send error = %v", err)
	}
}

func helperProcess() {
	writer := bufio.NewWriter(os.Stdout)
	emit := func(kind string, values map[string]any) {
		frame := map[string]any{"v": ProtocolVersion, "type": kind}
		for key, value := range values {
			frame[key] = value
		}
		raw, _ := json.Marshal(frame)
		fmt.Fprintln(writer, string(raw))
		writer.Flush()
	}
	emit("worker.ready", nil)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var frame map[string]any
		_ = json.Unmarshal(scanner.Bytes(), &frame)
		switch frame["type"] {
		case "run.start":
			emit("run.status", map[string]any{"phase": "starting"})
		case "control.cancel":
			emit("run.complete", map[string]any{"status": "cancelled"})
			return
		}
	}
}
