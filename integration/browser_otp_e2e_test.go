//go:build integration

package integration_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
)

const (
	testPhone      = "5559876543"
	testOTP        = "482913"
	testBBPassword = "synthetic-bluebubbles-password"
	testChatGUID   = "SMS;-;55555"
	testSender     = "55555"
	testService    = "SMS"
)

// TestControlPlanePythonBrowserBlueBubblesOTP exercises the real JSON-lines
// subprocess boundary, Python action package, Playwright browser, Yodel DOM
// interaction, Go coordinator, and query-only BlueBubbles adapter. Both HTTP
// services are process-local and every credential/code is synthetic.
func TestControlPlanePythonBrowserBlueBubblesOTP(t *testing.T) {
	if testing.Short() {
		t.Skip("real-browser integration test")
	}

	repoRoot := repositoryRoot(t)
	python, pythonArgs := pythonWorker(t, repoRoot)
	browserExecutable := browserPath()
	flow := &e2eFlow{}

	blueBubbles := httptest.NewServer(http.HandlerFunc(flow.serveBlueBubbles))
	t.Cleanup(blueBubbles.Close)
	provider, err := bluebubbles.New(bluebubbles.Config{
		BaseURL:      blueBubbles.URL,
		Password:     testBBPassword,
		ChatGUID:     testChatGUID,
		Sender:       testSender,
		Service:      testService,
		PollInterval: 10 * time.Millisecond,
		Freshness:    time.Minute,
	})
	if err != nil {
		t.Fatalf("create BlueBubbles provider: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		t.Fatalf("BlueBubbles health: %v", err)
	}

	yodel := httptest.NewUnstartedServer(http.HandlerFunc(flow.serveYodel))
	yodel.Config.ErrorLog = log.New(io.Discard, "", 0)
	yodel.StartTLS()
	t.Cleanup(yodel.Close)
	profileDir := filepath.Join(t.TempDir(), "browser-profile")
	artifactDir := filepath.Join(t.TempDir(), "artifacts")

	var outputMu sync.Mutex
	var stderrLines []string
	var durableEvents []string
	hub := control.NewHub()
	result, err := control.Run(ctx, control.RunInput{
		JobID:   9001,
		Command: model.CommandAuthCheck,
		Mode:    model.RunModeAuto,
		StartConfig: map[string]any{
			"profile_dir":           profileDir,
			"target_date":           "2030-01-15",
			"timezone":              "UTC",
			"login_probe_url":       yodel.URL + "/buntzen-lake",
			"allowed_yodel_origins": []string{yodel.URL},
			"all_day_pass_url":      nil,
			"half_day_pass_url":     nil,
			"vehicle_keyword":       "Synthetic vehicle",
			"pass_order":            []string{},
			"headless":              true,
			"browser_channel":       nil,
			"executable_path":       nullableString(browserExecutable),
			"default_timeout_ms":    10_000,
			"poll_deadline_seconds": 10,
			"poll_min_seconds":      0.05,
			"poll_max_seconds":      0.1,
			"artifacts_dir":         artifactDir,
		},
		Credentials: model.ProfileCredentials{Phone: testPhone},
		Provider:    provider,
		OTPFilter: otp.Filter{
			ChatGUID:     testChatGUID,
			Sender:       testSender,
			Service:      testService,
			RequireYodel: true,
		},
		OTPTimeout:  10 * time.Second,
		CancelGrace: 5 * time.Second,
		Hub:         hub,
		NewProcess: func(processCtx context.Context) (control.ActionProcess, error) {
			return actionproc.Start(processCtx, actionproc.Config{
				Executable: python,
				Args:       pythonArgs,
				Environment: []string{
					"BUNTZEN_ACTIONPROC_HELPER=e2e-local-tls",
					"PYTHONDONTWRITEBYTECODE=1",
					"PYTHONUNBUFFERED=1",
				},
				CancelGrace: 5 * time.Second,
				OnStderr: func(line string) {
					outputMu.Lock()
					stderrLines = append(stderrLines, line)
					outputMu.Unlock()
				},
			})
		},
		Hooks: control.RunHooks{Event: func(kind, message string) {
			outputMu.Lock()
			durableEvents = append(durableEvents, kind+":"+message)
			outputMu.Unlock()
		}},
	})
	if err != nil {
		t.Fatalf("run coordinated action: %v\nworker stderr:\n%s", err, strings.Join(stderrLines, "\n"))
	}
	if result.Status != model.JobSucceeded || result.ExitCode != 0 {
		t.Fatalf("action result = %#v\nworker stderr:\n%s", result, strings.Join(stderrLines, "\n"))
	}

	snapshot := flow.snapshot()
	if len(snapshot.errors) != 0 {
		t.Fatalf("fake-service protocol violations: %s", strings.Join(snapshot.errors, "; "))
	}
	if snapshot.healthCalls != 1 {
		t.Errorf("BlueBubbles health calls = %d, want 1", snapshot.healthCalls)
	}
	if snapshot.armCalls != 1 || snapshot.waitCalls < 1 {
		t.Errorf("BlueBubbles query calls: arm=%d wait=%d", snapshot.armCalls, snapshot.waitCalls)
	}
	if !snapshot.armedBeforeLogin {
		t.Error("fake Yodel generated its OTP before BlueBubbles captured a cursor")
	}
	if snapshot.loginPhone != testPhone {
		t.Error("real browser did not submit the synthetic mobile number")
	}
	if snapshot.submittedOTP != testOTP {
		t.Errorf("real browser submitted OTP %q, want the provider code", snapshot.submittedOTP)
	}
	if !regexp.MustCompile(`\bChrome/\d+\.\d+\.\d+\.\d+\b`).MatchString(snapshot.userAgent) ||
		strings.Contains(snapshot.userAgent, "HeadlessChrome") {
		t.Errorf("fake Yodel did not observe an ordinary four-part Chrome user agent: %q", snapshot.userAgent)
	}
	if _, err := os.Stat(profileDir); err != nil {
		t.Errorf("persistent browser profile was not created: %v", err)
	}
	if _, ok := hub.OTP("9001", time.Now()); ok {
		t.Error("transient OTP remained in the live hub after browser submission")
	}

	outputMu.Lock()
	observedOutput := strings.Join(append(append([]string{}, stderrLines...), durableEvents...), "\n")
	eventOutput := strings.Join(durableEvents, "\n")
	outputMu.Unlock()
	for _, expected := range []string{"otp.armed:", "otp.received:", "otp.submitted:"} {
		if !strings.Contains(eventOutput, expected) {
			t.Errorf("durable event stream did not observe %s", expected)
		}
	}
	assertExcludesSecrets(t, []byte(observedOutput), "worker stderr and durable events")
	assertTreeExcludesSecrets(t, artifactDir)
	artifacts, err := os.ReadDir(artifactDir)
	if err != nil || len(artifacts) == 0 {
		t.Errorf("safe post-authentication trace was not produced: entries=%d error=%v", len(artifacts), err)
	}
}

type e2eFlow struct {
	mu sync.Mutex

	armed            bool
	triggered        bool
	armedBeforeLogin bool
	healthCalls      int
	armCalls         int
	waitCalls        int
	loginPhone       string
	submittedOTP     string
	userAgent        string
	errors           []string
}

type flowSnapshot struct {
	armedBeforeLogin bool
	healthCalls      int
	armCalls         int
	waitCalls        int
	loginPhone       string
	submittedOTP     string
	userAgent        string
	errors           []string
}

func (f *e2eFlow) snapshot() flowSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return flowSnapshot{
		armedBeforeLogin: f.armedBeforeLogin,
		healthCalls:      f.healthCalls,
		armCalls:         f.armCalls,
		waitCalls:        f.waitCalls,
		loginPhone:       f.loginPhone,
		submittedOTP:     f.submittedOTP,
		userAgent:        f.userAgent,
		errors:           append([]string(nil), f.errors...),
	}
}

