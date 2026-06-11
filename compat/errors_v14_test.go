// PostgREST v14 error-vocabulary conformance checks (review item series 04.x).
// These run only when both a live PostgREST and a live dbrest are reachable,
// using the same harness as compat_test.go.
package compat

import (
	"encoding/json"
	"net/http"
	"testing"
)

// errEnvelope is the four-key PostgREST error body.
type errEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
	Hint    json.RawMessage `json:"hint"`
}

func decodeEnvelope(t *testing.T, r response) errEnvelope {
	t.Helper()
	var e errEnvelope
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("error body is not a JSON envelope: %v: %s", err, r.body)
	}
	return e
}

// TestContentTypeContract locks the request Content-Type error contract
// (review item 04.1 task 4). The published v14 error table still carries a
// stale PGRST107/415 row for an invalid request Content-Type; live v14
// actually answers 400 PGRST102 "Content-Type not acceptable: <mime>", which
// this probe verified against a running PostgREST. The probe pins the live
// behavior on both servers so a regression on either side is caught.
func TestContentTypeContract(t *testing.T) {
	pgrest, dbrest := urls(t)
	c := compatCase{
		name:   "unsupported request content-type",
		method: "POST",
		path:   "/todos",
		headers: map[string]string{
			"Content-Type": "application/yaml",
		},
		body: "task: write tests",
	}

	for name, base := range map[string]string{"postgrest": pgrest, "dbrest": dbrest} {
		resp := doRequest(t, base, c)
		env := decodeEnvelope(t, resp)
		if resp.status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", name, resp.status)
		}
		if env.Code != "PGRST102" {
			t.Errorf("%s code = %q, want PGRST102", name, env.Code)
		}
		if want := "Content-Type not acceptable: application/yaml"; env.Message != want {
			t.Errorf("%s message = %q, want %q", name, env.Message, want)
		}
	}
}
