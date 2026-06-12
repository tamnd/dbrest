package authz

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRegistryGrantsAndPolicies(t *testing.T) {
	reg, err := ParseRegistry(`{
		"grants": [
			{"role": "web_user", "relation": "films", "actions": ["select", "insert"], "columns": ["id", "title"]},
			{"role": "web_anon", "relation": "films", "actions": ["select"]}
		],
		"policies": [
			{"role": "web_user", "relation": "films",
			 "using": "owner = request.jwt.claims.sub",
			 "with_check": "owner = request.jwt.claims.sub"}
		]
	}`)
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}

	sel, ok := reg.grants[grantKey{"web_user", "films", Select}]
	if !ok || sel.all || !sel.cols["id"] || !sel.cols["title"] || sel.cols["secret"] {
		t.Errorf("web_user select grant = %+v, want columns id,title", sel)
	}
	ins, ok := reg.grants[grantKey{"web_user", "films", Insert}]
	if !ok || ins.all {
		t.Errorf("web_user insert grant = %+v, want the same column set", ins)
	}
	anon, ok := reg.grants[grantKey{"web_anon", "films", Select}]
	if !ok || !anon.all {
		t.Errorf("web_anon select grant = %+v, want all columns", anon)
	}
	if _, ok := reg.grants[grantKey{"web_user", "films", Delete}]; ok {
		t.Error("an undeclared action must not be granted")
	}

	pol, ok := reg.policies[polKey{"web_user", "films"}]
	if !ok {
		t.Fatal("policy not registered")
	}
	wantTerm := Term{Column: "owner", Op: OpEq, Claim: "sub"}
	if len(pol.Using.Terms) != 1 || pol.Using.Terms[0] != wantTerm {
		t.Errorf("using = %+v, want [%+v]", pol.Using.Terms, wantTerm)
	}
	if len(pol.WithCheck.Terms) != 1 || pol.WithCheck.Terms[0] != wantTerm {
		t.Errorf("with_check = %+v, want [%+v]", pol.WithCheck.Terms, wantTerm)
	}
}

func TestParseRegistryEmptyDocumentDeniesAll(t *testing.T) {
	// An explicitly empty registry is a deliberate deny-all: it parses, and
	// the gate then refuses every request for lack of a grant.
	reg, err := ParseRegistry(`{}`)
	if err != nil {
		t.Fatalf("ParseRegistry: %v", err)
	}
	if len(reg.grants) != 0 || len(reg.policies) != 0 {
		t.Errorf("empty document = %d grants, %d policies, want none", len(reg.grants), len(reg.policies))
	}
}

func TestParsePredicateForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []Term
	}{
		{"claim canonical", "tenant_id = request.jwt.claims.tenant",
			[]Term{{Column: "tenant_id", Op: OpEq, Claim: "tenant"}}},
		{"claim singular spelling", "tenant_id = request.jwt.claim.tenant",
			[]Term{{Column: "tenant_id", Op: OpEq, Claim: "tenant"}}},
		{"nested claim path", "org = request.jwt.claims.app_metadata.org",
			[]Term{{Column: "org", Op: OpEq, Claim: "app_metadata.org"}}},
		{"string literal", "status = 'open'",
			[]Term{{Column: "status", Op: OpEq, Literal: "open"}}},
		{"number literal", "tier = 2",
			[]Term{{Column: "tier", Op: OpEq, Literal: json.Number("2")}}},
		{"bool literal", "archived = false",
			[]Term{{Column: "archived", Op: OpEq, Literal: false}}},
		{"inequality", "status != 'deleted'",
			[]Term{{Column: "status", Op: OpNeq, Literal: "deleted"}}},
		{"conjunction", "tenant_id = request.jwt.claims.tenant and status = 'open'",
			[]Term{
				{Column: "tenant_id", Op: OpEq, Claim: "tenant"},
				{Column: "status", Op: OpEq, Literal: "open"},
			}},
		{"identifier containing and", "band = 'rush'",
			[]Term{{Column: "band", Op: OpEq, Literal: "rush"}}},
		{"and inside a string literal", "title = 'salt and pepper'",
			[]Term{{Column: "title", Op: OpEq, Literal: "salt and pepper"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parsePredicate(c.src)
			if err != nil {
				t.Fatalf("parsePredicate(%q): %v", c.src, err)
			}
			if len(p.Terms) != len(c.want) {
				t.Fatalf("terms = %+v, want %+v", p.Terms, c.want)
			}
			for i := range c.want {
				if p.Terms[i] != c.want[i] {
					t.Errorf("term %d = %+v, want %+v", i, p.Terms[i], c.want[i])
				}
			}
		})
	}
}

func TestParseRegistryFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // a fragment the error must carry
	}{
		{"malformed json", `{`, "policy-registry"},
		{"unknown top-level key", `{"grant": []}`, "unknown field"},
		{"trailing data", `{} {}`, "trailing data"},
		{"grant missing role", `{"grants": [{"relation": "films", "actions": ["select"]}]}`, "role and relation"},
		{"grant missing actions", `{"grants": [{"role": "r", "relation": "films"}]}`, "actions is required"},
		{"unknown action", `{"grants": [{"role": "r", "relation": "films", "actions": ["grant"]}]}`, `unknown action "grant"`},
		{"policy missing role", `{"policies": [{"relation": "films", "using": "a = 1"}]}`, "role and relation"},
		{"policy with no predicate", `{"policies": [{"role": "r", "relation": "films"}]}`, "at least one of"},
		{"predicate without operator", `{"policies": [{"role": "r", "relation": "films", "using": "owner"}]}`, "expected = or !="},
		{"predicate missing rhs", `{"policies": [{"role": "r", "relation": "films", "using": "owner ="}]}`, "missing right-hand side"},
		{"unterminated string", `{"policies": [{"role": "r", "relation": "films", "using": "owner = 'x"}]}`, "unterminated string"},
		{"bare word rhs", `{"policies": [{"role": "r", "relation": "films", "using": "owner = sub"}]}`, "not a claim reference"},
		{"bad column name", `{"policies": [{"role": "r", "relation": "films", "using": "owner; drop = 'x'"}]}`, "not a column name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseRegistry(c.src)
			if err == nil {
				t.Fatalf("ParseRegistry(%q) parsed, want an error", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}
