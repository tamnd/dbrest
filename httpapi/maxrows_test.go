package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readJSONArray decodes a JSON array response body.
func readJSONArray(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return rows
}

// TestMaxRowsCapsRead checks that db-max-rows is an implicit LIMIT on reads:
// the body is truncated and Content-Range reports the served window.
func TestMaxRowsCapsRead(t *testing.T) {
	srv := newServer(t) // 4 films seeded
	srv.SetMaxRows(2)

	resp := do(t, srv, http.MethodGet, "/films?order=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no count requested)", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "0-1/*" {
		t.Errorf("Content-Range = %q, want 0-1/*", got)
	}
	if rows := readJSONArray(t, resp); len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
}

// TestMaxRowsWithExactCount checks the 206 shape: a capped read with
// count=exact reports the true total and Partial Content.
func TestMaxRowsWithExactCount(t *testing.T) {
	srv := newServer(t)
	srv.SetMaxRows(2)

	resp := do(t, srv, http.MethodGet, "/films?order=id", map[string]string{"Prefer": "count=exact"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "0-1/4" {
		t.Errorf("Content-Range = %q, want 0-1/4", got)
	}
}

// TestMaxRowsMinWithRequestedLimit checks min(requested, max-rows) in both
// directions.
func TestMaxRowsMinWithRequestedLimit(t *testing.T) {
	srv := newServer(t)
	srv.SetMaxRows(2)

	resp := do(t, srv, http.MethodGet, "/films?order=id&limit=1", nil)
	if rows := readJSONArray(t, resp); len(rows) != 1 {
		t.Errorf("limit below cap: rows = %d, want 1", len(rows))
	}
	resp = do(t, srv, http.MethodGet, "/films?order=id&limit=10", nil)
	if rows := readJSONArray(t, resp); len(rows) != 2 {
		t.Errorf("limit above cap: rows = %d, want 2", len(rows))
	}
}

// TestMaxRowsExemptsMutationRepresentation checks the PostgREST v10+ rule:
// the representation of a write returns every affected row, uncapped.
func TestMaxRowsExemptsMutationRepresentation(t *testing.T) {
	srv := newServer(t)
	srv.SetMaxRows(1)

	body := `[{"title":"One"},{"title":"Two"},{"title":"Three"}]`
	req := httptest.NewRequest(http.MethodPost, "/films", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=representation")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if rows := readJSONArray(t, resp); len(rows) != 3 {
		t.Errorf("representation rows = %d, want all 3 despite max-rows=1", len(rows))
	}
}

// TestMaxRowsCapsRPC checks that a table-returning function is capped too.
// The setof-scalar path compiles the function body verbatim in the sqlite
// backend and cannot take a window yet; the cap reaches it once that
// compiler gap closes (the RPC pagination item).
func TestMaxRowsCapsRPC(t *testing.T) {
	srv := newRPCServer(t) // 3 films
	srv.SetMaxRows(1)

	resp := do(t, srv, http.MethodGet, "/rpc/films_after?y=1900", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rows []any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rpc rows = %d, want 1", len(rows))
	}
}
