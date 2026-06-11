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
		{"unknown-table", ErrUnknownTable("films"), http.StatusNotFound, CodeUnknownTable},
		{"unknown-column", ErrUnknownColumn("titel"), http.StatusBadRequest, CodeUnknownColumn},
		{"no-relationship", ErrNoRelationship("films", "actors"), http.StatusBadRequest, CodeNoRelationship},
		{"ambiguous-embed", ErrAmbiguousEmbed("films", "actors"), http.StatusMultipleChoices, CodeAmbiguousEmbed},
		{"no-function", ErrNoFunction("add"), http.StatusNotFound, CodeNoFunction},
		{"method-not-allowed", ErrMethodNotAllowed(""), http.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{"unsupported", ErrUnsupported("the sl operator", "mysql"), http.StatusBadRequest, CodeUnsupported},
		{"fts-unavailable", ErrFullTextUnavailable("body", "sqlite"), http.StatusBadRequest, CodeUnsupported},
		{"unique", ErrUniqueViolation("Key (id)=(1) already exists"), http.StatusConflict, CodeUniqueViolation},
		{"not-null", ErrNotNullViolation("column title"), http.StatusBadRequest, CodeNotNullViolation},
		{"foreign-key", ErrForeignKeyViolation("Key (dir)=(9) is not present"), http.StatusConflict, CodeForeignKeyViolation},
		{"check", ErrCheckViolation("rating must be positive"), http.StatusBadRequest, CodeCheckViolation},
		{"invalid-input", ErrInvalidInput("integer", "abc"), http.StatusBadRequest, CodeInvalidText},
		{"jwt-expired", ErrJWTExpired(), http.StatusUnauthorized, CodeJWTExpired},
		{"jwt-invalid", ErrJWTInvalid(""), http.StatusUnauthorized, CodeJWTInvalid},
		{"role-not-allowed", ErrRoleNotAllowed("admin"), http.StatusForbidden, CodeInsufficientPrivilege},
		{"permission-denied", ErrPermissionDenied("films", false), http.StatusForbidden, CodeInsufficientPrivilege},
		{"rls-violation", ErrRLSViolation("films"), http.StatusForbidden, CodeInsufficientPrivilege},
		{"internal", ErrInternal("boom"), http.StatusInternalServerError, CodeInternal},
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

// The empty-message constructors fall back to a non-empty default rather than
// shipping a blank message to the client.
func TestEmptyMessageDefaults(t *testing.T) {
	if got := ErrMethodNotAllowed("").Message; got == "" {
		t.Error("ErrMethodNotAllowed default message is empty")
	}
	if got := ErrJWTInvalid("").Message; got == "" {
		t.Error("ErrJWTInvalid default message is empty")
	}
	if got := ErrMethodNotAllowed("custom").Message; got != "custom" {
		t.Errorf("ErrMethodNotAllowed override = %q, want custom", got)
	}
	if got := ErrJWTInvalid("custom").Message; got != "custom" {
		t.Errorf("ErrJWTInvalid override = %q, want custom", got)
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
