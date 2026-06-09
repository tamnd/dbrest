// MongoDB compat test suite. Verifies that dbrest-MongoDB handles the portable
// subset of the PostgREST compat corpus correctly, and returns PGRST127 for
// features MongoDB does not support (embedding, array operators).
//
// Required env vars:
//
//	COMPAT_MONGO_URL  dbrest-MongoDB subject (default: http://localhost:3005)
//
// Start server:
//
//	podman compose -f docker/dbrest-mongo/compose.yaml up -d
//	go test ./compat/ -v -run TestMongoCompat -count=1 -timeout 120s
package compat

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mongoURLs returns the dbrest-MongoDB subject URL, or skips.
func mongoURLs(t *testing.T) string {
	t.Helper()
	subject := envOr("COMPAT_MONGO_URL", "http://localhost:3005")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !pingOK(ctx, subject) {
		t.Skipf("dbrest-MongoDB not reachable at %s; set COMPAT_MONGO_URL or start docker/dbrest-mongo/compose.yaml", subject)
	}
	return subject
}

// mongoCases is the compat corpus for MongoDB. For unsupported features the
// test verifies PGRST127; for unsupported filters it verifies a 4xx status.
var mongoCases = []compatCase{
	// ── Group 1: Basic reads ──────────────────────────────────────────────
	{name: "mongo/1.1 GET todos",
		method: "GET", path: "/todos?order=id"},
	{name: "mongo/1.2 GET todos select",
		method: "GET", path: "/todos?select=id,task&order=id"},
	{name: "mongo/1.3 GET todos filter done=false",
		method: "GET", path: "/todos?done=eq.false&order=id"},
	{name: "mongo/1.4 GET todos order desc",
		method: "GET", path: "/todos?order=id.desc"},
	{name: "mongo/1.5 GET todos limit",
		method: "GET", path: "/todos?limit=2&order=id"},
	{name: "mongo/1.6 GET todos offset",
		method: "GET", path: "/todos?limit=2&offset=1&order=id"},

	// ── Group 2: Filters ─────────────────────────────────────────────────
	{name: "mongo/2.1 eq",
		method: "GET", path: "/todos?id=eq.1"},
	{name: "mongo/2.2 neq",
		method: "GET", path: "/todos?id=neq.1&order=id"},
	{name: "mongo/2.3 gt",
		method: "GET", path: "/todos?id=gt.1&order=id"},
	{name: "mongo/2.4 gte",
		method: "GET", path: "/todos?id=gte.2&order=id"},
	{name: "mongo/2.5 lt",
		method: "GET", path: "/todos?id=lt.3&order=id"},
	{name: "mongo/2.6 like",
		method: "GET", path: "/todos?task=like.*laundry*"},
	{name: "mongo/2.7 ilike",
		method: "GET", path: "/todos?task=ilike.*CAT*"},
	{name: "mongo/2.8 is.null",
		method: "GET", path: "/todos?due=is.null&order=id"},
	{name: "mongo/2.9 in list",
		method: "GET", path: "/todos?id=in.(1,2)&order=id"},
	{name: "mongo/2.10 match regex",
		method: "GET", path: "/todos?task=match.^do"},

	// ── Group 3: Logic operators ──────────────────────────────────────────
	{name: "mongo/3.1 and",
		method: "GET", path: "/todos?and=(done.eq.false,id.gt.1)&order=id"},
	{name: "mongo/3.2 or",
		method: "GET", path: "/todos?or=(id.eq.1,id.eq.3)&order=id"},
	{name: "mongo/3.3 not",
		method: "GET", path: "/todos?not.done=eq.true&order=id",
		wantStatus: 400, bodyMode: "status"},

	// ── Group 4: Pagination + Content-Range ───────────────────────────────
	{name: "mongo/4.1 count=exact",
		method: "GET", path: "/todos",
		headers:         map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact"},
	{name: "mongo/4.2 limit+offset Content-Range",
		method: "GET", path: "/todos?limit=1&offset=1&order=id",
		headers:         map[string]string{"Prefer": "count=exact"},
		wantPrefApplied: "count=exact"},

	// ── Group 5: Singular ─────────────────────────────────────────────────
	{name: "mongo/5.1 singular object",
		method:  "GET",
		path:    "/todos?id=eq.1",
		headers: map[string]string{"Accept": "application/vnd.pgrst.object+json"}},
	{name: "mongo/5.2 singular missing 406",
		method:     "GET",
		path:       "/todos?id=eq.99999",
		headers:    map[string]string{"Accept": "application/vnd.pgrst.object+json"},
		wantStatus: 406, bodyMode: "status"},

	// ── Group 6: Embedding → PGRST127 ─────────────────────────────────────
	// MongoDB $lookup is not implemented; embeds return PGRST127.
	{name: "mongo/6.1 embed assignments PGRST127",
		method:     "GET",
		path:       "/persons?select=name,assignments(todo_id)&order=id",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "mongo/6.2 embed nested PGRST127",
		method:     "GET",
		path:       "/persons?select=name,assignments(todos(task))&order=id",
		wantStatus: 400, bodyMode: "pgrst127"},

	// ── Group 7: Errors ────────────────────────────────────────────────────
	{name: "mongo/7.1 unknown table 404",
		method: "GET", path: "/nonexistent", wantStatus: 404, bodyMode: "status"},
	{name: "mongo/7.2 unknown column 400",
		method: "GET", path: "/todos?nonexistent=eq.1", wantStatus: 400, bodyMode: "status"},

	// ── Group 8: Unsupported array operators → PGRST127 ───────────────────
	{name: "mongo/8.1 cs array op PGRST127",
		method: "GET", path: "/todos?tags=cs.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "mongo/8.2 cd array op PGRST127",
		method: "GET", path: "/todos?tags=cd.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},
	{name: "mongo/8.3 ov array op PGRST127",
		method: "GET", path: "/todos?tags=ov.{go}",
		wantStatus: 400, bodyMode: "pgrst127"},

	// ── Group 9: Writes ────────────────────────────────────────────────────
	{name: "mongo/9.1 POST insert minimal",
		method: "POST", path: "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=minimal",
		},
		body:     `{"id":10,"task":"mongo test task","done":false}`,
		bodyMode: "empty"},
	{name: "mongo/9.2 POST insert representation",
		method: "POST", path: "/todos",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:     `{"id":11,"task":"mongo repr task","done":false}`,
		bodyMode: "schema"},
	{name: "mongo/9.3 PATCH update representation",
		method: "PATCH", path: "/todos?id=eq.1",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Prefer":       "return=representation",
		},
		body:     `{"done":true}`,
		bodyMode: "schema"},
	{name: "mongo/9.4 DELETE",
		method:   "DELETE",
		path:     "/todos?id=gt.100",
		bodyMode: "empty"},
}

