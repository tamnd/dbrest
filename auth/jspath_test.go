package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The accepted forms, straight from the v14 jwt-role-claim-key documentation:
// plain keys, nested keys, quoted namespaced keys, array indexes, and the five
// filter operators.
func TestParseJSPathAcceptedForms(t *testing.T) {
	cases := []string{
		".role",
		".roles[0]",
		".app_metadata.role",
		`."https://example.com/role"`,
		`.realm_access.roles[?(@ == "client_admin")]`,
		`.roles[?(@ != "user")]`,
		`.roles[?(@ ^== "adm")]`,
		`.roles[?(@ ==^ "min")]`,
		`.roles[?(@ *== "dmi")]`,
		`.roles[?(@=="compact")]`,
		".a.b[2].c",
		"[0].role",
	}
	for _, c := range cases {
		if _, err := parseJSPath(c); err != nil {
			t.Errorf("parseJSPath(%q): %v", c, err)
		}
	}
}

// An empty key falls back to the default ".role" path rather than erroring.
func TestParseJSPathEmptyDefaultsToRole(t *testing.T) {
	path, err := parseJSPath("")
	if err != nil {
		t.Fatalf("parseJSPath(\"\"): %v", err)
	}
	if len(path) != 1 || path[0].kind != jspKey || path[0].key != "role" {
		t.Fatalf("default path = %+v, want [.role]", path)
	}
}

// The rejected forms: a missing leading dot, unterminated brackets and quotes,
// an unknown operator, and a filter that is not the final element. PostgREST
// refuses these at config load, so the verifier must refuse them at startup.
func TestParseJSPathRejectedForms(t *testing.T) {
	cases := []string{
		"role",
		".",
		".roles[",
		".roles[1",
		".roles[x]",
		`.roles[?(@ = "x")]`,
		`.roles[?(@ == "x")`,
		`.roles[?(@ == "x)]`,
		`.roles[?(@ == "x")].more`,
		`."unterminated`,
		".role extra",
	}
	for _, c := range cases {
		if _, err := parseJSPath(c); err == nil {
			t.Errorf("parseJSPath(%q): want error, got none", c)
		}
	}
}

// An invalid jwt-role-claim-key is a startup error on NewVerifier, never a
// silently broken verifier.
func TestInvalidRoleClaimKeyRefusedAtStartup(t *testing.T) {
	_, err := NewVerifier(Config{Secret: hmacKey, RoleClaimKey: "role"})
	if err == nil {
		t.Fatal("a role-claim-key without a leading dot must be refused")
	}
}

// walkJSPath drives role resolution end to end through Authenticate for each
// documented form.
func TestRoleClaimKeyForms(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		claims jwt.MapClaims
		want   string
	}{
		{"array index", ".roles[1]",
			jwt.MapClaims{"roles": []any{"alpha", "beta"}}, "beta"},
		{"quoted namespaced key", `."https://example.com/role"`,
			jwt.MapClaims{"https://example.com/role": "web_user"}, "web_user"},
		{"keycloak filter", `.realm_access.roles[?(@ == "client_admin")]`,
			jwt.MapClaims{"realm_access": map[string]any{
				"roles": []any{"offline_access", "client_admin"},
			}}, "client_admin"},
		{"not-equals filter takes first non-match", `.roles[?(@ != "user")]`,
			jwt.MapClaims{"roles": []any{"user", "editor", "admin"}}, "editor"},
		{"prefix filter", `.roles[?(@ ^== "web_")]`,
			jwt.MapClaims{"roles": []any{"admin", "web_user"}}, "web_user"},
		{"suffix filter", `.roles[?(@ ==^ "_user")]`,
			jwt.MapClaims{"roles": []any{"admin", "web_user"}}, "web_user"},
		{"contains filter", `.roles[?(@ *== "b_us")]`,
			jwt.MapClaims{"roles": []any{"admin", "web_user"}}, "web_user"},
		{"index out of range falls back to anon", ".roles[5]",
			jwt.MapClaims{"roles": []any{"only"}}, anonRole},
		{"filter with no match falls back to anon", `.roles[?(@ == "nope")]`,
			jwt.MapClaims{"roles": []any{"admin"}}, anonRole},
		{"filter over non-array falls back to anon", `.role[?(@ == "x")]`,
			jwt.MapClaims{"role": "admin"}, anonRole},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := hmacVerifier(t, Config{RoleClaimKey: c.key})
			c.claims["exp"] = clockNow.Add(time.Hour).Unix()
			res, err := v.Authenticate("Bearer " + signHS(t, c.claims))
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if res.Role != c.want {
				t.Errorf("role = %q, want %q", res.Role, c.want)
			}
		})
	}
}
