package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/authz"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/rpc"
)

// TestRootServesOpenAPI checks GET / returns the OpenAPI document with the
// right media type and describes the films table.
func TestRootServesOpenAPI(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/openapi+json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/openapi+json; charset=utf-8", ct)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["swagger"] != "2.0" {
		t.Errorf("swagger = %v, want 2.0", doc["swagger"])
	}
	paths := doc["paths"].(map[string]any)
	films, ok := paths["/films"].(map[string]any)
	if !ok {
		t.Fatal("document is missing the /films path")
	}
	for _, op := range []string{"get", "post", "patch", "delete"} {
		if _, ok := films[op]; !ok {
			t.Errorf("/films missing %s", op)
		}
	}
	def := doc["definitions"].(map[string]any)["films"].(map[string]any)
	if def["properties"].(map[string]any)["title"] == nil {
		t.Error("films definition missing title property")
	}
}

// TestRootHeadHasNoBody checks HEAD / carries the headers but no body.
func TestRootHeadHasNoBody(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodHead, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/openapi+json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("HEAD / should have no body")
	}
}

// TestRootNegotiatesAccept pins the root's Accept handling: openapi+json,
// plain json, and wildcards are served; anything else is 406 PGRST107 with
// the requested types echoed in q-descending order, parameters stripped.
func TestRootNegotiatesAccept(t *testing.T) {
	srv := newServer(t)
	for _, accept := range []string{
		"application/openapi+json",
		"application/json",
		"*/*",
		"application/*",
		"application/json;q=0.5, text/html", // one acceptable type suffices
	} {
		resp := do(t, srv, http.MethodGet, "/", map[string]string{"Accept": accept})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Accept %q: status = %d, want 200", accept, resp.StatusCode)
		}
	}

	resp := do(t, srv, http.MethodGet, "/", map[string]string{"Accept": "text/csv;q=0.3, application/xml"})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "PGRST107" {
		t.Errorf("code = %q, want PGRST107", e.Code)
	}
	if e.Message != "None of these media types are available: application/xml, text/csv" {
		t.Errorf("message = %q, want q-ordered type list", e.Message)
	}
}

// TestRootDisabledIs404 checks openapi-mode=disabled turns the root off with
// PostgREST's explicit PGRST126 code, not a bare not-found.
func TestRootDisabledIs404(t *testing.T) {
	srv := newServer(t)
	srv.SetOpenAPI(config.OpenAPIDisabled, "", false)
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != "PGRST126" {
		t.Errorf("code = %q, want PGRST126", e.Code)
	}
	if e.Message != "Root endpoint metadata is disabled" {
		t.Errorf("message = %q", e.Message)
	}
}

// TestRootMethodNotAllowed pins the verb gate at the root: anything besides
// GET, HEAD, and OPTIONS is 405 PGRST117 naming the method, with the Allow
// header listing what the root serves. The gate runs before the disabled
// check, so the answer is the same in every openapi-mode.
func TestRootMethodNotAllowed(t *testing.T) {
	srv := newServer(t)
	for _, mode := range []string{config.OpenAPIFollowPrivileges, config.OpenAPIDisabled} {
		srv.SetOpenAPI(mode, "", false)
		for _, method := range []string{http.MethodDelete, http.MethodPatch, http.MethodPost, http.MethodPut, "TRACE"} {
			resp := do(t, srv, method, "/", nil)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("mode %s %s /: status = %d, want 405", mode, method, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); allow != "OPTIONS,GET,HEAD" {
				t.Errorf("%s /: Allow = %q, want OPTIONS,GET,HEAD", method, allow)
			}
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if e.Code != "PGRST117" {
				t.Errorf("%s /: code = %q, want PGRST117", method, e.Code)
			}
			if e.Message != "Unsupported HTTP method: "+method {
				t.Errorf("%s /: message = %q", method, e.Message)
			}
		}
	}
}

