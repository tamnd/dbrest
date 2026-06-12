package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/dbrest/config"
)

// shortSocketPath returns a socket path short enough for sun_path. t.TempDir
// can exceed the limit on macOS, so this uses a fresh small temp dir.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbrest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}

// TestListenAPIUnixSocket covers the server-unix-socket listener: it replaces
// TCP, gets the configured mode, survives a stale socket file from a previous
// run, and serves HTTP.
func TestListenAPIUnixSocket(t *testing.T) {
	path := shortSocketPath(t)
	// A stale file at the path must not block the bind.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.FromMap(map[string]string{
		"db-uri": "x", "server-unix-socket": path, "server-unix-socket-mode": "600",
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listenAPI(cfg)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer ln.Close()
	if ln.Addr().Network() != "unix" {
		t.Fatalf("network = %q, want unix", ln.Addr().Network())
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "over the socket")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://unix/anything")
	if err != nil {
		t.Fatalf("GET over the socket: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "over the socket" {
		t.Errorf("body = %q", body)
	}
}

// TestListenAPITCPDefault keeps the TCP path intact when no socket is set.
func TestListenAPITCPDefault(t *testing.T) {
	cfg, err := config.FromMap(map[string]string{"db-uri": "x", "server-port": "0"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := listenAPI(cfg)
	if err != nil {
		t.Fatalf("listenAPI: %v", err)
	}
	defer ln.Close()
	if ln.Addr().Network() != "tcp" {
		t.Errorf("network = %q, want tcp", ln.Addr().Network())
	}
}
