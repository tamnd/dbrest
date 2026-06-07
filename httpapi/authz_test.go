package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/authz"
	"github.com/tamnd/dbrest/httpapi"
)

// authzServer is an authenticated server with the given grants and policies
// attached. Tokens are minted with the integration secret from auth_test.go.
func authzServer(t *testing.T, grants []authz.Grant, policies []authz.Policy) *httpapi.Server {
	t.Helper()
	srv := authServer(t, auth.Config{})
	srv.SetAuthz(authz.NewRegistry(grants, policies))
	return srv
}

func userToken(t *testing.T, role, sub string) string {
	t.Helper()
	return mintToken(t, jwt.MapClaims{
		"role": role,
		"sub":  sub,
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
}

func TestAuthzNoGrantIsForbidden(t *testing.T) {
	srv := authzServer(t, nil, nil)
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "42501" {
		t.Errorf("code = %v, want 42501", env["code"])
	}
}

func TestAuthzNoGrantAnonIs401(t *testing.T) {
	// No token: the request runs as anon, which has no grant.
	srv := authzServer(t, nil, nil)
	resp := do(t, srv, http.MethodGet, "/films?select=id", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthzGrantedReadSucceeds(t *testing.T) {
	srv := authzServer(t, []authz.Grant{
		{Role: "web_user", Relation: "films", Action: authz.Select},
	}, nil)
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := len(decodeArray(t, resp)); got != 4 {
		t.Errorf("rows = %d, want 4", got)
	}
}

func TestAuthzColumnGrantHidesUngrantedColumn(t *testing.T) {
	srv := authzServer(t, []authz.Grant{
		{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"id", "title"}},
	}, nil)
	// A star projection is narrowed to the granted columns.
	resp := do(t, srv, http.MethodGet, "/films?id=eq.2", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if _, ok := rows[0]["rating"]; ok {
		t.Error("rating is not granted and must not appear")
	}
	if _, ok := rows[0]["title"]; !ok {
		t.Error("title is granted and should appear")
	}
}

func TestAuthzExplicitUngrantedColumnIsForbidden(t *testing.T) {
	srv := authzServer(t, []authz.Grant{
		{Role: "web_user", Relation: "films", Action: authz.Select, Columns: []string{"id", "title"}},
	}, nil)
	resp := do(t, srv, http.MethodGet, "/films?select=id,rating", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuthzRLSFiltersRowsByClaim(t *testing.T) {
	// films has no owner column in the seed; use rating as the policy column so a
	// claim selects a subset. web_user may only see rating = NR rows.
	srv := authzServer(t,
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "rating", Op: authz.OpEq, Claim: "vis"}}},
		}},
	)
	tok := mintToken(t, jwt.MapClaims{
		"role": "web_user",
		"vis":  "NR",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	resp := do(t, srv, http.MethodGet, "/films?select=id,rating", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	// Only the two NR films (id 1 and 4) are visible.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the NR films)", len(rows))
	}
	for _, r := range rows {
		if r["rating"] != "NR" {
			t.Errorf("leaked a row with rating %v", r["rating"])
		}
	}
}

func TestAuthzRLSCannotBeORedAway(t *testing.T) {
	// A client OR filter must not widen past the policy: the policy is AND-ed
	// above the whole client tree.
	srv := authzServer(t,
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "rating", Op: authz.OpEq, Claim: "vis"}}},
		}},
	)
	tok := mintToken(t, jwt.MapClaims{
		"role": "web_user",
		"vis":  "NR",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	// The client tries to reach an R-rated row by OR-ing its own id filter.
	resp := do(t, srv, http.MethodGet, "/films?select=id,rating&or=(id.eq.2,id.eq.1)", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	// id=2 is rating R and must stay hidden; only id=1 (NR) comes through.
	if len(rows) != 1 || rows[0]["id"] != float64(1) {
		t.Fatalf("policy was bypassed: got %v", rows)
	}
}

func TestAuthzMissingClaimHidesAllRows(t *testing.T) {
	srv := authzServer(t,
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Select}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			Using: authz.Predicate{Terms: []authz.Term{{Column: "rating", Op: authz.OpEq, Claim: "vis"}}},
		}},
	)
	// Token without the vis claim: the policy denies every row, empty 200.
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := len(decodeArray(t, resp)); got != 0 {
		t.Errorf("rows = %d, want 0 (claim absent)", got)
	}
}

func TestAuthzWriteNeedsInsertGrant(t *testing.T) {
	srv := authzServer(t, []authz.Grant{
		{Role: "web_user", Relation: "films", Action: authz.Select},
	}, nil)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":9,"title":"X"}`, map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no insert grant)", resp.StatusCode)
	}
}

func TestAuthzWithCheckRejectsWrite(t *testing.T) {
	srv := authzServer(t,
		[]authz.Grant{{Role: "web_user", Relation: "films", Action: authz.Insert}},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "rating", Op: authz.OpEq, Claim: "vis"}}},
		}},
	)
	tok := mintToken(t, jwt.MapClaims{
		"role": "web_user",
		"vis":  "NR",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	// The row sets rating=R, but the policy only permits NR.
	resp := send(t, srv, http.MethodPost, "/films", `{"id":9,"title":"X","rating":"R"}`, map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "42501" {
		t.Errorf("code = %v, want 42501", env["code"])
	}
}

func TestAuthzWithCheckAllowsConformingWrite(t *testing.T) {
	srv := authzServer(t,
		[]authz.Grant{
			{Role: "web_user", Relation: "films", Action: authz.Insert},
			{Role: "web_user", Relation: "films", Action: authz.Select},
		},
		[]authz.Policy{{
			Relation: "films", Role: "web_user",
			WithCheck: authz.Predicate{Terms: []authz.Term{{Column: "rating", Op: authz.OpEq, Claim: "vis"}}},
		}},
	)
	tok := mintToken(t, jwt.MapClaims{
		"role": "web_user",
		"vis":  "NR",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	resp := send(t, srv, http.MethodPost, "/films", `{"id":9,"title":"X","rating":"NR"}`, map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

func TestAuthzDisabledWhenNoRegistry(t *testing.T) {
	// With a verifier but no registry, every authenticated request is allowed:
	// the engine (or, here, nothing) governs authorization.
	srv := authServer(t, auth.Config{})
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Authorization": "Bearer " + userToken(t, "web_user", "alice"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
