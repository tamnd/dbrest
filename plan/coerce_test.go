package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

func TestReadCoercesIntegerFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpEq, Value: ir.Value{Text: "abc"}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "22P02" {
		t.Fatalf("a non-integer operand on an integer column should be 22P02, got %v", err)
	}
}

func TestReadAcceptsValidIntegerFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	if _, err := Read(model(), q, nil, Options{}); err != nil {
		t.Fatalf("a valid integer operand should pass, got %v", err)
	}
}

func TestReadCoercesInListMembers(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpIn, Value: ir.Value{List: []string{"1", "2", "x"}}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "22P02" {
		t.Fatalf("a bad member of an in-list on an integer column should be 22P02, got %v", err)
	}
}

func TestReadTextFilterAcceptsAnything(t *testing.T) {
	// A text column carries any operand through to the engine.
	where := ir.Cond(ir.Compare{Path: []string{"title"}, Op: ir.OpEq, Value: ir.Value{Text: "anything 123 !@#"}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	if _, err := Read(model(), q, nil, Options{}); err != nil {
		t.Fatalf("a text operand should never be rejected, got %v", err)
	}
}

func TestReadLikePatternNotCoerced(t *testing.T) {
	// like takes a pattern, not a typed value, even on an integer column: it is
	// left for the engine, not rejected as a bad integer.
	where := ir.Cond(ir.Compare{Path: []string{"year"}, Op: ir.OpLike, Value: ir.Value{Text: "20%"}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	if _, err := Read(model(), q, nil, Options{}); err != nil {
		t.Fatalf("a like pattern should not be coerced, got %v", err)
	}
}

func TestWriteCoercesFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: "notnum"}})
	q := &ir.Query{
		Kind:     ir.Delete,
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Write:    &ir.WriteSpec{},
	}
	_, err := Write(model(), q, nil)
	if err == nil || err.Code != "22P02" {
		t.Fatalf("a write filter operand is coerced too, want 22P02, got %v", err)
	}
}
