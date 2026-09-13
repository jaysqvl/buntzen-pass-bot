package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPrivateHostCheckToggle(t *testing.T) {
	f := newWebFixture(t)
	f.server.config.AllowedHosts = []string{"listed.example:8091"}
	for _, enabled := range []bool{true, false} {
		f.server.config.HostCheckEnabled = enabled
		for _, tc := range []struct {
			host    string
			allowed bool
			valid   bool
		}{
			{"listed.example:8091", true, true},
			{"listed.example:8092", false, true},
			{"lake.example", false, true},
			{"192.0.2.10:8091", false, true},
			{"[2001:db8::1]:8091", false, true},
			{"localhost:8080", true, true},
			{"", false, false},
			{"lake.example/path", false, false},
			{"user@example.test", false, false},
			{"lake.example:99999", false, false},
			{"*.example", false, false},
		} {
			t.Run(fmt.Sprintf("enabled=%t/host=%s", enabled, tc.host), func(t *testing.T) {
				for _, path := range []string{"/login", "/static/app.css", "/healthz"} {
					r := httptest.NewRequest(http.MethodGet, "http://example.test"+path, nil)
					r.Host = tc.host
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					want := http.StatusBadRequest
					if tc.valid && (!enabled || tc.allowed) {
						want = http.StatusOK
					}
					if w.Code != want {
						t.Errorf("%s = %d, want %d: %s", path, w.Code, want, w.Body.String())
					}
				}
			})
		}
	}
}

func TestPrivateHostCheckDisabledPreservesLoginOriginAndCSRF(t *testing.T) {
	f := newWebFixture(t)
	f.server.config.HostCheckEnabled = false
	const base = "http://new-lake.example"
	get := httptest.NewRecorder()
	f.handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, base+"/login", nil))
	if get.Code != http.StatusOK || len(get.Result().Cookies()) != 1 {
		t.Fatalf("login page = %d, cookies=%d", get.Code, len(get.Result().Cookies()))
	}
	loginCSRF := get.Result().Cookies()[0]
	post := func(path, browserOrigin, token string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		form := url.Values{"username": {"admin"}, "password": {"long-test-password"}, "csrf_token": {token}}
		r := authenticatedRequest(http.MethodPost, base+path, cookies, form)
		r.Header.Set("Origin", browserOrigin)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	assertRejected := func(path, token string, cookies []*http.Cookie) {
		t.Helper()
		for _, tc := range []struct{ origin, csrf, message string }{
			{"http://foreign.example", token, "cross-origin"},
			{base, "wrong-token", "invalid CSRF token"},
		} {
			w := post(path, tc.origin, tc.csrf, cookies)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("rejected %s = %d: %s", path, w.Code, w.Body.String())
			}
		}
	}
	assertRejected("/login", loginCSRF.Value, []*http.Cookie{loginCSRF})
	w := post("/login", base, loginCSRF.Value, []*http.Cookie{loginCSRF})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login = %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	for _, cookie := range cookies {
		if cookie.Domain != "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatalf("unsafe cookie: %+v", cookie)
		}
	}
	assertRejected("/logout", csrfFrom(cookies), cookies)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, authenticatedRequest(http.MethodGet, base+"/account", cookies, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated account = %d", w.Code)
	}
	w = post("/logout", base, csrfFrom(cookies), cookies)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, authenticatedRequest(http.MethodGet, base+"/account", cookies, nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated account = %d", w.Code)
	}
}

func TestPrivateHostCheckDisabledStillRequiresSetupToken(t *testing.T) {
	f := newUninitializedWebFixture(t)
	f.server.config.HostCheckEnabled = false
	const base = "http://new-lake.example"
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, base+"/setup", nil))
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 1 {
		t.Fatalf("setup page = %d, cookies=%d", w.Code, len(w.Result().Cookies()))
	}
	cookie := w.Result().Cookies()[0]
	form := url.Values{
		"csrf_token": {cookie.Value}, "setup_token": {"wrong-setup-token"},
		"username": {"owner"}, "password": {"long-test-password"}, "password_confirm": {"long-test-password"},
	}
	r := authenticatedRequest(http.MethodPost, base+"/setup", []*http.Cookie{cookie}, form)
	r.Header.Set("Origin", base)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "not accepted") {
		t.Fatalf("setup without valid token = %d: %s", w.Code, w.Body.String())
	}
	if hasUsers, err := f.store.HasUsers(context.Background()); err != nil || hasUsers {
		t.Fatalf("invalid setup created user: hasUsers=%v err=%v", hasUsers, err)
	}
}
