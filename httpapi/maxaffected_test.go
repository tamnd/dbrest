package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// 02.2: Prefer: max-affected caps the rows a write may affect under
// handling=strict. A violation is 400 PGRST124 and the whole transaction rolls
// back; under lenient handling the preference is ignored entirely.

// TestPatchMaxAffectedExceededRollsBack: a PATCH whose filter matches more rows
// than max-affected fails with PGRST124 and leaves every row unchanged.
func TestPatchMaxAffectedExceededRollsBack(t *testing.T) {
	srv := newServer(t)
	// year >= 1900 matches films 1, 2, 3 (film 4 has a NULL year), three rows.
	resp := send(t, srv, http.MethodPatch, "/films?year=gte.1900", `{"rating":"X"}`, map[string]string{
		"Prefer": "handling=strict, max-affected=1",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env["code"] != "PGRST124" {
		t.Errorf("code = %v, want PGRST124", env["code"])
	}
	if env["details"] != "The query affects 3 rows" {
		t.Errorf("details = %v, want the affected count", env["details"])
	}
	// The transaction rolled back: no row took the new rating.
	after := do(t, srv, http.MethodGet, "/films?rating=eq.X&select=id", nil)
	if rows := decodeArray(t, after); len(rows) != 0 {
		t.Errorf("rollback failed, %d rows were updated", len(rows))
	}
}

// TestDeleteMaxAffectedExceededRollsBack: a DELETE matching more rows than the
// bound fails with PGRST124 and deletes nothing.
func TestDeleteMaxAffectedExceededRollsBack(t *testing.T) {
	srv := newServer(t)
	// No filter: all four seed rows match.
	resp := send(t, srv, http.MethodDelete, "/films", "", map[string]string{
		"Prefer": "handling=strict, max-affected=2",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if env := decodeEnvelope(t, resp); env["code"] != "PGRST124" {
		t.Errorf("code = %v, want PGRST124", env["code"])
	}
	after := do(t, srv, http.MethodGet, "/films?select=id", nil)
	if rows := decodeArray(t, after); len(rows) != 4 {
		t.Errorf("rollback failed, %d rows remain, want 4", len(rows))
	}
}

// TestPatchMaxAffectedWithinBoundCommits: a write at or under the bound proceeds
// normally and persists.
func TestPatchMaxAffectedWithinBoundCommits(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"X"}`, map[string]string{
		"Prefer": "handling=strict, max-affected=1",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	after := do(t, srv, http.MethodGet, "/films?id=eq.2&select=rating", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["rating"] != "X" {
		t.Errorf("write did not persist: %v", rows)
	}
}

// TestPatchMaxAffectedLenientIgnored: without handling=strict the preference is
// ignored, so an over-broad write still commits and is not echoed.
func TestPatchMaxAffectedLenientIgnored(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?year=gte.1900", `{"rating":"X"}`, map[string]string{
		"Prefer": "max-affected=1",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if pa := resp.Header.Get("Preference-Applied"); pa != "" {
		t.Errorf("Preference-Applied = %q, want max-affected not echoed under lenient", pa)
	}
	after := do(t, srv, http.MethodGet, "/films?rating=eq.X&select=id", nil)
	if rows := decodeArray(t, after); len(rows) != 3 {
		t.Errorf("lenient write affected %d rows, want all 3", len(rows))
	}
}

// TestMaxAffectedEchoedUnderStrict: a strict request that stays within the bound
// echoes max-affected in Preference-Applied.
func TestMaxAffectedEchoedUnderStrict(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"X"}`, map[string]string{
		"Prefer": "handling=strict, max-affected=5",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	pa := resp.Header.Get("Preference-Applied")
	if pa == "" || !strings.Contains(pa, "max-affected=5") {
		t.Errorf("Preference-Applied = %q, want it to echo max-affected=5", pa)
	}
}
