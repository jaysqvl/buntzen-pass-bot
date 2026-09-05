package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

// assets contains the complete browser UI. Keeping these files in the Go
// binary makes native and container deployments behave identically.
//
//go:embed assets/templates/*.html assets/static/*
var assets embed.FS

type Renderer struct {
	pages  map[string]*template.Template
	static http.Handler
}

type Flash struct {
	Kind    string
	Message string
}

// BaseData is embedded by every page model so shared layout fields remain
// explicit and template mistakes fail during tests instead of production.
type BaseData struct {
	Title         string
	Authenticated bool
	Username      string
	IsAdmin       bool
	CSRFToken     string
	CurrentPath   string
	Flash         *Flash
}

func NewRenderer() (*Renderer, error) {
	definitions := map[string][]string{
		"login":     {"assets/templates/base.html", "assets/templates/login.html"},
		"setup":     {"assets/templates/base.html", "assets/templates/setup.html"},
		"account":   {"assets/templates/base.html", "assets/templates/account.html"},
		"users":     {"assets/templates/base.html", "assets/templates/users.html"},
		"user":      {"assets/templates/base.html", "assets/templates/user.html"},
		"dashboard": {"assets/templates/base.html", "assets/templates/jobs_table.html", "assets/templates/dashboard.html"},
		"list":      {"assets/templates/base.html", "assets/templates/list.html"},
		"form":      {"assets/templates/base.html", "assets/templates/form.html"},
		"jobs":      {"assets/templates/base.html", "assets/templates/jobs_table.html", "assets/templates/jobs.html"},
		"job":       {"assets/templates/base.html", "assets/templates/job.html"},
	}
	pages := make(map[string]*template.Template, len(definitions))
	for name, files := range definitions {
		tmpl, err := template.New(name).Option("missingkey=error").ParseFS(assets, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", name, err)
		}
		pages[name] = tmpl
	}
	staticFS, err := fs.Sub(assets, "assets/static")
	if err != nil {
		return nil, fmt.Errorf("open embedded static files: %w", err)
	}
	return &Renderer{pages: pages, static: http.FileServer(http.FS(staticFS))}, nil
}

func (r *Renderer) Render(w http.ResponseWriter, status int, name string, data any) error {
	tmpl, ok := r.pages[name]
	if !ok {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return fmt.Errorf("unknown template %q", name)
	}
	// Finish evaluating templates before committing headers or partial HTML.
	// A missing page field must become a server error, not a truncated 200 page.
	var page bytes.Buffer
	if err := tmpl.ExecuteTemplate(&page, "base", data); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return fmt.Errorf("render %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(page.Bytes()); err != nil {
		return fmt.Errorf("write %s page: %w", name, err)
	}
	return nil
}

func (r *Renderer) Static(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if req.URL.Path == "/htmx.min.js" || req.URL.Path == "/app.js" || req.URL.Path == "/app.css" {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	r.static.ServeHTTP(w, req)
}
