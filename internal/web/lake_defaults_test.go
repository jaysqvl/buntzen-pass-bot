package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestLakeSelectorCarriesApprovedCatalogDefaults(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	page := serveForm(fixture, http.MethodGet, "/bookings/new", cookies, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("booking form status = %d", page.Code)
	}
	matches := regexp.MustCompile(`data-lake-defaults="([^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	if len(matches) != len(destinations.List()) {
		t.Fatalf("rendered defaults for %d lakes, want %d", len(matches), len(destinations.List()))
	}
	for _, match := range matches {
		var defaults lakeFormDefaults
		if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &defaults); err != nil {
			t.Fatalf("option data was not valid escaped JSON: %v", err)
		}
		lake, err := destinations.Resolve(defaults.ID)
		if err != nil {
			t.Fatal(err)
		}
		lake = lake.WithOrigin(fixture.server.config.YodelOrigins[0])
		if defaults.AllDayPassURL != lake.AllDayPassURL || defaults.HalfDayPassURL != lake.HalfDayPassURL || defaults.Timezone != lake.Timezone || defaults.ReleaseTime != lake.ReleaseTime || defaults.ReleasePolicy != lakeReleasePolicy(lake) {
			t.Fatalf("lake selector defaults disagree with approved catalog: %+v", defaults)
		}
		if len(defaults.Passes) != len(lake.SupportedPasses) {
			t.Fatalf("pass choices missing from selector defaults: %+v", defaults)
		}
		if !slices.Equal(defaults.PreferredPasses, lake.SupportedPasses) || defaults.ReleaseDaysBefore != lake.ReleaseDaysBefore {
			t.Fatalf("catalog pass order or release days missing: %+v", defaults)
		}
		for index, pass := range defaults.Passes {
			if pass.Value != lake.SupportedPasses[index] || pass.Label != passOptionLabel(pass.Value) {
				t.Fatalf("pass choices changed order or lost labels: %+v", defaults)
			}
		}
	}
}

func bookingDefaultsValues(profileID int64, settings model.LakeSettings, accountDefaults ...model.AccountSettings) url.Values {
	account := model.DefaultAccountSettings()
	if len(accountDefaults) > 0 {
		account = accountDefaults[0]
	}
	vehicle := settings.VehicleKeyword
	if vehicle == "" {
		vehicle = "Example Vehicle"
	}
	values := url.Values{
		"name": {"Personal defaults snapshot"}, "lake_id": {settings.LakeID}, "profile_id": {strconv.FormatInt(profileID, 10)},
		"target_date": {"2030-07-20"}, "timezone": {settings.Timezone}, "release_time": {settings.ReleaseTime},
		"release_days_before": {strconv.Itoa(settings.ReleaseDaysBefore)}, "enabled": {"1"}, "confirmation_mode": {"manual"},
		"all_day_pass_url": {settings.AllDayPassURL}, "half_day_pass_url": {settings.HalfDayPassURL},
		"vehicle_keyword":     {vehicle},
		"prep_minutes_before": {strconv.Itoa(account.PrepMinutesBefore)}, "auth_deadline_minutes_before": {strconv.Itoa(account.AuthDeadlineMinutesBefore)},
		"poll_deadline_seconds": {strconv.Itoa(account.PollDeadlineSeconds)}, "poll_min_seconds": {strconv.FormatFloat(account.PollMinSeconds, 'f', -1, 64)},
		"poll_max_seconds": {strconv.FormatFloat(account.PollMaxSeconds, 'f', -1, 64)},
	}
	for slot := 0; slot < 3; slot++ {
		pass := ""
		if slot < len(settings.PreferredPasses) {
			pass = string(settings.PreferredPasses[slot])
		}
		values.Set(fmt.Sprintf("pass_priority_%d", slot+1), pass)
	}
	return values
}

func TestPersonalLakeDefaultsAffectNewBookingsAndPreserveSavedSnapshots(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	profile, existing := createImmediateWebBooking(t, fixture, fixture.admin.ID, "Existing lake visit", true)
	lake, _ := destinations.Resolve(destinations.DefaultLakeID)
	settings := model.DefaultLakeSettings(lake.WithOrigin(fixture.cfg.YodelOrigins[0]))
	settings.Timezone, settings.ReleaseTime, settings.ReleaseDaysBefore = "Europe/London", "09:45", 3
	settings.AllDayPassURL, settings.HalfDayPassURL = "https://example.test/personal-all", "https://example.test/personal-half"
	settings.PreferredPasses = []model.PassType{model.PassMorning, model.PassAllDay}
	settings.VehicleKeyword = "Personal lake vehicle"
	accountSettings := model.DefaultAccountSettings()
	accountSettings.PrepMinutesBefore, accountSettings.AuthDeadlineMinutesBefore = 45, 10
	accountSettings.PollDeadlineSeconds, accountSettings.PollMinSeconds, accountSettings.PollMaxSeconds = 300, 0.05, 1.45
	if _, err := fixture.store.ForUser(fixture.admin.ID).SaveAccountSettings(ctx, accountSettings); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ForUser(fixture.admin.ID).SaveLakeSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{Username: "other-lake-defaults", Password: "another long password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := createImmediateWebBooking(t, fixture, member.ID, "Private other account", true)
	cookies := loginCookies(t, fixture)
	page := serveForm(fixture, http.MethodGet, "/bookings/new?lake_id=buntzen", cookies, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("new form=%d %s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for name, values := range bookingDefaultsValues(profile.ID, settings, accountSettings) {
		if name == "profile_id" || name == "name" || name == "target_date" || name == "lake_id" || name == "enabled" || name == "confirmation_mode" || strings.HasPrefix(name, "pass_priority_") {
			continue
		}
		if !strings.Contains(body, `name="`+name+`" value="`+values[0]+`"`) {
			t.Errorf("new form missing personal default %s=%s", name, values[0])
		}
	}
	assertBookingPassChoices(t, body, []string{"morning", "all_day", ""})
	if strings.Contains(body, foreign.Name) || !strings.Contains(body, profile.Name) {
		t.Fatal("lake selector profile defaults did not stay scoped to the current account")
	}
	var defaults lakeFormDefaults
	encoded := regexp.MustCompile(`data-lake-defaults="([^"]+)"`).FindStringSubmatch(body)
	if len(encoded) != 2 || json.Unmarshal([]byte(html.UnescapeString(encoded[1])), &defaults) != nil {
		t.Fatalf("missing selector data: %v", encoded)
	}
	if defaults.Timezone != settings.Timezone || defaults.ReleaseDaysBefore != 3 || !slices.Equal(defaults.PreferredPasses, []string{"morning", "all_day"}) || len(defaults.Passes) != 3 {
		t.Fatalf("personal selector defaults lost preferences or supported alternatives: %+v", defaults)
	}
	for _, timingName := range []string{"prepMinutesBefore", "authDeadlineMinutesBefore", "pollDeadlineSeconds", "pollMinSeconds", "pollMaxSeconds"} {
		if strings.Contains(encoded[1], timingName) {
			t.Errorf("global timing %s leaked into the lake-selector defaults", timingName)
		}
	}
	for _, timingName := range []string{"poll_min_seconds", "poll_max_seconds"} {
		field := regexp.MustCompile(`<input[^>]+name="` + timingName + `"[^>]+>`).FindString(body)
		if !strings.Contains(field, `min="0.05"`) || !strings.Contains(field, `step="0.05"`) {
			t.Errorf("booking field %s cannot accept supported global timing defaults: %s", timingName, field)
		}
	}
	otherCookies := loginCookiesAs(t, fixture, member.Username, "another long password")
	otherPage := serveForm(fixture, http.MethodGet, "/bookings/new", otherCookies, nil)
	if otherPage.Code != http.StatusOK || strings.Contains(otherPage.Body.String(), "personal-all") || !strings.Contains(otherPage.Body.String(), `name="timezone" value="America/Vancouver"`) {
		t.Fatal("personal lake defaults leaked to another account")
	}
	for _, want := range []string{`name="prep_minutes_before" value="30"`, `name="auth_deadline_minutes_before" value="5"`, `name="poll_deadline_seconds" value="120"`, `name="poll_min_seconds" value="1.4"`, `name="poll_max_seconds" value="3.6"`} {
		if !strings.Contains(otherPage.Body.String(), want) {
			t.Errorf("global account timing leaked into another account; missing %s", want)
		}
	}
	existingPage := serveForm(fixture, http.MethodGet, fmt.Sprintf("/bookings/%d", existing.ID), cookies, nil)
	if existingPage.Code != http.StatusOK || !strings.Contains(existingPage.Body.String(), `name="timezone" value="UTC"`) || !strings.Contains(existingPage.Body.String(), `name="release_days_before" value="1"`) {
		t.Fatal("saved booking was changed by personal defaults")
	}
	assertBookingPassChoices(t, existingPage.Body.String(), []string{"all_day", "", ""})
	if !strings.Contains(existingPage.Body.String(), `name="prep_minutes_before" value="30"`) {
		t.Fatal("saving general timing defaults changed an existing request")
	}

	values := bookingDefaultsValues(profile.ID, settings, accountSettings)
	values.Set("csrf_token", csrfFrom(cookies))
	response := serveForm(fixture, http.MethodPost, "/bookings/new", cookies, values)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("create snapshot=%d %s", response.Code, response.Body.String())
	}
	bookings, err := fixture.store.ForUser(fixture.admin.ID).ListBookingRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot model.BookingRequest
	for _, booking := range bookings {
		if booking.Name == values.Get("name") {
			snapshot = booking
		}
	}
	if snapshot.ID == 0 || snapshot.ReleaseDaysBefore == nil || *snapshot.ReleaseDaysBefore != 3 || snapshot.Timezone != settings.Timezone || snapshot.PrepMinutesBefore != 45 || snapshot.PollMinSeconds != 0.05 || snapshot.PollMaxSeconds != 1.45 || snapshot.VehicleKeyword != settings.VehicleKeyword {
		t.Fatalf("personal settings did not persist as a booking snapshot: %+v", snapshot)
	}
	settings.Timezone, settings.ReleaseDaysBefore = "UTC", 0
	settings.VehicleKeyword = "Different lake vehicle"
	if _, err := fixture.store.ForUser(fixture.admin.ID).SaveLakeSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	accountSettings.PrepMinutesBefore, accountSettings.PollMaxSeconds = 60, 2
	if _, err := fixture.store.ForUser(fixture.admin.ID).SaveAccountSettings(ctx, accountSettings); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/bookings/%d", snapshot.ID)
	page = serveForm(fixture, http.MethodGet, path, cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="timezone" value="Europe/London"`) || !strings.Contains(page.Body.String(), `name="release_days_before" value="3"`) {
		t.Fatal("changing lake defaults changed a saved request snapshot")
	}
	if !strings.Contains(page.Body.String(), `name="prep_minutes_before" value="45"`) || !strings.Contains(page.Body.String(), `name="poll_max_seconds" value="1.45"`) {
		t.Fatal("changing general defaults changed the saved request's timing")
	}
	if !strings.Contains(page.Body.String(), `name="vehicle_keyword" value="Personal lake vehicle"`) {
		t.Fatal("changing lake vehicle changed the saved request's vehicle")
	}
	newPage := serveForm(fixture, http.MethodGet, "/bookings/new", cookies, nil)
	if newPage.Code != http.StatusOK || !strings.Contains(newPage.Body.String(), `name="prep_minutes_before" value="60"`) || !strings.Contains(newPage.Body.String(), `name="poll_max_seconds" value="2"`) {
		t.Fatal("new request did not receive updated account timing defaults")
	}
	values.Del("release_days_before") // A pre-settings client may still submit its old form.
	values.Del("vehicle_keyword")
	values.Set("name", "Legacy client edit")
	response = serveForm(fixture, http.MethodPost, path, cookies, values)
	updated, err := fixture.store.ForUser(fixture.admin.ID).GetBookingRequest(ctx, snapshot.ID)
	if response.Code != http.StatusSeeOther || err != nil || updated.EffectiveReleaseDaysBefore() != 3 || updated.VehicleKeyword != snapshot.VehicleKeyword {
		t.Fatalf("legacy edit reset a saved policy: status=%d booking=%+v err=%v", response.Code, updated, err)
	}
	values.Set("release_days_before", "0")
	response = serveForm(fixture, http.MethodPost, path, cookies, values)
	updated, err = fixture.store.ForUser(fixture.admin.ID).GetBookingRequest(ctx, snapshot.ID)
	if response.Code != http.StatusSeeOther || err != nil || updated.ReleaseDaysBefore == nil || *updated.ReleaseDaysBefore != 0 {
		t.Fatalf("explicit same-day release not saved: status=%d booking=%+v err=%v", response.Code, updated, err)
	}
	values.Set("vehicle_keyword", "")
	response = serveForm(fixture, http.MethodPost, path, cookies, values)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "vehicle is required") {
		t.Fatalf("explicitly empty vehicle was silently replaced: %d %s", response.Code, response.Body.String())
	}
}

func TestBookingValidationKeepsSubmittedLakeOverrides(t *testing.T) {
	fixture := newWebFixture(t)
	profile, booking := createImmediateWebBooking(t, fixture, fixture.admin.ID, "Validation visit", true)
	lake, _ := destinations.Resolve(destinations.DefaultLakeID)
	settings := model.DefaultLakeSettings(lake.WithOrigin(fixture.cfg.YodelOrigins[0]))
	cookies := loginCookies(t, fixture)
	values := bookingDefaultsValues(profile.ID, settings)
	values.Set("csrf_token", csrfFrom(cookies))
	values.Set("timezone", "Europe/London")
	values.Set("release_days_before", "12")
	values.Set("prep_minutes_before", "not a number")
	values.Set("vehicle_keyword", "Unsaved vehicle")
	values.Set("pass_priority_1", "morning")
	values.Set("pass_priority_2", "")
	values.Set("pass_priority_3", "all_day")
	response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/bookings/%d", booking.ID), cookies, values)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid booking=%d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`name="timezone" value="Europe/London"`, `name="release_days_before" value="12"`, `name="prep_minutes_before" value="not a number"`, `value="buntzen" selected`, `value="` + strconv.FormatInt(profile.ID, 10) + `" selected`, "Passes release 12 days before your visit"} {
		if !strings.Contains(body, want) {
			t.Errorf("validation did not preserve %q", want)
		}
	}
	assertBookingPassChoices(t, body, []string{"morning", "", "all_day"})
	if !strings.Contains(body, `name="vehicle_keyword" value="Unsaved vehicle"`) {
		t.Fatal("validation discarded submitted vehicle keyword")
	}
}

func TestLakeSelectorScopesProvidersAndKeepsCurrentDisabledSignIn(t *testing.T) {
	lake, _ := destinations.Resolve(destinations.DefaultLakeID)
	settings := model.DefaultLakeSettings(lake)
	profiles := []model.Profile{
		{ID: 1, Name: "Legacy profile", Enabled: true},
		{ID: 2, ProviderID: "another-provider", Name: "Other provider", Enabled: true},
		{ID: 3, LakeID: lake.ID, Name: "Disabled", Enabled: false},
		{ID: 4, LakeID: lake.ID, Name: "Current disabled", Enabled: false},
		{ID: 5, LakeID: "another-lake", ProviderID: lake.ProviderID, Name: "Shared Yodel sign-in", Enabled: true},
	}
	option := lakeSelectOption(lake, settings, lake.ID, profiles, 4)
	var defaults lakeFormDefaults
	if err := json.Unmarshal([]byte(option.LakeDefaults), &defaults); err != nil {
		t.Fatal(err)
	}
	if len(defaults.Profiles) != 3 || defaults.Profiles[0].Value != "1" || defaults.Profiles[1].Value != "4" || defaults.Profiles[2].Value != "5" {
		t.Fatalf("lake defaults offered incompatible or unrelated disabled profiles: %+v", defaults.Profiles)
	}
}
