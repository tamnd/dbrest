package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// A top-level order=rel(col) lowers to a correlated scalar subquery selecting the
// to-one embed's column, joined back to the parent: a parent with no related row
// yields NULL, which the dialect's NULLs placement then orders (item 07.6).
func TestRelatedOrderToOneSubquery(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"title"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			Cardinality: ir.CardToOne,
			OutKey:      "directors",
			Target:      ir.Ref{Schema: "public", Name: "directors"},
			Rel:         relate(t, m, "films", "directors"),
			Query:       ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}},
		}},
		// Match by written target name, since this embed has no alias.
		Order: []ir.OrderTerm{{Rel: "directors", Path: []string{"name"}}},
	}
	got := compileEmbed(t, q).SQL
	// The embed subquery consumes t1; the order subquery takes the next alias, o2.
	want := ` ORDER BY (SELECT o2."name" FROM "public"."directors" o2 ` +
		`WHERE o2."id" = t0."director_id") ASC NULLS LAST`
	if !strings.Contains(got, want) {
		t.Errorf("related order subquery missing\n want %q\n  in %q", want, got)
	}
}

// The embed an order term names is matched by its alias when one is given, so
// order=client(...) resolves through `client:clients(...)`.
func TestRelatedOrderMatchesAlias(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"title"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			Cardinality: ir.CardToOne,
			Alias:       "director",
			OutKey:      "director",
			Target:      ir.Ref{Schema: "public", Name: "directors"},
			Rel:         relate(t, m, "films", "directors"),
			Query:       ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}},
		}},
		Order: []ir.OrderTerm{{Rel: "director", Path: []string{"name"}, Desc: true}},
	}
	got := compileEmbed(t, q).SQL
	want := ` ORDER BY (SELECT o2."name" FROM "public"."directors" o2 ` +
		`WHERE o2."id" = t0."director_id") DESC NULLS FIRST`
	if !strings.Contains(got, want) {
		t.Errorf("aliased related order missing\n want %q\n  in %q", want, got)
	}
}

// nullsfirst/nullslast still apply to a related order: the parent's NULL (no
// related row) sorts where the client asks, not where the default lands.
func TestRelatedOrderHonorsNullsPlacement(t *testing.T) {
	m := embedModel()
	nf := true
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"title"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			Cardinality: ir.CardToOne,
			OutKey:      "directors",
			Target:      ir.Ref{Schema: "public", Name: "directors"},
			Rel:         relate(t, m, "films", "directors"),
			Query:       ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}},
		}},
		Order: []ir.OrderTerm{{Rel: "directors", Path: []string{"name"}, NullsFirst: &nf}},
	}
	got := compileEmbed(t, q).SQL
	if !strings.Contains(got, `ASC NULLS FIRST`) {
		t.Errorf("related order did not honor nullsfirst\n in %q", got)
	}
}
