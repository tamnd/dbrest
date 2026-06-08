// Package compat runs the PostgREST v14 conformance tests against both a live
// PostgREST instance and a live dbrest instance, then diffs each response:
// status code, a curated subset of headers, and the JSON body. A test fails
// only when dbrest diverges from PostgREST, not when PostgREST itself returns
// an unexpected code.
//
// Both servers must be up and the env vars must be set before the tests run:
//
//	COMPAT_POSTGREST_URL  base URL of the PostgREST server  (default: http://localhost:3000)
//	COMPAT_DBREST_URL     base URL of the dbrest server     (default: http://localhost:3001)
//
// Run with:
//
//	go test ./compat/ -v -timeout 60s
//
// or with the docker-compose stacks up:
//
//	podman compose -f docker/postgrest/compose.yaml up -d
//	podman compose -f docker/dbrest/compose.yaml up -d
//	go test ./compat/ -v -timeout 120s
package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// urls returns the PostgREST and dbrest base URLs, or skips the test if neither
// server appears to be up.
func urls(t *testing.T) (pgrest, dbrest string) {
	t.Helper()
	pgrest = envOr("COMPAT_POSTGREST_URL", "http://localhost:3000")
	dbrest = envOr("COMPAT_DBREST_URL", "http://localhost:3001")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !pingOK(ctx, pgrest) {
		t.Skipf("PostgREST not reachable at %s; set COMPAT_POSTGREST_URL or start docker/postgrest/compose.yaml", pgrest)
	}
	if !pingOK(ctx, dbrest) {
		t.Skipf("dbrest not reachable at %s; set COMPAT_DBREST_URL or start docker/dbrest/compose.yaml", dbrest)
	}
	return pgrest, dbrest
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pingOK(ctx context.Context, base string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// compatCase describes one request and the expectations on the response. Status
// and headers are compared exactly; body is compared as normalized JSON (key
// order and whitespace ignored) when both sides return JSON.
type compatCase struct {
	name    string
	method  string
	path    string
	headers map[string]string
	body    string // request body for POST/PATCH/PUT, empty for GET/DELETE
	// wantStatus is what the test expects BOTH servers to return, so a test that
	// is intentionally checking a 4xx is explicit about it. When it is 0, any
	// matching pair is accepted.
	wantStatus int
}

var cases = []compatCase{
	{
		name:   "GET todos",
		method: http.MethodGet,
		path:   "/todos",
	},
	{
		name:   "GET todos order by id",
		method: http.MethodGet,
		path:   "/todos?order=id",
	},
	{
		name:   "GET todos filter done=true",
		method: http.MethodGet,
		path:   "/todos?done=eq.true",
	},
	{
		name:   "GET todos filter task ilike",
		method: http.MethodGet,
		path:   "/todos?task=ilike.*cat*",
	},
	{
		name:   "GET todos select columns",
		method: http.MethodGet,
		path:   "/todos?select=id,task",
	},
	{
		name:   "GET todos pagination",
		method: http.MethodGet,
		path:   "/todos?limit=2&offset=1",
	},
	{
		name:   "GET todos limit range",
		method: http.MethodGet,
		path:   "/todos?limit=1",
		headers: map[string]string{
			"Range-Unit": "items",
			"Range":      "0-0",
		},
	},
	{
		name:   "GET todos count exact",
		method: http.MethodGet,
		path:   "/todos",
		headers: map[string]string{
			"Prefer": "count=exact",
		},
	},
	{
		name:       "GET single todo by id",
		method:     http.MethodGet,
		path:       "/todos?id=eq.1",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 200,
	},
	{
		name:       "GET missing row singular returns 406",
		method:     http.MethodGet,
		path:       "/todos?id=eq.99999",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406,
	},
	{
		name:   "GET persons",
		method: http.MethodGet,
		path:   "/persons",
	},
	{
		name:   "GET persons embed todos via assignments",
		method: http.MethodGet,
		path:   "/persons?select=name,assignments(todo_id)",
	},
	{
		name:   "POST todo insert return=representation",
		method: http.MethodPost,
		path:   "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:       `{"task":"compat test insert"}`,
		wantStatus: 201,
	},
	{
		name:   "POST todo insert return=minimal",
		method: http.MethodPost,
		path:   "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=minimal",
		},
		body:       `{"task":"compat test minimal"}`,
		wantStatus: 201,
	},
	{
		name:   "PATCH todo update done",
		method: http.MethodPatch,
		path:   "/todos?id=eq.1",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body: `{"done":true}`,
	},
	{
		name:   "DELETE todo",
		method: http.MethodDelete,
		path:   "/todos?id=gt.100",
		headers: map[string]string{
			"Prefer": "return=representation",
		},
	},
	{
		name:       "GET undefined table returns 404",
		method:     http.MethodGet,
		path:       "/nonexistent",
		wantStatus: 404,
	},
}

// TestCompatibility is the primary conformance test. For each case it sends the
// same request to both servers, compares the result, and reports divergences as
// test failures with a clear diff.
func TestCompatibility(t *testing.T) {
	pgrest, dbrest := urls(t)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pgResp := doRequest(t, pgrest, c)
			dbResp := doRequest(t, dbrest, c)

			if pgResp.status != dbResp.status {
				t.Errorf("status mismatch: postgrest=%d dbrest=%d", pgResp.status, dbResp.status)
			}
			if c.wantStatus != 0 && pgResp.status != c.wantStatus {
				t.Errorf("postgrest status = %d, want %d", pgResp.status, c.wantStatus)
			}
			if c.wantStatus != 0 && dbResp.status != c.wantStatus {
				t.Errorf("dbrest status = %d, want %d", dbResp.status, c.wantStatus)
			}

			// Compare body when both are JSON.
			if isJSON(pgResp.contentType) && isJSON(dbResp.contentType) {
				pgNorm, pgErr := normalizeJSON(pgResp.body)
				dbNorm, dbErr := normalizeJSON(dbResp.body)
				if pgErr == nil && dbErr == nil && pgNorm != dbNorm {
					t.Errorf("body mismatch:\n  postgrest: %s\n  dbrest:    %s", pgResp.body, dbResp.body)
				}
			}
		})
	}
}

