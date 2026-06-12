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

// ── 06.5 root verb handling ────────────────────────────────────────────────

// A verb the root does not serve is 405 PGRST117 naming the method.
func TestRootUnsupportedVerb(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		for _, method := range []string{"DELETE", "PATCH", "PUT", "TRACE"} {
			res := doRequest(t, base, compatCase{method: method, path: "/"})
			if res.status != http.StatusMethodNotAllowed {
				t.Fatalf("%s /: status = %d, want 405\n%s", method, res.status, res.body)
			}
			e := decodeErr(t, res.body)
			if e.Code != "PGRST117" {
				t.Errorf("%s /: code = %q, want PGRST117", method, e.Code)
			}
			if e.Message != "Unsupported HTTP method: "+method {
				t.Errorf("%s /: message = %q", method, e.Message)
			}
		}
	})
}

// OPTIONS on the root answers 200 with the verb set in Allow and no body.
func TestRootOptionsAllow(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "OPTIONS", path: "/"})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200\n%s", res.status, res.body)
		}
		if allow := res.header.Get("Allow"); allow != "OPTIONS,GET,HEAD" {
			t.Errorf("Allow = %q, want OPTIONS,GET,HEAD", allow)
		}
		if len(res.body) != 0 {
			t.Errorf("body = %q, want empty", res.body)
		}
	})
}

// ── 06.6 document shape ────────────────────────────────────────────────────

