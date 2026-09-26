package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthHandler serves the container probe endpoint: GET / and
// /healthz answer 200 "ok" while the daemon is running, anything else
// 404. It carries no state on purpose — it exists so orchestrators can
// probe the daemon without exec-ing the clow CLI inside the container.
func healthHandler() http.Handler {
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
	return mux
}

// listenHealth binds the health endpoint address. The caller owns the
// listener and the server.
func listenHealth(addr string) (net.Listener, *http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: health endpoint: %w", err)
	}
	srv := &http.Server{
		Handler:           healthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return ln, srv, nil
}
