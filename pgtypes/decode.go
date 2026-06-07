package pgtypes

import (
	"fmt"
	"strconv"
	"strings"
)

// CoerceError reports that a textual operand could not be coerced to a canonical
// type. It carries both the canonical name and the offending input so the
// frontend can build the PostgREST envelope (an invalid_text_representation,
// SQLSTATE 22P02, as a 400) without re-deriving them. Its message mirrors
// PostgreSQL's own "invalid input syntax for type T: ..." text. See spec 16,
// "Casts in filters and the value-parsing direction".
type CoerceError struct {
	Canonical string
	Input     string
}

func (e *CoerceError) Error() string {
	return fmt.Sprintf("invalid input syntax for type %s: %q", e.Canonical, e.Input)
}

// ParseScalar coerces a query-string operand (always text) to the Go value the
// canonical type implies, validating it in the process. It is the decode half of
// a codec: an integer column turns "18" into an int64, a bool column turns
// t/f/true/false/1/0 into a bool, a uuid column validates and lowercases the
// hyphenated form. A value that cannot be coerced is a *CoerceError, which the
// frontend maps to a 22P02 400 before the query reaches the engine, so the
// behavior is identical on every backend.
//
// The string-shaped classes (text, temporal, json, bytea) and any name with no
// dedicated codec are carried through unchanged: their operand is bound as text
// and the engine does the final coercion, which keeps dbrest from rejecting a
// timestamp spelling the engine would have accepted.
func ParseScalar(typeName, text string) (any, error) {
	canonicalName, ok := Normalize(typeName)
	if !ok {
		return text, nil
	}
	info, _ := lookup(canonicalName)
	switch info.class {
	case ClassInteger:
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, info.bits)
		if err != nil {
			return nil, &CoerceError{Canonical: canonicalName, Input: text}
		}
		return n, nil
	case ClassFloat:
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, &CoerceError{Canonical: canonicalName, Input: text}
		}
		return f, nil
	case ClassNumeric:
		if !isDecimal(strings.TrimSpace(text)) {
			return nil, &CoerceError{Canonical: canonicalName, Input: text}
		}
		// numeric is carried as text so its precision is never routed through a
		// float64; the engine binds the exact decimal.
		return text, nil
	case ClassBool:
		b, ok := parseBool(text)
		if !ok {
			return nil, &CoerceError{Canonical: canonicalName, Input: text}
		}
		return b, nil
	case ClassUUID:
		u, ok := normalizeUUID(text)
		if !ok {
			return nil, &CoerceError{Canonical: canonicalName, Input: text}
		}
		return u, nil
	default:
		return text, nil
	}
}

// parseBool accepts the spellings PostgreSQL's boolean input does, folding case.
func parseBool(text string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "t", "true", "y", "yes", "on", "1":
		return true, true
	case "f", "false", "n", "no", "off", "0":
		return false, true
	default:
		return false, false
	}
}

// isDecimal reports whether text is a valid numeric literal: an optional sign, a
// digit run with an optional fractional part, an optional exponent, or one of the
// special values PostgreSQL's numeric accepts (NaN, Infinity).
func isDecimal(text string) bool {
	if text == "" {
		return false
	}
	switch strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(text, "+"), "-")) {
	case "nan", "inf", "infinity":
		return true
	}
	s := text
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	mantissa := s
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		exp := s[i+1:]
		if exp != "" && (exp[0] == '+' || exp[0] == '-') {
			exp = exp[1:]
		}
		if exp == "" || !allDigits(exp) {
			return false
		}
	}
	intPart, fracPart, hasDot := strings.Cut(mantissa, ".")
	if hasDot {
		// A dot needs a digit on at least one side: "1.", ".5", "1.5" are fine.
		if intPart == "" && fracPart == "" {
			return false
		}
		return (intPart == "" || allDigits(intPart)) && (fracPart == "" || allDigits(fracPart))
	}
	return allDigits(intPart)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// normalizeUUID validates a uuid operand and returns it as the canonical
// lowercase hyphenated form. It accepts the hyphenated form and the bare 32-hex
// form, optionally wrapped in braces, matching the input shapes PostgreSQL takes.
func normalizeUUID(text string) (string, bool) {
	s := strings.TrimSpace(text)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return "", false
	}
	var b strings.Builder
	b.Grow(36)
	for i := 0; i < 32; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			c += 'a' - 'A'
		default:
			return "", false
		}
		if i == 8 || i == 12 || i == 16 || i == 20 {
			b.WriteByte('-')
		}
		b.WriteByte(c)
	}
	return b.String(), true
}
