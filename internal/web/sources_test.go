package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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
				if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), test.wantDetail) || len(jobs) != 0 {
					t.Fatalf("pair POST prerequisite = %d body=%q jobs=%+v", recorder.Code, recorder.Body.String(), jobs)
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
