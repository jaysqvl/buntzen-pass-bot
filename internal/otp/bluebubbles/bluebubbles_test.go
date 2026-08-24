package bluebubbles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
)

const testPassword = "bb-secret-password"

func TestHealthUsesOnlyAuthenticatedPing(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/ping" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		if got := request.URL.Query().Get("password"); got != testPassword || len(request.URL.Query()) != 1 {
			t.Errorf("unexpected authentication query: %#v", request.URL.Query())
		}
		if request.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected Accept header: %q", request.Header.Get("Accept"))
		}
		writeJSON(t, response, map[string]any{"status": 200, "message": "Ping received!", "data": "pong"})
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

func TestArmThenWaitUsesBoundedPairedQueries(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 20, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/message/query" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("password") != testPassword || len(request.URL.Query()) != 1 {
			t.Errorf("unexpected query parameters: %#v", request.URL.Query())
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content type: %q", request.Header.Get("Content-Type"))
		}
		var query queryRequest
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			t.Errorf("decode query: %v", err)
		}
		switch calls.Add(1) {
		case 1:
			if query.ChatGUID != "SMS;-;55555" || query.Limit != 1 || query.Offset != 0 || query.Sort != "DESC" || !reflect.DeepEqual(query.With, []string{"chat"}) || len(query.Where) != 0 {
				t.Errorf("unexpected arm query: %#v", query)
			}
			writeQuery(t, response, query, []wireMessage{wire("existing", 100, now.Add(-time.Minute), "old", "55555", "SMS", false)}, 1)
		case 2:
			if query.ChatGUID != "SMS;-;55555" || query.Limit != defaultPageLimit || query.Offset != 0 || query.Sort != "ASC" || !reflect.DeepEqual(query.With, []string{"chat"}) {
				t.Errorf("unexpected wait query: %#v", query)
			}
			if len(query.Where) != 1 || query.Where[0].Statement != "message.ROWID > :minRowID" || query.Where[0].Args["minRowID"] != 100 {
				t.Errorf("wait query did not use armed cursor: %#v", query.Where)
			}
			writeQuery(t, response, query, []wireMessage{
				wire("outbound", 101, now.Add(time.Second), "Yodel code 111111", "55555", "SMS", true),
				wire("wrong-sender", 102, now.Add(2*time.Second), "Yodel code 222222", "99999", "SMS", false),
				wire("wanted", 103, now.Add(3*time.Second), "Your Yodel code is 333333", "55555", "SMS", false),
			}, 3)
		default:
			t.Errorf("unexpected extra query")
			writeQuery(t, response, query, nil, 0)
		}
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, Config{ChatGUID: "SMS;-;55555", Sender: "55555", Service: "SMS"})
	provider.now = func() time.Time { return now }
	armed, err := provider.Arm(context.Background(), otp.Filter{})
	if err != nil {
		t.Fatalf("Arm() error: %v", err)
	}
	if armed.Cursor.Position != 100 || armed.Provider != Kind {
		t.Fatalf("unexpected arm: %#v", armed)
	}
	message, err := provider.WaitForCode(context.Background(), armed)
	if err != nil {
		t.Fatalf("WaitForCode() error: %v", err)
	}
	if message.ID != "wanted" || message.Code != "333333" || message.Cursor != 103 {
		t.Fatalf("unexpected OTP: %#v", message)
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d requests, want 2", calls.Load())
	}
}

func TestSupervisedPairingReturnsOnlyYodelCandidates(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 21, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var query queryRequest
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			t.Errorf("decode query: %v", err)
		}
		switch calls.Add(1) {
		case 1:
			if query.ChatGUID != "" || query.Limit != 1 {
				t.Errorf("pairing arm must be globally bounded: %#v", query)
			}
			writeQuery(t, response, query, []wireMessage{wire("existing", 50, now.Add(-time.Minute), "old", "11111", "SMS", false)}, 1)
		case 2:
			if query.ChatGUID != "" || query.Where[0].Args["minRowID"] != 50 {
				t.Errorf("unexpected pairing poll: %#v", query)
			}
			missingService := wire("missing-service", 51, now, "Yodel code 111111", "22222", "", false)
			writeQuery(t, response, query, []wireMessage{
				wire("unrelated", 52, now.Add(time.Second), "Bank code 222222", "33333", "SMS", false),
				missingService,
				wire("candidate", 53, now.Add(2*time.Second), "Yodel verification code 444444", "+15550100123", "SMS", false),
			}, 3)
		default:
			t.Errorf("unexpected extra query")
		}
	}))
	defer server.Close()

	provider := newTestProvider(t, server.URL, Config{})
	provider.now = func() time.Time { return now }
	armed, err := provider.Arm(context.Background(), otp.Filter{Pairing: true})
	if err != nil {
		t.Fatalf("Arm(pairing) error: %v", err)
	}
	candidates, err := provider.WaitForPairingCandidates(context.Background(), armed)
	if err != nil {
		t.Fatalf("WaitForPairingCandidates() error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != "candidate" || candidates[0].Code != "444444" {
		t.Fatalf("unexpected candidates: %#v", candidates)
	}
	if got := otp.MaskAddress(candidates[0].Sender); got != "*******0123" {
		t.Fatalf("masked sender = %q", got)
	}
}

