package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func settingsPageDefaults(t *testing.T, f webFixture) model.LakeSettings {
	t.Helper()
	lake, err := destinations.Resolve("buntzen")
	if err != nil {
		t.Fatal(err)
	}
	return model.DefaultLakeSettings(lake.WithOrigin(f.cfg.YodelOrigins[0]))
}

func TestSettingsAndLakePagesSavePersonalDefaultsAndResetOnlyTheirOwner(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "personal-settings-member", Password: "another personal settings password"})
	if err != nil {
		t.Fatal(err)
	}
	adminProfile, adminBooking := createImmediateWebBooking(t, f, f.admin.ID, "Administrator private lake", true)
	memberProfile, memberBooking := createImmediateWebBooking(t, f, member.ID, "Member private lake", true)
	adminCookies := loginCookies(t, f)
	memberCookies := loginCookiesAs(t, f, member.Username, "another personal settings password")
	owners := []struct {
		user                                model.User
		cookies                             []*http.Cookie
		profile                             model.Profile
		booking                             model.BookingRequest
		otherProfileName, timezone, channel string
		timeout, days                       int
	}{
		{f.admin, adminCookies, adminProfile, adminBooking, memberProfile.Name, "Europe/London", "chrome", 24000, 3},
		{member, memberCookies, memberProfile, memberBooking, adminProfile.Name, "America/Toronto", "chrome-beta", 31000, 0},
	}
	for _, owner := range owners {
		settings := settingsPageDefaults(t, f)
		settings.Timezone, settings.ReleaseDaysBefore, settings.ReleaseTime = owner.timezone, owner.days, "09:45"
		values := bookingDefaultsValues(owner.profile.ID, settings)
		values.Set("csrf_token", csrfFrom(owner.cookies))
		values.Set("user_id", strconv.FormatInt(f.admin.ID+member.ID-owner.user.ID, 10))
		response := serveForm(f, http.MethodPost, "/lakes/buntzen", owner.cookies, values)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/lakes/buntzen?ok=updated#defaults" {
			t.Fatalf("save personal lake defaults = %d %s", response.Code, response.Body.String())
		}
		accountValues := url.Values{
			"csrf_token": {csrfFrom(owner.cookies)}, "browser_channel": {owner.channel}, "default_timeout_ms": {strconv.Itoa(owner.timeout)},
			"user_id": values["user_id"],
		}
		response = serveForm(f, http.MethodPost, "/settings", owner.cookies, accountValues)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/settings?ok=updated" {
			t.Fatalf("save personal settings = %d %s", response.Code, response.Body.String())
		}
	}
	for _, owner := range owners {
		resources := f.store.ForUser(owner.user.ID)
		lake, err := resources.GetLakeSettings(ctx, "buntzen")
		if err != nil || lake.Timezone != owner.timezone || lake.ReleaseDaysBefore != owner.days || lake.UserID != owner.user.ID {
			t.Fatalf("personal lake defaults escaped owner: %+v %v", lake, err)
		}
		account, err := resources.GetAccountSettings(ctx)
		if err != nil || account.BrowserChannel != owner.channel || account.DefaultTimeoutMS != owner.timeout || account.UserID != owner.user.ID || account.Headless {
			t.Fatalf("personal browser defaults escaped owner: %+v %v", account, err)
		}
		page := serveForm(f, http.MethodGet, "/lakes/buntzen", owner.cookies, nil)
		body := page.Body.String()
		if page.Code != http.StatusOK || !strings.Contains(body, owner.profile.Name) || strings.Contains(body, owner.otherProfileName) || !strings.Contains(body, `name="timezone" value="`+owner.timezone+`"`) || !strings.Contains(body, "Personal defaults") {
			t.Fatalf("lake page did not keep profiles/defaults private: %d %s", page.Code, body)
		}
		page = serveForm(f, http.MethodGet, "/lakes", owner.cookies, nil)
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), owner.timezone) || !strings.Contains(page.Body.String(), `href="/lakes/buntzen"`) {
			t.Fatalf("lake index does not summarize personal defaults: %d %s", page.Code, page.Body.String())
		}
		page = serveForm(f, http.MethodGet, "/settings", owner.cookies, nil)
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `value="`+owner.channel+`" selected`) || !strings.Contains(page.Body.String(), `name="default_timeout_ms" value="`+strconv.Itoa(owner.timeout)+`"`) || !strings.Contains(page.Body.String(), `href="/sources"`) {
			t.Fatalf("settings page lost personal defaults or global sources link: %d %s", page.Code, page.Body.String())
		}
	}
	response := serveForm(f, http.MethodPost, "/lakes/buntzen/reset", adminCookies, url.Values{"csrf_token": {csrfFrom(adminCookies)}, "user_id": {strconv.FormatInt(member.ID, 10)}})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/lakes/buntzen?notice=lake-defaults-reset#defaults" {
		t.Fatalf("reset lake defaults = %d %s", response.Code, response.Body.String())
	}
	if _, err := f.store.ForUser(f.admin.ID).GetLakeSettings(ctx, "buntzen"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reset retained owner's saved defaults: %v", err)
	}
	memberDefaults, err := f.store.ForUser(member.ID).GetLakeSettings(ctx, "buntzen")
	if err != nil || memberDefaults.Timezone != "America/Toronto" {
		t.Fatalf("reset touched another account: %+v %v", memberDefaults, err)
	}
	page := serveForm(f, http.MethodGet, "/lakes/buntzen", adminCookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="timezone" value="America/Vancouver"`) || !strings.Contains(page.Body.String(), `name="all_day_pass_url" value="https://example.test/`) || !strings.Contains(page.Body.String(), "Built-in defaults") || strings.Contains(page.Body.String(), `action="/lakes/buntzen/reset"`) {
		t.Fatalf("reset did not restore approved built-in values: %d %s", page.Code, page.Body.String())
	}
	for _, owner := range owners {
		resources := f.store.ForUser(owner.user.ID)
		profile, err := resources.GetProfile(ctx, owner.profile.ID)
		if err != nil || !reflect.DeepEqual(profile, owner.profile) {
			t.Fatalf("settings save/reset changed an existing profile: %+v %v", profile, err)
		}
		booking, err := resources.GetBookingRequest(ctx, owner.booking.ID)
		if err != nil || !reflect.DeepEqual(booking, owner.booking) {
			t.Fatalf("settings save/reset changed an existing booking: %+v %v", booking, err)
		}
	}
}

