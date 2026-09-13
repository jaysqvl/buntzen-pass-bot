package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/config"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestBrowserPlainErrorsRenderSafePagesAndPreserveHTTPHeaders(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{renderer: renderer, config: config.Config{AllowedHosts: []string{"example.test"}}}
	for _, status := range []int{400, 401, 403, 404, 405, 413, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			raw := strings.Repeat("private-provider-diagnostic ", 1024)
			handler := server.browserErrorPages(server.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Content-Length", "12")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Retry-After", "60")
				w.Header().Set("Allow", "GET, HEAD")
				w.WriteHeader(status)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Fatal(err)
				}
				for range 8 {
					if n, err := w.Write([]byte(raw)); err != nil || n != len(raw) {
						t.Fatalf("discarded write n=%d err=%v", n, err)
					}
				}
			})))
			request := httptest.NewRequest(http.MethodPost, "http://example.test/bookings/123/run?notice=queue-pending&return=https://untrusted.invalid", nil)
			request.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9")
			request.Header.Set("Referer", "https://untrusted.invalid/private")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != status || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, "Lake Pass Bot") {
				t.Fatalf("fallback status=%d headers=%v body=%q", recorder.Code, recorder.Header(), body)
			}
			returnURL := "/bookings"
			if status >= http.StatusInternalServerError {
				returnURL = "/jobs"
			}
			if status == http.StatusUnauthorized {
				returnURL = "/login"
			}
			if !strings.Contains(body, `href="`+returnURL+`"`) || strings.Contains(body, "private-provider-diagnostic") || strings.Contains(body, "untrusted.invalid") ||
				strings.Contains(body, "A job already exists") || strings.Contains(body, `class="topbar"`) || strings.Contains(body, "csrf_token") {
				t.Fatalf("fallback leaked raw input, fabricated authentication, or unsafe return link: %q", body)
			}
			if recorder.Header().Get("Retry-After") != "60" || recorder.Header().Get("Allow") != "GET, HEAD" ||
				recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Content-Security-Policy") == "" || recorder.Header().Get("X-Frame-Options") != "DENY" {
				t.Fatalf("HTTP policy headers changed: %v", recorder.Header())
			}
			if recorder.Header().Get("Content-Length") != "" || recorder.Header().Get("Content-Encoding") != "" || recorder.Flushed {
				t.Fatalf("discarded representation was committed: headers=%v flushed=%v", recorder.Header(), recorder.Flushed)
			}
		})
	}
}

func TestBrowserActionServerErrorsDirectUsersToCheckJobs(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{renderer: renderer}
	for _, path := range []string{"/bookings/1/run", "/jobs/1/decision"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Accept", "text/html")
			recorder := httptest.NewRecorder()
			server.browserErrorPages(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unknown action outcome", http.StatusInternalServerError)
			})).ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusInternalServerError ||
				!strings.Contains(body, "We could not confirm the result of this action. Check Jobs before trying again.") ||
				!strings.Contains(body, `href="/jobs"`) || strings.Contains(body, "Return to the app and try again.") {
				t.Fatalf("uncertain action encouraged a blind retry: status=%d body=%q", recorder.Code, body)
			}
		})
	}
}

func TestBrowserErrorFallbackLeavesMachineHTMLRedirectsAndStreamsUnchanged(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{renderer: renderer}
	for _, accept := range []string{"", "application/json", "*/*", "text/html;q=0,application/json", "text/event-stream,text/html"} {
		t.Run("plain machine "+accept, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/jobs/1/events", nil)
			request.Header.Set("Accept", accept)
			server.browserErrorPages(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "machine error", http.StatusForbidden)
			})).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden || recorder.Body.String() != "machine error\n" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
				t.Fatalf("machine response changed: status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
	for _, test := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"validation HTML", "text/html; charset=utf-8", "<form>Keep the original validation message</form>", http.StatusBadRequest},
		{"redirect", "text/html; charset=utf-8", "redirect body", http.StatusSeeOther},
		{"live stream", "text/event-stream", "event: state\ndata: {\"status\":\"queued\"}\n\n", http.StatusOK},
		{"JSON error", "application/json", `{"error":"conflict"}`, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/jobs/1", nil)
			request.Header.Set("Accept", "text/html")
			server.browserErrorPages(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Location", "/jobs")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Fatal(err)
				}
				// Data is visible before the handler returns: streams are not buffered.
				if recorder.Body.String() != test.body || !recorder.Flushed {
					t.Fatal("ordinary response was buffered")
				}
			})).ServeHTTP(recorder, request)
			if recorder.Code != test.status || recorder.Body.String() != test.body || recorder.Header().Get("Content-Type") != test.contentType || recorder.Header().Get("Location") != "/jobs" {
				t.Fatalf("ordinary response changed: status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
}

func TestBrowserErrorFallbackCoversSecurityOwnershipAndRoutingFailures(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{Username: "error-page-member", Password: "a long member password"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := createImmediateWebBooking(t, fixture, member.ID, "private error profile", true)
	for _, test := range []struct {
		name, method, path, host, returnURL string
		status                              int
	}{
		{"CSRF rejected", http.MethodPost, "/account/password", "example.test", "/account", http.StatusForbidden},
		{"foreign profile", http.MethodGet, fmt.Sprintf("/profiles/%d", foreign.ID), "example.test", "/", http.StatusNotFound},
		{"missing job", http.MethodGet, "/jobs/999999", "example.test", "/jobs", http.StatusNotFound},
		{"unknown user", http.MethodGet, "/admin/users/999999", "example.test", "/admin/users", http.StatusNotFound},
		{"wrong method", http.MethodDelete, "/bookings", "example.test", "/bookings", http.StatusMethodNotAllowed},
		{"invalid host", http.MethodGet, "/login", "untrusted.invalid", "/login", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "http://example.test"+test.path, strings.NewReader(url.Values{"csrf_token": {"invalid"}}.Encode()))
			request.Host = test.host
			request.Header.Set("Accept", "text/html")
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", "http://example.test")
			for _, cookie := range cookies {
				request.AddCookie(cookie)
			}
			recorder := httptest.NewRecorder()
			fixture.handler.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != test.status || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, `href="`+test.returnURL+`"`) || !strings.Contains(body, "Lake Pass Bot") {
				t.Fatalf("browser failure status=%d headers=%v body=%q", recorder.Code, recorder.Header(), body)
			}
			if strings.Contains(body, foreign.Name) || strings.Contains(body, "invalid CSRF token") || strings.Contains(body, "invalid Host header") || strings.Contains(body, "synthetic-secret") {
				t.Fatalf("browser error exposed raw detail: %q", body)
			}
			if recorder.Header().Get("Content-Security-Policy") == "" || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("security headers missing: %v", recorder.Header())
			}
		})
	}
}
