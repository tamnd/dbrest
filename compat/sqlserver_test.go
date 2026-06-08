// SQL Server compat test suite. Sends the portable subset of the compat corpus
// to dbrest-SQL Server and diffs responses against the PostgREST-PostgreSQL golden.
//
// Required env vars (both servers must be up):
//
//	COMPAT_POSTGREST_URL   PostgREST golden (default: http://localhost:3000)
//	COMPAT_SQLSERVER_URL   dbrest-SQL Server subject (default: http://localhost:3004)
//
// Start servers:
//
//	podman compose -f docker/postgrest/compose.yaml up -d
//	podman compose -f docker/dbrest-sqlserver/compose.yaml up -d
//	go test ./compat/ -v -run TestSQLServerCompat -count=1 -timeout 120s
package compat

import (
	"context"
	"strings"
	"testing"
	"time"
)

// sqlserverURLs returns PostgREST golden + dbrest-SQL Server subject URLs, or skips.
func sqlserverURLs(t *testing.T) (golden, subject string) {
	t.Helper()
	golden = envOr("COMPAT_POSTGREST_URL", "http://localhost:3000")
	subject = envOr("COMPAT_SQLSERVER_URL", "http://localhost:3004")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !pingOK(ctx, golden) {
		t.Skipf("PostgREST not reachable at %s; set COMPAT_POSTGREST_URL or start docker/postgrest/compose.yaml", golden)
	}
	if !pingOK(ctx, subject) {
		t.Skipf("dbrest-SQL Server not reachable at %s; set COMPAT_SQLSERVER_URL or start docker/dbrest-sqlserver/compose.yaml", subject)
	}
	return golden, subject
}

