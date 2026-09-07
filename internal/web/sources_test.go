package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func TestSourcesPairFromTheLinkedProfileWithoutABooking(t *testing.T) {
	for _, test := range []struct {
		name                            string
		enabled                         bool
		loginURL, wantLabel, wantDetail string
	}{
		{name: "ready without a booking", enabled: true, loginURL: "https://example.test/login", wantLabel: "Pair with Yodel"},
		{name: "disabled profile", loginURL: "https://example.test/login", wantLabel: "Enable profile", wantDetail: "enable the Yodel profile"},
		{name: "unapproved profile login URL", enabled: true, loginURL: "https://unapproved.example/login", wantLabel: "Review profile", wantDetail: "Yodel login URL must use an approved Yodel origin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWebFixture(t)
			ctx := context.Background()
			resources := fixture.store.ForUser(fixture.admin.ID)
			source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
				Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://127.0.0.1:2234",
				ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:2234", Password: "synthetic-password"},
			})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := resources.CreateProfile(ctx, store.ProfileInput{
				Name: "Linked Yodel profile", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID,
				LoginProbeURL: test.loginURL, Headless: true, DefaultTimeoutMS: 15_000, Enabled: test.enabled,
				Credentials: &model.ProfileCredentials{Phone: "5559876543"},
			})
			if err != nil {
				t.Fatal(err)
			}
			cookies := loginCookies(t, fixture)
			recorder := serveForm(fixture, http.MethodGet, "/", cookies, nil)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || !strings.Contains(body, test.wantLabel) || !strings.Contains(body, test.wantDetail) || !strings.Contains(body, profile.Name) {
				t.Fatalf("sources setup guidance = %d body=%s", recorder.Code, body)
			}
			pairURL := fmt.Sprintf("/sources/%d/pair", source.ID)
			if test.wantDetail == "" {
				if !strings.Contains(body, `action="`+pairURL+`"`) {
					t.Fatal("ready source does not offer pairing")
				}
			} else {
				if strings.Contains(body, `action="`+pairURL+`"`) {
					t.Fatal("incomplete source offers a pairing action that cannot succeed")
				}
				wantURL := fmt.Sprintf("/profiles/%d", profile.ID)
				if !strings.Contains(body, `href="`+wantURL+`"`) {
					t.Fatalf("sources lacks corrective profile link %q", wantURL)
				}
			}
			recorder = serveForm(fixture, http.MethodPost, pairURL, cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
			jobs, err := resources.ListJobs(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantDetail != "" {
				if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/?notice=pairing-unavailable#otp-sources" || len(jobs) != 0 {
					t.Fatalf("pair POST prerequisite = %d location=%q jobs=%+v", recorder.Code, recorder.Header().Get("Location"), jobs)
				}
				// Browsers keep the fragment locally; it is not part of the next HTTP request.
				page := serveForm(fixture, http.MethodGet, strings.SplitN(recorder.Header().Get("Location"), "#", 2)[0], cookies, nil)
				if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Pairing could not start") || !strings.Contains(page.Body.String(), test.wantLabel) {
					t.Fatalf("pairing prerequisite notice = %d body=%s", page.Code, page.Body.String())
				}
			} else {
				if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/jobs/") {
					t.Fatalf("ready pair POST = %d body=%s", recorder.Code, recorder.Body.String())
				}
				if len(jobs) != 1 || jobs[0].BookingRequestID != nil || jobs[0].ProfileID != profile.ID || jobs[0].Command != model.CommandAuthCheck {
					t.Fatalf("pairing did not enqueue profile-only auth: %+v", jobs)
				}
			}
		})
	}
}

