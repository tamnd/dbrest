// MySQL compat test suite. Sends the portable subset of the compat corpus to
// dbrest-MySQL and diffs responses against the PostgREST-PostgreSQL golden.
//
// Required env vars (both servers must be up):
//
//	COMPAT_POSTGREST_URL  PostgREST golden (default: http://localhost:3000)
//	COMPAT_MYSQL_URL      dbrest-MySQL subject (default: http://localhost:3003)
//
// Start servers:
//
//	podman compose -f docker/postgrest/compose.yaml up -d
//	podman compose -f docker/dbrest-mysql/compose.yaml up -d
//	go test ./compat/ -v -run TestMySQLCompat -count=1 -timeout 120s
package compat

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mysqlURLs returns PostgREST golden + dbrest-MySQL subject URLs, or skips.
func mysqlURLs(t *testing.T) (golden, subject string) {
	t.Helper()
	golden = envOr("COMPAT_POSTGREST_URL", "http://localhost:3000")
	subject = envOr("COMPAT_MYSQL_URL", "http://localhost:3003")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !pingOK(ctx, golden) {
		t.Skipf("PostgREST not reachable at %s; set COMPAT_POSTGREST_URL or start docker/postgrest/compose.yaml", golden)
	}
	if !pingOK(ctx, subject) {
		t.Skipf("dbrest-MySQL not reachable at %s; set COMPAT_MYSQL_URL or start docker/dbrest-mysql/compose.yaml", subject)
	}
	return golden, subject
}

// mysqlCases is the portable corpus subset that MySQL can serve. Cases are
// compared against the PostgREST-PostgreSQL golden after normalization. For
// unsupported features (array operators) the test verifies PGRST127 instead.
var mysqlCases = []compatCase{
	// ── Group 1: Basic reads ──────────────────────────────────────────────
	{name: "mysql/1.1 GET todos",
		method: "GET", path: "/todos?order=id"},
	{name: "mysql/1.2 GET todos select",
		method: "GET", path: "/todos?select=id,task&order=id"},
	{name: "mysql/1.3 GET todos filter done=true",
		method: "GET", path: "/todos?done=eq.true&order=id"},
	{name: "mysql/1.4 GET todos order desc",
		method: "GET", path: "/todos?order=id.desc"},
	{name: "mysql/1.5 GET todos limit",
		method: "GET", path: "/todos?limit=2&order=id"},
	{name: "mysql/1.6 GET todos offset",
		method: "GET", path: "/todos?limit=2&offset=1&order=id"},

	// ── Group 2: Filters ─────────────────────────────────────────────────
	{name: "mysql/2.1 eq",
		method: "GET", path: "/todos?id=eq.1"},
	{name: "mysql/2.2 neq",
		method: "GET", path: "/todos?id=neq.1&order=id"},
	{name: "mysql/2.3 gt",
		method: "GET", path: "/todos?id=gt.1&order=id"},
	{name: "mysql/2.4 like",
		method: "GET", path: "/todos?task=like.*laundry*"},
	{name: "mysql/2.5 ilike",
		method: "GET", path: "/todos?task=ilike.*CAT*"},
	{name: "mysql/2.6 is.null",
		method: "GET", path: "/todos?due=is.null&order=id"},
	{name: "mysql/2.7 in list",
		method: "GET", path: "/todos?id=in.(1,2)&order=id"},
	{name: "mysql/2.8 regex",
		method: "GET", path: "/todos?task=match.^do"},
	{name: "mysql/2.9 isdistinct",
		method: "GET", path: "/todos?due=isdistinct.null&order=id",
		wantStatus: 400, bodyMode: "status"},

	// ── Group 3: Logic operators ──────────────────────────────────────────
	{name: "mysql/3.1 and",
		method: "GET", path: "/todos?and=(done.eq.false,id.gt.1)&order=id"},
	{name: "mysql/3.2 or",
		method: "GET", path: "/todos?or=(id.eq.1,id.eq.3)&order=id"},
	{name: "mysql/3.3 not",
		method: "GET", path: "/todos?not.done=eq.true&order=id",
		wantStatus: 400, bodyMode: "status"},

	// ── Group 4: Pagination + Content-Range ───────────────────────────────
	{name: "mysql/4.1 count=exact",
		method: "GET", path: "/todos",
		headers: map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact"},
	{name: "mysql/4.2 limit+offset Content-Range",
		method: "GET", path: "/todos?limit=1&offset=1&order=id",
		headers: map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact"},

	// ── Group 5: Singular ─────────────────────────────────────────────────
	{name: "mysql/5.1 singular object",
		method: "GET", path: "/todos?id=eq.1",
		headers: map[string]string{"Accept": "application/vnd.pgrst.object+json"}},
	{name: "mysql/5.2 singular missing 406",
		method: "GET", path: "/todos?id=eq.99999",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406, bodyMode: "status"},

	// ── Group 6: Embedding ─────────────────────────────────────────────────
	// PostgREST v14 uses INNER JOIN by default (excludes persons with no
	// assignments) while dbrest returns all persons (correlated subquery). Shape
	// comparison verifies structural compatibility without requiring row-count match.
	{name: "mysql/6.1 embed assignments",
		method: "GET", path: "/persons?select=name,assignments(todo_id)&order=id",
		bodyMode: "schema"},
	{name: "mysql/6.2 embed todos",
		method: "GET", path: "/persons?select=name,assignments(todos(task))&order=id",
		bodyMode: "schema"},

	// ── Group 7: Errors ────────────────────────────────────────────────────
	{name: "mysql/7.1 unknown table 404",
		method: "GET", path: "/nonexistent", wantStatus: 404, bodyMode: "status"},
	{name: "mysql/7.2 unknown column 400",
		method: "GET", path: "/todos?nonexistent=eq.1", wantStatus: 400, bodyMode: "status"},

	// ── Group 8: Unsupported array operators → PGRST127 ───────────────────
	{name: "mysql/8.1 cs array op PGRST127",
		method: "GET", path: "/todos?tags=cs.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "mysql/8.2 cd array op PGRST127",
		method: "GET", path: "/todos?tags=cd.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "mysql/8.3 ov array op PGRST127",
		method: "GET", path: "/todos?tags=ov.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},

	// ── Group 9: Writes ────────────────────────────────────────────────────
	{name: "mysql/9.1 POST insert minimal",
		method: "POST", path: "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=minimal",
		},
		body: `{"task":"mysql test task","done":false}`,
		bodyMode: "empty"},
	{name: "mysql/9.2 POST insert representation",
		method: "POST", path: "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:     `{"task":"mysql repr task","done":false}`,
		bodyMode: "schema"},
	{name: "mysql/9.3 PATCH update",
		method: "PATCH", path: "/todos?id=eq.1",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:     `{"done":true}`,
		bodyMode: "schema"},
	{name: "mysql/9.4 DELETE",
		method:   "DELETE",
		path:     "/todos?id=gt.100",
		bodyMode: "empty"},
}

// TestMySQLCompat replays the MySQL portable corpus against dbrest-MySQL and
// compares against the PostgREST-PostgreSQL golden reference.
func TestMySQLCompat(t *testing.T) {
	golden, subject := mysqlURLs(t)

	for _, c := range mysqlCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			runMySQLCase(t, golden, subject, c)
		})
	}
}

