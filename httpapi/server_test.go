package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
)

func newServer(t testing.TB) *httpapi.Server {
	t.Helper()
	srv := newServerNoRole(t)
	srv.SetDefaultRole("anon")
	return srv
}

// newServerNoRole builds the test server without a default role, the state a
// bare NewServer is in before db-anon-role is applied.
func newServerNoRole(t testing.TB) *httpapi.Server {
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
	// PostgREST v14: ?limit= without count=exact returns 200, not 206.
	// 206 only when count=exact confirms the window is partial.
	resp := do(t, srv, http.MethodGet, "/films?order=id&limit=2", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
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
	// v14 texts: the message dropped the pre-v12 spelling and the row count
	// rides in details.
	if env["message"] != "Cannot coerce the result to a single JSON object" {
		t.Errorf("message = %v", env["message"])
	}
	if env["details"] != "The result contains 0 rows" {
		t.Errorf("details = %v, want row count", env["details"])
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
	// PGRST205 schema-qualifies the relation name (item 04.3): a backend with no
	// schema namespace still reports the default public schema, as PostgREST does.
	if msg, _ := env["message"].(string); msg != "Could not find the table 'public.nope' in the schema cache" {
		t.Errorf("message = %q, want it schema-qualified", msg)
	}
}

// A path with more than one segment names no routable resource and is PGRST125
// at 404, not the PGRST205 a single unknown relation gets (item 04.8).
func TestNestedTablePathIsInvalidPath(t *testing.T) {
	srv := newServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		resp := do(t, srv, method, "/films/extra", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", method, resp.StatusCode)
		}
		var env map[string]any
		json.NewDecoder(resp.Body).Decode(&env)
		if env["code"] != "PGRST125" {
			t.Errorf("%s code = %v, want PGRST125", method, env["code"])
		}
		if msg, _ := env["message"].(string); msg != "Invalid path specified in request URL" {
			t.Errorf("%s message = %q", method, msg)
		}
	}
}

func TestUnknownColumnIsError(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=bogus", nil)
	// An unknown select column reaches PostgreSQL: 42703 at 400 (item 04.5), not
	// the schema-cache PGRST204 reserved for write payloads.
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "42703" {
		t.Errorf("code = %v, want 42703", env["code"])
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

// send is like do but with a request body and an explicit content type.
func send(t *testing.T, srv *httpapi.Server, method, target, body string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Result()
}

func TestPostInsertMinimalIs201WithoutLocation(t *testing.T) {
	srv := newServer(t)
	// PostgREST v14: minimal insert (no Prefer) returns 201 with no Location header.
	resp := send(t, srv, http.MethodPost, "/films", `{"id":5,"title":"Dune","year":2021}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("Location = %q, want empty for minimal insert", loc)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("minimal insert should have no body")
	}
}

func TestPostInsertHeadersOnlyIs201WithLocation(t *testing.T) {
	srv := newServer(t)
	// PostgREST v14: return=headers-only sets the Location header.
	resp := send(t, srv, http.MethodPost, "/films",
		`{"id":6,"title":"Dune2","year":2024}`,
		map[string]string{"Prefer": "return=headers-only"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/films?id=eq.6" {
		t.Errorf("Location = %q, want /films?id=eq.6", loc)
	}
}

func TestPostInsertRepresentation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":6,"title":"Tenet","year":2020}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Tenet" {
		t.Fatalf("representation body = %v", rows)
	}
}

func TestPostInsertSingularRepresentation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":8,"title":"Solaris"}`, map[string]string{
		"Prefer": "return=representation",
		"Accept": "application/vnd.pgrst.object+json",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var obj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	if obj["title"] != "Solaris" {
		t.Errorf("title = %v", obj["title"])
	}
}

func TestPostBulkInsertNoLocation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `[{"id":10,"title":"A"},{"id":11,"title":"B"}]`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("bulk insert should not set Location, got %q", loc)
	}
	// A POST reports the total-only range, never a row span (02.8).
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
	if len(decodeArray(t, resp)) != 2 {
		t.Error("want 2 inserted rows")
	}
}

func TestPatchUpdateRepresentation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"PG"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["rating"] != "PG" {
		t.Fatalf("patch body = %v", rows)
	}
}

func TestPatchMinimalIs204(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"PG"}`, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestDeleteMinimalIs204(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodDelete, "/films?id=eq.1", "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	// The row is gone.
	after := do(t, srv, http.MethodGet, "/films?id=eq.1", nil)
	if len(decodeArray(t, after)) != 0 {
		t.Error("row should be deleted")
	}
}

func TestDeleteRepresentation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodDelete, "/films?id=eq.3", "", map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Arrival" {
		t.Fatalf("deleted representation = %v", rows)
	}
}

// A merge-duplicates upsert that hits an existing row updates it; PostgREST v14
// reports 200, not 201, because nothing new was created.
func TestPostUpsertMergeDuplicatesUpdateIs200(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":1,"title":"Metropolis (restored)"}`, map[string]string{
		"Prefer": "return=representation, resolution=merge-duplicates",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Metropolis (restored)" {
		t.Fatalf("upsert body = %v", rows)
	}
}

// A merge-duplicates upsert whose key is new inserts a row, so v14 reports 201.
func TestPostUpsertMergeDuplicatesInsertIs201(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":50,"title":"Brand New"}`, map[string]string{
		"Prefer": "return=representation, resolution=merge-duplicates",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

// PUT replaces or creates the addressed row. When the key did not exist the row
// is created, so v14 reports 201.
func TestPutUpsertNewIs201(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPut, "/films?id=eq.20", `{"id":20,"title":"New"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

// PUT to an existing key replaces it; nothing is created, so v14 reports 200.
func TestPutUpsertExistingIs200(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPut, "/films?id=eq.1", `{"id":1,"title":"Metropolis (cut)"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPostUniqueViolationIs409(t *testing.T) {
	srv := newServer(t)
	// id=1 already exists; a plain insert conflicts.
	resp := send(t, srv, http.MethodPost, "/films", `{"id":1,"title":"Dup"}`, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "23505" {
		t.Errorf("code = %v, want 23505", env["code"])
	}
}

func TestPatchUnknownColumnIs400(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.1", `{"bogus":"x"}`, nil)
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST204" {
		t.Errorf("code = %v, want PGRST204", env["code"])
	}
}

func TestPostBadJSONIs400(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{nope`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func BenchmarkPostInsert(b *testing.B) {
	srv := newServer(b)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		body := `{"id":` + strconv.Itoa(1000+i) + `,"title":"Bench"}`
		req := httptest.NewRequest(http.MethodPost, "/films", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			b.Fatalf("status = %d", rec.Code)
		}
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

// TestReloadPublishesNewSchema pins the schema cache reload: DDL applied
// after startup is invisible (404 PGRST205) until Reload re-runs
// introspection, after which the new table serves and the OpenAPI document
// describes it. This is the dbrest side of PostgREST's SIGUSR1 / NOTIFY
// reload flow; the signal wiring lives in cmd.
func TestReloadPublishesNewSchema(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if _, err := be.DB().Exec(`CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT NOT NULL);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")

	if _, err := be.DB().Exec(`CREATE TABLE actors (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`); err != nil {
		t.Fatalf("ddl: %v", err)
	}

	resp := do(t, srv, http.MethodGet, "/actors", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pre-reload status = %d, want 404", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST205" {
		t.Errorf("pre-reload code = %v, want PGRST205", env["code"])
	}

	if err := srv.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	resp = do(t, srv, http.MethodGet, "/actors", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-reload status = %d, want 200", resp.StatusCode)
	}

	resp = do(t, srv, http.MethodGet, "/", nil)
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	if _, ok := doc["paths"].(map[string]any)["/actors"]; !ok {
		t.Error("the document should describe the new table after reload")
	}
}

// newJSONColumnServer builds a server over a table with a JSON column, the
// shape the array round-trip tests need (films has none).
func newJSONColumnServer(t testing.TB) *httpapi.Server {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	_, err = be.DB().Exec(`
		CREATE TABLE todos (
			id   INTEGER PRIMARY KEY,
			task TEXT NOT NULL,
			tags JSON
		);
		INSERT INTO todos (id, task, tags) VALUES (1, 'write spec', '["pets"]');
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	return srv
}

// A JSON array in a write payload must land in a JSON column as JSON text and
// read back as the same array, not as PostgreSQL {a,b} literal text. This was
// the bug that corrupted tags columns on every PATCH/POST carrying an array.
func TestPatchJSONArrayRoundTrips(t *testing.T) {
	srv := newJSONColumnServer(t)
	resp := send(t, srv, http.MethodPatch, "/todos?id=eq.1", `{"tags":["go","sql"]}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("patch rows = %v", rows)
	}
	assertTags := func(stage string, v any) {
		t.Helper()
		tags, ok := v.([]any)
		if !ok || len(tags) != 2 || tags[0] != "go" || tags[1] != "sql" {
			t.Fatalf("%s tags = %#v, want [go sql]", stage, v)
		}
	}
	assertTags("representation", rows[0]["tags"])

	resp = do(t, srv, http.MethodGet, "/todos?id=eq.1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	rows = decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("get rows = %v", rows)
	}
	assertTags("stored", rows[0]["tags"])
}

func TestPostJSONArrayRoundTrips(t *testing.T) {
	srv := newJSONColumnServer(t)
	resp := send(t, srv, http.MethodPost, "/todos", `{"id":2,"task":"pack","tags":["home",2]}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post status = %d, want 201", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("post rows = %v", rows)
	}
	tags, ok := rows[0]["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "home" || tags[1] != float64(2) {
		t.Fatalf("tags = %#v, want [home 2]", rows[0]["tags"])
	}
}
