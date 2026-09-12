package web

import (
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/destinations"
)

func TestLakeSelectorCarriesApprovedCatalogDefaults(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	page := serveForm(fixture, http.MethodGet, "/bookings/new", cookies, nil)
	if page.Code != http.StatusOK {
		t.Fatalf("booking form status = %d", page.Code)
	}
	matches := regexp.MustCompile(`data-lake-defaults="([^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	if len(matches) != len(destinations.List()) {
		t.Fatalf("rendered defaults for %d lakes, want %d", len(matches), len(destinations.List()))
	}
	for _, match := range matches {
		var defaults lakeFormDefaults
		if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &defaults); err != nil {
			t.Fatalf("option data was not valid escaped JSON: %v", err)
		}
		lake, err := destinations.Resolve(defaults.ID)
		if err != nil {
			t.Fatal(err)
		}
		lake = lake.WithOrigin(fixture.server.config.YodelOrigins[0])
		if defaults.AllDayPassURL != lake.AllDayPassURL || defaults.HalfDayPassURL != lake.HalfDayPassURL || defaults.Timezone != lake.Timezone || defaults.ReleaseTime != lake.ReleaseTime || defaults.ReleasePolicy != lakeReleasePolicy(lake) {
			t.Fatalf("lake selector defaults disagree with approved catalog: %+v", defaults)
		}
		if len(defaults.Passes) != len(lake.SupportedPasses) {
			t.Fatalf("pass choices missing from selector defaults: %+v", defaults)
		}
		for index, pass := range defaults.Passes {
			if pass.Value != lake.SupportedPasses[index] || pass.Label != passOptionLabel(pass.Value) {
				t.Fatalf("pass choices changed order or lost labels: %+v", defaults)
			}
		}
	}
}
