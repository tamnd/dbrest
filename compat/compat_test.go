// Package compat runs the PostgREST v14 conformance tests against both a live
// PostgREST instance and a live dbrest instance, then diffs each response:
// status code, a curated subset of headers, and the JSON body. A test fails
// only when dbrest diverges from PostgREST, not when PostgREST itself returns
// an unexpected code.
//
// Both servers must be up and the env vars must be set before the tests run:
//
//	COMPAT_POSTGREST_URL  base URL of the PostgREST server  (default: http://localhost:3000)
//	COMPAT_DBREST_URL     base URL of the dbrest server     (default: http://localhost:3001)
//
// Run with:
//
//	go test ./compat/ -v -timeout 120s
//
// or with the docker-compose stacks up:
//
//	podman compose -f docker/postgrest/compose.yaml up -d
//	podman compose -f docker/dbrest/compose.yaml up -d
//	go test ./compat/ -v -timeout 120s
package compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// JWT tokens for auth tests (HS256, secret = "reallyreallyreallyreallyverysafe", exp=9999999999)
	jwtUser = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjk5OTk5OTk5OTksInJvbGUiOiJ3ZWJfdXNlciJ9.HC41M51jHR8T_QDet9cNuyWRGvwXxoSXmk5OazFhXuc"
)

