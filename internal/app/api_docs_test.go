package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIDocsPublicAndEmbedded(t *testing.T) {
	for _, path := range []string{"/api/docs", "/api/openapi.json"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			w := httptest.NewRecorder()
			if !serveAPIDocs(w, httptest.NewRequest(method, path, nil)) {
				t.Fatal("docs not routed")
			}
			if method == "POST" {
				if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
					t.Fatalf("method response: %v", w)
				}
			} else if w.Code != 200 {
				t.Fatalf("docs: %d", w.Code)
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD returned a body")
			}
			if method == "GET" && path == "/api/openapi.json" && !json.Valid(w.Body.Bytes()) {
				t.Fatal("invalid schema JSON")
			}
			if method == "GET" && path == "/api/docs" && !strings.Contains(w.Body.String(), "Download OpenAPI JSON") {
				t.Fatal("missing reference page")
			}
		}
	}
	if serveAPIDocs(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/stats", nil)) {
		t.Fatal("docs intercepted management endpoint")
	}
}

func TestOpenAPIPathsMatchHandlers(t *testing.T) {
	var spec struct {
		OpenAPI    string                                `json:"openapi"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Security   []json.RawMessage                     `json:"security"`
		Components struct {
			SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPISchema, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI != "3.1.0" || len(spec.Paths) != 7 {
		t.Fatalf("unexpected schema: %s, %d paths", spec.OpenAPI, len(spec.Paths))
	}
	if len(spec.Security) != 0 || len(spec.Components.SecuritySchemes) != 0 {
		t.Fatal("management schema must not require client credentials")
	}
	a := testManagementAPI(t)
	for path, methods := range spec.Paths {
		for method := range methods {
			path = strings.ReplaceAll(path, "{id}", "1")
			r := httptest.NewRequest(strings.ToUpper(method), path, nil)
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code == 401 {
				t.Fatalf("%s %s unexpectedly requires client credentials", method, path)
			}
			if w.Code == 405 || (w.Code == 404 && !strings.Contains(path, "/jobs/")) {
				t.Fatalf("schema operation unsupported: %s %s (%d)", method, path, w.Code)
			}
		}
	}
}
