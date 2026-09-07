package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/egress"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/scheduler"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func TestExecutionDeadlinePreservesAdmissionAndCheckoutWindows(t *testing.T) {
	fixture := newEngineTestFixture(t)
	now := time.Now().UTC()
	for _, command := range []model.JobCommand{model.CommandAuthCheck, model.CommandDryRun} {
		deadline, err := jobExecutionDeadline(model.Job{Command: command}, fixture.booking, now, interactiveExecutionBudget, checkoutExecutionGrace)
		if err != nil || !deadline.Equal(now.Add(15*time.Minute)) {
			t.Fatalf("interactive deadline=%s err=%v", deadline, err)
		}
	}
	expiry := now.Add(time.Minute)
	immediate := model.Job{Command: model.CommandBook, RunMode: model.RunModeManual, RunImmediately: true, DueAt: now.Add(-14 * time.Minute), ExpiresAt: &expiry}
	deadline, err := jobExecutionDeadline(immediate, fixture.booking, now, interactiveExecutionBudget, checkoutExecutionGrace)
	if err != nil || !deadline.Equal(expiry) {
		t.Fatalf("persisted immediate expiry reset: %s %v", deadline, err)
	}
	booking := fixture.booking
	booking.PrepMinutesBefore = 180
	booking.PollDeadlineSeconds = 900
	window, err := scheduler.WindowFor(booking)
	if err != nil {
		t.Fatal(err)
	}
	regular := model.Job{Command: model.CommandBook, DueAt: window.PrepAt, ExpiresAt: &window.PollEndsAt}
	deadline, err = jobExecutionDeadline(regular, booking, window.PrepAt, interactiveExecutionBudget, checkoutExecutionGrace)
	if err != nil || !deadline.Equal(window.PollEndsAt.Add(15*time.Minute)) {
		t.Fatalf("checkout deadline=%s err=%v", deadline, err)
	}
	if deadline.Sub(window.PrepAt) != 210*time.Minute || !regular.ExpiresAt.Equal(window.PollEndsAt) {
		t.Fatal("preparation or persisted queue expiry changed")
	}
}

func TestExecutionWatchdogCoversProviderPreparation(t *testing.T) {
	fixture := newEngineTestFixture(t)
	entered := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	defer provider.Close()
	job := configureBudgetFixture(t, &fixture, provider.URL, model.CommandAuthCheck, "waiting")
	_, err := fixture.engine.executeWithBudgets(context.Background(), job, 250*time.Millisecond, checkoutExecutionGrace)
	if !errors.Is(err, ErrExecutionBudget) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provider preparation escaped watchdog: %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("control never entered provider preparation")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fixture.engine.config.PythonExecutable), "started")); !os.IsNotExist(err) {
		t.Fatal("worker launched after failed provider preparation")
	}
}

func TestExecutionWatchdogClassifiesRealWorkerAndPreservesReservation(t *testing.T) {
	for _, test := range []struct {
		name    string
		command model.JobCommand
		mode    string
		want    model.JobStatus
	}{
		{"startup hang", model.CommandAuthCheck, "startup", model.JobFailed},
		{"auth hang", model.CommandAuthCheck, "waiting", model.JobFailed},
		{"dry run hang", model.CommandDryRun, "waiting", model.JobFailed},
		{"healthy completion", model.CommandAuthCheck, "success", model.JobSucceeded},
		{"unverified confirmation", model.CommandBook, "confirm", model.JobOutcomeUnknown},
		{"verified confirmation cleanup", model.CommandBook, "completed", model.JobSucceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineTestFixture(t)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":200,"data":"pong"}`)) }))
			defer provider.Close()
			job := configureBudgetFixture(t, &fixture, provider.URL, test.command, test.mode)
			// The same runtime path uses production durations through execute;
			// this private test entrypoint shortens only the waiting interval.
			if job.RunImmediately {
				expiry := time.Now().Add(750 * time.Millisecond)
				job.ExpiresAt = &expiry
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := fixture.engine.executeWithBudgets(ctx, job, 750*time.Millisecond, checkoutExecutionGrace)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.want {
				t.Fatalf("status=%s message=%q want=%s", result.Status, result.Message, test.want)
			}
			if test.want == model.JobFailed && result.Message != executionBudgetMessage {
				t.Fatalf("missing watchdog notice: %q", result.Message)
			}
			if ctx.Err() != nil {
				t.Fatal("watchdog relied on external test timeout")
			}
			fixture.engine.finish(job.ID, result.Status, result.Message, &result.ExitCode)
			finished, err := fixture.resources.GetJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if finished.Status != test.want {
				t.Fatalf("durable status=%s want=%s", finished.Status, test.want)
			}
			if test.command == model.CommandBook {
				if finished.ConfirmationStartedAt == nil {
					t.Fatal("missing durable confirmation barrier")
				}
				if _, err := fixture.engine.QueueBookingNow(context.Background(), fixture.user.ID, fixture.booking.ID); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("reservation released after confirmation: %v", err)
				}
			}
		})
	}
}

