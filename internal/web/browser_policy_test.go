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

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
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
