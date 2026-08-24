package web

import (
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
	data := struct {
		BaseData
		Error string
	}{BaseData: BaseData{Title: "Sign in", CSRFToken: "csrf"}}
	if err := renderer.Render(recorder, 200, "login", data); err != nil {
		t.Fatal(err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "Buntzen Bot") || !strings.Contains(body, "csrf") {
		t.Fatalf("unexpected login body: %s", body)
	}

	staticRecorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "http://example.test/htmx.min.js", nil)
	renderer.Static(staticRecorder, request)
	if staticRecorder.Code != 200 || staticRecorder.Body.Len() < 1000 {
		t.Fatalf("embedded htmx response: status=%d bytes=%d", staticRecorder.Code, staticRecorder.Body.Len())
	}
}
