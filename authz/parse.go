package authz

// This file parses the policy-registry configuration value (spec 14, spec 20)
// into the Registry the authorization gate consults. The registry is the
// security boundary on the emulated backends, so parsing fails closed: any
// unknown key, unknown action, or unparseable predicate is a startup error,
// never a silently ignored rule.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// grantDecl is one declared privilege: a role may perform the listed actions
// on a relation, optionally narrowed to a column set (empty means every
// column). It expands to one Grant per action.
type grantDecl struct {
	Role     string   `json:"role"`
	Relation string   `json:"relation"`
	Actions  []string `json:"actions"`
	Columns  []string `json:"columns"`
}

// policyDecl is one declared Row Level Security policy. The predicates use the
// declaration syntax from spec 14: terms of the form `column = rhs` or
// `column != rhs` joined with `and`, where rhs is a claim reference
// (request.jwt.claims.tenant) or a literal ('open', 42, true).
type policyDecl struct {
	Role      string `json:"role"`
	Relation  string `json:"relation"`
	Using     string `json:"using"`
	WithCheck string `json:"with_check"`
}

// registryDecl is the top-level policy-registry document.
type registryDecl struct {
	Grants   []grantDecl  `json:"grants"`
	Policies []policyDecl `json:"policies"`
}

// validActions maps the declared action names onto the privilege verbs.
var validActions = map[string]Action{
	"select": Select,
	"insert": Insert,
	"update": Update,
	"delete": Delete,
}

// ParseRegistry decodes a JSON policy-registry declaration into a Registry.
// The document is an object with two lists:
//
//	grants    [{role, relation, actions: ["select", ...], columns?: [...]}]
//	policies  [{role, relation, using?: "<predicate>", with_check?: "<predicate>"}]
//
// A predicate is one or more `column = rhs` / `column != rhs` terms joined
// with `and`; rhs is a request.jwt.claims reference or a literal. Once a
// registry is configured, the absence of a grant is a denial, so a declaration
// this function cannot fully understand is an error: nothing is skipped.
func ParseRegistry(raw string) (*Registry, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var doc registryDecl
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("policy-registry: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("policy-registry: trailing data after the document")
	}

	grants := make([]Grant, 0, len(doc.Grants))
	for i, g := range doc.Grants {
		if g.Role == "" || g.Relation == "" {
			return nil, fmt.Errorf("policy-registry: grant %d: role and relation are required", i)
		}
		if len(g.Actions) == 0 {
			return nil, fmt.Errorf("policy-registry: grant %d (%s on %s): actions is required", i, g.Role, g.Relation)
		}
		for _, a := range g.Actions {
			action, ok := validActions[strings.ToLower(strings.TrimSpace(a))]
			if !ok {
				return nil, fmt.Errorf("policy-registry: grant %d (%s on %s): unknown action %q", i, g.Role, g.Relation, a)
			}
			grants = append(grants, Grant{
				Role:     g.Role,
				Relation: g.Relation,
				Action:   action,
				Columns:  g.Columns,
			})
		}
	}

	policies := make([]Policy, 0, len(doc.Policies))
	for i, p := range doc.Policies {
		if p.Role == "" || p.Relation == "" {
			return nil, fmt.Errorf("policy-registry: policy %d: role and relation are required", i)
		}
		if p.Using == "" && p.WithCheck == "" {
			return nil, fmt.Errorf("policy-registry: policy %d (%s on %s): at least one of using and with_check is required", i, p.Role, p.Relation)
		}
		using, err := parsePredicate(p.Using)
		if err != nil {
			return nil, fmt.Errorf("policy-registry: policy %d (%s on %s): using: %w", i, p.Role, p.Relation, err)
		}
		check, err := parsePredicate(p.WithCheck)
		if err != nil {
			return nil, fmt.Errorf("policy-registry: policy %d (%s on %s): with_check: %w", i, p.Role, p.Relation, err)
		}
		policies = append(policies, Policy{
			Role:      p.Role,
			Relation:  p.Relation,
			Using:     using,
			WithCheck: check,
		})
	}

	return NewRegistry(grants, policies), nil
}

