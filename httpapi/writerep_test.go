package httpapi_test

import (
	"net/http"
	"testing"
)

// 07.3: PostgREST v13 dropped limited update/delete, so order and limit on a
// PATCH shape only the returned representation, never the mutation. The body is
// the ordered, limited slice; Content-Range still reports the full affected set;
// and every matching row is written.
func TestPatchOrderLimitShapesBodyNotMutation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?order=id.desc&limit=2", `{"rating":"X"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Content-Range carries the full affected count (4 seed rows), not the 2 the
	// body shows: the limit bounds the body, not the write.
	if got := resp.Header.Get("Content-Range"); got != "0-3/*" {
		t.Errorf("Content-Range = %q, want 0-3/* (full affected set)", got)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 2 {
		t.Fatalf("body rows = %d, want 2", len(rows))
	}
	if rows[0]["id"] != float64(4) || rows[1]["id"] != float64(3) {
		t.Errorf("body ids = [%v %v], want [4 3] (id desc)", rows[0]["id"], rows[1]["id"])
	}

	// Every row was updated, including the two outside the body window.
	after := do(t, srv, http.MethodGet, "/films?rating=eq.X&select=id", nil)
	if all := decodeArray(t, after); len(all) != 4 {
		t.Errorf("updated rows = %d, want 4 (the whole table)", len(all))
	}
}
