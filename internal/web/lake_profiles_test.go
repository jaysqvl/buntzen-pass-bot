package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestLakeProfileCreationAndEditingStayWithinTheirLake(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Global OTP source", Provider: model.OTPProviderTwilio, Identity: "twilio:lake-profile",
		ProviderConfig: map[string]string{"auth_token": "synthetic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	form := url.Values{
		"csrf_token": {csrfFrom(cookies)}, "lake_id": {"buntzen"}, "name": {"Lake vehicle"},
		"default_vehicle": {"Saved car"}, "otp_source_id": {strconv.FormatInt(source.ID, 10)},
		"login_probe_url": {"https://example.test/buntzen-lake"}, "default_timeout_ms": {"25000"},
		"browser_channel": {"chrome"}, "yodel_phone": {"5559876543"}, "enabled": {"1"},
	}
	request := func(method, path string, values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		r := authenticatedRequest(method, "http://example.test"+path, cookies, values)
		if method == http.MethodPost {
			r.Header.Set("Origin", "http://example.test")
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	page := request(http.MethodGet, "/profiles/new?lake_id=buntzen", nil)
	for _, text := range []string{
		"New Buntzen Lake profile", "Buntzen Lake vehicle", `name="lake_id" value="buntzen"`,
		`href="/lakes/buntzen#profiles"`, "Global OTP source",
	} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), text) {
			t.Fatalf("new lake profile missing %q: %d %s", text, page.Code, page.Body.String())
		}
	}
	created := request(http.MethodPost, "/profiles/new", form)
	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != "/lakes/buntzen?ok=created#profiles" {
		t.Fatalf("create redirect = %d %q %s", created.Code, created.Header().Get("Location"), created.Body.String())
	}
	profiles, err := resources.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].LakeID != "buntzen" || profiles[0].OTPSourceID != source.ID {
		t.Fatalf("saved lake profile = %+v, %v", profiles, err)
	}
	profile := profiles[0]
	path := fmt.Sprintf("/profiles/%d", profile.ID)
	form.Set("lake_id", "unknown-lake")
	form.Set("name", "Changed by invalid submission")
	form.Del("yodel_phone")
	rejected := request(http.MethodPost, path, form)
	if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), "Edit Buntzen Lake profile") || !strings.Contains(rejected.Body.String(), `name="lake_id" value="buntzen"`) {
		t.Fatalf("lake change did not preserve saved lake form: %d %s", rejected.Code, rejected.Body.String())
	}
	retained, err := resources.GetProfile(ctx, profile.ID)
	if err != nil || retained.LakeID != "buntzen" || retained.Name != profile.Name {
		t.Fatalf("invalid lake edit changed profile: %+v %v", retained, err)
	}
	// Existing clients without a lake field continue editing the saved lake.
	form.Del("lake_id")
	form.Set("name", "Renamed lake vehicle")
	updated := request(http.MethodPost, path, form)
	if updated.Code != http.StatusSeeOther || updated.Header().Get("Location") != "/lakes/buntzen?ok=updated#profiles" {
		t.Fatalf("legacy update = %d %s", updated.Code, updated.Body.String())
	}
	credentials, err := f.store.SystemGetProfileCredentials(ctx, profile.ID)
	if err != nil || credentials.Phone != "5559876543" {
		t.Fatalf("editing lake profile lost saved credentials: %+v %v", credentials, err)
	}
	card := profileCard(profile, source.Name)
	if !strings.Contains(card.Subtitle, "Buntzen Lake") || card.Actions[len(card.Actions)-1].URL != fmt.Sprintf("/bookings/new?lake_id=buntzen&profile_id=%d", profile.ID) {
		t.Fatalf("profile card loses lake context: %+v", card)
	}
	index := request(http.MethodGet, "/profiles", nil)
	if index.Code != http.StatusSeeOther || index.Header().Get("Location") != "/lakes" {
		t.Fatalf("legacy index = %d %q", index.Code, index.Header().Get("Location"))
	}
	unknown := request(http.MethodGet, "/profiles/new?lake_id=unknown-lake", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown lake create form = %d", unknown.Code)
	}
}

func TestNewLakeProfilesUsePersonalBrowserDefaultsAndKeepExistingOverrides(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{
		Name: "Browser defaults source", Provider: model.OTPProviderTwilio, Identity: "twilio:browser-defaults",
		ProviderConfig: map[string]string{"auth_token": "synthetic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{
		LakeID: "buntzen", Name: "Existing snapshot", DefaultVehicle: "Saved car", LoginProbeURL: "https://example.test/buntzen-lake",
		OTPSourceID: source.ID, Headless: true, BrowserChannel: "chrome-beta", DefaultTimeoutMS: 18000,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resources.SaveAccountSettings(ctx, model.AccountSettings{Headless: false, BrowserChannel: "chrome", DefaultTimeoutMS: 29000}); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	for _, test := range []struct {
		path, channel, timeout string
		headless               bool
	}{
		{path: "/profiles/new?lake_id=buntzen", channel: "chrome", timeout: "29000"},
		{path: fmt.Sprintf("/profiles/%d", profile.ID), channel: "chrome-beta", timeout: "18000", headless: true},
	} {
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, authenticatedRequest(http.MethodGet, "http://example.test"+test.path, cookies, nil))
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, `value="`+test.channel+`" selected`) || !strings.Contains(body, `name="default_timeout_ms" value="`+test.timeout+`"`) {
			t.Fatalf("browser defaults or saved snapshot changed: %d %s", w.Code, body)
		}
		headlessStart := strings.Index(body, `name="headless"`)
		if headlessStart < 0 {
			t.Fatal("headless checkbox missing")
		}
		headlessEnd := strings.Index(body[headlessStart:], ">")
		if headlessEnd < 0 || strings.Contains(body[headlessStart:headlessStart+headlessEnd], "checked") != test.headless {
			t.Fatalf("headless state does not match snapshot for %s", test.path)
		}
		if !strings.Contains(body, "Browser options are copied from Settings") || strings.Contains(body, "Booking site URLs and preparation timing") {
			t.Fatalf("browser overrides have misleading booking help: %s", body)
		}
	}
}