func configureBudgetFixture(t *testing.T, fixture *engineTestFixture, providerURL string, command model.JobCommand, mode string) model.Job {
	t.Helper()
	ctx := context.Background()
	profile, err := fixture.resources.GetProfile(ctx, fixture.booking.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.resources.UpdateOTPSource(ctx, profile.OTPSourceID, store.OTPSourceInput{Name: "Budget inbox", Provider: model.OTPProviderBlueBubbles, Identity: providerURL, ProviderConfig: map[string]string{"base_url": providerURL, "password": "synthetic-provider-password"}}); err != nil {
		t.Fatal(err)
	}
	fixture.engine.config.BlueBubblesPolicy, err = egress.NewPolicy([]egress.Rule{{Origin: providerURL, Networks: []string{"127.0.0.1/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python worker transport required")
	}
	directory := t.TempDir()
	worker := `import json, sys
from pathlib import Path
Path(__file__).with_name("started").touch()
mode = sys.argv[1]
def emit(kind, **fields):
    print(json.dumps(dict(v=2, type=kind, **fields)), flush=True)
if mode != "startup":
    emit("worker.ready", action="yodel", protocol=2)
    start = json.loads(sys.stdin.readline())
    assert start["type"] == "run.start"
    if mode in ("confirm", "completed", "growth-confirm", "growth-completed"):
        emit("confirmation.starting", confirmation_id="budget-confirmation")
        assert json.loads(sys.stdin.readline())["type"] == "confirmation.ready"
        if mode in ("completed", "growth-completed"):
            emit("confirmation.completed", confirmation_id="budget-confirmation")
    if mode.startswith("growth"):
        profile = Path(start["config"]["profile_dir"])
        (profile / "saved-session").write_text("synthetic saved login")
        with (profile / "oversized-cache").open("wb") as cache:
            cache.truncate((512 << 20) + 1)
    if mode == "success":
        emit("run.complete", status="succeeded", message="Authenticated")
        sys.exit(0)
for line in sys.stdin:
    if json.loads(line)["type"] == "control.cancel":
        break
`
	workerPath := filepath.Join(directory, "budget_worker.py")
	if err := os.WriteFile(workerPath, []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	launcher := filepath.Join(directory, "run-worker")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+quote(python)+" "+quote(workerPath)+" "+quote(mode)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.engine.config.PythonExecutable = launcher
	fixture.engine.config.PythonModule = "budget_worker"
	var job model.Job
	if command == model.CommandBook {
		booking := fixture.booking
		booking.TargetDate = time.Now().UTC().Format(time.DateOnly)
		fixture.booking, err = fixture.resources.UpdateBookingRequest(ctx, booking)
		if err != nil {
			t.Fatal(err)
		}
		job, err = fixture.engine.QueueBookingNow(ctx, fixture.user.ID, booking.ID)
	} else if command == model.CommandDryRun {
		bookingID := fixture.booking.ID
		job, err = fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{BookingRequestID: &bookingID, Command: command, RunMode: model.RunModeDryRun})
	} else {
		job, err = fixture.resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, Command: command, RunMode: model.RunModeManual})
	}
	if err != nil {
		t.Fatal(err)
	}
	job, err = fixture.store.SystemClaimNextDueJob(ctx, "budget-test")
	if err != nil {
		t.Fatal(err)
	}
	return job
}
