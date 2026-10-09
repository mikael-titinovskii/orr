package app

import (
	_ "embed"
	"net/http"
)

//go:embed api_docs/openapi.json
var openAPISchema []byte

//go:embed api_docs/index.html
var apiDocsHTML []byte

// serveAPIDocs exposes static documentation bundled with the local API.
func serveAPIDocs(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/docs" && r.URL.Path != "/api/openapi.json" {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, 405, "method not allowed")
		return true
	}
	data := openAPISchema
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/docs" {
		data = apiDocsHTML
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
	return true
}
