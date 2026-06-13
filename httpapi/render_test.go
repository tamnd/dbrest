package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// fakeBackend is a backend.Backend whose only behaviour under test is MapError;
// the rest are inert. asAPIError is the only caller that touches the backend at
// all, and only for MapError, so the other methods need no real bodies.
type fakeBackend struct {
	mapErr func(error) *pgerr.APIError
}

func (f fakeBackend) Capabilities() backend.Capabilities { return backend.Capabilities{} }
func (f fakeBackend) Introspect(context.Context) (*schema.Model, error) {
	return nil, nil
}
func (f fakeBackend) Functions() rpc.Registry { return nil }
func (f fakeBackend) Execute(context.Context, *ir.Plan, *reqctx.Context) (backend.Result, error) {
	return nil, nil
}
func (f fakeBackend) MapError(err error) *pgerr.APIError { return f.mapErr(err) }
func (f fakeBackend) Close() error                       { return nil }

// csvCell formats one result value for a CSV cell. A scanning backend hands the
// renderer whichever Go type its driver produced, so the formatter must handle
// each: text, bytes, bool, the JSON number forms, and a nested structure that
// becomes embedded JSON. NULL is the empty field. The forms are covered here
// because a CSV response otherwise only exercises whatever type one fixture
// column happens to be.
func TestCSVCellForms(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"null", nil, ""},
		{"string", "Dune", "Dune"},
		{"bytes", []byte("blob"), "blob"},
		{"bool-true", true, "true"},
		{"bool-false", false, "false"},
		{"json-number", json.Number("42"), "42"},
		{"float", 3.5, "3.5"},
		{"int64", int64(-7), "-7"},
		{"nested-map", map[string]any{"a": 1}, `{"a":1}`},
		{"nested-slice", []any{1, 2}, "[1,2]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := csvCell(c.in); got != c.want {
				t.Errorf("csvCell(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A value json.Marshal cannot encode falls back to fmt.Sprint rather than
// producing an empty cell or panicking.
func TestCSVCellUnmarshalableFallsBackToSprint(t *testing.T) {
	// A channel is not JSON-encodable; the default branch must still return text.
	got := csvCell(make(chan int))
	if got == "" {
		t.Error("an unmarshalable value should fall back to a non-empty text form")
	}
}

// rawJSON wraps engine-assembled JSON so the encoder emits it verbatim; a NULL
// to-one embed stays nil and renders as JSON null.
func TestRawJSONForms(t *testing.T) {
	if got := rawJSON(nil); got != nil {
		t.Errorf("rawJSON(nil) = %#v, want nil", got)
	}
	if got, ok := rawJSON(`{"a":1}`).(json.RawMessage); !ok || string(got) != `{"a":1}` {
		t.Errorf("rawJSON(string) = %#v, want RawMessage", got)
	}
	if got, ok := rawJSON([]byte(`[1]`)).(json.RawMessage); !ok || string(got) != `[1]` {
		t.Errorf("rawJSON(bytes) = %#v, want RawMessage", got)
	}
	// A non-text value (an already-decoded number) passes through unchanged.
	if got := rawJSON(42); got != 42 {
		t.Errorf("rawJSON(42) = %#v, want 42 unchanged", got)
	}
}

// rawJSONValue embeds a json/jsonb-declared scalar verbatim so a function
// returning json does not double-encode into a quoted string on a portable
// backend, where the driver hands the result back as TEXT. A non-json declaration
// leaves the value untouched, and an invalid-JSON string under a json declaration
// is left as a string rather than emitted as a broken document.
func TestRawJSONValue(t *testing.T) {
	if got, ok := rawJSONValue(`{"a":1}`, "json").(json.RawMessage); !ok || string(got) != `{"a":1}` {
		t.Errorf("json scalar = %#v, want RawMessage", got)
	}
	if got, ok := rawJSONValue(`[1,2]`, "jsonb").(json.RawMessage); !ok || string(got) != `[1,2]` {
		t.Errorf("jsonb scalar = %#v, want RawMessage", got)
	}
	// A non-json declaration passes the text through as a plain string, which the
	// encoder will quote.
	if got := rawJSONValue(`{"a":1}`, "text"); got != `{"a":1}` {
		t.Errorf("text scalar = %#v, want the string unchanged", got)
	}
	// Malformed JSON under a json declaration is not wrapped, so the encoder quotes
	// it rather than emitting an invalid document.
	if _, ok := rawJSONValue(`{not json`, "json").(json.RawMessage); ok {
		t.Error("invalid JSON should not become a RawMessage")
	}
}

// asAPIError normalizes a backend execution error three ways: an error that is
// already an API error passes straight through, an engine-native error the
// backend recognizes becomes whatever it maps to, and anything else falls back
// to an internal error carrying the original text. Each branch is exercised so
// the fallback is not the only path a live engine happens to hit.
func TestAsAPIError(t *testing.T) {
	// Branch one: an error that already is an APIError is returned unchanged,
	// without consulting the backend at all.
	t.Run("already-api-error", func(t *testing.T) {
		want := pgerr.ErrSingularZeroMany()
		b := fakeBackend{mapErr: func(error) *pgerr.APIError {
			t.Fatal("MapError must not be called when the error is already an APIError")
			return nil
		}}
		if got := asAPIError(b, want); got != want {
			t.Errorf("asAPIError = %#v, want the original %#v", got, want)
		}
	})

	// Branch two: a raw engine error the backend recognizes becomes its mapping.
	t.Run("backend-maps-it", func(t *testing.T) {
		mapped := pgerr.ErrUniqueViolation("films_pkey")
		b := fakeBackend{mapErr: func(error) *pgerr.APIError { return mapped }}
		if got := asAPIError(b, errors.New("duplicate key")); got != mapped {
			t.Errorf("asAPIError = %#v, want the backend mapping %#v", got, mapped)
		}
	})

	// Branch three: an error the backend does not recognize falls back to an
	// internal error that preserves the original message.
	t.Run("internal-fallback", func(t *testing.T) {
		b := fakeBackend{mapErr: func(error) *pgerr.APIError { return nil }}
		got := asAPIError(b, errors.New("boom"))
		if got == nil || got.HTTPStatus != 500 {
			t.Fatalf("want a 500 internal error, got %#v", got)
		}
		if got.Message != "boom" {
			t.Errorf("message = %q, want the original %q", got.Message, "boom")
		}
	})
}
