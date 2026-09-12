package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func assertBookingPassChoices(t *testing.T, body string, choices []string) {
	t.Helper()
	for i, choice := range choices {
		field := fmt.Sprintf("pass_priority_%d", i+1)
		selectMarkup := regexp.MustCompile(`<select name="` + field + `"[^>]*>(.*?)</select>`).FindStringSubmatch(body)
		if len(selectMarkup) != 2 || !strings.Contains(selectMarkup[1], `value="`+choice+`" selected`) {
			t.Fatalf("%s did not retain selection %q: %v", field, choice, selectMarkup)
		}
	}
}

func TestBookingPassPriorityCreateEditAndValidation(t *testing.T) {
	fixture := newWebFixture(t)
	profile, _ := createImmediateWebBooking(t, fixture, fixture.admin.ID, "priority owner", true)
	cookies := loginCookies(t, fixture)
	form := url.Values{
		"csrf_token": {csrfFrom(cookies)}, "name": {"Custom priority"}, "profile_id": {stringID(profile.ID)},
		"enabled": {"1"}, "target_date": {"2030-01-15"}, "timezone": {"UTC"}, "release_time": {"07:00"},
		"confirmation_mode": {"manual"}, "all_day_pass_url": {"https://example.test/all"}, "half_day_pass_url": {"https://example.test/half"},
		"prep_minutes_before": {"30"}, "auth_deadline_minutes_before": {"5"}, "poll_deadline_seconds": {"120"},
		"poll_min_seconds": {"1"}, "poll_max_seconds": {"2"},
		"pass_priority_1": {"morning"}, "pass_priority_2": {""}, "pass_priority_3": {"all_day"},
	}
	response := serveForm(fixture, http.MethodPost, "/bookings/new", cookies, form)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("create custom order=%d body=%s", response.Code, response.Body.String())
	}
	bookings, err := fixture.store.ForUser(fixture.admin.ID).ListBookingRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var saved model.BookingRequest
	for _, booking := range bookings {
		if booking.Name == "Custom priority" {
			saved = booking
		}
	}
	if saved.ID == 0 || !slices.Equal(saved.PassOrder(), []model.PassType{model.PassMorning, model.PassAllDay}) {
		t.Fatalf("None gap lost custom priority: %+v", saved)
	}
	path := fmt.Sprintf("/bookings/%d", saved.ID)
	response = serveForm(fixture, http.MethodGet, path, cookies, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("edit form=%d", response.Code)
	}
	assertBookingPassChoices(t, response.Body.String(), []string{"morning", "all_day", ""})
	form.Set("pass_priority_1", "afternoon")
	form.Set("pass_priority_2", "morning")
	form.Set("pass_priority_3", "")
	response = serveForm(fixture, http.MethodPost, path, cookies, form)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("update custom order=%d body=%s", response.Code, response.Body.String())
	}
	saved, err = fixture.store.ForUser(fixture.admin.ID).GetBookingRequest(context.Background(), saved.ID)
	if err != nil || !slices.Equal(saved.PassOrder(), []model.PassType{model.PassAfternoon, model.PassMorning}) {
		t.Fatalf("updated priority=%+v err=%v", saved, err)
	}

	for _, test := range []struct {
		name    string
		choices []string
		prep    string
		message string
	}{
		{"duplicate choices", []string{"morning", "", "morning"}, "30", "only be selected once"},
		{"no choices", []string{"", "", ""}, "30", "at least one pass preference"},
		{"earlier numeric error", []string{"morning", "", "all_day"}, "invalid", "prep minutes must be a number"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for i, choice := range test.choices {
				form.Set(fmt.Sprintf("pass_priority_%d", i+1), choice)
			}
			form.Set("prep_minutes_before", test.prep)
			response := serveForm(fixture, http.MethodPost, path, cookies, form)
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), test.message) {
				t.Fatalf("validation response=%d body=%s", response.Code, response.Body.String())
			}
			assertBookingPassChoices(t, response.Body.String(), test.choices)
			if !strings.Contains(response.Body.String(), `name="name" value="Custom priority"`) {
				t.Fatal("validation failure discarded another submitted value")
			}
			unchanged, err := fixture.store.ForUser(fixture.admin.ID).GetBookingRequest(context.Background(), saved.ID)
			if err != nil || !slices.Equal(unchanged.PassOrder(), saved.PassOrder()) {
				t.Fatalf("invalid update changed stored priority: %+v err=%v", unchanged, err)
			}
		})
	}
}
