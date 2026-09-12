package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestHomeShowsGlobalYodelSignInsAndOnlyOwnedResources(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "other-member", Password: "other-member-password"})
	if err != nil {
		t.Fatal(err)
	}
	var own []model.Profile
	for _, owner := range []struct {
		id   int64
		name string
	}{{f.admin.ID, "Owned"}, {member.ID, "Private"}} {
		source := createDefaultSignInSource(t, f, owner.id, owner.name+" inbox")
		for i := 1; i <= 2; i++ {
			profile, err := f.store.ForUser(owner.id).CreateProfile(ctx, store.ProfileInput{ProviderID: "yodel", Name: fmt.Sprintf("%s sign-in %d", owner.name, i), DefaultVehicle: owner.name + " legacy vehicle", LoginProbeURL: "https://example.test/login", OTPSourceID: source.ID, DefaultTimeoutMS: 15000, Enabled: true, Headless: true, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
			if err != nil {
				t.Fatal(err)
			}
			if owner.id == f.admin.ID {
				own = append(own, profile)
			}
		}
	}
	response := serveForm(f, http.MethodGet, "/", loginCookies(t, f), nil)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("home=%d %s", response.Code, body)
	}
	for _, text := range []string{`id="yodel-sign-in"`, "Yodel sign-in", "Owned inbox", "Owned sign-in 1", "Owned sign-in 2", "Signing in does not book a pass", `href="/profiles/new"`, `href="/sources"`, `href="/lakes"`, `href="/settings"`, "2. Sign in to Yodel", "Already queued jobs remain scheduled"} {
		if !strings.Contains(body, text) {
			t.Errorf("Home missing %q", text)
		}
	}
	for _, profile := range own {
		if !strings.Contains(body, fmt.Sprintf(`action="/profiles/%d/sign-in"`, profile.ID)) {
			t.Errorf("Home lost action for existing sign-in%d", profile.ID)
		}
	}
	for _, text := range []string{"Private inbox", "Private sign-in", "legacy vehicle", "5559876543", "synthetic", "https://example.test/login"} {
		if strings.Contains(body, text) {
			t.Errorf("Home exposed %q", text)
		}
	}
}

func TestHomeGuidesDefaultOTPSetupBeforeYodelSignIn(t *testing.T) {
	f := newWebFixture(t)
	response := serveForm(f, http.MethodGet, "/", loginCookies(t, f), nil)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "Choose default OTP source") || !strings.Contains(body, "Configure BlueBubbles or Twilio") || strings.Contains(body, `action="/profiles/`) {
		t.Fatalf("empty Home=%d %s", response.Code, body)
	}
}

func TestUpcomingVisitsUseVisitDateAndLakeTimezone(t *testing.T) {
	now := time.Date(2026, 9, 13, 1, 0, 0, 0, time.UTC)
	bookings := []model.BookingRequest{
		{ID: 1, Name: "Alphabetically first, later visit", TargetDate: "2026-09-20", Timezone: "America/Vancouver", Enabled: true},
		{ID: 2, TargetDate: "2026-09-11", Timezone: "America/Vancouver", Enabled: true},
		{ID: 3, TargetDate: "2026-09-12", Timezone: "America/Vancouver", Enabled: true},
		{ID: 4, TargetDate: "2026-09-12", Timezone: "UTC", Enabled: true},
		{ID: 5, TargetDate: "2026-09-13", Timezone: "America/Vancouver", Enabled: false},
		{ID: 6, TargetDate: "2026-09-14", Timezone: "America/Vancouver", Enabled: true},
		{ID: 7, TargetDate: "2026-09-15", Timezone: "America/Vancouver", Enabled: true},
	}
	var ids []int64
	for _, booking := range upcomingBookings(bookings, now) {
		ids = append(ids, booking.ID)
	}
	if !slices.Equal(ids, []int64{3, 6, 7}) {
		t.Fatalf("upcoming IDs = %v, want local-today then next two enabled visits", ids)
	}
	if bookings[0].ID != 1 {
		t.Fatal("dashboard ordering changed the original request list")
	}
}
