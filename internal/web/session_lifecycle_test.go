package web

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func sessionTestSQL(t *testing.T, f webFixture, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.cfg.AppDataDir, "buntzen.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestSessionTouchFailureDoesNotRunAuthenticatedHandler(t *testing.T) {
	f := newWebFixture(t)
	cookies := loginCookies(t, f)
	reached := false
	f.server.mux.HandleFunc("GET /session-test", f.server.authenticated(func(http.ResponseWriter, *http.Request) { reached = true }))
	sessionTestSQL(t, f, `CREATE TRIGGER reject_session_touch BEFORE UPDATE OF last_seen_at ON sessions BEGIN SELECT RAISE(FAIL, 'synthetic touch failure'); END`)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, authenticatedRequest(http.MethodGet, "http://example.test/session-test", cookies, nil))
	if reached || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("touch failure reached handler=%v, status=%d", reached, w.Code)
	}
	sessionTestSQL(t, f, `DROP TRIGGER reject_session_touch`)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, authenticatedRequest(http.MethodGet, "http://example.test/session-test", cookies, nil))
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("healthy session rejected: %d", w.Code)
	}
}

func TestLogoutDoesNotClaimSuccessWhenRevocationFails(t *testing.T) {
	f := newWebFixture(t)
	cookies := loginCookies(t, f)
	var token string
	for _, c := range cookies {
		if c.Name == sessionCookie {
			token = c.Value
		}
	}
	sessionTestSQL(t, f, `CREATE TRIGGER reject_session_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(FAIL, 'synthetic delete failure'); END`)
	form := url.Values{"csrf_token": {csrfFrom(cookies)}}
	w := serveForm(f, http.MethodPost, "/logout", cookies, form)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Location") != "" || len(w.Result().Cookies()) != 0 {
		t.Fatalf("failed logout claimed success: %d cookies=%d", w.Code, len(w.Result().Cookies()))
	}
	if _, err := f.store.GetSession(context.Background(), token); err != nil {
		t.Fatalf("test failed to preserve unrevokeable session: %v", err)
	}
	sessionTestSQL(t, f, `DROP TRIGGER reject_session_delete`)
	w = serveForm(f, http.MethodPost, "/logout", cookies, form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("logout after recovery = %d", w.Code)
	}
	if _, err := f.store.GetSession(context.Background(), token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("logout did not revoke: %v", err)
	}
}

func TestPublicModeRejectsRenamedPrivateSession(t *testing.T) {
	f := publicFixture(t)
	credentials, err := f.store.NewSession(context.Background(), f.admin.ID, sessionLifetime)
	if err != nil {
		t.Fatal(err)
	}
	r := publicRequest(http.MethodGet, "http://example.test/account")
	r.AddCookie(&http.Cookie{Name: "__Host-" + sessionCookie, Value: credentials.Token})
	r.AddCookie(&http.Cookie{Name: "__Host-" + csrfCookie, Value: credentials.CSRFToken})
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("renamed private session accepted: %d", w.Code)
	}
}

func publicLoginCookies(t *testing.T, f webFixture) []*http.Cookie {
	t.Helper()
	get := httptest.NewRecorder()
	f.handler.ServeHTTP(get, publicRequest(http.MethodGet, "http://example.test/login"))
	csrf := get.Result().Cookies()[0]
	r := publicRequest(http.MethodPost, "http://example.test/login")
	r.Header.Set("Origin", "https://example.test")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Body = io.NopCloser(strings.NewReader(url.Values{"username": {"admin"}, "password": {"long-test-password"}, "csrf_token": {csrf.Value}}.Encode()))
	r.AddCookie(csrf)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("public login=%d", w.Code)
	}
	return w.Result().Cookies()
}

func TestPublicSessionCannotMoveToAnotherOriginOrPrivateMode(t *testing.T) {
	f := publicFixture(t)
	cookies := publicLoginCookies(t, f)
	for _, tc := range []struct{ name, scope, host string }{
		{"issuing origin", "https://example.test", "example.test"},
		{"different origin", "https://different.test", "different.test"},
		{"private mode", "", "example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := f.cfg
			cfg.PublicOrigin = tc.scope
			if tc.scope == "" {
				cfg.TrustedProxies = nil
			}
			server, err := NewServer(cfg, f.store, f.server.engine)
			if err != nil {
				t.Fatal(err)
			}
			r := publicRequest(http.MethodGet, "http://"+tc.host+"/account")
			for _, c := range cookies {
				clone := *c
				if tc.scope == "" {
					clone.Name = strings.TrimPrefix(clone.Name, "__Host-")
				}
				r.AddCookie(&clone)
			}
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if tc.scope == f.cfg.PublicOrigin {
				if w.Code != http.StatusOK {
					t.Fatalf("same origin rejected: %d", w.Code)
				}
			} else if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
				t.Fatalf("cross-scope replay accepted: %d", w.Code)
			}
		})
	}
}

func TestRevocationDuringFormParsingCannotReachHandler(t *testing.T) {
	f := newWebFixture(t)
	cookies := loginCookies(t, f)
	var token string
	for _, c := range cookies {
		if c.Name == sessionCookie {
			token = c.Value
		}
	}
	reached := false
	f.server.mux.HandleFunc("POST /session-test", f.server.authenticated(func(http.ResponseWriter, *http.Request) { reached = true }))
	body := &revokeOnRead{Reader: strings.NewReader(url.Values{"csrf_token": {csrfFrom(cookies)}}.Encode()), revoke: func() {
		if err := f.store.DeleteSession(context.Background(), token); err != nil {
			t.Fatal(err)
		}
	}}
	r := httptest.NewRequest(http.MethodPost, "http://example.test/session-test", body)
	r.Header.Set("Origin", "http://example.test")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if reached || w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("revoked during parsing: reached=%v status=%d", reached, w.Code)
	}
}

type revokeOnRead struct {
	io.Reader
	revoke func()
}

func (r *revokeOnRead) Read(p []byte) (int, error) {
	if r.revoke != nil {
		r.revoke()
		r.revoke = nil
	}
	return r.Reader.Read(p)
}

func TestSessionReadFailureDoesNotImplyLogout(t *testing.T) {
	f := newWebFixture(t)
	cookies := loginCookies(t, f)
	sessionTestSQL(t, f, `ALTER TABLE sessions RENAME TO unavailable_sessions`)
	for _, target := range []struct{ method, path string }{{http.MethodPost, "/logout"}, {http.MethodGet, "/account"}, {http.MethodGet, "/login"}} {
		w := serveForm(f, target.method, target.path, cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
		if w.Code != http.StatusServiceUnavailable || len(w.Result().Cookies()) != 0 || w.Header().Get("Location") != "" {
			t.Errorf("%s session-read failure implied logout: %d cookies=%d", target.path, w.Code, len(w.Result().Cookies()))
		}
	}
	sessionTestSQL(t, f, `ALTER TABLE unavailable_sessions RENAME TO sessions`)
	w := serveForm(f, http.MethodPost, "/logout", cookies, url.Values{"csrf_token": {csrfFrom(cookies)}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("retry logout=%d", w.Code)
	}
}
