package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func publicFormCookie(t *testing.T, fixture webFixture, target string) (*http.Cookie, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://example.test"+target, nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, recorder.Code, recorder.Body.String())
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == loginCSRFCookie {
			return cookie, recorder.Body.String()
		}
	}
	t.Fatalf("GET %s did not set the public form CSRF cookie", target)
	return nil, ""
}

func serveForm(fixture webFixture, method, target string, cookies []*http.Cookie, values url.Values) *httptest.ResponseRecorder {
	request := authenticatedRequest(method, "http://example.test"+target, cookies, values)
	request.Header.Set("Origin", "http://example.test")
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestFirstRunSetupCreatesThePermanentAdministrator(t *testing.T) {
	fixture := newUninitializedWebFixture(t)

	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/setup" {
		t.Fatalf("uninitialized root = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}

	csrfCookie, body := publicFormCookie(t, fixture, "/setup")
	for _, expected := range []string{"Create the administrator", `name="setup_token"`, `name="username"`, `name="password"`, `name="password_confirm"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("setup form missing %q", expected)
		}
	}
	password := "initial-owner-password"
	form := url.Values{
		"csrf_token":       {csrfCookie.Value},
		"setup_token":      {fixture.cfg.SetupToken},
		"username":         {"Owner"},
		"password":         {password},
		"password_confirm": {password},
	}
	recorder = serveForm(fixture, http.MethodPost, "/setup", []*http.Cookie{csrfCookie}, form)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/?ok=setup" {
		t.Fatalf("POST setup = %d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), password) {
		t.Fatal("setup response rendered the submitted password")
	}
	users, err := fixture.store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Username != "Owner" || users[0].Role != model.RoleAdmin || users[0].Status != model.UserActive {
		t.Fatalf("unexpected initial user: %#v", users)
	}

	request = httptest.NewRequest(http.MethodGet, "http://example.test/setup", nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("repeat setup = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestSetupRejectsCrossOriginAndDoesNotEchoPasswords(t *testing.T) {
	fixture := newUninitializedWebFixture(t)
	csrfCookie, _ := publicFormCookie(t, fixture, "/setup")
	password := "a-password-that-must-not-render"
	form := url.Values{
		"csrf_token":       {csrfCookie.Value},
		"setup_token":      {fixture.cfg.SetupToken},
		"username":         {"owner"},
		"password":         {password},
		"password_confirm": {"different-password"},
	}
	recorder := serveForm(fixture, http.MethodPost, "/setup", []*http.Cookie{csrfCookie}, form)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "do not match") {
		t.Fatalf("mismatched setup = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), password) {
		t.Fatal("setup error rendered the submitted password")
	}

	request := authenticatedRequest(http.MethodPost, "http://example.test/setup", []*http.Cookie{csrfCookie}, form)
	request.Header.Set("Origin", "http://evil.example")
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin") {
		t.Fatalf("cross-origin setup = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSetupRequiresHostTokenAndRejectsUntrustedHost(t *testing.T) {
	fixture := newUninitializedWebFixture(t)
	csrfCookie, _ := publicFormCookie(t, fixture, "/setup")
	form := url.Values{
		"csrf_token":       {csrfCookie.Value},
		"setup_token":      {"wrong-setup-token"},
		"username":         {"owner"},
		"password":         {"valid-owner-password"},
		"password_confirm": {"valid-owner-password"},
	}
	recorder := serveForm(fixture, http.MethodPost, "/setup", []*http.Cookie{csrfCookie}, form)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "not accepted") {
		t.Fatalf("wrong setup token = %d: %s", recorder.Code, recorder.Body.String())
	}
	if hasUsers, err := fixture.store.HasUsers(context.Background()); err != nil || hasUsers {
		t.Fatalf("wrong setup token created account: hasUsers=%v err=%v", hasUsers, err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://attacker.example/setup", nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("untrusted Host setup = %d", recorder.Code)
	}
}

func TestAdminCreatesMemberAndMemberChangesTemporaryPassword(t *testing.T) {
	fixture := newWebFixture(t)
	adminCookies := loginCookies(t, fixture)
	temporaryPassword := "temporary-member-password"
	create := url.Values{
		"csrf_token":       {csrfFrom(adminCookies)},
		"username":         {"member-one"},
		"password":         {temporaryPassword},
		"password_confirm": {temporaryPassword},
	}
	recorder := serveForm(fixture, http.MethodPost, "/admin/users/new", adminCookies, create)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/admin/users?ok=user-created" {
		t.Fatalf("create member = %d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	users, err := fixture.store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[1].Role != model.RoleMember || !users[1].MustChangePassword {
		t.Fatalf("unexpected member: %#v", users)
	}

	memberCookies := loginCookiesAs(t, fixture, "MEMBER-ONE", temporaryPassword)
	request := authenticatedRequest(http.MethodGet, "http://example.test/", memberCookies, nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/account?password=required" {
		t.Fatalf("temporary-password redirect = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
	unchanged := url.Values{
		"csrf_token":       {csrfFrom(memberCookies)},
		"current_password": {temporaryPassword},
		"new_password":     {temporaryPassword},
		"password_confirm": {temporaryPassword},
	}
	recorder = serveForm(fixture, http.MethodPost, "/account/password", memberCookies, unchanged)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "differs") {
		t.Fatalf("unchanged temporary password = %d: %s", recorder.Code, recorder.Body.String())
	}

	newPassword := "member-replacement-password"
	change := url.Values{
		"csrf_token":       {csrfFrom(memberCookies)},
		"current_password": {temporaryPassword},
		"new_password":     {newPassword},
		"password_confirm": {newPassword},
	}
	recorder = serveForm(fixture, http.MethodPost, "/account/password", memberCookies, change)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?ok=password-changed" {
		t.Fatalf("change member password = %d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), temporaryPassword) || strings.Contains(recorder.Body.String(), newPassword) {
		t.Fatal("password change response rendered a password")
	}

	memberCookies = loginCookiesAs(t, fixture, "member-one", newPassword)
	request = authenticatedRequest(http.MethodGet, "http://example.test/admin/users", memberCookies, nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("member admin access = %d", recorder.Code)
	}
}

func TestPasswordChangeRejectsConcurrentAndRepeatedArgonWork(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	form := url.Values{
		"csrf_token":       {csrfFrom(cookies)},
		"current_password": {"wrong current password"},
		"new_password":     {"a different valid password"},
		"password_confirm": {"a different valid password"},
	}

	release := fixture.server.tryPasswordChange(fixture.admin.ID)
	if release == nil {
		t.Fatal("could not reserve password-change admission")
	}
	recorder := serveForm(fixture, http.MethodPost, "/account/password", cookies, form)
	release()
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent password change = %d: %s", recorder.Code, recorder.Body.String())
	}

	for attempt := 0; attempt < passwordLimit; attempt++ {
		recorder = serveForm(fixture, http.MethodPost, "/account/password", cookies, form)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("password attempt %d = %d: %s", attempt+1, recorder.Code, recorder.Body.String())
		}
	}
	recorder = serveForm(fixture, http.MethodPost, "/account/password", cookies, form)
	if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "too many password attempts") {
		t.Fatalf("rate-limited password change = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminCannotModifyPermanentAdminAndCanDisableMember(t *testing.T) {
	fixture := newWebFixture(t)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{
		Username: "member-two", Password: "member-two-password", MustChangePassword: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	memberCookies := loginCookiesAs(t, fixture, member.Username, "member-two-password")
	adminCookies := loginCookies(t, fixture)

	adminUpdate := url.Values{"csrf_token": {csrfFrom(adminCookies)}, "username": {fixture.admin.Username}}
	recorder := serveForm(fixture, http.MethodPost, "/admin/users/"+stringID(fixture.admin.ID), adminCookies, adminUpdate)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("admin mutation = %d", recorder.Code)
	}
	adminReset := url.Values{
		"csrf_token":       {csrfFrom(adminCookies)},
		"password":         {"replacement-admin-password"},
		"password_confirm": {"replacement-admin-password"},
	}
	recorder = serveForm(fixture, http.MethodPost, "/admin/users/"+stringID(fixture.admin.ID)+"/password", adminCookies, adminReset)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("admin managed reset = %d", recorder.Code)
	}

	disable := url.Values{"csrf_token": {csrfFrom(adminCookies)}, "username": {member.Username}}
	recorder = serveForm(fixture, http.MethodPost, "/admin/users/"+stringID(member.ID), adminCookies, disable)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("disable member = %d: %s", recorder.Code, recorder.Body.String())
	}
	request := authenticatedRequest(http.MethodGet, "http://example.test/account", memberCookies, nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("disabled member session = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestAdminDeletesDisabledMemberAndReleasesBlueBubblesSlot(t *testing.T) {
	fixture := newWebFixture(t)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{
		Username: "deletable-member", Password: "deletable-member-password", MustChangePassword: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ForUser(member.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Claimed Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://member-messages.example:1234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://member-messages.example:1234", Password: "synthetic-password"},
	}); err != nil {
		t.Fatal(err)
	}
	adminCookies := loginCookies(t, fixture)
	memberCookies := loginCookiesAs(t, fixture, member.Username, "deletable-member-password")
	deletePath := "/admin/users/" + stringID(member.ID) + "/delete"

	memberDelete := url.Values{
		"csrf_token": {csrfFrom(memberCookies)}, "confirm_username": {member.Username},
	}
	recorder := serveForm(fixture, http.MethodPost, deletePath, memberCookies, memberDelete)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("member deletion access = %d", recorder.Code)
	}
	adminDelete := url.Values{
		"csrf_token": {csrfFrom(adminCookies)}, "confirm_username": {member.Username},
	}
	recorder = serveForm(fixture, http.MethodPost, deletePath, adminCookies, adminDelete)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "Disable this member") {
		t.Fatalf("active member deletion = %d: %s", recorder.Code, recorder.Body.String())
	}
	disable := url.Values{"csrf_token": {csrfFrom(adminCookies)}, "username": {member.Username}}
	recorder = serveForm(fixture, http.MethodPost, "/admin/users/"+stringID(member.ID), adminCookies, disable)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("disable member = %d: %s", recorder.Code, recorder.Body.String())
	}
	wrongConfirmation := url.Values{
		"csrf_token": {csrfFrom(adminCookies)}, "confirm_username": {strings.ToUpper(member.Username)},
	}
	recorder = serveForm(fixture, http.MethodPost, deletePath, adminCookies, wrongConfirmation)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "username exactly") {
		t.Fatalf("inexact member deletion = %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = serveForm(fixture, http.MethodPost, deletePath, adminCookies, adminDelete)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/admin/users?ok=user-deleted" {
		t.Fatalf("delete member = %d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if _, err := fixture.store.GetUser(context.Background(), member.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted member lookup error=%v", err)
	}
	if _, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Admin Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://admin-messages.example:1234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://admin-messages.example:1234", Password: "synthetic-password"},
	}); err != nil {
		t.Fatalf("administrator could not reclaim BlueBubbles slot: %v", err)
	}
	adminDeletePath := "/admin/users/" + stringID(fixture.admin.ID) + "/delete"
	adminConfirmation := url.Values{
		"csrf_token": {csrfFrom(adminCookies)}, "confirm_username": {fixture.admin.Username},
	}
	recorder = serveForm(fixture, http.MethodPost, adminDeletePath, adminCookies, adminConfirmation)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("permanent administrator deletion = %d", recorder.Code)
	}
}

func TestAdminCanResetMemberPasswordAndRevokeSessions(t *testing.T) {
	fixture := newWebFixture(t)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{
		Username: "member-three", Password: "member-three-password", MustChangePassword: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	memberCookies := loginCookiesAs(t, fixture, member.Username, "member-three-password")
	adminCookies := loginCookies(t, fixture)
	newPassword := "member-three-replacement"
	reset := url.Values{
		"csrf_token":       {csrfFrom(adminCookies)},
		"password":         {newPassword},
		"password_confirm": {newPassword},
	}
	recorder := serveForm(fixture, http.MethodPost, "/admin/users/"+stringID(member.ID)+"/password", adminCookies, reset)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/admin/users?ok=user-password" {
		t.Fatalf("reset member password = %d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	request := authenticatedRequest(http.MethodGet, "http://example.test/account", memberCookies, nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("reset member session = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}
	user, authenticated, err := fixture.store.AuthenticateUser(context.Background(), member.Username, newPassword)
	if err != nil || !authenticated || !user.MustChangePassword {
		t.Fatalf("authenticate reset member: authenticated=%v user=%#v err=%v", authenticated, user, err)
	}
}

func TestMemberResourcePagesAreOwnerScoped(t *testing.T) {
	fixture := newWebFixture(t)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{
		Username: "isolated-member", Password: "isolated-member-password", MustChangePassword: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	adminSource, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Admin Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://127.0.0.1:3101",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:3101", Password: "admin-source-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.store.ForUser(member.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Member Phone", Provider: model.OTPProviderTwilio, Identity: "twilio:member-phone",
		ProviderConfig: map[string]string{"auth_token": "member-source-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookiesAs(t, fixture, member.Username, "isolated-member-password")

	request := authenticatedRequest(http.MethodGet, "http://example.test/sources", cookies, nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Member Phone") {
		t.Fatalf("member source list = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "Admin Messages") {
		t.Fatal("member source list rendered another user's source")
	}

	request = authenticatedRequest(http.MethodGet, "http://example.test/sources/"+stringID(adminSource.ID), cookies, nil)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-owner source read = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func stringID(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestBlueBubblesIdentityCanonicalizesEquivalentOrigins(t *testing.T) {
	for input, expected := range map[string]string{
		"HTTP://Messages.Example.:80/": "http://messages.example",
		"https://Messages.Example:443": "https://messages.example",
		"http://[::1]:80":              "http://[::1]",
		"http://[::1]:1234/":           "http://[::1]:1234",
	} {
		actual, err := blueBubblesIdentity(input)
		if err != nil || actual != expected {
			t.Fatalf("blueBubblesIdentity(%q)=%q err=%v, want %q", input, actual, err, expected)
		}
	}
}

func TestBlueBubblesServerChangeRequiresPasswordReentry(t *testing.T) {
	fixture := newWebFixture(t)
	resources := fixture.store.ForUser(fixture.admin.ID)
	source, err := resources.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://old.example.test:1234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://old.example.test:1234", Password: "write-only-old-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	form := url.Values{
		"csrf_token":  {csrfFrom(cookies)},
		"name":        {source.Name},
		"provider":    {string(model.OTPProviderBlueBubbles)},
		"bb_base_url": {"http://new.example.test:1234"},
		"bb_password": {""},
	}
	recorder := serveForm(fixture, http.MethodPost, "/sources/"+stringID(source.ID), cookies, form)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank-secret server change = %d: %s", recorder.Code, recorder.Body.String())
	}
	unchanged, err := resources.GetOTPSource(context.Background(), source.ID)
	if err != nil || unchanged.Identity != source.Identity {
		t.Fatalf("source identity after rejected edit = %+v err=%v", unchanged, err)
	}
	var oldConfig bluebubbles.Config
	if err := resources.GetOTPSourceConfig(context.Background(), source.ID, &oldConfig); err != nil {
		t.Fatal(err)
	}
	if oldConfig.BaseURL != source.Identity || oldConfig.Password != "write-only-old-password" {
		t.Fatalf("provider config changed after rejected edit: %+v", oldConfig)
	}

	form.Set("bb_password", "new-server-password")
	recorder = serveForm(fixture, http.MethodPost, "/sources/"+stringID(source.ID), cookies, form)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("reentered-secret server change = %d: %s", recorder.Code, recorder.Body.String())
	}
	changed, err := resources.GetOTPSource(context.Background(), source.ID)
	if err != nil || changed.Identity != "http://new.example.test:1234" {
		t.Fatalf("source identity after approved edit = %+v err=%v", changed, err)
	}
	var newConfig bluebubbles.Config
	if err := resources.GetOTPSourceConfig(context.Background(), source.ID, &newConfig); err != nil {
		t.Fatal(err)
	}
	if newConfig.BaseURL != changed.Identity || newConfig.Password != "new-server-password" {
		t.Fatalf("replacement provider config = %+v", newConfig)
	}
}

func TestBookingFormCannotExpandYodelCredentialOrigin(t *testing.T) {
	fixture := newWebFixture(t)
	resources := fixture.store.ForUser(fixture.admin.ID)
	source, err := resources.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Phone", Provider: model.OTPProviderTwilio, Identity: "twilio:booking-origin-test",
		ProviderConfig: map[string]string{"auth_token": "synthetic-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(context.Background(), store.ProfileInput{
		Name: "Profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	form := url.Values{
		"csrf_token":                   {csrfFrom(cookies)},
		"name":                         {"Unsafe booking"},
		"profile_id":                   {stringID(profile.ID)},
		"target_date":                  {"2030-01-15"},
		"timezone":                     {"UTC"},
		"release_time":                 {"07:00"},
		"confirmation_mode":            {string(model.RunModeManual)},
		"login_probe_url":              {"https://attacker.example/login"},
		"all_day_pass_url":             {"https://attacker.example/pass"},
		"check_all_day":                {"1"},
		"prep_minutes_before":          {"30"},
		"auth_deadline_minutes_before": {"5"},
		"poll_deadline_seconds":        {"120"},
		"poll_min_seconds":             {"1"},
		"poll_max_seconds":             {"2"},
	}
	recorder := serveForm(fixture, http.MethodPost, "/bookings/new", cookies, form)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "approved Yodel origin") {
		t.Fatalf("unsafe booking origin = %d: %s", recorder.Code, recorder.Body.String())
	}
	bookings, err := resources.ListBookingRequests(context.Background())
	if err != nil || len(bookings) != 0 {
		t.Fatalf("unsafe booking was persisted: count=%d err=%v", len(bookings), err)
	}
}

func TestLoginRateLimitHasIndependentHashedIPBucketAndBodyCap(t *testing.T) {
	fixture := newWebFixture(t)
	csrfCookie, _ := publicFormCookie(t, fixture, "/login")
	for attempt := 0; attempt < loginIPLimit; attempt++ {
		values := url.Values{
			"csrf_token": {csrfCookie.Value},
			"username":   {fmt.Sprintf("missing-%d", attempt)},
			"password":   {"not-the-password"},
		}
		recorder := serveForm(fixture, http.MethodPost, "/login", []*http.Cookie{csrfCookie}, values)
		if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=invalid" {
			t.Fatalf("failed login %d = %d location=%q", attempt, recorder.Code, recorder.Header().Get("Location"))
		}
	}
	values := url.Values{
		"csrf_token": {csrfCookie.Value},
		"username":   {fixture.admin.Username},
		"password":   {"long-test-password"},
	}
	recorder := serveForm(fixture, http.MethodPost, "/login", []*http.Cookie{csrfCookie}, values)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login?error=limited" {
		t.Fatalf("IP-limited login = %d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}

	key := loginRateKey("ip-user", "192.0.2.1", "private-user")
	if len(key) != 64 || strings.Contains(key, "192.0.2.1") || strings.Contains(key, "private-user") {
		t.Fatalf("login rate key is not a fixed digest: %q", key)
	}

	oversized := "csrf_token=" + url.QueryEscape(csrfCookie.Value) + "&username=" + strings.Repeat("a", maxFormBody)
	request := httptest.NewRequest(http.MethodPost, "http://example.test/login", strings.NewReader(oversized))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.test")
	request.AddCookie(csrfCookie)
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized login body = %d", recorder.Code)
	}
}
