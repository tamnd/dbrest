package compat

// TestLatencyBenchmark measures the throughput advantage of dbrest over
// PostgREST when there is realistic network latency between the application
// and PostgreSQL. On loopback both servers are bottlenecked by PostgreSQL
// itself; the session-setup batch optimisation only shows when each round trip
// to the database costs real time.
//
// Topology:
//
//	PostgREST (container :3010) → toxiproxy :5453 (+1ms downstream) → PG :5433
//	dbrest    (host      :3011) → toxiproxy :5464 (+1ms downstream) → PG :5434
//
// 1ms downstream latency adds ~1ms per network round trip (one-way).
// PostgREST sends ~7 serial statements to set up the session, then the query;
// dbrest folds all session statements into one pgx.Batch round trip.
//
// Run:
//
//	go test ./compat/ -v -run TestLatencyBenchmark -timeout 5m

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

const (
	toxiAPI             = "http://127.0.0.1:8474"
	proxyPGRESTListen   = "127.0.0.1:5453"
	proxyDBRestListen   = "127.0.0.1:5464"
	proxyPGRESTUpstream = "127.0.0.1:5433"
	proxyDBRestUpstream = "127.0.0.1:5434"
	latencyPGRESTPort   = "3010"
	latencyDBRestPort   = "3011"
	latencyMS           = 1 // one-way, ms
)

// TestLatencyBenchmark is the main entry point. It skips if toxiproxy-server
// is not installed or the underlying PostgreSQL instances are not up.
func TestLatencyBenchmark(t *testing.T) {
	if _, err := exec.LookPath("toxiproxy-server"); err != nil {
		t.Skip("toxiproxy-server not in PATH: brew install toxiproxy")
	}
	if !tcpReachable(proxyPGRESTUpstream) {
		t.Skip("PostgREST postgres not reachable on :5433; start docker/postgrest/compose.yaml")
	}
	if !tcpReachable(proxyDBRestUpstream) {
		t.Skip("dbrest postgres not reachable on :5434; start docker/dbrest/compose.yaml")
	}
	dbrestBin, err := exec.LookPath("dbrest")
	if err != nil {
		// fall back to build output
		dbrestBin = "/tmp/dbrest-latency"
		if _, serr := os.Stat(dbrestBin); serr != nil {
			t.Skip("dbrest binary not found; run: go build -o /tmp/dbrest-latency ./cmd/dbrest")
		}
	}

	// ── toxiproxy ────────────────────────────────────────────────────────────
	stopToxi := startToxiproxyServer(t)
	t.Cleanup(stopToxi)

	createToxiProxy(t, "pg-postgrest", proxyPGRESTListen, proxyPGRESTUpstream)
	createToxiProxy(t, "pg-dbrest", proxyDBRestListen, proxyDBRestUpstream)
	addLatencyToxic(t, "pg-postgrest", latencyMS)
	addLatencyToxic(t, "pg-dbrest", latencyMS)
	t.Logf("toxiproxy: +%dms one-way latency on both PG connections", latencyMS)

	// ── PostgREST (container) ────────────────────────────────────────────────
	pgrestURL := "http://127.0.0.1:" + latencyPGRESTPort
	stopPGREST := startPostgRESTContainer(t, latencyPGRESTPort,
		"postgres://authenticator:authenticator_pass@host.containers.internal:5453/postgres")
	t.Cleanup(stopPGREST)

	// ── dbrest (host process) ────────────────────────────────────────────────
	dbrestURL := "http://127.0.0.1:" + latencyDBRestPort
	stopDBRest := startDBRestProcess(t, dbrestBin, latencyDBRestPort,
		"postgres://authenticator:authenticator_pass@127.0.0.1:5464/postgres")
	t.Cleanup(stopDBRest)

	// ── wait for both ────────────────────────────────────────────────────────
	waitServerReady(t, pgrestURL+"/todos", 60*time.Second)
	waitServerReady(t, dbrestURL+"/todos", 60*time.Second)
	t.Logf("both servers ready")

	// ── cases ────────────────────────────────────────────────────────────────
	cases := []struct {
		name   string
		path   string
		warmup int
		n      int
	}{
		{"GET /todos sequential (200 reqs)", "/todos?order=id", 30, 200},
		{"GET /todos?select=id,task sequential", "/todos?select=id,task&order=id", 30, 200},
	}

	type result struct {
		pgRPS float64
		dbRPS float64
		ratio float64
	}
	var results []result

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pgDur := measure(t, pgrestURL, c.path, "application/json", nil, c.warmup, c.n)
			dbDur := measure(t, dbrestURL, c.path, "application/json", nil, c.warmup, c.n)
			pgRPS := float64(c.n) / pgDur.Seconds()
			dbRPS := float64(c.n) / dbDur.Seconds()
			ratio := dbRPS / pgRPS
			mark := "OK"
			if ratio < 2.0 {
				mark = "BELOW_TARGET"
			}
			t.Logf("[%s] %s", mark, c.name)
			t.Logf("  PostgREST: %5.0f req/s  (%v)", pgRPS, pgDur.Round(time.Millisecond))
			t.Logf("  dbrest:    %5.0f req/s  (%v)", dbRPS, dbDur.Round(time.Millisecond))
			t.Logf("  ratio:     %.2fx (target >= 5.0x)", ratio)
			results = append(results, result{pgRPS, dbRPS, ratio})
		})
	}

	// ── concurrent ───────────────────────────────────────────────────────────
	t.Run("GET /todos concurrent (20×500)", func(t *testing.T) {
		pgDur := measureConcurrent(t, pgrestURL, "/todos?order=id", 20, 500)
		dbDur := measureConcurrent(t, dbrestURL, "/todos?order=id", 20, 500)
		pgRPS := float64(500) / pgDur.Seconds()
		dbRPS := float64(500) / dbDur.Seconds()
		ratio := dbRPS / pgRPS
		t.Logf("  PostgREST: %5.0f req/s", pgRPS)
		t.Logf("  dbrest:    %5.0f req/s", dbRPS)
		t.Logf("  ratio:     %.2fx", ratio)
		results = append(results, result{pgRPS, dbRPS, ratio})
	})

	// ── summary ──────────────────────────────────────────────────────────────
	t.Logf("=== latency benchmark summary (latency=%dms one-way) ===", latencyMS)
	best := 0.0
	for _, r := range results {
		if r.ratio > best {
			best = r.ratio
		}
	}
	t.Logf("best ratio: %.2fx", best)
	if best < 5.0 {
		t.Logf("NOTE: best ratio %.2fx < 5.0x target; increase latency or check PostgREST statement count", best)
	}
}