// TestRootOptionsAnswersAllow checks OPTIONS / is 200 with the verb set and
// no body, the way PostgREST's info response answers it.
func TestRootOptionsAnswersAllow(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodOptions, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "OPTIONS,GET,HEAD" {
		t.Errorf("Allow = %q, want OPTIONS,GET,HEAD", allow)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("OPTIONS / should have no body")
	}
}

// TestRootFollowPrivilegesFiltersDocument checks the default openapi-mode:
// the document only describes the relations and operations the requesting
// role can access, so anon and an authenticated role see different documents.
func TestRootFollowPrivilegesFiltersDocument(t *testing.T) {
	srv := authzServer(t, []authz.Grant{
		{Role: "web_user", Relation: "films", Action: authz.Select},
		{Role: "web_user", Relation: "films", Action: authz.Insert},
	}, nil)
	srv.SetOpenAPI(config.OpenAPIFollowPrivileges, "", false)

	// The authenticated role sees films with exactly its granted operations.
	resp := do(t, srv, http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var doc struct {
		Paths       map[string]map[string]any `json:"paths"`
		Definitions map[string]any            `json:"definitions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	films, ok := doc.Paths["/films"]
	if !ok {
		t.Fatal("granted role should see /films")
	}
	for _, op := range []string{"get", "post"} {
		if _, ok := films[op]; !ok {
			t.Errorf("/films missing granted operation %s", op)
		}
	}
	for _, op := range []string{"patch", "delete"} {
		if _, ok := films[op]; ok {
			t.Errorf("/films advertises ungranted operation %s", op)
		}
	}

	// Anon holds no grants: nothing is enumerated. Only the "/" entry that
	// describes the document itself remains, as in v14.
	resp = do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon status = %d, want 200", resp.StatusCode)
	}
	doc.Paths, doc.Definitions = nil, nil
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Paths) != 1 {
		t.Errorf("anon sees paths %v, want only the root entry", doc.Paths)
	}
	if _, ok := doc.Paths["/"]; !ok {
		t.Errorf("anon paths = %v, want the root entry", doc.Paths)
	}
	if len(doc.Definitions) != 0 {
		t.Errorf("anon sees definitions %v, want none", doc.Definitions)
	}
}

// TestRootIgnorePrivilegesEmitsAll checks openapi-mode=ignore-privileges keeps
// the full document even for a role with no grants.
func TestRootIgnorePrivilegesEmitsAll(t *testing.T) {
	srv := authzServer(t, nil, nil)
	srv.SetOpenAPI(config.OpenAPIIgnorePrivileges, "", false)
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var doc struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := doc.Paths["/films"]; !ok {
		t.Error("ignore-privileges should still describe /films")
	}
}

// TestRootSecurityActive checks openapi-security-active emits the JWT scheme
// and a document-level security requirement, the way PostgREST v14 shapes it;
// off (the default) the document carries neither, even with JWT configured.
func TestRootSecurityActive(t *testing.T) {
	srv := authServer(t, auth.Config{})
	srv.SetOpenAPI(config.OpenAPIIgnorePrivileges, "", true)
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var doc struct {
		SecurityDefinitions map[string]map[string]any `json:"securityDefinitions"`
		Security            []map[string][]any        `json:"security"`
		Paths               map[string]map[string]struct {
			Security []map[string][]any `json:"security"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	jwt, ok := doc.SecurityDefinitions["JWT"]
	if !ok {
		t.Fatal("securityDefinitions missing the JWT scheme")
	}
	if jwt["type"] != "apiKey" || jwt["name"] != "Authorization" || jwt["in"] != "header" {
		t.Errorf("JWT scheme = %v", jwt)
	}
	if len(doc.Security) != 1 {
		t.Fatalf("security = %v, want one document-level requirement", doc.Security)
	}
	if _, ok := doc.Security[0]["JWT"]; !ok {
		t.Errorf("security requirement = %v, want JWT", doc.Security[0])
	}
	// v14 attaches the requirement at the document, never per operation.
	if sec := doc.Paths["/films"]["get"].Security; len(sec) != 0 {
		t.Errorf("get security = %v, want none per operation", sec)
	}

	// Off (the default): no securityDefinitions and no requirement at all.
	srv.SetOpenAPI(config.OpenAPIIgnorePrivileges, "", false)
	resp = do(t, srv, http.MethodGet, "/", nil)
	doc.SecurityDefinitions, doc.Security, doc.Paths = nil, nil, nil
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.SecurityDefinitions) != 0 {
		t.Errorf("securityDefinitions = %v, want none when inactive", doc.SecurityDefinitions)
	}
	if len(doc.Security) != 0 {
		t.Errorf("security = %v, want none when inactive", doc.Security)
	}
}

// TestRootProxyURIRewritesHost checks openapi-server-proxy-uri overrides the
// host, scheme, and base path the document advertises.
func TestRootProxyURIRewritesHost(t *testing.T) {
	srv := newServer(t)
	srv.SetOpenAPI(config.OpenAPIFollowPrivileges, "https://api.example.com/v1", false)
	resp := do(t, srv, http.MethodGet, "/", nil)
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["host"] != "api.example.com" {
		t.Errorf("host = %v, want api.example.com", doc["host"])
	}
	if doc["basePath"] != "/v1" {
		t.Errorf("basePath = %v, want /v1", doc["basePath"])
	}
	schemes := doc["schemes"].([]any)
	if len(schemes) != 1 || schemes[0] != "https" {
		t.Errorf("schemes = %v, want [https]", schemes)
	}
}

// TestRootAdvertisesServedOperators checks the document does not promise the
// array/range operators SQLite cannot serve, so a client reading the root is
// not led into a PGRST127.
func TestRootAdvertisesServedOperators(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/", nil)
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)

	// Operations reference the shared rowFilter parameters; the operator list
	// lives on the definition in the document's parameters map.
	title, ok := doc["parameters"].(map[string]any)["rowFilter.films.title"].(map[string]any)
	if !ok {
		t.Fatal("document is missing the rowFilter.films.title parameter")
	}
	desc := title["description"].(string)
	// match/imatch and fts are served on SQLite; the range operators are not.
	for _, want := range []string{"match", "fts"} {
		if !strings.Contains(desc, want) {
			t.Errorf("expected %q advertised; desc = %q", want, desc)
		}
	}
	if strings.Contains(desc, " sl,") || strings.Contains(desc, " adj.") {
		t.Errorf("range operators should not be advertised on SQLite; desc = %q", desc)
	}
}

