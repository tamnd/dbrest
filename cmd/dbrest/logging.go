// Request logging, filtered by the log-level option the way PostgREST filters
// its own request log: crit logs no requests, error logs server failures
// (5xx), warn adds client failures (4xx), and info and debug log every
// request. The level is read per request so a SIGUSR2 config reload takes
// effect without a restart.
package main

import (
	"log"
	"net"
	"net/http"
	"time"
)

// shouldLog decides whether a response status is logged at the given level.
func shouldLog(level string, status int) bool {
	switch level {
	case "crit":
		return false
	case "error":
		return status >= 500
	case "warn":
		return status >= 400
	default: // info, debug
		return true
	}
}

// statusWriter records the response status for the log line.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush passes through so streaming responses keep working behind the logger.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLogger wraps the API handler with the filtered request log. level is
// consulted on every request, so it follows config reloads.
type requestLogger struct {
	next  http.Handler
	level func() string
	out   *log.Logger
}

func (l *requestLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	started := time.Now()
	l.next.ServeHTTP(sw, r)
	if !shouldLog(l.level(), sw.status) {
		return
	}
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	l.out.Printf("%s - %q %d - %s", remote, r.Method+" "+r.URL.RequestURI()+" "+r.Proto, sw.status, time.Since(started).Round(time.Microsecond))
}
