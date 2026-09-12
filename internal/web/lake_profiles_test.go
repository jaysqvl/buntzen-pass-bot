package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func createDefaultSignInSource(t *testing.T, f webFixture, userID int64, name string) model.OTPSource {
	t.Helper()
	resources := f.store.ForUser(userID)
	source, err := resources.CreateOTPSource(context.Background(), store.OTPSourceInput{Name: name, Provider: model.OTPProviderTwilio, Identity: "twilio:" + name, ProviderConfig: map[string]string{"auth_token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetDefaultOTPSource(context.Background(), source.ID); err != nil {
		t.Fatal(err)
	}
	return source
}

func assertGlobalSignInFields(t *testing.T, body string) {
	t.Helper()
	for _, name := range []string{"name", "yodel_phone", "enabled"} {
		if !strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("sign-in form missing %s", name)
		}
	}
	for _, name := range []string{"lake_id", "provider_id", "default_vehicle", "otp_source_id", "login_probe_url", "headless", "browser_channel", "browser_executable", "default_timeout_ms"} {
		if strings.Contains(body, `name="`+name+`"`) {
			t.Errorf("sign-in form exposes non-sign-in field %s", name)
		}
	}
}

func TestGlobalYodelSignInUsesAccountDefaultsAndRetainsExistingSnapshot(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	source := createDefaultSignInSource(t, f, f.admin.ID, "Default sign-in inbox")
	settings := model.DefaultAccountSettings()
	settings.Headless, settings.BrowserChannel, settings.DefaultTimeoutMS = false, "chrome", 29000
	if _, err := resources.SaveAccountSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	page := serveForm(f, http.MethodGet, "/profiles/new?lake_id=irrelevant&source_id=99999", cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Add Yodel sign-in") || !strings.Contains(page.Body.String(), source.Name) || !strings.Contains(page.Body.String(), `href="/sources"`) {
		t.Fatalf("global sign-in form=%d %s", page.Code, page.Body.String())
	}
	assertGlobalSignInFields(t, page.Body.String())
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {"My Yodel account"}, "yodel_phone": {"5559876543"}, "enabled": {"1"}, "default_vehicle": {"Ignored vehicle"}, "otp_source_id": {"99999"}, "login_probe_url": {"https://unapproved.example/login"}, "browser_channel": {"chrome-beta"}, "default_timeout_ms": {"5000"}, "lake_id": {"ignored-lake"}}
	created := serveForm(f, http.MethodPost, "/profiles/new", cookies, form)
	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != "/?ok=created#yodel-sign-in" {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	profiles, err := resources.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles=%+v %v", profiles, err)
	}
	profile := profiles[0]
	lake, _ := destinations.Resolve(destinations.DefaultLakeID)
	if profile.EffectiveProviderID() != "yodel" || profile.OTPSourceID != source.ID || profile.BrowserChannel != "chrome" || profile.Headless || profile.DefaultTimeoutMS != 29000 || profile.DefaultVehicle != "" || profile.LoginProbeURL != lake.WithOrigin(f.cfg.YodelOrigins[0]).LoginURL {
		t.Fatalf("new sign-in did not inherit trusted defaults: %+v", profile)
	}
	otherDefault := createDefaultSignInSource(t, f, f.admin.ID, "New default inbox")
	settings.Headless, settings.BrowserChannel, settings.DefaultTimeoutMS = true, "chrome-beta", 17000
	if _, err := resources.SaveAccountSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	form.Set("name", "Renamed Yodel account")
	form.Del("yodel_phone")
	edited := serveForm(f, http.MethodPost, fmt.Sprintf("/profiles/%d", profile.ID), cookies, form)
	if edited.Code != http.StatusSeeOther || edited.Header().Get("Location") != "/?ok=updated#yodel-sign-in" {
		t.Fatalf("edit=%d %s", edited.Code, edited.Body.String())
	}
	retained, err := resources.GetProfile(ctx, profile.ID)
	if err != nil || retained.Name != "Renamed Yodel account" || retained.OTPSourceID != source.ID || retained.BrowserChannel != profile.BrowserChannel || retained.Headless != profile.Headless || retained.DefaultTimeoutMS != profile.DefaultTimeoutMS || retained.LoginProbeURL != profile.LoginProbeURL || retained.DefaultVehicle != profile.DefaultVehicle {
		t.Fatalf("edit changed saved private settings: %+v %v", retained, err)
	}
	credentials, err := f.store.SystemGetProfileCredentials(ctx, profile.ID)
	if err != nil || credentials.Phone != "5559876543" {
		t.Fatalf("edit lost saved credentials: %v", err)
	}
	page = serveForm(f, http.MethodGet, fmt.Sprintf("/profiles/%d", profile.ID), cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), otherDefault.Name) || strings.Contains(page.Body.String(), credentials.Phone) {
		t.Fatalf("edit default summary or credential privacy failed: %d %s", page.Code, page.Body.String())
	}
	assertGlobalSignInFields(t, page.Body.String())
	// Two existing identities may intentionally share the selected inbox.
	form.Set("name", "Second Yodel account")
	form.Set("yodel_phone", "5559876543")
	created = serveForm(f, http.MethodPost, "/profiles/new", cookies, form)
	if created.Code != http.StatusSeeOther {
		t.Fatalf("shared source rejected second identity: %d %s", created.Code, created.Body.String())
	}
	form.Set("name", "Third Yodel account")
	created = serveForm(f, http.MethodPost, "/profiles/new", cookies, form)
	profiles, err = resources.ListProfiles(ctx)
	if created.Code != http.StatusSeeOther || err != nil || len(profiles) != 3 {
		t.Fatalf("shared source identities lost: %d %+v %v", created.Code, profiles, err)
	}
	index := serveForm(f, http.MethodGet, "/profiles", cookies, nil)
	if index.Code != http.StatusSeeOther || index.Header().Get("Location") != "/#yodel-sign-in" {
		t.Fatalf("legacy profile index=%d %q", index.Code, index.Header().Get("Location"))
	}
}

