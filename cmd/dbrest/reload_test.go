package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tamnd/dbrest/adminapi"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/config"
)

// newApp boots an app over an in-memory sqlite with one table, the way run()
// does, minus the listeners.
func newApp(t *testing.T, cfg *config.Config) *app {
	t.Helper()
	dsn := "file:reload_" + t.Name() + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if _, err := be.DB().Exec(`CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT);
		INSERT INTO films (title) VALUES ('Metropolis'), ('Sunrise');`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := &app{be: be, cfg: cfg, metrics: adminapi.NewMetrics(cfg.DBPool)}
	if err := a.reloadSchema(); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	return a
}

func mustConfig(t *testing.T, raw map[string]string) *config.Config {
	t.Helper()
	if raw["db-uri"] == "" {
		raw["db-uri"] = "x"
	}
	cfg, err := config.FromMap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func status(t *testing.T, a *app, target string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code
}

// TestReloadSchemaPicksUpNewTable is the SIGUSR1 path: a table created after
// boot is 404 until the schema cache reload, then served.
func TestReloadSchemaPicksUpNewTable(t *testing.T) {
	a := newApp(t, mustConfig(t, map[string]string{"db-anon-role": "web_anon"}))

	if got := status(t, a, "/directors"); got != http.StatusNotFound {
		t.Fatalf("before reload: status = %d, want 404", got)
	}
	if _, err := a.be.(*sqlite.Backend).DB().Exec(`CREATE TABLE directors (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := a.reloadSchema(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := status(t, a, "/directors"); got != http.StatusOK {
		t.Errorf("after reload: status = %d, want 200", got)
	}
}

// TestReloadConfigAppliesReloadableSubset is the SIGUSR2 path: a new max-rows
// takes effect on the next request, while the request keeps flowing.
func TestReloadConfigAppliesReloadableSubset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbrest.conf")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`db-uri = "x"` + "\n" + `db-anon-role = "web_anon"` + "\n")
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(t, cfg)
	a.cfgPath = path

	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/films", nil))
	if got := rec.Header().Get("Content-Range"); got != "0-1/*" {
		t.Fatalf("before reload: Content-Range = %q, want 0-1/*", got)
	}

	write(`db-uri = "x"` + "\n" + `db-anon-role = "web_anon"` + "\n" + `db-max-rows = 1` + "\n")
	if err := a.reloadConfig(nil); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/films", nil))
	if got := rec.Header().Get("Content-Range"); got != "0-0/*" {
		t.Errorf("after reload: Content-Range = %q, want 0-0/* (max-rows applied)", got)
	}
}

// TestReloadConfigKeepsServingOnBadFile checks the failure mode: a config that
// no longer loads is rejected and the old one stays in service.
func TestReloadConfigKeepsServingOnBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dbrest.conf")
	if err := os.WriteFile(path, []byte(`db-uri = "x"`+"\n"+`db-anon-role = "web_anon"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(t, cfg)
	a.cfgPath = path

	if err := os.WriteFile(path, []byte(`db-tx-end = "explode"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.reloadConfig(nil); err == nil {
		t.Fatal("expected the bad config to be rejected")
	}
	if got := status(t, a, "/films"); got != http.StatusOK {
		t.Errorf("after failed reload: status = %d, want 200", got)
	}
}

// TestSchemaReloadFailureKeepsOldCache mirrors upstream: when re-introspection
// fails the old cache keeps serving.
func TestSchemaReloadFailureKeepsOldCache(t *testing.T) {
	a := newApp(t, mustConfig(t, map[string]string{}))
	a.be.(*sqlite.Backend).Close()
	if err := a.reloadSchema(); err == nil {
		t.Skip("introspection on a closed handle did not fail; nothing to assert")
	}
	if a.Model() == nil || a.Model().Len() == 0 {
		t.Error("old schema cache was dropped on a failed reload")
	}
}