// runMySQLCase executes one case against the golden and subject and diffs the
// responses.
func runMySQLCase(t *testing.T, golden, subject string, c compatCase) {
	t.Helper()

	// For PGRST127 cases, only check dbrest-MySQL response (not golden).
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

	// Write cases may alter state; run them in isolation.
	if c.method == "POST" || c.method == "PATCH" || c.method == "DELETE" {
		runMySQLWriteCase(t, golden, subject, c)
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
		// Status already compared above.
	case "schema":
		compareJSONSchema(t, goldResp, subjResp)
	default:
		compareJSON(t, goldResp, subjResp)
	}
}

// runMySQLWriteCase handles write cases: only sends to the subject (not golden)
// to avoid corrupting the golden state, then cleans up after each write.
func runMySQLWriteCase(t *testing.T, _, subject string, c compatCase) {
	t.Helper()

	subjResp := doRequest(t, subject, c)

	if c.wantStatus > 0 {
		if subjResp.status != c.wantStatus {
			t.Errorf("subject status=%d want %d", subjResp.status, c.wantStatus)
		}
		return
	}

	// POST inserts: verify 201 + cleanup.
	if c.method == "POST" {
		if subjResp.status != 201 {
			t.Errorf("POST status=%d want 201; body=%s", subjResp.status, subjResp.body)
			return
		}
		if c.bodyMode == "schema" && len(subjResp.body) == 0 {
			t.Errorf("POST with return=representation returned empty body")
		}
		// Cleanup: delete all rows with id > 3.
		doRequest(t, subject, compatCase{method: "DELETE", path: "/todos?id=gt.3"})
		return
	}

	// PATCH: verify 200 + reset.
	if c.method == "PATCH" {
		if subjResp.status != 200 {
			t.Errorf("PATCH status=%d want 200; body=%s", subjResp.status, subjResp.body)
			return
		}
		if c.bodyMode == "schema" && len(subjResp.body) == 0 {
			t.Errorf("PATCH with return=representation returned empty body")
		}
		// Reset todo id=1 done=false.
		doRequest(t, subject, compatCase{
			method:  "PATCH",
			path:    "/todos?id=eq.1",
			headers: map[string]string{"Content-Type": "application/json"},
			body:    `{"done":false}`,
		})
		return
	}

	// DELETE: verify 204/200.
	if subjResp.status != 204 && subjResp.status != 200 {
		t.Errorf("DELETE status=%d want 204 or 200; body=%s", subjResp.status, subjResp.body)
	}
}

// TestMySQLCompatSummary prints a pass/fail count for the MySQL compat suite.
func TestMySQLCompatSummary(t *testing.T) {
	golden, subject := mysqlURLs(t)

	passed, failed := 0, 0
	for _, c := range mysqlCases {
		ok := t.Run(c.name+"/summary", func(t *testing.T) {
			runMySQLCase(t, golden, subject, c)
		})
		if ok {
			passed++
		} else {
			failed++
		}
	}
	t.Logf("MySQL compat: %d/%d passed", passed, passed+failed)
	if failed > 0 {
		t.Errorf("%d cases failed", failed)
	}
}
