package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func createImmediateWebBooking(t *testing.T, fixture webFixture, ownerID int64, name string, enabled bool) (model.Profile, model.BookingRequest) {
	t.Helper()
	ctx := context.Background()
	resources := fixture.store.ForUser(ownerID)
	source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: name + " inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:" + name, ProviderConfig: map[string]string{"auth_token": "synthetic-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{Name: name + " profile", DefaultVehicle: "Example Vehicle", LoginProbeURL: "https://example.test/login", OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15000, Enabled: enabled, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := resources.CreateBookingRequest(ctx, model.BookingRequest{
		Name: name + " booking", ProfileID: profile.ID, Enabled: true, ScheduleEnabled: true,
		TargetDate: time.Now().UTC().Format(time.DateOnly), Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120, PollMinSeconds: 1, PollMaxSeconds: 2,
		ConfirmationMode: model.RunModeAuto, AllDayPassURL: "https://example.test/all", CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile, booking
}

func TestBookNowHTTPForcesManualApprovalAndRejectsDuplicate(t *testing.T) {
	fixture := newWebFixture(t)
	_, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "immediate-http", true)
	cookies := loginCookies(t, fixture)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "command": {"book"}, "timing": {"now"}, "mode": {"auto"}}
	path := fmt.Sprintf("/bookings/%d/run", booking.ID)
	response := serveForm(fixture, http.MethodPost, path, cookies, form)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("book now response=%d body=%q", response.Code, response.Body.String())
	}
	jobs, err := fixture.store.ForUser(fixture.admin.ID).ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job := jobs[0]
	if !job.RunImmediately || job.Command != model.CommandBook || job.RunMode != model.RunModeManual || job.ExpiresAt == nil || job.ExpiresAt.Sub(job.DueAt) != 15*time.Minute {
		t.Fatalf("posted automatic mode escaped manual immediate policy: %+v", job)
	}
	response = serveForm(fixture, http.MethodPost, path, cookies, form)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "notice=queue-pending") {
		t.Fatalf("duplicate response=%d", response.Code)
	}
	for _, invalid := range []url.Values{
		{"csrf_token": {csrfFrom(cookies)}, "command": {"auth-check"}, "timing": {"now"}},
		{"csrf_token": {csrfFrom(cookies)}, "command": {"book"}, "timing": {"whenever"}},
	} {
		response = serveForm(fixture, http.MethodPost, path, cookies, invalid)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/bookings?notice=booking-action" {
			t.Fatalf("invalid timing response=%d body=%q", response.Code, response.Body.String())
		}
	}
}

func TestBookNowFormsExplainAutoQueueingAndOwnedProfilePreselection(t *testing.T) {
	fixture := newWebFixture(t)
	profile, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "selected", true)
	disabled, _ := createImmediateWebBooking(t, fixture, fixture.admin.ID, "disabled", false)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{Username: "another-owner", Password: "a long member password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := createImmediateWebBooking(t, fixture, member.ID, "foreign", true)
	cookies := loginCookies(t, fixture)
	page := serveForm(fixture, http.MethodGet, "/bookings", cookies, nil)
	for _, want := range []string{"Auto-queueing is off for this server", "Already queued jobs remain scheduled", "No booking queued", "Book now · manual approval", `name="timing" value="now"`, `name="command" value="book"`, fmt.Sprintf(`action="/bookings/%d/run"`, booking.ID)} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("booking page missing %q: %d body=%q", want, page.Code, page.Body.String())
		}
	}
	for _, test := range []struct {
		name     string
		id       int64
		selected bool
	}{
		{"owned enabled", profile.ID, true}, {"owned disabled", disabled.ID, false}, {"foreign", foreign.ID, false}, {"missing", 999999, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := serveForm(fixture, http.MethodGet, fmt.Sprintf("/bookings/new?profile_id=%d", test.id), cookies, nil)
			if page.Code != http.StatusOK {
				t.Fatalf("new booking=%d", page.Code)
			}
			selected := fmt.Sprintf(`value="%d" selected`, test.id)
			if strings.Contains(page.Body.String(), selected) != test.selected {
				t.Fatalf("profile selection mismatch for %s", test.name)
			}
			if strings.Contains(page.Body.String(), foreign.Name) {
				t.Fatal("foreign profile leaked into booking choices")
			}
		})
	}
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "command": {"book"}, "timing": {"now"}}
	foreignBookings, err := fixture.store.ForUser(member.ID).ListBookingRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/bookings/%d/run", foreignBookings[0].ID), cookies, form)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign booking POST=%d", response.Code)
	}
}

func TestBookNowExplainsUnreleasedAndExpiredDates(t *testing.T) {
	fixture := newWebFixture(t)
	_, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "immediate-dates", true)
	cookies := loginCookies(t, fixture)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "command": {"book"}, "timing": {"now"}}
	for _, test := range []struct {
		name    string
		days    int
		message string
	}{
		{"unreleased", 2, "Queue for release instead"},
		{"past", -1, "Choose today or a future date"},
	} {
		t.Run(test.name, func(t *testing.T) {
			booking.TargetDate = time.Now().UTC().AddDate(0, 0, test.days).Format(time.DateOnly)
			var err error
			booking, err = fixture.store.ForUser(fixture.admin.ID).UpdateBookingRequest(context.Background(), booking)
			if err != nil {
				t.Fatal(err)
			}
			response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/bookings/%d/run", booking.ID), cookies, form)
			if response.Code != http.StatusSeeOther {
				t.Fatalf("date response=%d", response.Code)
			}
			page := serveForm(fixture, http.MethodGet, response.Header().Get("Location"), cookies, nil)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), test.message) || !strings.Contains(page.Body.String(), `role="alert"`) {
				t.Fatalf("date notification=%d body=%q", page.Code, page.Body.String())
			}
		})
	}
	jobs, err := fixture.store.ForUser(fixture.admin.ID).ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("invalid dates queued jobs=%+v err=%v", jobs, err)
	}
}
