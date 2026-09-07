package web

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func publicFixture(t *testing.T) webFixture {
	t.Helper()
	f := newWebFixture(t)
	f.cfg.PublicOrigin = "https://example.test"
	f.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	server, err := NewServer(f.cfg, f.store, f.server.engine)
	if err != nil {
		t.Fatal(err)
	}
	f.server, f.handler = server, server.Handler()
	return f
}

func TestPublicBoundaryRejectsUntrustedTransportAndAmbiguousHeaders(t *testing.T) {
	f := publicFixture(t)
	cases := []struct {
		name   string
		change func(*http.Request)
	}{
		{"untrusted socket with forged headers", func(r *http.Request) { r.RemoteAddr = "198.51.100.2:51000" }},
		{"missing scheme", func(r *http.Request) { r.Header.Del("X-Forwarded-Proto") }},
		{"HTTP visitor", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }},
		{"duplicate scheme", func(r *http.Request) { r.Header.Add("X-Forwarded-Proto", "https") }},
		{"comma scheme", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http,https") }},
		{"missing visitor", func(r *http.Request) { r.Header.Del("CF-Connecting-IP") }},
		{"duplicate visitor", func(r *http.Request) { r.Header.Add("CF-Connecting-IP", "203.0.113.8") }},
		{"visitor list", func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "203.0.113.7, 203.0.113.8") }},
		{"visitor with port", func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "203.0.113.7:1234") }},
		{"visitor zone", func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "fe80::1%en0") }},
		{"loopback visitor", func(r *http.Request) { r.Header.Set("CF-Connecting-IP", "::ffff:127.0.0.1") }},
		{"localhost alias", func(r *http.Request) { r.Host = "localhost:8080" }},
		{"operator host alias", func(r *http.Request) { r.Host = "container.internal" }},
		{"different port", func(r *http.Request) { r.Host = "example.test:444" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := publicRequest(http.MethodGet, "http://example.test/login")
			tc.change(r)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
				t.Fatalf("rejected request = %d cookies=%d", w.Code, len(w.Result().Cookies()))
			}
		})
	}
}

