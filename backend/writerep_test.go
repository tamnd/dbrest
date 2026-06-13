package backend

import (
	"reflect"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

func intp(n int) *int { return &n }

func boolp(b bool) *bool { return &b }

// Ordering a write representation sorts the buffered rows by a plain column,
// numerically for an integer column and honouring descending.
func TestShapeWriteRepresentationOrders(t *testing.T) {
	cols := []string{"id", "name"}
	rows := [][]any{
		{int64(2), "b"},
		{int64(10), "j"},
		{int64(1), "a"},
	}
	q := &ir.Query{Order: []ir.OrderTerm{{Path: []string{"id"}, Desc: true}}}
	got := ShapeWriteRepresentation(cols, rows, q)
	want := [][]any{{int64(10), "j"}, {int64(2), "b"}, {int64(1), "a"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ordered rows = %v, want %v", got, want)
	}
}

// limit and offset bound the returned body after ordering.
func TestShapeWriteRepresentationPaginates(t *testing.T) {
	cols := []string{"id"}
	rows := [][]any{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}}
	q := &ir.Query{
		Order:  []ir.OrderTerm{{Path: []string{"id"}}},
		Offset: intp(1),
		Limit:  intp(2),
	}
	got := ShapeWriteRepresentation(cols, rows, q)
	want := [][]any{{int64(2)}, {int64(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged rows = %v, want %v", got, want)
	}
}

// An offset past the end yields an empty body, not an error.
func TestShapeWriteRepresentationOffsetPastEnd(t *testing.T) {
	cols := []string{"id"}
	rows := [][]any{{int64(1)}, {int64(2)}}
	q := &ir.Query{Offset: intp(5)}
	if got := ShapeWriteRepresentation(cols, rows, q); len(got) != 0 {
		t.Errorf("rows = %v, want empty", got)
	}
}

// NULLs sort last on ascending by default and first on descending, matching
// PostgreSQL; an explicit nullsfirst overrides the default.
func TestShapeWriteRepresentationNullsPlacement(t *testing.T) {
	cols := []string{"v"}
	mk := func() [][]any { return [][]any{{int64(2)}, {nil}, {int64(1)}} }

	asc := ShapeWriteRepresentation(cols, mk(), &ir.Query{Order: []ir.OrderTerm{{Path: []string{"v"}}}})
	if asc[2][0] != nil {
		t.Errorf("asc default: null should sort last, got %v", asc)
	}

	desc := ShapeWriteRepresentation(cols, mk(), &ir.Query{Order: []ir.OrderTerm{{Path: []string{"v"}, Desc: true}}})
	if desc[0][0] != nil {
		t.Errorf("desc default: null should sort first, got %v", desc)
	}

	nf := ShapeWriteRepresentation(cols, mk(), &ir.Query{Order: []ir.OrderTerm{{Path: []string{"v"}, NullsFirst: boolp(true)}}})
	if nf[0][0] != nil {
		t.Errorf("asc nullsfirst: null should sort first, got %v", nf)
	}
}

// A term naming a column outside the projection is skipped: the representation
// cannot order by a value it never carried. The rows keep their order.
func TestShapeWriteRepresentationSkipsAbsentColumn(t *testing.T) {
	cols := []string{"id"}
	rows := [][]any{{int64(3)}, {int64(1)}, {int64(2)}}
	q := &ir.Query{Order: []ir.OrderTerm{{Path: []string{"name"}}}}
	got := ShapeWriteRepresentation(cols, rows, q)
	want := [][]any{{int64(3)}, {int64(1)}, {int64(2)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want unchanged %v", got, want)
	}
}

// Shaping never alters the affected count: the caller takes it from the full
// buffer before shaping. This guards the contract that order/limit bound only
// the body, not the mutation. Here the full set is 4 rows; the shaped body is 1.
func TestShapeWriteRepresentationLeavesCallerCountAlone(t *testing.T) {
	cols := []string{"id"}
	full := [][]any{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}}
	affected := int64(len(full))
	q := &ir.Query{Order: []ir.OrderTerm{{Path: []string{"id"}}}, Limit: intp(1)}
	body := ShapeWriteRepresentation(cols, full, q)
	if affected != 4 {
		t.Errorf("affected = %d, want 4 (the full mutated set)", affected)
	}
	if len(body) != 1 {
		t.Errorf("body rows = %d, want 1", len(body))
	}
}
