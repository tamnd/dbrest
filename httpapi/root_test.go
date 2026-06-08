package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
	if ct := resp.Header.Get("Content-Type"); ct != "application/openapi+json" {
		t.Errorf("Content-Type = %q, want application/openapi+json", ct)
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
	if ct := resp.Header.Get("Content-Type"); ct != "application/openapi+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Error("HEAD / should have no body")
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