func TestSourceConnectionActionsReturnToSetupWithoutProviderErrors(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprintf("healthy=%t", healthy), func(t *testing.T) {
			fixture := newWebFixture(t)
			const password = "write-only-provider-password"
			const privateError = "upstream-private-debug-detail"
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/ping" || r.URL.Query().Get("password") != password {
					t.Error("connection test did not use the authenticated read-only ping")
				}
				if !healthy {
					http.Error(w, privateError, http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":200,"message":"Ping received!","data":"pong"}`))
			}))
			defer provider.Close()
			source, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
				Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: provider.URL,
				ProviderConfig: bluebubbles.Config{BaseURL: provider.URL, Password: password},
			})
			if err != nil {
				t.Fatal(err)
			}
			cookies := loginCookies(t, fixture)
			response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/sources/%d/health", source.ID), cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
			wantLocation, wantMessage := "/?ok=healthy#otp-sources", "Provider authentication succeeded."
			if !healthy {
				wantLocation, wantMessage = "/?notice=provider-unavailable#otp-sources", "connection test failed"
			}
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != wantLocation || calls.Load() != 1 {
				t.Fatalf("connection action = %d location=%q calls=%d", response.Code, response.Header().Get("Location"), calls.Load())
			}
			page := serveForm(fixture, http.MethodGet, strings.SplitN(wantLocation, "#", 2)[0], cookies, nil)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), wantMessage) || !strings.Contains(page.Body.String(), `id="otp-sources"`) {
				t.Fatalf("connection result page = %d body=%s", page.Code, page.Body.String())
			}
			for _, secret := range []string{password, privateError} {
				if strings.Contains(response.Body.String()+response.Header().Get("Location")+page.Body.String(), secret) {
					t.Fatal("connection result exposed a provider secret or upstream error")
				}
			}
		})
	}
}

func TestPairingQueueFailuresReturnToSetupWithoutCreatingAnotherJob(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			fixture := newWebFixture(t)
			ctx := context.Background()
			resources := fixture.store.ForUser(fixture.admin.ID)
			source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
				Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://messages.example.test:1234",
				ProviderConfig: bluebubbles.Config{BaseURL: "http://messages.example.test:1234", Password: "synthetic-password"},
			})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := resources.CreateProfile(ctx, store.ProfileInput{
				Name: "Yodel", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID,
				LoginProbeURL: "https://example.test/login", Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
				Credentials: &model.ProfileCredentials{Phone: "5559876543"},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantJobs, notice := 1, "pairing-unavailable"
			if full {
				wantJobs, notice = store.MaxPendingJobsPerUser, "queue-full"
				for range store.MaxPendingJobsPerUser {
					if _, err := resources.EnqueueJob(ctx, store.EnqueueJobParams{ProfileID: profile.ID, Command: model.CommandAuthCheck}); err != nil {
						t.Fatal(err)
					}
				}
			} else if _, err := fixture.server.engine.QueuePairing(ctx, fixture.admin.ID, source.ID); err != nil {
				t.Fatal(err)
			}
			cookies := loginCookies(t, fixture)
			response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/sources/%d/pair", source.ID), cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?notice="+notice+"#otp-sources" {
				t.Fatalf("pairing queue refusal = %d location=%q", response.Code, response.Header().Get("Location"))
			}
			jobs, err := resources.ListJobs(ctx, 20)
			if err != nil || len(jobs) != wantJobs {
				t.Fatalf("rejected pairing changed the queue: jobs=%d err=%v", len(jobs), err)
			}
		})
	}
}

func TestSourceActionsKeepCrossOwnerRequestsNotFound(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	source, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Private Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://messages.example.test:1234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://messages.example.test:1234", Password: "synthetic-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{Username: "other", Password: "other-user-password"})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookiesAs(t, fixture, member.Username, "other-user-password")
	for _, action := range []string{"health", "pair"} {
		response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/sources/%d/%s", source.ID, action), cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
		if response.Code != http.StatusNotFound || response.Header().Get("Location") != "" {
			t.Fatalf("foreign source %s = %d location=%q", action, response.Code, response.Header().Get("Location"))
		}
	}
}
