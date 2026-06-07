package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/schema"
)

func model() *schema.Model {
	return schema.NewModel([]*schema.Relation{
		{Name: "films", Kind: schema.KindTable, Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "title", Type: "text", Position: 2},
			{Name: "year", Type: "integer", Position: 3},
		}},
	})
}

func TestReadResolvesRelation(t *testing.T) {
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Select: []ir.SelectItem{ir.Column{Path: []string{"title"}}}}
	p, err := Read(model(), q, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if p.Rel == nil || p.Rel.Name != "films" {
		t.Fatalf("relation not bound: %+v", p.Rel)
	}
	if !p.ReadOnly {
		t.Error("read plan should be ReadOnly")
	}
}

func TestReadUnknownTable(t *testing.T) {
	q := &ir.Query{Relation: ir.Ref{Name: "ghosts"}}
	_, err := Read(model(), q, nil)
	if err == nil || err.Code != "PGRST205" {
		t.Fatalf("want PGRST205, got %v", err)
	}
}

func TestReadUnknownColumnInSelect(t *testing.T) {
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Select: []ir.SelectItem{ir.Column{Path: []string{"bogus"}}}}
	_, err := Read(model(), q, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

func TestReadUnknownColumnInFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"missing"}, Op: ir.OpEq, Value: ir.Value{Text: "x"}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	_, err := Read(model(), q, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

func TestReadUnknownColumnInOrder(t *testing.T) {
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Order: []ir.OrderTerm{{Path: []string{"nope"}}}}
	_, err := Read(model(), q, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

func TestReadNestedLogicalColumnChecked(t *testing.T) {
	where := ir.Cond(ir.And{Kids: []ir.Cond{
		ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}},
		ir.Or{Kids: []ir.Cond{
			ir.Compare{Path: []string{"ghost"}, Op: ir.OpEq, Value: ir.Value{Text: "x"}},
		}},
	}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	_, err := Read(model(), q, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("nested unknown column should be caught, got %v", err)
	}
}