// ── toxiproxy helpers ─────────────────────────────────────────────────────────

func startToxiproxyServer(t *testing.T) func() {
	t.Helper()
	// Kill any stale toxiproxy before starting fresh.
	exec.Command("pkill", "-f", "toxiproxy-server").Run() //nolint:errcheck
	time.Sleep(200 * time.Millisecond)

	cmd := exec.Command("toxiproxy-server", "--port", "8474", "--host", "127.0.0.1")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start toxiproxy-server: %v", err)
	}
	// Wait until the API is reachable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(toxiAPI + "/proxies") //nolint:noctx
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() { cmd.Process.Kill() } //nolint:errcheck
}

func toxiDoMayFail(t *testing.T, method, path string, body any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, toxiAPI+path, r)
	if err != nil {
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
}

func toxiDo(t *testing.T, method, path string, body any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, toxiAPI+path, r)
	if err != nil {
		t.Fatalf("toxiproxy request build: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
}

func createToxiProxy(t *testing.T, name, listen, upstream string) {
	t.Helper()
	// Delete if exists from a prior run; ignore 404.
	toxiDoMayFail(t, http.MethodDelete, "/proxies/"+name, nil)
	toxiDo(t, http.MethodPost, "/proxies", map[string]any{
		"name":     name,
		"listen":   listen,
		"upstream": upstream,
		"enabled":  true,
	})
}

func addLatencyToxic(t *testing.T, proxyName string, ms int) {
	t.Helper()
	toxiDo(t, http.MethodPost, "/proxies/"+proxyName+"/toxics", map[string]any{
		"type":       "latency",
		"name":       "latency",
		"stream":     "downstream",
		"toxicity":   1.0,
		"attributes": map[string]any{"latency": ms, "jitter": 0},
	})
}

// ── server start helpers ──────────────────────────────────────────────────────

func startPostgRESTContainer(t *testing.T, port, dbURI string) func() {
	t.Helper()
	name := "latency-bench-postgrest"
	// Remove any existing container from a prior run.
	exec.Command("podman", "rm", "-f", name).Run() //nolint:errcheck
	cmd := exec.Command("podman", "run", "-d", "--rm",
		"--name", name,
		"-p", port+":3000",
		"-e", "PGRST_DB_URI="+dbURI,
		"-e", "PGRST_DB_SCHEMAS=api",
		"-e", "PGRST_DB_ANON_ROLE=web_anon",
		"-e", "PGRST_SERVER_PORT=3000",
		"-e", "PGRST_LOG_LEVEL=error",
		"docker.io/postgrest/postgrest:v14.13",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("start PostgREST container: %v", err)
	}
	return func() {
		exec.Command("podman", "stop", name).Run() //nolint:errcheck
	}
}

func startDBRestProcess(t *testing.T, bin, port, dbURI string) func() {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DBREST_DB_BACKEND=postgres",
		"DBREST_DB_URI="+dbURI,
		"DBREST_DB_SCHEMAS=api",
		"DBREST_DB_ANON_ROLE=web_anon",
		"DBREST_SERVER_PORT="+port,
		"DBREST_LOG_LEVEL=error",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dbrest: %v", err)
	}
	return func() { cmd.Process.Kill() } //nolint:errcheck
}

func waitServerReady(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url) //nolint:noctx
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return
		}
		if err == nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("server at %s not ready after %v", url, timeout)
}

func tcpReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// toxiDo needs fmt imported only if we add log lines; keep the compiler happy.
var _ = fmt.Sprintf
