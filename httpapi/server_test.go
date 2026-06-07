package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
)

func newServer(t testing.TB) *httpapi.Server {
	t.Helper()
	// A uniquely named shared-cache memory DB isolates each test's data.
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })

	_, err = be.DB().Exec(`
		CREATE TABLE films (
			id     INTEGER PRIMARY KEY,
			title  TEXT NOT NULL,
			year   INTEGER,
			rating TEXT
		);
		INSERT INTO films (id, title, year, rating) VALUES
			(1, 'Metropolis', 1927, 'NR'),
			(2, 'Blade Runner', 1982, 'R'),
			(3, 'Arrival', 2016, 'PG13'),
			(4, 'Untitled', NULL, 'NR');
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	return httpapi.NewServer(be, model, nil)
}

func do(t *testing.T, srv *httpapi.Server, method, target string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Result()
}

func decodeArray(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode array: %v", err)
	}
	return out
}

func TestGetAll(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-3/*" {
		t.Errorf("Content-Range = %q, want 0-3/*", cr)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
}

func TestGetSelectFilterOrder(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,year&year=gte.1980&order=year.desc", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["title"] != "Arrival" {
		t.Errorf("first title = %v, want Arrival", rows[0]["title"])
	}
	if _, ok := rows[0]["rating"]; ok {
		t.Error("rating should not be projected")
	}
}

func TestGetPaginationPartial(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?order=id&limit=2", nil)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-1/*" {
		t.Errorf("Content-Range = %q, want 0-1/*", cr)
	}
	if len(decodeArray(t, resp)) != 2 {
		t.Error("want 2 rows")
	}
}

func TestGetSingular(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.2", map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	if obj["title"] != "Blade Runner" {
		t.Errorf("title = %v", obj["title"])
	}
}

func TestGetSingularZeroRowsIs406(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.999", map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
	})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST116" {
		t.Errorf("code = %v, want PGRST116", env["code"])
	}
}

func TestGetEmptyArray(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.999", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if rows == nil || len(rows) != 0 {
		t.Errorf("want empty array, got %v", rows)
	}
}

func TestUnknownTableIs404Code(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/nope", nil)
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST205" {
		t.Errorf("code = %v, want PGRST205", env["code"])
	}
}

func TestUnknownColumnIsError(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=bogus", nil)
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST204" {
		t.Errorf("code = %v, want PGRST204", env["code"])
	}
}

func TestHeadHasNoBody(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodHead, "/films", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-3/*" {
		t.Errorf("Content-Range = %q", cr)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("HEAD should have no body")
	}
}

func TestGetExactCount(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?order=id&limit=2", map[string]string{
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-1/4" {
		t.Errorf("Content-Range = %q, want 0-1/4", cr)
	}
	if len(decodeArray(t, resp)) != 2 {
		t.Error("want 2 rows in the window")
	}
}

func TestGetCountWholeSetIs200(t *testing.T) {
	srv := newServer(t)
	// A window wide enough to cover every row, with a count, is 200 not 206.
	resp := do(t, srv, http.MethodGet, "/films?order=id&limit=100", map[string]string{
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-3/4" {
		t.Errorf("Content-Range = %q, want 0-3/4", cr)
	}
}

func TestGetOffsetPastEndIs416(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?order=id&limit=2&offset=10", map[string]string{
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "*/4" {
		t.Errorf("Content-Range = %q, want */4", cr)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST103" {
		t.Errorf("code = %v, want PGRST103", env["code"])
	}
}

func TestGetEmptyCountedResult(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.999", map[string]string{
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// PostgREST emits */0 for an empty counted result.
	if cr := resp.Header.Get("Content-Range"); cr != "*/0" {
		t.Errorf("Content-Range = %q, want */0", cr)
	}
}

func BenchmarkGetFilteredRead(b *testing.B) {
	srv := newServer(b)
	req := httptest.NewRequest(http.MethodGet, "/films?select=id,title&year=gte.1900&order=year.desc&limit=10", nil)
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusPartialContent && rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