// TestRootDocumentFraming pins the framing both servers must share: the v14
// info defaults, the externalDocs pointer, the vendor media types, and the "/"
// entry describing the document itself.
func TestRootDocumentFraming(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/"})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.status)
		}
		var doc struct {
			Info struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"info"`
			ExternalDocs struct {
				Description string `json:"description"`
				URL         string `json:"url"`
			} `json:"externalDocs"`
			Consumes []string `json:"consumes"`
			Produces []string `json:"produces"`
			Paths    map[string]map[string]struct {
				Summary  string   `json:"summary"`
				Tags     []string `json:"tags"`
				Produces []string `json:"produces"`
			} `json:"paths"`
		}
		if err := json.Unmarshal(res.body, &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.Info.Title != "PostgREST API" {
			t.Errorf("info title = %q", doc.Info.Title)
		}
		if doc.Info.Description != "This is a dynamic API generated by PostgREST" {
			t.Errorf("info description = %q", doc.Info.Description)
		}
		if doc.ExternalDocs.URL != "https://postgrest.org/en/v14/references/api.html" {
			t.Errorf("externalDocs url = %q", doc.ExternalDocs.URL)
		}
		if doc.ExternalDocs.Description != "PostgREST Documentation" {
			t.Errorf("externalDocs description = %q", doc.ExternalDocs.Description)
		}
		want := []string{
			"application/json",
			"application/vnd.pgrst.object+json;nulls=stripped",
			"application/vnd.pgrst.object+json",
			"text/csv",
		}
		if strings.Join(doc.Consumes, " ") != strings.Join(want, " ") {
			t.Errorf("consumes = %v", doc.Consumes)
		}
		if strings.Join(doc.Produces, " ") != strings.Join(want, " ") {
			t.Errorf("produces = %v", doc.Produces)
		}
		root, ok := doc.Paths["/"]
		if !ok {
			t.Fatal(`document lacks the "/" path entry`)
		}
		get := root["get"]
		if get.Summary != "OpenAPI description (this document)" {
			t.Errorf("root summary = %q", get.Summary)
		}
		if len(get.Tags) != 1 || get.Tags[0] != "Introspection" {
			t.Errorf("root tags = %v", get.Tags)
		}
		if len(get.Produces) != 2 || get.Produces[0] != "application/openapi+json" || get.Produces[1] != "application/json" {
			t.Errorf("root produces = %v", get.Produces)
		}
	})
}

// TestRootRelationLayout pins how a table is laid out: operations reference
// the shared rowFilter and reserved parameters, GET answers 200 with an
// array-of-definition schema plus a 206, writes carry the v14 single-status
// responses, and the primary key carries the pk marker in its definition.
func TestRootRelationLayout(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/"})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.status)
		}
		var doc struct {
			Paths map[string]map[string]struct {
				Parameters []struct {
					Ref string `json:"$ref"`
				} `json:"parameters"`
				Responses map[string]struct {
					Description string `json:"description"`
					Schema      *struct {
						Type  string `json:"type"`
						Items *struct {
							Ref string `json:"$ref"`
						} `json:"items"`
					} `json:"schema"`
				} `json:"responses"`
			} `json:"paths"`
			Parameters map[string]struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required *bool  `json:"required"`
				Default  string `json:"default"`
			} `json:"parameters"`
			Definitions map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"definitions"`
		}
		if err := json.Unmarshal(res.body, &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		todos, ok := doc.Paths["/todos"]
		if !ok {
			t.Fatal("document lacks /todos")
		}

		// GET: rowFilter refs for the columns, then the fixed read block.
		get := todos["get"]
		readBlock := []string{
			"#/parameters/select", "#/parameters/order", "#/parameters/range",
			"#/parameters/rangeUnit", "#/parameters/offset", "#/parameters/limit",
			"#/parameters/preferCount",
		}
		if len(get.Parameters) < len(readBlock)+1 {
			t.Fatalf("get parameters = %v", get.Parameters)
		}
		head := get.Parameters[:len(get.Parameters)-len(readBlock)]
		for i, p := range head {
			if !strings.HasPrefix(p.Ref, "#/parameters/rowFilter.todos.") {
				t.Errorf("get parameter %d = %q, want a rowFilter.todos ref", i, p.Ref)
			}
		}
		tail := get.Parameters[len(get.Parameters)-len(readBlock):]
		for i, want := range readBlock {
			if tail[i].Ref != want {
				t.Errorf("get read block[%d] = %q, want %q", i, tail[i].Ref, want)
			}
		}

		// GET responses: 200 carries the array-of-definition schema, plus a 206.
		ok200, present := get.Responses["200"]
		if !present || ok200.Schema == nil || ok200.Schema.Type != "array" ||
			ok200.Schema.Items == nil || ok200.Schema.Items.Ref != "#/definitions/todos" {
			t.Errorf("get 200 = %+v, want an array of #/definitions/todos", ok200)
		}
		if p206, present := get.Responses["206"]; !present || p206.Description != "Partial Content" {
			t.Errorf("get 206 = %+v, want Partial Content", get.Responses["206"])
		}

		// Writes: POST 201 only; PATCH and DELETE 204 only.
		for op, want := range map[string]string{"post": "201", "patch": "204", "delete": "204"} {
			r := todos[op].Responses
			if len(r) != 1 {
				t.Errorf("%s responses = %v, want only %s", op, r, want)
			}
			if _, present := r[want]; !present {
				t.Errorf("%s responses lack %s", op, want)
			}
		}

		// POST opens with the shared body parameter.
		if post := todos["post"]; len(post.Parameters) == 0 || post.Parameters[0].Ref != "#/parameters/body.todos" {
			t.Errorf("post parameters = %v, want body.todos first", post.Parameters)
		}
		body, present := doc.Parameters["body.todos"]
		if !present || body.Name != "todos" || body.In != "body" {
			t.Errorf("body.todos = %+v", body)
		}
		if body.Required == nil || *body.Required {
			t.Errorf("body.todos required = %v, want explicit false", body.Required)
		}
		if ru, present := doc.Parameters["rangeUnit"]; !present || ru.Default != "items" {
			t.Errorf("rangeUnit = %+v, want default items", doc.Parameters["rangeUnit"])
		}

		// The primary key column carries the v14 pk marker.
		def, present := doc.Definitions["todos"]
		if !present {
			t.Fatal("definitions lack todos")
		}
		id, present := def.Properties["id"]
		if !present || !strings.Contains(id.Description, "Note:\nThis is a Primary Key.<pk/>") {
			t.Errorf("todos.id description = %q, want the pk marker", id.Description)
		}
		if len(def.Required) == 0 {
			t.Error("todos definition lists no required columns")
		}
	})
}

// TestRootSecurityInactiveByDefault pins the default openapi-security-active
// shape: with it off the document carries neither securityDefinitions nor a
// security requirement, even though both servers authenticate JWTs.
func TestRootSecurityInactiveByDefault(t *testing.T) {
	onBoth(t, func(t *testing.T, base string) {
		res := doRequest(t, base, compatCase{method: "GET", path: "/"})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.status)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(res.body, &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := doc["securityDefinitions"]; ok {
			t.Error("securityDefinitions should be absent by default")
		}
		if _, ok := doc["security"]; ok {
			t.Error("security should be absent by default")
		}
	})
}
