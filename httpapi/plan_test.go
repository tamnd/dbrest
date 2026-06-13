package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
)

const planMedia = "application/vnd.pgrst.plan+json"

// explainBackend wraps the sqlite backend with canned Explain methods, standing
// in for an engine that supports EXPLAIN. The three methods mirror the read,
// write, and call execution paths the Explainer interface covers.
type explainBackend struct {
	*sqlite.Backend
}

func (e *explainBackend) ExplainRead(context.Context, *ir.Plan, *reqctx.Context, backend.PlanOptions) ([]byte, error) {
	return []byte(`[{"Plan":{"Node Type":"Seq Scan"}}]`), nil
}

func (e *explainBackend) ExplainWrite(context.Context, *ir.Plan, *reqctx.Context, backend.PlanOptions) ([]byte, error) {
	return []byte(`[{"Plan":{"Node Type":"ModifyTable"}}]`), nil
}

func (e *explainBackend) ExplainCall(context.Context, *ir.Plan, *reqctx.Context, backend.PlanOptions) ([]byte, error) {
	return []byte(`[{"Plan":{"Node Type":"Function Scan"}}]`), nil
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
	srv := httpapi.NewServer(&explainBackend{be}, model, nil)
	srv.SetDefaultRole("web_anon")
	return srv
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
	// PostgREST echoes the negotiated plan media type with its parameters: the
	// +json suffix, the for="<target>" the plan was computed for (application/json
	// by default), and the charset.
	wantCT := `application/vnd.pgrst.plan+json; for="application/json"; charset=utf-8`
	if ct := resp.Header.Get("Content-Type"); ct != wantCT {
		t.Errorf("Content-Type = %q, want %q", ct, wantCT)
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

// TestPlanForWrite checks a mutation plan request routes to ExplainWrite and
// returns the plan instead of executing the write. This pins that the write
// handler hands a plan-typed request to servePlan before touching Execute.
func TestPlanForWrite(t *testing.T) {
	srv := planServer(t)
	srv.SetPlanEnabled(true)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":7,"title":"M"}`, map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `[{"Plan":{"Node Type":"ModifyTable"}}]` {
		t.Errorf("body = %s", b)
	}
}

// TestPlanForWriteDisabledIs406 pins that a mutation plan request under a closed
// gate fails with the media-type error, not a 500 or a silently executed write.
func TestPlanForWriteDisabledIs406(t *testing.T) {
	srv := planServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":7,"title":"M"}`, map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
}

// planRPCServer is planServer with a portable function registered, so the RPC
// plan path has something to explain.
func planRPCServer(t *testing.T) *httpapi.Server {
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
	be.Register(rpc.NewStaticRegistry([]*rpc.Function{{
		Name:       "add_them",
		Params:     []rpc.Param{{Name: "a", Type: "integer"}, {Name: "b", Type: "integer"}},
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "integer"},
		Volatility: rpc.Immutable,
		Query:      &rpc.PortableQuery{SQL: "SELECT :a + :b"},
	}}))
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(&explainBackend{be}, model, nil)
	srv.SetDefaultRole("web_anon")
	return srv
}

// TestPlanForRPC checks an RPC plan request routes to ExplainCall and returns
// the plan instead of invoking the function.
func TestPlanForRPC(t *testing.T) {
	srv := planRPCServer(t)
	srv.SetPlanEnabled(true)
	resp := send(t, srv, http.MethodPost, "/rpc/add_them", `{"a":2,"b":3}`, map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `[{"Plan":{"Node Type":"Function Scan"}}]` {
		t.Errorf("body = %s", b)
	}
}

// TestPlanForRPCDisabledIs406 pins that an RPC plan request under a closed gate
// fails with the media-type error rather than 500ing or running the function.
func TestPlanForRPCDisabledIs406(t *testing.T) {
	srv := planRPCServer(t)
	resp := send(t, srv, http.MethodPost, "/rpc/add_them", `{"a":2,"b":3}`, map[string]string{"Accept": planMedia})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
}
