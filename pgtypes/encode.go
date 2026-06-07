package pgtypes

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// RenderJSON renders a driver-native value as the canonical JSON bytes its type
// implies, the encode half of a codec. It is what guarantees byte-identical
// bodies across backends (spec 22): a bool from a MySQL TINYINT(1), a SQL Server
// BIT, and a BSON Boolean all render as true/false; a timestamp from datetime2,
// DATETIME, and a BSON Date all render as the same ISO-8601 string; a uuid from
// any physical form renders as the same lowercase hyphenated string. A nil value
// renders as JSON null.
//
// A backend that assembles JSON in the engine (the Body path) bypasses this and
// steers the engine's JSON functions to the same rendering instead; the
// row-scanning backends call RenderJSON per column. See spec 16, "Codecs".
func RenderJSON(typeName string, v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	switch ClassOf(typeName) {
	case ClassBool:
		b, ok := coerceBool(v)
		if !ok {
			return nil, &CoerceError{Canonical: "bool", Input: fmt.Sprint(v)}
		}
		if b {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case ClassInteger:
		return renderInteger(v)
	case ClassFloat, ClassNumeric:
		return renderNumber(typeName, v)
	case ClassUUID:
		s, ok := coerceUUID(v)
		if !ok {
			return nil, &CoerceError{Canonical: "uuid", Input: fmt.Sprint(v)}
		}
		return jsonString(s), nil
	case ClassTemporal:
		return jsonString(renderTemporal(v)), nil
	case ClassBytea:
		return jsonString(renderBytea(v)), nil
	case ClassJSON:
		return renderEmbeddedJSON(v)
	default:
		return jsonString(fmt.Sprint(coerceString(v))), nil
	}
}

// coerceBool reads a boolean from the physical forms an engine returns: a Go
// bool, an integer 0/1 (MySQL/SQL Server), or the single-char/word text forms.
func coerceBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case int64:
		return x != 0, true
	case int:
		return x != 0, true
	case float64:
		return x != 0, true
	case []byte:
		return parseBool(string(x))
	case string:
		return parseBool(x)
	default:
		return false, false
	}
}

func renderInteger(v any) ([]byte, error) {
	switch x := v.(type) {
	case int64:
		return []byte(strconv.FormatInt(x, 10)), nil
	case int:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case int32:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case float64:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case []byte:
		if isDecimal(string(x)) {
			return x, nil
		}
	case string:
		if isDecimal(x) {
			return []byte(x), nil
		}
	}
	return nil, &CoerceError{Canonical: "int4", Input: fmt.Sprint(v)}
}

// renderNumber emits a JSON number. numeric is emitted verbatim from its exact
// text so precision is never routed through a float64; float is formatted with
// the shortest round-tripping representation.
func renderNumber(typeName string, v any) ([]byte, error) {
	switch x := v.(type) {
	case float64:
		return strconv.AppendFloat(nil, x, 'g', -1, 64), nil
	case float32:
		return strconv.AppendFloat(nil, float64(x), 'g', -1, 32), nil
	case int64:
		return []byte(strconv.FormatInt(x, 10)), nil
	case []byte:
		if isDecimal(string(x)) {
			return x, nil
		}
	case string:
		if isDecimal(x) {
			return []byte(x), nil
		}
	}
	canonicalName, _ := Normalize(typeName)
	return nil, &CoerceError{Canonical: canonicalName, Input: fmt.Sprint(v)}
}

// renderTemporal renders a time.Time as RFC 3339 (PostgreSQL's ISO-8601 text
// output); a value already in text form is passed through.
func renderTemporal(v any) string {
	switch x := v.(type) {
	case time.Time:
		return x.Format(time.RFC3339)
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// renderBytea renders a byte string the way PostgreSQL's text output does: a
// \x prefix followed by lowercase hex.
func renderBytea(v any) string {
	switch x := v.(type) {
	case []byte:
		return `\x` + hex.EncodeToString(x)
	case string:
		return `\x` + hex.EncodeToString([]byte(x))
	default:
		return `\x`
	}
}

// renderEmbeddedJSON emits a json/jsonb value as embedded JSON, not a quoted
// string: a []byte or string already holding JSON text is passed through, any
// other Go value is marshaled.
func renderEmbeddedJSON(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		return []byte(x), nil
	case json.RawMessage:
		return x, nil
	default:
		return json.Marshal(x)
	}
}

func coerceUUID(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return normalizeUUID(x)
	case []byte:
		if len(x) == 16 {
			return hyphenate(hex.EncodeToString(x)), true
		}
		return normalizeUUID(string(x))
	default:
		return "", false
	}
}

func hyphenate(h string) string {
	s, _ := normalizeUUID(h)
	return s
}

func coerceString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// jsonString quotes s as a JSON string with the standard escaping.
func jsonString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return b
}
