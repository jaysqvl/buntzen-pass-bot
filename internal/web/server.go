package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
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

type Server struct {
	config       config.Config
	store        *store.Store
	engine       *engine.Engine
	renderer     *Renderer
	mux          *http.ServeMux
	loginMu      sync.Mutex
	passwordMu   sync.Mutex
	passwordBusy map[int64]struct{}
}

func NewServer(cfg config.Config, database *store.Store, runner *engine.Engine) (*Server, error) {
	if database == nil || runner == nil {
		return nil, errors.New("database and job engine are required")
	}
	renderer, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	server := &Server{
		config: cfg, store: database, engine: runner, renderer: renderer,
		mux: http.NewServeMux(), passwordBusy: make(map[int64]struct{}),
	}
	server.routes()
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.securityHeaders(s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/static", http.HandlerFunc(s.renderer.Static)).ServeHTTP(w, r)
	})
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("GET /setup", s.setupPage)
	s.mux.HandleFunc("POST /setup", s.setup)
	s.mux.HandleFunc("POST /logout", s.authenticated(s.logout))
	s.mux.HandleFunc("GET /account", s.authenticated(s.accountPage))
	s.mux.HandleFunc("POST /account/password", s.authenticated(s.accountPassword))

	s.mux.HandleFunc("GET /admin/users", s.authenticated(s.adminOnly(s.usersPage)))
	s.mux.HandleFunc("GET /admin/users/new", s.authenticated(s.adminOnly(s.userNewPage)))
	s.mux.HandleFunc("POST /admin/users/new", s.authenticated(s.adminOnly(s.userCreate)))
	s.mux.HandleFunc("GET /admin/users/{id}", s.authenticated(s.adminOnly(s.userEditPage)))
	s.mux.HandleFunc("POST /admin/users/{id}", s.authenticated(s.adminOnly(s.userUpdate)))
	s.mux.HandleFunc("POST /admin/users/{id}/password", s.authenticated(s.adminOnly(s.userResetPassword)))
	s.mux.HandleFunc("POST /admin/users/{id}/delete", s.authenticated(s.adminOnly(s.userDelete)))

	s.mux.HandleFunc("GET /{$}", s.authenticated(s.dashboard))
	s.mux.HandleFunc("GET /sources", s.authenticated(s.sources))
	s.mux.HandleFunc("GET /sources/new", s.authenticated(s.sourceNew))
	s.mux.HandleFunc("POST /sources/new", s.authenticated(s.sourceCreate))
	s.mux.HandleFunc("GET /sources/{id}", s.authenticated(s.sourceEdit))
	s.mux.HandleFunc("POST /sources/{id}", s.authenticated(s.sourceUpdate))
	s.mux.HandleFunc("POST /sources/{id}/health", s.authenticated(s.sourceHealth))
	s.mux.HandleFunc("POST /sources/{id}/pair", s.authenticated(s.sourcePair))

	s.mux.HandleFunc("GET /profiles", s.authenticated(s.profiles))
	s.mux.HandleFunc("GET /profiles/new", s.authenticated(s.profileNew))
	s.mux.HandleFunc("POST /profiles/new", s.authenticated(s.profileCreate))
	s.mux.HandleFunc("GET /profiles/{id}", s.authenticated(s.profileEdit))
	s.mux.HandleFunc("POST /profiles/{id}", s.authenticated(s.profileUpdate))

	s.mux.HandleFunc("GET /bookings", s.authenticated(s.bookings))
	s.mux.HandleFunc("GET /bookings/new", s.authenticated(s.bookingNew))
	s.mux.HandleFunc("POST /bookings/new", s.authenticated(s.bookingCreate))
	s.mux.HandleFunc("GET /bookings/{id}", s.authenticated(s.bookingEdit))
	s.mux.HandleFunc("POST /bookings/{id}", s.authenticated(s.bookingUpdate))
	s.mux.HandleFunc("POST /bookings/{id}/run", s.authenticated(s.bookingRun))

	s.mux.HandleFunc("GET /jobs", s.authenticated(s.jobs))
	s.mux.HandleFunc("GET /jobs/{id}", s.authenticated(s.job))
	s.mux.HandleFunc("GET /jobs/{id}/events", s.authenticated(s.jobEvents))
	s.mux.HandleFunc("POST /jobs/{id}/decision", s.authenticated(s.jobDecision))
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
	host, err := config.CanonicalHost(value)
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
	origin, err := config.CanonicalOrigin(r.Header.Get("Origin"))
	if err != nil {
		return false
	}
	for _, scheme := range []string{"http", "https"} {
		requestOrigin, err := config.CanonicalOrigin(scheme + "://" + r.Host)
		if err == nil && origin == requestOrigin {
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
	origin, err := config.CanonicalOrigin(r.Header.Get("Origin"))
	if err != nil {
		return false
	}
	for _, allowed := range s.config.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func rejectCrossOrigin(w http.ResponseWriter, r *http.Request) {
	origin := "<missing>"
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		if canonical, err := config.CanonicalOrigin(r.Header.Get("Origin")); err == nil {
			origin = canonical
		} else {
			origin = "<invalid>"
		}
	}
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	switch fetchSite {
	case "", "same-origin", "same-site", "cross-site", "none":
	default:
		fetchSite = "<invalid>"
	}
	log.Printf("cross-origin request rejected host=%q origin=%q sec_fetch_site=%q", r.Host, origin, fetchSite)
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

func base(r *http.Request, title string) BaseData {
	session := requestAuth(r)
	user := session.Authenticated.User
	return BaseData{Title: title, Authenticated: user.ID > 0, Username: user.Username, IsAdmin: user.Role == model.RoleAdmin, CSRFToken: session.CSRFToken, CurrentPath: r.URL.Path, Flash: flashFor(r.URL.Query().Get("ok"))}
}

func flashFor(value string) *Flash {
	messages := map[string]string{
		"created": "Saved successfully.", "updated": "Changes saved.", "queued": "Job queued.",
		"healthy": "Provider authentication succeeded.", "cancelled": "Cancellation requested.", "decided": "Decision sent to the waiting browser.",
		"setup": "Administrator account created.", "user-created": "User account created.",
		"user-updated": "User access updated.", "user-password": "Temporary password set and existing sessions revoked.",
		"user-deleted":         "Member account and database records were deleted; managed local files were reconciled.",
		"user-deleted-cleanup": "Member account and database records were deleted; managed local-file cleanup will retry automatically.",
	}
	if message := messages[value]; message != "" {
		return &Flash{Kind: "success", Message: message}
	}
	return nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

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

type accountPageData struct {
	BaseData
	Error            string
	PasswordRequired bool
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, "")
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request) {
	password := r.Form.Get("new_password")
	if password != r.Form.Get("password_confirm") {
		s.renderAccount(w, r, "The new passwords do not match.")
		return
	}
	userID := requestAuth(r).Authenticated.User.ID
	release := s.tryPasswordChange(userID)
	if release == nil {
		http.Error(w, "another password change is already running for this account", http.StatusTooManyRequests)
		return
	}
	defer release()
	rateKey := loginRateKey("password-change", strconv.FormatInt(userID, 10))
	allowed, _, err := s.store.LoginRateLimit(r.Context(), rateKey, time.Now().UTC(), loginWindow, passwordLimit)
	if err != nil {
		s.internal(w)
		return
	}
	if !allowed {
		http.Error(w, "too many password attempts; wait before trying again", http.StatusTooManyRequests)
		return
	}
	changed, err := s.store.ChangeUserPassword(r.Context(), userID, r.Form.Get("current_password"), password)
	if recordErr := s.store.RecordLoginAttempt(r.Context(), rateKey, err == nil && changed); recordErr != nil {
		s.internal(w)
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrPasswordUnchanged) {
			s.renderAccount(w, r, "Choose a new password that differs from the temporary or current password.")
			return
		}
		s.renderAccount(w, r, accountFormError(err))
		return
	}
	if !changed {
		s.renderAccount(w, r, "The current password was not accepted.")
		return
	}
	clearAuthCookies(w)
	http.Redirect(w, r, "/login?ok=password-changed", http.StatusSeeOther)
}

func (s *Server) tryPasswordChange(userID int64) func() {
	s.passwordMu.Lock()
	defer s.passwordMu.Unlock()
	if userID <= 0 {
		return nil
	}
	if _, busy := s.passwordBusy[userID]; busy {
		return nil
	}
	s.passwordBusy[userID] = struct{}{}
	return func() {
		s.passwordMu.Lock()
		delete(s.passwordBusy, userID)
		s.passwordMu.Unlock()
	}
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, formError string) {
	user := requestAuth(r).Authenticated.User
	s.render(w, formStatus(formError), "account", accountPageData{
		BaseData:         base(r, "Account"),
		Error:            formError,
		PasswordRequired: user.MustChangePassword || r.URL.Query().Get("password") == "required",
	})
}

type userRow struct {
	ID                                                               int64
	Username, Role, Status, StatusClass, PasswordState, CreatedLabel string
	Editable                                                         bool
}

type usersPageData struct {
	BaseData
	Users []userRow
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	data := usersPageData{BaseData: base(r, "Users")}
	for _, user := range users {
		status, statusClass := "Disabled", ""
		if user.Status == model.UserActive {
			status, statusClass = "Active", "ok"
		}
		passwordState := "Current"
		if user.MustChangePassword {
			passwordState = "Change required"
		}
		role := "Member"
		if user.Role == model.RoleAdmin {
			role = "Permanent admin"
		}
		data.Users = append(data.Users, userRow{
			ID:            user.ID,
			Username:      user.Username,
			Role:          role,
			Status:        status,
			StatusClass:   statusClass,
			PasswordState: passwordState,
			CreatedLabel:  user.CreatedAt.Local().Format("Jan 2, 2006"),
			Editable:      user.Role == model.RoleMember,
		})
	}
	s.render(w, http.StatusOK, "users", data)
}

type userPageData struct {
	BaseData
	Heading, Description, Error, FormUsername string
	UserID                                    int64
	Creating, Enabled, DeleteAllowed          bool
}

func (s *Server) userNewPage(w http.ResponseWriter, r *http.Request) {
	s.renderUserNew(w, r, "", "")
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request) {
	username, password := strings.TrimSpace(r.Form.Get("username")), r.Form.Get("password")
	if password != r.Form.Get("password_confirm") {
		s.renderUserNew(w, r, username, "The passwords do not match.")
		return
	}
	_, err := s.store.CreateMember(r.Context(), store.CreateUserInput{Username: username, Password: password, MustChangePassword: true})
	if err != nil {
		s.renderUserNew(w, r, username, accountFormError(err))
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-created", http.StatusSeeOther)
}

func (s *Server) renderUserNew(w http.ResponseWriter, r *http.Request, username, formError string) {
	s.render(w, formStatus(formError), "user", userPageData{
		BaseData:     base(r, "New user"),
		Heading:      "New user",
		Description:  "Create a regular member account. Administrator privileges cannot be assigned here.",
		Error:        formError,
		FormUsername: username,
		Creating:     true,
	})
}

func (s *Server) userEditPage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	user, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if user.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator is managed from Account", http.StatusForbidden)
		return
	}
	s.renderUserEdit(w, r, user.ID, user.Username, user.Status == model.UserActive, "")
}

