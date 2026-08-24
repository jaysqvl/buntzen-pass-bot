package web

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type webFixture struct {
	handler http.Handler
	store   *store.Store
	cfg     config.Config
}

func newWebFixture(t *testing.T) webFixture {
	t.Helper()
	directory := t.TempDir()
	box, err := secretcrypto.LoadOrCreate(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenMigrated(context.Background(), filepath.Join(directory, "buntzen.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, created, err := database.BootstrapAdmin(context.Background(), "admin", "long-test-password"); err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	cfg := config.Config{AppDataDir: directory, ProfilesDir: filepath.Join(directory, "profiles"), ArtifactsDir: filepath.Join(directory, "artifacts"), AdminUsername: "admin", MaxConcurrentJobs: 1, PythonExecutable: "python3", PythonModule: "buntzen_actions", BlueBubblesURL: "http://127.0.0.1:1234"}
	runner := engine.New(cfg, database, control.NewHub())
	server, err := NewServer(cfg, database, runner)
	if err != nil {
		t.Fatal(err)
	}
	return webFixture{handler: server.Handler(), store: database, cfg: cfg}
}

func loginCookies(t *testing.T, fixture webFixture) []*http.Cookie {
	t.Helper()
	get := httptest.NewRequest(http.MethodGet, "http://example.test/login", nil)
	getRecorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GET login = %d", getRecorder.Code)
	}
	var loginCSRF *http.Cookie
	for _, cookie := range getRecorder.Result().Cookies() {
		if cookie.Name == loginCSRFCookie {
			loginCSRF = cookie
		}
	}
	if loginCSRF == nil {
		t.Fatal("login CSRF cookie missing")
	}
	form := url.Values{"csrf_token": {loginCSRF.Value}, "password": {"long-test-password"}}
	post := httptest.NewRequest(http.MethodPost, "http://example.test/login", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("Origin", "http://example.test")
	post.AddCookie(loginCSRF)
	postRecorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(postRecorder, post)
	if postRecorder.Code != http.StatusSeeOther {
		t.Fatalf("POST login = %d: %s", postRecorder.Code, postRecorder.Body.String())
	}
	var result []*http.Cookie
	for _, cookie := range postRecorder.Result().Cookies() {
		if cookie.Name == sessionCookie || cookie.Name == csrfCookie {
			if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
				t.Fatalf("weak cookie attributes: %#v", cookie)
			}
			result = append(result, cookie)
		}
	}
	if len(result) != 2 {
		t.Fatalf("auth cookies = %d", len(result))
	}
	return result
}

func authenticatedRequest(method, target string, cookies []*http.Cookie, form url.Values) *http.Request {
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, target, body)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return request
}

func csrfFrom(cookies []*http.Cookie) string {
	for _, cookie := range cookies {
		if cookie.Name == csrfCookie {
			return cookie.Value
		}
	}
	return ""
}

func TestLoginCookiesCSRFOriginAndNoStore(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	request := authenticatedRequest(http.MethodGet, "http://example.test/", cookies, nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", recorder.Code)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", recorder.Header().Get("Cache-Control"))
	}
	if !strings.Contains(recorder.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("security policy missing")
	}
	if recorder.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("referrer policy = %q", recorder.Header().Get("Referrer-Policy"))
	}

	loginRequest := httptest.NewRequest(http.MethodGet, "http://example.test/login", nil)
	loginRecorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(loginRecorder, loginRequest)
	loginHTML := loginRecorder.Body.String()
	if !strings.Contains(loginHTML, `<meta name="referrer" content="same-origin">`) {
		t.Fatal("same-origin referrer meta missing")
	}
	if !strings.Contains(loginHTML, `"includeIndicatorStyles":false`) {
		t.Fatal("HTMX inline indicator styles are not disabled")
	}

	form := url.Values{"csrf_token": {csrfFrom(cookies)}}
	post := authenticatedRequest(http.MethodPost, "http://example.test/logout", cookies, form)
	postRecorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(postRecorder, post)
	if postRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin-less POST = %d", postRecorder.Code)
	}
	post = authenticatedRequest(http.MethodPost, "http://example.test/logout", cookies, form)
	post.Header.Set("Origin", "http://evil.example")
	postRecorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(postRecorder, post)
	if postRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d", postRecorder.Code)
	}
}

