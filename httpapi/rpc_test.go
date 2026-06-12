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
	"github.com/tamnd/dbrest/rpc"
)

// rpcFunctions are the portable functions the SQLite backend exposes at /rpc.
func rpcFunctions() []*rpc.Function {
	return []*rpc.Function{
		{
			Name:       "add_them",
			Params:     []rpc.Param{{Name: "a", Type: "integer"}, {Name: "b", Type: "integer"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "integer"},
			Volatility: rpc.Immutable,
			Query:      &rpc.PortableQuery{SQL: "SELECT :a + :b"},
		},
		{
			Name:       "bump_year",
			Params:     []rpc.Param{{Name: "film_id", Type: "integer"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "integer"},
			Volatility: rpc.Volatile,
			Query:      &rpc.PortableQuery{SQL: "UPDATE films SET year = year + 1 WHERE id = :film_id RETURNING year"},
		},
		{
			Name:       "film_titles",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "text"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT title FROM films ORDER BY id"},
		},
		{
			Name:       "films_after",
			Params:     []rpc.Param{{Name: "y", Type: "integer"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y ORDER BY id"},
		},
	}
}

func newRPCServer(t testing.TB) *httpapi.Server {
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
			(2, 'Blade Runner', 1982),
			(3, 'Arrival', 2016);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	be.Register(rpc.NewStaticRegistry(rpcFunctions()))

	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	return httpapi.NewServer(be, model, nil)
}

func TestRPCGetScalarAddThem(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/add_them?a=2&b=3", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var n json.Number
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n.String() != "5" {
		t.Errorf("body = %s, want 5", n)
	}
}

func TestRPCPostScalarAddThem(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/add_them", `{"a":2,"b":3}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var n json.Number
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n.String() != "5" {
		t.Errorf("body = %s, want 5", n)
	}
}

func TestRPCUnknownFunctionIs404(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/nope?x=1", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST202" {
		t.Errorf("code = %v, want PGRST202", env["code"])
	}
}

func TestRPCGetOnVolatileIs405(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/bump_year?film_id=1", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST101" {
		t.Errorf("code = %v, want PGRST101", env["code"])
	}
}

func TestRPCPostVolatilePersists(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/bump_year", `{"film_id":1}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var n json.Number
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	dec.Decode(&n)
	if n.String() != "1928" {
		t.Errorf("returned year = %s, want 1928", n)
	}
	// The side effect persisted.
	after := do(t, srv, http.MethodGet, "/films?id=eq.1&select=year", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["year"].(float64) != 1928 {
		t.Errorf("persisted year = %v", rows)
	}
}

func TestRPCPostVolatileRollback(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/bump_year", `{"film_id":2}`, map[string]string{
		"Prefer": "tx=rollback",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The work was rolled back: the year is unchanged.
	after := do(t, srv, http.MethodGet, "/films?id=eq.2&select=year", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["year"].(float64) != 1982 {
		t.Errorf("year after rollback = %v, want 1982", rows)
	}
}

func TestRPCSetofScalar(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var titles []string
	if err := json.NewDecoder(resp.Body).Decode(&titles); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"Metropolis", "Blade Runner", "Arrival"}
	if len(titles) != 3 || titles[0] != want[0] || titles[2] != want[2] {
		t.Errorf("titles = %v, want %v", titles, want)
	}
}

func TestRPCTableReturn(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/films_after", `{"y":1950}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["title"] != "Blade Runner" {
		t.Errorf("first title = %v", rows[0]["title"])
	}
}

func TestRPCTablePostFilter(t *testing.T) {
	srv := newRPCServer(t)
	// Project just title and keep one row from the table return.
	resp := do(t, srv, http.MethodGet, "/rpc/films_after?y=1950&select=title&limit=1", nil)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if _, ok := rows[0]["id"]; ok {
		t.Error("id should not be projected")
	}
	if rows[0]["title"] != "Blade Runner" {
		t.Errorf("title = %v", rows[0]["title"])
	}
}

func TestRPCTableSingular(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/films_after?y=2000", map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	if obj["title"] != "Arrival" {
		t.Errorf("title = %v, want Arrival", obj["title"])
	}
}

func TestRPCMethodNotAllowed(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPut, "/rpc/add_them", `{"a":1,"b":2}`, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRPCCountHeader(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/films_after?y=1950", map[string]string{
		"Prefer": "count=exact",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "0-1/2" {
		t.Errorf("Content-Range = %q, want 0-1/2", cr)
	}
}

func BenchmarkRPCGetScalar(b *testing.B) {
	srv := newRPCServer(b)
	req := httptest.NewRequest(http.MethodGet, "/rpc/add_them?a=2&b=3", nil)
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

// TestRPCScalarJSONReturnsRaw pins the declared-json contract: a function
// returning json emits the document itself, not a quoted string, the way a
// PostgreSQL json function behaves through PostgREST. An expression carries
// no column type, so the declared return type drives the conversion.
func TestRPCScalarJSONReturnsRaw(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	be.Register(rpc.NewStaticRegistry([]*rpc.Function{{
		Name:       "payload",
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "json"},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: `SELECT json_object('a', 1)`},
	}}))
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)

	resp := do(t, srv, http.MethodGet, "/rpc/payload", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("the body should be a JSON object, not a quoted string: %v", err)
	}
	if doc["a"] != float64(1) {
		t.Errorf("body = %v", doc)
	}
}