func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if current.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator cannot be modified here", http.StatusForbidden)
		return
	}
	status := model.UserDisabled
	if checked(r, "enabled") {
		status = model.UserActive
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	_, err = s.store.UpdateUser(r.Context(), id, store.UserUpdateInput{Username: username, Status: status})
	if err != nil {
		s.renderUserEdit(w, r, id, username, status == model.UserActive, accountFormError(err))
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-updated", http.StatusSeeOther)
}

func (s *Server) userResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if current.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator cannot be reset here", http.StatusForbidden)
		return
	}
	password := r.Form.Get("password")
	if password != r.Form.Get("password_confirm") {
		s.renderUserEdit(w, r, id, current.Username, current.Status == model.UserActive, "The passwords do not match.")
		return
	}
	if err := s.store.ResetUserPassword(r.Context(), id, password, true); err != nil {
		s.renderUserEdit(w, r, id, current.Username, current.Status == model.UserActive, accountFormError(err))
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-password", http.StatusSeeOther)
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if current.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator cannot be deleted", http.StatusForbidden)
		return
	}
	if err := s.store.DeleteMember(r.Context(), id, r.Form.Get("confirm_username")); err != nil {
		s.renderUserEdit(w, r, id, current.Username, current.Status == model.UserActive, memberDeleteFormError(err))
		return
	}
	if err := s.engine.ReconcileStorage(r.Context()); err != nil {
		log.Printf("post-deletion managed storage reconciliation failed; periodic maintenance will retry: %v", err)
		http.Redirect(w, r, "/admin/users?ok=user-deleted-cleanup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-deleted", http.StatusSeeOther)
}