func TestSettingsAndLakeMutationsRequireCSRF(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	savedLake, err := resources.SaveLakeSettings(ctx, settingsPageDefaults(t, f))
	if err != nil {
		t.Fatal(err)
	}
	savedAccount, err := resources.SaveAccountSettings(ctx, model.AccountSettings{Headless: true, BrowserChannel: "chrome", DefaultTimeoutMS: 23000})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	for _, test := range []struct {
		path   string
		values url.Values
	}{
		{"/lakes/buntzen", bookingDefaultsValues(1, settingsPageDefaults(t, f))},
		{"/lakes/buntzen/reset", url.Values{}},
		{"/settings", url.Values{"browser_channel": {"chrome-beta"}, "default_timeout_ms": {"31000"}}},
	} {
		for _, token := range []string{"", "invalid-token"} {
			test.values.Set("csrf_token", token)
			response := serveForm(f, http.MethodPost, test.path, cookies, test.values)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s accepted missing or invalid CSRF: %d %s", test.path, response.Code, response.Body.String())
			}
		}
	}
	lake, err := resources.GetLakeSettings(ctx, "buntzen")
	if err != nil || !reflect.DeepEqual(lake, savedLake) {
		t.Fatalf("CSRF-rejected request mutated lake defaults: %+v %v", lake, err)
	}
	account, err := resources.GetAccountSettings(ctx)
	if err != nil || !reflect.DeepEqual(account, savedAccount) {
		t.Fatalf("CSRF-rejected request mutated browser defaults: %+v %v", account, err)
	}
}

