package sqlite

import (
	"testing"

	"github.com/tamnd/dbrest/reqctx"
)

// The read result streams rows: Body is nil (SQLite does not assemble JSON on
// this path), Affected does not apply, Count reflects an exact-count request,
// and ResponseControls passes through whatever the planner set. These accessors
// are part of the backend.Result contract but the read-path tests reach them
// only through Rows, so they are pinned directly here.
func TestReadResultAccessors(t *testing.T) {
	ctrl := &reqctx.ResponseControls{}
	r := &result{controls: ctrl, count: 12, hasCount: true}
	if r.Body() != nil {
		t.Error("read result assembles no Body; it streams Rows")
	}
	if n, ok := r.Affected(); ok || n != 0 {
		t.Errorf("Affected = (%d, %v), want (0, false) for a read", n, ok)
	}
	if n, ok := r.Count(); !ok || n != 12 {
		t.Errorf("Count = (%d, %v), want (12, true)", n, ok)
	}
	if r.ResponseControls() != ctrl {
		t.Error("ResponseControls should pass through the planner's controls")
	}
}

// The write result buffers rows so it can be replayed: Body is nil, Count does
// not apply, Affected reports the mutation's row count, and a fresh Rows cursor
// starts before the first row so the handler can iterate more than once.
func TestWriteResultAccessors(t *testing.T) {
	ctrl := &reqctx.ResponseControls{}
	r := &writeResult{
		cols:     []string{"id"},
		rows:     [][]any{{int64(1)}, {int64(2)}},
		affected: 2,
		hasAff:   true,
		controls: ctrl,
	}
	if r.Body() != nil {
		t.Error("write result assembles no Body")
	}
	if n, ok := r.Count(); ok || n != 0 {
		t.Errorf("Count = (%d, %v), want (0, false) for a write", n, ok)
	}
	if n, ok := r.Affected(); !ok || n != 2 {
		t.Errorf("Affected = (%d, %v), want (2, true)", n, ok)
	}
	if r.ResponseControls() != ctrl {
		t.Error("ResponseControls should pass through")
	}
	// A second iteration must see the same rows: the buffer replays.
	for pass := range 2 {
		rs := r.Rows()
		var got int
		for rs.Next() {
			if _, err := rs.Values(); err != nil {
				t.Fatalf("Values: %v", err)
			}
			got++
		}
		if got != 2 {
			t.Errorf("pass %d saw %d rows, want 2", pass, got)
		}
		if err := rs.Err(); err != nil {
			t.Errorf("Err = %v", err)
		}
		rs.Close()
	}
}

// Open reports an error rather than returning a half-built backend when the DSN
// cannot be opened: a path under a directory that does not exist fails the ping.
func TestOpenBadDSNErrors(t *testing.T) {
	if b, err := Open("/no/such/dir/db.sqlite"); err == nil {
		b.Close()
		t.Fatal("Open of an unreachable path should error")
	}
}