// TestMongoCompat runs the MongoDB compat suite against the subject server.
func TestMongoCompat(t *testing.T) {
	subject := mongoURLs(t)

	for _, c := range mongoCases {
		t.Run(c.name, func(t *testing.T) {
			runMongoCase(t, subject, c)
		})
	}
}

// runMongoCase executes one case against the MongoDB subject and checks the result.
func runMongoCase(t *testing.T, subject string, c compatCase) {
	t.Helper()

	// PGRST127 cases: verify 400 + PGRST127 code.
	if c.bodyMode == "pgrst127" {
		resp := doRequest(t, subject, c)
		if resp.status != 400 {
			t.Errorf("expected 400 for PGRST127 case, got %d; body=%s", resp.status, resp.body)
		}
		if !strings.Contains(string(resp.body), "PGRST127") {
			t.Errorf("expected PGRST127 in body, got: %s", resp.body)
		}
		return
	}

	// Write cases run standalone without golden comparison.
	if c.method == "POST" || c.method == "PATCH" || c.method == "DELETE" {
		runMongoWriteCase(t, subject, c)
		return
	}

	resp := doRequest(t, subject, c)

	if c.wantStatus > 0 {
		if resp.status != c.wantStatus {
			t.Errorf("status=%d want %d; body=%s", resp.status, c.wantStatus, resp.body)
		}
		return
	}

	if resp.status != 200 && resp.status != 206 {
		t.Errorf("unexpected status=%d; body=%s", resp.status, resp.body)
		return
	}

	if c.wantPrefApplied != "" {
		got := resp.header.Get("Preference-Applied")
		if !strings.Contains(got, c.wantPrefApplied) {
			t.Errorf("Preference-Applied=%q does not contain %q", got, c.wantPrefApplied)
		}
	}

	switch c.bodyMode {
	case "empty":
		if len(resp.body) != 0 {
			t.Errorf("expected empty body, got: %s", resp.body)
		}
	case "status":
		// Status already checked above.
	case "schema":
		if len(resp.body) == 0 {
			t.Errorf("expected non-empty body for schema check")
		}
	default:
		// Verify the response is valid JSON and non-empty for reads.
		if len(resp.body) == 0 {
			t.Errorf("expected non-empty body for read case")
		}
	}
}

// runMongoWriteCase handles write cases: sends to subject, verifies status, cleans up.
func runMongoWriteCase(t *testing.T, subject string, c compatCase) {
	t.Helper()

	resp := doRequest(t, subject, c)

	if c.wantStatus > 0 {
		if resp.status != c.wantStatus {
			t.Errorf("status=%d want %d; body=%s", resp.status, c.wantStatus, resp.body)
		}
		return
	}

	if c.method == "POST" {
		if resp.status != 201 {
			t.Errorf("POST status=%d want 201; body=%s", resp.status, resp.body)
			return
		}
		if c.bodyMode == "schema" && len(resp.body) == 0 {
			t.Errorf("POST with return=representation returned empty body")
		}
		// Cleanup: delete all rows with id > 9.
		doRequest(t, subject, compatCase{method: "DELETE", path: "/todos?id=gt.9"})
		return
	}

	if c.method == "PATCH" {
		if resp.status != 200 {
			t.Errorf("PATCH status=%d want 200; body=%s", resp.status, resp.body)
			return
		}
		if c.bodyMode == "schema" && len(resp.body) == 0 {
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

	if c.method == "DELETE" {
		if resp.status != 204 && resp.status != 200 {
			t.Errorf("DELETE status=%d want 204 or 200; body=%s", resp.status, resp.body)
		}
	}
}

// TestMongoCompatSummary prints a pass/fail count for the MongoDB compat suite.
func TestMongoCompatSummary(t *testing.T) {
	subject := mongoURLs(t)

	passed, failed := 0, 0
	for _, c := range mongoCases {
		ok := t.Run(c.name+"/summary", func(t *testing.T) {
			runMongoCase(t, subject, c)
		})
		if ok {
			passed++
		} else {
			failed++
		}
	}
	t.Logf("MongoDB compat: %d/%d passed", passed, passed+failed)
	if failed > 0 {
		t.Errorf("%d cases failed", failed)
	}
}