func TestYodelSignInRequiresDefaultSourceAndPreservesInvalidDraft(t *testing.T) {
	f := newWebFixture(t)
	cookies := loginCookies(t, f)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {"Draft Yodel account"}, "yodel_phone": {"5559876543"}, "enabled": {"1"}}
	response := serveForm(f, http.MethodPost, "/profiles/new", cookies, form)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "choose a default OTP source") || !strings.Contains(response.Body.String(), `href="/sources"`) || !strings.Contains(response.Body.String(), `name="name" value="Draft Yodel account"`) {
		t.Fatalf("missing OTP default handling=%d %s", response.Code, response.Body.String())
	}
	createDefaultSignInSource(t, f, f.admin.ID, "Draft default source")
	form.Set("yodel_phone", "not-a-phone")
	response = serveForm(f, http.MethodPost, "/profiles/new", cookies, form)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `name="name" value="Draft Yodel account"`) || strings.Contains(response.Body.String(), "not-a-phone") {
		t.Fatalf("invalid sign-in draft handling=%d %s", response.Code, response.Body.String())
	}
	assertGlobalSignInFields(t, response.Body.String())
	profiles, err := f.store.ForUser(f.admin.ID).ListProfiles(context.Background())
	if err != nil || len(profiles) != 0 {
		t.Fatalf("invalid sign-in persisted: %+v %v", profiles, err)
	}
}

func TestHomeYodelSignInQueuesOwnedProfileOnlyAuthWithDefaultSource(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	first := createDefaultSignInSource(t, f, f.admin.ID, "Original sign-in source")
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{ProviderID: "yodel", Name: "Ready Yodel", LoginProbeURL: "https://example.test/login", OTPSourceID: first.ID, Headless: true, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
	if err != nil {
		t.Fatal(err)
	}
	selected := createDefaultSignInSource(t, f, f.admin.ID, "Current sign-in source")
	cookies := loginCookies(t, f)
	path := fmt.Sprintf("/profiles/%d/sign-in", profile.ID)
	denied := serveForm(f, http.MethodPost, path, cookies, url.Values{})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("sign-in accepted missing CSRF: %d", denied.Code)
	}
	response := serveForm(f, http.MethodPost, path, cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/jobs/") {
		t.Fatalf("sign-in queue=%d %s", response.Code, response.Body.String())
	}
	jobs, err := resources.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("sign-in jobs=%+v %v", jobs, err)
	}
	job := jobs[0]
	if job.ProfileID != profile.ID || job.OTPSourceID != selected.ID || job.BookingRequestID != nil || job.Command != model.CommandAuthCheck || job.RunMode != model.RunModeManual {
		t.Fatalf("sign-in job became booking or used wrong source: %+v", job)
	}
	response = serveForm(f, http.MethodPost, path, cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Check Jobs before trying again") {
		t.Fatalf("duplicate sign-in=%d %s", response.Code, response.Body.String())
	}
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "foreign-sign-in", Password: "foreign sign in password"})
	if err != nil {
		t.Fatal(err)
	}
	foreignCookies := loginCookiesAs(t, f, member.Username, "foreign sign in password")
	for _, target := range []string{fmt.Sprintf("/profiles/%d", profile.ID), path} {
		response = serveForm(f, http.MethodPost, target, foreignCookies, url.Values{"csrf_token": {csrfFrom(foreignCookies)}, "name": {"Cannot edit"}})
		if response.Code != http.StatusNotFound {
			t.Fatalf("foreign profile access %s=%d", target, response.Code)
		}
	}
	jobs, err = resources.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("rejected calls queued more jobs: %+v %v", jobs, err)
	}
}
