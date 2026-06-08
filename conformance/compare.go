package conformance

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// contractualHeaders are the response headers that carry the PostgREST contract
// and are compared exactly when the golden side sets them. Every other header is
// transport (Date, Server) or recomputed (Content-Length) and is not compared
// (spec 22 section 2).
var contractualHeaders = []string{
	"Content-Range", "Content-Location", "Location", "Preference-Applied",
	"Content-Type", "Allow", "WWW-Authenticate", "Proxy-Status",
}

// maskSentinel replaces a volatile value at a masked JSON pointer in both the
// golden and the subject body, so a timestamp or generated id does not cause a
// spurious mismatch while still asserting the field is present.
const maskSentinel = "<masked>"

// CompareOptions tunes a comparison. Ordered keeps array element order
// significant (set when the request pins order); without it arrays compare as
// multisets. Mask lists JSON pointers blanked before the body compare.
// FloatTolerance is the absolute tolerance for comparing two numbers, since
// engines render floats differently.
type CompareOptions struct {
	Ordered        bool
	Mask           []string
	FloatTolerance float64
}

// Compare normalizes the subject response against the golden response and
// returns the list of differences; an empty list means they are equivalent. It
// compares the status exactly, the contractual headers exactly, and the body
// structurally as JSON (falling back to a trimmed-string compare when a body is
// not JSON).
func Compare(golden, subject Response, opts CompareOptions) []Diff {
	var diffs []Diff

	if golden.Status != subject.Status {
		diffs = append(diffs, Diff{"status", strconv.Itoa(golden.Status), strconv.Itoa(subject.Status)})
	}

	diffs = append(diffs, compareHeaders(golden.Headers, subject.Headers)...)
	diffs = append(diffs, compareBody(golden.Body, subject.Body, opts)...)
	return diffs
}

// Diff is one normalized difference between a golden and a subject response.
type Diff struct {
	Field  string
	Golden string
	Sub    string
}

func (d Diff) String() string {
	return fmt.Sprintf("%s: golden=%q subject=%q", d.Field, d.Golden, d.Sub)
}

// compareHeaders compares only the contractual headers, case-insensitively on
// the header name. A contractual header set on the golden side must be present
// and equal on the subject; the subject's extra headers are ignored.
func compareHeaders(golden, subject map[string]string) []Diff {
	g := lowerKeys(golden)
	s := lowerKeys(subject)
	var diffs []Diff
	for _, h := range contractualHeaders {
		key := strings.ToLower(h)
		gv, ok := g[key]
		if !ok {
			continue
		}
		if sv := s[key]; sv != gv {
			diffs = append(diffs, Diff{"header " + h, gv, sv})
		}
	}
	return diffs
}

func lowerKeys(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

// compareBody compares two response bodies. When both parse as JSON they are
// compared structurally (object keys unordered, arrays ordered or as a multiset
// per opts, numbers within tolerance) after masking; otherwise they are
// compared as trimmed strings.
func compareBody(golden, subject string, opts CompareOptions) []Diff {
	gv, gok := decodeJSON(golden)
	sv, sok := decodeJSON(subject)
	if !gok || !sok {
		if strings.TrimSpace(golden) != strings.TrimSpace(subject) {
			return []Diff{{"body", golden, subject}}
		}
		return nil
	}
	for _, ptr := range opts.Mask {
		applyMask(&gv, ptr)
		applyMask(&sv, ptr)
	}
	if !equalJSON(gv, sv, opts.Ordered, opts.FloatTolerance) {
		return []Diff{{"body", canonical(gv), canonical(sv)}}
	}
	return nil
}

func decodeJSON(s string) (any, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, false
	}
	return v, true
}

// canonical re-encodes a decoded JSON value deterministically (Go sorts object
// keys), used to render a body diff readably.
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// equalJSON reports deep equivalence of two decoded JSON values. Objects compare
// key by key regardless of order; arrays compare in order when ordered, else as
// multisets; numbers compare within tol; everything else compares by value.
func equalJSON(a, b any, ordered bool, tol float64) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, x := range av {
			y, ok := bv[k]
			if !ok || !equalJSON(x, y, ordered, tol) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		if ordered {
			for i := range av {
				if !equalJSON(av[i], bv[i], ordered, tol) {
					return false
				}
			}
			return true
		}
		return multisetEqual(av, bv, tol)
	case float64:
		bv, ok := b.(float64)
		return ok && math.Abs(av-bv) <= tol
	default:
		return a == b
	}
}

// multisetEqual reports whether two arrays hold the same elements regardless of
// order, used for an unordered result set. It greedily matches each element of a
// to an unused element of b.
func multisetEqual(a, b []any, tol float64) bool {
	used := make([]bool, len(b))
	for _, x := range a {
		found := false
		for j, y := range b {
			if used[j] {
				continue
			}
			if equalJSON(x, y, false, tol) {
				used[j] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// applyMask blanks the value at a JSON pointer (RFC 6901, e.g. /0/created_at)
// inside a decoded JSON value, replacing it with the sentinel. A pointer that
// does not resolve is ignored, so a mask is safe across responses of different
// shapes.
func applyMask(root *any, pointer string) {
	tokens := parsePointer(pointer)
	if len(tokens) == 0 {
		return
	}
	maskAt(*root, tokens)
}

func maskAt(node any, tokens []string) {
	last := len(tokens) - 1
	for i, tok := range tokens {
		switch n := node.(type) {
		case map[string]any:
			if i == last {
				if _, ok := n[tok]; ok {
					n[tok] = maskSentinel
				}
				return
			}
			node = n[tok]
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(n) {
				return
			}
			if i == last {
				n[idx] = maskSentinel
				return
			}
			node = n[idx]
		default:
			return
		}
	}
}

// parsePointer splits a JSON pointer into its decoded reference tokens.
func parsePointer(pointer string) []string {
	if pointer == "" || pointer == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		parts[i] = strings.ReplaceAll(p, "~0", "~")
	}
	return parts
}
