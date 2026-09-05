package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
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
	sourceSection, profileSection := strings.Index(body, `id="otp-sources"`), strings.Index(body, `id="profiles"`)
	if sourceSection < 0 || profileSection <= sourceSection {
		t.Fatal("home does not show OTP sources before profiles")
	}
	for _, text := range []string{"Owned inbox", "Owned vehicle", "Linked OTP source", "3. Pair with Yodel", "4. Booking request", "Automatic schedules are off", `href="/bookings/new?profile_id=1"`} {
		if !strings.Contains(body, text) {
			t.Fatalf("home missing %q", text)
		}
	}
	for _, text := range []string{"Private inbox", "Private vehicle", "secret-never-rendered", "5559876543"} {
		if strings.Contains(body, text) {
			t.Fatalf("home exposed %q", text)
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
