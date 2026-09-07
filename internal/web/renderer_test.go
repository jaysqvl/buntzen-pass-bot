package web

import (
	"crypto/sha256"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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

func TestPageAssetURLsMatchTheEmbeddedContent(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := renderer.Render(recorder, http.StatusOK, "login", authPageData{}); err != nil {
		t.Fatal(err)
	}
	links := regexp.MustCompile(`(?:src|href)="(/static/[^\"]+)"`).FindAllStringSubmatch(recorder.Body.String(), -1)
	if len(links) != 4 {
		t.Fatalf("expected favicon, stylesheet and both scripts, got %v", links)
	}
	for _, link := range links {
		assetURL, err := url.Parse(link[1])
		if err != nil {
			t.Fatal(err)
		}
		assetURL.Path = strings.TrimPrefix(assetURL.Path, "/static")
		response := httptest.NewRecorder()
		renderer.Static(response, httptest.NewRequest(http.MethodGet, assetURL.String(), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("asset %s status=%d", link[1], response.Code)
		}
		digest := sha256.Sum256(response.Body.Bytes())
		if version := assetURL.Query().Get("v"); version != fmt.Sprintf("%x", digest[:8]) {
			t.Fatalf("asset %s version does not match its content: %q", assetURL.Path, version)
		}
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

func TestBuildIdentityAppearsOnSignedInSignedOutAndErrorPages(t *testing.T) {
	const revision = "1234567890abcdef1234567890abcdef12345678"
	for _, build := range []struct{ version, revision, label string }{
		{"1.2.3", revision, "v1.2.3"},
		{"dev", "", "Development build"},
	} {
		renderer, err := newRenderer(build.version, build.revision)
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range []struct {
			name string
			data any
		}{
			{"login", authPageData{}},
			{"jobs", jobsData{BaseData: BaseData{Authenticated: true, Username: "member"}}},
			{"error", browserErrorPage{BaseData: BaseData{Title: "Page not found"}, ReturnURL: "/", ReturnLabel: "Back to Setup"}},
		} {
			t.Run(build.version+"/"+page.name, func(t *testing.T) {
				response := httptest.NewRecorder()
				if err := renderer.Render(response, http.StatusOK, page.name, page.data); err != nil {
					t.Fatal(err)
				}
				body := response.Body.String()
				if strings.Count(body, `id="build-info"`) != 1 || !strings.Contains(body, ">"+build.label+"<") {
					t.Fatalf("missing or duplicated build identity on %s", page.name)
				}
				if build.version == "dev" {
					if strings.Contains(body, "/releases/tag/") || strings.Contains(body, "/commit/") {
						t.Fatal("development build claimed a release or commit")
					}
					return
				}
				for _, expected := range []string{
					`href="https://github.com/jaysqvl/buntzen-pass-bot/releases/tag/buntzen-pass-bot-v1.2.3"`,
					`href="https://github.com/jaysqvl/buntzen-pass-bot/commit/` + revision + `"`,
					`title="Build ` + revision + `"`, ">Build 1234567<",
				} {
					if !strings.Contains(body, expected) {
						t.Fatalf("missing release identity %q", expected)
					}
				}
			})
		}
	}
}
