package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A non-integer operand on an integer column is rejected in the frontend, before
// the query reaches the engine, as the PostgREST 22P02 envelope with a 400.
func TestFilterCoercionRejectsBadInteger(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?year=eq.abc", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "22P02" {
		t.Errorf("code = %q, want 22P02", body.Code)
	}
	if body.Message != `invalid input syntax for type int4: "abc"` {
		t.Errorf("message = %q", body.Message)
	}
}

// A valid integer operand passes through and filters.
func TestFilterCoercionAcceptsValidInteger(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?year=eq.1982", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Blade Runner" {
		t.Fatalf("rows = %v, want the single 1982 film", rows)
	}
}

// A text column never coerces: any operand reaches the engine.
func TestFilterTextColumnNotCoerced(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?rating=eq.PG13", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Arrival" {
		t.Fatalf("rows = %v, want the PG13 film", rows)
	}
}

// A bad member inside an in-list is caught the same way.
func TestFilterInListCoercion(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=in.(1,2,notnum)", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
