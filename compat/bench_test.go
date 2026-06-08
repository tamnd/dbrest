package compat

// TestBenchmarkSuite runs a structured throughput and concurrency benchmark
// against both the live PostgREST and dbrest servers. Each sub-test prints a
// table row so the full output can be pasted directly into the benchmark doc.
//
// Run:
//
//	go test ./compat/ -v -run TestBenchmarkSuite -count=1 -timeout 10m
//
// The test auto-skips when the servers are not up.

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// benchCase describes one benchmark scenario.
type benchCase struct {
	name    string
	method  string
	path    string
	body    string
	headers map[string]string
	warmup  int
	n       int // total requests for sequential run
}

// workloads covers reads, projections, embedding, counted reads, writes, and
// RPC so the suite exercises the full stack rather than a single hot path.
var workloads = []benchCase{
	// ── reads ────────────────────────────────────────────────────────────────
	{
		name: "GET /todos (simple read)",
		path: "/todos?order=id", warmup: 50, n: 500,
	},
	{
		name: "GET /todos projection",
		path: "/todos?select=id,task&order=id", warmup: 50, n: 500,
	},
	{
		name: "GET /todos count=exact",
		path: "/todos?order=id",
		headers: map[string]string{"Prefer": "count=exact"},
		warmup: 50, n: 500,
	},
	{
		name: "GET /todos?limit=1 singular",
		path: "/todos?id=eq.1",
		headers: map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		warmup: 30, n: 300,
	},
	{
		name: "GET /persons embed",
		path: "/persons?select=name,assignments(todo_id)", warmup: 30, n: 300,
	},
	// ── writes ───────────────────────────────────────────────────────────────
	{
		name:   "POST /todos (insert minimal)",
		method: http.MethodPost,
		path:   "/todos",
		body:   `{"task":"bench"}`,
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=minimal",
		},
		warmup: 20, n: 200,
	},
	{
		name:   "PATCH /todos?id=eq.1 (update minimal)",
		method: http.MethodPatch,
		path:   "/todos?id=eq.1",
		body:   `{"task":"bench-upd"}`,
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=minimal",
		},
		warmup: 20, n: 200,
	},
	// ── RPC ──────────────────────────────────────────────────────────────────
	{
		name:   "GET /rpc/get_todos_count (stable RPC)",
		method: http.MethodGet,
		path:   "/rpc/get_todos_count",
		warmup: 30, n: 300,
	},
}

// concLevels are the goroutine counts used for the concurrency sweep.
var concLevels = []int{1, 5, 10, 20, 50, 100, 200}

// TestBenchmarkSuite runs every workload sequentially, then runs a concurrency
// sweep on the most common read path (/todos simple read).
func TestBenchmarkSuite(t *testing.T) {
	pgrest, dbrest := urls(t)
	if testing.Short() {
		t.Skip("benchmark suite skipped in short mode")
	}

	t.Log("=== Sequential throughput ===")
	t.Log(fmtHeader())

	for _, wl := range workloads {
		t.Run("seq/"+wl.name, func(t *testing.T) {
			// Clean up write side-effects before measuring.
			if wl.method == http.MethodPost || wl.method == http.MethodPatch {
				resetTestDB(t, pgrest, dbrest)
			}

			method := wl.method
			if method == "" {
				method = http.MethodGet
			}

			pgDur := measureMethod(t, pgrest, method, wl.path, wl.body, wl.headers, wl.warmup, wl.n)
			dbDur := measureMethod(t, dbrest, method, wl.path, wl.body, wl.headers, wl.warmup, wl.n)

			pgRPS := float64(wl.n) / pgDur.Seconds()
			dbRPS := float64(wl.n) / dbDur.Seconds()
			t.Log(fmtRow(wl.name, pgRPS, dbRPS))
		})
	}

	t.Log("")
	t.Log("=== Concurrency sweep: GET /todos (simple read) ===")
	t.Log(fmtConcHeader())

	path := "/todos?order=id"
	total := 2000

	for _, conc := range concLevels {
		t.Run(fmt.Sprintf("conc/%d", conc), func(t *testing.T) {
			pgDur := measureConcurrentMethod(t, pgrest, http.MethodGet, path, "", nil, conc, total)
			dbDur := measureConcurrentMethod(t, dbrest, http.MethodGet, path, "", nil, conc, total)
			pgRPS := float64(total) / pgDur.Seconds()
			dbRPS := float64(total) / dbDur.Seconds()
			t.Log(fmtConcRow(conc, pgRPS, dbRPS))
		})
	}

	t.Log("")
	t.Log("=== Concurrency sweep: POST /todos (insert minimal) ===")
	t.Log(fmtConcHeader())

	for _, conc := range []int{1, 5, 10, 20, 50} {
		postTotal := 500
		t.Run(fmt.Sprintf("conc-write/%d", conc), func(t *testing.T) {
			resetTestDB(t, pgrest, dbrest)
			pgDur := measureConcurrentMethod(t, pgrest, http.MethodPost, "/todos",
				`{"task":"bench"}`,
				map[string]string{"Content-Type": "application/json", "Prefer": "return=minimal"},
				conc, postTotal)
			resetTestDB(t, pgrest, dbrest)
			dbDur := measureConcurrentMethod(t, dbrest, http.MethodPost, "/todos",
				`{"task":"bench"}`,
				map[string]string{"Content-Type": "application/json", "Prefer": "return=minimal"},
				conc, postTotal)
			pgRPS := float64(postTotal) / pgDur.Seconds()
			dbRPS := float64(postTotal) / dbDur.Seconds()
			t.Log(fmtConcRow(conc, pgRPS, dbRPS))
		})
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func measureMethod(t *testing.T, base, method, path, body string, headers map[string]string, warmup, n int) time.Duration {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	send := func() {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, base+path, r)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	for range warmup {
		send()
	}
	start := time.Now()
	for range n {
		send()
	}
	return time.Since(start)
}

func measureConcurrentMethod(t *testing.T, base, method, path, body string, headers map[string]string, concurrency, total int) time.Duration {
	t.Helper()
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: concurrency + 4,
		},
	}
	type void struct{}
	work := make(chan void, total)
	for range total {
		work <- void{}
	}
	close(work)

	done := make(chan time.Duration, concurrency)
	start := time.Now()
	for range concurrency {
		go func() {
			for range work {
				var r io.Reader
				if body != "" {
					r = strings.NewReader(body)
				}
				req, _ := http.NewRequest(method, base+path, r)
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			done <- time.Since(start)
		}()
	}
	var last time.Duration
	for range concurrency {
		if d := <-done; d > last {
			last = d
		}
	}
	return last
}

// ── table formatting ──────────────────────────────────────────────────────────

func fmtHeader() string {
	return fmt.Sprintf("%-42s  %9s  %9s  %6s", "Scenario", "PostgREST", "dbrest", "Ratio")
}

func fmtRow(name string, pgRPS, dbRPS float64) string {
	ratio := dbRPS / pgRPS
	return fmt.Sprintf("%-42s  %6.0f r/s  %6.0f r/s  %.2fx", name, pgRPS, dbRPS, ratio)
}

func fmtConcHeader() string {
	return fmt.Sprintf("%-8s  %9s  %9s  %6s", "Concurr.", "PostgREST", "dbrest", "Ratio")
}

func fmtConcRow(conc int, pgRPS, dbRPS float64) string {
	ratio := dbRPS / pgRPS
	return fmt.Sprintf("%-8d  %6.0f r/s  %6.0f r/s  %.2fx", conc, pgRPS, dbRPS, ratio)
}
