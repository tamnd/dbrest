package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
)

// newDocsServer seeds a table with a JSON column so the 07.1 JSON-path render
// contract can be checked end to end through the HTTP layer.
func newDocsServer(t testing.TB) *httpapi.Server {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })

	_, err = be.DB().Exec(`
		CREATE TABLE docs (id INTEGER PRIMARY KEY, data JSON);
		INSERT INTO docs (id, data) VALUES
			(1, '{"blood_type":"A-","meta":{"k":1}}');
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

// A final -> projection renders as raw JSON (decodes back to an object), while a
// final ->> projection renders as a plain JSON string. This is the renderer
// contract behind PostgREST's -> json / ->> text typing (07.1).
func TestJSONPathRenderTyping(t *testing.T) {
	srv := newDocsServer(t)
	resp := do(t, srv, http.MethodGet, "/docs?select=id,data->meta,data->>blood_type", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// -> meta is spliced raw: it decodes to a JSON object, not a quoted string.
	meta, ok := rows[0]["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta = %T (%v), want a JSON object spliced verbatim", rows[0]["meta"], rows[0]["meta"])
	}
	if meta["k"] != float64(1) {
		t.Errorf("meta.k = %v, want 1", meta["k"])
	}
	// ->> blood_type is text: a plain JSON string.
	if bt, ok := rows[0]["blood_type"].(string); !ok || bt != "A-" {
		t.Errorf("blood_type = %v (%T), want the string A-", rows[0]["blood_type"], rows[0]["blood_type"])
	}
}
