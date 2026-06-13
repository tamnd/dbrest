package httpapi_test

import (
	"net/http"
	"testing"
)

// 07.12: an empty payload is a no-op the server accepts, not a 400. A POST with
// an empty array inserts nothing and returns 201; a PATCH with an empty object
// (or the empty-array forms [] and [{}]) updates nothing and returns 204, or 200
// with an empty representation when one is asked for. Either way no row changes
// and the write Content-Range stays */*.
func TestPostEmptyArrayInsertsNothing(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `[]`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
	if rows := decodeArray(t, resp); len(rows) != 0 {
		t.Errorf("body = %v, want empty array", rows)
	}
	// The table is untouched.
	after := do(t, srv, http.MethodGet, "/films", nil)
	if all := decodeArray(t, after); len(all) != 4 {
		t.Errorf("row count = %d, want 4 (nothing inserted)", len(all))
	}
}

func TestPostEmptyArrayMinimalIs201(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `[]`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("minimal insert should have no body")
	}
}

func TestPatchEmptyObjectIs204NoOp(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films", `{}`, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
}

func TestPatchEmptyObjectRepresentationIs200EmptyArray(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films", `{}`, map[string]string{
		"Prefer": "return=representation",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
	if rows := decodeArray(t, resp); len(rows) != 0 {
		t.Errorf("body = %v, want empty array", rows)
	}
}

// The empty-array forms of a PATCH body are the same no-op as the empty object.
func TestPatchEmptyArrayFormsAreNoOp(t *testing.T) {
	for _, body := range []string{`[]`, `[{}]`} {
		t.Run(body, func(t *testing.T) {
			srv := newServer(t)
			resp := send(t, srv, http.MethodPatch, "/films", body, nil)
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("PATCH %s: status = %d, want 204", body, resp.StatusCode)
			}
		})
	}
}

// A PATCH array carrying a non-empty object is not a shape upstream defines; it
// stays a 400 rather than silently updating.
func TestPatchNonEmptyArrayIs400(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films", `[{"rating":"X"}]`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

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
