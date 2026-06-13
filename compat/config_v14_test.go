// HTTP-level checks for the v14 configuration surface (review items 05.x).
// Unlike the main suite, these start an in-process dbrest built from the
// current tree, so the behavior under test is the working copy's, and compare
// it against a live PostgREST when one is reachable.
package compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
)

// localDBREST starts an in-process dbrest over a seeded sqlite database and
// returns its base URL. The schema mirrors the todos table of the compat seed
// closely enough for header-level comparisons.
func localDBREST(t *testing.T) (*httptest.Server, *httpapi.Server) {
	t.Helper()
	dsn := "file:compat_" + t.Name() + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if _, err := be.DB().Exec(`
		CREATE TABLE todos (id INTEGER PRIMARY KEY, task TEXT, done BOOLEAN, due TIMESTAMP);
		INSERT INTO todos (id, task, done) VALUES (1, 'do laundry', 0);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	api := httpapi.NewServer(be, model, nil)
	// Mirror the live PostgREST rig's db-anon-role=web_anon so tokenless reads
	// run as anon rather than failing closed with 401.
	api.SetDefaultRole("web_anon")
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	return ts, api
}

// livePostgREST returns the base URL of a reachable PostgREST, or skips.
func livePostgREST(t *testing.T) string {
	t.Helper()
	base := envOr("COMPAT_POSTGREST_URL", "http://localhost:3000")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !pingOK(ctx, base) {
		t.Skipf("PostgREST not reachable at %s; set COMPAT_POSTGREST_URL or start docker/postgrest/compose.yaml", base)
	}
	return base
}

// corsHeaders are the response headers compared between the two servers.
var corsHeaders = []string{
	"Access-Control-Allow-Origin",
	"Access-Control-Allow-Credentials",
	"Access-Control-Allow-Methods",
	"Access-Control-Allow-Headers",
	"Access-Control-Max-Age",
	"Access-Control-Expose-Headers",
}

// TestV14CORSPreflight compares the default preflight answer (item 05.2)
// against a live PostgREST: wildcard origin, the full method list, the
// requested headers reflected, and the one-day max age.
func TestV14CORSPreflight(t *testing.T) {
	pgrest := livePostgREST(t)
	local, _ := localDBREST(t)

	c := compatCase{
		method: "OPTIONS", path: "/todos",
		headers: map[string]string{
			"Origin":                         "http://example.com",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "Foo,Bar",
		},
	}
	pg := doRequest(t, pgrest, c)
	db := doRequest(t, local.URL, c)
	if pg.status != db.status {
		t.Errorf("preflight status: postgrest %d, dbrest %d", pg.status, db.status)
	}
	for _, h := range corsHeaders {
		if pgv, dbv := pg.header.Get(h), db.header.Get(h); pgv != dbv {
			t.Errorf("preflight %s: postgrest %q, dbrest %q", h, pgv, dbv)
		}
	}
}

// TestV14CORSSimpleRequest compares the cross-origin headers on a plain read
// (item 05.2): wildcard origin plus the exposed-headers list.
func TestV14CORSSimpleRequest(t *testing.T) {
	pgrest := livePostgREST(t)
	local, _ := localDBREST(t)

	c := compatCase{
		method: "GET", path: "/todos",
		headers: map[string]string{"Origin": "http://example.com"},
	}
	pg := doRequest(t, pgrest, c)
	db := doRequest(t, local.URL, c)
	if pg.status != http.StatusOK || db.status != http.StatusOK {
		t.Fatalf("status: postgrest %d, dbrest %d", pg.status, db.status)
	}
	for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Expose-Headers"} {
		if pgv, dbv := pg.header.Get(h), db.header.Get(h); pgv != dbv {
			t.Errorf("%s: postgrest %q, dbrest %q", h, pgv, dbv)
		}
	}
}
