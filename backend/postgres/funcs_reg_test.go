package postgres

import (
	"reflect"
	"testing"

	"github.com/tamnd/dbrest/rpc"
)

func strp(s string) *string { return &s }

// TestBuildParams covers the input-parameter reconstruction from pg_proc facts that
// loadFunctionRegistry feeds it (finding 03-P03): proargtypes is input-only and in
// order, the trailing pronargdefaults inputs are optional, a variadic function's
// last input collects its tail, and a lone unnamed body-typed input is a raw body.
func TestBuildParams(t *testing.T) {
	cases := []struct {
		name      string
		nargs     int
		ndefaults int
		variadic  bool
		inTypes   []string
		inNames   []*string
		want      []rpc.Param
	}{
		{
			name:    "no arguments",
			inTypes: nil,
			want:    nil,
		},
		{
			name:    "two required",
			nargs:   2,
			inTypes: []string{"int4", "int4"},
			inNames: []*string{strp("a"), strp("b")},
			want: []rpc.Param{
				{Name: "a", Type: "int4"},
				{Name: "b", Type: "int4"},
			},
		},
		{
			name:      "trailing default is optional",
			nargs:     2,
			ndefaults: 1,
			inTypes:   []string{"text", "text"},
			inNames:   []*string{strp("name"), strp("greeting")},
			want: []rpc.Param{
				{Name: "name", Type: "text"},
				{Name: "greeting", Type: "text", Optional: true},
			},
		},
		{
			name:     "variadic last input",
			nargs:    1,
			variadic: true,
			inTypes:  []string{"_int4"},
			inNames:  []*string{strp("vals")},
			want: []rpc.Param{
				{Name: "vals", Type: "_int4", Variadic: true},
			},
		},
		{
			name:    "single unnamed json is a raw body",
			nargs:   1,
			inTypes: []string{"json"},
			inNames: nil,
			want: []rpc.Param{
				{Name: "__raw_body", Type: "json", RawBody: true},
			},
		},
		{
			name:    "single unnamed non-body type is an ordinary unnamed arg",
			nargs:   1,
			inTypes: []string{"int4"},
			inNames: nil,
			want: []rpc.Param{
				{Name: "", Type: "int4"},
			},
		},
		{
			name:    "named json is not a raw body",
			nargs:   1,
			inTypes: []string{"jsonb"},
			inNames: []*string{strp("payload")},
			want: []rpc.Param{
				{Name: "payload", Type: "jsonb"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildParams(c.nargs, c.ndefaults, c.variadic, c.inTypes, c.inNames)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("buildParams = %+v\nwant         %+v", got, c.want)
			}
		})
	}
}

func TestIsRawBodyType(t *testing.T) {
	for _, ok := range []string{"json", "jsonb", "text", "xml", "bytea"} {
		if !isRawBodyType(ok) {
			t.Errorf("isRawBodyType(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"int4", "numeric", "uuid", "timestamptz", ""} {
		if isRawBodyType(no) {
			t.Errorf("isRawBodyType(%q) = true, want false", no)
		}
	}
}
