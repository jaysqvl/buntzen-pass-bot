package web

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/origin"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

const (
	sessionCookie   = "buntzen_session"
	csrfCookie      = "buntzen_csrf"
	loginCSRFCookie = "buntzen_login_csrf"
	sessionLifetime = 24 * time.Hour
	loginWindow     = 15 * time.Minute
	loginUserLimit  = 5
	loginIPLimit    = 10
	passwordLimit   = 5
	maxFormBody     = 64 << 10
)

type contextKey string

const sessionContextKey contextKey = "session"

type requestSession struct {
	Authenticated model.AuthenticatedSession
	CSRFToken     string
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if !s.hostAllowed(r.Host) {
			http.Error(w, "invalid Host header", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(value string) bool {
	host, err := origin.Host(value)
	if err != nil {
		return false
	}
	hostname := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		hostname = parsed
	}
	hostname = strings.Trim(hostname, "[]")
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	if address := net.ParseIP(hostname); address != nil && address.IsLoopback() {
		return true
	}
	for _, allowed := range s.config.AllowedHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

func sameOrigin(r *http.Request) bool {
	browserOrigin, err := origin.Canonical(r.Header.Get("Origin"))
	if err != nil {
		return false
	}
	for _, scheme := range []string{"http", "https"} {
		requestOrigin, err := origin.Canonical(scheme + "://" + r.Host)
		if err == nil && browserOrigin == requestOrigin {
			return true
		}
	}
	return false
}

// originAllowed retains the normal same-origin check and additionally allows
// explicit browser origins for a trusted reverse proxy that rewrites Host.
func (s *Server) originAllowed(r *http.Request) bool {
	// Some privacy-focused browsers and extensions omit Origin even for a
	// same-origin form POST. Sec-Fetch-Site is a forbidden browser header, so
	// page JavaScript cannot forge "same-origin". The CSRF token is still
	// checked independently after this gate.
	if strings.TrimSpace(r.Header.Get("Origin")) == "" && strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "same-origin") {
		return true
	}
	if sameOrigin(r) {
		return true
	}
	browserOrigin, err := origin.Canonical(r.Header.Get("Origin"))
	if err != nil {
		return false
	}
	for _, allowed := range s.config.AllowedOrigins {
		if browserOrigin == allowed {
			return true
		}
	}
	return false
}

func rejectCrossOrigin(w http.ResponseWriter, r *http.Request) {
	browserOrigin := "<missing>"
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		if canonical, err := origin.Canonical(r.Header.Get("Origin")); err == nil {
			browserOrigin = canonical
		} else {
			browserOrigin = "<invalid>"
		}
	}
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	switch fetchSite {
	case "", "same-origin", "same-site", "cross-site", "none":
	default:
		fetchSite = "<invalid>"
	}
	slog.Warn("cross-origin request rejected", "host", r.Host, "origin", browserOrigin, "sec_fetch_site", fetchSite)
	http.Error(w, "cross-origin request rejected", http.StatusForbidden)
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionToken, err := r.Cookie(sessionCookie)
		if err != nil || sessionToken.Value == "" {
			s.unauthorized(w, r)
			return
		}
		authenticated, err := s.store.GetSession(r.Context(), sessionToken.Value)
		if err != nil {
			clearAuthCookies(w)
			s.unauthorized(w, r)
			return
		}
		csrfValue, err := r.Cookie(csrfCookie)
		if err != nil || !store.ValidateCSRF(authenticated.Session, csrfValue.Value) {
			clearAuthCookies(w)
			s.unauthorized(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !s.originAllowed(r) {
				rejectCrossOrigin(w, r)
				return
			}
			if err := r.ParseForm(); err != nil || !constantEqual(r.Form.Get("csrf_token"), csrfValue.Value) || !store.ValidateCSRF(authenticated.Session, r.Form.Get("csrf_token")) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		_ = s.store.TouchSession(r.Context(), sessionToken.Value)
		ctx := context.WithValue(r.Context(), sessionContextKey, requestSession{Authenticated: authenticated, CSRFToken: csrfValue.Value})
		authenticatedRequest := r.WithContext(ctx)
		if authenticated.User.MustChangePassword && r.URL.Path != "/account" && r.URL.Path != "/account/password" && r.URL.Path != "/logout" {
			http.Redirect(w, authenticatedRequest, "/account?password=required", http.StatusSeeOther)
			return
		}
		next(w, authenticatedRequest)
	}
}

func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := requestAuth(r).Authenticated.User
		if user.Role != model.RoleAdmin || user.Status != model.UserActive {
			http.Error(w, "administrator access required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func constantEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || strings.HasSuffix(r.URL.Path, "/events") {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if hasUsers, err := s.store.HasUsers(r.Context()); err == nil && !hasUsers {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func requestAuth(r *http.Request) requestSession {
	value, _ := r.Context().Value(sessionContextKey).(requestSession)
	return value
}

func (s *Server) userStore(r *http.Request) store.UserStore {
	return s.store.ForUser(requestAuth(r).Authenticated.User.ID)
}
