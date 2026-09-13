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

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestSourcesPairFromTheLinkedProfileWithoutABooking(t *testing.T) {
	for _, test := range []struct {
		name                            string
		enabled                         bool
		loginURL, wantLabel, wantDetail string
	}{
		{name: "ready without a booking", enabled: true, loginURL: "https://example.test/login", wantLabel: "Pair with Yodel"},
		{name: "disabled profile", loginURL: "https://example.test/login", wantLabel: "Enable sign-in", wantDetail: "enable the Yodel sign-in"},
		{name: "unapproved profile login URL", enabled: true, loginURL: "https://unapproved.example/login", wantLabel: "Review sign-in", wantDetail: "Yodel login URL must use an approved Yodel origin"},
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
			recorder := serveForm(fixture, http.MethodGet, "/sources", cookies, nil)
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
				wantURL := "/lakes/buntzen#connection"
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
				if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/sources?notice=pairing-unavailable" || len(jobs) != 0 {
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

func TestSourceConnectionActionsReturnToSourcesWithoutProviderErrors(t *testing.T) {
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
			approveTestBlueBubbles(t, &fixture, provider.URL)
			source, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
				Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: provider.URL,
				ProviderConfig: bluebubbles.Config{BaseURL: provider.URL, Password: password},
			})
			if err != nil {
				t.Fatal(err)
			}
			cookies := loginCookies(t, fixture)
			response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/sources/%d/health", source.ID), cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
			wantLocation, wantMessage := "/sources?ok=healthy", "Provider authentication succeeded."
			if !healthy {
				wantLocation, wantMessage = "/sources?notice=provider-unavailable", "connection test failed"
			}
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != wantLocation || calls.Load() != 1 {
				t.Fatalf("connection action = %d location=%q calls=%d", response.Code, response.Header().Get("Location"), calls.Load())
			}
			page := serveForm(fixture, http.MethodGet, strings.SplitN(wantLocation, "#", 2)[0], cookies, nil)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), wantMessage) || !strings.Contains(page.Body.String(), `href="/sources" aria-current="page"`) {
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

func TestPairingQueueFailuresReturnToSourcesWithoutCreatingAnotherJob(t *testing.T) {
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
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/sources?notice="+notice {
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

func TestAmbiguousSourcePairingSelectsAnOwnedSignInWithoutChangingTheDefault(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	defaultSource, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: "Default Twilio", Provider: model.OTPProviderTwilio, Identity: "twilio:pairing-default", ProviderConfig: map[string]string{"auth_token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	secondary, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: "Secondary Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://messages.example.test:1234", ProviderConfig: bluebubbles.Config{BaseURL: "http://messages.example.test:1234", Password: "synthetic-password"}})
	if err != nil {
		t.Fatal(err)
	}
	var profiles []model.Profile
	for _, input := range []struct {
		name, login string
		enabled     bool
	}{
		{"First Yodel identity", "https://example.test/login", true},
		{"Second Yodel identity", "https://example.test/login", true},
		{"Disabled Yodel identity", "https://example.test/login", false},
		{"Invalid Yodel identity", "https://unapproved.example/login", true},
	} {
		profile, err := resources.CreateProfile(ctx, store.ProfileInput{Name: input.name, OTPSourceID: defaultSource.ID, LoginProbeURL: input.login, Headless: true, DefaultTimeoutMS: 15_000, Enabled: input.enabled, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
		if err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, profile)
	}
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "foreign-pairing-owner", Password: "private pairing owner password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := createImmediateWebBooking(t, f, member.ID, "Private Yodel identity", true)
	cookies := loginCookies(t, f)
	page := serveForm(f, http.MethodGet, "/sources", cookies, nil)
	body := page.Body.String()
	pairURL := fmt.Sprintf("/sources/%d/pair", secondary.ID)
	if page.Code != http.StatusOK || !strings.Contains(body, `action="`+pairURL+`"`) || !strings.Contains(body, `name="profile_id"`) || !strings.Contains(body, "Your default OTP source stays unchanged") {
		t.Fatalf("ambiguous secondary source lacks an explicit pairing choice: %d %s", page.Code, body)
	}
	for _, profile := range profiles[:2] {
		if !strings.Contains(body, fmt.Sprintf(`value="%d"`, profile.ID)) || !strings.Contains(body, profile.Name) {
			t.Fatalf("pairing choices omit owned enabled identity %s", profile.Name)
		}
	}
	for _, profile := range append(profiles[2:], foreign) {
		if strings.Contains(body, profile.Name) {
			t.Fatalf("pairing choices expose an unusable or foreign identity %s", profile.Name)
		}
	}
	csrf := csrfFrom(cookies)
	for _, selected := range []string{"", "not-an-id", "0", "-1", fmt.Sprint(profiles[2].ID), fmt.Sprint(profiles[3].ID)} {
		response := serveForm(f, http.MethodPost, pairURL, cookies, url.Values{"csrf_token": {csrf}, "profile_id": {selected}})
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/sources?notice=pairing-unavailable" {
			t.Fatalf("invalid pairing selection %q = %d %s", selected, response.Code, response.Body.String())
		}
	}
	for _, selected := range []string{fmt.Sprint(foreign.ID), "999999"} {
		response := serveForm(f, http.MethodPost, pairURL, cookies, url.Values{"csrf_token": {csrf}, "profile_id": {selected}})
		if response.Code != http.StatusNotFound {
			t.Fatalf("foreign or missing pairing selection %q = %d", selected, response.Code)
		}
	}
	response := serveForm(f, http.MethodPost, pairURL, cookies, url.Values{"profile_id": {fmt.Sprint(profiles[1].ID)}})
	if response.Code != http.StatusForbidden {
		t.Fatalf("pairing selection without CSRF = %d", response.Code)
	}
	jobs, err := resources.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("rejected pairing changed the queue: %+v %v", jobs, err)
	}
	response = serveForm(f, http.MethodPost, pairURL, cookies, url.Values{"csrf_token": {csrf}, "profile_id": {fmt.Sprint(profiles[1].ID)}, "user_id": {fmt.Sprint(member.ID)}, "source_id": {fmt.Sprint(defaultSource.ID)}})
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/jobs/") {
		t.Fatalf("owned explicit pairing = %d %s", response.Code, response.Body.String())
	}
	jobs, err = resources.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ProfileID != profiles[1].ID || jobs[0].OTPSourceID != secondary.ID || jobs[0].UserID != f.admin.ID || jobs[0].BookingRequestID != nil || jobs[0].Command != model.CommandAuthCheck {
		t.Fatalf("pairing did not bind the selected source and identity: %+v %v", jobs, err)
	}
	retainedDefault, err := resources.GetDefaultOTPSource(ctx)
	if err != nil || retainedDefault.ID != defaultSource.ID {
		t.Fatalf("pairing another source changed the default: %+v %v", retainedDefault, err)
	}
	retainedProfile, err := resources.GetProfile(ctx, profiles[1].ID)
	if err != nil || retainedProfile.OTPSourceID != defaultSource.ID {
		t.Fatalf("pairing changed the existing identity: %+v %v", retainedProfile, err)
	}
}
