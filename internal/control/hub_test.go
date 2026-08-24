package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
)

func TestOTPLifecycleIsEphemeral(t *testing.T) {
	hub := NewHub()
	now := time.Now()
	hub.SetOTP("job", "123456", now.Add(time.Minute))
	otp, ok := hub.OTP("job", now)
	if !ok || otp.Code != "123456" {
		t.Fatalf("unexpected OTP state: %#v %v", otp, ok)
	}
	hub.ClearOTP("job")
	if _, ok := hub.OTP("job", now); ok {
		t.Fatal("OTP remained after clear")
	}
}

func TestPairingCandidatesAreEphemeralAndSelectable(t *testing.T) {
	hub := NewHub()
	candidate := otp.Message{ID: "message-1", Code: "654321", Sender: "+15550100123", ChatGUID: "iMessage;-;sender", Service: "iMessage"}
	if err := hub.SetPairingCandidates("job", []otp.Message{candidate}); err != nil {
		t.Fatal(err)
	}
	stream, unsubscribe := hub.Subscribe("job")
	defer unsubscribe()
	event := <-stream
	if event.Kind != "pairing" {
		t.Fatalf("event kind = %q", event.Kind)
	}
	if err := hub.ChoosePairing("job", "message-1"); err != nil {
		t.Fatal(err)
	}
	selected, err := hub.WaitPairing(context.Background(), "job")
	if err != nil || selected.Code != "654321" || selected.Sender != candidate.Sender {
		t.Fatalf("selected = %#v, %v", selected, err)
	}
	if err := hub.ChoosePairing("job", "message-1"); !errors.Is(err, ErrPairingNotPending) {
		t.Fatalf("choice after clear = %v", err)
	}
}

func TestOTPExpires(t *testing.T) {
	hub := NewHub()
	now := time.Now()
	hub.SetOTP("job", "123456", now.Add(-time.Second))
	if _, ok := hub.OTP("job", now); ok {
		t.Fatal("expired OTP was returned")
	}
}

func TestOTPExpiryActivelyClearsConnectedClients(t *testing.T) {
	hub := NewHub()
	hub.SetOTP("job", "123456", time.Now().Add(20*time.Millisecond))
	stream, unsubscribe := hub.Subscribe("job")
	defer unsubscribe()
	if event := <-stream; event.Kind != "otp" {
		t.Fatalf("initial event = %q", event.Kind)
	}
	select {
	case event := <-stream:
		data, _ := event.Data.(map[string]any)
		if event.Kind != "otp" || data["active"] != false {
			t.Fatalf("expiry event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("OTP expiry was not broadcast")
	}
}

func TestDecisionFirstWriterWins(t *testing.T) {
	hub := NewHub()
	if err := hub.BeginDecision("job"); err != nil {
		t.Fatal(err)
	}
	if err := hub.Decide("job", "approve"); err != nil {
		t.Fatal(err)
	}
	if err := hub.Decide("job", "cancel"); !errors.Is(err, ErrDecisionAlreadySet) {
		t.Fatalf("second decision error = %v", err)
	}
	decision, err := hub.WaitDecision(context.Background(), "job")
	if err != nil || decision != "approve" {
		t.Fatalf("decision = %q, err = %v", decision, err)
	}
}
