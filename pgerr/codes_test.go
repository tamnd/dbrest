package pgerr

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Every constructor is pinned to its spec-mandated status and code. This is the
// table PostgREST clients depend on: a given failure must surface the same code
// and HTTP status on every backend, so the mapping is asserted exhaustively here
// rather than incidentally through the one or two paths that happen to be hit by
// higher-level tests.
func TestConstructorStatusAndCode(t *testing.T) {
	cases := []struct {
		name   string
		err    *APIError
		status int
		code   string
	}{
		{"parse", ErrParse("bad operator"), http.StatusBadRequest, CodeParse},
		{"invalid-body", ErrInvalidBody(""), http.StatusBadRequest, CodeInvalidBody},
		{"singular", ErrSingularZeroMany(), http.StatusNotAcceptable, CodeSingularZeroMany},
		{"range", ErrRangeNotSatisfiable(), http.StatusRequestedRangeNotSatisfiable, CodeRangeUnsatisfied},
		{"not-acceptable", ErrNotAcceptable("text/csv"), http.StatusNotAcceptable, CodeMediaType},
		{"unsupported-media", ErrUnsupportedMediaType("text/yaml"), http.StatusBadRequest, CodeInvalidBody},
		{"unknown-table", ErrUnknownTable("public", "films"), http.StatusNotFound, CodeUnknownTable},
		{"unknown-column", ErrUnknownColumn("titel", "films"), http.StatusBadRequest, CodeUnknownColumn},
		{"undefined-column", ErrUndefinedColumn("todos.nope"), http.StatusBadRequest, CodeUndefinedColumn},
		{"no-relationship", ErrNoRelationship("films", "actors", "public", ""), http.StatusBadRequest, CodeNoRelationship},
		{"ambiguous-embed", ErrAmbiguousEmbed("films", "actors", nil), http.StatusMultipleChoices, CodeAmbiguousEmbed},
		{"no-function", ErrNoFunction("public", "add", []string{"a", "b"}, ""), http.StatusNotFound, CodeNoFunction},
		{"ambiguous-function", ErrAmbiguousFunction([]string{"api.add(a => integer)", "api.add(a => text)"}), http.StatusMultipleChoices, CodeAmbiguousFunc},
		{"invalid-path", ErrInvalidPath(), http.StatusNotFound, CodeInvalidPath},
		{"guc-headers", ErrInvalidResponseHeaders(), http.StatusInternalServerError, CodeGucHeaders},
		{"guc-status", ErrInvalidResponseStatus(), http.StatusInternalServerError, CodeGucStatus},
		{"method-not-allowed", ErrMethodNotAllowed(""), http.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{"invalid-rpc-method", ErrInvalidRPCMethod("DELETE"), http.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{"read-only-txn", ErrReadOnlyTransaction("UPDATE"), http.StatusMethodNotAllowed, CodeReadOnlyTransaction},
		{"unsupported", ErrUnsupported("the sl operator", "mysql"), http.StatusBadRequest, CodeUnsupported},
		{"fts-unavailable", ErrFullTextUnavailable("body", "sqlite"), http.StatusBadRequest, CodeUnsupported},
		{"constraint-unique", ErrConstraintViolation("23505", "m", "", ""), http.StatusConflict, CodeUniqueViolation},
		{"constraint-fk", ErrConstraintViolation("23503", "m", "", ""), http.StatusConflict, CodeForeignKeyViolation},
		{"constraint-not-null", ErrConstraintViolation("23502", "m", "", ""), http.StatusBadRequest, CodeNotNullViolation},
		{"constraint-check", ErrConstraintViolation("23514", "m", "", ""), http.StatusBadRequest, CodeCheckViolation},
		{"invalid-input", ErrInvalidInput("integer", "abc"), http.StatusBadRequest, CodeInvalidText},
		{"jwt-secret-missing", ErrJWTSecretMissing(), http.StatusInternalServerError, CodeJWTSecretMissing},
		{"jwt-decode", ErrJWTDecode("JWT couldn't be decoded"), http.StatusUnauthorized, CodeJWTDecode},
		{"jwt-required", ErrJWTRequired(), http.StatusUnauthorized, CodeJWTRequired},
		{"jwt-claims", ErrJWTClaims("JWT expired"), http.StatusUnauthorized, CodeJWTClaims},
		{"role-not-allowed", ErrRoleNotAllowed("admin"), http.StatusForbidden, CodeInsufficientPrivilege},
		{"permission-denied", ErrPermissionDenied("films", false), http.StatusForbidden, CodeInsufficientPrivilege},
		{"rls-violation", ErrRLSViolation("films"), http.StatusForbidden, CodeInsufficientPrivilege},
		{"internal", ErrInternal("boom"), http.StatusInternalServerError, CodeInternal},
	}
	// The internal code is pinned to its literal: clients and monitors match the
	// documented PGRSTX00, so a private spelling would never match anything.
	if CodeInternal != "PGRSTX00" {
		t.Errorf("CodeInternal = %q, want PGRSTX00", CodeInternal)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err.HTTPStatus != c.status {
				t.Errorf("status = %d, want %d", c.err.HTTPStatus, c.status)
			}
			if c.err.Code != c.code {
				t.Errorf("code = %q, want %q", c.err.Code, c.code)
			}
			if strings.TrimSpace(c.err.Message) == "" {
				t.Error("message is empty")
			}
			// Every envelope must still serialize with the four mandatory keys.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(c.err.JSON(), &m); err != nil {
				t.Fatalf("envelope not valid json: %v", err)
			}
			for _, k := range []string{"code", "message", "details", "hint"} {
				if _, ok := m[k]; !ok {
					t.Errorf("missing key %q in %s", k, c.err.JSON())
				}
			}
		})
	}
}