func TestLakeProfileSourceChoicesOnlyOfferAvailableOwnedSources(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	cookies := loginCookies(t, f)
	page := serveForm(f, http.MethodGet, "/profiles/new?lake_id=buntzen", cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Add an OTP source before creating a lake profile") || !strings.Contains(page.Body.String(), `href="/sources/new"`) {
		t.Fatalf("no-source setup lacks a useful next step: %d %s", page.Code, page.Body.String())
	}
	current, _ := createImmediateWebBooking(t, f, f.admin.ID, "Current linked", true)
	other, _ := createImmediateWebBooking(t, f, f.admin.ID, "Other linked", true)
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "source-choice-member", Password: "other source choice password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := createImmediateWebBooking(t, f, member.ID, "Private source", true)
	page = serveForm(f, http.MethodGet, "/profiles/new?lake_id=buntzen", cookies, nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "All your OTP sources are already linked to profiles") || !strings.Contains(body, "No available OTP sources") || !strings.Contains(body, `href="/sources/new"`) {
		t.Fatalf("assigned sources still offered without a next step: %d %s", page.Code, body)
	}
	for _, name := range []string{"Current linked inbox", "Other linked inbox", "Private source inbox"} {
		if strings.Contains(body, name) {
			t.Fatalf("new profile offered assigned or foreign source %q", name)
		}
	}
	page = serveForm(f, http.MethodGet, fmt.Sprintf("/profiles/%d", current.ID), cookies, nil)
	body = page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`value="%d" selected`, current.OTPSourceID)) || strings.Contains(body, "Other linked inbox") || strings.Contains(body, "Private source inbox") || strings.Contains(body, "No available OTP sources") {
		t.Fatalf("editing did not retain only its current source: %d %s", page.Code, body)
	}
	resources := f.store.ForUser(f.admin.ID)
	var available []model.OTPSource
	for _, name := range []string{"Available first", "Available second"} {
		source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: name, Provider: model.OTPProviderTwilio, Identity: "twilio:" + name, ProviderConfig: map[string]string{"auth_token": "synthetic"}})
		if err != nil {
			t.Fatal(err)
		}
		available = append(available, source)
	}
	selectedID := strconv.FormatInt(available[1].ID, 10)
	page = serveForm(f, http.MethodGet, "/profiles/new?lake_id=buntzen&source_id="+selectedID, cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `value="`+selectedID+`" selected`) || !strings.Contains(page.Body.String(), available[0].Name) || !strings.Contains(page.Body.String(), available[1].Name) {
		t.Fatalf("available source selection lost: %d %s", page.Code, page.Body.String())
	}
	for _, unavailable := range []int64{other.OTPSourceID, foreign.OTPSourceID} {
		page = serveForm(f, http.MethodGet, fmt.Sprintf("/profiles/new?lake_id=buntzen&source_id=%d", unavailable), cookies, nil)
		if page.Code != http.StatusOK || strings.Contains(page.Body.String(), fmt.Sprintf(`<option value="%d"`, unavailable)) {
			t.Fatalf("query offered unavailable or foreign source %d", unavailable)
		}
	}
	values := url.Values{
		"csrf_token": {csrfFrom(cookies)}, "lake_id": {"buntzen"}, "name": {"Draft profile"},
		"default_vehicle": {"Draft vehicle"}, "otp_source_id": {selectedID}, "yodel_phone": {"5559876543"},
		"login_probe_url": {"https://example.test/login"}, "default_timeout_ms": {"not-a-number"},
	}
	page = serveForm(f, http.MethodPost, "/profiles/new", cookies, values)
	body = page.Body.String()
	if page.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `value="`+selectedID+`" selected`) || !strings.Contains(body, `name="name" value="Draft profile"`) || !strings.Contains(body, `name="default_timeout_ms" value="not-a-number"`) || !strings.Contains(body, `min="1000" max="120000"`) {
		t.Fatalf("invalid form lost available selection or browser limits: %d %s", page.Code, body)
	}
	// The store remains authoritative when an old page posts a source that is
	// already assigned. The corrected form must not keep offering that source.
	values.Set("default_timeout_ms", "15000")
	values.Set("otp_source_id", strconv.FormatInt(other.OTPSourceID, 10))
	page = serveForm(f, http.MethodPost, "/profiles/new", cookies, values)
	body = page.Body.String()
	if page.Code != http.StatusUnprocessableEntity || strings.Contains(body, fmt.Sprintf(`<option value="%d"`, other.OTPSourceID)) || !strings.Contains(body, "The selected OTP source is unavailable") {
		t.Fatalf("assigned source submission bypassed exclusivity or remained offered: %d %s", page.Code, body)
	}
	profiles, err := resources.ListProfiles(ctx)
	if err != nil || len(profiles) != 2 {
		t.Fatalf("rejected source submission created a profile: %+v %v", profiles, err)
	}
}
