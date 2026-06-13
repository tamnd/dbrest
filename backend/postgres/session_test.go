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
