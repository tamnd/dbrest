package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/rpc"
)

// 07.14: a portable registry function steers the response the way a PostgreSQL
// function does with the response.status / response.headers GUCs, except an
// emulated backend has no setting a single SELECT can write, so the function
// projects reserved columns of the same name. The backend lifts them into the
// response controls and strips them from the body.

func responseControlFunctions() []*rpc.Function {
	return []*rpc.Function{
		{
			Name:       "gone",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "message"}}},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: `SELECT 'resource gone' AS message, 410 AS "response.status"`},
		},
		{
			Name:       "with_header",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "message"}}},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: `SELECT 'ok' AS message, '[{"X-Total-Count":"42"}]' AS "response.headers"`},
		},
		{
			Name:       "archive",
			Params:     []rpc.Param{{Name: "id", Type: "integer"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "title"}}},
			Volatility: rpc.Volatile,
			Query:      &rpc.PortableQuery{SQL: `UPDATE films SET year = 0 WHERE id = :id RETURNING title, 202 AS "response.status"`},
		},
		{
			Name:       "bad_status",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "message"}}},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: `SELECT 'x' AS message, 9999 AS "response.status"`},
		},
		{
			Name:       "bad_header",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "message"}}},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: `SELECT 'x' AS message, 'not-a-header' AS "response.headers"`},
		},
		{
			Name:       "bad_status_volatile",
			Params:     []rpc.Param{{Name: "id", Type: "integer"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "title"}}},
			Volatility: rpc.Volatile,
			Query:      &rpc.PortableQuery{SQL: `UPDATE films SET year = 0 WHERE id = :id RETURNING title, 9999 AS "response.status"`},
		},
	}
}

func newResponseControlServer(t *testing.T) *httpapi.Server {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })

	_, err = be.DB().Exec(`
		CREATE TABLE films (
			id    INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			year  INTEGER
		);
		INSERT INTO films (id, title, year) VALUES
			(1, 'Metropolis', 1927),
			(2, 'Blade Runner', 1982);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	be.Register(rpc.NewStaticRegistry(responseControlFunctions()))

	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	return srv
}

// TestRPCResponseStatusOverride: a read-only function projecting response.status
// sets the HTTP status and the column never appears in the body.
func TestRPCResponseStatusOverride(t *testing.T) {
	srv := newResponseControlServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/gone", nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["message"] != "resource gone" {
		t.Fatalf("body = %v", rows)
	}
	if _, leaked := rows[0]["response.status"]; leaked {
		t.Error("response.status column leaked into the body")
	}
}

// TestRPCResponseHeaderOverride: a function projecting response.headers merges the
// header into the response.
func TestRPCResponseHeaderOverride(t *testing.T) {
	srv := newResponseControlServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/with_header", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Total-Count"); got != "42" {
		t.Errorf("X-Total-Count = %q, want 42", got)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["message"] != "ok" {
		t.Fatalf("body = %v", rows)
	}
	if _, leaked := rows[0]["response.headers"]; leaked {
		t.Error("response.headers column leaked into the body")
	}
}

// TestRPCResponseStatusVolatile: a volatile function steers the status the same
// way through its RETURNING projection, after the mutation it commits.
func TestRPCResponseStatusVolatile(t *testing.T) {
	srv := newResponseControlServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/archive", `{"id":1}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Metropolis" {
		t.Fatalf("body = %v", rows)
	}
	if _, leaked := rows[0]["response.status"]; leaked {
		t.Error("response.status column leaked into the body")
	}
	// The mutation committed: the archived film now has year 0.
	after := do(t, srv, http.MethodGet, "/films?id=eq.1&select=year", nil)
	got := decodeArray(t, after)
	if len(got) != 1 || got[0]["year"].(float64) != 0 {
		t.Errorf("archive did not persist: %v", got)
	}
}

// TestRPCInvalidResponseStatus: a function projecting an out-of-range status is
// PGRST112, the way PostgREST rejects a junk response.status rather than
// forwarding it.
func TestRPCInvalidResponseStatus(t *testing.T) {
	srv := newResponseControlServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/bad_status", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST112" {
		t.Errorf("code = %v, want PGRST112", env["code"])
	}
}

// TestRPCInvalidResponseHeaders: a malformed response.headers is PGRST111.
func TestRPCInvalidResponseHeaders(t *testing.T) {
	srv := newResponseControlServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/bad_header", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST111" {
		t.Errorf("code = %v, want PGRST111", env["code"])
	}
}

// TestRPCInvalidResponseStatusVolatileRollsBack: an invalid status from a
// volatile function fails before commit, so the mutation is discarded.
func TestRPCInvalidResponseStatusVolatileRollsBack(t *testing.T) {
	srv := newResponseControlServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/bad_status_volatile", `{"id":2}`, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST112" {
		t.Errorf("code = %v, want PGRST112", env["code"])
	}
	// The UPDATE rolled back: film 2 still has its seeded year.
	after := do(t, srv, http.MethodGet, "/films?id=eq.2&select=year", nil)
	got := decodeArray(t, after)
	if len(got) != 1 || got[0]["year"].(float64) != 1982 {
		t.Errorf("rollback failed, film 2 year = %v, want 1982", got)
	}
}