func (s *Server) renderUserEdit(w http.ResponseWriter, r *http.Request, id int64, username string, enabled bool, formError string) {
	s.render(w, formStatus(formError), "user", userPageData{
		BaseData:      base(r, "Manage user"),
		Heading:       "Manage " + username,
		Description:   "Rename, enable, disable, reset, or permanently delete this member account.",
		Error:         formError,
		FormUsername:  username,
		UserID:        id,
		Enabled:       enabled,
		DeleteAllowed: !enabled,
	})
}

func memberDeleteFormError(err error) string {
	switch {
	case errors.Is(err, store.ErrMemberMustBeDisabled):
		return "Disable this member before deleting the account."
	case errors.Is(err, store.ErrMemberDeleteConfirmation):
		return "Type the member username exactly to confirm deletion."
	case errors.Is(err, store.ErrMemberHasActiveJobs):
		return "Wait for the member's active jobs to finish cancellation, then try again."
	default:
		return "The member account could not be deleted."
	}
}

func accountFormError(err error) string {
	if errors.Is(err, store.ErrConflict) {
		return "That username is already in use."
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "username") {
		return "Use a valid username that is not already in use."
	}
	if strings.Contains(message, "password") {
		return "Use a password that meets the minimum length requirement."
	}
	return "The submitted account details were not accepted."
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

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	if err := s.renderer.Render(w, status, name, data); err != nil {
		log.Printf("render %s failed: %v", name, err)
	}
}

