package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler(t *testing.T) {
	tests := []struct {
		name  string
		pprof bool
		path  string
		code  int
		body  string
	}{
		{"probe path", false, "/healthz", http.StatusOK, "ok\n"},
		{"root", false, "/", http.StatusOK, "ok\n"},
		{"unknown path", false, "/other", http.StatusNotFound, ""},
		{"pprof disabled", false, "/debug/pprof/", http.StatusNotFound, ""},
		{"pprof index", true, "/debug/pprof/", http.StatusOK, ""},
		{"pprof heap profile", true, "/debug/pprof/heap", http.StatusOK, ""},
		{"stats removed: /stats 404s", false, "/stats", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			healthHandler(tt.pprof).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.code {
				t.Fatalf("GET %s: status = %d, want %d", tt.path, rec.Code, tt.code)
			}
			if tt.body != "" && rec.Body.String() != tt.body {
				t.Fatalf("GET %s: body = %q, want %q", tt.path, rec.Body.String(), tt.body)
			}
		})
	}
}
