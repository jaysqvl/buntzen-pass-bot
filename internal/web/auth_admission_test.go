package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestPublicAuthRejectsExcessWorkWithoutQueueing(t *testing.T) {
	for _, target := range []string{"/login", "/setup"} {
		t.Run(target, func(t *testing.T) {
			f := newUninitializedWebFixture(t)
			if target == "/login" {
				f = newWebFixture(t)
			}
			cookie, _ := publicFormCookie(t, f, target)
			form := url.Values{"csrf_token": {cookie.Value}, "username": {"admin"}, "password": {"long-test-password"}, "password_confirm": {"long-test-password"}, "setup_token": {f.cfg.SetupToken}}
			f.server.loginMu.Lock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- serveForm(f, http.MethodPost, target, []*http.Cookie{cookie}, form) }()
			var w *httptest.ResponseRecorder
			select {
			case w = <-done:
				f.server.loginMu.Unlock()
			case <-time.After(time.Second):
				f.server.loginMu.Unlock()
				<-done
				t.Fatal("excess auth request queued behind another request")
			}
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
				t.Fatalf("overload response = %d %v", w.Code, w.Header())
			}
			w = serveForm(f, http.MethodPost, target, []*http.Cookie{cookie}, form)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("auth after capacity released = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestWeakBootstrapTokenRejectedOnlyBeforeSetup(t *testing.T) {
	f := newUninitializedWebFixture(t)
	f.cfg.SetupToken = "short"
	if _, err := NewServer(f.cfg, f.store, f.server.engine); err == nil {
		t.Fatal("short operator bootstrap token accepted")
	}
	if _, err := f.store.SetupAdmin(context.Background(), "owner", "long-test-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(f.cfg, f.store, f.server.engine); err != nil {
		t.Fatalf("obsolete setup token blocked existing installation: %v", err)
	}
}

func TestSetupFailureLimitsPersistAndCannotLockOutValidToken(t *testing.T) {
	f := newUninitializedWebFixture(t)
	csrf, _ := publicFormCookie(t, f, "/setup")
	form := url.Values{"csrf_token": {csrf.Value}, "setup_token": {"wrong"}, "username": {"owner"}, "password": {"long-test-password"}, "password_confirm": {"long-test-password"}}
	for i := 0; i < loginIPLimit; i++ {
		w := serveForm(f, http.MethodPost, "/setup", []*http.Cookie{csrf}, form)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attempt %d = %d", i, w.Code)
		}
	}
	server, err := NewServer(f.cfg, f.store, f.server.engine)
	if err != nil {
		t.Fatal(err)
	}
	f.server, f.handler = server, server.Handler()
	w := serveForm(f, http.MethodPost, "/setup", []*http.Cookie{csrf}, form)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("limited setup = %d", w.Code)
	}
	// Rotating IPs must not create unbounded persistent failure rows, and a
	// hostile anonymous visitor must not lock the legitimate operator out.
	for i := loginIPLimit; i < 100; i++ {
		if err := f.store.RecordLoginAttempt(context.Background(), loginRateKey("setup-global"), false); err != nil {
			t.Fatal(err)
		}
	}
	if f.server.admitInvalidSetupToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://example.test/setup", nil)) {
		t.Fatal("global setup budget ignored")
	}
	form.Set("setup_token", f.cfg.SetupToken)
	w = serveForm(f, http.MethodPost, "/setup", []*http.Cookie{csrf}, form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?ok=setup" {
		t.Fatalf("valid setup locked out: %d %s", w.Code, w.Body.String())
	}
}

func TestPublicModeRequiresCompletedBootstrap(t *testing.T) {
	f := newUninitializedWebFixture(t)
	f.cfg.PublicOrigin = "https://example.test"
	f.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	if _, err := NewServer(f.cfg, f.store, f.server.engine); err == nil {
		t.Fatal("public mode exposed uninitialized bootstrap")
	}
	if _, err := f.store.SetupAdmin(context.Background(), "owner", "long-test-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(f.cfg, f.store, f.server.engine); err != nil {
		t.Fatalf("initialized public mode rejected: %v", err)
	}
}
