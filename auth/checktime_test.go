package auth

import (
	"encoding/json"
	"testing"
	"time"
)

// checkTime re-validates the exp and nbf claims on a cache hit, so a cached
// verification can never resurrect a token that has since expired or is not yet
// valid. It is reached directly here because the cache-hit revalidation is hard
// to provoke through the public verify path without racing the cache.
func TestCheckTime(t *testing.T) {
	v := hmacVerifier(t, Config{})
	now := clockNow.Unix()

	cases := []struct {
		name   string
		claims map[string]any
		expErr string // "" means it must pass
	}{
		{"valid window", map[string]any{"exp": float64(now + 60), "nbf": float64(now - 60)}, ""},
		{"no time claims", map[string]any{}, ""},
		{"expired", map[string]any{"exp": float64(now - 60)}, "PGRST303"},
		{"expired within skew", map[string]any{"exp": float64(now - 10)}, ""}, // 30s skew
		{"not yet valid", map[string]any{"nbf": float64(now + 60)}, "PGRST303"},
		{"not-before within skew", map[string]any{"nbf": float64(now + 10)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := v.checkTime(c.claims)
			if c.expErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || err.Code != c.expErr {
				t.Fatalf("want %s, got %v", c.expErr, err)
			}
		})
	}
}

// numClaim reads a numeric time claim across the forms a decoded claim set can
// carry, and reports false for an absent claim or one whose type or value is not
// a usable integer.
func TestNumClaim(t *testing.T) {
	cases := []struct {
		name  string
		in    any
		want  int64
		wantO bool
	}{
		{"float64", float64(1700), 1700, true},
		{"int64", int64(1700), 1700, true},
		{"json.Number", json.Number("1700"), 1700, true},
		{"json.Number non-integer", json.Number("1.5e3"), 0, false},
		{"absent", nil, 0, false},
		{"wrong type", "1700", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := map[string]any{}
			if c.in != nil {
				claims["exp"] = c.in
			}
			got, ok := numClaim(claims, "exp")
			if got != c.want || ok != c.wantO {
				t.Errorf("numClaim = (%d, %v), want (%d, %v)", got, ok, c.want, c.wantO)
			}
		})
	}
}

// A guard that the test clock and skew defaults are what the time cases assume,
// so a future change to either is caught here rather than silently shifting the
// windows above.
func TestCheckTimeAssumptions(t *testing.T) {
	v := hmacVerifier(t, Config{})
	if v.skew != 30*time.Second {
		t.Fatalf("skew = %v, want the 30s default the cases assume", v.skew)
	}
}
