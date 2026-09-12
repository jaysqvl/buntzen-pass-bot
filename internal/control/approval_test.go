package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/actionproc"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestApprovalCarriesOnlyValidatedPassIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		passKey any
		mode    model.RunMode
		want    model.PassType
	}{
		{"all day", "all_day", model.RunModeManual, model.PassAllDay},
		{"afternoon", "afternoon", model.RunModeManual, model.PassAfternoon},
		{"morning", "morning", model.RunModeManual, model.PassMorning},
		{"missing pass", nil, model.RunModeManual, ""},
		{"unknown pass", "private arbitrary label", model.RunModeManual, ""},
		{"automatic", "all_day", model.RunModeAuto, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			process := newFakeProcess()
			hub := NewHub()
			review := make(chan model.PassType, 1)
			finished := make(chan error, 1)
			go func() {
				_, err := Run(ctx, RunInput{
					JobID: 41, Command: model.CommandBook, Mode: test.mode, Provider: &fakeProvider{}, Hub: hub,
					NewProcess: func(context.Context) (ActionProcess, error) { return process, nil },
					Hooks:      RunHooks{AwaitingApproval: func(_ string, pass model.PassType) error { review <- pass; return nil }},
				})
				finished <- err
			}()
			process.events <- frame("worker.ready", map[string]any{"action": "yodel", "protocol": float64(actionproc.ProtocolVersion)})
			select {
			case <-process.sent:
			case <-ctx.Done():
				t.Fatal("worker handshake timed out")
			}
			process.events <- frame("approval.request", map[string]any{"approval_id": "review-1", "pass_key": test.passKey, "label": "untrusted provider label"})
			if test.want == "" {
				select {
				case err := <-finished:
					if err == nil {
						t.Fatal("invalid approval accepted")
					}
				case <-ctx.Done():
					t.Fatal("invalid approval not rejected")
				}
				select {
				case <-review:
					t.Fatal("invalid pass reached approval hook")
				default:
				}
				if err := hub.Decide("41", "approve"); !errors.Is(err, ErrDecisionNotPending) {
					t.Fatalf("invalid approval opened decision gate: %v", err)
				}
				return
			}
			select {
			case pass := <-review:
				if pass != test.want {
					t.Fatalf("review pass=%s want=%s", pass, test.want)
				}
			case <-ctx.Done():
				t.Fatal("review context never arrived")
			}
			if err := hub.Decide("41", "cancel"); err != nil {
				t.Fatal(err)
			}
			select {
			case sent := <-process.sent:
				if sent.Type != "approval.cancel" || sent.Payload["approval_id"] != "review-1" {
					t.Fatalf("decision=%+v", sent)
				}
			case <-ctx.Done():
				t.Fatal("manual decision timed out")
			}
			process.events <- frame("run.complete", map[string]any{"status": "cancelled", "message": "Cancelled without confirmation"})
			close(process.events)
			process.done <- actionproc.Result{}
			close(process.done)
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("worker did not finish")
			}
		})
	}
}
