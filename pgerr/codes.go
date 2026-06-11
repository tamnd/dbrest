package pgerr

import (
	"fmt"
	"net/http"
)

// The PGRST code families (spec 18-errors.md, section "The PGRST code families"):
//
//	PGRST1xx  query-string syntax
//	PGRST2xx  schema-cache and resolution
//	PGRST3xx  JWT and auth
//	PGRST127  the one dbrest-specific code: feature unsupported on this backend
//
// Each constructor returns a fully-formed *APIError with the spec-mandated
// status. Callers add details/hint with WithDetails / WithHint.
const (
	CodeParse            = "PGRST100" // 400 query-string parse error
	CodeMethodNotAllowed = "PGRST101" // 405 method not allowed (GET on a volatile fn)
	CodeInvalidBody      = "PGRST102" // 400 invalid request body
	CodeRangeUnsatisfied = "PGRST103" // 416 requested range not satisfiable
	CodeMediaType        = "PGRST107" // 406 Accept negotiation failed
	CodeSingularZeroMany = "PGRST116" // 406 singular requested, zero or many rows
	CodeNoRelationship   = "PGRST200" // 400 relationship not found
	CodeAmbiguousEmbed   = "PGRST201" // 300 embedding ambiguous
	CodeNoFunction       = "PGRST202" // 404 no function matches name/args
	CodeUnknownColumn    = "PGRST204" // 400 column in write payload not found
	CodeUnknownTable     = "PGRST205" // 404 table or view not found / not exposed
	CodeJWTExpired       = "PGRST301" // 401 JWT expired
	CodeJWTInvalid       = "PGRST302" // 401 JWT malformed/bad signature/alg/nbf/aud
	CodeUnsupported      = "PGRST127" // 400 feature not implemented on this backend
	CodeInternal         = "PGRSTXX0" // 500 internal error (XX family rendered as 500)
)

// ErrParse is a query-string syntax error (bad operator, malformed logic tree).
func ErrParse(msg string) *APIError {
	return New(http.StatusBadRequest, CodeParse, msg)
}

// ErrInvalidBody is an invalid request body (PostgREST's PGRST102, HTTP 400):
// an empty or malformed JSON or CSV payload, or a bulk insert whose objects do
// not all share the same key set ("All object keys must match"). An empty msg
// falls back to PostgREST's generic JSON-body message.
func ErrInvalidBody(msg string) *APIError {
	if msg == "" {
		msg = "Empty or invalid json"
	}
	return New(http.StatusBadRequest, CodeInvalidBody, msg)
}

// ErrSingularZeroMany is raised when a singular response was requested but zero
// or many rows were produced.
func ErrSingularZeroMany() *APIError {
	return New(http.StatusNotAcceptable, CodeSingularZeroMany,
		"JSON object requested, multiple (or no) rows returned")
}

// ErrRangeNotSatisfiable is raised when the requested window starts past the end
// of the result set (an out-of-range offset/Range), matching PostgREST's 416.
func ErrRangeNotSatisfiable() *APIError {
	return New(http.StatusRequestedRangeNotSatisfiable, CodeRangeUnsatisfied,
		"Requested range not satisfiable")
}

// ErrNotAcceptable is raised when the Accept header names no media type the
// renderer can produce. It carries the list of types that were offered, matching
// PostgREST's PGRST107 with a 406.
func ErrNotAcceptable(offered string) *APIError {
	return New(http.StatusNotAcceptable, CodeMediaType,
		"None of these media types are available: "+offered)
}

// ErrUnsupportedMediaType is raised when a write or RPC body arrives with a
// Content-Type no parser handles. The published v14 error table still shows a
// stale PGRST107/415 row for this, but live v14 answers 400 PGRST102
// "Content-Type not acceptable: <mime>" (verified against a running PostgREST
// by compat/errors_v14_test.go), so the wire behavior wins: PGRST107 stays
// reserved for failed Accept negotiation (ErrNotAcceptable, 406).
func ErrUnsupportedMediaType(contentType string) *APIError {
	return New(http.StatusBadRequest, CodeInvalidBody,
		fmt.Sprintf("Content-Type not acceptable: %s", contentType))
}

