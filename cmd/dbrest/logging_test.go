package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShouldLog pins the per-level filter to PostgREST's: crit logs nothing,
// error logs 5xx, warn adds 4xx, info and debug log everything.
func TestShouldLog(t *testing.T) {
	cases := []struct {
		level  string
		status int
		want   bool
	}{
		{"crit", 500, false},
		{"crit", 200, false},
		{"error", 500, true},
		{"error", 404, false},
		{"error", 200, false},
		{"warn", 500, true},
		{"warn", 404, true},
		{"warn", 200, false},
		{"info", 200, true},
		{"info", 404, true},
		{"debug", 200, true},
	}
	for _, tc := range cases {
		if got := shouldLog(tc.level, tc.status); got != tc.want {
			t.Errorf("shouldLog(%q, %d) = %v, want %v", tc.level, tc.status, got, tc.want)
		}
	}
}

// TestRequestLoggerFiltersAndFormats runs requests through the middleware at
// different levels and checks what reaches the log.
func TestRequestLoggerFiltersAndFormats(t *testing.T) {
	level := "error"
	var buf bytes.Buffer
	rl := &requestLogger{
		next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/boom":
				w.WriteHeader(http.StatusInternalServerError)
			case "/missing":
				w.WriteHeader(http.StatusNotFound)
			default:
				w.Write([]byte("ok")) // implicit 200
			}
		}),
		level: func() string { return level },
		out:   log.New(&buf, "", 0),
	}

	get := func(path string) {
		rec := httptest.NewRecorder()
		rl.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	get("/films")
	get("/missing")
	if buf.Len() != 0 {
		t.Errorf("level error logged a non-5xx request: %q", buf.String())
	}

	get("/boom")
	line := buf.String()
	if !strings.Contains(line, `"GET /boom HTTP/1.1" 500`) {
		t.Errorf("5xx line = %q, want method, path, and status", line)
	}

	buf.Reset()
	level = "warn"
	get("/missing")
	get("/films")
	if !strings.Contains(buf.String(), "404") || strings.Contains(buf.String(), "200") {
		t.Errorf("level warn: %q, want the 404 and not the 200", buf.String())
	}

	buf.Reset()
	level = "info"
	get("/films")
	if !strings.Contains(buf.String(), `"GET /films HTTP/1.1" 200`) {
		t.Errorf("level info missed a 200: %q", buf.String())
	}

	buf.Reset()
	level = "crit"
	get("/boom")
	if buf.Len() != 0 {
		t.Errorf("level crit logged: %q", buf.String())
	}
}
