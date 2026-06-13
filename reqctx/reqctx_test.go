package reqctx

import "testing"

func TestClaimsJSONEmptyIsObject(t *testing.T) {
	c := &Context{}
	if got := string(c.ClaimsJSON()); got != "{}" {
		t.Errorf("ClaimsJSON() = %q, want {}", got)
	}
}

func TestClaimsJSONMarshalsClaims(t *testing.T) {
	c := &Context{Claims: map[string]any{"role": "web_user", "sub": "alice"}}
	// encoding/json sorts map keys, so the document is deterministic.
	want := `{"role":"web_user","sub":"alice"}`
	if got := string(c.ClaimsJSON()); got != want {
		t.Errorf("ClaimsJSON() = %q, want %q", got, want)
	}
}

func TestHeadersJSONFlattensAndLowercases(t *testing.T) {
	c := &Context{Headers: map[string][]string{
		"X-Tenant":      {"acme"},
		"Accept":        {"application/json"},
		"Cache-Control": {"no-cache", "no-store"},
	}}
	// Lower-cased names, sorted keys, a repeated header resolved to its last value
	// (PostgREST's later-wins), not a comma-join.
	want := `{"accept":"application/json","cache-control":"no-store","x-tenant":"acme"}`
	if got := string(c.HeadersJSON()); got != want {
		t.Errorf("HeadersJSON() = %q, want %q", got, want)
	}
}

// The Cookie header is excluded from request.headers (it is request.cookies),
// matching PostgREST, verified against PostgREST 14.12. A repeated header keeps
// its last value.
func TestHeadersJSONExcludesCookieAndKeepsLast(t *testing.T) {
	c := &Context{Headers: map[string][]string{
		"Cookie": {"sessionid=abc123"},
		"X-Dup":  {"first", "second"},
	}}
	want := `{"x-dup":"second"}`
	if got := string(c.HeadersJSON()); got != want {
		t.Errorf("HeadersJSON() = %q, want %q", got, want)
	}
}

// The resolved role is folded into request.jwt.claims under "role", overwriting
// any token role, matching PostgREST 14.12 (anonymous presents the anon role).
func TestClaimsJSONFoldsInRole(t *testing.T) {
	// Anonymous: no claims, the resolved role still appears.
	anon := &Context{Role: "anon"}
	if got := string(anon.ClaimsJSON()); got != `{"role":"anon"}` {
		t.Errorf("anon ClaimsJSON() = %q, want {\"role\":\"anon\"}", got)
	}
	// Authenticated: the resolved role overwrites whatever the token carried.
	auth := &Context{Role: "web_user", Claims: map[string]any{"role": "stale", "sub": "alice"}}
	if got := string(auth.ClaimsJSON()); got != `{"role":"web_user","sub":"alice"}` {
		t.Errorf("auth ClaimsJSON() = %q", got)
	}
}

func TestHeadersJSONEmptyIsObject(t *testing.T) {
	c := &Context{}
	if got := string(c.HeadersJSON()); got != "{}" {
		t.Errorf("HeadersJSON() = %q, want {}", got)
	}
}

func TestCookiesJSONSortsKeys(t *testing.T) {
	c := &Context{Cookies: map[string]string{"session": "abc", "csrf": "xyz"}}
	want := `{"csrf":"xyz","session":"abc"}`
	if got := string(c.CookiesJSON()); got != want {
		t.Errorf("CookiesJSON() = %q, want %q", got, want)
	}
}

func TestCookiesJSONEscapesSpecials(t *testing.T) {
	c := &Context{Cookies: map[string]string{"k": `a"b`}}
	want := `{"k":"a\"b"}`
	if got := string(c.CookiesJSON()); got != want {
		t.Errorf("CookiesJSON() = %q, want %q", got, want)
	}
}

func TestControlsSetStatusAndHeader(t *testing.T) {
	c := &Context{}
	ctrl := c.Controls()
	ctrl.SetStatus(201)
	ctrl.SetHeader("X-Total", "5")
	// Controls returns the same backing value each call.
	if c.Controls().Status != 201 {
		t.Errorf("Status = %d, want 201", c.Controls().Status)
	}
	if c.Controls().Headers["X-Total"] != "5" {
		t.Errorf("Headers[X-Total] = %q, want 5", c.Controls().Headers["X-Total"])
	}
}

func BenchmarkHeadersJSON(b *testing.B) {
	c := &Context{Headers: map[string][]string{
		"Accept":        {"application/json"},
		"Authorization": {"Bearer x"},
		"X-Tenant":      {"acme"},
		"User-Agent":    {"dbrest-test"},
	}}
	b.ReportAllocs()
	for b.Loop() {
		_ = c.HeadersJSON()
	}
}
