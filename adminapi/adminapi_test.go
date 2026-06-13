package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, s *Server, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

// TestLive covers both sides of the liveness probe: 200 while the API socket
// answers, 500 once it does not, matching the PostgREST admin server.
func TestLive(t *testing.T) {
	up := &Server{Live: func(context.Context) error { return nil }}
	if resp := get(t, up, "/live"); resp.StatusCode != http.StatusOK {
		t.Errorf("live up: status = %d, want 200", resp.StatusCode)
	}
	down := &Server{Live: func(context.Context) error { return errors.New("refused") }}
	if resp := get(t, down, "/live"); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("live down: status = %d, want 500", resp.StatusCode)
	}
}

// TestReady covers the three readiness answers: 500 when the API is not
// reachable, 503 when it is up but the backend is not usable, 200 otherwise.
func TestReady(t *testing.T) {
	ok := func(context.Context) error { return nil }
	bad := func(context.Context) error { return errors.New("down") }

	cases := []struct {
		name string
		srv  *Server
		want int
	}{
		{"loaded", &Server{Live: ok, Ready: ok}, http.StatusOK},
		{"backend pending", &Server{Live: ok, Ready: bad}, http.StatusServiceUnavailable},
		{"api unreachable", &Server{Live: bad, Ready: ok}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if resp := get(t, tc.srv, "/ready"); resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

// TestSchemaCache checks the dump is served as JSON, and that a failing dump
// degrades to 500 rather than half a body.
func TestSchemaCache(t *testing.T) {
	srv := &Server{SchemaCache: func() ([]byte, error) {
		return json.Marshal(map[string]any{"relations": []string{"films"}})
	}}
	resp := get(t, srv, "/schema_cache")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	broken := &Server{SchemaCache: func() ([]byte, error) { return nil, errors.New("nope") }}
	if resp := get(t, broken, "/schema_cache"); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("broken dump: status = %d, want 500", resp.StatusCode)
	}
}

// TestMetrics checks the Prometheus text rendering: content type, the load
// counters by status, the last query time, and the pool gauge.
func TestMetrics(t *testing.T) {
	m := NewMetrics(10)
	m.ObserveSchemaCacheLoad(250*time.Millisecond, nil)
	m.ObserveSchemaCacheLoad(0, errors.New("introspect failed"))
	srv := &Server{Metrics: m}

	resp := get(t, srv, "/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		`pgrst_schema_cache_loads_total{status="SUCCESS"} 1`,
		`pgrst_schema_cache_loads_total{status="FAIL"} 1`,
		"pgrst_schema_cache_query_time_seconds 0.25",
		"pgrst_db_pool_max 10",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n%s", want, body)
		}
	}
}

// TestUnknownPathIs404 checks the fall-through, including the root.
func TestUnknownPathIs404(t *testing.T) {
	srv := &Server{}
	for _, path := range []string{"/", "/config", "/live/extra"} {
		if resp := get(t, srv, path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestNilChecksDegradeGracefully checks the zero-value server: health reports
// up (nothing to check), the cache dump is empty, metrics body is empty.
func TestNilChecksDegradeGracefully(t *testing.T) {
	srv := &Server{}
	for _, path := range []string{"/live", "/ready", "/schema_cache", "/metrics"} {
		if resp := get(t, srv, path); resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, resp.StatusCode)
		}
	}
}