// sqlserverCases is the portable corpus subset that SQL Server can serve.
var sqlserverCases = []compatCase{
	// ── Group 1: Basic reads ──────────────────────────────────────────────
	{name: "sqlserver/1.1 GET todos",
		method: "GET", path: "/todos?order=id"},
	{name: "sqlserver/1.2 GET todos select",
		method: "GET", path: "/todos?select=id,task&order=id"},
	{name: "sqlserver/1.3 GET todos filter done=true",
		method: "GET", path: "/todos?done=eq.true&order=id"},
	{name: "sqlserver/1.4 GET todos order desc",
		method: "GET", path: "/todos?order=id.desc"},
	{name: "sqlserver/1.5 GET todos limit",
		method: "GET", path: "/todos?limit=2&order=id"},
	{name: "sqlserver/1.6 GET todos offset",
		method: "GET", path: "/todos?limit=2&offset=1&order=id"},

	// ── Group 2: Filters ─────────────────────────────────────────────────
	{name: "sqlserver/2.1 eq",
		method: "GET", path: "/todos?id=eq.1"},
	{name: "sqlserver/2.2 neq",
		method: "GET", path: "/todos?id=neq.1&order=id"},
	{name: "sqlserver/2.3 gt",
		method: "GET", path: "/todos?id=gt.1&order=id"},
	{name: "sqlserver/2.4 like",
		method: "GET", path: "/todos?task=like.*laundry*"},
	{name: "sqlserver/2.5 ilike",
		method: "GET", path: "/todos?task=ilike.*CAT*"},
	{name: "sqlserver/2.6 is.null",
		method: "GET", path: "/todos?due=is.null&order=id"},
	{name: "sqlserver/2.7 in list",
		method: "GET", path: "/todos?id=in.(1,2)&order=id"},
	// regex: REGEXP_LIKE requires SQL Server 2025 / Azure; on 2022 it is PGRST127.
	{name: "sqlserver/2.8 regex PGRST127",
		method: "GET", path: "/todos?task=match.^do",
		wantStatus: 400, bodyMode: "pgrst127"},
	// IS DISTINCT FROM: OpIsDistinct is not yet lowered in the shared compiler;
	// dbrest returns PGRST127 on all SQL backends for now.
	{name: "sqlserver/2.9 isdistinct PGRST127",
		method: "GET", path: "/todos?due=isdistinct.null&order=id",
		wantStatus: 400, bodyMode: "pgrst127"},

	// ── Group 3: Logic operators ──────────────────────────────────────────
	{name: "sqlserver/3.1 and",
		method: "GET", path: "/todos?and=(done.eq.false,id.gt.1)&order=id"},
	{name: "sqlserver/3.2 or",
		method: "GET", path: "/todos?or=(id.eq.1,id.eq.3)&order=id"},
	// not.col= syntax: PostgREST returns PGRST108, dbrest returns PGRST204 (same
	// status code, different PGRST code). Status-only comparison.
	{name: "sqlserver/3.3 not",
		method: "GET", path: "/todos?not.done=eq.true&order=id",
		wantStatus: 400, bodyMode: "status"},

	// ── Group 4: Pagination + Content-Range ───────────────────────────────
	{name: "sqlserver/4.1 count=exact",
		method: "GET", path: "/todos",
		headers: map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact"},
	{name: "sqlserver/4.2 limit+offset Content-Range",
		method: "GET", path: "/todos?limit=1&offset=1&order=id",
		headers: map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact"},

	// ── Group 5: Singular ─────────────────────────────────────────────────
	{name: "sqlserver/5.1 singular object",
		method: "GET", path: "/todos?id=eq.1",
		headers: map[string]string{"Accept": "application/vnd.pgrst.object+json"}},
	{name: "sqlserver/5.2 singular missing 406",
		method: "GET", path: "/todos?id=eq.99999",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406, bodyMode: "status"},

	// ── Group 6: Embedding ─────────────────────────────────────────────────
	// Azure SQL Edge is SQL Server 2019 (v15) which lacks JSON_OBJECT and
	// JSON_ARRAYAGG (added in SQL Server 2022). Embed compilation is therefore
	// PGRST127 on this test platform. On SQL Server 2022+ or Azure SQL Database
	// (EngineEdition=5) embeds work natively with JSON_OBJECT/JSON_ARRAYAGG.
	{name: "sqlserver/6.1 embed PGRST127",
		method: "GET", path: "/persons?select=name,assignments(todo_id)&order=id",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "sqlserver/6.2 embed PGRST127",
		method: "GET", path: "/persons?select=name,assignments(todos(task))&order=id",
		wantStatus: 400, bodyMode: "pgrst127"},

	// ── Group 7: Errors ────────────────────────────────────────────────────
	{name: "sqlserver/7.1 unknown table 404",
		method: "GET", path: "/nonexistent", wantStatus: 404, bodyMode: "status"},
	{name: "sqlserver/7.2 unknown column 400",
		method: "GET", path: "/todos?nonexistent=eq.1", wantStatus: 400, bodyMode: "status"},

	// ── Group 8: Unsupported array operators → PGRST127 ───────────────────
	{name: "sqlserver/8.1 cs array op PGRST127",
		method: "GET", path: "/todos?tags=cs.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "sqlserver/8.2 cd array op PGRST127",
		method: "GET", path: "/todos?tags=cd.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "sqlserver/8.3 ov array op PGRST127",
		method: "GET", path: "/todos?tags=ov.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},

	// ── Group 9: Writes ────────────────────────────────────────────────────
	{name: "sqlserver/9.1 POST insert minimal",
		method: "POST", path: "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=minimal",
		},
		body: `{"task":"sqlserver test task","done":false}`,
		bodyMode: "empty"},
	{name: "sqlserver/9.2 POST insert representation",
		method: "POST", path: "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:     `{"task":"sqlserver repr task","done":false}`,
		bodyMode: "schema"},
	{name: "sqlserver/9.3 PATCH update",
		method: "PATCH", path: "/todos?id=eq.1",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:     `{"done":true}`,
		bodyMode: "schema"},
	{name: "sqlserver/9.4 DELETE",
		method:   "DELETE",
		path:     "/todos?id=gt.100",
		bodyMode: "empty"},
}

// TestSQLServerCompat replays the SQL Server portable corpus against
// dbrest-SQL Server and compares against the PostgREST-PostgreSQL golden reference.
func TestSQLServerCompat(t *testing.T) {
	golden, subject := sqlserverURLs(t)

	for _, c := range sqlserverCases {
		t.Run(c.name, func(t *testing.T) {
			runSQLServerCase(t, golden, subject, c)
		})
	}
}

func runSQLServerCase(t *testing.T, golden, subject string, c compatCase) {
	t.Helper()

	if c.bodyMode == "pgrst127" {
		resp := doRequest(t, subject, c)
		if resp.status != 400 {
			t.Errorf("expected 400 for PGRST127 case, got %d", resp.status)
		}
		if !strings.Contains(string(resp.body), "PGRST127") {
			t.Errorf("expected PGRST127 in body, got: %s", resp.body)
		}
		return
	}

	if c.method == "POST" || c.method == "PATCH" || c.method == "DELETE" {
		runSQLServerWriteCase(t, golden, subject, c)
		return
	}

	goldResp := doRequest(t, golden, c)
	subjResp := doRequest(t, subject, c)

	if c.wantStatus > 0 {
		if goldResp.status != c.wantStatus {
			t.Errorf("golden status=%d want %d", goldResp.status, c.wantStatus)
		}
		if subjResp.status != c.wantStatus {
			t.Errorf("subject status=%d want %d", subjResp.status, c.wantStatus)
		}
		return
	}

	if goldResp.status != subjResp.status {
		t.Errorf("status mismatch: golden=%d subject=%d", goldResp.status, subjResp.status)
	}
	if c.wantPrefApplied != "" {
		got := subjResp.header.Get("Preference-Applied")
		if !strings.Contains(got, c.wantPrefApplied) {
			t.Errorf("Preference-Applied=%q does not contain %q", got, c.wantPrefApplied)
		}
	}

	switch c.bodyMode {
	case "empty":
		if len(subjResp.body) != 0 {
			t.Errorf("expected empty body, got: %s", subjResp.body)
		}
	case "status":
		// status already compared above
	case "schema":
		compareJSONSchema(t, goldResp, subjResp)
	default:
		compareJSON(t, goldResp, subjResp)
	}
}

func runSQLServerWriteCase(t *testing.T, _, subject string, c compatCase) {
	t.Helper()

	subjResp := doRequest(t, subject, c)

	if c.wantStatus > 0 {
		if subjResp.status != c.wantStatus {
			t.Errorf("subject status=%d want %d", subjResp.status, c.wantStatus)
		}
		return
	}

	if c.method == "POST" {
		if subjResp.status != 201 {
			t.Errorf("POST status=%d want 201; body=%s", subjResp.status, subjResp.body)
			return
		}
		if c.bodyMode == "schema" && len(subjResp.body) == 0 {
			t.Errorf("POST with return=representation returned empty body")
		}
		doRequest(t, subject, compatCase{method: "DELETE", path: "/todos?id=gt.3"})
		return
	}

	if c.method == "PATCH" {
		if subjResp.status != 200 {
			t.Errorf("PATCH status=%d want 200; body=%s", subjResp.status, subjResp.body)
			return
		}
		if c.bodyMode == "schema" && len(subjResp.body) == 0 {
			t.Errorf("PATCH with return=representation returned empty body")
		}
		doRequest(t, subject, compatCase{
			method:  "PATCH",
			path:    "/todos?id=eq.1",
			headers: map[string]string{"Content-Type": "application/json"},
			body:    `{"done":false}`,
		})
		return
	}

	if subjResp.status != 204 && subjResp.status != 200 {
		t.Errorf("DELETE status=%d want 204 or 200; body=%s", subjResp.status, subjResp.body)
	}
}

// TestSQLServerCompatSummary prints a pass/fail count for the SQL Server compat suite.
func TestSQLServerCompatSummary(t *testing.T) {
	golden, subject := sqlserverURLs(t)

	passed, failed := 0, 0
	for _, c := range sqlserverCases {
		ok := t.Run(c.name+"/summary", func(t *testing.T) {
			runSQLServerCase(t, golden, subject, c)
		})
		if ok {
			passed++
		} else {
			failed++
		}
	}
	t.Logf("SQL Server compat: %d/%d passed", passed, passed+failed)
	if failed > 0 {
		t.Errorf("%d cases failed", failed)
	}
}
