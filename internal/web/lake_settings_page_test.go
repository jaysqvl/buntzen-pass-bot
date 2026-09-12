package web

import (
	"context"
	"errors"
	"fmt"
	"html"
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

func lakeSettingsPageValues(settings model.LakeSettings) url.Values {
	values := url.Values{
		"vehicle_keyword": {settings.VehicleKeyword},
		"timezone":        {settings.Timezone}, "release_time": {settings.ReleaseTime}, "release_days_before": {strconv.Itoa(settings.ReleaseDaysBefore)},
		"all_day_pass_url": {settings.AllDayPassURL}, "half_day_pass_url": {settings.HalfDayPassURL},
	}
	for i := 0; i < 3; i++ {
		pass := ""
		if i < len(settings.PreferredPasses) {
			pass = string(settings.PreferredPasses[i])
		}
		values.Set(fmt.Sprintf("pass_priority_%d", i+1), pass)
	}
	return values
}

func accountSettingsPageValues(settings model.AccountSettings) url.Values {
	values := url.Values{
		"browser_channel": {settings.BrowserChannel}, "default_timeout_ms": {strconv.Itoa(settings.DefaultTimeoutMS)},
		"prep_minutes_before": {strconv.Itoa(settings.PrepMinutesBefore)}, "auth_deadline_minutes_before": {strconv.Itoa(settings.AuthDeadlineMinutesBefore)},
		"poll_deadline_seconds": {strconv.Itoa(settings.PollDeadlineSeconds)}, "poll_min_seconds": {strconv.FormatFloat(settings.PollMinSeconds, 'f', -1, 64)},
		"poll_max_seconds": {strconv.FormatFloat(settings.PollMaxSeconds, 'f', -1, 64)},
	}
	if settings.Headless {
		values.Set("headless", "1")
	}
	return values
}

func TestLakeVehicleFallbackUsesOnlyUnambiguousOwnedLegacyChoices(t *testing.T) {
	for _, test := range []struct {
		name, secondVehicle, want string
	}{
		{"matching legacy vehicles", "Legacy car", "Legacy car"},
		{"conflicting legacy vehicles", "Other car", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWebFixture(t)
			ctx := context.Background()
			resources := f.store.ForUser(f.admin.ID)
			source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: "Legacy inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:legacy-vehicle", ProviderConfig: map[string]string{"auth_token": "synthetic"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, legacy := range []struct{ name, lake, vehicle string }{
				{"First legacy sign-in", "buntzen", "Legacy car"},
				{"Second legacy sign-in", "buntzen", test.secondVehicle},
				{"New global sign-in", "buntzen", ""},
				{"Unrelated legacy lake", "other-lake", "Another lake car"},
			} {
				_, err := resources.CreateProfile(ctx, store.ProfileInput{Name: legacy.name, LakeID: legacy.lake, DefaultVehicle: legacy.vehicle, LoginProbeURL: "https://example.test/login", OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "foreign-vehicle-owner", Password: "another private vehicle password"})
			if err != nil {
				t.Fatal(err)
			}
			createImmediateWebBooking(t, f, member.ID, "Foreign vehicle", true)
			before, err := resources.ListProfiles(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cookies := loginCookies(t, f)
			for _, path := range []string{"/lakes/buntzen", "/bookings/new?lake_id=buntzen"} {
				page := serveForm(f, http.MethodGet, path, cookies, nil)
				if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="vehicle_keyword" value="`+test.want+`"`) {
					t.Fatalf("%s did not use the safe legacy vehicle fallback %q: %d %s", path, test.want, page.Code, page.Body.String())
				}
			}
			if _, err := resources.GetLakeSettings(ctx, "buntzen"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("reading legacy defaults persisted an override: %v", err)
			}
			after, err := resources.ListProfiles(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("reading defaults changed existing identities: %+v %v", after, err)
			}
			for _, vehicle := range []string{"Explicit lake car", ""} {
				settings := settingsPageDefaults(t, f)
				settings.VehicleKeyword = vehicle
				if _, err := resources.SaveLakeSettings(ctx, settings); err != nil {
					t.Fatal(err)
				}
				page := serveForm(f, http.MethodGet, "/lakes/buntzen", cookies, nil)
				if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="vehicle_keyword" value="`+vehicle+`"`) {
					t.Fatalf("saved vehicle choice %q was replaced by legacy defaults: %d %s", vehicle, page.Code, page.Body.String())
				}
			}
		})
	}
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
		preparation, window                 int
		retryMin, retryMax                  float64
	}{
		{f.admin, adminCookies, adminProfile, adminBooking, memberProfile.Name, "Europe/London", "chrome", 24000, 3, 50, 240, 2, 4},
		{member, memberCookies, memberProfile, memberBooking, adminProfile.Name, "America/Toronto", "chrome-beta", 31000, 0, 70, 450, 3, 6},
	}
	for _, owner := range owners {
		settings := settingsPageDefaults(t, f)
		settings.Timezone, settings.ReleaseDaysBefore, settings.ReleaseTime = owner.timezone, owner.days, "09:45"
		settings.VehicleKeyword = owner.user.Username + " vehicle"
		values := lakeSettingsPageValues(settings)
		values.Set("csrf_token", csrfFrom(owner.cookies))
		values.Set("user_id", strconv.FormatInt(f.admin.ID+member.ID-owner.user.ID, 10))
		response := serveForm(f, http.MethodPost, "/lakes/buntzen", owner.cookies, values)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/lakes/buntzen?ok=updated#defaults" {
			t.Fatalf("save personal lake defaults = %d %s", response.Code, response.Body.String())
		}
		account := model.DefaultAccountSettings()
		account.Headless, account.BrowserChannel, account.DefaultTimeoutMS = false, owner.channel, owner.timeout
		account.PrepMinutesBefore, account.AuthDeadlineMinutesBefore, account.PollDeadlineSeconds = owner.preparation, 10, owner.window
		account.PollMinSeconds, account.PollMaxSeconds = owner.retryMin, owner.retryMax
		accountValues := accountSettingsPageValues(account)
		accountValues.Set("csrf_token", csrfFrom(owner.cookies))
		accountValues.Set("user_id", values.Get("user_id"))
		response = serveForm(f, http.MethodPost, "/settings", owner.cookies, accountValues)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/settings?ok=updated" {
			t.Fatalf("save personal settings = %d %s", response.Code, response.Body.String())
		}
	}
	for _, owner := range owners {
		resources := f.store.ForUser(owner.user.ID)
		lake, err := resources.GetLakeSettings(ctx, "buntzen")
		if err != nil || lake.Timezone != owner.timezone || lake.ReleaseDaysBefore != owner.days || lake.UserID != owner.user.ID || lake.VehicleKeyword != owner.user.Username+" vehicle" {
			t.Fatalf("personal lake defaults escaped owner: %+v %v", lake, err)
		}
		account, err := resources.GetAccountSettings(ctx)
		if err != nil || account.BrowserChannel != owner.channel || account.DefaultTimeoutMS != owner.timeout || account.UserID != owner.user.ID || account.Headless {
			t.Fatalf("personal browser defaults escaped owner: %+v %v", account, err)
		}
		if account.PrepMinutesBefore != owner.preparation || account.AuthDeadlineMinutesBefore != 10 || account.PollDeadlineSeconds != owner.window || account.PollMinSeconds != owner.retryMin || account.PollMaxSeconds != owner.retryMax {
			t.Fatalf("personal preparation/retry defaults escaped owner: %+v", account)
		}
		page := serveForm(f, http.MethodGet, "/lakes/buntzen", owner.cookies, nil)
		body := page.Body.String()
		if page.Code != http.StatusOK || strings.Contains(body, owner.profile.Name) || strings.Contains(body, owner.otherProfileName) || !strings.Contains(body, `name="timezone" value="`+owner.timezone+`"`) || !strings.Contains(body, `name="vehicle_keyword" value="`+owner.user.Username+` vehicle"`) || !strings.Contains(body, "Personal defaults") {
			t.Fatalf("lake page did not keep vehicle/defaults personal and sign-in management separate: %d %s", page.Code, body)
		}
		if strings.Contains(body, "/profiles/") || strings.Contains(body, "Profiles &amp; vehicles") {
			t.Fatal("lake settings still expose sign-in management")
		}
		for _, name := range []string{"prep_minutes_before", "auth_deadline_minutes_before", "poll_deadline_seconds", "poll_min_seconds", "poll_max_seconds"} {
			if strings.Contains(body, `name="`+name+`"`) {
				t.Fatalf("lake settings still contain global %s", name)
			}
		}
		page = serveForm(f, http.MethodGet, "/lakes", owner.cookies, nil)
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), owner.timezone) || !strings.Contains(page.Body.String(), `href="/lakes/buntzen"`) {
			t.Fatalf("lake index does not summarize personal defaults: %d %s", page.Code, page.Body.String())
		}
		page = serveForm(f, http.MethodGet, "/settings", owner.cookies, nil)
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `value="`+owner.channel+`" selected`) || !strings.Contains(page.Body.String(), `name="default_timeout_ms" value="`+strconv.Itoa(owner.timeout)+`"`) || !strings.Contains(page.Body.String(), `href="/sources"`) {
			t.Fatalf("settings page lost personal defaults or global sources link: %d %s", page.Code, page.Body.String())
		}
		if strings.Contains(page.Body.String(), "Manage OTP sources") || strings.Contains(page.Body.String(), "<h2>OTP sources</h2>") {
			t.Fatal("OTP configuration appears inside global Settings instead of its own page")
		}
		for name, values := range accountSettingsPageValues(account) {
			if name != "browser_channel" && name != "headless" && !strings.Contains(page.Body.String(), `name="`+name+`" value="`+values[0]+`"`) {
				t.Fatalf("global settings page lost %s=%s", name, values[0])
			}
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
		account, err := resources.GetAccountSettings(ctx)
		if err != nil || account.PrepMinutesBefore != owner.preparation || account.PollDeadlineSeconds != owner.window || account.PollMinSeconds != owner.retryMin || account.PollMaxSeconds != owner.retryMax {
			t.Fatalf("resetting lake defaults changed global timing: %+v %v", account, err)
		}
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
	account := model.DefaultAccountSettings()
	account.BrowserChannel, account.DefaultTimeoutMS = "chrome", 23000
	savedAccount, err := resources.SaveAccountSettings(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	for _, test := range []struct {
		path   string
		values url.Values
	}{
		{"/lakes/buntzen", lakeSettingsPageValues(settingsPageDefaults(t, f))},
		{"/lakes/buntzen/reset", url.Values{}},
		{"/settings", accountSettingsPageValues(model.DefaultAccountSettings())},
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
	account, err = resources.GetAccountSettings(ctx)
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
		{"invalid number", "release_days_before", "not-a-number", "Release days must be a whole number"},
		{"unsupported preference", "pass_priority_1", "future-pass", "pass preference is not supported"},
		{"duplicate preference", "pass_priority_3", "morning", "each pass preference can only be selected once"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := lakeSettingsPageValues(settingsPageDefaults(t, f))
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
			for _, name := range []string{"timezone", "release_days_before", "all_day_pass_url"} {
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
	settings := model.DefaultAccountSettings()
	settings.BrowserChannel, settings.DefaultTimeoutMS = "chrome", 18000
	saved, err := resources.SaveAccountSettings(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	for _, test := range []struct{ name, value, message string }{
		{"default_timeout_ms", "not-a-number", "Action timeout must be a whole number"},
		{"prep_minutes_before", "not-a-number", "Preparation time must be a whole number"},
		{"auth_deadline_minutes_before", "100", "auth deadline must fall within the preparation window"},
		{"poll_deadline_seconds", "1.5", "Availability check window must be a whole number"},
		{"poll_min_seconds", "not-a-number", "Minimum retry delay must be a number"},
		{"poll_max_seconds", "not-a-number", "Maximum retry delay must be a number"},
		{"poll_min_seconds", "NaN", "poll timing must fit the worker bounds"},
		{"poll_max_seconds", "+Inf", "poll timing must fit the worker bounds"},
		{"poll_max_seconds", "0.1", "poll timing must fit the worker bounds"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			values := accountSettingsPageValues(settings)
			values.Set("csrf_token", csrfFrom(cookies))
			values.Set("browser_channel", "chrome-beta")
			values.Set(test.name, test.value)
			response := serveForm(f, http.MethodPost, "/settings", cookies, values)
			body := response.Body.String()
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(body, test.message) || !strings.Contains(body, `value="chrome-beta" selected`) {
				t.Fatalf("invalid global defaults lost form input: %d %s", response.Code, body)
			}
			for name, submitted := range values {
				if name == "csrf_token" || name == "browser_channel" || name == "headless" {
					continue
				}
				if !strings.Contains(html.UnescapeString(body), `name="`+name+`" value="`+submitted[0]+`"`) {
					t.Errorf("invalid global defaults lost raw numeric input %s=%s", name, submitted[0])
				}
			}
			retained, err := resources.GetAccountSettings(ctx)
			if err != nil || !reflect.DeepEqual(retained, saved) {
				t.Fatalf("invalid global defaults changed saved settings: %+v %v", retained, err)
			}
		})
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
