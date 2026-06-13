package pgerr

import (
	"fmt"
	"net/http"
	"strings"
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
	CodePutPrimaryKey    = "PGRST105" // 405 PUT filters not exactly the PK with eq
	CodePutLimit         = "PGRST114" // 400 limit/offset on a PUT
	CodePutPayloadKey    = "PGRST115" // 400 PUT payload PK differs from the URL filter
	CodeMediaType        = "PGRST107" // 406 Accept negotiation failed
	CodeGucHeaders       = "PGRST111" // 500 invalid response.headers from a function
	CodeGucStatus        = "PGRST112" // 500 invalid response.status from a function
	CodeSingularZeroMany = "PGRST116" // 406 singular requested, zero or many rows
	CodeInvalidPath      = "PGRST125" // 404 invalid path in request URL
	CodeNoRelationship   = "PGRST200" // 400 relationship not found
	CodeAmbiguousEmbed   = "PGRST201" // 300 embedding ambiguous
	CodeNoFunction       = "PGRST202" // 404 no function matches name/args
	CodeAmbiguousFunc    = "PGRST203" // 300 overloaded function call ambiguous
	CodeUnknownColumn    = "PGRST204" // 400 column in write payload not found
	CodeUnknownTable     = "PGRST205" // 404 table or view not found / not exposed
	CodeJWTSecretMissing = "PGRST300" // 500 a token was presented but no jwt-secret is configured
	CodeJWTDecode        = "PGRST301" // 401 JWT could not be decoded (parts/key/alg/signature)
	CodeJWTRequired      = "PGRST302" // 401 no token sent and the anonymous role is disabled
	CodeJWTClaims        = "PGRST303" // 401 JWT claims validation or parsing failed
	CodeAggregatesOff    = "PGRST123" // 400 aggregate functions used while db-aggregates-enabled is off
	CodeUnsupported      = "PGRST127" // 400 feature not implemented on this backend
	CodeInternal         = "PGRSTX00" // 500 internal error (upstream group X has only X00)
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
// or many rows were produced. The text is v14's; render call sites attach the
// row count as details ("The result contains N rows").
func ErrSingularZeroMany() *APIError {
	return New(http.StatusNotAcceptable, CodeSingularZeroMany,
		"Cannot coerce the result to a single JSON object")
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

// ErrUnknownColumn is raised when a column named in a write payload or the
// columns= parameter is not found on the target relation. PostgREST reserves
// PGRST204 for those two; a column referenced by select, a filter, or order
// reaches PostgreSQL instead and surfaces as 42703 (ErrUndefinedColumn).
func ErrUnknownColumn(col string) *APIError {
	return New(http.StatusBadRequest, CodeUnknownColumn,
		fmt.Sprintf("Could not find the '%s' column in the schema cache", col))
}

// CodeUndefinedColumn is PostgreSQL's undefined_column. In PostgREST an unknown
// column in select, a filter, or order is not caught by the schema cache; it
// reaches the server and comes back as this SQLSTATE with a 400.
const CodeUndefinedColumn = "42703"

// ErrUndefinedColumn mirrors PostgreSQL's own message for a reference to a
// column that does not exist; column is the relation-qualified name the query
// used ("todos.nope"). Callers add the server's "Perhaps you meant to reference
// the column ..." suggestion with WithHint when a near-miss exists.
func ErrUndefinedColumn(column string) *APIError {
	return New(http.StatusBadRequest, CodeUndefinedColumn,
		fmt.Sprintf("column %s does not exist", column))
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

// ErrNoFunction is raised when no function matches the name and argument set. It
// names the function schema-qualified with the argument list that was searched
// for ("public.add(a, b)"), or the "without parameters" form when the call
// supplied none, the way PostgREST spells PGRST202. A non-empty hint (the nearest
// registered signature) is attached so the caller sees the closest match.
func ErrNoFunction(schemaName, name string, argNames []string, hint string) *APIError {
	qualified := name
	if schemaName != "" {
		qualified = schemaName + "." + name
	}
	var msg string
	if len(argNames) == 0 {
		msg = fmt.Sprintf("Could not find the function %s without parameters in the schema cache", qualified)
	} else {
		msg = fmt.Sprintf("Could not find the function %s(%s) in the schema cache", qualified, strings.Join(argNames, ", "))
	}
	e := New(http.StatusNotFound, CodeNoFunction, msg)
	if hint != "" {
		e = e.WithHint(hint)
	}
	return e
}

// ErrAmbiguousFunction is raised when more than one overload of a function
// survives argument matching, PostgREST's PGRST203 with a 300. candidates are
// the surviving signatures, schema-qualified with their parameter lists
// ("api.add(a => integer, b => integer)"), spelled into the message the way
// upstream does.
func ErrAmbiguousFunction(candidates []string) *APIError {
	e := New(http.StatusMultipleChoices, CodeAmbiguousFunc,
		"Could not choose the best candidate function between: "+strings.Join(candidates, ", "))
	return e.WithHint("Try renaming the parameters or the function itself in the database so function overloading can be resolved")
}

// ErrPutPrimaryKey is raised when a PUT's URL filters are not exactly the
// relation's primary key columns, each with eq. PostgREST insists a PUT address
// one row by its whole key, so a partial, extra, or non-eq filter is its
// PGRST105 with a 405 (verified live).
func ErrPutPrimaryKey() *APIError {
	return New(http.StatusMethodNotAllowed, CodePutPrimaryKey,
		"Filters must include all and only primary key columns with 'eq' operators")
}

// ErrPutLimit is raised when a PUT carries a limit or offset; PostgREST rejects
// paginating a single-row replace as its PGRST114 with a 400.
func ErrPutLimit() *APIError {
	return New(http.StatusBadRequest, CodePutLimit,
		"limit/offset querystring parameters are not allowed for PUT")
}

// ErrPutPayloadKey is raised when a PUT body's primary key values differ from
// the URL filter values, or the body is not a single object. PostgREST condemns
// the transaction so nothing is written; it is its PGRST115 with a 400.
func ErrPutPayloadKey() *APIError {
	return New(http.StatusBadRequest, CodePutPayloadKey,
		"Payload values do not match URL in primary key column(s)")
}

// ErrInvalidPath is raised for a request path PostgREST has no route for: more
// than one segment after the relation, or extra segments after /rpc/<fn>. It is
// v14's PGRST125, a 404 with this exact message (verified live), distinct from
// the PGRST205 an unknown relation gets.
func ErrInvalidPath() *APIError {
	return New(http.StatusNotFound, CodeInvalidPath,
		"Invalid path specified in request URL")
}

// ErrInvalidResponseHeaders is raised when a function sets response.headers to
// something other than an array of one-key string objects. PostgREST returns
// PGRST111 at 500 rather than forwarding junk headers; the message is
// upstream's.
func ErrInvalidResponseHeaders() *APIError {
	return New(http.StatusInternalServerError, CodeGucHeaders,
		"response.headers guc must be a JSON array composed of objects with a single key and a string value")
}

// ErrInvalidResponseStatus is raised when a function sets response.status to
// anything that is not a valid status code; PostgREST's PGRST112 at 500.
func ErrInvalidResponseStatus() *APIError {
	return New(http.StatusInternalServerError, CodeGucStatus,
		"response.status guc must be a valid status code")
}

// ErrMethodNotAllowed is a 405 PGRST101 with a caller-supplied message. Prefer
// ErrInvalidRPCMethod for the wrong-verb-on-a-function case, which carries
// upstream's exact text.
func ErrMethodNotAllowed(msg string) *APIError {
	if msg == "" {
		msg = "Method not allowed"
	}
	return New(http.StatusMethodNotAllowed, CodeMethodNotAllowed, msg)
}

// ErrInvalidRPCMethod is raised when a function is called with a verb other
// than GET, HEAD, or POST. The text matches v14's PGRST101 ("Cannot use the
// DELETE method on RPC", verified live).
func ErrInvalidRPCMethod(method string) *APIError {
	return New(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
		fmt.Sprintf("Cannot use the %s method on RPC", method))
}

// CodeReadOnlyTransaction is PostgreSQL's read_only_sql_transaction. PostgREST
// runs a GET/HEAD function call in a read-only transaction; a function that
// writes fails with this SQLSTATE, surfaced as a 405 with the server's message.
// dbrest's registry path raises it up front when a GET reaches a function
// declared volatile, since registry backends cannot run the call to find out.
const CodeReadOnlyTransaction = "25006"

// ErrReadOnlyTransaction mirrors PostgreSQL's "cannot execute X in a read-only
// transaction" for a write attempted under a read verb; action names what was
// attempted (a statement kind, or the function for the declared-volatility
// pre-check).
func ErrReadOnlyTransaction(action string) *APIError {
	return New(http.StatusMethodNotAllowed, CodeReadOnlyTransaction,
		fmt.Sprintf("cannot execute %s in a read-only transaction", action))
}

// ErrUnsupported is PGRST127, which v14 defines as "the feature specified in
// the details field is not implemented"; the message is upstream's "Feature not
// implemented" and the details string always names both the feature and the
// backend, per spec 18 section "PGRST127". Emission must happen strictly before
// any backend call.
func ErrUnsupported(feature, backend string) *APIError {
	e := New(http.StatusBadRequest, CodeUnsupported, "Feature not implemented")
	e = e.WithDetails(fmt.Sprintf("%s is not supported by the %s backend", feature, backend))
	return e.WithHint("see the capability matrix for supported features on this backend")
}

// ErrAggregatesDisabled is PGRST123, raised when a request uses an aggregate
// function (count(), col.sum(), ...) while db-aggregates-enabled is off. The
// message and hint are upstream's, pointing the operator at the config flag.
func ErrAggregatesDisabled() *APIError {
	e := New(http.StatusBadRequest, CodeAggregatesOff,
		"Use of aggregate functions is not allowed")
	return e.WithHint("Enable the 'db-aggregates-enabled' config parameter to allow the use of aggregate functions")
}

// ErrFullTextUnavailable is the PGRST127 for a full-text predicate on a column the
// backend has no full-text structure for (a SQLite column with no covering FTS5
// table). It names the column so the missing structure is actionable, per spec
// 21's "never silently wrong" rule: dbrest errors rather than degrading to a
// substring scan. Emission happens before any backend call.
func ErrFullTextUnavailable(column, backend string) *APIError {
	e := New(http.StatusBadRequest, CodeUnsupported, "Feature not implemented")
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

// ErrConstraintViolation surfaces a backend's integrity-constraint error with
// the engine's text carried through verbatim, the way PostgREST forwards
// PostgreSQL's: message names the constraint ("duplicate key value violates
// unique constraint \"todos_pkey\"") and detail carries the key ("Key (id)=(1)
// already exists."). Clients parse both, so pgerr contributes only the status:
// a key that conflicts with an existing row (23505, 23503) is a 409, the rest
// of class 23 is a 400. Drivers whose engine reports structure instead of
// PG-shaped text synthesize the message before calling this; the fixed-message
// constructors below predate it and are being migrated.
func ErrConstraintViolation(sqlstate, message, detail, hint string) *APIError {
	status := http.StatusBadRequest
	if sqlstate == CodeUniqueViolation || sqlstate == CodeForeignKeyViolation {
		status = http.StatusConflict
	}
	e := New(status, sqlstate, message)
	if detail != "" {
		e = e.WithDetails(detail)
	}
	if hint != "" {
		e = e.WithHint(hint)
	}
	return e
}

// ErrUniqueViolation is a duplicate-key conflict (PostgreSQL 23505). It
// rewrites the message to a fixed canonical one, dropping the constraint name
// clients parse; driver call sites are migrating to ErrConstraintViolation.
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

// pgTypeSpelling maps dbrest's canonical type names to the spellings PostgreSQL
// uses in its own error messages, so a 22P02 reads exactly like the server's
// ("invalid input syntax for type integer", never "type int4").
var pgTypeSpelling = map[string]string{
	"int2":   "smallint",
	"int4":   "integer",
	"int8":   "bigint",
	"float4": "real",
	"float8": "double precision",
	"bool":   "boolean",
}

// ErrInvalidInput is raised when a query-string operand or a payload value cannot
// be coerced to its canonical type. It mirrors PostgreSQL's "invalid input syntax
// for type T" message and surfaces the 22P02 SQLSTATE as a 400.
func ErrInvalidInput(canonicalType, input string) *APIError {
	if s, ok := pgTypeSpelling[canonicalType]; ok {
		canonicalType = s
	}
	return New(http.StatusBadRequest, CodeInvalidText,
		fmt.Sprintf("invalid input syntax for type %s: %q", canonicalType, input))
}

// ErrJWTSecretMissing is raised when a request presents a Bearer token but the
// server has no key material to verify it with. It is PostgREST's PGRST300, a
// 500: the misconfiguration is on the server, not the client, and no challenge
// header is sent.
func ErrJWTSecretMissing() *APIError {
	return New(http.StatusInternalServerError, CodeJWTSecretMissing, "Server lacks JWT secret")
}

// ErrJWTDecode is raised when a JWT cannot be decoded: a wrong number of parts,
// no suitable key, a disallowed algorithm, or a failed signature check. It is
// PostgREST's PGRST301 with the RFC 6750 invalid_token challenge.
func ErrJWTDecode(msg string) *APIError {
	e := New(http.StatusUnauthorized, CodeJWTDecode, msg)
	e.WWWAuthenticate = BearerInvalidToken(msg)
	return e
}

// ErrJWTClaims is raised when a decoded JWT fails claims validation or parsing:
// exp/nbf/iat out of range, an audience mismatch, or an unparseable claim set.
// It is PostgREST's PGRST303 with the RFC 6750 invalid_token challenge.
func ErrJWTClaims(msg string) *APIError {
	e := New(http.StatusUnauthorized, CodeJWTClaims, msg)
	e.WWWAuthenticate = BearerInvalidToken(msg)
	return e
}

// ErrJWTRequired is raised when a request presents no token and the anonymous
// role is disabled, so there is no role to run it as. It is PostgREST's PGRST302
// with the bare Bearer challenge.
func ErrJWTRequired() *APIError {
	e := New(http.StatusUnauthorized, CodeJWTRequired, "Anonymous access is disabled")
	e.WWWAuthenticate = "Bearer"
	return e
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
	e := New(status, CodeInsufficientPrivilege,
		fmt.Sprintf("permission denied for table %s", relation))
	if anonymous {
		// PostgREST sends the bare Bearer challenge on every 401, including a
		// privilege denial lifted from 403 for an unauthenticated request.
		e.HTTPStatus = http.StatusUnauthorized
		e.WWWAuthenticate = "Bearer"
	}
	return e
}

// GradePrivilegeStatus applies PostgREST's 42501 rule to e: insufficient
// privilege is 403 when the request was authenticated and 401 when it ran as
// anon, so an authenticated client never gets the 401 that would trigger a
// token-refresh loop. An error with any other code passes through unchanged.
// This is the one place the rule lives; the exec-error mapping and the
// per-driver SQLSTATE tables defer to it.
func GradePrivilegeStatus(e *APIError, authenticated bool) *APIError {
	if e == nil || e.Code != CodeInsufficientPrivilege {
		return e
	}
	c := *e
	if authenticated {
		c.HTTPStatus = http.StatusForbidden
	} else {
		c.HTTPStatus = http.StatusUnauthorized
	}
	return &c
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
