package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRebrandPreservesLegacySessionsAndLogout(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "public"}[public], func(t *testing.T) {
			fixture := newWebFixture(t)
			if public {
				fixture = publicFixture(t)
			}
			var issued []*http.Cookie
			if public {
				issued = publicLoginCookies(t, fixture)
			} else {
				issued = loginCookies(t, fixture)
			}
			var cookies []*http.Cookie
			var csrf, session string
			for _, cookie := range issued {
				switch cookie.Name {
				case fixture.server.cookieName(sessionCookie):
					session = cookie.Value
					cookies = append(cookies, &http.Cookie{Name: fixture.server.cookieName("buntzen_session"), Value: cookie.Value})
				case fixture.server.cookieName(csrfCookie):
					csrf = cookie.Value
					cookies = append(cookies, &http.Cookie{Name: fixture.server.cookieName("buntzen_csrf"), Value: cookie.Value})
				}
			}
			request := func(method, path string, values url.Values) *http.Request {
				r := authenticatedRequest(method, "http://example.test"+path, cookies, values)
				if public {
					r = publicRequest(method, "http://example.test"+path)
					for _, cookie := range cookies {
						r.AddCookie(cookie)
					}
					r.Header.Set("Origin", "https://example.test")
				} else {
					r.Header.Set("Origin", "http://example.test")
				}
				if values != nil {
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					r.Body = io.NopCloser(strings.NewReader(values.Encode()))
				}
				return r
			}
			w := httptest.NewRecorder()
			fixture.handler.ServeHTTP(w, request(http.MethodGet, "/account", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("old session rejected: %d", w.Code)
			}
			bad := request(http.MethodGet, "/account", nil)
			bad.AddCookie(&http.Cookie{Name: fixture.server.cookieName(sessionCookie), Value: "invalid-new-session"})
			w = httptest.NewRecorder()
			fixture.handler.ServeHTTP(w, bad)
			if w.Code != http.StatusSeeOther {
				t.Fatal("invalid new identity fell back to legacy session")
			}
			if public {
				bad = publicRequest(http.MethodGet, "http://example.test/account")
				bad.AddCookie(&http.Cookie{Name: "buntzen_session", Value: session})
				bad.AddCookie(&http.Cookie{Name: "buntzen_csrf", Value: csrf})
				w = httptest.NewRecorder()
				fixture.handler.ServeHTTP(w, bad)
				if w.Code != http.StatusSeeOther {
					t.Fatal("public mode accepted unprefixed legacy cookies")
				}
			}
			w = httptest.NewRecorder()
			fixture.handler.ServeHTTP(w, request(http.MethodPost, "/logout", url.Values{"csrf_token": {csrf}}))
			if w.Code != http.StatusSeeOther {
				t.Fatalf("legacy logout failed: %d", w.Code)
			}
			cleared := make(map[string]bool)
			for _, cookie := range w.Result().Cookies() {
				cleared[cookie.Name] = cookie.MaxAge == -1
			}
			for _, name := range []string{sessionCookie, csrfCookie, "buntzen_session", "buntzen_csrf"} {
				if !cleared[fixture.server.cookieName(name)] {
					t.Fatalf("cookie %s not cleared", name)
				}
			}
			if _, err := fixture.store.GetSession(context.Background(), session); err == nil {
				t.Fatal("logout retained server session")
			}
		})
	}
}

func TestLakeSelectionCreateEditAndRejectUnknown(t *testing.T) {
	fixture := newWebFixture(t)
	profile, _ := createImmediateWebBooking(t, fixture, fixture.admin.ID, "lake owner", true)
	cookies := loginCookies(t, fixture)
	page := serveForm(fixture, http.MethodGet, "/bookings/new", cookies, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `<select name="lake_id" required>`) || !strings.Contains(page.Body.String(), `value="buntzen" selected>Buntzen Lake`) {
		t.Fatal("new booking does not expose the supported lake selector")
	}
	form := url.Values{
		"csrf_token": {csrfFrom(cookies)}, "name": {"Selected lake"}, "lake_id": {"buntzen"}, "profile_id": {stringID(profile.ID)},
		"enabled": {"1"}, "target_date": {"2030-01-15"}, "timezone": {"America/Vancouver"}, "release_time": {"07:00"},
		"confirmation_mode": {"manual"}, "all_day_pass_url": {"https://example.test/all"}, "half_day_pass_url": {"https://example.test/half"},
		"prep_minutes_before": {"30"}, "auth_deadline_minutes_before": {"5"}, "poll_deadline_seconds": {"120"},
		"poll_min_seconds": {"1"}, "poll_max_seconds": {"2"}, "pass_priority_1": {"all_day"},
	}
	response := serveForm(fixture, http.MethodPost, "/bookings/new", cookies, form)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("create=%d: %s", response.Code, response.Body.String())
	}
	bookings, err := fixture.store.ForUser(fixture.admin.ID).ListBookingRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, booking := range bookings {
		if booking.Name == "Selected lake" {
			id = booking.ID
			if booking.LakeID != "buntzen" {
				t.Fatal("lake not persisted")
			}
		}
	}
	if id == 0 {
		t.Fatal("created booking missing")
	}
	path := "/bookings/" + stringID(id)
	page = serveForm(fixture, http.MethodGet, path, cookies, nil)
	if !strings.Contains(page.Body.String(), `value="buntzen" selected>Buntzen Lake`) {
		t.Fatal("edit lost lake choice")
	}
	form.Set("lake_id", "unsupported")
	response = serveForm(fixture, http.MethodPost, path, cookies, form)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatal("unknown lake accepted")
	}
	saved, err := fixture.store.ForUser(fixture.admin.ID).GetBookingRequest(context.Background(), id)
	if err != nil || saved.LakeID != "buntzen" {
		t.Fatal("invalid update changed the destination")
	}
	page = serveForm(fixture, http.MethodGet, "/bookings/new?lake_id=unsupported", cookies, nil)
	if page.Code != http.StatusBadRequest {
		t.Fatal("unknown lake query silently defaulted")
	}
}
