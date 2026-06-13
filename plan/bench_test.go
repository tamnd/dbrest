package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/schema"
)

// benchModel is the planning fixture with an embeddable edge, so the read
// benchmark exercises relation lookup, column resolution, and embed binding in
// one pass, the work Read does on every request.
func benchModel() *schema.Model {
	directors := &schema.Relation{
		Schema: "public", Name: "directors", Kind: schema.KindTable,
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "name", Type: "text", Position: 2},
		},
	}
	films := &schema.Relation{
		Schema: "public", Name: "films", Kind: schema.KindTable,
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "title", Type: "text", Position: 2},
			{Name: "year", Type: "integer", Position: 3},
			{Name: "director_id", Type: "integer", Position: 4},
		},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "films_director_id_fkey", Columns: []string{"director_id"},
			RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
		}},
	}
	return schema.NewModel([]*schema.Relation{directors, films})
}

// Read resolves names against the schema model once per request, between parse
// and compile. The benchmark plans a projection plus a to-one embed, the shape
// that drives relation lookup and embed binding together.
func BenchmarkReadPlan(b *testing.B) {
	m := benchModel()
	path := []string{"public"}
	newQuery := func() *ir.Query {
		return &ir.Query{
			Relation: ir.Ref{Schema: "public", Name: "films"},
			Select: []ir.SelectItem{
				ir.Column{Path: []string{"title"}},
				ir.EmbedRef{Index: 0},
			},
			Embeds: []ir.Embed{{
				OutKey: "director",
				Target: ir.Ref{Name: "directors"},
				Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}},
			}},
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		// A fresh query each iteration: Read binds resolved pointers onto it, so a
		// reused value would measure planning an already-planned query.
		if _, err := Read(m, newQuery(), path, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