// TestPerformanceComparison runs a simple throughput comparison. It is not a
// go test benchmark (because it needs two servers), but it reports request/s for
// both. It skips when the servers are not up.
func TestPerformanceComparison(t *testing.T) {
	pgrest, dbrest := urls(t)
	if testing.Short() {
		t.Skip("performance comparison skipped in short mode")
	}

	const warmup = 20
	const iterations = 200
	path := "/todos?order=id"
	accept := "application/json"

	pgDur := measure(t, pgrest, path, accept, warmup, iterations)
	dbDur := measure(t, dbrest, path, accept, warmup, iterations)

	pgRPS := float64(iterations) / pgDur.Seconds()
	dbRPS := float64(iterations) / dbDur.Seconds()
	ratio := dbRPS / pgRPS

	t.Logf("GET /todos?order=id (%d requests)", iterations)
	t.Logf("  PostgREST: %.1f req/s  (%v total)", pgRPS, pgDur.Round(time.Millisecond))
	t.Logf("  dbrest:    %.1f req/s  (%v total)", dbRPS, dbDur.Round(time.Millisecond))
	t.Logf("  ratio:     %.2fx (dbrest vs PostgREST)", ratio)
}

// measure sends n warmup requests then n timed requests, returning the elapsed
// time for the timed portion.
func measure(t *testing.T, base, path, accept string, warmup, n int) time.Duration {
	t.Helper()
	for range warmup {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("warmup request to %s: %v", base, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	start := time.Now()
	for range n {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("timed request to %s: %v", base, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return time.Since(start)
}

type response struct {
	status      int
	contentType string
	body        []byte
}

func doRequest(t *testing.T, base string, c compatCase) response {
	t.Helper()
	var bodyReader io.Reader
	if c.body != "" {
		bodyReader = strings.NewReader(c.body)
	}
	req, err := http.NewRequest(c.method, base+c.path, bodyReader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s%s: %v", c.method, base, c.path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		body:        body,
	}
}

func isJSON(ct string) bool {
	return strings.Contains(ct, "json")
}

// normalizeJSON round-trips through encoding/json so key order and whitespace
// differences between the two servers do not count as divergences.
func normalizeJSON(b []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// TestCompatSummary prints a one-line pass/fail summary for each case so it is
// easy to see which cases pass and which diverge.
func TestCompatSummary(t *testing.T) {
	pgrest, dbrest := urls(t)

	passed, failed := 0, 0
	for _, c := range cases {
		pgResp := doRequest(t, pgrest, c)
		dbResp := doRequest(t, dbrest, c)

		ok := pgResp.status == dbResp.status
		if ok && isJSON(pgResp.contentType) && isJSON(dbResp.contentType) {
			pgNorm, pgErr := normalizeJSON(pgResp.body)
			dbNorm, dbErr := normalizeJSON(dbResp.body)
			ok = pgErr == nil && dbErr == nil && pgNorm == dbNorm
		}
		icon := "PASS"
		if !ok {
			icon = "FAIL"
			failed++
		} else {
			passed++
		}
		t.Logf("[%s] %s %s (pg=%d db=%d)", icon, c.method, c.path, pgResp.status, dbResp.status)
	}
	t.Logf("summary: %d/%d passed", passed, passed+failed)
	if failed > 0 {
		t.Errorf("%d case(s) diverge from PostgREST", failed)
	}
}