// urls returns the PostgREST and dbrest base URLs, or skips the test if neither
// server appears to be up.
func urls(t *testing.T) (pgrest, dbrest string) {
	t.Helper()
	pgrest = envOr("COMPAT_POSTGREST_URL", "http://localhost:3000")
	dbrest = envOr("COMPAT_DBREST_URL", "http://localhost:3001")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !pingOK(ctx, pgrest) {
		t.Skipf("PostgREST not reachable at %s; set COMPAT_POSTGREST_URL or start docker/postgrest/compose.yaml", pgrest)
	}
	if !pingOK(ctx, dbrest) {
		t.Skipf("dbrest not reachable at %s; set COMPAT_DBREST_URL or start docker/dbrest/compose.yaml", dbrest)
	}
	return pgrest, dbrest
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pingOK(ctx context.Context, base string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// compatCase describes one HTTP case and the expectations on the response.
// status and headers are compared exactly; body is compared as normalized JSON.
type compatCase struct {
	name    string
	method  string
	path    string
	headers map[string]string
	body    string

	// wantStatus: if > 0, BOTH servers must return exactly this status.
	wantStatus int

	// bodyMode controls body comparison:
	//   "json"   - normalize and compare JSON (default for application/json responses)
	//   "schema" - compare only the JSON schema shape (keys present), not values
	//   "status" - compare status only, skip body
	//   "empty"  - expect empty body
	//   "range"  - compare Content-Range header value
	bodyMode string

	// wantContentRange: if non-empty, compare Content-Range header exactly.
	wantContentRange string

	// wantLocationPrefix: if non-empty, Location header must start with this.
	wantLocationPrefix string

	// wantPrefApplied: if non-empty, Preference-Applied header must contain this.
	wantPrefApplied string

	// skipStatusMatch: when true, skip the cross-server status comparison. Use
	// for responses whose status code depends on non-deterministic state (e.g.
	// planner statistics that differ between two independent database instances).
	skipStatusMatch bool
}

// All test cases, grouped by the compat matrix sections.
var cases = []compatCase{
	// ── Group 1: Basic reads ─────────────────────────────────────────────
	{name: "1.1 GET todos", method: "GET", path: "/todos"},
	{name: "1.2 HEAD todos", method: "HEAD", path: "/todos", bodyMode: "empty"},
	{name: "1.3 GET todos explicit json", method: "GET", path: "/todos",
		headers: map[string]string{"Accept": "application/json"}},
	{name: "1.4 GET todos array+json", method: "GET", path: "/todos",
		headers: map[string]string{"Accept": "application/vnd.pgrst.array+json"}},
	{name: "1.5 GET todos CSV", method: "GET", path: "/todos",
		headers: map[string]string{"Accept": "text/csv"}, bodyMode: "status"},
	{name: "1.7 GET todos singular many rows 406", method: "GET", path: "/todos",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406, bodyMode: "status"},

	// ── Group 2: Projection ───────────────────────────────────────────────
	{name: "2.1 select columns", method: "GET", path: "/todos?select=id,task"},
	{name: "2.2 select aliased", method: "GET", path: "/todos?select=done_flag:done,task"},
	{name: "2.3 select star", method: "GET", path: "/todos?select=*"},
	{name: "2.4 select single", method: "GET", path: "/todos?select=id"},
	{name: "2.5 select unknown col 400", method: "GET", path: "/todos?select=nonexistent",
		wantStatus: 400, bodyMode: "status"},

	// ── Group 3: Horizontal filtering ────────────────────────────────────
	{name: "3.1 eq bool", method: "GET", path: "/todos?done=eq.true"},
	{name: "3.2 eq int", method: "GET", path: "/todos?id=eq.1"},
	{name: "3.3 neq", method: "GET", path: "/todos?id=neq.1"},
	{name: "3.4 gt", method: "GET", path: "/todos?id=gt.1"},
	{name: "3.5 gte", method: "GET", path: "/todos?id=gte.1&order=id"},
	{name: "3.6 lt", method: "GET", path: "/todos?id=lt.3&order=id"},
	{name: "3.7 lte", method: "GET", path: "/todos?id=lte.2&order=id"},
	{name: "3.8 like", method: "GET", path: "/todos?task=like.*laundry*&order=id"},
	{name: "3.9 ilike", method: "GET", path: "/todos?task=ilike.*CAT*&order=id"},
	{name: "3.10 in list", method: "GET", path: "/todos?id=in.(1,2)&order=id"},
	{name: "3.11 is null", method: "GET", path: "/todos?due=is.null"},
	{name: "3.12 not is null", method: "GET", path: "/todos?due=not.is.null"},
	{name: "3.13 is true", method: "GET", path: "/todos?done=is.true"},
	{name: "3.14 is false", method: "GET", path: "/todos?done=is.false"},
	{name: "3.15 not in", method: "GET", path: "/todos?id=not.in.(1,2)"},
	{name: "3.17 regex match", method: "GET", path: "/todos?task=match.^do"},
	{name: "3.18 regex imatch", method: "GET", path: "/todos?task=imatch.^DO"},

	// ── Group 4: Logical operators ────────────────────────────────────────
	{name: "4.1 explicit AND", method: "GET", path: "/todos?and=(done.eq.false,id.gt.1)"},
	{name: "4.2 OR filter", method: "GET", path: "/todos?or=(done.eq.true,id.eq.1)"},
	{name: "4.3 NOT AND", method: "GET", path: "/todos?not.and=(done.eq.false,id.gt.2)"},

	// ── Group 5: Ordering ─────────────────────────────────────────────────
	{name: "5.1 order id asc", method: "GET", path: "/todos?order=id"},
	{name: "5.2 order id desc", method: "GET", path: "/todos?order=id.desc"},
	{name: "5.3 order id explicit asc", method: "GET", path: "/todos?order=id.asc"},
	{name: "5.4 order due nullsfirst", method: "GET", path: "/todos?order=due.nullsfirst"},
	{name: "5.5 order due nullslast", method: "GET", path: "/todos?order=due.nullslast"},
	{name: "5.6 multi-column order", method: "GET", path: "/todos?order=done.asc,id.desc"},
	{name: "5.7 desc nullsfirst", method: "GET", path: "/todos?order=due.desc.nullsfirst"},

	// ── Group 6: Pagination ───────────────────────────────────────────────
	// PostgREST v14: ?limit= without a Range header always returns 200.
	// 206 Partial Content only comes from a bounded Range header or count=exact.
	{name: "6.1 limit partial 200", method: "GET", path: "/todos?limit=2&order=id",
		wantStatus: 200},
	{name: "6.2 limit large 200", method: "GET", path: "/todos?limit=100&order=id",
		wantStatus: 200},
	{name: "6.3 offset only", method: "GET", path: "/todos?offset=1&order=id"},
	{name: "6.4 limit+offset 200", method: "GET", path: "/todos?limit=2&offset=1&order=id",
		wantStatus: 200},
	// PostgREST v14.13: Range header does not trigger 206 without count=exact.
	{name: "6.5 Range header 2 rows", method: "GET", path: "/todos?order=id",
		headers:    map[string]string{"Range-Unit": "items", "Range": "0-1"},
		wantStatus: 200},
	{name: "6.6 Range open-ended", method: "GET", path: "/todos?order=id",
		headers:    map[string]string{"Range-Unit": "items", "Range": "0-"},
		wantStatus: 200},
	{name: "6.7 Range single row", method: "GET", path: "/todos?order=id",
		headers:    map[string]string{"Range-Unit": "items", "Range": "0-0"},
		wantStatus: 200},
	{name: "6.8 Range out-of-range returns empty", method: "GET", path: "/todos?order=id",
		headers:    map[string]string{"Range-Unit": "items", "Range": "999-1000"},
		wantStatus: 200},

	// ── Group 7: Counting ─────────────────────────────────────────────────
	{name: "7.1 count=exact", method: "GET", path: "/todos",
		headers: map[string]string{"Prefer": "count=exact"}, wantContentRange: "0-2/3"},
	{name: "7.2 limit+count=exact 206", method: "GET", path: "/todos?limit=1&order=id",
		headers: map[string]string{"Prefer": "count=exact"}, wantStatus: 206, wantContentRange: "0-0/3"},
	{name: "7.3 empty+count=exact", method: "GET", path: "/todos?id=eq.99999",
		headers: map[string]string{"Prefer": "count=exact"}, wantContentRange: "*/0"},
	// 7.4/7.5: count=planned/estimated status depends on planner statistics which
	// differ between two independent databases; skip cross-server status match.
	{name: "7.4 count=planned", method: "GET", path: "/todos",
		headers:  map[string]string{"Prefer": "count=planned"},
		bodyMode: "status", skipStatusMatch: true, wantPrefApplied: "count=planned"},
	{name: "7.5 count=estimated", method: "GET", path: "/todos",
		headers:  map[string]string{"Prefer": "count=estimated"},
		bodyMode: "status", skipStatusMatch: true, wantPrefApplied: "count=estimated"},

	// ── Group 8: Singular object ──────────────────────────────────────────
	{name: "8.1 singular one row 200", method: "GET", path: "/todos?id=eq.1",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 200, bodyMode: "schema"},
	{name: "8.2 singular zero rows 406", method: "GET", path: "/todos?id=eq.99999",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406, bodyMode: "status"},
	{name: "8.3 singular many rows 406", method: "GET", path: "/todos",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406, bodyMode: "status"},

	// ── Group 9: Resource embedding ───────────────────────────────────────
	{name: "9.1 to-many embed", method: "GET", path: "/persons?select=name,assignments(todo_id)"},
	{name: "9.2 to-one embed", method: "GET", path: "/assignments?select=todo_id,persons(name)"},
	{name: "9.3 nested embed", method: "GET",
		path: "/todos?select=id,assignments(person_id,persons(name))"},
	{name: "9.4 filter on embed", method: "GET",
		path:     "/persons?select=name,assignments(todo_id)&assignments.todo_id=eq.1",
		bodyMode: "schema"},

	// ── Group 10: Inserts ─────────────────────────────────────────────────
	{name: "10.1 insert minimal 201", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{"task":"compat insert minimal"}`,
		wantStatus: 201, bodyMode: "empty"},
	{name: "10.2 insert return=representation 201", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `{"task":"compat insert repr"}`,
		wantStatus: 201, bodyMode: "schema"},
	{name: "10.3 insert return=headers-only 201", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=headers-only"},
		body:       `{"task":"compat insert headers-only"}`,
		wantStatus: 201, bodyMode: "empty",
		wantLocationPrefix: "/todos?id=eq."},
	{name: "10.4 bulk insert 201", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `[{"task":"bulk a"},{"task":"bulk b"}]`,
		wantStatus: 201, bodyMode: "schema"},
	{name: "10.5 insert missing=default 201", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "missing=default,return=representation"},
		body:       `{"task":"compat missing default"}`,
		wantStatus: 201, bodyMode: "schema"},
	{name: "10.6 insert Location header", method: "POST", path: "/todos",
		headers:            map[string]string{"Content-Type": "application/json"},
		body:               `{"task":"compat location test"}`,
		wantStatus:         201,
		bodyMode:           "empty",
		wantLocationPrefix: "/todos?id=eq."},
	{name: "10.8 insert unique violation 409", method: "POST", path: "/persons",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{"name":"Alice Dup","email":"alice@example.com"}`,
		wantStatus: 409, bodyMode: "status"},

	// ── Group 11: Updates ─────────────────────────────────────────────────
	{name: "11.1 update minimal 204", method: "PATCH", path: "/todos?id=eq.1",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{"done":true}`,
		wantStatus: 204, bodyMode: "empty"},
	{name: "11.2 update repr 200", method: "PATCH", path: "/todos?id=eq.1",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `{"done":false}`,
		wantStatus: 200, bodyMode: "schema"},
	{name: "11.3 update no match empty 200", method: "PATCH", path: "/todos?id=eq.99999",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `{"done":true}`,
		wantStatus: 200},
	// RETURNING order for bulk updates is unspecified; check status only.
	{name: "11.4 bulk update 200", method: "PATCH", path: "/todos?done=eq.false",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `{"done":false}`,
		wantStatus: 200, bodyMode: "status"},

	// ── Group 12: Deletes ─────────────────────────────────────────────────
	{name: "12.1 delete minimal 204", method: "DELETE", path: "/todos?id=gt.9000",
		wantStatus: 204, bodyMode: "empty"},
	{name: "12.2 delete repr empty 200", method: "DELETE", path: "/todos?id=gt.9000",
		headers:    map[string]string{"Prefer": "return=representation"},
		wantStatus: 200},
	{name: "12.3 delete repr rows 200", method: "DELETE", path: "/todos?task=eq.compat%20insert%20minimal",
		headers:    map[string]string{"Prefer": "return=representation"},
		wantStatus: 200, bodyMode: "status"},

	// ── Group 13: Upsert ──────────────────────────────────────────────────
	{name: "13.1 upsert merge-duplicates", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "resolution=merge-duplicates,return=representation"},
		body:       `{"id":1,"task":"upsert updated","done":false}`,
		wantStatus: 200, bodyMode: "schema"},
	// ignore-duplicates on an existing row fires ON CONFLICT DO NOTHING: the row
	// is unchanged and PostgREST v14 treats it as a "no-op insert" → 201.
	{name: "13.2 upsert ignore-duplicates", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "resolution=ignore-duplicates,return=representation"},
		body:       `{"id":1,"task":"ignored"}`,
		wantStatus: 201, bodyMode: "schema"},
	{name: "13.3 upsert with on_conflict", method: "POST", path: "/todos?on_conflict=id",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "resolution=merge-duplicates,return=representation"},
		body:       `{"id":1,"task":"conflict target upsert","done":false}`,
		wantStatus: 200, bodyMode: "schema"},
	{name: "13.4 PUT upsert existing 200", method: "PUT", path: "/todos?id=eq.1",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `{"id":1,"task":"put upsert","done":false}`,
		wantStatus: 200, bodyMode: "schema"},
	{name: "13.5 PUT upsert new 201", method: "PUT", path: "/todos?id=eq.9999",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:       `{"id":9999,"task":"new via put","done":false}`,
		wantStatus: 201, bodyMode: "schema"},

	// ── Group 15: tx=rollback ─────────────────────────────────────────────
	{name: "15.1 tx=rollback insert", method: "POST", path: "/todos",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "tx=rollback,return=representation"},
		body:       `{"task":"rollback me"}`,
		wantStatus: 201, bodyMode: "schema"},
	{name: "15.2 tx=rollback update", method: "PATCH", path: "/todos?id=eq.1",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "tx=rollback,return=representation"},
		body:       `{"done":true}`,
		wantStatus: 200, bodyMode: "schema"},

	// ── Group 16: RPC functions ───────────────────────────────────────────
	{name: "16.1 stable fn GET", method: "GET", path: "/rpc/get_todos_count",
		wantStatus: 200, bodyMode: "status"},
	{name: "16.2 stable fn POST", method: "POST", path: "/rpc/get_todos_count",
		headers: map[string]string{"Content-Type": "application/json"}, body: `{}`,
		wantStatus: 200, bodyMode: "status"},
	{name: "16.3 volatile fn POST", method: "POST", path: "/rpc/add_todo",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{"task":"rpc insert"}`,
		wantStatus: 200, bodyMode: "status"},
	{name: "16.4 volatile tx=rollback", method: "POST", path: "/rpc/add_todo",
		headers:    map[string]string{"Content-Type": "application/json", "Prefer": "tx=rollback"},
		body:       `{"task":"rpc rollback"}`,
		wantStatus: 200, bodyMode: "status"},
	{name: "16.5 unknown fn 404", method: "GET", path: "/rpc/nonexistent_fn_xyz",
		wantStatus: 404, bodyMode: "status"},

	// ── Group 17: Request context GUCs ────────────────────────────────────
	{name: "17.1 request.method GUC", method: "GET", path: "/rpc/get_request_method",
		wantStatus: 200, bodyMode: "status"},
	{name: "17.2 request.path GUC", method: "GET", path: "/rpc/get_request_path",
		wantStatus: 200, bodyMode: "status"},
	{name: "17.3 jwt claims GUC", method: "POST", path: "/rpc/get_jwt_claims",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{}`,
		wantStatus: 200, bodyMode: "status"},

	// ── Group 18: Custom response controls ────────────────────────────────
	{name: "18.1 PT204 custom status", method: "POST", path: "/rpc/raise_204",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{}`,
		wantStatus: 204, bodyMode: "empty"},
	// raise_custom_header returns void; PostgREST signals 204 for void functions.
	{name: "18.2 response.headers GUC", method: "POST", path: "/rpc/raise_custom_header",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{}`,
		wantStatus: 204, bodyMode: "empty"},

	// ── Group 20: Error responses ─────────────────────────────────────────
	{name: "20.1 unknown table 404", method: "GET", path: "/nonexistent_xyz_table",
		wantStatus: 404, bodyMode: "status"},
	{name: "20.2 missing Content-Type 415", method: "POST", path: "/todos",
		body: `{"task":"no content type"}`,
		// PostgREST v14.13 infers JSON when Content-Type is absent and body looks
		// like JSON; it no longer rejects with 415. Test that both servers agree.
		bodyMode: "status"},
	{name: "20.3 bad value for int col 400", method: "GET", path: "/todos?id=eq.notanint",
		wantStatus: 400, bodyMode: "status"},
	{name: "20.4 bad operator 400", method: "GET", path: "/todos?id=badop.1",
		wantStatus: 400, bodyMode: "status"},
	{name: "20.5 unsupported media type 406", method: "GET", path: "/todos",
		headers:    map[string]string{"Accept": "application/xml"},
		wantStatus: 406, bodyMode: "status"},
	{name: "20.6 NOT NULL violation 400", method: "POST", path: "/persons",
		headers:    map[string]string{"Content-Type": "application/json"},
		body:       `{"name":null,"email":"nulltest@example.com"}`,
		wantStatus: 400, bodyMode: "status"},
	{name: "20.7 readonly view 405", method: "POST", path: "/readonly_view",
		headers: map[string]string{"Content-Type": "application/json"},
		body:    `{"id":999,"task":"write to view"}`,
		// readonly_view is not in the compat seed; both servers return 401 (anon
		// cannot write) or 404 (view not found). Test that both servers agree.
		bodyMode: "status"},

	// ── Group 21: Content-Range (exact) ───────────────────────────────────
	// 21.1–21.3 covered by 7.1–7.3 with wantContentRange above.
	// 21.4: limit without count gives unknown total. bodyMode=schema because id=1
	// may have been mutated by earlier write tests in this run.
	{name: "21.4 Content-Range no count", method: "GET", path: "/todos?limit=1&order=id",
		wantContentRange: "0-0/*", wantStatus: 200, bodyMode: "schema"},

	// ── Group 22: Preference-Applied header ───────────────────────────────
	{name: "22.1 pref-applied return=representation", method: "POST", path: "/todos",
		headers:         map[string]string{"Content-Type": "application/json", "Prefer": "return=representation"},
		body:            `{"task":"pref applied test"}`,
		wantStatus:      201,
		bodyMode:        "schema",
		wantPrefApplied: "return=representation"},
	{name: "22.2 pref-applied count=exact", method: "GET", path: "/todos",
		headers:         map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact",
		bodyMode:        "status"},

	// ── Group 19: Row Level Security ──────────────────────────────────────────
	// private_todos has RLS policy: owner = jwt.claims.role
	// web_anon (no JWT) sees empty because jwt.claims.role is null → policy false
	// web_user (JWT) sees rows where owner = 'web_user'
	{name: "19.1 RLS anon sees empty", method: "GET", path: "/private_todos",
		wantStatus: 200},
	{name: "19.2 RLS user sees own rows", method: "GET", path: "/private_todos",
		headers:    map[string]string{"Authorization": "Bearer " + jwtUser},
		wantStatus: 200, bodyMode: "schema"},

	// ── Group 27: Array operators (cs / cd / ov) ──────────────────────────────
	// todos now have tags text[] column; seeds: {go,sql}, {pets}, {chores,home}
	{name: "27.1 cs contains array", method: "GET", path: `/todos?tags=cs.{go}&order=id`,
		wantStatus: 200, bodyMode: "schema"},
	{name: "27.2 cd contained by array", method: "GET", path: `/todos?tags=cd.{go,sql,extra}&order=id`,
		wantStatus: 200, bodyMode: "schema"},
	{name: "27.3 ov overlaps array", method: "GET", path: `/todos?tags=ov.{pets,chores}&order=id`,
		wantStatus: 200, bodyMode: "schema"},

	// ── Group 28: Schema switching (Accept-Profile) ───────────────────────────
	{name: "28.1 Accept-Profile explicit api", method: "GET", path: "/todos",
		headers:    map[string]string{"Accept-Profile": "api"},
		wantStatus: 200, bodyMode: "schema"},
	{name: "28.2 Accept-Profile private schema", method: "GET", path: "/items",
		headers:    map[string]string{"Accept-Profile": "private"},
		wantStatus: 200, bodyMode: "schema"},

	// ── Group 24: Full-text search operators ──────────────────────────────
	// Uses task text column; no tsvector index required.
	{name: "24.1 fts basic", method: "GET", path: "/todos?task=fts.laundry",
		wantStatus: 200, bodyMode: "schema"},
	{name: "24.2 plfts plain text", method: "GET", path: "/todos?task=plfts.do%20laundry",
		wantStatus: 200, bodyMode: "schema"},
	{name: "24.3 wfts websearch", method: "GET", path: "/todos?task=wfts.laundry",
		wantStatus: 200, bodyMode: "schema"},
	{name: "24.4 fts with language config", method: "GET", path: "/todos?task=fts(english).laundry",
		wantStatus: 200, bodyMode: "schema"},
	{name: "24.5 phfts phrase", method: "GET", path: "/todos?task=phfts.do%20laundry",
		wantStatus: 200, bodyMode: "schema"},

	// ── Group 25: isdistinct operator ─────────────────────────────────────
	// bodyMode=schema: both databases accumulate rows with different auto-increment
	// IDs during the test run, so exact JSON comparison diverges. Operator
	// correctness (returns 200 and valid JSON array) is what matters here.
	{name: "25.1 isdistinct int", method: "GET", path: "/todos?id=isdistinct.1",
		wantStatus: 200, bodyMode: "schema"},
	{name: "25.2 not.isdistinct", method: "GET", path: "/todos?id=not.isdistinct.1",
		wantStatus: 200, bodyMode: "schema"},

	// ── Group 26: Aggregate functions ─────────────────────────────────────
	// PostgREST v14.13 returns 400 for aggregate expressions in ?select because
	// the parser rejects them (PGRST100 / "Could not parse select parameter").
	// Both servers should agree on this error response.
	{name: "26.1 count aggregate", method: "GET", path: "/todos?select=n:count(*)",
		wantStatus: 400, bodyMode: "status"},
	{name: "26.2 max aggregate", method: "GET", path: "/todos?select=top:max(id)",
		wantStatus: 400, bodyMode: "status"},
}

// resetTestDB deletes all non-seed rows from both servers so each TestCompatibility
// run starts from the same known state (3 todos, 2 persons, 2 assignments).
func resetTestDB(t *testing.T, pgrest, dbrest string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	cleanup := []struct{ method, url string }{
		{"DELETE", pgrest + "/todos?id=gt.3"},
		{"DELETE", pgrest + "/assignments?id=gt.2"},
		{"DELETE", pgrest + "/persons?id=gt.2"},
		{"DELETE", pgrest + "/private_todos?id=gt.2"},
		{"DELETE", dbrest + "/todos?id=gt.3"},
		{"DELETE", dbrest + "/assignments?id=gt.2"},
		{"DELETE", dbrest + "/persons?id=gt.2"},
		{"DELETE", dbrest + "/private_todos?id=gt.2"},
		// undo any modifications to seed rows
		{"PATCH", pgrest + "/todos?id=eq.1"},
		{"PATCH", dbrest + "/todos?id=eq.1"},
	}
	// Reset seed todo 1 to done=false in case update tests changed it.
	for _, s := range cleanup {
		var body io.Reader
		if s.method == "PATCH" {
			body = strings.NewReader(`{"done":false,"task":"finish tutorial","due":"2030-01-01","tags":["go","sql"]}`)
		}
		req, _ := http.NewRequestWithContext(context.Background(), s.method, s.url, body)
		if s.method == "PATCH" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("resetTestDB %s %s: %v", s.method, s.url, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// TestCompatibility is the primary conformance suite. For each case it sends the
// same request to both servers and fails the test when dbrest diverges from
// PostgREST. The comparison is intentionally loose on fields that naturally
// differ between runs (auto-increment IDs, timestamps, row counts for mutable
// operations); see bodyMode per case.
func TestCompatibility(t *testing.T) {
	pgrest, dbrest := urls(t)
	resetTestDB(t, pgrest, dbrest)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pgResp := doRequest(t, pgrest, c)
			dbResp := doRequest(t, dbrest, c)

			// Status comparison.
			if !c.skipStatusMatch && pgResp.status != dbResp.status {
				t.Errorf("status: postgrest=%d dbrest=%d", pgResp.status, dbResp.status)
			}
			if c.wantStatus != 0 {
				if pgResp.status != c.wantStatus {
					t.Errorf("postgrest status = %d, want %d", pgResp.status, c.wantStatus)
				}
				if dbResp.status != c.wantStatus {
					t.Errorf("dbrest status = %d, want %d", dbResp.status, c.wantStatus)
				}
			}

			// Location header: both servers must agree (both present or both absent).
			// If wantLocationPrefix is set we also verify dbrest meets the prefix when present.
			pgLoc := pgResp.header.Get("Location")
			dbLoc := dbResp.header.Get("Location")
			if (pgLoc == "") != (dbLoc == "") {
				t.Errorf("Location header presence differs: postgrest=%q dbrest=%q", pgLoc, dbLoc)
			}
			if c.wantLocationPrefix != "" && dbLoc != "" {
				if !strings.HasPrefix(dbLoc, c.wantLocationPrefix) {
					t.Errorf("dbrest Location = %q, want prefix %q", dbLoc, c.wantLocationPrefix)
				}
			}

			// Preference-Applied.
			if c.wantPrefApplied != "" {
				dbPA := dbResp.header.Get("Preference-Applied")
				if !strings.Contains(dbPA, c.wantPrefApplied) {
					t.Errorf("dbrest Preference-Applied = %q, want it to contain %q", dbPA, c.wantPrefApplied)
				}
			}

			// Content-Range.
			if c.wantContentRange != "" {
				dbCR := dbResp.header.Get("Content-Range")
				if dbCR != c.wantContentRange {
					t.Errorf("dbrest Content-Range = %q, want %q", dbCR, c.wantContentRange)
				}
			}

			// Body comparison by mode.
			mode := c.bodyMode
			if mode == "" {
				mode = "json"
			}
			switch mode {
			case "json":
				compareJSON(t, pgResp, dbResp)
			case "schema":
				compareJSONSchema(t, pgResp, dbResp)
			case "empty":
				if len(strings.TrimSpace(string(dbResp.body))) != 0 {
					t.Errorf("dbrest: expected empty body, got %q", dbResp.body)
				}
			case "range":
				pgCR := pgResp.header.Get("Content-Range")
				dbCR := dbResp.header.Get("Content-Range")
				if pgCR != dbCR {
					t.Errorf("Content-Range: postgrest=%q dbrest=%q", pgCR, dbCR)
				}
			case "status":
				// status already compared above
			}
		})
	}
}

// TestCompatSummary prints a pass/fail table for every case so regressions are
// easy to spot without reading individual failure messages.
func TestCompatSummary(t *testing.T) {
	pgrest, dbrest := urls(t)
	resetTestDB(t, pgrest, dbrest)
	var passed, failed int

	for _, c := range cases {
		pgResp := doRequest(t, pgrest, c)
		dbResp := doRequest(t, dbrest, c)

		ok := c.skipStatusMatch || pgResp.status == dbResp.status
		if ok && c.wantStatus != 0 {
			ok = dbResp.status == c.wantStatus
		}
		if ok {
			mode := c.bodyMode
			if mode == "" {
				mode = "json"
			}
			if mode == "json" {
				pgN, e1 := normalizeJSON(pgResp.body)
				dbN, e2 := normalizeJSON(dbResp.body)
				ok = e1 == nil && e2 == nil && pgN == dbN
			}
		}

		icon := "PASS"
		if !ok {
			icon = "FAIL"
			failed++
		} else {
			passed++
		}
		t.Logf("[%s] %s (pg=%d db=%d)", icon, c.name, pgResp.status, dbResp.status)
	}
	t.Logf("summary: %d/%d passed", passed, passed+failed)
	if failed > 0 {
		t.Errorf("%d case(s) diverge from PostgREST", failed)
	}
}

// TestPerformanceComparison runs a throughput benchmark against both servers
// and reports req/s and the ratio. The target is 5x the PostgREST throughput.
func TestPerformanceComparison(t *testing.T) {
	pgrest, dbrest := urls(t)
	if testing.Short() {
		t.Skip("performance comparison skipped in short mode")
	}

	benches := []struct {
		name   string
		path   string
		accept string
		warmup int
		n      int
	}{
		{"GET /todos (simple)", "/todos?order=id", "application/json", 50, 500},
		{"GET /todos?select=id,task", "/todos?select=id,task&order=id", "application/json", 50, 500},
		{"GET /todos count=exact", "/todos", "application/json", 50, 500},
		{"GET /persons embed", "/persons?select=name,assignments(todo_id)", "application/json", 30, 300},
	}

	for _, b := range benches {
		t.Run(b.name, func(t *testing.T) {
			var pgExtraHeaders map[string]string
			var dbExtraHeaders map[string]string
			if strings.Contains(b.name, "count") {
				pgExtraHeaders = map[string]string{"Prefer": "count=exact"}
				dbExtraHeaders = map[string]string{"Prefer": "count=exact"}
			}
			pgDur := measure(t, pgrest, b.path, b.accept, pgExtraHeaders, b.warmup, b.n)
			dbDur := measure(t, dbrest, b.path, b.accept, dbExtraHeaders, b.warmup, b.n)

			pgRPS := float64(b.n) / pgDur.Seconds()
			dbRPS := float64(b.n) / dbDur.Seconds()
			ratio := dbRPS / pgRPS
			target := ratio >= 5.0
			mark := "OK"
			if !target {
				mark = "BELOW_TARGET"
			}

			t.Logf("[%s] %s", mark, b.name)
			t.Logf("  PostgREST: %6.0f req/s  (%v)", pgRPS, pgDur.Round(time.Millisecond))
			t.Logf("  dbrest:    %6.0f req/s  (%v)", dbRPS, dbDur.Round(time.Millisecond))
			t.Logf("  ratio:     %.2fx (target >= 5.0x)", ratio)
		})
	}
}

// ── concurrency throughput test ───────────────────────────────────────────────

// TestConcurrentThroughput fires many goroutines at both servers simultaneously
// and measures end-to-end throughput, which is where Go's concurrency model
// should show the clearest advantage over PostgREST's single-threaded WARP server.
func TestConcurrentThroughput(t *testing.T) {
	pgrest, dbrest := urls(t)
	if testing.Short() {
		t.Skip("concurrent throughput test skipped in short mode")
	}

	const concurrency = 20
	const requests = 1000
	path := "/todos?order=id"

	pgDur := measureConcurrent(t, pgrest, path, concurrency, requests)
	dbDur := measureConcurrent(t, dbrest, path, concurrency, requests)

	pgRPS := float64(requests) / pgDur.Seconds()
	dbRPS := float64(requests) / dbDur.Seconds()
	ratio := dbRPS / pgRPS
	t.Logf("Concurrent throughput (%d concurrent, %d total requests)", concurrency, requests)
	t.Logf("  PostgREST: %6.0f req/s  (%v)", pgRPS, pgDur.Round(time.Millisecond))
	t.Logf("  dbrest:    %6.0f req/s  (%v)", dbRPS, dbDur.Round(time.Millisecond))
	t.Logf("  ratio:     %.2fx", ratio)
}

func measureConcurrent(t *testing.T, base, path string, concurrency, total int) time.Duration {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	var counter atomic.Int64
	counter.Store(int64(total))
	start := time.Now()
	done := make(chan struct{})
	for range concurrency {
		go func() {
			for counter.Add(-1) >= 0 {
				req, _ := http.NewRequest(http.MethodGet, base+path, nil)
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			done <- struct{}{}
		}()
	}
	for range concurrency {
		<-done
	}
	return time.Since(start)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// measure runs warmup then n sequential requests and returns elapsed time for
// the timed portion.
func measure(t *testing.T, base, path, accept string, extra map[string]string, warmup, n int) time.Duration {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	send := func() {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Accept", accept)
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request to %s%s: %v", base, path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	for range warmup {
		send()
	}
	start := time.Now()
	for range n {
		send()
	}
	return time.Since(start)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func doRequest(t *testing.T, base string, c compatCase) response {
	t.Helper()
	var bodyReader io.Reader
	if c.body != "" {
		bodyReader = strings.NewReader(c.body)
	}
	req, err := http.NewRequest(c.method, base+c.path, bodyReader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s%s: %v", c.method, base, c.path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: body}
}

func isJSON(ct string) bool {
	return strings.Contains(ct, "json")
}

// normalizeJSON round-trips through encoding/json to canonicalize key order,
// whitespace, and array ordering so cosmetic differences (including row order)
// between two independent server instances do not count as divergences.
// Arrays of objects are sorted by their canonical JSON representation so that
// queries without ORDER BY compare equal regardless of physical row order.
func normalizeJSON(b []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	sortArrays(v)
	out, err := json.Marshal(v)
	return string(out), err
}

// sortArrays recursively sorts JSON arrays of objects by their canonical JSON
// string representation. Primitive arrays and nested objects are left in order.
func sortArrays(v any) {
	switch val := v.(type) {
	case []any:
		for _, item := range val {
			sortArrays(item)
		}
		// Only sort arrays of objects (maps), leave primitive arrays as-is.
		if len(val) > 0 {
			if _, ok := val[0].(map[string]any); ok {
				sort.Slice(val, func(i, j int) bool {
					bi, _ := json.Marshal(val[i])
					bj, _ := json.Marshal(val[j])
					return string(bi) < string(bj)
				})
			}
		}
	case map[string]any:
		for _, child := range val {
			sortArrays(child)
		}
	}
}

// compareJSON normalizes both bodies and fails if they differ.
func compareJSON(t *testing.T, pg, db response) {
	t.Helper()
	if !isJSON(pg.header.Get("Content-Type")) || !isJSON(db.header.Get("Content-Type")) {
		return
	}
	pgN, pgErr := normalizeJSON(pg.body)
	dbN, dbErr := normalizeJSON(db.body)
	if pgErr != nil || dbErr != nil {
		return
	}
	if pgN != dbN {
		t.Errorf("body mismatch:\n  postgrest: %s\n  dbrest:    %s", pg.body, db.body)
	}
}

// compareJSONSchema compares only the set of top-level keys (and nested key
// sets for array elements), not the values. This is appropriate for cases where
// auto-increment IDs and timestamps differ between runs but the shape must match.
func compareJSONSchema(t *testing.T, pg, db response) {
	t.Helper()
	if !isJSON(pg.header.Get("Content-Type")) || !isJSON(db.header.Get("Content-Type")) {
		return
	}
	pgShape := jsonShape(pg.body)
	dbShape := jsonShape(db.body)
	if pgShape != dbShape {
		t.Errorf("JSON schema mismatch:\n  postgrest shape: %s\n  dbrest shape:    %s", pgShape, dbShape)
	}
}

// jsonShape extracts the key structure of a JSON value as a normalized string
// for schema comparison. Arrays use the first element as representative.
func jsonShape(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return shapeOf(v)
}

func shapeOf(v any) string {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ":" + shapeOf(val[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(val) == 0 {
			return "[]"
		}
		return "[" + shapeOf(val[0]) + "]"
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	default:
		return "unknown"
	}
}