// newRootSpecServer builds a server whose registry carries a custom-spec
// function and points db-root-spec at it.
func newRootSpecServer(t *testing.T) *httpapi.Server {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })
	if _, err := be.DB().Exec(`CREATE TABLE films (id INTEGER PRIMARY KEY, title TEXT NOT NULL);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	be.Register(rpc.NewStaticRegistry([]*rpc.Function{{
		Name:       "custom_spec",
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "json"},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: `SELECT json_object('swagger', '2.0', 'info', json_object('title', 'My Custom API'))`},
	}}))
	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	srv.SetRootSpec("custom_spec")
	return srv
}

// TestRootSpecOverridesDocument pins db-root-spec: the named function's JSON
// result replaces the generated document, served with the root's media type.
func TestRootSpecOverridesDocument(t *testing.T) {
	srv := newRootSpecServer(t)
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/openapi+json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["swagger"] != "2.0" {
		t.Errorf("swagger = %v", doc["swagger"])
	}
	info, ok := doc["info"].(map[string]any)
	if !ok || info["title"] != "My Custom API" {
		t.Errorf("info = %v, want the custom title", doc["info"])
	}
	if _, generated := doc["paths"]; generated {
		t.Error("the generated document should be fully replaced")
	}
}

// TestRootSpecDisabledStaysOff checks openapi-mode=disabled wins over
// db-root-spec: the root stays a 404 PGRST126.
func TestRootSpecDisabledStaysOff(t *testing.T) {
	srv := newRootSpecServer(t)
	srv.SetOpenAPI(config.OpenAPIDisabled, "", false)
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST126" {
		t.Errorf("code = %v, want PGRST126", env["code"])
	}
}
