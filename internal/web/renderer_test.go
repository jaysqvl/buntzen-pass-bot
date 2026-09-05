package web

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedTemplatesAndStaticAssets(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	data := authPageData{BaseData: BaseData{Title: "Sign in", CSRFToken: "csrf"}, Username: "example-user"}
	if err := renderer.Render(recorder, 200, "login", data); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "Buntzen Bot") || !strings.Contains(body, "csrf") || !strings.Contains(body, "example-user") {
		t.Fatalf("unexpected login body: %s", body)
	}

	staticRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "http://example.test/htmx.min.js", nil)
	renderer.Static(staticRecorder, request)
	if staticRecorder.Code != http.StatusOK || staticRecorder.Body.Len() == 0 {
		t.Fatalf("embedded htmx response: status=%d bytes=%d", staticRecorder.Code, staticRecorder.Body.Len())
	}

	staticRecorder = httptest.NewRecorder()
	request = httptest.NewRequest("GET", "http://example.test/app.js", nil)
	renderer.Static(staticRecorder, request)
	if staticRecorder.Code != http.StatusOK || staticRecorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("embedded client response: status=%d headers=%v", staticRecorder.Code, staticRecorder.Header())
	}
}

func TestFailedTemplateDoesNotReturnPartialSuccessfulPage(t *testing.T) {
	broken := template.Must(template.New("broken").Option("missingkey=error").Parse(`{{define "base"}}private partial content{{.MissingField}}{{end}}`))
	server := &Server{renderer: &Renderer{pages: map[string]*template.Template{"broken": broken}}}
	recorder := httptest.NewRecorder()
	server.render(recorder, http.StatusOK, "broken", map[string]string{})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("failed render status = %d, want 500", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, "private partial content") || strings.Contains(body, "MissingField") {
		t.Fatalf("failed render exposed partial output or template details: %q", body)
	}
}

func TestRendererEscapesUserProvidedHTML(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	data := authPageData{BaseData: BaseData{Title: "Sign in"}, Username: `<script>alert("unsafe")</script>`}
	if err := renderer.Render(recorder, http.StatusOK, "login", data); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if strings.Contains(body, data.Username) || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("login page did not escape the supplied username")
	}
}
