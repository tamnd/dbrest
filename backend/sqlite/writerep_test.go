package sqlite

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// A bulk update that asks for a representation with order+limit writes every
// matching row (v13 dropped limited update/delete) but returns only the ordered,
// limited slice. The affected count stays the full set, and a re-read confirms
// no row escaped the mutation.
func TestExecuteUpdateRepresentationOrderedLimited(t *testing.T) {
	b := openSeeded(t)
	limit := 2
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Update,
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"id"}, Desc: true}},
		Limit:    &limit,
		Write: &ir.WriteSpec{
			Return: ir.ReturnRepresentation,
			Set:    map[string]ir.Value{"rating": {JSON: "X"}},
		},
	})

	// The affected count is the full mutated set, not the limited body.
	if n, ok := res.Affected(); !ok || n != 4 {
		t.Errorf("Affected = %d,%v want 4,true", n, ok)
	}

	// The body is the top two rows by id descending.
	rows := readAll(t, res)
	if len(rows) != 2 {
		t.Fatalf("body rows = %d, want 2", len(rows))
	}
	if rows[0]["id"].(int64) != 4 || rows[1]["id"].(int64) != 3 {
		t.Errorf("body ids = [%v %v], want [4 3]", rows[0]["id"], rows[1]["id"])
	}

	// Every row was updated, including the two the representation omitted.
	all := execRead(t, b, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"rating"}}},
	})
	if len(all) != 4 {
		t.Fatalf("after update, count = %d, want 4", len(all))
	}
	for _, r := range all {
		if r["rating"] != "X" {
			t.Errorf("a row escaped the update: rating = %v, want X", r["rating"])
		}
	}
}

// offset on a delete representation skips rows in the returned body while still
// deleting every matching row.
func TestExecuteDeleteRepresentationOffset(t *testing.T) {
	b := openSeeded(t)
	offset := 1
	res := execWrite(t, b, &ir.Query{
		Kind:     ir.Delete,
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"id"}}},
		Offset:   &offset,
		Write:    &ir.WriteSpec{Return: ir.ReturnRepresentation},
	})

	if n, ok := res.Affected(); !ok || n != 4 {
		t.Errorf("Affected = %d,%v want 4,true", n, ok)
	}
	rows := readAll(t, res)
	if len(rows) != 3 {
		t.Fatalf("body rows = %d, want 3 (offset 1 of 4)", len(rows))
	}
	if rows[0]["id"].(int64) != 2 {
		t.Errorf("first body id = %v, want 2", rows[0]["id"])
	}
	// The whole table is gone regardless of the body window.
	if all := execRead(t, b, &ir.Query{Relation: ir.Ref{Name: "films"}}); len(all) != 0 {
		t.Errorf("after delete, count = %d, want 0", len(all))
	}
}
