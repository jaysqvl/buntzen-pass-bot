package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
)

const (
	testAccountSID = "synthetic-account-id"
	testAuthToken  = "twilio-secret-token"
	testToNumber   = "+15550100100"
)

func TestHealthUsesReadOnlyAccountRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		wantPath := "/2010-04-01/Accounts/" + testAccountSID + ".json"
		if request.Method != http.MethodGet || request.URL.Path != wantPath || request.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.String())
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != testAccountSID || password != testAuthToken {
			t.Error("missing or incorrect Basic authentication")
		}
		writeJSON(t, response, map[string]any{"sid": testAccountSID, "status": "active"})
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	if err := provider.Health(context.Background()); err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	transport, ok := provider.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("provider transport must disable environment proxies")
	}
}

func TestArmThenWaitReadsOnlyFreshInboundMessage(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 20, 0, 30, 0, time.UTC)
	oldAt := now.Add(-30 * time.Second).Truncate(time.Second)
	newAt := now.Add(30 * time.Second).Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("Twilio adapter attempted non-GET method %s", request.Method)
		}
		wantPath := "/2010-04-01/Accounts/" + testAccountSID + "/Messages.json"
		if request.URL.Path != wantPath {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		if request.URL.Query().Get("To") != testToNumber || request.URL.Query().Get("PageSize") != "20" || len(request.URL.Query()) != 2 {
			t.Errorf("unexpected query: %#v", request.URL.Query())
		}
		username, password, _ := request.BasicAuth()
		if username != testAccountSID || password != testAuthToken {
			t.Error("incorrect Basic authentication")
		}
		old := wireMessage{SID: "SM-old", Body: "Your Yodel code is 111111", From: "55555", To: testToNumber, Direction: "inbound", DateSent: oldAt.Format(time.RFC1123Z)}
		if calls.Add(1) == 1 {
			writeList(t, response, []wireMessage{old}, "")
			return
		}
		writeList(t, response, []wireMessage{
			old,
			{SID: "SM-outbound", Body: "Yodel 222222", From: "55555", To: testToNumber, Direction: "outbound-api", DateSent: newAt.Add(time.Second).Format(time.RFC1123Z)},
			{SID: "SM-wrong-to", Body: "Yodel 333333", From: "55555", To: "+15550109999", Direction: "inbound", DateSent: newAt.Format(time.RFC1123Z)},
			{SID: "SM-new", Body: "Your Yodel verification code is 444444", From: "55555", To: testToNumber, Direction: "inbound", DateSent: newAt.Format(time.RFC1123Z)},
		}, "")
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, Config{Sender: "55555"})
	provider.now = func() time.Time { return now }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatalf("Arm() error: %v", err)
	}
	if len(armed.Cursor.IDs) != 1 || armed.Cursor.IDs[0] != "SM-old" || !armed.Cursor.Timestamp.Equal(oldAt) {
		t.Fatalf("unexpected cursor: %#v", armed.Cursor)
	}
	message, err := provider.WaitForCode(context.Background(), armed)
	if err != nil {
		t.Fatalf("WaitForCode() error: %v", err)
	}
	if message.ID != "SM-new" || message.Code != "444444" || message.Sender != "55555" {
		t.Fatalf("unexpected OTP: %#v", message)
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d requests; want 2", calls.Load())
	}
}

func TestSameSecondSnapshotUsesSIDToDeduplicate(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2030, 1, 14, 20, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		old := wireMessage{SID: "SM-old", Body: "Yodel 111111", From: "55555", To: testToNumber, Direction: "inbound", DateSent: stamp.Format(time.RFC1123Z)}
		if calls.Add(1) == 1 {
			writeList(t, response, []wireMessage{old}, "")
			return
		}
		fresh := wireMessage{SID: "SM-new", Body: "Yodel 222222", From: "55555", To: testToNumber, Direction: "inbound", DateSent: stamp.Format(time.RFC1123Z)}
		writeList(t, response, []wireMessage{old, fresh}, "")
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	provider.now = func() time.Time { return stamp }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatalf("Arm() error: %v", err)
	}
	message, err := provider.WaitForCode(context.Background(), armed)
	if err != nil {
		t.Fatalf("WaitForCode() error: %v", err)
	}
	if message.ID != "SM-new" || message.Code != "222222" {
		t.Fatalf("unexpected OTP: %#v", message)
	}
}

func TestPaginationIsBoundedAndRestrictedToMessagesPath(t *testing.T) {
	t.Parallel()
	stamp := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			writeList(t, response, []wireMessage{{SID: "old", DateSent: stamp.Add(-time.Minute).Format(time.RFC1123Z)}}, "")
		case 2:
			next := "/2010-04-01/Accounts/" + testAccountSID + "/Messages.json?Page=1&PageSize=20&To=" + url.QueryEscape(testToNumber)
			writeList(t, response, []wireMessage{{SID: "skip", DateSent: stamp.Format(time.RFC1123Z)}}, next)
		case 3:
			if request.URL.Query().Get("Page") != "1" || request.URL.Query().Get("To") != testToNumber {
				t.Errorf("unexpected next page query: %#v", request.URL.Query())
			}
			writeList(t, response, []wireMessage{{SID: "wanted", Body: "Yodel code 777777", From: "55555", To: testToNumber, Direction: "inbound", DateSent: stamp.Add(time.Second).Format(time.RFC1123Z)}}, "")
		default:
			t.Errorf("unexpected request %d", call)
		}
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	provider.now = func() time.Time { return stamp }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatalf("Arm() error: %v", err)
	}
	message, err := provider.WaitForCode(context.Background(), armed)
	if err != nil {
		t.Fatalf("WaitForCode() error: %v", err)
	}
	if message.ID != "wanted" {
		t.Fatalf("unexpected message: %#v", message)
	}
}

