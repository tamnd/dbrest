package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
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
			Name:       "get_request_method",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "text"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT :request_method"},
		},
		{
			Name:       "get_jwt_claims",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "json"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT :request_jwt_claims"},
		},
		{
			Name:       "films_after",
			Params:     []rpc.Param{{Name: "y", Type: "integer"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnTable, Columns: []rpc.Column{{Name: "id"}, {Name: "title"}}},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT id, title FROM films WHERE year > :y ORDER BY id"},
		},
		{
			Name:       "pick_titles",
			Params:     []rpc.Param{{Name: "ids", Type: "integer", Variadic: true}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "text"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT title FROM films WHERE id IN (:ids) ORDER BY id"},
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
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	return srv
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

// /rpc/<fn>/extra is a multi-segment path, not a missing function: PostgREST
// answers PGRST125 at 404, not the PGRST202 a missing function gets (item 04.8).
func TestRPCNestedPathIsInvalidPath(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/add/extra", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST125" {
		t.Errorf("code = %v, want PGRST125", env["code"])
	}
}

// A GET to a volatile function fails with the read-only-transaction SQLSTATE
// 25006 at 405, the same code and status PostgREST surfaces when the read-only
// transaction rejects the function's write (item 04.6).
func TestRPCGetOnVolatileIs405(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/bump_year?film_id=1", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "25006" {
		t.Errorf("code = %v, want 25006", env["code"])
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
	// tx= is only honored under an allow-override db-tx-end policy; the default
	// commit ignores it (02.4). Enable override so the rollback takes effect.
	srv.SetTxEnd("commit-allow-override")
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

// TestRPCGetArgAndColumnFilter pins the GET argument-versus-filter split: y names
// the function parameter and binds as an argument, while title names no parameter
// and post-filters the table return as a horizontal filter, the way PostgREST
// treats a non-argument query key on a table-valued function.
func TestRPCGetArgAndColumnFilter(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/films_after?y=1900&title=eq.Arrival", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["title"] != "Arrival" {
		t.Errorf("title = %v, want Arrival", rows[0]["title"])
	}
}

// TestRPCVariadicGet checks a variadic parameter collects repeated query keys on
// GET and expands into the IN list, so pick_titles?ids=1&ids=3 binds both ids.
func TestRPCVariadicGet(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/pick_titles?ids=1&ids=3", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var titles []string
	if err := json.NewDecoder(resp.Body).Decode(&titles); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(titles) != 2 || titles[0] != "Metropolis" || titles[1] != "Arrival" {
		t.Errorf("titles = %v, want [Metropolis Arrival]", titles)
	}
}

// TestRPCVariadicPost checks a variadic parameter takes a JSON array on POST and
// expands into the same IN list.
func TestRPCVariadicPost(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/pick_titles", `{"ids":[1,3]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var titles []string
	if err := json.NewDecoder(resp.Body).Decode(&titles); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(titles) != 2 || titles[0] != "Metropolis" || titles[1] != "Arrival" {
		t.Errorf("titles = %v, want [Metropolis Arrival]", titles)
	}
}

// TestRPCGetBadArgTypeIs400 checks a GET argument that does not coerce to its
// declared parameter type is a 22P02 400, the same error a read filter raises,
// rather than reaching the engine as raw text.
func TestRPCGetBadArgTypeIs400(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/add_them?a=notanint&b=3", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
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
	srv.SetDefaultRole("anon")

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

// TestRPCVoidReturns200Null pins the void contract: PostgREST answers a
// void-returning function with 200 and a null JSON body, never 204. The function
// runs its side effect (an INSERT here) and the response carries null.
func TestRPCVoidReturns200Null(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if _, err := be.DB().Exec(`
		CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT NOT NULL, year INTEGER);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	be.Register(rpc.NewStaticRegistry([]*rpc.Function{{
		Name:       "touch_film",
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnVoid},
		Volatility: rpc.Volatile,
		Query:      &rpc.PortableQuery{SQL: `INSERT INTO films(id, title, year) VALUES (999, 'Void', 2000)`},
	}}))
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")

	resp := send(t, srv, http.MethodPost, "/rpc/touch_film", `{}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != "null" {
		t.Errorf("body = %q, want null", got)
	}
	// The side effect ran: the row is present.
	var n int
	if err := be.DB().QueryRow(`SELECT count(*) FROM films WHERE id = 999`).Scan(&n); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if n != 1 {
		t.Errorf("void function side effect did not persist: count = %d", n)
	}
}

// TestRPCSingleRawBodyTakesWholeBody pins the single-unnamed-parameter form: a
// function with one raw-body parameter receives the entire POST body as that one
// argument, decoded by Content-Type, rather than read as an object of named
// arguments. A JSON array body would fail the named-object decode, so its
// round-trip proves the raw-body path bound it whole.
func TestRPCSingleRawBodyTakesWholeBody(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	be.Register(rpc.NewStaticRegistry([]*rpc.Function{{
		Name:       "echo_payload",
		Params:     []rpc.Param{{Name: "payload", Type: "json", RawBody: true}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "json"},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: `SELECT :payload`},
	}}))
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")

	resp := send(t, srv, http.MethodPost, "/rpc/echo_payload", `[1,2,3]`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var arr []json.Number
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&arr); err != nil {
		t.Fatalf("the array body should round-trip whole: %v", err)
	}
	if len(arr) != 3 || arr[0].String() != "1" || arr[2].String() != "3" {
		t.Errorf("body = %v, want [1 2 3]", arr)
	}
}

// TestRPCSingleRawBodyText pins the text content type on the raw-body form: a
// text/plain body binds to the lone parameter as text and echoes back.
func TestRPCSingleRawBodyText(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	be.Register(rpc.NewStaticRegistry([]*rpc.Function{{
		Name:       "shout",
		Params:     []rpc.Param{{Name: "line", Type: "text", RawBody: true}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "text"},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: `SELECT upper(:line)`},
	}}))
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")

	resp := send(t, srv, http.MethodPost, "/rpc/shout", `hello`, map[string]string{
		"Content-Type": "text/plain",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var s string
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s != "HELLO" {
		t.Errorf("body = %q, want HELLO", s)
	}
}

// The reserved :request_* placeholders give a registry function the request
// context PostgreSQL functions read with current_setting (spec 15). The HTTP
// surface matches PostgREST's GUC behavior on every engine.
func TestRPCContextRequestMethod(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/get_request_method", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var s string
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s != "GET" {
		t.Errorf("body = %q, want GET", s)
	}
}

// TestRPCSetofContentRangeAlwaysPresent pins 02.7: an RPC read carries a
// Content-Range like a table read even with no count requested, the unknown
// total rendered as 0-2/*.
func TestRPCSetofContentRangeAlwaysPresent(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-2/*" {
		t.Errorf("Content-Range = %q, want 0-2/*", cr)
	}
}

// TestRPCRangeHeaderOnGet pins 02.7: a GET /rpc honors a unitless Range header
// the way a table read does, slicing the set. With no count the total is
// unknown, so the status stays 200 (PostgREST's rangeStatus on a missing total)
// while Content-Range echoes the slice.
func TestRPCRangeHeaderOnGet(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles", map[string]string{
		"Range": "0-1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-1/*" {
		t.Errorf("Content-Range = %q, want 0-1/*", cr)
	}
	var titles []string
	if err := json.NewDecoder(resp.Body).Decode(&titles); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(titles) != 2 || titles[0] != "Metropolis" || titles[1] != "Blade Runner" {
		t.Errorf("titles = %v, want [Metropolis Blade Runner]", titles)
	}
}

// TestRPCRangeHeaderOnGetWithCountIs206 pins 02.7: the same slice with an exact
// count knows the total exceeds the slice, so it is the 206 a table read gives.
func TestRPCRangeHeaderOnGetWithCountIs206(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles", map[string]string{
		"Range":  "0-1",
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-1/3" {
		t.Errorf("Content-Range = %q, want 0-1/3", cr)
	}
}

// TestRPCRangeOutOfBoundsIs416 pins 02.7: a GET /rpc Range whose offset is past
// the known total is the same 416 a table read raises.
func TestRPCRangeOutOfBoundsIs416(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles", map[string]string{
		"Range":  "5-9",
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
}

// TestRPCInvertedRangeOnGetIs416 pins 02.7: an inverted Range on a GET /rpc is
// the same 416 a table read raises, before any work runs.
func TestRPCInvertedRangeOnGetIs416(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles", map[string]string{
		"Range": "3-1",
	})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
}

// An anonymous request still carries the resolved role in request.jwt.claims:
// PostgREST folds the role into the claims object even when the token had none,
// so the claims are {"role":"<anon-role>"}, not {}. Verified against PostgREST
// 14.12, where an anonymous call presents {"role":"<db-anon-role>"}.
func TestRPCContextJWTClaimsCarriesAnonRole(t *testing.T) {
	srv := newRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/get_jwt_claims", `{}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var claims map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(claims) != 1 || claims["role"] != "anon" {
		t.Errorf("claims = %v, want {\"role\":\"anon\"}", claims)
	}
}
