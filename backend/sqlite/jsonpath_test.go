package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
)

// openDocs seeds a table with a JSON column holding a nested document, so the
// 07.1 JSON-path lowering can be exercised against the real SQLite engine.
func openDocs(t *testing.T) *Backend {
	t.Helper()
	b := openSeeded(t)
	_, err := b.DB().Exec(`
		CREATE TABLE docs (id INTEGER PRIMARY KEY, data JSON);
		INSERT INTO docs (id, data) VALUES
			(1, '{"blood_type":"A-","phones":[{"number":"555"}],"meta":{"k":1}}'),
			(2, '{"blood_type":"O+","phones":[{"number":"999"}],"meta":{"k":2}}'),
			(3, '{"blood_type":"A-","flag":"true","phones":[],"meta":{"k":3}}');
	`)
	if err != nil {
		t.Fatalf("seed docs: %v", err)
	}
	return b
}

func planDocs(t *testing.T, b *Backend, query string) *ir.Query {
	t.Helper()
	q, perr := ir.ParseRead("docs", query, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	pl, perr := plan.Read(model, q, nil, plan.Options{})
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	return pl.Query
}

func runDocs(t *testing.T, b *Backend, q *ir.Query) []map[string]any {
	t.Helper()
	pl := &ir.Plan{Query: q, ReadOnly: true}
	res, err := b.Execute(context.Background(), pl, &reqctx.Context{Role: "anon"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return readAll(t, res)
}

// A ->> filter selects rows by the text scalar at the path.
func TestJSONPathFilterText(t *testing.T) {
	b := openDocs(t)
	rows := runDocs(t, b, planDocs(t, b, "select=id&data->>blood_type=eq.A-&order=id"))
	if len(rows) != 2 || rows[0]["id"].(int64) != 1 || rows[1]["id"].(int64) != 3 {
		t.Errorf("rows = %v, want ids [1 3]", rows)
	}
}

// A ->> filter reaches through an array index and a nested key.
func TestJSONPathFilterArrayIndex(t *testing.T) {
	b := openDocs(t)
	rows := runDocs(t, b, planDocs(t, b, "select=id&data->phones->0->>number=eq.999"))
	if len(rows) != 1 || rows[0]["id"].(int64) != 2 {
		t.Errorf("rows = %v, want id [2]", rows)
	}
}

// A ->> projection returns the text scalar under the last hop's name.
func TestJSONPathProjectionText(t *testing.T) {
	b := openDocs(t)
	rows := runDocs(t, b, planDocs(t, b, "select=id,data->>blood_type&order=id"))
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if got, _ := asString(rows[0]["blood_type"]); got != "A-" {
		t.Errorf("row0 blood_type = %q, want A-", got)
	}
}

// A -> projection returns the engine's JSON text for the path. The backend
// surfaces it as the JSON string {"k":1}; the renderer splices it verbatim
// (proved at the httpapi layer, where embedKeys flags the -> column).
func TestJSONPathProjectionJSON(t *testing.T) {
	b := openDocs(t)
	rows := runDocs(t, b, planDocs(t, b, "select=id,data->meta&order=id&id=eq.1"))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got, _ := asString(rows[0]["meta"])
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("meta not JSON: %v (%q)", err, got)
	}
	if m["k"] != float64(1) {
		t.Errorf("meta.k = %v, want 1", m["k"])
	}
}

// eq.true on a ->> extract compares the literal word: the row whose JSON flag
// holds the string "true" matches, proving the access is text, not boolean.
func TestJSONPathEqTrueIsText(t *testing.T) {
	b := openDocs(t)
	rows := runDocs(t, b, planDocs(t, b, "select=id&data->>flag=eq.true"))
	if len(rows) != 1 || rows[0]["id"].(int64) != 3 {
		t.Errorf("rows = %v, want id [3]", rows)
	}
}

// Ordering by a ->> extract sorts by the path's text value.
func TestJSONPathOrder(t *testing.T) {
	b := openDocs(t)
	rows := runDocs(t, b, planDocs(t, b, "select=id&order=data->meta->>k.desc"))
	if len(rows) != 3 || rows[0]["id"].(int64) != 3 || rows[2]["id"].(int64) != 1 {
		t.Errorf("rows = %v, want ids [3 2 1]", rows)
	}
}
