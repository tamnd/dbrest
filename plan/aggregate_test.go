package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// aggQuery builds a films read whose projection carries the given select items.
func aggQuery(items ...ir.SelectItem) *ir.Query {
	return &ir.Query{
		Kind:     ir.Read,
		Relation: ir.Ref{Name: "films"},
		Select:   items,
	}
}

func TestAggregateGatedOffByDefault(t *testing.T) {
	q := aggQuery(ir.Aggregate{Func: ir.AggCount})
	_, err := Read(model(), q, nil, Options{}) // AggregatesEnabled defaults false
	if err == nil || err.Code != "PGRST123" {
		t.Fatalf("want PGRST123 with aggregates off, got %v", err)
	}
}

func TestAggregateAllowedWhenEnabled(t *testing.T) {
	q := aggQuery(
		ir.Column{Path: []string{"year"}},
		ir.Aggregate{Func: ir.AggSum, Arg: &ir.Column{Path: []string{"id"}}},
	)
	if _, err := Read(model(), q, nil, Options{AggregatesEnabled: true}); err != nil {
		t.Fatalf("unexpected error with aggregates on: %v", err)
	}
}

func TestAggregateArgColumnValidated(t *testing.T) {
	// nope is not a films column; even with aggregates enabled the arg is checked.
	q := aggQuery(ir.Aggregate{Func: ir.AggSum, Arg: &ir.Column{Path: []string{"nope"}}})
	_, err := Read(model(), q, nil, Options{AggregatesEnabled: true})
	// An aggregate over a column that does not exist reaches PostgreSQL: 42703
	// (item 04.5), not the schema-cache PGRST204.
	if err == nil || err.Code != "42703" {
		t.Fatalf("want 42703 for unknown aggregate column, got %v", err)
	}
}

func TestLegacyEmbedCountExemptFromGate(t *testing.T) {
	// A legacy bare count carried by an embed is allowed even with aggregates off.
	// It is validated through the embedded relation, so use a real relationship.
	q := &ir.Query{
		Kind:     ir.Read,
		Relation: ir.Ref{Name: "films"},
		Embeds: []ir.Embed{{
			Target: ir.Ref{Name: "directors"},
			OutKey: "directors",
			Query: ir.Query{
				Kind:     ir.Read,
				Relation: ir.Ref{Name: "directors"},
				Select:   []ir.SelectItem{ir.Aggregate{Func: ir.AggCount, Legacy: true}},
			},
		}},
	}
	if _, err := Read(nullEmbedModel(), q, []string{"public"}, Options{}); err != nil {
		t.Fatalf("legacy embed count should be exempt from the gate, got %v", err)
	}
}
