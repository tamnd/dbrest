package sqlgen

import (
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/schema"
)

// embedStub is the compiler stub with real JSON-assembly fragments, so an
// embedded read snapshots as the nested-document SQL it actually emits rather
// than collapsing the assembly to empty strings. The spellings are readable
// stand-ins, not any one engine's; the point is that the compiler splices them
// in the right places (spec 06 section 7: snapshot the fragment for a fixed
// plan, no database).
type embedStub struct{ stub }

func (embedStub) JSONObject(pairs []Pair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = "'" + p.Key + "', " + p.Value
	}
	return "json_object(" + strings.Join(parts, ", ") + ")"
}

func (embedStub) JSONAgg(expr, _ string) string {
	return "json_group_array(" + expr + ")"
}

// embedModel wires the canonical embedding fixture: films reference one director
// (forward FK, to-one), directors own many films (the reverse, to-many), and
// films relate to actors through the roles junction (many-to-many).
func embedModel() *schema.Model {
	cols := func(names ...string) []*schema.Column {
		out := make([]*schema.Column, len(names))
		for i, n := range names {
			out[i] = &schema.Column{Name: n, Type: "text", Position: i + 1}
		}
		return out
	}
	directors := &schema.Relation{Schema: "public", Name: "directors", Columns: cols("id", "name")}
	films := &schema.Relation{
		Schema:  "public",
		Name:    "films",
		Columns: cols("id", "title", "director_id"),
		ForeignKeys: []*schema.ForeignKey{{
			Name: "films_director_id_fkey", Columns: []string{"director_id"},
			RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
		}},
	}
	actors := &schema.Relation{Schema: "public", Name: "actors", Columns: cols("id", "name")}
	roles := &schema.Relation{
		Schema:  "public",
		Name:    "roles",
		Columns: cols("film_id", "actor_id"),
		ForeignKeys: []*schema.ForeignKey{
			{Name: "roles_film_id_fkey", Columns: []string{"film_id"}, RefSchema: "public", RefRelation: "films", RefColumns: []string{"id"}},
			{Name: "roles_actor_id_fkey", Columns: []string{"actor_id"}, RefSchema: "public", RefRelation: "actors", RefColumns: []string{"id"}},
		},
	}
	return schema.NewModel([]*schema.Relation{directors, films, actors, roles})
}

// relate resolves the single relationship from parent to target in the fixture,
// the same edge the planner would attach to an embed.
func relate(t *testing.T, m *schema.Model, parent, target string) *schema.Relationship {
	t.Helper()
	p, ok := m.Lookup(parent, []string{"public"})
	if !ok {
		t.Fatalf("parent %q not in model", parent)
	}
	cands, found := m.Relationships(p, target, []string{"public"})
	if !found || len(cands) != 1 {
		t.Fatalf("relate(%s, %s): found=%v candidates=%d, want exactly 1", parent, target, found, len(cands))
	}
	return &cands[0]
}

func compileEmbed(t *testing.T, q *ir.Query) *Statement {
	t.Helper()
	st, err := CompileRead(embedStub{}, q)
	if err != nil {
		t.Fatalf("CompileRead: %v", err)
	}
	return st
}

// A to-one embed (films -> director) is a scalar object subquery aliased to the
// out key, correlated on the parent's foreign key, with LIMIT 1.
func TestEmbedToOneObjectSubquery(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"title"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			OutKey: "director",
			Target: ir.Ref{Schema: "public", Name: "directors"},
			Rel:    relate(t, m, "films", "directors"),
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}},
		}},
	}
	want := `SELECT t0."title", (SELECT json_object('name', t1."name") ` +
		`FROM "public"."directors" t1 WHERE t1."id" = t0."director_id" LIMIT 1) ` +
		`AS "director" FROM "public"."films" t0`
	if got := compileEmbed(t, q).SQL; got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// A to-many embed (directors -> films) folds per-row objects into a JSON array
// through the dialect's JSONAgg over an inner subquery.
func TestEmbedToManyAggregatesArray(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"name"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			OutKey: "films",
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"title"}}}},
		}},
	}
	got := compileEmbed(t, q).SQL
	for _, want := range []string{
		`json_group_array(CAST(je."__e" AS json))`,
		`json_object('title', t1."title") AS "__e" FROM "public"."films" t1`,
		`WHERE t1."director_id" = t0."id"`,
		`) je) AS "films"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("array embed missing %q\n in %q", want, got)
		}
	}
}

// A many-to-many embed (films -> actors) crosses the roles junction inside the
// array subquery: JOIN the junction on the target key, filter it on the parent.
func TestEmbedManyToManyCrossesJunction(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"title"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			OutKey: "actors",
			Target: ir.Ref{Schema: "public", Name: "actors"},
			Rel:    relate(t, m, "films", "actors"),
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}},
		}},
	}
	got := compileEmbed(t, q).SQL
	for _, want := range []string{
		`FROM "public"."actors" t1 JOIN "public"."roles" j1`,
		`ON j1."actor_id" = t1."id"`,
		`WHERE j1."film_id" = t0."id"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("junction embed missing %q\n in %q", want, got)
		}
	}
}

