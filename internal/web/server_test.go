package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
	server  *Server
	store   *store.Store
	cfg     config.Config
	admin   model.User
}

func newWebFixture(t *testing.T) webFixture {
	return newWebFixtureWithSetup(t, true)
}

func newUninitializedWebFixture(t *testing.T) webFixture {
	return newWebFixtureWithSetup(t, false)
}

func newWebFixtureWithSetup(t *testing.T, setup bool) webFixture {
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
	var admin model.User
	if setup {
		admin, err = database.SetupAdmin(context.Background(), "admin", "long-test-password")
		if err != nil {
			t.Fatalf("setup administrator: %v", err)
		}
	}
	cfg := config.Config{AppDataDir: directory, ProfilesDir: filepath.Join(directory, "profiles"), ArtifactsDir: filepath.Join(directory, "artifacts"), MaxConcurrentJobs: 1, PythonExecutable: "python3", PythonModule: "buntzen_actions", BlueBubblesURL: "http://127.0.0.1:1234", YodelOrigins: []string{"https://example.test"}, AllowedHosts: []string{"example.test", "container.internal"}, SetupToken: "test-only-setup-token"}
	runner := engine.New(cfg, database, control.NewHub())
	server, err := NewServer(cfg, database, runner)
	if err != nil {
		t.Fatal(err)
	}
	return webFixture{handler: server.Handler(), server: server, store: database, cfg: cfg, admin: admin}
}

type blockingFormReader struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (r *blockingFormReader) Read([]byte) (int, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	return 0, io.EOF
}

func loginCookies(t *testing.T, fixture webFixture) []*http.Cookie {
	return loginCookiesAs(t, fixture, "admin", "long-test-password")
}

