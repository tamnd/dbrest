package httpapi_test

import (
	"net/http"
	"testing"
)

// TestCORSPreflightDefault checks the permissive default: any origin gets a
// wildcard preflight answer with the PostgREST method and header lists.
func TestCORSPreflightDefault(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodOptions, "/films", map[string]string{
		"Origin":                         "http://example.com",
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "Foo,Bar",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200", resp.StatusCode)
	}
	want := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, POST, PATCH, PUT, DELETE, OPTIONS, HEAD",
		"Access-Control-Allow-Headers": "Authorization, Foo, Bar, Accept, Accept-Language, Content-Language",
		"Access-Control-Max-Age":       "86400",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("wildcard preflight must not carry Allow-Credentials")
	}
}

// TestCORSSimpleRequestDefault checks that a plain cross-origin read carries
// the wildcard origin and the exposed-headers list.
func TestCORSSimpleRequestDefault(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Origin": "http://example.com",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
	const expose = "Content-Encoding, Content-Location, Content-Range, Content-Type, " +
		"Date, Location, Server, Transfer-Encoding, Range-Unit"
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != expose {
		t.Errorf("Expose-Headers = %q", got)
	}
}

// TestCORSRestrictedOrigins checks server-cors-allowed-origins semantics: a
// listed origin is reflected with credentials, an unlisted one gets no CORS
// headers but the request still runs.
func TestCORSRestrictedOrigins(t *testing.T) {
	srv := newServer(t)
	srv.SetCORSAllowedOrigins([]string{"http://allowed.example"})

	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Origin": "http://allowed.example",
	})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://allowed.example" {
		t.Errorf("Allow-Origin = %q, want the reflected origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q, want true", got)
	}

	resp = do(t, srv, http.MethodGet, "/films", map[string]string{
		"Origin": "http://denied.example",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlisted origin must still be served, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin got Allow-Origin %q, want none", got)
	}

	preflight := do(t, srv, http.MethodOptions, "/films", map[string]string{
		"Origin":                        "http://allowed.example",
		"Access-Control-Request-Method": "POST",
	})
	if got := preflight.Header.Get("Access-Control-Allow-Origin"); got != "http://allowed.example" {
		t.Errorf("preflight Allow-Origin = %q", got)
	}
	if got := preflight.Header.Get("Access-Control-Allow-Headers"); got != "Authorization, Accept, Accept-Language, Content-Language" {
		t.Errorf("preflight Allow-Headers = %q", got)
	}
}

// TestCORSNoOriginUntouched checks that a same-origin request gets no CORS
// headers at all.
func TestCORSNoOriginUntouched(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films", nil)
	for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Expose-Headers"} {
		if got := resp.Header.Get(k); got != "" {
			t.Errorf("%s = %q on a request without Origin", k, got)
		}
	}
}