func TestLakeSettingsPageRejectsInvalidInputWithoutLosingDraft(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	saved, err := resources.SaveLakeSettings(ctx, settingsPageDefaults(t, f))
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	for _, test := range []struct {
		name, field, value, message string
	}{
		{"unapproved provider", "all_day_pass_url", "https://unapproved.example/pass", "approved Yodel origin"},
		{"invalid number", "prep_minutes_before", "not-a-number", "Preparation time must be a whole number"},
		{"unsupported preference", "pass_priority_1", "future-pass", "pass preference is not supported"},
		{"duplicate preference", "pass_priority_3", "morning", "each pass preference can only be selected once"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := bookingDefaultsValues(1, settingsPageDefaults(t, f))
			values.Set("csrf_token", csrfFrom(cookies))
			values.Set("timezone", "Europe/London")
			values.Set("release_days_before", "12")
			values.Set("pass_priority_1", "morning")
			values.Set("pass_priority_2", "")
			values.Set("pass_priority_3", "all_day")
			values.Set(test.field, test.value)
			response := serveForm(f, http.MethodPost, "/lakes/buntzen", cookies, values)
			body := response.Body.String()
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(body, test.message) {
				t.Fatalf("invalid lake defaults = %d %s", response.Code, body)
			}
			for _, name := range []string{"timezone", "release_days_before", "prep_minutes_before", "all_day_pass_url"} {
				if !strings.Contains(body, `name="`+name+`" value="`+values.Get(name)+`"`) {
					t.Errorf("validation lost entered %s=%q", name, values.Get(name))
				}
			}
			assertBookingPassChoices(t, body, []string{values.Get("pass_priority_1"), values.Get("pass_priority_2"), values.Get("pass_priority_3")})
			retained, err := resources.GetLakeSettings(ctx, "buntzen")
			if err != nil || !reflect.DeepEqual(retained, saved) {
				t.Fatalf("invalid submission changed saved defaults: %+v %v", retained, err)
			}
		})
	}
}

func TestPersonalSettingsValidationPreservesInput(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	saved, err := resources.SaveAccountSettings(ctx, model.AccountSettings{Headless: true, BrowserChannel: "chrome", DefaultTimeoutMS: 18000})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	values := url.Values{"csrf_token": {csrfFrom(cookies)}, "browser_channel": {"chrome-beta"}, "default_timeout_ms": {"not-a-number"}}
	response := serveForm(f, http.MethodPost, "/settings", cookies, values)
	body := response.Body.String()
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "Action timeout must be a whole number") || !strings.Contains(body, `name="default_timeout_ms" value="not-a-number"`) || !strings.Contains(body, `value="chrome-beta" selected`) {
		t.Fatalf("invalid browser defaults lost form input: %d %s", response.Code, body)
	}
	retained, err := resources.GetAccountSettings(ctx)
	if err != nil || !reflect.DeepEqual(retained, saved) {
		t.Fatalf("invalid browser defaults changed saved settings: %+v %v", retained, err)
	}
}

func TestUnknownLakeSettingsRoutesReturnNotFound(t *testing.T) {
	f := newWebFixture(t)
	cookies := loginCookies(t, f)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/lakes/unknown-lake"},
		{http.MethodPost, "/lakes/unknown-lake"},
		{http.MethodPost, "/lakes/unknown-lake/reset"},
	} {
		response := serveForm(f, route.method, route.path, cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, expected 404", route.method, route.path, response.Code)
		}
	}
	settings, err := f.store.ForUser(f.admin.ID).GetLakeSettings(context.Background(), "buntzen")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown lake mutation wrote default lake state: %+v %v", settings, err)
	}
}