func (f *e2eFlow) recordError(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *e2eFlow) serveBlueBubbles(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	if request.URL.Query().Get("password") != testBBPassword || len(request.URL.Query()) != 1 {
		f.recordError("BlueBubbles request did not use exactly one password query parameter")
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api/v1/ping":
		f.healthCalls++
		writeJSON(response, map[string]any{"status": 200, "data": "pong"})
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/message/query":
		var query struct {
			ChatGUID string `json:"chatGuid"`
			With     []string
			Offset   int
			Limit    int
			Sort     string
			Where    []struct {
				Statement string
				Args      map[string]int64
			}
		}
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			f.recordError("decode BlueBubbles query: %v", err)
			http.Error(response, "invalid query", http.StatusBadRequest)
			return
		}
		if query.ChatGUID != testChatGUID || len(query.With) != 1 || query.With[0] != "chat" || query.Offset != 0 {
			f.recordError("BlueBubbles query escaped paired, bounded chat scope")
		}
		if len(query.Where) == 0 {
			f.armCalls++
			if query.Sort != "DESC" || query.Limit != 1 {
				f.recordError("arm query was not a one-message descending snapshot")
			}
			f.armed = true
			writeBBQuery(response, query.Offset, query.Limit, []map[string]any{bbMessage(100, "old-message", "No code here", time.Now().Add(-time.Minute))})
			return
		}
		f.waitCalls++
		if query.Sort != "ASC" || query.Limit < 1 || query.Limit > 50 || len(query.Where) != 1 || query.Where[0].Statement != "message.ROWID > :minRowID" || query.Where[0].Args["minRowID"] != 100 {
			f.recordError("wait query did not use the armed cursor and bounded ascending page")
		}
		messages := []map[string]any{}
		if f.triggered {
			messages = append(messages, bbMessage(101, "fresh-yodel-otp", "Your Yodel verification code is "+testOTP, time.Now()))
		}
		writeBBQuery(response, query.Offset, query.Limit, messages)
	default:
		f.recordError("unexpected BlueBubbles operation %s %s", request.Method, request.URL.Path)
		http.NotFound(response, request)
	}
}