func TestRejectsUntrustedNextPageURI(t *testing.T) {
	t.Parallel()
	stamp := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeList(t, response, []wireMessage{{SID: "old", DateSent: stamp.Format(time.RFC1123Z)}}, "")
			return
		}
		writeList(t, response, []wireMessage{{SID: "m", DateSent: stamp.Format(time.RFC1123Z)}}, "https://evil.example/messages")
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	provider.now = func() time.Time { return stamp }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatalf("Arm() error: %v", err)
	}
	_, err = provider.WaitForCode(context.Background(), armed)
	if err == nil || !strings.Contains(err.Error(), "malformed API response") {
		t.Fatalf("WaitForCode() error = %v; want safe next-page error", err)
	}
}

func TestPaginationOverflowFailsClosed(t *testing.T) {
	t.Parallel()
	stamp := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			writeList(t, response, []wireMessage{{SID: "old", DateSent: stamp.Format(time.RFC1123Z)}}, "")
			return
		}
		nextPage := call - 1
		next := fmt.Sprintf("/2010-04-01/Accounts/%s/Messages.json?Page=%d", testAccountSID, nextPage)
		writeList(t, response, []wireMessage{}, next)
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{MaxPages: 2})
	provider.now = func() time.Time { return stamp }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatalf("Arm() error: %v", err)
	}
	_, err = provider.WaitForCode(context.Background(), armed)
	if !errors.Is(err, otp.ErrPollWindowExceeded) {
		t.Fatalf("WaitForCode() error = %v; want ErrPollWindowExceeded", err)
	}
}

func TestPollingStopsAfterCrossingArmCursorInsteadOfReadingLifetimeHistory(t *testing.T) {
	t.Parallel()
	stamp := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeList(t, response, []wireMessage{{SID: "armed", DateSent: stamp.Format(time.RFC1123Z)}}, "")
		case 2:
			next := fmt.Sprintf("/2010-04-01/Accounts/%s/Messages.json?Page=1", testAccountSID)
			writeList(t, response, []wireMessage{
				{SID: "fresh", Body: "Yodel code 919191", From: "55555", To: testToNumber, Direction: "inbound", DateSent: stamp.Add(time.Second).Format(time.RFC1123Z)},
				{SID: "older", Body: "old", From: "55555", To: testToNumber, Direction: "inbound", DateSent: stamp.Add(-time.Second).Format(time.RFC1123Z)},
			}, next)
		default:
			t.Error("polling followed lifetime history after crossing the arm cursor")
			http.Error(response, "unexpected page", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	provider.now = func() time.Time { return stamp }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	message, err := provider.WaitForCode(context.Background(), armed)
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != "fresh" || calls.Load() != 2 {
		t.Fatalf("message=%#v calls=%d", message, calls.Load())
	}
}

func TestRedirectAndStatusErrorsDoNotLeakSecrets(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Store(true)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL, http.StatusFound)
	}))
	defer redirect.Close()
	provider := newTestProvider(t, redirect.URL, Config{})
	err := provider.Health(context.Background())
	if err == nil || followed.Load() {
		t.Fatalf("redirect handling failed: followed=%v error=%v", followed.Load(), err)
	}
	if strings.Contains(err.Error(), testAuthToken) || strings.Contains(err.Error(), redirect.URL) {
		t.Fatalf("error leaked credential details: %q", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, testAuthToken, http.StatusUnauthorized)
	}))
	defer failing.Close()
	provider = newTestProvider(t, failing.URL, Config{})
	err = provider.Health(context.Background())
	if err == nil || strings.Contains(err.Error(), testAuthToken) || strings.Contains(err.Error(), failing.URL) {
		t.Fatalf("unsafe status error: %v", err)
	}
}

func TestMalformedAndOversizedResponsesFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("malformed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			writeJSON(t, response, map[string]any{"messages": nil})
		}))
		defer server.Close()
		provider := newTestProvider(t, server.URL, Config{})
		if _, err := provider.Arm(context.Background(), otp.Filter{}); err == nil {
			t.Fatal("expected malformed response error")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte(strings.Repeat("x", 65)))
		}))
		defer server.Close()
		provider := newTestProvider(t, server.URL, Config{MaxResponseBytes: 64})
		if err := provider.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("Health() error = %v; want size-limit error", err)
		}
	})
}

func TestPairingIsBlueBubblesOnly(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	if _, err := provider.Arm(context.Background(), otp.Filter{Pairing: true}); err == nil {
		t.Fatal("Twilio pairing arm must be rejected without a request")
	}
}

func newTestProvider(t *testing.T, baseURL string, overrides Config) *Provider {
	t.Helper()
	overrides.AccountSID = testAccountSID
	overrides.AuthToken = testAuthToken
	overrides.ToNumber = testToNumber
	overrides.BaseURL = baseURL
	overrides.PollInterval = time.Millisecond
	provider, err := New(overrides)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return provider
}

func writeList(t *testing.T, response http.ResponseWriter, messages []wireMessage, next string) {
	t.Helper()
	writeJSON(t, response, listResponse{Messages: messages, NextPageURI: next})
}

func writeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
