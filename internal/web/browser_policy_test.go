package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestMemberCannotCreateOrUpdateExecutableOverride(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "browser-member", Password: "long member password"})
	if err != nil {
		t.Fatal(err)
	}
	resources := f.store.ForUser(member.ID)
	source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: "inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:browser-member", ProviderConfig: map[string]string{"auth_token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetDefaultOTPSource(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	settings := model.DefaultAccountSettings()
	settings.BrowserChannel = "chrome"
	if _, err := resources.SaveAccountSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookiesAs(t, f, member.Username, "long member password")
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {"Browser"}, "default_vehicle": {"Car"}, "otp_source_id": {strconv.FormatInt(source.ID, 10)}, "login_probe_url": {"https://example.test/login"}, "default_timeout_ms": {"1000"}, "yodel_phone": {"5559876543"}, "browser_channel": {"chrome"}}
	post := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		r := authenticatedRequest(http.MethodPost, "http://example.test"+target, cookies, form)
		r.Header.Set("Origin", "http://example.test")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	form.Set("browser_executable", "/tmp/member-program")
	if w := post("/profiles/new"); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "operator-controlled") {
		t.Fatalf("forged create = %d %s", w.Code, w.Body.String())
	}
	form.Del("browser_executable")
	if w := post("/profiles/new"); w.Code != http.StatusSeeOther {
		t.Fatalf("legitimate create = %d %s", w.Code, w.Body.String())
	}
	profiles, err := resources.ListProfiles(ctx)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles = %v, %v", profiles, err)
	}
	target := fmt.Sprintf("/profiles/%d", profiles[0].ID)
	form.Set("browser_executable", "../program")
	if w := post(target); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "operator-controlled") {
		t.Fatalf("forged update = %d %s", w.Code, w.Body.String())
	}
	profile, err := resources.GetProfile(ctx, profiles[0].ID)
	if err != nil || profile.BrowserExecutable != "" || profile.BrowserChannel != "chrome" {
		t.Fatalf("rejected update changed profile: %+v %v", profile, err)
	}
}

func TestEditingLegacySignInClearsRetiredExecutableAndKeepsOtherSettings(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	member, err := f.store.CreateMember(ctx, store.CreateUserInput{Username: "legacy-browser-member", Password: "legacy browser member password"})
	if err != nil {
		t.Fatal(err)
	}
	resources := f.store.ForUser(member.ID)
	source := createDefaultSignInSource(t, f, member.ID, "Legacy browser inbox")
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{
		ProviderID: "yodel", Name: "Legacy Yodel", DefaultVehicle: "Legacy vehicle",
		LoginProbeURL: "https://example.test/saved-login", OTPSourceID: source.ID,
		BrowserChannel: "chrome-beta", Headless: false, DefaultTimeoutMS: 27000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Represent a migrated record from when per-profile executable paths were
	// accepted. Current writes reject these paths before reaching storage.
	database, err := sql.Open("sqlite", filepath.Join(f.cfg.AppDataDir, "buntzen.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, `UPDATE profiles SET browser_executable = ? WHERE id = ? AND user_id = ?`, "/legacy/browser", profile.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookiesAs(t, f, member.Username, "legacy browser member password")
	path := fmt.Sprintf("/profiles/%d", profile.ID)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {"Updated Yodel"}, "enabled": {"1"}, "browser_executable": {"/injected/browser"}}
	rejected := serveForm(f, http.MethodPost, path, cookies, form)
	if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), "operator-controlled") {
		t.Fatalf("legacy edit accepted incoming executable: %d %s", rejected.Code, rejected.Body.String())
	}
	unchanged, err := resources.GetProfile(ctx, profile.ID)
	if err != nil || unchanged.BrowserExecutable != "/legacy/browser" || unchanged.Name != profile.Name {
		t.Fatalf("rejected edit changed legacy state: %+v %v", unchanged, err)
	}
	form.Del("browser_executable")
	updated := serveForm(f, http.MethodPost, path, cookies, form)
	if updated.Code != http.StatusSeeOther || updated.Header().Get("Location") != "/?ok=updated#yodel-sign-in" {
		t.Fatalf("normal sign-in edit failed to clear retired override: %d %s", updated.Code, updated.Body.String())
	}
	retained, err := resources.GetProfile(ctx, profile.ID)
	if err != nil || retained.BrowserExecutable != "" || retained.Name != "Updated Yodel" ||
		retained.BrowserChannel != profile.BrowserChannel || retained.Headless != profile.Headless || retained.DefaultTimeoutMS != profile.DefaultTimeoutMS ||
		retained.OTPSourceID != source.ID || retained.LoginProbeURL != profile.LoginProbeURL || retained.DefaultVehicle != profile.DefaultVehicle || retained.ProviderID != profile.ProviderID {
		t.Fatalf("legacy edit did not preserve supported saved settings: %+v %v", retained, err)
	}
	if err := retained.ValidateForOrigins(f.cfg.YodelOrigins); err != nil {
		t.Fatalf("legacy sign-in remains invalid after save: %v", err)
	}
	credentials, err := f.store.SystemGetProfileCredentials(ctx, profile.ID)
	if err != nil || credentials.Phone != "5559876543" {
		t.Fatalf("clearing legacy override lost saved credentials: %v", err)
	}
}

func TestEditingLegacySignInRepairsMissingLoginURLWhenReenteringPhone(t *testing.T) {
	f := newWebFixture(t)
	ctx := context.Background()
	resources := f.store.ForUser(f.admin.ID)
	source := createDefaultSignInSource(t, f, f.admin.ID, "Unpaired legacy inbox")
	profile, err := resources.CreateProfile(ctx, store.ProfileInput{
		ProviderID: "yodel", Name: "Old sign-in without bookings", LoginProbeURL: "https://example.test/login",
		OTPSourceID: source.ID, BrowserChannel: "chrome", Headless: true, DefaultTimeoutMS: 19000,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Migration 0005 could retain a sign-in without a mobile credential or
	// login URL when it had no booking to supply the old login page.
	database, err := sql.Open("sqlite", filepath.Join(f.cfg.AppDataDir, "buntzen.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.ExecContext(ctx, `UPDATE profiles SET login_probe_url = '', yodel_phone_ciphertext = '' WHERE id = ?`, profile.ID); err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, f)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}, "name": {profile.Name}, "yodel_phone": {"5559876543"}, "enabled": {"1"}}
	response := serveForm(f, http.MethodPost, fmt.Sprintf("/profiles/%d", profile.ID), cookies, form)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("re-entering phone could not repair blank legacy login URL: %d %s", response.Code, response.Body.String())
	}
	repaired, err := resources.GetProfile(ctx, profile.ID)
	lake, _ := destinations.Resolve(destinations.DefaultLakeID)
	if err != nil || repaired.LoginProbeURL != lake.WithOrigin(f.cfg.YodelOrigins[0]).LoginURL || !repaired.Enabled || repaired.BrowserChannel != profile.BrowserChannel || repaired.DefaultTimeoutMS != profile.DefaultTimeoutMS || repaired.OTPSourceID != source.ID {
		t.Fatalf("legacy URL repair changed supported configuration: %+v %v", repaired, err)
	}
	if err := repaired.ValidateForOrigins(f.cfg.YodelOrigins); err != nil {
		t.Fatalf("repaired legacy sign-in remains invalid: %v", err)
	}
	credentials, err := f.store.SystemGetProfileCredentials(ctx, profile.ID)
	if err != nil || credentials.Phone != "5559876543" {
		t.Fatalf("legacy repair failed to store the newly entered phone: %v", err)
	}
}
