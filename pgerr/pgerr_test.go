package pgerr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvelopeAlwaysHasFourKeys(t *testing.T) {
	e := ErrParse("bad operator")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.JSON(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"code", "message", "details", "hint"} {
		if _, ok := m[k]; !ok {
			t.Errorf("envelope missing key %q: %s", k, e.JSON())
		}
	}
	// details and hint are null when unset, never omitted.
	if string(m["details"]) != "null" {
		t.Errorf("details = %s, want null", m["details"])
	}
	if string(m["hint"]) != "null" {
		t.Errorf("hint = %s, want null", m["hint"])
	}
}

func TestWithDetailsHintImmutable(t *testing.T) {
	base := ErrParse("x")
	d := base.WithDetails("more")
	if base.Details != nil {
		t.Error("WithDetails mutated the receiver")
	}
	if d.Details == nil || *d.Details != "more" {
		t.Errorf("details not set on copy: %+v", d.Details)
	}
	h := d.WithHint("try this")
	if d.Hint != nil {
		t.Error("WithHint mutated the receiver")
	}
	if h.Hint == nil || *h.Hint != "try this" {
		t.Errorf("hint not set on copy: %+v", h.Hint)
	}
}

// PGRST201 returns details as a JSON array of candidate relationship objects;
// the envelope must carry it as an array, not a quoted string, while string
// details and null keep their existing encodings.
func TestDetailsCanCarryNonStringJSON(t *testing.T) {
	candidates := []map[string]string{{
		"cardinality":  "many-to-one",
		"embedding":    "orders with addresses",
		"relationship": "billing using orders(billing_address_id) and addresses(id)",
	}}
	base := ErrAmbiguousEmbed("orders", "addresses", nil)
	e := base.WithDetailsJSON(candidates)
	if base.RawDetails != nil {
		t.Error("WithDetailsJSON mutated the receiver")
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.JSON(), &m); err != nil {
		t.Fatalf("envelope not valid json: %v", err)
	}
	var got []map[string]string
	if err := json.Unmarshal(m["details"], &got); err != nil {
		t.Fatalf("details is not a JSON array: %v: %s", err, m["details"])
	}
	if len(got) != 1 || got[0]["embedding"] != "orders with addresses" {
		t.Errorf("details round-trip = %v", got)
	}

	// A string details still renders as a JSON string.
	var sm map[string]json.RawMessage
	se := ErrParse("x").WithDetails("plain text")
	if err := json.Unmarshal(se.JSON(), &sm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(sm["details"]) != `"plain text"` {
		t.Errorf("string details = %s, want %q", sm["details"], `"plain text"`)
	}

	// Raw details win over a previously set string.
	both := se.WithDetailsJSON([]int{1, 2})
	var bm map[string]json.RawMessage
	if err := json.Unmarshal(both.JSON(), &bm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(bm["details"]) != "[1,2]" {
		t.Errorf("raw details = %s, want [1,2]", bm["details"])
	}
}

func TestUnsupportedNamesFeatureAndBackend(t *testing.T) {
	e := ErrUnsupported("range operator 'sl'", "mysql")
	if e.HTTPStatus != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", e.HTTPStatus)
	}
	if e.Code != CodeUnsupported {
		t.Errorf("code = %s, want %s", e.Code, CodeUnsupported)
	}
	if e.Details == nil {
		t.Fatal("details must name feature and backend")
	}
	got := *e.Details
	if want := "range operator 'sl' is not supported by the mysql backend"; got != want {
		t.Errorf("details = %q, want %q", got, want)
	}
}

func TestWriteSetsStatusAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	ErrUnknownTable("films").Write(rec)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	// v14 names the error code in Proxy-Status so HEAD requests can identify
	// the failure; the value matches a live v14's byte for byte.
	if ps := rec.Header().Get("Proxy-Status"); ps != "PostgREST; error=PGRST205" {
		t.Errorf("Proxy-Status = %q, want %q", ps, "PostgREST; error=PGRST205")
	}
	var b body
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body not valid json: %v", err)
	}
	if b.Code != CodeUnknownTable {
		t.Errorf("code = %s", b.Code)
	}
	if h := rec.Header().Get("WWW-Authenticate"); h != "" {
		t.Errorf("a non-auth error must not carry WWW-Authenticate, got %q", h)
	}
}

func TestWriteEmitsWWWAuthenticate(t *testing.T) {
	rec := httptest.NewRecorder()
	ErrJWTClaims("JWT expired").Write(rec)
	want := `Bearer error="invalid_token", error_description="JWT expired"`
	if h := rec.Header().Get("WWW-Authenticate"); h != want {
		t.Errorf("WWW-Authenticate = %q, want %q", h, want)
	}
}

func TestAs(t *testing.T) {
	if As(nil) != nil {
		t.Error("As(nil) should be nil")
	}
	e := ErrParse("x")
	if As(error(e)) != e {
		t.Error("As should unwrap an *APIError")
	}
}