// ErrUnknownTable is raised when a table or view is not in the schema model
// (unknown, or not exposed by db-schemas).
func ErrUnknownTable(name string) *APIError {
	return New(http.StatusNotFound, CodeUnknownTable,
		fmt.Sprintf("Could not find the table '%s' in the schema cache", name))
}

// ErrUnknownColumn is raised when a column named in a payload or select is not
// found on the target relation.
func ErrUnknownColumn(col string) *APIError {
	return New(http.StatusBadRequest, CodeUnknownColumn,
		fmt.Sprintf("Could not find the '%s' column in the schema cache", col))
}

// ErrNoRelationship is raised when an embed names a resource the schema model
// has no relationship to (no foreign key connects them, and none is declared).
// It is PostgREST's PGRST200 with a 400.
func ErrNoRelationship(parent, target string) *APIError {
	return New(http.StatusBadRequest, CodeNoRelationship,
		fmt.Sprintf("Could not find a relationship between '%s' and '%s' in the schema cache", parent, target))
}

// ErrAmbiguousEmbed is raised when more than one relationship connects the
// parent and the embedded resource and no hint disambiguates. It is PostgREST's
// PGRST201 with a 300 Multiple Choices.
func ErrAmbiguousEmbed(parent, target string) *APIError {
	return New(http.StatusMultipleChoices, CodeAmbiguousEmbed,
		fmt.Sprintf("Could not embed because more than one relationship was found for '%s' and '%s'", parent, target))
}

// ErrNoFunction is raised when no function matches the name and argument set.
func ErrNoFunction(name string) *APIError {
	return New(http.StatusNotFound, CodeNoFunction,
		fmt.Sprintf("Could not find the function '%s' in the schema cache", name))
}

// ErrMethodNotAllowed is raised when a read method calls a volatile function: a
// GET to a function with side effects, which PostgREST rejects with 405.
func ErrMethodNotAllowed(msg string) *APIError {
	if msg == "" {
		msg = "Method not allowed"
	}
	return New(http.StatusMethodNotAllowed, CodeMethodNotAllowed, msg)
}

// ErrUnsupported is the dbrest-specific PGRST127. The details string always
// names both the feature and the backend, per spec 18 section "PGRST127".
// Emission must happen strictly before any backend call.
func ErrUnsupported(feature, backend string) *APIError {
	e := New(http.StatusBadRequest, CodeUnsupported, "feature not implemented on this backend")
	e = e.WithDetails(fmt.Sprintf("%s is not supported by the %s backend", feature, backend))
	return e.WithHint("see the capability matrix for supported features on this backend")
}

// ErrFullTextUnavailable is the PGRST127 for a full-text predicate on a column the
// backend has no full-text structure for (a SQLite column with no covering FTS5
// table). It names the column so the missing structure is actionable, per spec
// 21's "never silently wrong" rule: dbrest errors rather than degrading to a
// substring scan. Emission happens before any backend call.
func ErrFullTextUnavailable(column, backend string) *APIError {
	e := New(http.StatusBadRequest, CodeUnsupported, "feature not implemented on this backend")
	e = e.WithDetails(fmt.Sprintf("full-text search on column %q has no full-text index on the %s backend", column, backend))
	return e.WithHint("create a full-text index covering the column")
}

// The class-23 SQLSTATEs are PostgreSQL's integrity-constraint violations. Every
// backend maps its native constraint error to one of these so a client sees the
// same code regardless of engine; PostgREST reports the SQLSTATE as the error
// code. The HTTP status follows PostgREST: 409 for a key that conflicts with an
// existing row, 400 for a payload that violates the column's own rules.
const (
	CodeUniqueViolation     = "23505" // 409 duplicate key
	CodeNotNullViolation    = "23502" // 400 null in a NOT NULL column
	CodeForeignKeyViolation = "23503" // 409 references a missing row
	CodeCheckViolation      = "23514" // 400 fails a CHECK constraint
)

// ErrUniqueViolation is a duplicate-key conflict (PostgreSQL 23505).
func ErrUniqueViolation(detail string) *APIError {
	return New(http.StatusConflict, CodeUniqueViolation,
		"duplicate key value violates unique constraint").WithDetails(detail)
}

