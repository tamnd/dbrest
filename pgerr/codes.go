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
	CodeRangeUnsatisfied = "PGRST103" // 416 requested range not satisfiable
	CodeMediaType        = "PGRST107" // 406/415 media type not negotiable
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
// Content-Type no parser handles. It is PGRST107 with a 415, the request-side
// twin of ErrNotAcceptable.
func ErrUnsupportedMediaType(contentType string) *APIError {
	return New(http.StatusUnsupportedMediaType, CodeMediaType,
		fmt.Sprintf("Content-Type not supported: '%s'", contentType))
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

// ErrUnsupported is the dbrest-specific PGRST127. The details string always
// names both the feature and the backend, per spec 18 section "PGRST127".
// Emission must happen strictly before any backend call.
func ErrUnsupported(feature, backend string) *APIError {
	e := New(http.StatusBadRequest, CodeUnsupported, "feature not implemented on this backend")
	e = e.WithDetails(fmt.Sprintf("%s is not supported by the %s backend", feature, backend))
	return e.WithHint("see the capability matrix for supported features on this backend")
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

// ErrInternal renders an unexpected internal failure as a 500. The XX family in
// upstream covers internal errors; dbrest renders them as 500.
func ErrInternal(msg string) *APIError {
	return New(http.StatusInternalServerError, CodeInternal, msg)
}
