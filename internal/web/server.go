package web

import (
	"context"
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
)

type contextKey string

const sessionContextKey contextKey = "session"

type requestSession struct {
	Authenticated model.AuthenticatedSession
	CSRFToken     string
}

type Server struct {
	config   config.Config
	store    *store.Store
	engine   *engine.Engine
	renderer *Renderer
	mux      *http.ServeMux
	loginMu  sync.Mutex
}

func NewServer(cfg config.Config, database *store.Store, runner *engine.Engine) (*Server, error) {
	if database == nil || runner == nil {
		return nil, errors.New("database and job engine are required")
	}
	renderer, err := NewRenderer()
	if err != nil {
		return nil, err
	}
	server := &Server{config: cfg, store: database, engine: runner, renderer: renderer, mux: http.NewServeMux()}
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
	s.mux.HandleFunc("POST /logout", s.authenticated(s.logout))

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
		if !validHost(r.Host) {
			http.Error(w, "invalid Host header", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, ` /\\@`) {
		return false
	}
	name := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		name = parsed
	} else if strings.Count(host, ":") > 1 && !strings.HasPrefix(host, "[") {
		return false
	}
	name = strings.Trim(name, "[]")
	return name != ""
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
		next(w, r.WithContext(ctx))
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
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func requestAuth(r *http.Request) requestSession {
	value, _ := r.Context().Value(sessionContextKey).(requestSession)
	return value
}

func base(r *http.Request, title string) BaseData {
	session := requestAuth(r)
	return BaseData{Title: title, Authenticated: session.Authenticated.Admin.ID > 0, CSRFToken: session.CSRFToken, CurrentPath: r.URL.Path, Flash: flashFor(r.URL.Query().Get("ok"))}
}

func flashFor(value string) *Flash {
	messages := map[string]string{
		"created": "Saved successfully.", "updated": "Changes saved.", "queued": "Job queued.",
		"healthy": "Provider authentication succeeded.", "cancelled": "Cancellation requested.", "decided": "Decision sent to the waiting browser.",
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

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
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
	data := struct {
		BaseData
		Error string
	}{BaseData: BaseData{Title: "Sign in", CSRFToken: token}, Error: loginError(r.URL.Query().Get("error"))}
	s.render(w, http.StatusOK, "login", data)
}

func loginError(value string) string {
	switch value {
	case "invalid":
		return "The password was not accepted."
	case "limited":
		return "Too many attempts. Wait before trying again."
	default:
		return ""
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
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
	rateKey := remoteIP(r) + ":" + s.config.AdminUsername
	allowed, _, err := s.store.LoginRateLimit(r.Context(), rateKey, time.Now().UTC(), 15*time.Minute, 5)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Redirect(w, r, "/login?error=limited", http.StatusSeeOther)
		return
	}
	admin, ok, err := s.store.AuthenticateAdmin(r.Context(), s.config.AdminUsername, r.Form.Get("password"))
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if err := s.store.RecordLoginAttempt(r.Context(), rateKey, ok); err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}
	credentials, err := s.store.NewSession(r.Context(), admin.ID, sessionLifetime)
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	setCookie(w, sessionCookie, credentials.Token, sessionLifetime)
	setCookie(w, csrfCookie, credentials.CSRFToken, sessionLifetime)
	clearCookie(w, loginCSRFCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	profiles, err := s.store.ListProfiles(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	bookings, err := s.store.ListBookingRequests(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), 10)
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
	data.Jobs = s.jobRows(r.Context(), jobs)
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
	jobs, err := s.store.ListJobs(r.Context(), 200)
	if err != nil {
		s.internal(w)
		return
	}
	s.render(w, http.StatusOK, "jobs", jobsData{BaseData: base(r, "Jobs"), Jobs: s.jobRows(r.Context(), jobs)})
}

func (s *Server) jobRows(ctx context.Context, jobs []model.Job) []jobRow {
	profiles, _ := s.store.ListProfiles(ctx)
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
	job, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	profile, _ := s.store.GetProfile(r.Context(), job.ProfileID)
	events, err := s.store.ListJobEvents(r.Context(), id, 0, 500)
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
	if _, err := s.store.GetJob(r.Context(), id); err != nil {
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

	existing, _ := s.store.ListJobEvents(r.Context(), id, 0, 1000)
	var afterID int64
	if len(existing) > 0 {
		afterID = existing[len(existing)-1].ID
	}
	s.writeJobState(w, id)
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
				s.writeJobState(w, id)
			}
			flusher.Flush()
		case <-poll.C:
			if !streamAuthorized() {
				expireStream()
				return
			}
			events, err := s.store.ListJobEvents(r.Context(), id, afterID, 100)
			if err != nil {
				return
			}
			for _, event := range events {
				writeSSE(w, "job_event", map[string]any{"time": event.CreatedAt.Local().Format("15:04:05"), "type": event.Kind, "message": event.Message})
				afterID = event.ID
			}
			job, err := s.store.GetJob(r.Context(), id)
			if err != nil {
				return
			}
			s.writeJobState(w, id)
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

func (s *Server) writeJobState(w http.ResponseWriter, id int64) {
	job, err := s.store.GetJob(context.Background(), id)
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
	decision := r.Form.Get("decision")
	var err error
	switch decision {
	case "approve":
		err = s.engine.Decide(r.Context(), id, model.DecisionApprove)
	case "cancel":
		err = s.engine.Decide(r.Context(), id, model.DecisionCancel)
	case "cancel-job":
		err = s.engine.CancelJob(r.Context(), id)
	case "pair":
		err = s.engine.ChoosePairing(id, r.Form.Get("message_id"))
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