// ErrNotNullViolation is a NULL written to a NOT NULL column (23502).
func ErrNotNullViolation(detail string) *APIError {
	return New(http.StatusBadRequest, CodeNotNullViolation,
		"null value violates not-null constraint").WithDetails(detail)
}

// ErrForeignKeyViolation is a reference to a row that does not exist (23503).
func ErrForeignKeyViolation(detail string) *APIError {
	return New(http.StatusConflict, CodeForeignKeyViolation,
		"insert or update violates foreign key constraint").WithDetails(detail)
}

// ErrCheckViolation is a row that fails a CHECK constraint (23514). It is also
// the fallback for any other integrity violation the backend cannot classify.
func ErrCheckViolation(detail string) *APIError {
	return New(http.StatusBadRequest, CodeCheckViolation,
		"new row violates check constraint").WithDetails(detail)
}

// CodeInvalidText is PostgreSQL's invalid_text_representation: an operand or
// payload value that cannot be coerced to the column's type. dbrest raises it in
// the frontend, before the query reaches the engine, so a bad filter value is the
// same 400 on every backend (spec 16).
const CodeInvalidText = "22P02"

// ErrInvalidInput is raised when a query-string operand or a payload value cannot
// be coerced to its canonical type. It mirrors PostgreSQL's "invalid input syntax
// for type T" message and surfaces the 22P02 SQLSTATE as a 400.
func ErrInvalidInput(canonicalType, input string) *APIError {
	return New(http.StatusBadRequest, CodeInvalidText,
		fmt.Sprintf("invalid input syntax for type %s: %q", canonicalType, input))
}

// ErrJWTExpired is raised when a JWT is past its exp (with skew applied).
func ErrJWTExpired() *APIError {
	return New(http.StatusUnauthorized, CodeJWTExpired, "JWT expired")
}

// ErrJWTInvalid is raised for a malformed token, bad signature, disallowed alg,
// or a failed nbf/aud check.
func ErrJWTInvalid(msg string) *APIError {
	if msg == "" {
		msg = "JWT invalid"
	}
	return New(http.StatusUnauthorized, CodeJWTInvalid, msg)
}

// CodeInsufficientPrivilege is PostgreSQL's class-42 SQLSTATE for a denied role
// switch. A cryptographically valid token that names a role the authenticator
// may not assume maps to this, a 403, distinct from the 401 a bad token gets.
const CodeInsufficientPrivilege = "42501"

// ErrRoleNotAllowed is raised when a valid JWT names a role the authenticator is
// not permitted to become. It mirrors PostgreSQL's "permission denied to set
// role" mapped to 403, the same status PostgREST surfaces (spec 13).
func ErrRoleNotAllowed(role string) *APIError {
	return New(http.StatusForbidden, CodeInsufficientPrivilege,
		fmt.Sprintf("permission denied to set role \"%s\"", role))
}

// ErrPermissionDenied is a table or column privilege denial: PostgreSQL's 42501
// surfaced as 403 for an authenticated role, or 401 when the request carried no
// JWT and was denied to anon (spec 14).
func ErrPermissionDenied(relation string, anonymous bool) *APIError {
	status := http.StatusForbidden
	if anonymous {
		status = http.StatusUnauthorized
	}
	return New(status, CodeInsufficientPrivilege,
		fmt.Sprintf("permission denied for table %s", relation))
}

// ErrRLSViolation is a row that fails a WITH CHECK policy on a write, mirroring
// PostgreSQL's "new row violates row-level security policy" mapped to 403. The
// transaction is aborted so nothing is committed (spec 14).
func ErrRLSViolation(relation string) *APIError {
	return New(http.StatusForbidden, CodeInsufficientPrivilege,
		fmt.Sprintf("new row violates row-level security policy for table %q", relation))
}

// ErrInternal renders an unexpected internal failure as a 500. The XX family in
// upstream covers internal errors; dbrest renders them as 500.
func ErrInternal(msg string) *APIError {
	return New(http.StatusInternalServerError, CodeInternal, msg)
}