func TestPollingFailsClosedWhenBoundedWindowOverflows(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var query queryRequest
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			t.Errorf("decode query: %v", err)
		}
		messages := make([]wireMessage, query.Limit)
		for i := range messages {
			messages[i] = wire(fmt.Sprintf("m-%d", i), int64(i+1), now, "not an otp", "55555", "SMS", false)
		}
		writeQuery(t, response, query, messages, defaultPageLimit*defaultMaxPages+1)
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{ChatGUID: "SMS;-;55555", Sender: "55555", Service: "SMS"})
	_, err := provider.WaitForCode(context.Background(), otp.Armed{
		Provider: Kind,
		Filter:   otp.Filter{ChatGUID: "SMS;-;55555", Sender: "55555", Service: "SMS"},
	})
	if !errors.Is(err, otp.ErrPollWindowExceeded) {
		t.Fatalf("WaitForCode() error = %v; want ErrPollWindowExceeded", err)
	}
}

func TestMalformedUnboundedResponseFailsClosed(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(t, response, map[string]any{"status": 200, "data": []any{}})
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{ChatGUID: "chat", Sender: "sender", Service: "SMS"})
	if _, err := provider.Arm(context.Background(), otp.Filter{}); err == nil || !strings.Contains(err.Error(), "malformed API response") {
		t.Fatalf("Arm() error = %v; want safe shape error", err)
	}
}

func TestRedirectsAndErrorsDoNotLeakPassword(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Store(true)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/leak", http.StatusFound)
	}))
	defer redirect.Close()
	provider := newTestProvider(t, redirect.URL, Config{})
	err := provider.Health(context.Background())
	if err == nil {
		t.Fatal("expected redirect error")
	}
	if followed.Load() {
		t.Fatal("redirect was followed")
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), redirect.URL) {
		t.Fatalf("error leaked authenticated URL: %q", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "password="+testPassword, http.StatusInternalServerError)
	}))
	defer failing.Close()
	provider = newTestProvider(t, failing.URL, Config{})
	err = provider.Health(context.Background())
	if err == nil || strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), failing.URL) {
		t.Fatalf("unsafe status error: %v", err)
	}
}

func TestHealthRejectsProviderErrorStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status := status
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(status)
				_, _ = response.Write([]byte(`{"error":"password=` + testPassword + `"}`))
			}))
			defer server.Close()
			provider := newTestProvider(t, server.URL, Config{})
			err := provider.Health(context.Background())
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("status %d", status)) {
				t.Fatalf("Health() error = %v", err)
			}
			if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error leaked provider details: %q", err)
			}
		})
	}
}

func TestResponseSizeCap(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(strings.Repeat("x", 65)))
	}))
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{MaxResponseBytes: 64})
	err := provider.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("Health() error = %v; want size-limit error", err)
	}
}

func TestConfigRequiresExactPairingForNormalArm(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	provider := newTestProvider(t, server.URL, Config{})
	if _, err := provider.Arm(context.Background(), otp.Filter{}); err == nil {
		t.Fatal("normal arm must reject an unpaired source before making a request")
	}
}

func newTestProvider(t *testing.T, baseURL string, overrides Config) *Provider {
	t.Helper()
	overrides.BaseURL = baseURL
	overrides.Password = testPassword
	overrides.PollInterval = time.Millisecond
	provider, err := New(overrides)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return provider
}

func wire(id string, rowID int64, receivedAt time.Time, body, sender, service string, fromMe bool) wireMessage {
	return wireMessage{
		OriginalROWID: rowID,
		GUID:          id,
		Text:          body,
		Handle:        &wireHandle{Address: sender, Service: service},
		Chats:         []wireChat{{GUID: "SMS;-;55555"}},
		DateCreated:   receivedAt.UnixMilli(),
		IsFromMe:      fromMe,
	}
}

func writeQuery(t *testing.T, response http.ResponseWriter, request queryRequest, messages []wireMessage, total int) {
	t.Helper()
	writeJSON(t, response, map[string]any{
		"status": 200,
		"data":   messages,
		"metadata": queryMetadata{
			Offset: request.Offset,
			Limit:  request.Limit,
			Total:  total,
			Count:  len(messages),
		},
	})
}

func writeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
