// Package adminapi is the admin listener PostgREST runs next to the API when
// admin-server-port is set: GET /live and /ready for orchestrator probes,
// GET /schema_cache for the loaded cache, and GET /metrics in Prometheus text
// format. The endpoints, paths, and status codes mirror PostgREST v14's
// admin server (PostgREST.Admin): /live is 200 while the API listener accepts
// connections and 500 otherwise; /ready adds the backend and schema cache
// health and degrades to 503; any other path is 404 with an empty body.
//
// Spec 20 sketches a POST /schema_cache for an on-demand reload; upstream has
// no such endpoint (reload is SIGUSR1 or NOTIFY), so it is not served here.
// If the reload entry point lands it belongs next to the signal handler, not
// in this package's GET-only surface.
package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Server serves the admin endpoints. The health checks are injected by the
// command, which knows the API listener address and owns the backend; the
// zero value of any field degrades gracefully (a nil check reports healthy,
// a nil SchemaCache serves an empty body, matching upstream's "no cache yet").
type Server struct {
	// Live reports whether the API listener accepts connections. PostgREST
	// implements this as a TCP dial of its own socket; the command wires the
	// same here.
	Live func(ctx context.Context) error

	// Ready reports whether the backend connection and the schema cache are
	// usable. It is consulted in addition to Live.
	Ready func(ctx context.Context) error

	// SchemaCache returns the loaded schema cache rendered as JSON.
	SchemaCache func() ([]byte, error)

	// Metrics holds the counters rendered at /metrics.
	Metrics *Metrics
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/live":
		w.WriteHeader(s.liveStatus(r.Context()))
	case "/ready":
		w.WriteHeader(s.readyStatus(r.Context()))
	case "/schema_cache":
		body, err := s.schemaCacheJSON()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if s.Metrics != nil {
			w.Write([]byte(s.Metrics.Text()))
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *Server) liveStatus(ctx context.Context) int {
	if s.Live != nil && s.Live(ctx) != nil {
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

func (s *Server) readyStatus(ctx context.Context) int {
	if status := s.liveStatus(ctx); status != http.StatusOK {
		return status
	}
	if s.Ready != nil && s.Ready(ctx) != nil {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

func (s *Server) schemaCacheJSON() ([]byte, error) {
	if s.SchemaCache == nil {
		return nil, nil
	}
	return s.SchemaCache()
}

// Metrics is a small Prometheus-text registry covering what dbrest measures
// today: schema cache loads and the configured pool ceiling. The names follow
// PostgREST's metric names where the concept matches.
type Metrics struct {
	mu              sync.Mutex
	loads           map[string]int64 // by status label: SUCCESS / FAIL
	lastLoadSeconds float64
	poolMax         int
}

// NewMetrics builds a registry; poolMax is the db-pool setting.
func NewMetrics(poolMax int) *Metrics {
	return &Metrics{loads: map[string]int64{}, poolMax: poolMax}
}

// ObserveSchemaCacheLoad records one schema cache load attempt.
func (m *Metrics) ObserveSchemaCacheLoad(d time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := "SUCCESS"
	if err != nil {
		status = "FAIL"
	}
	m.loads[status]++
	if err == nil {
		m.lastLoadSeconds = d.Seconds()
	}
}

// Text renders the registry in the Prometheus text exposition format.
func (m *Metrics) Text() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP pgrst_schema_cache_query_time_seconds The query time in seconds of the last schema cache load\n")
	b.WriteString("# TYPE pgrst_schema_cache_query_time_seconds gauge\n")
	fmt.Fprintf(&b, "pgrst_schema_cache_query_time_seconds %g\n", m.lastLoadSeconds)
	b.WriteString("# HELP pgrst_schema_cache_loads_total The total number of schema cache loads\n")
	b.WriteString("# TYPE pgrst_schema_cache_loads_total counter\n")
	statuses := make([]string, 0, len(m.loads))
	for status := range m.loads {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Fprintf(&b, "pgrst_schema_cache_loads_total{status=%q} %d\n", status, m.loads[status])
	}
	b.WriteString("# HELP pgrst_db_pool_max Max pool connections\n")
	b.WriteString("# TYPE pgrst_db_pool_max gauge\n")
	fmt.Fprintf(&b, "pgrst_db_pool_max %d\n", m.poolMax)
	return b.String()
}
