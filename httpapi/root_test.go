package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/authz"
	"github.com/tamnd/dbrest/config"
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

// TestRootDisabledIs404 checks openapi-mode=disabled turns the root off.
func TestRootDisabledIs404(t *testing.T) {
	srv := newServer(t)
	srv.SetOpenAPI(config.OpenAPIDisabled, "")
	resp := do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
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
	srv.SetOpenAPI(config.OpenAPIFollowPrivileges, "")

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

	// Anon holds no grants: the document is empty, not an enumeration.
	resp = do(t, srv, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon status = %d, want 200", resp.StatusCode)
	}
	doc.Paths, doc.Definitions = nil, nil
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Paths) != 0 || len(doc.Definitions) != 0 {
		t.Errorf("anon sees paths %v definitions %v, want none", doc.Paths, doc.Definitions)
	}
}

// TestRootIgnorePrivilegesEmitsAll checks openapi-mode=ignore-privileges keeps
// the full document even for a role with no grants.
func TestRootIgnorePrivilegesEmitsAll(t *testing.T) {
	srv := authzServer(t, nil, nil)
	srv.SetOpenAPI(config.OpenAPIIgnorePrivileges, "")
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

// TestRootProxyURIRewritesHost checks openapi-server-proxy-uri overrides the
// host, scheme, and base path the document advertises.
func TestRootProxyURIRewritesHost(t *testing.T) {
	srv := newServer(t)
	srv.SetOpenAPI(config.OpenAPIFollowPrivileges, "https://api.example.com/v1")
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

	params := doc["paths"].(map[string]any)["/films"].(map[string]any)["get"].(map[string]any)["parameters"].([]any)
	for _, p := range params {
		pm := p.(map[string]any)
		if pm["name"] != "title" {
			continue
		}
		desc := pm["description"].(string)
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
}
