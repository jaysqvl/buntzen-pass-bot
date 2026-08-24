package control

import (
	"context"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
)

type fakeProcess struct {
	events chan actionproc.Frame
	done   chan actionproc.Result
	sent   chan actionproc.Frame
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{events: make(chan actionproc.Frame, 16), done: make(chan actionproc.Result, 1), sent: make(chan actionproc.Frame, 16)}
}
func (p *fakeProcess) Events() <-chan actionproc.Frame { return p.events }
func (p *fakeProcess) Done() <-chan actionproc.Result  { return p.done }
func (p *fakeProcess) Send(kind string, payload map[string]any) error {
	p.sent <- actionproc.Frame{Version: 1, Type: kind, Payload: payload}
	return nil
}
func (p *fakeProcess) Cancel(time.Duration) {}

type fakeProvider struct {
	armedAt time.Time
	code    string
}

func (p *fakeProvider) Health(context.Context) error { return nil }
func (p *fakeProvider) Arm(_ context.Context, filter otp.Filter) (otp.Armed, error) {
	p.armedAt = time.Now()
	return otp.Armed{Provider: "fake", ArmedAt: p.armedAt, Filter: filter}, nil
}
func (p *fakeProvider) WaitForCode(_ context.Context, armed otp.Armed) (otp.Message, error) {
	return otp.Message{ID: "new-message", Code: p.code, ReceivedAt: time.Now()}, nil
}

func frame(kind string, values map[string]any) actionproc.Frame {
	if values == nil {
		values = map[string]any{}
	}
	return actionproc.Frame{Version: 1, Type: kind, Payload: values}
}

func TestCoordinatorArmsBeforeProvidingOTPAndClearsIt(t *testing.T) {
	process := newFakeProcess()
	provider := &fakeProvider{code: "482913"}
	hub := NewHub()
	resultCh := make(chan RunResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := Run(context.Background(), RunInput{
			JobID: 4, Command: model.CommandAuthCheck, Mode: model.RunModeAuto,
			StartConfig: map[string]any{"profile_dir": "/tmp/profile"},
			Credentials: model.ProfileCredentials{Email: "person@example.test", Password: "secret"},
			Provider:    provider, OTPTimeout: time.Second, Hub: hub,
			NewProcess: func(context.Context) (ActionProcess, error) { return process, nil },
		})
		resultCh <- result
		errCh <- err
	}()

	process.events <- frame("worker.ready", map[string]any{"action": "yodel"})
	if sent := <-process.sent; sent.Type != "run.start" {
		t.Fatalf("sent = %q", sent.Type)
	}
	process.events <- frame("otp.prepare", map[string]any{"challenge_id": "challenge"})
	if sent := <-process.sent; sent.Type != "otp.ready" || provider.armedAt.IsZero() {
		t.Fatalf("ready before arm: %#v", sent)
	}
	process.events <- frame("otp.triggered", map[string]any{"challenge_id": "challenge"})
	provided := <-process.sent
	if provided.Type != "otp.provide" || provided.Payload["code"] != "482913" {
		t.Fatalf("provided = %#v", provided)
	}
	if state, ok := hub.OTP("4", time.Now()); !ok || state.Code != "482913" {
		t.Fatalf("live OTP = %#v, %v", state, ok)
	}
	process.events <- frame("otp.submitted", map[string]any{"challenge_id": "challenge"})
	process.events <- frame("run.complete", map[string]any{"status": "succeeded", "message": "Authenticated"})
	close(process.events)
	process.done <- actionproc.Result{ExitCode: 0}
	close(process.done)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if result := <-resultCh; result.Status != model.JobSucceeded {
		t.Fatalf("result = %#v", result)
	}
	if _, ok := hub.OTP("4", time.Now()); ok {
		t.Fatal("OTP remained live after submission")
	}
}

func TestCoordinatorCrashAfterConfirmationIsOutcomeUnknown(t *testing.T) {
	process := newFakeProcess()
	resultCh := make(chan RunResult, 1)
	marked := false
	go func() {
		result, _ := Run(context.Background(), RunInput{
			JobID: 5, Command: model.CommandBook, Mode: model.RunModeAuto,
			Provider: &fakeProvider{}, Hub: NewHub(),
			NewProcess: func(context.Context) (ActionProcess, error) { return process, nil },
			Hooks: RunHooks{ConfirmationStarting: func() error {
				marked = true
				return nil
			}},
		})
		resultCh <- result
	}()
	process.events <- frame("worker.ready", map[string]any{"action": "yodel"})
	<-process.sent
	process.events <- frame("confirmation.starting", map[string]any{"confirmation_id": "confirm-1"})
	ack := <-process.sent
	if !marked || ack.Type != "confirmation.ready" || ack.Payload["confirmation_id"] != "confirm-1" {
		t.Fatalf("durable confirmation ack = %#v, marked=%v", ack, marked)
	}
	close(process.events)
	process.done <- actionproc.Result{ExitCode: 1}
	close(process.done)
	if result := <-resultCh; result.Status != model.JobOutcomeUnknown {
		t.Fatalf("status = %q", result.Status)
	}
}

func TestSanitizeMessageRemovesSecretsAndCodes(t *testing.T) {
	got := sanitizeMessage("user@example.test password 729104", model.ProfileCredentials{Email: "user@example.test", Password: "password"})
	if got != "[redacted] [redacted] [redacted-code]" {
		t.Fatalf("sanitized = %q", got)
	}
}
