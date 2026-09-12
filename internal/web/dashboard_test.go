package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestHomeShowsSetupOrderAndOnlyOwnedLinkedResources(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{Username: "other-member", Password: "other-member-password"})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []struct {
		id   int64
		name string
	}{{fixture.admin.ID, "Owned"}, {member.ID, "Private"}} {
		resources := fixture.store.ForUser(owner.id)
		source, err := resources.CreateOTPSource(ctx, store.OTPSourceInput{Name: owner.name + " inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:" + owner.name, ProviderConfig: map[string]string{"auth_token": "secret-never-rendered"}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = resources.CreateProfile(ctx, store.ProfileInput{Name: owner.name + " vehicle", DefaultVehicle: owner.name + " car", LoginProbeURL: "https://example.test/login", OTPSourceID: source.ID, DefaultTimeoutMS: 15000, Enabled: true, Headless: true, Credentials: &model.ProfileCredentials{Phone: "5559876543"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "http://example.test/", loginCookies(t, fixture), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("home=%d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, text := range []string{"1. Connect an inbox", "2. Set up a lake", "3. Plan your visit", "4. Follow the booking", "Already queued jobs remain scheduled", `href="/lakes"`, `href="/sources"`, `href="/settings"`} {
		if !strings.Contains(body, text) {
			t.Fatalf("home missing %q", text)
		}
	}
	for _, target := range []string{"/sources", "/lakes/buntzen"} {
		response = httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "http://example.test"+target, loginCookies(t, fixture), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s=%d %s", target, response.Code, response.Body.String())
		}
		body = response.Body.String()
		expected := "Owned inbox"
		if target == "/lakes/buntzen" {
			expected = "Owned vehicle"
		}
		if !strings.Contains(body, expected) {
			t.Fatalf("%s missing owned resource", target)
		}
		for _, text := range []string{"Private inbox", "Private vehicle", "secret-never-rendered", "5559876543"} {
			if strings.Contains(body, text) {
				t.Fatalf("%s exposed %q", target, text)
			}
		}
	}
}

func TestProfileFormOwnsLoginURLAndPreselectsOnlyOwnedSource(t *testing.T) {
	fixture := newWebFixture(t)
	ctx := context.Background()
	source, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(ctx, store.OTPSourceInput{Name: "Saved inbox", Provider: model.OTPProviderTwilio, Identity: "twilio:profile-form", ProviderConfig: map[string]string{"auth_token": "synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	for _, requestedID := range []int64{source.ID, source.ID + 100} {
		response := httptest.NewRecorder()
		target := fmt.Sprintf("http://example.test/profiles/new?source_id=%d", requestedID)
		fixture.handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, target, cookies, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("profile form=%d %s", response.Code, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, `name="login_probe_url" value="https://example.test/buntzen-lake"`) {
			t.Fatal("profile missing approved default login URL")
		}
		if requestedID == source.ID && !strings.Contains(body, fmt.Sprintf(`value="%d" selected`, source.ID)) {
			t.Fatal("owned source was not preselected")
		}
		if requestedID != source.ID && strings.Contains(body, fmt.Sprintf(`value="%d"`, requestedID)) {
			t.Fatal("unowned source was rendered")
		}
	}
}
