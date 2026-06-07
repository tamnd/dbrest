package sqlite

import "strings"

// canonicalType maps a SQLite declared column type to dbrest's canonical PG type
// name (spec 16). SQLite uses type affinity, not strict types, so the declared
// type is matched by the affinity rules from the SQLite datatype documentation:
// the substring tests below mirror the affinity algorithm. A column with no
// declared type has BLOB affinity and is reported as text.
func canonicalType(declared string) string {
	d := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case d == "":
		return "text"
	case strings.Contains(d, "INT"):
		return "integer"
	case strings.Contains(d, "CHAR"), strings.Contains(d, "CLOB"), strings.Contains(d, "TEXT"):
		return "text"
	case strings.Contains(d, "BLOB"):
		return "bytea"
	case strings.Contains(d, "REAL"), strings.Contains(d, "FLOA"), strings.Contains(d, "DOUB"):
		return "double precision"
	case strings.Contains(d, "BOOL"):
		return "boolean"
	case strings.Contains(d, "DATE"), strings.Contains(d, "TIME"):
		return "timestamp"
	case strings.Contains(d, "JSON"):
		return "json"
	default:
		return "numeric"
	}
}
