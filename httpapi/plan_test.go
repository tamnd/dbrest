package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
)

const planMedia = "application/vnd.pgrst.plan+json"

// explainBackend wraps the sqlite backend with a canned ExplainRead, standing
// in for an engine that supports EXPLAIN.
type explainBackend struct {
	*sqlite.Backend
}

func (e *explainBackend) ExplainRead(context.Context, *ir.Plan, *reqctx.Context, bool) ([]byte, error) {
	return []byte(`[{"Plan":{"Node Type":"Seq Scan"}}]`), nil
}

// planServer builds a server over a seeded films table with an
// EXPLAIN-capable backend, so the db-plan-enabled gate is the only variable.
func planServer(t *testing.T) *httpapi.Server {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if _, err := be.DB().Exec(`CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	return httpapi.NewServer(&explainBackend{be}, model, nil)
}

// TestPlanDisabledByDefault pins the upstream security default: without
// db-plan-enabled = true a plan request fails with the media-type error even
// when the backend could explain the query.
func TestPlanDisabledByDefault(t *testing.T) {
	srv := planServer(t)
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "PGRST107" {
		t.Errorf("code = %q, want PGRST107", body.Code)
	}
}

// TestPlanServedWhenEnabled checks the gate opens with the option on.
func TestPlanServedWhenEnabled(t *testing.T) {
	srv := planServer(t)
	srv.SetPlanEnabled(true)
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != planMedia {
		t.Errorf("Content-Type = %q, want %q", ct, planMedia)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `[{"Plan":{"Node Type":"Seq Scan"}}]` {
		t.Errorf("body = %s", b)
	}
}

// TestPlanEnabledStillNeedsExplainer keeps the older behavior under the gate:
// an enabled config on a backend without EXPLAIN support is still 406.
func TestPlanEnabledStillNeedsExplainer(t *testing.T) {
	srv := newServer(t)
	srv.SetPlanEnabled(true)
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
}