// An !inner embed restricts the parent through an EXISTS over the relationship,
// so a parent row with no embedded match drops out (PostgREST inner-join).
func TestEmbedInnerAddsExists(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"name"}},
			ir.EmbedRef{Index: 0},
		},
		Embeds: []ir.Embed{{
			OutKey: "films",
			Join:   ir.JoinInner,
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"title"}}}},
		}},
	}
	got := compileEmbed(t, q).SQL
	// The EXISTS alias is x2: the array subquery already consumed t1 on the
	// shared alias counter, so the inner-join probe takes the next number.
	if !strings.Contains(got, `WHERE EXISTS (SELECT 1 FROM "public"."films" x2 WHERE x2."director_id" = t0."id")`) {
		t.Errorf("inner embed missing EXISTS predicate\n in %q", got)
	}
}

// An embed's own horizontal filter is ANDed onto the join predicate, bound, and
// qualified by the target alias.
func TestEmbedHorizontalFilterIsBound(t *testing.T) {
	m := embedModel()
	where := ir.Cond(ir.Compare{Path: []string{"name"}, Op: ir.OpEq, Value: ir.Value{Text: "Lynch"}})
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{ir.EmbedRef{Index: 0}},
		Embeds: []ir.Embed{{
			OutKey: "director",
			Target: ir.Ref{Schema: "public", Name: "directors"},
			Rel:    relate(t, m, "films", "directors"),
			Query:  ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}, Where: &where},
		}},
	}
	st := compileEmbed(t, q)
	if !strings.Contains(st.SQL, `AND (t1."name" = $1)`) {
		t.Errorf("embed filter not ANDed and qualified\n in %q", st.SQL)
	}
	if len(st.Args) != 1 || st.Args[0] != "Lynch" {
		t.Errorf("Args = %v, want [Lynch]", st.Args)
	}
}

// An empty embed projection takes every column of the target relation, in
// position order.
func TestEmbedStarProjectsAllColumns(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{ir.EmbedRef{Index: 0}},
		Embeds: []ir.Embed{{
			OutKey: "director",
			Target: ir.Ref{Schema: "public", Name: "directors"},
			Rel:    relate(t, m, "films", "directors"),
		}},
	}
	got := compileEmbed(t, q).SQL
	if !strings.Contains(got, `json_object('id', t1."id", 'name', t1."name")`) {
		t.Errorf("star embed did not project all columns\n in %q", got)
	}
}

// A spread embed is not yet lowered to SQL; it must report PGRST127 rather than
// emit something wrong.
func TestEmbedSpreadUnsupported(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{ir.EmbedRef{Index: 0}},
		Embeds: []ir.Embed{{
			OutKey: "director",
			Spread: true,
			Target: ir.Ref{Schema: "public", Name: "directors"},
			Rel:    relate(t, m, "films", "directors"),
		}},
	}
	if _, err := CompileRead(embedStub{}, q); err == nil || err.Code != "PGRST127" {
		t.Fatalf("spread embed err = %v, want PGRST127", err)
	}
}

// The embedded read is the compiler's most expensive shape, so it carries its
// own benchmark: a parent projection plus a to-one object and a to-many array.
func BenchmarkCompileReadEmbedded(b *testing.B) {
	m := embedModel()
	p, _ := m.Lookup("films", []string{"public"})
	toOne, _ := m.Relationships(p, "directors", []string{"public"})
	toMany, _ := m.Relationships(p, "actors", []string{"public"})
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"title"}},
			ir.EmbedRef{Index: 0},
			ir.EmbedRef{Index: 1},
		},
		Embeds: []ir.Embed{
			{OutKey: "director", Target: ir.Ref{Schema: "public", Name: "directors"}, Rel: &toOne[0],
				Query: ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}}},
			{OutKey: "actors", Target: ir.Ref{Schema: "public", Name: "actors"}, Rel: &toMany[0],
				Query: ir.Query{Select: []ir.SelectItem{ir.Column{Path: []string{"name"}}}}},
		},
	}
	d := embedStub{}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := CompileRead(d, q); err != nil {
			b.Fatal(err)
		}
	}
}
