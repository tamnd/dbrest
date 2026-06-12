package auth

import (
	"encoding/json"
	"testing"
	"time"
)

// validateClaims applies PostgREST's exp/nbf/iat/aud rules with the 30s skew.
// It is reached directly here because it also guards the cache-hit path, which
// is hard to provoke through the public verify path without racing the cache.
func TestValidateClaims(t *testing.T) {
	v := hmacVerifier(t, Config{})
	now := clockNow.Unix()

	cases := []struct {
		name    string
		claims  map[string]any
		expErr  string // "" means it must pass
		message string // when non-empty the exact PGRST message
	}{
		{"valid window", map[string]any{"exp": float64(now + 60), "nbf": float64(now - 60)}, "", ""},
		{"no claims at all", map[string]any{}, "", ""},
		{"expired", map[string]any{"exp": float64(now - 60)}, "PGRST303", "JWT expired"},
		{"expired within skew", map[string]any{"exp": float64(now - 10)}, "", ""},
		{"not yet valid", map[string]any{"nbf": float64(now + 60)}, "PGRST303", "JWT not yet valid"},
		{"not-before within skew", map[string]any{"nbf": float64(now + 10)}, "", ""},
		{"issued in future", map[string]any{"iat": float64(now + 60)}, "PGRST303", "JWT issued at future"},
		{"issued-at within skew", map[string]any{"iat": float64(now + 10)}, "", ""},
		{"issued in past is fine", map[string]any{"iat": float64(now - 3600)}, "", ""},
		{"exp not a number", map[string]any{"exp": "soon"}, "PGRST303", "The JWT 'exp' claim must be a number"},
		{"nbf not a number", map[string]any{"nbf": true}, "PGRST303", "The JWT 'nbf' claim must be a number"},
		{"iat not a number", map[string]any{"iat": "x"}, "PGRST303", "The JWT 'iat' claim must be a number"},
		{"null time claim passes", map[string]any{"exp": nil}, "", ""},
		{"exp checked before nbf", map[string]any{
			"exp": float64(now - 60), "nbf": float64(now + 60),
		}, "PGRST303", "JWT expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := v.validateClaims(c.claims)
			if c.expErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || err.Code != c.expErr {
				t.Fatalf("want %s, got %v", c.expErr, err)
			}
			if c.message != "" && err.Message != c.message {
				t.Errorf("message = %q, want %q", err.Message, c.message)
			}
		})
	}
}

// The aud rules with a configured jwt-aud: absent, null, and empty-array
// audiences pass (the token is valid for all audiences), a matching string or
// array element passes, and a wrong type is its own PGRST303.
func TestValidateClaimsAudience(t *testing.T) {
	v := hmacVerifier(t, Config{Audience: testAud})

	cases := []struct {
		name    string
		aud     any
		present bool
		expErr  string
		message string
	}{
		{"absent aud passes", nil, false, "", ""},
		{"null aud passes", nil, true, "", ""},
		{"empty array passes", []any{}, true, "", ""},
		{"matching string", testAud, true, "", ""},
		{"matching array element", []any{"other", testAud}, true, "", ""},
		{"wrong string", "other", true, "PGRST303", "JWT not in audience"},
		{"no array element matches", []any{"a", "b"}, true, "PGRST303", "JWT not in audience"},
		{"non-string element", []any{42}, true, "PGRST303", "The JWT 'aud' claim must be a string or an array of strings"},
		{"number aud", float64(7), true, "PGRST303", "The JWT 'aud' claim must be a string or an array of strings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := map[string]any{}
			if c.present {
				claims["aud"] = c.aud
			}
			err := v.validateClaims(claims)
			if c.expErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || err.Code != c.expErr || err.Message != c.message {
				t.Fatalf("want %s %q, got %v", c.expErr, c.message, err)
			}
		})
	}
}

// With no jwt-aud configured every audience is accepted.
func TestAudienceUncheckedWhenUnconfigured(t *testing.T) {
	v := hmacVerifier(t, Config{})
	if err := v.validateClaims(map[string]any{"aud": "anything"}); err != nil {
		t.Fatalf("aud must be ignored with no jwt-aud, got %v", err)
	}
}

// claimNumber reads a numeric claim across the forms a decoded claim set can
// carry, and reports false for a value whose type is not a number.
func TestClaimNumber(t *testing.T) {
	cases := []struct {
		name  string
		in    any
		want  int64
		wantO bool
	}{
		{"float64", float64(1700), 1700, true},
		{"int64", int64(1700), 1700, true},
		{"json.Number", json.Number("1700"), 1700, true},
		{"json.Number scientific", json.Number("1.5e3"), 1500, true},
		{"wrong type", "1700", 0, false},
		{"bool", true, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := claimNumber(c.in)
			if got != c.want || ok != c.wantO {
				t.Errorf("claimNumber = (%d, %v), want (%d, %v)", got, ok, c.want, c.wantO)
			}
		})
	}
}

// A guard that the test clock and skew defaults are what the time cases assume,
// so a future change to either is caught here rather than silently shifting the
// windows above.
func TestValidateClaimsAssumptions(t *testing.T) {
	v := hmacVerifier(t, Config{})
	if v.skew != 30*time.Second {
		t.Fatalf("skew = %v, want the 30s default the cases assume", v.skew)
	}
}
