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
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	session, err := Start(ctx, Config{
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
	t.Cleanup(func() { session.Cancel(100 * time.Millisecond) })
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
		t.Fatalf("result: exit=%d error=%v", result.ExitCode, result.Err)
	}
}

func TestCancellationKillsWorkerWithBlockedInput(t *testing.T) {
	const helperMode = "blocked-stdin"
	if os.Getenv("BUNTZEN_ACTIONPROC_HELPER") == helperMode {
		fmt.Fprintln(os.Stdout, `{"v":2,"type":"worker.ready"}`)
		// Keep stdin open without consuming it, like a hung browser worker.
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	session, err := Start(t.Context(), Config{
		Executable:  os.Args[0],
		Args:        []string{"-test.run=^TestCancellationKillsWorkerWithBlockedInput$"},
		Environment: []string{"BUNTZEN_ACTIONPROC_HELPER=" + helperMode},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.forceKill)
	select {
	case <-session.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not start")
	}
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for range 128 {
			if err := session.Send("run.start", map[string]any{"padding": strings.Repeat("x", 60*1024)}); err != nil {
				return
			}
		}
	}()
	select {
	case <-writeDone:
		t.Fatal("expected the worker's unread pipe to block the writer")
	case <-time.After(100 * time.Millisecond):
	}
	cancelReturned := make(chan struct{})
	go func() {
		session.Cancel(100 * time.Millisecond)
		close(cancelReturned)
	}()
	select {
	case <-cancelReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation blocked on the worker's stdin")
	}
	select {
	case result := <-session.Done():
		if result.ExitCode == 0 {
			t.Fatal("hung worker exited successfully instead of being killed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hung worker was not reaped after the cancellation deadline")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not stop after the worker was killed")
	}
}

func TestCancellationReapsWorkerWhenCallerStopsReadingEvents(t *testing.T) {
	const helperMode = "unread-events"
	if os.Getenv("BUNTZEN_ACTIONPROC_HELPER") == helperMode {
		for range 256 {
			fmt.Fprintln(os.Stdout, `{"v":2,"type":"heartbeat"}`)
		}
		fmt.Fprintln(os.Stderr, "events-written")
		os.Exit(0)
	}
	written := make(chan struct{})
	session, err := Start(t.Context(), Config{
		Executable:  os.Args[0],
		Args:        []string{"-test.run=^TestCancellationReapsWorkerWhenCallerStopsReadingEvents$"},
		Environment: []string{"BUNTZEN_ACTIONPROC_HELPER=" + helperMode},
		OnStderr: func(line string) {
			if line == "events-written" {
				close(written)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.forceKill)
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not write its events")
	}
	// A caller may return on a protocol error without reading more events.
	// Cleanup must not depend on making space in the abandoned event channel.
	session.Cancel(100 * time.Millisecond)
	select {
	case <-session.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("unread events prevented process cleanup")
	}
}

func TestDecodeRejectsMalformedAndOversizedFrames(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"v":1,"type":"worker.ready"}`),
		[]byte(`{"v":2}`),
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

func TestChildEnvironmentAllowsActionLogLevel(t *testing.T) {
	environment, err := childEnvironment([]string{"BUNTZEN_ACTION_LOG_LEVEL=debug"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(environment, "\n"), "BUNTZEN_ACTION_LOG_LEVEL=debug") {
		t.Fatalf("child environment omitted action log level: %q", environment)
	}
}

func TestStderrDrainDiscardsOversizedLinesAndBoundsTotalCallbacks(t *testing.T) {
	phoneAtBoundary := "5559876543"
	longLine := strings.Repeat("x", maxStderrLineBytes-len(phoneAtBoundary)+2) + phoneAtBoundary
	var input strings.Builder
	input.WriteString(longLine)
	input.WriteByte('\n')
	for input.Len() <= maxStderrTotalBytes+maxStderrLineBytes {
		input.WriteString(strings.Repeat("y", 1024))
		input.WriteByte('\n')
	}

	finished := make(chan struct{})
	var lines []string
	drainStderr(strings.NewReader(input.String()), func(line string) {
		lines = append(lines, line)
	}, finished)
	if len(lines) == 0 || lines[0] != stderrOversizedLine {
		t.Fatalf("oversized stderr line was not discarded: %#v", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, phoneAtBoundary) || strings.Contains(line, "555987") {
			t.Fatalf("oversized stderr exposed a credential fragment: %#v", lines)
		}
	}
	if lines[len(lines)-1] != stderrSuppressedLine {
		t.Fatalf("missing stderr suppression marker: %#v", lines[len(lines)-3:])
	}
	if len(lines) >= maxStderrLines {
		t.Fatalf("stderr callback count was not bounded: %d", len(lines))
	}
}

func TestStderrCallbackPanicCannotCrashDrain(t *testing.T) {
	finished := make(chan struct{})
	go drainStderr(strings.NewReader("first\nsecond\n"), func(string) {
		panic("synthetic callback panic")
	}, finished)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stderr drain did not recover from callback panic")
	}
}

func TestProcessDrainsMultiMegabyteUnterminatedStderr(t *testing.T) {
	const helperMode = "oversized-stderr"
	const sentinel = "SENSITIVE_STDERR_SENTINEL_4f17"
	if os.Getenv("BUNTZEN_ACTIONPROC_HELPER") == helperMode {
		payload := strings.Repeat(sentinel, (2<<20)/len(sentinel)+1)
		if _, err := fmt.Fprint(os.Stderr, payload); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var stderrLines []string
	session, err := Start(ctx, Config{
		Executable: os.Args[0],
		Args:       []string{"-test.run=^TestProcessDrainsMultiMegabyteUnterminatedStderr$"},
		Environment: []string{
			"BUNTZEN_ACTIONPROC_HELPER=" + helperMode,
		},
		CancelGrace: 100 * time.Millisecond,
		OnStderr: func(line string) {
			stderrLines = append(stderrLines, line)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-session.Done():
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("oversized-stderr helper result = %#v", result)
		}
	case <-ctx.Done():
		select {
		case result := <-session.Done():
			t.Fatalf("oversized child stderr pinned process completion until cancellation: %#v", result)
		case <-time.After(time.Second):
			t.Fatal("oversized child stderr prevented process cleanup after cancellation")
		}
	}

	if len(stderrLines) != 1 || stderrLines[0] != "action stderr line exceeded the safety limit; remaining diagnostics suppressed" {
		t.Fatalf("unexpected oversized-stderr diagnostics: %#v", stderrLines)
	}
	for _, line := range stderrLines {
		if strings.Contains(line, sentinel) || strings.Contains(line, "SENSITIVE_STDERR") {
			t.Fatalf("oversized stderr exposed sentinel content: %#v", stderrLines)
		}
	}
}

func TestSendRejectsOversizedFrame(t *testing.T) {
	if os.Getenv("BUNTZEN_ACTIONPROC_HELPER") == "1" {
		helperProcess()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := Start(ctx, Config{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestSendRejectsOversizedFrame"},
		Environment: []string{
			"BUNTZEN_ACTIONPROC_HELPER=1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Cancel(100 * time.Millisecond)
		select {
		case <-session.Done():
		case <-time.After(2 * time.Second):
			t.Error("helper did not exit during cleanup")
		}
	})
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