// A denied request that carried no JWT is a 401 against anon, not the 403 an
// authenticated role gets. PostgREST draws that line and clients branch on it.
func TestPermissionDeniedAnonymousIs401(t *testing.T) {
	if got := ErrPermissionDenied("films", true).HTTPStatus; got != http.StatusUnauthorized {
		t.Errorf("anonymous denial status = %d, want 401", got)
	}
}

// GradePrivilegeStatus is the single spelling of the 42501 rule: 403 when
// authenticated, 401 when anonymous, untouched for every other code.
func TestGradePrivilegeStatus(t *testing.T) {
	native := New(http.StatusUnauthorized, CodeInsufficientPrivilege, "permission denied for table films")
	if got := GradePrivilegeStatus(native, true).HTTPStatus; got != http.StatusForbidden {
		t.Errorf("authenticated 42501 = %d, want 403", got)
	}
	if got := GradePrivilegeStatus(native, false).HTTPStatus; got != http.StatusUnauthorized {
		t.Errorf("anonymous 42501 = %d, want 401", got)
	}
	if native.HTTPStatus != http.StatusUnauthorized {
		t.Error("GradePrivilegeStatus mutated its argument")
	}
	other := ErrConstraintViolation("23505", "duplicate key", "", "")
	if got := GradePrivilegeStatus(other, true); got != other {
		t.Error("non-42501 errors must pass through unchanged")
	}
	if GradePrivilegeStatus(nil, true) != nil {
		t.Error("nil must pass through")
	}
}

// The empty-message constructors fall back to a non-empty default rather than
// shipping a blank message to the client.
func TestEmptyMessageDefaults(t *testing.T) {
	if got := ErrMethodNotAllowed("").Message; got == "" {
		t.Error("ErrMethodNotAllowed default message is empty")
	}
	if got := ErrMethodNotAllowed("custom").Message; got != "custom" {
		t.Errorf("ErrMethodNotAllowed override = %q, want custom", got)
	}
}

// The JWT errors carry the WWW-Authenticate challenge PostgREST sends on every
// 401: the RFC 6750 invalid_token form on PGRST301/PGRST303, the bare Bearer on
// PGRST302 and on an anonymous privilege denial.
func TestJWTErrorsCarryWWWAuthenticate(t *testing.T) {
	wantInvalid := `Bearer error="invalid_token", error_description="JWT expired"`
	if got := ErrJWTClaims("JWT expired").WWWAuthenticate; got != wantInvalid {
		t.Errorf("ErrJWTClaims challenge = %q, want %q", got, wantInvalid)
	}
	if got := ErrJWTDecode("JWT couldn't be decoded").WWWAuthenticate; got == "" {
		t.Error("ErrJWTDecode must carry an invalid_token challenge")
	}
	if got := ErrJWTRequired().WWWAuthenticate; got != "Bearer" {
		t.Errorf("ErrJWTRequired challenge = %q, want Bearer", got)
	}
	if got := ErrPermissionDenied("films", true).WWWAuthenticate; got != "Bearer" {
		t.Errorf("anonymous ErrPermissionDenied challenge = %q, want Bearer", got)
	}
	if got := ErrPermissionDenied("films", false).WWWAuthenticate; got != "" {
		t.Errorf("authenticated ErrPermissionDenied challenge = %q, want none", got)
	}
	if got := ErrJWTSecretMissing().WWWAuthenticate; got != "" {
		t.Errorf("ErrJWTSecretMissing challenge = %q, want none on a 500", got)
	}
}