// claimPrefixes are the accepted spellings of a claim reference. The canonical
// form matches PostgREST's GUC vocabulary (request.jwt.claims.<path>); the
// singular spelling appears in spec 14's examples and is accepted as the same
// thing.
var claimPrefixes = []string{"request.jwt.claims.", "request.jwt.claim."}

// parsePredicate parses the declared predicate syntax into a conjunction of
// terms. An empty declaration is the always-true predicate (a policy may set
// only one of using/with_check).
func parsePredicate(src string) (Predicate, error) {
	if strings.TrimSpace(src) == "" {
		return Predicate{}, nil
	}
	var terms []Term
	for _, part := range splitAnd(src) {
		t, err := parseTerm(part)
		if err != nil {
			return Predicate{}, err
		}
		terms = append(terms, t)
	}
	return Predicate{Terms: terms}, nil
}

// splitAnd splits a predicate on the `and` keyword outside quotes.
func splitAnd(src string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	i := 0
	for i < len(src) {
		c := src[i]
		if c == '\'' {
			inQuote = !inQuote
		}
		if !inQuote && !inWord(src, i) && hasWordAt(src, i, "and") {
			parts = append(parts, cur.String())
			cur.Reset()
			i += len("and")
			continue
		}
		cur.WriteByte(c)
		i++
	}
	parts = append(parts, cur.String())
	return parts
}

// hasWordAt reports whether the keyword appears at position i as a whole word.
func hasWordAt(src string, i int, word string) bool {
	if !strings.HasPrefix(strings.ToLower(src[i:]), word) {
		return false
	}
	end := i + len(word)
	before := i == 0 || src[i-1] == ' ' || src[i-1] == '\t'
	after := end == len(src) || src[end] == ' ' || src[end] == '\t'
	return before && after
}

// inWord reports whether position i continues an identifier started earlier,
// so "band = 1" does not split on its inner "and".
func inWord(src string, i int) bool {
	return i > 0 && isIdentChar(src[i-1])
}

// parseTerm parses one `column <op> rhs` comparison.
func parseTerm(src string) (Term, error) {
	s := strings.TrimSpace(src)
	if s == "" {
		return Term{}, fmt.Errorf("empty term")
	}

	// The operator: != before = so the longer token wins.
	var op Op
	var lhs, rhs string
	if i := strings.Index(s, "!="); i >= 0 {
		op, lhs, rhs = OpNeq, s[:i], s[i+2:]
	} else if i := strings.Index(s, "="); i >= 0 {
		op, lhs, rhs = OpEq, s[:i], s[i+1:]
	} else {
		return Term{}, fmt.Errorf("term %q: expected = or !=", s)
	}

	col := strings.TrimSpace(lhs)
	if col == "" || !isIdent(col) {
		return Term{}, fmt.Errorf("term %q: %q is not a column name", s, col)
	}

	t := Term{Column: col, Op: op}
	val := strings.TrimSpace(rhs)
	switch {
	case val == "":
		return Term{}, fmt.Errorf("term %q: missing right-hand side", s)
	case isClaimRef(val):
		t.Claim = claimPath(val)
		if t.Claim == "" {
			return Term{}, fmt.Errorf("term %q: empty claim path", s)
		}
	case val[0] == '\'':
		if len(val) < 2 || val[len(val)-1] != '\'' {
			return Term{}, fmt.Errorf("term %q: unterminated string literal", s)
		}
		t.Literal = val[1 : len(val)-1]
	case val == "true" || val == "false":
		t.Literal = val == "true"
	default:
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return Term{}, fmt.Errorf("term %q: %q is not a claim reference, string, number, or boolean", s, val)
		}
		t.Literal = json.Number(val)
	}
	return t, nil
}

// isClaimRef reports whether a right-hand side is a request.jwt claim
// reference.
func isClaimRef(val string) bool {
	for _, p := range claimPrefixes {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	return false
}

// claimPath strips the claim-reference prefix, leaving the dotted path into
// the claim set.
func claimPath(val string) string {
	for _, p := range claimPrefixes {
		if strings.HasPrefix(val, p) {
			return val[len(p):]
		}
	}
	return ""
}

// isIdent reports whether s is a plain identifier (a column name).
func isIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isIdentChar(s[i]) {
			return false
		}
	}
	return len(s) > 0
}

// isIdentChar is the identifier alphabet for columns in a predicate.
func isIdentChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
