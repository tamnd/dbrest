package postgres

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/reqctx"
)

// queuedSQL collects the SQL text of every item in a batch, for asserting which
// session-setup statements were queued.
func queuedSQL(batch *pgx.Batch) []string {
	out := make([]string, 0, len(batch.QueuedQueries))
	for _, q := range batch.QueuedQueries {
		out = append(out, q.SQL)
	}
	return out
}

// TestQueueSessionTimeZone checks Prefer: timezone= becomes a SET LOCAL timezone
// (via set_config(...,true)) carrying the validated zone as a parameter.
func TestQueueSessionTimeZone(t *testing.T) {
	b := &Backend{}
	batch := &pgx.Batch{}
	rc := &reqctx.Context{Role: "web_anon", TimeZone: "America/Los_Angeles"}
	queueSessionItems(batch, b, rc)

	var tzItem *pgx.QueuedQuery
	for _, q := range batch.QueuedQueries {
		if strings.Contains(q.SQL, "'timezone'") {
			tzItem = q
		}
	}
	if tzItem == nil {
		t.Fatalf("no timezone item queued; queued: %v", queuedSQL(batch))
	}
	if !strings.Contains(tzItem.SQL, "set_config('timezone',$1,true)") {
		t.Errorf("timezone SQL = %q", tzItem.SQL)
	}
	if len(tzItem.Arguments) != 1 || tzItem.Arguments[0] != "America/Los_Angeles" {
		t.Errorf("timezone args = %v, want [America/Los_Angeles]", tzItem.Arguments)
	}
}

// TestQueueSessionNoTimeZone checks the timezone item is absent when the request
// stated no zone, so the engine default stands.
func TestQueueSessionNoTimeZone(t *testing.T) {
	b := &Backend{}
	batch := &pgx.Batch{}
	queueSessionItems(batch, b, &reqctx.Context{Role: "web_anon"})
	for _, sql := range queuedSQL(batch) {
		if strings.Contains(sql, "'timezone'") {
			t.Errorf("unexpected timezone item: %q", sql)
		}
	}
}

// TestQueueSessionPreRequest checks db-pre-request becomes a SELECT of the quoted
// function as the last session item, so it runs after the GUCs and before the
// main query.
func TestQueueSessionPreRequest(t *testing.T) {
	b := &Backend{}
	batch := &pgx.Batch{}
	queueSessionItems(batch, b, &reqctx.Context{Role: "web_anon", PreRequest: "auth.check_request"})
	last := queuedSQL(batch)
	if len(last) == 0 {
		t.Fatal("no items queued")
	}
	got := last[len(last)-1]
	if got != `SELECT "auth"."check_request"()` {
		t.Errorf("pre-request item = %q", got)
	}
}

// TestQueueSessionNoPreRequest checks no pre-request item is queued when none is
// configured.
func TestQueueSessionNoPreRequest(t *testing.T) {
	b := &Backend{}
	batch := &pgx.Batch{}
	queueSessionItems(batch, b, &reqctx.Context{Role: "web_anon"})
	for _, sql := range queuedSQL(batch) {
		if strings.HasPrefix(sql, "SELECT \"") {
			t.Errorf("unexpected pre-request item: %q", sql)
		}
	}
}
