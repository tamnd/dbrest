// Package pgtypes owns the canonical type surface dbrest presents at the API
// boundary and the codecs that move values across the wire/engine edge.
//
// A PostgREST client always sees PostgreSQL types: it writes casts like
// col::int4, filters with typed operands, and reads back ISO-8601 timestamps and
// JSON booleans, regardless of which engine actually stored the row. This package
// defines the single canonical type set (the PostgreSQL type names), folds the
// aliases a client may write onto it, classifies each canonical type, parses a
// query-string or JSON operand into a typed value (the decode direction), and
// renders a driver-native value into the canonical JSON form (the encode
// direction). The frontend only ever speaks canonical names; a backend translates
// at the edges. See spec 16-types-and-casts.
package pgtypes

import "strings"

// Class groups the canonical types by how they parse and render. The frontend
// branches on the class, not the individual type name, so int2/int4/int8 share
// one parsing rule and float4/float8 another.
type Class uint8

const (
	// ClassText is the string family: text, varchar, char, bpchar, name.
	ClassText Class = iota
	// ClassInteger is the integer family: int2, int4, int8.
	ClassInteger
	// ClassFloat is the binary floating-point family: float4, float8.
	ClassFloat
	// ClassNumeric is arbitrary-precision numeric/decimal.
	ClassNumeric
	// ClassBool is boolean.
	ClassBool
	// ClassTemporal is the date/time family: timestamp, timestamptz, date, time.
	ClassTemporal
	// ClassUUID is uuid.
	ClassUUID
	// ClassJSON is json and jsonb.
	ClassJSON
	// ClassBytea is the binary string bytea.
	ClassBytea
	// ClassOther covers a canonical name with no dedicated codec yet (arrays,
	// ranges, enums introspected as text fall back here): it is carried through
	// without frontend coercion and left to the engine.
	ClassOther
)

// canon records a canonical type's class and the integer width when it has one,
// so the integer parser can range-check int2/int4/int8 without a second table.
type canon struct {
	class Class
	bits  int // 16/32/64 for the integer family, else 0
}

// canonical maps every canonical name to its descriptor. Aliases are folded onto
// these names by Normalize before lookup, so this table holds the canonical
// spellings only.
var canonical = map[string]canon{
	"text":        {class: ClassText},
	"varchar":     {class: ClassText},
	"int2":        {class: ClassInteger, bits: 16},
	"int4":        {class: ClassInteger, bits: 32},
	"int8":        {class: ClassInteger, bits: 64},
	"float4":      {class: ClassFloat},
	"float8":      {class: ClassFloat},
	"numeric":     {class: ClassNumeric},
	"bool":        {class: ClassBool},
	"timestamp":   {class: ClassTemporal},
	"timestamptz": {class: ClassTemporal},
	"date":        {class: ClassTemporal},
	"time":        {class: ClassTemporal},
	"uuid":        {class: ClassUUID},
	"json":        {class: ClassJSON},
	"jsonb":       {class: ClassJSON},
	"bytea":       {class: ClassBytea},
}

// aliases folds the names a client or an introspector may write onto the
// canonical spelling. The PostgreSQL long forms (integer, boolean, double
// precision) and a few engine-native spellings all resolve here, so the rest of
// the package only ever sees the canonical name. See spec 16, "The canonical type
// surface".
var aliases = map[string]string{
	"smallint":                    "int2",
	"int":                         "int4",
	"integer":                     "int4",
	"bigint":                      "int8",
	"serial":                      "int4",
	"bigserial":                   "int8",
	"real":                        "float4",
	"double precision":            "float8",
	"double":                      "float8",
	"float":                       "float8",
	"decimal":                     "numeric",
	"boolean":                     "bool",
	"character varying":           "varchar",
	"char":                        "text",
	"bpchar":                      "text",
	"character":                   "text",
	"name":                        "text",
	"string":                      "text",
	"timestamptz with time zone":  "timestamptz",
	"timestamp with time zone":    "timestamptz",
	"timestamp without time zone": "timestamp",
	"datetime":                    "timestamp",
}

// Normalize folds a type name the client or an introspector wrote onto its
// canonical spelling and reports whether it is a known canonical type. The input
// is matched case-insensitively and trimmed; an unknown name comes back unchanged
// with ok=false so a caller can decide whether to reject or carry it through.
func Normalize(name string) (canonical string, ok bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if a, found := aliases[n]; found {
		n = a
	}
	if _, found := lookup(n); found {
		return n, true
	}
	return n, false
}

// ClassOf returns the class of a canonical or aliased type name. An unknown name
// is ClassOther, the carry-through class, so an unrecognized type never trips the
// frontend coercion path.
func ClassOf(name string) Class {
	c, ok := Normalize(name)
	if !ok {
		return ClassOther
	}
	info, _ := lookup(c)
	return info.class
}

// Known reports whether name resolves to a canonical type.
func Known(name string) bool {
	_, ok := Normalize(name)
	return ok
}

func lookup(canonicalName string) (canon, bool) {
	c, ok := canonical[canonicalName]
	return c, ok
}