func TestPublicClientIPComesFromValidatedPeerOnly(t *testing.T) {
	f := publicFixture(t)
	f.server.mux.HandleFunc("GET /test-ip", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, remoteIP(r)) })
	for _, tc := range []struct {
		name, peer, forwarded, want string
		directTLS                   bool
	}{
		{"trusted visitor", "127.0.0.1:51000", "203.0.113.7", "203.0.113.7", false},
		{"mapped IPv4 visitor", "[::ffff:127.0.0.1]:51000", "::ffff:203.0.113.7", "203.0.113.7", false},
		{"direct TLS ignores forged forwarding", "198.51.100.2:51000", "203.0.113.7", "198.51.100.2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := publicRequest(http.MethodGet, "http://example.test:443/test-ip")
			r.RemoteAddr = tc.peer
			r.Header.Set("CF-Connecting-IP", tc.forwarded)
			r.Header.Set("X-Forwarded-For", "attacker-selected")
			r.Header.Set("X-Forwarded-Host", "attacker.example")
			if tc.directTLS {
				r.TLS = &tls.ConnectionState{}
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != tc.want {
				t.Fatalf("visitor = %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestPublicHealthExceptionDoesNotExposeUI(t *testing.T) {
	f := publicFixture(t)
	for _, tc := range []struct {
		target, peer string
		want         int
	}{
		{"http://127.0.0.1:8080/healthz", "127.0.0.1:51000", http.StatusOK},
		{"http://container.internal/healthz", "198.51.100.2:51000", http.StatusOK},
		{"http://127.0.0.1:8080/healthz", "198.51.100.2:51000", http.StatusBadRequest},
		{"http://container.internal/login", "127.0.0.1:51000", http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.target, nil)
		r.RemoteAddr = tc.peer
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != tc.want || len(w.Result().Cookies()) != 0 {
			t.Errorf("%s = %d, cookies=%d", tc.target, w.Code, len(w.Result().Cookies()))
		}
	}
}

func TestPublicOriginAndCookiesAcrossLoginLogout(t *testing.T) {
	f := publicFixture(t)
	get := httptest.NewRecorder()
	f.handler.ServeHTTP(get, publicRequest(http.MethodGet, "http://example.test/login"))
	csrf := get.Result().Cookies()[0]
	form := url.Values{"username": {"admin"}, "password": {"long-test-password"}, "csrf_token": {csrf.Value}}
	post := func(target, browserOrigin string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		r := publicRequest(http.MethodPost, "http://example.test"+target)
		r.Body = io.NopCloser(strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", browserOrigin)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	if w := post("/login", "http://example.test", []*http.Cookie{csrf}); w.Code != http.StatusForbidden {
		t.Fatalf("HTTP Origin accepted: %d", w.Code)
	}
	w := post("/login", "https://example.test", []*http.Cookie{csrf})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("HTTPS login = %d %s", w.Code, w.Body.String())
	}
	var authCookies []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" || !strings.HasPrefix(c.Name, "__Host-") || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("unsafe cookie: %+v", c)
		}
		if c.Name == "__Host-"+sessionCookie || c.Name == "__Host-"+csrfCookie {
			authCookies = append(authCookies, c)
		}
	}
	if len(authCookies) != 2 {
		t.Fatalf("expected public session and CSRF cookies, got %d", len(authCookies))
	}
	var csrfToken string
	for _, c := range authCookies {
		if c.Name == "__Host-"+csrfCookie {
			csrfToken = c.Value
		}
	}
	form = url.Values{"csrf_token": {csrfToken}}
	w = post("/logout", "https://example.test", authCookies)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if !c.Secure || c.MaxAge != -1 || !strings.HasPrefix(c.Name, "__Host-") || c.Path != "/" || c.Domain != "" {
			t.Fatalf("unsafe cookie deletion: %+v", c)
		}
	}
}

func TestPublicProxyVisitorLoginLimitsAreIndependent(t *testing.T) {
	f := publicFixture(t)
	for i := 0; i < loginIPLimit; i++ {
		if err := f.store.RecordLoginAttempt(context.Background(), loginRateKey("ip", "203.0.113.7"), false); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ visitor, want string }{{"203.0.113.7", "/login?error=limited"}, {"203.0.113.8", "/"}} {
		get := httptest.NewRecorder()
		f.handler.ServeHTTP(get, publicRequest(http.MethodGet, "http://example.test/login"))
		csrf := get.Result().Cookies()[0]
		r := publicRequest(http.MethodPost, "http://example.test/login")
		r.Header.Set("CF-Connecting-IP", tc.visitor)
		r.Header.Set("Origin", "https://example.test")
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Body = io.NopCloser(strings.NewReader(url.Values{"username": {"admin"}, "password": {"long-test-password"}, "csrf_token": {csrf.Value}}.Encode()))
		r.AddCookie(csrf)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != tc.want {
			t.Errorf("visitor %s: %d %s", tc.visitor, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestPublicModeRejectsUnprefixedDomainCookieSession(t *testing.T) {
	f := publicFixture(t)
	credentials, err := f.store.NewSession(context.Background(), f.admin.ID, sessionLifetime)
	if err != nil {
		t.Fatal(err)
	}
	r := publicRequest(http.MethodGet, "http://example.test/account")
	// A sibling can set parent-Domain cookies with these old names. HTTP's
	// Cookie header carries no Domain/Path provenance for the server to check.
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: credentials.Token})
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: credentials.CSRFToken})
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("public mode accepted unprefixed domain-cookie session: %d", w.Code)
	}
}

func publicRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	return r
}

func TestPublicLoginHasSecureCookiesAndHSTS(t *testing.T) {
	f := publicFixture(t)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, publicRequest(http.MethodGet, "http://example.test/login"))
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d", w.Code)
	}
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("missing HSTS")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Domain != "" || cookies[0].Path != "/" || cookies[0].Name != "__Host-"+loginCSRFCookie {
		t.Fatalf("unsafe login cookie attributes: %+v", cookies)
	}
}
