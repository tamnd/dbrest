package main

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/httpapi"
)

// openTestBackend opens an in-memory SQLite backend for the wiring tests.
func openTestBackend(t *testing.T) backend.Backend {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

// preRequestBackend declares pre-request support over a real backend, standing
// in for a driver that runs the function inside its transaction.
type preRequestBackend struct{ backend.Backend }

func (preRequestBackend) SupportsPreRequest() bool { return true }

func TestAttachPreRequestNoopWhenUnset(t *testing.T) {
	be := openTestBackend(t)
	srv := httpapi.NewServer(be, nil, nil)
	if err := attachPreRequest(srv, be, &config.Config{Backend: "sqlite"}); err != nil {
		t.Fatalf("attachPreRequest with no option = %v, want nil", err)
	}
}

func TestAttachPreRequestRefusesUnsupportedBackend(t *testing.T) {
	// No backend declares pre-request support yet, so a configured
	// db-pre-request must refuse startup rather than silently drop the
	// function (deployments use it for blocking and custom auth).
	be := openTestBackend(t)
	srv := httpapi.NewServer(be, nil, nil)
	cfg := &config.Config{Backend: "sqlite", PreRequest: "api.check_request"}
	err := attachPreRequest(srv, be, cfg)
	if err == nil {
		t.Fatal("attachPreRequest = nil, want a startup refusal on a backend without pre-request support")
	}
	if !strings.Contains(err.Error(), "db-pre-request") {
		t.Errorf("error %q does not name the db-pre-request option", err)
	}
}

func TestAttachPreRequestAcceptsSupportingBackend(t *testing.T) {
	be := preRequestBackend{openTestBackend(t)}
	srv := httpapi.NewServer(be, nil, nil)
	cfg := &config.Config{Backend: "sqlite", PreRequest: "api.check_request"}
	if err := attachPreRequest(srv, be, cfg); err != nil {
		t.Fatalf("attachPreRequest on a supporting backend = %v, want nil", err)
	}
}

func TestAttachAuthzNoopWhenUnset(t *testing.T) {
	be := openTestBackend(t)
	srv := httpapi.NewServer(be, nil, nil)
	if err := attachAuthz(srv, &config.Config{Backend: "sqlite"}); err != nil {
		t.Fatalf("attachAuthz with no registry = %v, want nil", err)
	}
}

func TestAttachAuthzWiresParsedRegistry(t *testing.T) {
	be := openTestBackend(t)
	srv := httpapi.NewServer(be, nil, nil)
	cfg := &config.Config{Backend: "sqlite", PolicyRegistry: `{
		"grants": [{"role": "web_user", "relation": "todos", "actions": ["select"]}]
	}`}
	if err := attachAuthz(srv, cfg); err != nil {
		t.Fatalf("attachAuthz with a valid registry = %v, want nil", err)
	}
}

func TestAttachAuthzRefusesBadRegistry(t *testing.T) {
	// The registry is the security boundary on the emulated backends, so a
	// declaration the parser cannot fully understand must stop the boot.
	be := openTestBackend(t)
	srv := httpapi.NewServer(be, nil, nil)
	cfg := &config.Config{Backend: "sqlite", PolicyRegistry: `{"grants": [{"role": "r"}]}`}
	err := attachAuthz(srv, cfg)
	if err == nil {
		t.Fatal("attachAuthz = nil, want a startup refusal on an unparseable registry")
	}
	if !strings.Contains(err.Error(), "policy-registry") {
		t.Errorf("error %q does not name the policy-registry option", err)
	}
}

func TestAttachAuthzRefusesPostgres(t *testing.T) {
	// Postgres enforces grants and RLS in the engine; a registry there would
	// suggest a second enforcement layer that does not exist, so it is a
	// misconfiguration rather than a silent no-op.
	be := openTestBackend(t)
	srv := httpapi.NewServer(be, nil, nil)
	cfg := &config.Config{Backend: "postgres", PolicyRegistry: `{}`}
	err := attachAuthz(srv, cfg)
	if err == nil {
		t.Fatal("attachAuthz = nil, want a refusal on the postgres backend")
	}
	if !strings.Contains(err.Error(), "policy-registry") {
		t.Errorf("error %q does not name the policy-registry option", err)
	}
}
