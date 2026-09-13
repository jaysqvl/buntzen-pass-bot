package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/egress"
	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestSavedProviderCannotReachUnapprovedDestination(t *testing.T) {
	fixture := newWebFixture(t)
	var requests atomic.Int32
	unapproved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"status":200,"data":"pong"}`))
	}))
	defer unapproved.Close()
	source, err := fixture.store.ForUser(fixture.admin.ID).CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "unapproved", Provider: model.OTPProviderBlueBubbles, Identity: unapproved.URL,
		ProviderConfig: bluebubbles.Config{BaseURL: unapproved.URL, Password: "synthetic-credential-canary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	response := serveForm(fixture, http.MethodPost, fmt.Sprintf("/sources/%d/health", source.ID), cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
	if requests.Load() != 0 {
		t.Fatal("saved provider sent credential-bearing request to unapproved destination")
	}
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/sources?notice=provider-unavailable" {
		t.Fatal("destination denial did not return safe provider notice")
	}
}

func approveTestBlueBubbles(t *testing.T, fixture *webFixture, endpoints ...string) {
	t.Helper()
	rules := make([]egress.Rule, 0, len(endpoints))
	for _, endpoint := range endpoints {
		rules = append(rules, egress.Rule{Origin: endpoint, Networks: []string{"127.0.0.1/32"}})
	}
	policy, err := egress.NewPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cfg.BlueBubblesPolicy = policy
	fixture.server.config.BlueBubblesPolicy = policy
}
