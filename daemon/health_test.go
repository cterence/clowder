package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		{"no stats func: /stats 404s", false, "/stats", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			healthHandler(tt.pprof, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.code {
				t.Fatalf("GET %s: status = %d, want %d", tt.path, rec.Code, tt.code)
			}
			if tt.body != "" && rec.Body.String() != tt.body {
				t.Fatalf("GET %s: body = %q, want %q", tt.path, rec.Body.String(), tt.body)
			}
		})
	}
}

// TestHealthStatsEndpoint pins /stats: JSON snapshot from the provided
// func, 404 without one.
func TestHealthStatsEndpoint(t *testing.T) {
	want := HealthStats{Spool: 2, SpoolBytes: 4096, Outbox: 1, Transfers: 3}
	rec := httptest.NewRecorder()
	healthHandler(false, func() HealthStats { return want }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stats: status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got HealthStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parsing /stats body %q: %v", rec.Body.String(), err)
	}
	if got != want {
		t.Fatalf("/stats = %+v, want %+v", got, want)
	}
}

// freeLocalAddr reserves a loopback TCP port and gives it back.
func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func TestRunServesHealthEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := Init(dir, "healthcat"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	addr := freeLocalAddr(t)
	d, err := New(Config{
		Dir:        dir,
		HealthAddr: addr,
		RetryEvery: time.Second,
		PollEvery:  time.Second,
		Logf:       t.Logf,
	}, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, func() bool { return d.Me().Addr != "" }, "daemon to listen")

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("probing health endpoint: %v", err)
	}
	body := make([]byte, 8)
	n, _ := resp.Body.Read(body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if string(body[:n]) != "ok\n" {
		t.Fatalf("GET /healthz: body = %q, want %q", string(body[:n]), "ok\n")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after cancel")
	}
	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("health endpoint still listening after Run returned")
	}
}

func TestRunHealthEndpointBindError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := Init(dir, "bindcat"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupying a port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	d, err := New(Config{
		Dir:        dir,
		HealthAddr: ln.Addr().String(),
		Logf:       t.Logf,
	}, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("Run error = %v, want a health endpoint bind failure", err)
	}
}

func TestRunServesPprofOnHealth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := Init(dir, "profcat"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	addr := freeLocalAddr(t)
	d, err := New(Config{
		Dir:        dir,
		HealthAddr: addr,
		Pprof:      true,
		RetryEvery: time.Second,
		PollEvery:  time.Second,
		Logf:       t.Logf,
	}, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, func() bool { return d.Me().Addr != "" }, "daemon to listen")

	resp, err := http.Get("http://" + addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		t.Fatalf("fetching heap profile: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after cancel")
	}
}

func TestRunPprofRequiresHealthAddr(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := Init(dir, "pprofcat"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	d, err := New(Config{
		Dir:   dir,
		Pprof: true,
		Logf:  t.Logf,
	}, &LocalTransport{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("Run error = %v, want a pprof-without-health-addr error", err)
	}
}
