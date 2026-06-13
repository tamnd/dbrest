package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestOptionsOnTableAnswersAllow checks a plain OPTIONS on a table (no CORS
// preflight headers) is 200 with the full relation verb set and no body, the way
// PostgREST answers OPTIONS without running a transaction.
func TestOptionsOnTableAnswersAllow(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodOptions, "/films", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "OPTIONS,GET,HEAD,POST,PUT,PATCH,DELETE" {
		t.Errorf("Allow = %q", allow)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("OPTIONS should have no body")
	}
}

// TestOptionsOnVolatileRPCIsPostOnly checks OPTIONS on a volatile function
// answers OPTIONS,POST: a function that writes is not reachable by GET.
func TestOptionsOnVolatileRPCIsPostOnly(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodOptions, "/rpc/bump_year", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "OPTIONS,POST" {
		t.Errorf("Allow = %q, want OPTIONS,POST", allow)
	}
}

// TestOptionsOnReadOnlyRPCAllowsGet checks OPTIONS on a read-only function also
// answers GET and HEAD, the verbs a stable/immutable function accepts.
func TestOptionsOnReadOnlyRPCAllowsGet(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodOptions, "/rpc/film_titles", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "OPTIONS,GET,HEAD,POST" {
		t.Errorf("Allow = %q, want OPTIONS,GET,HEAD,POST", allow)
	}
}

// TestUnsupportedMethodIs405PGRST117 checks a verb the server implements nowhere
// is PostgREST's 405 PGRST117 naming the method, not the capability gate's 400
// PGRST127.
func TestUnsupportedMethodIs405PGRST117(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodTrace, "/films", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST117" {
		t.Errorf("code = %v, want PGRST117", env["code"])
	}
}

// TestDeleteOnRPCIsPGRST101 checks PUT/PATCH/DELETE on a function keep
// PostgREST's PGRST101 with the exact "Cannot use the <method> method on RPC"
// text, distinct from the PGRST117 unsupported-method case.
func TestDeleteOnRPCIsPGRST101(t *testing.T) {
	srv := newRPCServer(t)
	resp := do(t, srv, http.MethodDelete, "/rpc/add_them", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST101" {
		t.Errorf("code = %v, want PGRST101", env["code"])
	}
	if env["message"] != "Cannot use the DELETE method on RPC" {
		t.Errorf("message = %v", env["message"])
	}
}
