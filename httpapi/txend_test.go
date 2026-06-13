package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tamnd/dbrest/httpapi"
)

// yearOf reads the year of a film for the tx= persistence checks.
func yearOf(t *testing.T, srv *httpapi.Server, id string) float64 {
	t.Helper()
	resp := do(t, srv, http.MethodGet, "/films?id=eq."+id+"&select=year", nil)
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	return rows[0]["year"].(float64)
}

// TestTxRollbackIgnoredUnderDefaultCommit checks the default db-tx-end=commit
// ignores Prefer: tx=rollback: the write persists and tx= is not echoed, the
// PostgREST default-deployment behavior.
func TestTxRollbackIgnoredUnderDefaultCommit(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.1", `{"year":1900}`, map[string]string{
		"Prefer": "tx=rollback",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if applied := resp.Header.Get("Preference-Applied"); applied != "" {
		t.Errorf("Preference-Applied = %q, want tx= not echoed", applied)
	}
	if got := yearOf(t, srv, "1"); got != 1900 {
		t.Errorf("year after default-commit = %v, want 1900 (persisted)", got)
	}
}

// TestTxRollbackHonoredUnderOverride checks an allow-override policy honors
// tx=rollback: the write does not persist and tx= is echoed.
func TestTxRollbackHonoredUnderOverride(t *testing.T) {
	srv := newServer(t)
	srv.SetTxEnd("commit-allow-override")
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.1", `{"year":1901}`, map[string]string{
		"Prefer": "tx=rollback",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if applied := resp.Header.Get("Preference-Applied"); applied != "tx=rollback" {
		t.Errorf("Preference-Applied = %q, want tx=rollback", applied)
	}
	if got := yearOf(t, srv, "1"); got != 1927 {
		t.Errorf("year after rolled-back patch = %v, want 1927 (unchanged)", got)
	}
}

// TestTxDisallowedIsStrictOffender checks a tx= under handling=strict with the
// default commit policy is the PGRST122 invalid-preference error.
func TestTxDisallowedIsStrictOffender(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.1", `{"year":1902}`, map[string]string{
		"Prefer": "tx=rollback, handling=strict",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env struct{ Code string }
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "PGRST122" {
		t.Errorf("code = %q, want PGRST122", env.Code)
	}
}

// TestRollbackPolicyForcesRollback checks db-tx-end=rollback rolls a write back
// even with no tx= preference, the mode test deployments use.
func TestRollbackPolicyForcesRollback(t *testing.T) {
	srv := newServer(t)
	srv.SetTxEnd("rollback")
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.1", `{"year":1903}`, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := yearOf(t, srv, "1"); got != 1927 {
		t.Errorf("year under rollback policy = %v, want 1927 (unchanged)", got)
	}
}
