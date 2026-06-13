package postgres

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// 07.13: a native (non-registry) RPC with count=exact used to crash, because the
// count path ran the registry count compiler on a nil function. The native count
// is now built in the backend by wrapping the same call the row query runs.

// TestCompileNativeCallCountWrapsCall: the native count is a count(*) over the
// SELECT * FROM fn(...) the row statement issues.
func TestCompileNativeCallCountWrapsCall(t *testing.T) {
	b := &Backend{searchPath: []string{"public"}}
	c := &ir.Call{Function: ir.Ref{Name: "recent_films"}}

	row, apiErr := b.compileNativeCall(c, "public")
	if apiErr != nil {
		t.Fatalf("compileNativeCall: %v", apiErr)
	}
	cnt, apiErr := b.compileNativeCallCount(c, "public")
	if apiErr != nil {
		t.Fatalf("compileNativeCallCount: %v", apiErr)
	}

	want := "SELECT count(*) FROM (" + row.SQL + ") _rpc"
	if cnt.SQL != want {
		t.Errorf("count SQL = %q, want %q", cnt.SQL, want)
	}
	if !strings.Contains(cnt.SQL, `"public"."recent_films"`) {
		t.Errorf("count SQL missing schema-qualified call: %q", cnt.SQL)
	}
}

// TestCompileNativeCallCountWithArgs: the wrapper carries the call arguments
// through as embedded literals, the same as the row statement.
func TestCompileNativeCallCountWithArgs(t *testing.T) {
	b := &Backend{searchPath: []string{"app"}}
	c := &ir.Call{
		Function: ir.Ref{Name: "search"},
		Args:     map[string]ir.Value{"q": {Text: "blade"}},
	}
	cnt, apiErr := b.compileNativeCallCount(c, "app")
	if apiErr != nil {
		t.Fatalf("compileNativeCallCount: %v", apiErr)
	}
	if !strings.HasPrefix(cnt.SQL, "SELECT count(*) FROM (SELECT * FROM ") {
		t.Errorf("count SQL prefix = %q", cnt.SQL)
	}
	if !strings.Contains(cnt.SQL, "'blade'") {
		t.Errorf("count SQL missing argument literal: %q", cnt.SQL)
	}
}
