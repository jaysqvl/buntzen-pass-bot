package otp

import (
	"testing"
	"time"
)

func TestExtractCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		body string
		want string
		ok   bool
	}{
		{"Your code is 1234.", "1234", true},
		{"Use 123456 to verify.", "123456", true},
		{"Yodel passcode: 12345678", "12345678", true},
		{"On 2030-01-14 your Yodel verification code is 876543", "876543", true},
		{"reference 123456789", "", false},
		{"no digits", "", false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.body, func(t *testing.T) {
			t.Parallel()
			got, ok := ExtractCode(test.body)
			if got != test.want || ok != test.ok {
				t.Fatalf("ExtractCode() = %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestMatchEnforcesFingerprintAndDropsBody(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 18, 0, 0, 0, time.UTC)
	raw := RawMessage{
		ID:         "message-1",
		Body:       "Your Yodel code is 654321",
		Sender:     "+1 (555) 010-0100",
		Recipient:  "+15550100200",
		ChatGUID:   "SMS;-;+15550100100",
		Service:    "SMS",
		ReceivedAt: now,
		Cursor:     42,
		Inbound:    true,
	}
	filter := Filter{
		Sender:       "+15550100100",
		Recipient:    "+1 555 010 0200",
		ChatGUID:     "SMS;-;+15550100100",
		Service:      "sms",
		FreshAfter:   now.Add(-time.Second),
		RequireYodel: true,
	}
	message, ok := Match(raw, filter)
	if !ok {
		t.Fatal("expected message to match")
	}
	if message.Code != "654321" || message.ID != raw.ID || message.Cursor != 42 {
		t.Fatalf("unexpected normalized message: %#v", message)
	}

	bad := []RawMessage{
		func() RawMessage { value := raw; value.Inbound = false; return value }(),
		func() RawMessage { value := raw; value.Sender = "+15550109999"; return value }(),
		func() RawMessage { value := raw; value.ChatGUID = "other"; return value }(),
		func() RawMessage { value := raw; value.Service = "iMessage"; return value }(),
		func() RawMessage { value := raw; value.ReceivedAt = now.Add(-2 * time.Second); return value }(),
		func() RawMessage { value := raw; value.Body = "Your code is 654321"; return value }(),
	}
	for i, candidate := range bad {
		if _, ok := Match(candidate, filter); ok {
			t.Fatalf("bad candidate %d unexpectedly matched", i)
		}
	}
}

func TestPairingRequiresFingerprintMetadataAndYodel(t *testing.T) {
	t.Parallel()
	base := RawMessage{
		ID:         "m1",
		Body:       "Yodel verification code: 111111",
		Sender:     "55555",
		ChatGUID:   "SMS;-;55555",
		Service:    "SMS",
		ReceivedAt: time.Now(),
		Inbound:    true,
	}
	if _, ok := Match(base, Filter{Pairing: true}); !ok {
		t.Fatal("expected complete pairing candidate")
	}
	for _, mutate := range []func(*RawMessage){
		func(value *RawMessage) { value.Sender = "" },
		func(value *RawMessage) { value.ChatGUID = "" },
		func(value *RawMessage) { value.Service = "" },
		func(value *RawMessage) { value.Body = "Verification code: 111111" },
	} {
		candidate := base
		mutate(&candidate)
		if _, ok := Match(candidate, Filter{Pairing: true}); ok {
			t.Fatalf("incomplete pairing candidate unexpectedly matched: %#v", candidate)
		}
	}
}

func TestSelectSortsNewestAndDeduplicates(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	seen := NewDeduper(10, "old")
	messages := Select([]RawMessage{
		{ID: "old", Body: "Yodel 111111", Inbound: true, ReceivedAt: now},
		{ID: "newer", Body: "Yodel 222222", Inbound: true, ReceivedAt: now.Add(time.Second), Cursor: 2},
		{ID: "older", Body: "Yodel 333333", Inbound: true, ReceivedAt: now, Cursor: 1},
		{ID: "newer", Body: "Yodel 222222", Inbound: true, ReceivedAt: now.Add(time.Second), Cursor: 2},
	}, Filter{}, seen)
	if len(messages) != 2 || messages[0].ID != "newer" || messages[1].ID != "older" {
		t.Fatalf("unexpected selected messages: %#v", messages)
	}
}

func TestDeduperIsBounded(t *testing.T) {
	t.Parallel()
	d := NewDeduper(2)
	if !d.Add("a") || !d.Add("b") || d.Add("a") {
		t.Fatal("initial deduplication failed")
	}
	if !d.Add("c") {
		t.Fatal("expected new ID")
	}
	if !d.Add("a") {
		t.Fatal("oldest ID should have been evicted")
	}
	if d.Add("") {
		t.Fatal("empty ID must not be accepted")
	}
}

func TestMaskAddress(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"+1 (555) 010-0123":  "*******0123",
		"sender@example.com": "s***@example.com",
		"shortcode":          "s********",
		"":                   "unknown",
	}
	for input, want := range tests {
		if got := MaskAddress(input); got != want {
			t.Errorf("MaskAddress(%q) = %q; want %q", input, got, want)
		}
	}
	if got := (Message{Sender: "+15550100123"}).MaskedSender(); got != "*******0123" {
		t.Errorf("Message.MaskedSender() = %q", got)
	}
}
