package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
)

func newReq(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}

func newReqBody(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// captureBackend wraps a real backend, recording the request context each
// Execute receives and optionally mutating its response controls. It lets the
// tests assert what the frontend assembled (the "in" direction) and that a
// backend's controls reach the response (the "out" direction).
type captureBackend struct {
	backend.Backend
	got    *reqctx.Context
	inject func(*reqctx.ResponseControls)
}

func (c *captureBackend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	c.got = rc
	if c.inject != nil {
		c.inject(rc.Controls())
	}
	return c.Backend.Execute(ctx, plan, rc)
}

// captureServer builds a server over a captureBackend wrapping SQLite, returning
// both so a test can drive HTTP and then inspect what the backend saw.
func captureServer(t *testing.T) (*httpapi.Server, *captureBackend) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })

	_, err = be.DB().Exec(`
		CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT NOT NULL);
		INSERT INTO films (id, title) VALUES (1, 'Metropolis'), (2, 'Arrival');
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	cap := &captureBackend{Backend: be}
	srv := httpapi.NewServer(cap, model, nil)
	srv.SetDefaultRole("anon")
	return srv, cap
}

func TestContextCarriesRequestMetadata(t *testing.T) {
	srv, cap := captureServer(t)
	req := newReq(http.MethodGet, "/films?select=id")
	req.Header.Set("X-Tenant", "acme")
	req.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
	srv.ServeHTTP(newRecorder(), req)

	if cap.got == nil {
		t.Fatal("Execute never received a context")
	}
	if cap.got.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", cap.got.Method)
	}
	if cap.got.Path != "/films" {
		t.Errorf("Path = %q, want /films", cap.got.Path)
	}
	if got := cap.got.Headers["X-Tenant"]; len(got) != 1 || got[0] != "acme" {
		t.Errorf("Headers[X-Tenant] = %v, want [acme]", got)
	}
	if cap.got.Cookies["session"] != "abc" {
		t.Errorf("Cookies[session] = %q, want abc", cap.got.Cookies["session"])
	}
	if cap.got.Role != "anon" {
		t.Errorf("Role = %q, want anon (no verifier)", cap.got.Role)
	}
}

func TestContextCarriesProfileSchema(t *testing.T) {
	srv, cap := captureServer(t)
	req := newReq(http.MethodGet, "/films?select=id")
	req.Header.Set("Accept-Profile", "reporting")
	srv.ServeHTTP(newRecorder(), req)

	if cap.got.Schema != "reporting" {
		t.Errorf("Schema = %q, want reporting (from Accept-Profile)", cap.got.Schema)
	}
}

func TestContextWriteUsesContentProfile(t *testing.T) {
	srv, cap := captureServer(t)
	req := newReqBody(http.MethodPost, "/films", `{"id":3,"title":"Dune"}`)
	req.Header.Set("Content-Profile", "staging")
	srv.ServeHTTP(newRecorder(), req)

	if cap.got.Schema != "staging" {
		t.Errorf("Schema = %q, want staging (from Content-Profile)", cap.got.Schema)
	}
}

func TestResponseControlStatusOverridesReadDefault(t *testing.T) {
	srv, cap := captureServer(t)
	// A backend that overrides the read status, as a function or policy would.
	cap.inject = func(ctrl *reqctx.ResponseControls) {
		ctrl.SetStatus(http.StatusAccepted)
		ctrl.SetHeader("X-Tenant", "acme")
	}
	rec := newRecorder()
	srv.ServeHTTP(rec, newReq(http.MethodGet, "/films?select=id"))

	resp := rec.Result()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (control override)", resp.StatusCode)
	}
	if h := resp.Header.Get("X-Tenant"); h != "acme" {
		t.Errorf("X-Tenant = %q, want acme", h)
	}
}

func TestResponseControlStatusOverridesWriteDefault(t *testing.T) {
	srv, cap := captureServer(t)
	cap.inject = func(ctrl *reqctx.ResponseControls) {
		ctrl.SetStatus(http.StatusTeapot)
	}
	rec := newRecorder()
	srv.ServeHTTP(rec, newReqBody(http.MethodPost, "/films", `{"id":4,"title":"Solaris"}`))

	if got := rec.Result().StatusCode; got != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (control override beats the 201 default)", got)
	}
}

func TestNoControlKeepsDefaultStatus(t *testing.T) {
	srv, _ := captureServer(t)
	rec := newRecorder()
	srv.ServeHTTP(rec, newReqBody(http.MethodPost, "/films", `{"id":5,"title":"Tenet"}`))
	if got := rec.Result().StatusCode; got != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (no override)", got)
	}
}