func (f *e2eFlow) serveYodel(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.userAgent == "" {
		f.userAgent = request.UserAgent()
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/buntzen-lake":
		if cookie, err := request.Cookie("synthetic-session"); err == nil && cookie.Value == "authenticated" {
			writeHTML(response, `<html><body><script>localStorage.setItem("BearerToken", "eyJhbGciOiJub25lIn0.eyJleHAiOjQxMDI0NDQ4MDB9.")</script><a href="/account">My Account</a></body></html>`)
			return
		}
		writeHTML(response, `<html><body><div class="popup"><a class="popup-close" href="#passes" onclick="this.parentElement.remove(); return false">Go To Pass(es)</a></div><a aria-label="Sign in / Register" href="/buntzen-lake/login">Profile</a></body></html>`)
	case request.Method == http.MethodGet && request.URL.Path == "/buntzen-lake/login":
		writeHTML(response, `<html><body><form method="post" action="/buntzen-lake/login"><input id="txtPhonenumber" name="number" inputmode="numeric" maxlength="10" aria-label="Mobile phone number mandatory"><a href="#" onclick="this.closest('form').requestSubmit(); return false">Next</a></form></body></html>`)
	case request.Method == http.MethodPost && request.URL.Path == "/buntzen-lake/login":
		if err := request.ParseForm(); err != nil {
			f.recordError("parse fake Yodel login: %v", err)
		}
		f.loginPhone = request.Form.Get("number")
		f.armedBeforeLogin = f.armed
		f.triggered = true
		http.Redirect(response, request, "/buntzen-lake/otp", http.StatusSeeOther)
	case request.Method == http.MethodGet && request.URL.Path == "/buntzen-lake/otp":
		writeHTML(response, `<html><body><form method="post" action="/buntzen-lake/otp"><input class="otpFocusInput" type="tel" name="code" maxlength="1" aria-label="verification code"><input class="otpFocusInput" type="tel" name="code" maxlength="1" aria-label="Digit 2"><input class="otpFocusInput" type="tel" name="code" maxlength="1" aria-label="Digit 3"><input class="otpFocusInput" type="tel" name="code" maxlength="1" aria-label="Digit 4"><input class="otpFocusInput" type="tel" name="code" maxlength="1" aria-label="Digit 5"><input class="otpFocusInput" type="tel" name="code" maxlength="1" aria-label="Digit 6"><a href="#" onclick="this.closest('form').requestSubmit(); return false">Verify</a></form></body></html>`)
	case request.Method == http.MethodPost && request.URL.Path == "/buntzen-lake/otp":
		if err := request.ParseForm(); err != nil {
			f.recordError("parse fake Yodel OTP: %v", err)
		}
		f.submittedOTP = strings.Join(request.Form["code"], "")
		if f.submittedOTP != testOTP {
			writeHTMLStatus(response, http.StatusUnauthorized, `<html><body>Invalid code</body></html>`)
			return
		}
		http.SetCookie(response, &http.Cookie{Name: "synthetic-session", Value: "authenticated", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(response, request, "/buntzen-lake", http.StatusSeeOther)
	default:
		http.NotFound(response, request)
	}
}

func bbMessage(rowID int64, guid, body string, receivedAt time.Time) map[string]any {
	return map[string]any{
		"originalROWID": rowID,
		"guid":          guid,
		"text":          body,
		"handle":        map[string]any{"address": testSender, "service": testService},
		"chats":         []map[string]any{{"guid": testChatGUID}},
		"dateCreated":   receivedAt.UnixMilli(),
		"isFromMe":      false,
	}
}

func writeBBQuery(response http.ResponseWriter, offset, limit int, messages []map[string]any) {
	writeJSON(response, map[string]any{
		"status": 200,
		"data":   messages,
		"metadata": map[string]any{
			"offset": offset,
			"limit":  limit,
			"total":  len(messages),
			"count":  len(messages),
		},
	})
}

func writeJSON(response http.ResponseWriter, value any) {
	_ = json.NewEncoder(response).Encode(value)
}

func writeHTML(response http.ResponseWriter, body string) {
	writeHTMLStatus(response, http.StatusOK, body)
}

func writeHTMLStatus(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), ".."))
}

