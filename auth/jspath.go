package auth

// This file implements the JSPath subset PostgREST accepts for
// jwt-role-claim-key (spec 13): dotted keys (".role"), quoted keys
// (."https://example.com/role"), array indexing (".roles[0]"), and a single
// trailing filter ([?(@ == "admin")]) with the five operators ==, !=, ^==
// (prefix), ==^ (suffix), and *== (contains). The grammar mirrors PostgREST's
// Config.JSPath parser: an invalid value is a startup error, never a silent
// fallback to anon.

import (
	"fmt"
	"strconv"
	"strings"
)

// jsPathExp is one step of a parsed role-claim path.
type jsPathExp struct {
	kind jsPathKind

	key   string // jspKey: the object key to descend into
	idx   int    // jspIdx: the array index to descend into
	op    string // jspFilter: one of == != ^== ==^ *==
	value string // jspFilter: the quoted comparison value
}

type jsPathKind int

const (
	jspKey jsPathKind = iota
	jspIdx
	jspFilter
)

// defaultRoleKey is the path used when jwt-role-claim-key is unset: the
// top-level "role" claim.
var defaultRoleKey = []jsPathExp{{kind: jspKey, key: "role"}}

// parseJSPath parses a jwt-role-claim-key value. An empty value yields the
// default ".role" path; anything else must match the DSL exactly, including
// the leading dot PostgREST requires.
func parseJSPath(s string) ([]jsPathExp, error) {
	if strings.TrimSpace(s) == "" {
		return defaultRoleKey, nil
	}
	p := &jsPathParser{src: s}
	path, err := p.parse()
	if err != nil {
		return nil, fmt.Errorf("failed to parse role-claim-key value (%s): %s", s, err)
	}
	return path, nil
}

// jsPathParser is a single-pass scanner over the role-claim-key text.
type jsPathParser struct {
	src string
	pos int
}

// parse consumes one or more path expressions up to the end of input. A filter
// is only legal as the final expression, as in PostgREST.
func (p *jsPathParser) parse() ([]jsPathExp, error) {
	var path []jsPathExp
	for p.pos < len(p.src) {
		exp, err := p.parseExp()
		if err != nil {
			return nil, err
		}
		path = append(path, *exp)
		if exp.kind == jspFilter && p.pos < len(p.src) {
			return nil, fmt.Errorf("a filter must be the last path element")
		}
	}
	if len(path) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	return path, nil
}

// parseExp reads the next key, index, or filter expression.
func (p *jsPathParser) parseExp() (*jsPathExp, error) {
	switch {
	case p.peek() == '.':
		return p.parseKey()
	case strings.HasPrefix(p.src[p.pos:], "[?("):
		return p.parseFilter()
	case p.peek() == '[':
		return p.parseIdx()
	default:
		return nil, fmt.Errorf("expected '.', '[n]', or '[?(' at position %d", p.pos)
	}
}

// parseKey reads ".name" (alphanumerics plus _$@) or a quoted ."any text" key.
func (p *jsPathParser) parseKey() (*jsPathExp, error) {
	p.pos++ // consume '.'
	if p.peek() == '"' {
		val, err := p.parseQuoted()
		if err != nil {
			return nil, err
		}
		return &jsPathExp{kind: jspKey, key: val}, nil
	}
	start := p.pos
	for p.pos < len(p.src) && isKeyChar(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return nil, fmt.Errorf("expected a key after '.' at position %d", start)
	}
	return &jsPathExp{kind: jspKey, key: p.src[start:p.pos]}, nil
}

// parseIdx reads "[n]" with a non-negative decimal index.
func (p *jsPathParser) parseIdx() (*jsPathExp, error) {
	p.pos++ // consume '['
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == start {
		return nil, fmt.Errorf("expected digits after '[' at position %d", start)
	}
	n, err := strconv.Atoi(p.src[start:p.pos])
	if err != nil {
		return nil, fmt.Errorf("bad array index: %s", err)
	}
	if p.peek() != ']' {
		return nil, fmt.Errorf("expected ']' at position %d", p.pos)
	}
	p.pos++
	return &jsPathExp{kind: jspIdx, idx: n}, nil
}

// parseFilter reads `[?(@ <op> "value")]`. The operators are tried in
// PostgREST's order so "==^" wins over "==".
func (p *jsPathParser) parseFilter() (*jsPathExp, error) {
	p.pos += len("[?(")
	if p.peek() != '@' {
		return nil, fmt.Errorf("expected '@' at position %d", p.pos)
	}
	p.pos++
	p.skipSpaces()
	var op string
	for _, candidate := range []string{"==^", "==", "!=", "^==", "*=="} {
		if strings.HasPrefix(p.src[p.pos:], candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return nil, fmt.Errorf("expected a filter operator at position %d", p.pos)
	}
	p.pos += len(op)
	p.skipSpaces()
	val, err := p.parseQuoted()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.src[p.pos:], ")]") {
		return nil, fmt.Errorf("expected ')]' at position %d", p.pos)
	}
	p.pos += len(")]")
	return &jsPathExp{kind: jspFilter, op: op, value: val}, nil
}

// parseQuoted reads a double-quoted string with no escape processing, matching
// the upstream grammar.
func (p *jsPathParser) parseQuoted() (string, error) {
	if p.peek() != '"' {
		return "", fmt.Errorf("expected '\"' at position %d", p.pos)
	}
	p.pos++
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '"' {
		p.pos++
	}
	if p.pos == len(p.src) {
		return "", fmt.Errorf("unterminated quoted value at position %d", start)
	}
	val := p.src[start:p.pos]
	p.pos++ // consume closing quote
	return val, nil
}

// peek returns the current byte, or 0 at end of input.
func (p *jsPathParser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

// skipSpaces advances over spaces inside a filter condition.
func (p *jsPathParser) skipSpaces() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

// isKeyChar reports whether c may appear in an unquoted key: alphanumerics
// plus the _, $, and @ PostgREST allows.
func isKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '$' || c == '@'
}

// walkJSPath descends the decoded claim set along a parsed path. A key step
// requires an object, an index step an array, and a filter step an array whose
// first matching string element is the result. Any mismatch resolves to no
// value, the same as a missing claim.
func walkJSPath(cur any, path []jsPathExp) (any, bool) {
	for _, e := range path {
		switch e.kind {
		case jspKey:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[e.key]; !ok {
				return nil, false
			}
		case jspIdx:
			ar, ok := cur.([]any)
			if !ok || e.idx >= len(ar) {
				return nil, false
			}
			cur = ar[e.idx]
		case jspFilter:
			ar, ok := cur.([]any)
			if !ok {
				return nil, false
			}
			for _, el := range ar {
				if s, ok := el.(string); ok && matchFilter(e.op, e.value, s) {
					return s, true
				}
			}
			return nil, false
		}
	}
	return cur, true
}

// matchFilter applies one filter operator to a candidate array element.
func matchFilter(op, pattern, candidate string) bool {
	switch op {
	case "==":
		return candidate == pattern
	case "!=":
		return candidate != pattern
	case "^==":
		return strings.HasPrefix(candidate, pattern)
	case "==^":
		return strings.HasSuffix(candidate, pattern)
	case "*==":
		return strings.Contains(candidate, pattern)
	}
	return false
}