type dashboardData struct {
	BaseData
	Stats struct {
		Profiles  int
		Scheduled int
		Active    int
		Waiting   int
	}
	Jobs []jobRow
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	profiles, err := userStore.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	bookings, err := userStore.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	jobs, err := userStore.ListJobs(r.Context(), 10)
	if err != nil {
		s.internal(w)
		return
	}
	data := dashboardData{BaseData: base(r, "Dashboard")}
	data.Stats.Profiles = len(profiles)
	for _, booking := range bookings {
		if booking.Enabled && booking.ScheduleEnabled {
			data.Stats.Scheduled++
		}
	}
	for _, job := range jobs {
		if job.Status == model.JobRunning || job.Status == model.JobAwaitingApproval {
			data.Stats.Active++
		}
		if job.Status == model.JobAwaitingApproval {
			data.Stats.Waiting++
		}
	}
	data.Jobs = s.jobRows(r.Context(), userStore, jobs)
	s.render(w, http.StatusOK, "dashboard", data)
}

type jobRow struct {
	ID           int64
	ShortID      string
	ProfileName  string
	Command      string
	StatusLabel  string
	StatusClass  string
	CreatedLabel string
}

type jobsData struct {
	BaseData
	Jobs []jobRow
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	userStore := s.userStore(r)
	jobs, err := userStore.ListJobs(r.Context(), 200)
	if err != nil {
		s.internal(w)
		return
	}
	s.render(w, http.StatusOK, "jobs", jobsData{BaseData: base(r, "Jobs"), Jobs: s.jobRows(r.Context(), userStore, jobs)})
}

