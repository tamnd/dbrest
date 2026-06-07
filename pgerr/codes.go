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
