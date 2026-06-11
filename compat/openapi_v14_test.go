// openapi_v14_test.go holds the v14 conformance tests for the OpenAPI root and
// the schema-profile machinery (audit topic 06): profile negotiation and
// PGRST106, root content negotiation, the document shape, and the schema
// cache. Each test runs against both live servers with the same harness as
// compat_test.go and asserts the exact v14 wire behavior, verified against
// PostgREST v14 directly.
package compat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// errBody is the PostgREST error envelope.
type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
	Details any    `json:"details"`
}

func decodeErr(t *testing.T, body []byte) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, body)
	}
	return e
}

// onBoth runs fn once per live server, as a subtest named for it.
func onBoth(t *testing.T, fn func(t *testing.T, base string)) {
	pgrest, dbrest := urls(t)
	for name, base := range map[string]string{"postgrest": pgrest, "dbrest": dbrest} {
		t.Run(name, func(t *testing.T) { fn(t, base) })
	}
}

// ── 06.1 profile headers and the active schema ─────────────────────────────

func TestProfileUnknownSchemaGET(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/todos",
			headers: map[string]string{"Accept-Profile": "nonexistent"}})
		if res.status != http.StatusNotAcceptable {
			t.Fatalf("status = %d, want 406\n%s", res.status, res.body)
		}
		e := decodeErr(t, res.body)
		if e.Code != "PGRST106" {
			t.Errorf("code = %q, want PGRST106", e.Code)
		}
		if e.Message != "Invalid schema: nonexistent" {
			t.Errorf("message = %q, want %q", e.Message, "Invalid schema: nonexistent")
		}
		if e.Hint != "Only the following schemas are exposed: api, private" {
			t.Errorf("hint = %q, want the exposed-schema list", e.Hint)
		}
		if h := res.header.Get("Content-Profile"); h != "" {
			t.Errorf("Content-Profile = %q on an error, want unset", h)
		}
	})
}

func TestProfileUnknownSchemaPOST(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "POST", path: "/todos",
			headers: map[string]string{"Content-Profile": "nope", "Content-Type": "application/json"},
			body:    "{}"})
		if res.status != http.StatusNotAcceptable {
			t.Fatalf("status = %d, want 406\n%s", res.status, res.body)
		}
		e := decodeErr(t, res.body)
		if e.Code != "PGRST106" || e.Message != "Invalid schema: nope" {
			t.Errorf("got %q %q, want PGRST106 / Invalid schema: nope", e.Code, e.Message)
		}
	})
}

// A write reads Content-Profile, never Accept-Profile: a bogus Accept-Profile
// on a DELETE is ignored.
func TestProfileWriteIgnoresAcceptProfile(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "DELETE", path: "/todos?id=eq.999999",
			headers: map[string]string{"Accept-Profile": "nonexistent"}})
		if res.status != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (Accept-Profile ignored on DELETE)\n%s", res.status, res.body)
		}
	})
}

func TestProfileSelectsSchemaAndEchoes(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/items",
			headers: map[string]string{"Accept-Profile": "private"}})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", res.status, res.body)
		}
		if h := res.header.Get("Content-Profile"); h != "private" {
			t.Errorf("Content-Profile = %q, want private", h)
		}
	})
}

// With no profile header on a multi-schema deployment the first exposed schema
// is active and is echoed in Content-Profile.
func TestProfileDefaultSchemaEchoed(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/todos"})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", res.status, res.body)
		}
		if h := res.header.Get("Content-Profile"); h != "api" {
			t.Errorf("Content-Profile = %q, want api (first exposed schema)", h)
		}
	})
}

// A failed request carries no Content-Profile even when the profile was valid.
func TestProfileNotEchoedOnError(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/no_such_table",
			headers: map[string]string{"Accept-Profile": "api"}})
		if res.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404\n%s", res.status, res.body)
		}
		if h := res.header.Get("Content-Profile"); h != "" {
			t.Errorf("Content-Profile = %q on an error, want unset", h)
		}
	})
}

// The root document is scoped to the active schema: under Accept-Profile:
// private it describes private's relations, not api's.
func TestRootScopedToActiveSchema(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/",
			headers: map[string]string{"Accept-Profile": "private"}})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", res.status, res.body)
		}
		if h := res.header.Get("Content-Profile"); h != "private" {
			t.Errorf("Content-Profile = %q, want private", h)
		}
		var doc struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if err := json.Unmarshal(res.body, &doc); err != nil {
			t.Fatalf("root is not JSON: %v", err)
		}
		if _, ok := doc.Paths["/items"]; !ok {
			t.Errorf("paths lack /items; private schema not described: %v", pathKeys(doc.Paths))
		}
		if _, ok := doc.Paths["/todos"]; ok {
			t.Errorf("paths include /todos from the api schema; root not scoped: %v", pathKeys(doc.Paths))
		}
	})
}

func pathKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── 06.2 root content negotiation and charset ──────────────────────────────

func TestRootContentTypeCharset(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		for _, accept := range []string{"", "application/json", "application/openapi+json", "*/*"} {
			c := compatCase{method: "GET", path: "/"}
			if accept != "" {
				c.headers = map[string]string{"Accept": accept}
			}
			res := doRequest(t, base, c)
			if res.status != http.StatusOK {
				t.Fatalf("Accept %q: status = %d, want 200", accept, res.status)
			}
			if ct := res.header.Get("Content-Type"); ct != "application/openapi+json; charset=utf-8" {
				t.Errorf("Accept %q: Content-Type = %q, want application/openapi+json; charset=utf-8", accept, ct)
			}
		}
	})
}

func TestRootUnacceptableAcceptIs406(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/",
			headers: map[string]string{"Accept": "text/csv"}})
		if res.status != http.StatusNotAcceptable {
			t.Fatalf("status = %d, want 406\n%s", res.status, res.body)
		}
		e := decodeErr(t, res.body)
		if e.Code != "PGRST107" {
			t.Errorf("code = %q, want PGRST107", e.Code)
		}
		if e.Message != "None of these media types are available: text/csv" {
			t.Errorf("message = %q, want the requested type echoed", e.Message)
		}
	})
}

// A path segment is a bare name inside the active schema, never a qualified
// reference into another one.
func TestDottedPathDoesNotEscapeSchema(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/private.items"})
		if res.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (no cross-schema escape)\n%s", res.status, res.body)
		}
		if !strings.Contains(string(res.body), "PGRST205") {
			t.Errorf("body = %s, want PGRST205", res.body)
		}
	})
}
