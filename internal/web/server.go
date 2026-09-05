package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

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

func (s *Server) Handler() http.Handler { return s.requestLogging(s.securityHeaders(s.mux)) }

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

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	if err := s.renderer.Render(w, status, name, data); err != nil {
		slog.Error("template render failed", "template", name, "error", err)
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid identifier", http.StatusBadRequest)
		return 0, false
	}
	return id, true
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
