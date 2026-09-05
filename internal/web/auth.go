package web

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/auth"
)

type authPageData struct {
	BaseData
	Error    string
	Message  string
	Username string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	hasUsers, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	if !hasUsers {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, err := s.store.GetSession(r.Context(), cookie.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	token, err := auth.NewToken()
	if err != nil {
		http.Error(w, "could not create login form", http.StatusInternalServerError)
		return
	}
	setCookie(w, loginCSRFCookie, token, 10*time.Minute)
	message := ""
	if r.URL.Query().Get("ok") == "password-changed" {
		message = "Password changed. Sign in again."
	}
	data := authPageData{BaseData: BaseData{Title: "Sign in", CSRFToken: token}, Error: loginError(r.URL.Query().Get("error")), Message: message}
	s.render(w, http.StatusOK, "login", data)
}

func loginError(value string) string {
	switch value {
	case "invalid":
		return "The username or password was not accepted."
	case "limited":
		return "Too many attempts. Wait before trying again."
	default:
		return ""
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		rejectCrossOrigin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	cookie, err := r.Cookie(loginCSRFCookie)
	if err != nil || !constantEqual(cookie.Value, r.Form.Get("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	hasUsers, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	if !hasUsers {
		clearCookie(w, loginCSRFCookie)
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	normalizedUsername, normalizeErr := auth.NormalizeUsername(username)
	if normalizeErr != nil {
		normalizedUsername = "<invalid>"
	}
	ipKey := loginRateKey("ip", remoteIP(r))
	userKey := loginRateKey("ip-user", remoteIP(r), normalizedUsername)
	allowedIP, _, err := s.store.LoginRateLimit(r.Context(), ipKey, time.Now().UTC(), loginWindow, loginIPLimit)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	allowedUser, _, err := s.store.LoginRateLimit(r.Context(), userKey, time.Now().UTC(), loginWindow, loginUserLimit)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if !allowedIP || !allowedUser {
		http.Redirect(w, r, "/login?error=limited", http.StatusSeeOther)
		return
	}
	_, credentials, ok, err := s.store.AuthenticateAndCreateSession(
		r.Context(), username, r.Form.Get("password"), sessionLifetime,
	)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if err := s.store.RecordLoginAttempts(r.Context(), []string{ipKey, userKey}, ok); err != nil {
		if ok {
			_ = s.store.DeleteSession(r.Context(), credentials.Token)
		}
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}
	setCookie(w, sessionCookie, credentials.Token, sessionLifetime)
	setCookie(w, csrfCookie, credentials.CSRFToken, sessionLifetime)
	clearCookie(w, loginCSRFCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	hasUsers, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	if hasUsers {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token, err := auth.NewToken()
	if err != nil {
		http.Error(w, "could not create setup form", http.StatusInternalServerError)
		return
	}
	setCookie(w, loginCSRFCookie, token, 10*time.Minute)
	s.render(w, http.StatusOK, "setup", authPageData{BaseData: BaseData{Title: "First-run setup", CSRFToken: token}, Username: "admin"})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		rejectCrossOrigin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	cookie, err := r.Cookie(loginCSRFCookie)
	if err != nil || !constantEqual(cookie.Value, r.Form.Get("csrf_token")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	hasUsers, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	if hasUsers {
		clearCookie(w, loginCSRFCookie)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if s.config.SetupToken == "" {
		http.Error(w, "first-run setup is unavailable until the host provides a setup token", http.StatusServiceUnavailable)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	if !tokenEqual(s.config.SetupToken, r.Form.Get("setup_token")) {
		s.renderSetupError(w, username, "The one-time setup token was not accepted. Check the host logs and try again.")
		return
	}
	password := r.Form.Get("password")
	if password != r.Form.Get("password_confirm") {
		s.renderSetupError(w, username, "The passwords do not match.")
		return
	}
	_, err = s.store.SetupAdmin(r.Context(), username, password)
	if err != nil {
		hasUsers, checkErr := s.store.HasUsers(r.Context())
		if checkErr == nil && hasUsers {
			clearCookie(w, loginCSRFCookie)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.renderSetupError(w, username, accountFormError(err))
		return
	}
	_, credentials, authenticated, err := s.store.AuthenticateAndCreateSession(
		r.Context(), username, password, sessionLifetime,
	)
	if err != nil || !authenticated {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	setCookie(w, sessionCookie, credentials.Token, sessionLifetime)
	setCookie(w, csrfCookie, credentials.CSRFToken, sessionLifetime)
	clearCookie(w, loginCSRFCookie)
	http.Redirect(w, r, "/?ok=setup", http.StatusSeeOther)
}

func tokenEqual(left, right string) bool {
	return constantEqual(auth.HashToken(left), auth.HashToken(right))
}

func (s *Server) renderSetupError(w http.ResponseWriter, username, message string) {
	token, err := auth.NewToken()
	if err != nil {
		http.Error(w, "could not create setup form", http.StatusInternalServerError)
		return
	}
	setCookie(w, loginCSRFCookie, token, 10*time.Minute)
	s.render(w, http.StatusUnprocessableEntity, "setup", authPageData{BaseData: BaseData{Title: "First-run setup", CSRFToken: token}, Error: message, Username: username})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	clearAuthCookies(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func setCookie(w http.ResponseWriter, name, value string, lifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(lifetime.Seconds()), Expires: time.Now().Add(lifetime), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: false})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func clearAuthCookies(w http.ResponseWriter) {
	clearCookie(w, sessionCookie)
	clearCookie(w, csrfCookie)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func loginRateKey(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", digest[:])
}