func pythonWorker(t *testing.T, repoRoot string) (string, []string) {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("BUNTZEN_E2E_PYTHON")); configured != "" {
		return configured, []string{"-m", "buntzen_actions"}
	}
	venvPython := filepath.Join(repoRoot, "actions", ".venv", "bin", "python")
	if info, err := os.Stat(venvPython); err == nil && !info.IsDir() {
		return venvPython, []string{"-m", "buntzen_actions"}
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Fatal("browser integration requires actions/.venv or uv on PATH")
	}
	return uv, []string{"run", "--project", filepath.Join(repoRoot, "actions"), "--locked", "python", "-m", "buntzen_actions"}
}

func browserPath() string {
	if configured := strings.TrimSpace(os.Getenv("BUNTZEN_E2E_BROWSER_EXECUTABLE")); configured != "" {
		return configured
	}
	for _, candidate := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	// The pinned Playwright runtime resolves its own bundled Chromium in CI.
	return ""
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func assertTreeExcludesSecrets(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
			archive, err := zip.OpenReader(path)
			if err != nil {
				return err
			}
			defer archive.Close()
			for _, item := range archive.File {
				reader, err := item.Open()
				if err != nil {
					return err
				}
				contents, err := io.ReadAll(io.LimitReader(reader, 16<<20))
				_ = reader.Close()
				if err != nil {
					return err
				}
				assertExcludesSecrets(t, contents, filepath.Base(path)+":"+item.Name)
			}
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertExcludesSecrets(t, contents, filepath.Base(path))
		return nil
	})
	if err != nil {
		t.Fatalf("inspect integration artifacts: %v", err)
	}
}

func assertExcludesSecrets(t *testing.T, contents []byte, label string) {
	t.Helper()
	secrets := []string{testPhone, testOTP, testBBPassword}
	sort.Strings(secrets)
	for _, secret := range secrets {
		if strings.Contains(string(contents), secret) {
			t.Errorf("%s retained synthetic secret %q", label, secret)
		}
	}
}
