package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	netpprof "net/http/pprof"
	"time"
)

// healthHandler serves GET / and /healthz (200 "ok"), 404 elsewhere.
// pprof is an explicit opt-in — profiling endpoints leak internals.
func healthHandler(pprof bool) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
	mux.HandleFunc("/healthz", ok)
	// "/" is a catch-all pattern in ServeMux, so the root handler
	// answers only "/" exactly; other paths 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		ok(w, r)
	})
	if pprof {
		mux.HandleFunc("/debug/pprof/", netpprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", netpprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", netpprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", netpprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", netpprof.Trace)
	}
	return mux
}

func listenHealth(addr string, pprof bool) (net.Listener, *http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: health endpoint: %w", err)
	}
	srv := &http.Server{
		Handler:           healthHandler(pprof),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return ln, srv, nil
}