func (s *Server) jobRows(ctx context.Context, userStore store.UserStore, jobs []model.Job) []jobRow {
	profiles, _ := userStore.ListProfiles(ctx)
	names := make(map[int64]string, len(profiles))
	for _, profile := range profiles {
		names[profile.ID] = profile.Name
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, job := range jobs {
		rows = append(rows, jobRow{ID: job.ID, ShortID: fmt.Sprintf("#%06d", job.ID), ProfileName: names[job.ProfileID], Command: string(job.Command), StatusLabel: statusLabel(job.Status), StatusClass: statusClass(job.Status), CreatedLabel: job.CreatedAt.Local().Format("Jan 2, 15:04")})
	}
	return rows
}

type labelValue struct{ Label, Value string }
type jobView struct {
	ID               int64
	ShortID          string
	ProfileName      string
	Command          string
	StatusLabel      string
	StatusClass      string
	CreatedLabel     string
	Message          string
	AwaitingApproval bool
	CanCancel        bool
	Fields           []labelValue
}
type eventView struct{ Time, Type, Message string }
type jobData struct {
	BaseData
	Job    jobView
	Events []eventView
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userStore := s.userStore(r)
	job, err := userStore.GetJob(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	profile, _ := userStore.GetProfile(r.Context(), job.ProfileID)
	events, err := userStore.ListJobEvents(r.Context(), id, 0, 500)
	if err != nil {
		s.internal(w)
		return
	}
	view := jobView{ID: job.ID, ShortID: fmt.Sprintf("#%06d", job.ID), ProfileName: profile.Name, Command: string(job.Command), StatusLabel: statusLabel(job.Status), StatusClass: statusClass(job.Status), CreatedLabel: job.CreatedAt.Local().Format(time.RFC1123), Message: job.Message, AwaitingApproval: job.Status == model.JobAwaitingApproval, CanCancel: job.Status == model.JobQueued || job.Status == model.JobRunning || job.Status == model.JobAwaitingApproval}
	view.Fields = []labelValue{{"Mode", string(job.RunMode)}, {"Status", string(job.Status)}, {"Due", job.DueAt.Local().Format(time.RFC1123)}, {"Started", optionalTime(job.StartedAt)}, {"Finished", optionalTime(job.FinishedAt)}, {"Final confirmation", optionalTime(job.ConfirmationStartedAt)}}
	data := jobData{BaseData: base(r, "Job "+view.ShortID), Job: view}
	for _, event := range events {
		data.Events = append(data.Events, eventView{Time: event.CreatedAt.Local().Format("15:04:05"), Type: event.Kind, Message: event.Message})
	}
	s.render(w, http.StatusOK, "job", data)
}

func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userStore := s.userStore(r)
	if _, err := userStore.GetJob(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	sessionToken, err := r.Cookie(sessionCookie)
	if err != nil || sessionToken.Value == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	streamAuthorized := func() bool {
		_, err := s.store.GetSession(r.Context(), sessionToken.Value)
		return err == nil
	}
	expireStream := func() {
		writeSSE(w, "auth_expired", map[string]any{})
		flusher.Flush()
	}

	existing, _ := userStore.ListJobEvents(r.Context(), id, 0, 1000)
	var afterID int64
	if len(existing) > 0 {
		afterID = existing[len(existing)-1].ID
	}
	s.writeJobState(w, userStore, id)
	flusher.Flush()
	jobKey := strconv.FormatInt(id, 10)
	live, unsubscribe := s.engine.Hub().Subscribe(jobKey)
	defer unsubscribe()
	poll := time.NewTicker(2 * time.Second)
	keepalive := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-live:
			if !open {
				return
			}
			if !streamAuthorized() {
				expireStream()
				return
			}
			if event.Kind == "otp" || event.Kind == "pairing" {
				writeSSE(w, event.Kind, event.Data)
			} else {
				s.writeJobState(w, userStore, id)
			}
			flusher.Flush()
		case <-poll.C:
			if !streamAuthorized() {
				expireStream()
				return
			}
			events, err := userStore.ListJobEvents(r.Context(), id, afterID, 100)
			if err != nil {
				return
			}
			for _, event := range events {
				writeSSE(w, "job_event", map[string]any{"time": event.CreatedAt.Local().Format("15:04:05"), "type": event.Kind, "message": event.Message})
				afterID = event.ID
			}
			job, err := userStore.GetJob(r.Context(), id)
			if err != nil {
				return
			}
			s.writeJobState(w, userStore, id)
			flusher.Flush()
			if job.Status.Terminal() {
				return
			}
		case <-keepalive.C:
			if !streamAuthorized() {
				expireStream()
				return
			}
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) writeJobState(w http.ResponseWriter, userStore store.UserStore, id int64) {
	job, err := userStore.GetJob(context.Background(), id)
	if err != nil {
		return
	}
	writeSSE(w, "state", map[string]any{"message": job.Message, "label": statusLabel(job.Status), "class_name": statusClass(job.Status), "awaiting_approval": job.Status == model.JobAwaitingApproval, "terminal": job.Status.Terminal()})
}

func writeSSE(w http.ResponseWriter, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}

func (s *Server) jobDecision(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.userStore(r).GetJob(r.Context(), id); err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	decision := r.Form.Get("decision")
	userID := requestAuth(r).Authenticated.User.ID
	var err error
	switch decision {
	case "approve":
		err = s.engine.Decide(r.Context(), userID, id, model.DecisionApprove)
	case "cancel":
		err = s.engine.Decide(r.Context(), userID, id, model.DecisionCancel)
	case "cancel-job":
		err = s.engine.CancelJob(r.Context(), userID, id)
	case "pair":
		err = s.engine.ChoosePairing(r.Context(), userID, id, r.Form.Get("message_id"))
	default:
		http.Error(w, "unsupported decision", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "decision was no longer available", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid identifier", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func statusLabel(status model.JobStatus) string { return strings.ReplaceAll(string(status), "_", " ") }
func statusClass(status model.JobStatus) string {
	switch status {
	case model.JobSucceeded:
		return "ok"
	case model.JobQueued, model.JobRunning:
		return "active"
	case model.JobAwaitingApproval:
		return "warn"
	case model.JobFailed, model.JobOutcomeUnknown:
		return "error"
	default:
		return ""
	}
}

func optionalTime(value *time.Time) string {
	if value == nil {
		return "—"
	}
	return value.Local().Format(time.RFC1123)
}

func (s *Server) notFoundOrInternal(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.internal(w)
}

func (s *Server) internal(w http.ResponseWriter) {
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