func TestOriginAllowedForConfiguredProxyOrigin(t *testing.T) {
	fixture := newWebFixture(t)
	fixture.cfg.AllowedOrigins = []string{"http://buntzen.example"}
	runner := engine.New(fixture.cfg, fixture.store, control.NewHub())
	server, err := NewServer(fixture.cfg, fixture.store, runner)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://container.internal/login", strings.NewReader("csrf_token=x"))
	request.Header.Set("Origin", "http://buntzen.example")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "invalid CSRF token") {
		t.Fatalf("configured proxy origin = %d: %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://container.internal/login", strings.NewReader("csrf_token=x"))
	request.Header.Set("Origin", "http://untrusted.example:9080")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request rejected") {
		t.Fatalf("untrusted proxy origin = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRootRouteDoesNotCatchBrowserAssetRequests(t *testing.T) {
	fixture := newWebFixture(t)
	request := httptest.NewRequest(http.MethodGet, "http://example.test/favicon.ico", nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("favicon status = %d", recorder.Code)
	}
	if recorder.Header().Get("Location") != "" {
		t.Fatalf("favicon redirected to %q", recorder.Header().Get("Location"))
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == loginCSRFCookie {
			t.Fatal("favicon request minted a login CSRF cookie")
		}
	}
}

func TestOriginAllowedForMissingOriginWithSameOriginFetchMetadata(t *testing.T) {
	fixture := newWebFixture(t)
	request := httptest.NewRequest(http.MethodPost, "http://example.test/login", strings.NewReader("csrf_token=x"))
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "invalid CSRF token") {
		t.Fatalf("same-origin fetch metadata = %d: %s", recorder.Code, recorder.Body.String())
	}

	for _, fetchSite := range []string{"", "same-site", "cross-site", "none"} {
		request = httptest.NewRequest(http.MethodPost, "http://example.test/login", strings.NewReader("csrf_token=x"))
		request.Header.Set("Sec-Fetch-Site", fetchSite)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder = httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request rejected") {
			t.Fatalf("fetch site %q = %d: %s", fetchSite, recorder.Code, recorder.Body.String())
		}
	}

	request = httptest.NewRequest(http.MethodPost, "http://example.test/login", strings.NewReader("csrf_token=x"))
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request rejected") {
		t.Fatalf("opaque origin = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestSameOriginCanonicalizesCaseAndDefaultPort(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://EXAMPLE.test/login", nil)
	request.Host = "example.test"
	request.Header.Set("Origin", "HTTP://example.TEST:80")
	if !sameOrigin(request) {
		t.Fatal("equivalent browser origin was rejected")
	}
}

func TestEncryptedSecretsAreNeverRendered(t *testing.T) {
	fixture := newWebFixture(t)
	const providerPassword = "synthetic-bluebubbles-password"
	const yodelEmail = "private-yodel@example.test"
	const yodelPassword = "synthetic-yodel-password"
	source, err := fixture.store.CreateOTPSource(context.Background(), store.OTPSourceInput{Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://127.0.0.1:1234", ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:1234", Password: providerPassword}, PairingChatGUID: "iMessage;-;sender", PairingSender: "+15550100123", PairingService: "iMessage"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := fixture.store.CreateProfile(context.Background(), store.ProfileInput{Name: "Example", BrowserProfile: "example", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Email: yodelEmail, Password: yodelPassword}})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	for _, target := range []string{fmt.Sprintf("http://example.test/sources/%d", source.ID), fmt.Sprintf("http://example.test/profiles/%d", profile.ID), "http://example.test/"} {
		request := authenticatedRequest(http.MethodGet, target, cookies, nil)
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, recorder.Code)
		}
		body := recorder.Body.String()
		for _, secret := range []string{providerPassword, yodelEmail, yodelPassword} {
			if strings.Contains(body, secret) {
				t.Fatalf("GET %s rendered a secret", target)
			}
		}
	}
}

func TestSSEStopsWhenSessionIsRevoked(t *testing.T) {
	fixture := newWebFixture(t)
	source, err := fixture.store.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "SSE Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://127.0.0.1:2234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:2234", Password: "test-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := fixture.store.CreateProfile(context.Background(), store.ProfileInput{
		Name: "SSE profile", BrowserProfile: "sse-profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Email: "sse@example.test", Password: "password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := fixture.store.CreateBookingRequest(context.Background(), model.BookingRequest{
		Name: "SSE booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2030-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := fixture.store.EnqueueJob(context.Background(), store.EnqueueJobParams{
		BookingRequestID: &booking.ID, Command: model.CommandDryRun,
		RunMode: model.RunModeDryRun, DueAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/jobs/%d/events", server.URL, job.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	var rawSession string
	for _, cookie := range cookies {
		request.AddCookie(cookie)
		if cookie.Name == sessionCookie {
			rawSession = cookie.Value
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("SSE response status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	if err := fixture.store.DeleteSession(context.Background(), rawSession); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatal("SSE closed without an auth_expired event")
			}
			if line == "event: auth_expired" {
				return
			}
		case <-timer.C:
			t.Fatal("revoked SSE session remained connected")
		}
	}
}