// The v14 message texts replaced several pre-v12 spellings; clients match on
// them, so each retired text is pinned to its current form here.
func TestV14MessageTexts(t *testing.T) {
	if got, want := ErrSingularZeroMany().Message, "Cannot coerce the result to a single JSON object"; got != want {
		t.Errorf("PGRST116 message = %q, want %q", got, want)
	}
	if got, want := ErrUnsupported("the sl operator", "mysql").Message, "Feature not implemented"; got != want {
		t.Errorf("PGRST127 message = %q, want %q", got, want)
	}
	if got, want := ErrFullTextUnavailable("body", "sqlite").Message, "Feature not implemented"; got != want {
		t.Errorf("PGRST127 fts message = %q, want %q", got, want)
	}
}

// A 22P02 names the type the way PostgreSQL's own message does: the SQL
// standard spelling, never the internal catalog name.
func TestInvalidInputTypeSpelling(t *testing.T) {
	cases := map[string]string{
		"int2":   "smallint",
		"int4":   "integer",
		"int8":   "bigint",
		"float4": "real",
		"float8": "double precision",
		"bool":   "boolean",
		"uuid":   "uuid", // no PG alias, passes through
	}
	for canonical, spelled := range cases {
		got := ErrInvalidInput(canonical, "abc").Message
		want := `invalid input syntax for type ` + spelled + `: "abc"`
		if got != want {
			t.Errorf("ErrInvalidInput(%q) message = %q, want %q", canonical, got, want)
		}
	}
}

// A constraint violation carries the engine's text through untouched: the
// message keeps its constraint name and the detail its key, the parts clients
// parse out of a live PostgREST response.
func TestConstraintViolationPassesTextThrough(t *testing.T) {
	e := ErrConstraintViolation("23505",
		`duplicate key value violates unique constraint "todos_pkey"`,
		"Key (id)=(1) already exists.", "")
	if e.Message != `duplicate key value violates unique constraint "todos_pkey"` {
		t.Errorf("message = %q", e.Message)
	}
	if e.Details == nil || *e.Details != "Key (id)=(1) already exists." {
		t.Errorf("details = %v", e.Details)
	}
	if e.Hint != nil {
		t.Errorf("hint = %v, want null when the engine gave none", e.Hint)
	}
}

