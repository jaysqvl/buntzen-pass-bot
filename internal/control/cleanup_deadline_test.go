package control

import (
	"context"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

func TestVerifiedConfirmationSurvivesCancellationDuringCleanup(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unverified remains unknown", true: "verified succeeds"}[completed], func(t *testing.T) {
			process := newFakeProcess()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			verified := make(chan struct{})
			finished := make(chan RunResult, 1)
			go func() {
				result, err := Run(ctx, RunInput{JobID: 29, Command: model.CommandBook, Mode: model.RunModeAuto, Provider: &fakeProvider{}, Hub: NewHub(), NewProcess: func(context.Context) (ActionProcess, error) { return process, nil }, Hooks: RunHooks{ConfirmationStarting: func() error { return nil }, Event: func(kind, message string) {
					if kind == "confirmation.completed" {
						close(verified)
					}
				}}})
				if err != nil {
					t.Error(err)
				}
				finished <- result
			}()
			process.events <- frame("worker.ready", map[string]any{"action": "yodel", "protocol": float64(actionproc.ProtocolVersion)})
			<-process.sent
			process.events <- frame("confirmation.starting", map[string]any{"confirmation_id": "bounded-cleanup"})
			<-process.sent
			if completed {
				process.events <- frame("confirmation.completed", map[string]any{"confirmation_id": "bounded-cleanup"})
				select {
				case <-verified:
				case <-time.After(time.Second):
					t.Fatal("verified completion not consumed")
				}
			}
			cancel()
			close(process.events)
			process.done <- actionproc.Result{ExitCode: 1}
			close(process.done)
			select {
			case result := <-finished:
				want := model.JobOutcomeUnknown
				if completed {
					want = model.JobSucceeded
				}
				if result.Status != want {
					t.Fatalf("cleanup cancellation status=%s want=%s", result.Status, want)
				}
			case <-time.After(time.Second):
				t.Fatal("coordinator did not finish")
			}
		})
	}
}

func TestCompletionCannotSkipOrMismatchConfirmationBarrier(t *testing.T) {
	for _, start := range []bool{false, true} {
		process := newFakeProcess()
		finished := make(chan error, 1)
		go func() {
			result, err := Run(context.Background(), RunInput{
				JobID: 30, Command: model.CommandBook, Mode: model.RunModeAuto,
				Provider: &fakeProvider{}, Hub: NewHub(),
				NewProcess: func(context.Context) (ActionProcess, error) { return process, nil },
				Hooks:      RunHooks{ConfirmationStarting: func() error { return nil }},
			})
			if result.Status == model.JobSucceeded {
				t.Error("unmatched confirmation became success")
			}
			finished <- err
		}()
		process.events <- frame("worker.ready", map[string]any{"action": "yodel", "protocol": float64(actionproc.ProtocolVersion)})
		<-process.sent
		if start {
			process.events <- frame("confirmation.starting", map[string]any{"confirmation_id": "expected"})
			<-process.sent
		}
		process.events <- frame("confirmation.completed", map[string]any{"confirmation_id": "wrong"})
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("invalid completion accepted")
			}
		case <-time.After(time.Second):
			t.Fatal("completion validation hung")
		}
	}
}
