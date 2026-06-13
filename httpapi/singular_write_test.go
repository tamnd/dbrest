package httpapi_test

import (
	"net/http"
	"testing"
)

// 07.11: a singular write (Accept: application/vnd.pgrst.object+json) must affect
// exactly one row. PostgREST enforces this inside the write transaction and rolls
// back when the count is zero or many, so the mutation never becomes durable. The
// check runs pre-commit, not in the renderer, which means return=minimal is held
// to the same guarantee even though it produces no body to inspect.

// TestPatchSingularManyRollsBack: a singular PATCH matching three rows fails with
// PGRST116 and leaves every row unchanged.
func TestPatchSingularManyRollsBack(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?year=gte.1900", `{"rating":"X"}`, map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST116" {
		t.Errorf("code = %v, want PGRST116", env["code"])
	}
	// The transaction rolled back: no row took the new rating.
	after := do(t, srv, http.MethodGet, "/films?rating=eq.X&select=id", nil)
	if rows := decodeArray(t, after); len(rows) != 0 {
		t.Errorf("rollback failed, %d rows were updated", len(rows))
	}
}

// TestPatchSingularManyMinimalRollsBack: the same over-broad PATCH under
// return=minimal still fails closed before commit, even though no representation
// is computed. This is the case the renderer's post-commit check could not catch.
func TestPatchSingularManyMinimalRollsBack(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?year=gte.1900", `{"rating":"X"}`, map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
		"Prefer": "return=minimal",
	})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST116" {
		t.Errorf("code = %v, want PGRST116", env["code"])
	}
	after := do(t, srv, http.MethodGet, "/films?rating=eq.X&select=id", nil)
	if rows := decodeArray(t, after); len(rows) != 0 {
		t.Errorf("rollback failed, %d rows were updated", len(rows))
	}
}

// TestPatchSingularZeroRows: a singular PATCH whose filter matches nothing is
// PGRST116; there is nothing to undo, but the wire contract still holds.
func TestPatchSingularZeroRows(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.999", `{"rating":"X"}`, map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST116" {
		t.Errorf("code = %v, want PGRST116", env["code"])
	}
}

// TestDeleteSingularManyRollsBack: a singular DELETE matching every row fails and
// deletes nothing.
func TestDeleteSingularManyRollsBack(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodDelete, "/films", "", map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
		"Prefer": "return=minimal",
	})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST116" {
		t.Errorf("code = %v, want PGRST116", env["code"])
	}
	after := do(t, srv, http.MethodGet, "/films?select=id", nil)
	if rows := decodeArray(t, after); len(rows) != 4 {
		t.Errorf("rollback failed, %d rows remain, want 4", len(rows))
	}
}

// TestPatchSingularOneRowCommits: a singular PATCH that affects exactly one row
// proceeds and persists, returning the single object.
func TestPatchSingularOneRowCommits(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"X"}`, map[string]string{
		"Accept": "application/vnd.pgrst.object+json",
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env["rating"] != "X" {
		t.Errorf("body rating = %v, want X", env["rating"])
	}
	after := do(t, srv, http.MethodGet, "/films?id=eq.2&select=rating", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["rating"] != "X" {
		t.Errorf("write did not persist: %v", rows)
	}
}