// 42703 carries PostgreSQL's own message shape: the qualified column, no
// quotes, exactly as a live v14 forwards it.
func TestUndefinedColumnMessage(t *testing.T) {
	got := ErrUndefinedColumn("todos.nope").Message
	if want := "column todos.nope does not exist"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// The wrong-verb and read-only texts match a live v14's exactly.
func TestRPCMethodMessages(t *testing.T) {
	if got, want := ErrInvalidRPCMethod("TRACE").Message, "Cannot use the TRACE method on RPC"; got != want {
		t.Errorf("PGRST101 message = %q, want %q", got, want)
	}
	if got, want := ErrReadOnlyTransaction("UPDATE").Message, "cannot execute UPDATE in a read-only transaction"; got != want {
		t.Errorf("25006 message = %q, want %q", got, want)
	}
}

// PGRST203 spells the surviving overloads into the message and tells the
// client how to break the tie; PGRST125's message is pinned to the live text.
func TestAmbiguousFunctionAndInvalidPath(t *testing.T) {
	e := ErrAmbiguousFunction([]string{"api.add(a => integer)", "api.add(a => text)"})
	want := "Could not choose the best candidate function between: api.add(a => integer), api.add(a => text)"
	if e.Message != want {
		t.Errorf("PGRST203 message = %q, want %q", e.Message, want)
	}
	if e.Hint == nil || !strings.Contains(*e.Hint, "function overloading can be resolved") {
		t.Errorf("PGRST203 hint = %v, want the renaming suggestion", e.Hint)
	}
	if got, want := ErrInvalidPath().Message, "Invalid path specified in request URL"; got != want {
		t.Errorf("PGRST125 message = %q, want %q", got, want)
	}
}

// PGRST102 is the v14 code for every request-body failure. The default message
// is PostgREST's generic JSON-body text; a specific parser failure overrides it.
func TestInvalidBodyMessages(t *testing.T) {
	if got := ErrInvalidBody("").Message; got != "Empty or invalid json" {
		t.Errorf("default message = %q, want %q", got, "Empty or invalid json")
	}
	if got := ErrInvalidBody("All object keys must match").Message; got != "All object keys must match" {
		t.Errorf("override message = %q", got)
	}
}

// The request-side media type error carries PostgREST's exact message shape,
// naming the offending Content-Type.
func TestUnsupportedMediaTypeMessage(t *testing.T) {
	got := ErrUnsupportedMediaType("application/yaml").Message
	if want := "Content-Type not acceptable: application/yaml"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// ErrUnsupported and ErrFullTextUnavailable both carry the detail and hint that
// make a PGRST127 actionable; a bare code is not enough for the client to know
// what to change.
func TestUnsupportedCarriesDetailAndHint(t *testing.T) {
	for _, e := range []*APIError{
		ErrUnsupported("the sl operator", "mysql"),
		ErrFullTextUnavailable("body", "sqlite"),
	} {
		if e.Details == nil || *e.Details == "" {
			t.Errorf("%s: missing details", e.Code)
		}
		if e.Hint == nil || *e.Hint == "" {
			t.Errorf("%s: missing hint", e.Code)
		}
	}
}

func TestErrorString(t *testing.T) {
	if got := ErrParse("bad").Error(); got != "PGRST100: bad" {
		t.Errorf("Error() = %q", got)
	}
	var nilErr *APIError
	if got := nilErr.Error(); got != "<nil pgerr.APIError>" {
		t.Errorf("nil Error() = %q", got)
	}
}

func TestWithMessage(t *testing.T) {
	base := ErrParse("original")
	got := base.WithMessage("replaced")
	if base.Message != "original" {
		t.Error("WithMessage mutated the receiver")
	}
	if got.Message != "replaced" {
		t.Errorf("message = %q, want replaced", got.Message)
	}
}

// ErrNoRelationship names the searched pair and the schema in its details, and
// the schema comes from the parent relation (item 04.4). A bare search reports
// no hint; a hinted one echoes the hint clause before the schema.
func TestNoRelationshipDetails(t *testing.T) {
	bare := ErrNoRelationship("films", "directors", "public", "")
	if bare.Details == nil {
		t.Fatal("details are nil")
	}
	want := "Searched for a foreign key relationship between 'films' and 'directors' in the schema 'public', but no matches were found."
	if *bare.Details != want {
		t.Errorf("details = %q, want %q", *bare.Details, want)
	}

	hinted := ErrNoRelationship("films", "directors", "api", "fk_director")
	wantHinted := "Searched for a foreign key relationship between 'films' and 'directors' using the hint 'fk_director' in the schema 'api', but no matches were found."
	if hinted.Details == nil || *hinted.Details != wantHinted {
		t.Errorf("hinted details = %v, want %q", hinted.Details, wantHinted)
	}
}

// ErrAmbiguousEmbed renders the candidate array verbatim and a Try-changing hint
// listing each candidate's disambiguated embed spelling (item 04.4). With no
// candidates it degrades to message only rather than an empty array and hint.
func TestAmbiguousEmbedDetailsAndHint(t *testing.T) {
	cands := []EmbedCandidate{
		{Cardinality: "many-to-one", Embedding: "films with people", Relationship: "films_director_id_fkey using films(director_id) and people(id)", Name: "films_director_id_fkey"},
		{Cardinality: "many-to-one", Embedding: "films with people", Relationship: "films_writer_id_fkey using films(writer_id) and people(id)", Name: "films_writer_id_fkey"},
	}
	e := ErrAmbiguousEmbed("films", "people", cands)
	if e.RawDetails == nil {
		t.Fatal("details are nil, want the candidate array")
	}
	var got []EmbedCandidate
	if err := json.Unmarshal(e.RawDetails, &got); err != nil {
		t.Fatalf("details not an array: %v", err)
	}
	if len(got) != 2 || got[0].Cardinality != "many-to-one" {
		t.Errorf("candidates = %v", got)
	}
	// Name is carried for the hint, not the details body.
	if bytesHasName := json.RawMessage(e.RawDetails); jsonContains(bytesHasName, `"name"`) {
		t.Error("details array leaked the unexported name key")
	}
	wantHint := "Try changing 'people' to one of the following: 'people!films_director_id_fkey', 'people!films_writer_id_fkey'. Find the desired relationship in the 'details' key."
	if e.Hint == nil || *e.Hint != wantHint {
		t.Errorf("hint = %v, want %q", e.Hint, wantHint)
	}

	if bare := ErrAmbiguousEmbed("films", "people", nil); bare.RawDetails != nil || bare.Hint != nil {
		t.Errorf("no-candidate ambiguous embed should be message only, got details=%s hint=%v", bare.RawDetails, bare.Hint)
	}
}

// jsonContains reports whether raw JSON bytes contain the literal substring,
// used to assert the details array does not serialize the candidate name key.
func jsonContains(raw json.RawMessage, sub string) bool {
	return strings.Contains(string(raw), sub)
}

// JSON rendering is on the error path of every failed request, so it carries its
// own benchmark: the four-key envelope with details and hint populated, the
// shape a PGRST127 actually ships.
func BenchmarkJSON(b *testing.B) {
	e := ErrUnsupported("the sl operator", "mysql")
	b.ReportAllocs()
	for b.Loop() {
		_ = e.JSON()
	}
}