func loginCookiesAs(t *testing.T, fixture webFixture, username, password string) []*http.Cookie {
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
	form := url.Values{"csrf_token": {loginCSRF.Value}, "username": {username}, "password": {password}}
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

func TestSlowInvalidBodyDoesNotHoldPublicAuthMutex(t *testing.T) {
	testRequest := func(t *testing.T, fixture webFixture, target string, form url.Values, csrfCookie *http.Cookie, wantLocation string) {
		t.Helper()
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		slow := httptest.NewRequest(http.MethodPost, "http://example.test"+target,
			&blockingFormReader{started: started, release: release})
		slow.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		slow.Header.Set("Origin", "http://example.test")
		slowDone := make(chan struct{})
		go func() {
			fixture.handler.ServeHTTP(httptest.NewRecorder(), slow)
			close(slowDone)
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("slow request never began reading its body")
		}

		goodDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			goodDone <- serveForm(fixture, http.MethodPost, target, []*http.Cookie{csrfCookie}, form)
		}()
		var recorder *httptest.ResponseRecorder
		select {
		case recorder = <-goodDone:
		case <-time.After(4 * time.Second):
			close(release)
			<-slowDone
			t.Fatal("a slow unauthenticated body blocked a valid auth request")
		}
		close(release)
		select {
		case <-slowDone:
		case <-time.After(2 * time.Second):
			t.Fatal("slow request did not finish after its body was released")
		}
		if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != wantLocation {
			t.Fatalf("valid %s request = %d location=%q body=%s", target, recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
		}
	}

	t.Run("login", func(t *testing.T) {
		fixture := newWebFixture(t)
		cookie, _ := publicFormCookie(t, fixture, "/login")
		form := url.Values{
			"csrf_token": {cookie.Value}, "username": {"admin"}, "password": {"long-test-password"},
		}
		testRequest(t, fixture, "/login", form, cookie, "/")
	})
	t.Run("setup", func(t *testing.T) {
		fixture := newUninitializedWebFixture(t)
		cookie, _ := publicFormCookie(t, fixture, "/setup")
		form := url.Values{
			"csrf_token": {cookie.Value}, "setup_token": {fixture.cfg.SetupToken},
			"username": {"owner"}, "password": {"initial owner password"},
			"password_confirm": {"initial owner password"},
		}
		testRequest(t, fixture, "/setup", form, cookie, "/?ok=setup")
	})
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
	const yodelPhone = "5559876543"
	userStore := fixture.store.ForUser(fixture.admin.ID)
	source, err := userStore.CreateOTPSource(context.Background(), store.OTPSourceInput{Name: "Messages", Provider: model.OTPProviderBlueBubbles, Identity: "http://127.0.0.1:1234", ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:1234", Password: providerPassword}, PairingChatGUID: "iMessage;-;sender", PairingSender: "+15550100123", PairingService: "iMessage"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := userStore.CreateProfile(context.Background(), store.ProfileInput{Name: "Example", DefaultVehicle: "Example Vehicle", OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15000, Enabled: true, Credentials: &model.ProfileCredentials{Phone: yodelPhone}})
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
		for _, secret := range []string{providerPassword, yodelPhone} {
			if strings.Contains(body, secret) {
				t.Fatalf("GET %s rendered a secret", target)
			}
		}
		if strings.Contains(target, "/profiles/") {
			if !strings.Contains(body, `name="yodel_phone"`) || strings.Contains(body, `name="yodel_email"`) || strings.Contains(body, `name="yodel_password"`) {
				t.Fatalf("profile form did not expose only the write-only mobile field")
			}
		}
	}
}

func TestNewBookingFormUsesLocalSafeDefaults(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	request := authenticatedRequest(http.MethodGet, "http://example.test/bookings/new", cookies, nil)
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("new booking form = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`name="timezone" value="America/Vancouver"`,
		`name="release_time" value="07:00"`,
		`name="login_probe_url" value="https://example.test/buntzen-lake"`,
		`name="all_day_pass_url" value="https://example.test/buntzen-lake/All-Day-Pass"`,
		`name="half_day_pass_url" value="https://example.test/buntzen-lake/Half-Day-Pass"`,
		`value="manual" selected`,
		`name="check_all_day" value="1" checked`,
		`name="check_afternoon" value="1" checked`,
		`name="check_morning" value="1" checked`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("new booking form missing safe default %q", expected)
		}
	}
}

func TestPairingExplainsTheMissingProfilePrerequisite(t *testing.T) {
	fixture := newWebFixture(t)
	resources := fixture.store.ForUser(fixture.admin.ID)
	source, err := resources.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Unassigned Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://127.0.0.1:2234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:2234", Password: "synthetic-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}}
	recorder := serveForm(fixture, http.MethodPost, fmt.Sprintf("/sources/%d/pair", source.ID), cookies, form)
	if recorder.Code != http.StatusConflict || recorder.Body.String() != "assign this source to an enabled Yodel profile before pairing\n" {
		t.Fatalf("pair without profile = %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestBookingRunReturnsBoundedConflictForPendingDuplicate(t *testing.T) {
	fixture := newWebFixture(t)
	resources := fixture.store.ForUser(fixture.admin.ID)
	source, err := resources.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "Queue test inbox", Provider: model.OTPProviderTwilio,
		Identity: "twilio:queue-web-test", ProviderConfig: map[string]string{"auth_token": "synthetic-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := resources.CreateProfile(context.Background(), store.ProfileInput{
		Name: "Queue test profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := resources.CreateBookingRequest(context.Background(), model.BookingRequest{
		Name: "Queue test booking", ProfileID: profile.ID, Enabled: true,
		TargetDate: "2030-01-15", Timezone: "UTC", ReleaseTime: "07:00",
		PrepMinutesBefore: 30, AuthDeadlineMinutesBefore: 5, PollDeadlineSeconds: 120,
		PollMinSeconds: 1, PollMaxSeconds: 2, ConfirmationMode: model.RunModeManual,
		LoginProbeURL: "https://example.test/login", AllDayPassURL: "https://example.test/all",
		CheckAllDay: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookies(t, fixture)
	form := url.Values{
		"csrf_token": {csrfFrom(cookies)},
		"command":    {string(model.CommandDryRun)},
	}
	recorder := serveForm(fixture, http.MethodPost,
		fmt.Sprintf("/bookings/%d/run", booking.ID), cookies, form)
	if recorder.Code != http.StatusSeeOther || !strings.HasPrefix(recorder.Header().Get("Location"), "/jobs/") {
		t.Fatalf("first run = %d location=%q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	recorder = serveForm(fixture, http.MethodPost,
		fmt.Sprintf("/bookings/%d/run", booking.ID), cookies, form)
	if recorder.Code != http.StatusConflict || recorder.Body.String() != "job could not be queued\n" {
		t.Fatalf("duplicate run = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if len(recorder.Body.Bytes()) > 64 {
		t.Fatalf("duplicate response was not bounded: %d bytes", len(recorder.Body.Bytes()))
	}
	jobs, err := resources.ListJobs(context.Background(), 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs after duplicate POST=%+v err=%v", jobs, err)
	}
}

func TestSSEStopsWhenSessionIsRevoked(t *testing.T) {
	fixture := newWebFixture(t)
	userStore := fixture.store.ForUser(fixture.admin.ID)
	source, err := userStore.CreateOTPSource(context.Background(), store.OTPSourceInput{
		Name: "SSE Messages", Provider: model.OTPProviderBlueBubbles,
		Identity:       "http://127.0.0.1:2234",
		ProviderConfig: bluebubbles.Config{BaseURL: "http://127.0.0.1:2234", Password: "test-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := userStore.CreateProfile(context.Background(), store.ProfileInput{
		Name: "SSE profile", DefaultVehicle: "Example Vehicle",
		OTPSourceID: source.ID, Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
		Credentials: &model.ProfileCredentials{Phone: "5559876543"},
	})
	if err != nil {
		t.Fatal(err)
	}
	booking, err := userStore.CreateBookingRequest(context.Background(), model.BookingRequest{
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
	job, err := userStore.EnqueueJob(context.Background(), store.EnqueueJobParams{
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
