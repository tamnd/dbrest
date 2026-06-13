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
		Schema:     "public",
		Name:       "roles",
		Columns:    cols("film_id", "actor_id"),
		PrimaryKey: []string{"film_id", "actor_id"}, // composite PK marks roles a junction
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

// films?actors=not.is.null filters the parent on the existence of a related
// actor: a semi-join, the same EXISTS an !inner embed adds, correlated to t0
// and crossing the roles junction (item 01.12).
func TestEmbedPredicateNotIsNullSemiJoin(t *testing.T) {
	m := embedModel()
	where := ir.Cond(ir.EmbedPredicate{Index: 0, Exists: true})
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "films"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"title"}}, ir.EmbedRef{Index: 0}},
		Where:    &where,
		Embeds: []ir.Embed{{
			OutKey: "actors",
			Target: ir.Ref{Schema: "public", Name: "actors"},
			Rel:    relate(t, m, "films", "actors"),
		}},
	}
	got := compileEmbed(t, q).SQL
	if strings.Contains(got, "NOT EXISTS") {
		t.Errorf("not.is.null should be a plain EXISTS, not anti-join\n in %q", got)
	}
	for _, want := range []string{
		`WHERE EXISTS (SELECT 1 FROM "public"."actors" x2`,
		`JOIN "public"."roles" xj2 ON xj2."actor_id" = x2."id"`,
		`WHERE xj2."film_id" = t0."id"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("semi-join missing %q\n in %q", want, got)
		}
	}
}

// films?actors=is.null is the anti-join complement: a parent with no related
// actor, lowered to NOT EXISTS over the same relationship (item 01.12).
func TestEmbedPredicateIsNullAntiJoin(t *testing.T) {
	m := embedModel()
	where := ir.Cond(ir.EmbedPredicate{Index: 0, Exists: false})
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"name"}}, ir.EmbedRef{Index: 0}},
		Where:    &where,
		Embeds: []ir.Embed{{
			OutKey: "films",
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
		}},
	}
	got := compileEmbed(t, q).SQL
	if !strings.Contains(got, `WHERE NOT EXISTS (SELECT 1 FROM "public"."films" x2 WHERE x2."director_id" = t0."id")`) {
		t.Errorf("is.null missing NOT EXISTS anti-join\n in %q", got)
	}
}

// The embed-existence predicate composes under or=(...): one disjunct is the
// semi-join EXISTS, the other an ordinary parent-column compare.
func TestEmbedPredicateInsideOr(t *testing.T) {
	m := embedModel()
	where := ir.Cond(ir.Or{Kids: []ir.Cond{
		ir.EmbedPredicate{Index: 0, Exists: true},
		ir.Compare{Path: []string{"name"}, Op: ir.OpEq, Value: ir.Value{Text: "Lynch"}},
	}})
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"name"}}, ir.EmbedRef{Index: 0}},
		Where:    &where,
		Embeds: []ir.Embed{{
			OutKey: "films",
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
		}},
	}
	got := compileEmbed(t, q).SQL
	if !strings.Contains(got, `WHERE (EXISTS (SELECT 1 FROM "public"."films" x2 WHERE x2."director_id" = t0."id") OR t0."name" = $1)`) {
		t.Errorf("or= with embed predicate not lowered as expected\n in %q", got)
	}
}

// A count over a query carrying an embed-existence filter correlates the EXISTS
// to the parent by its bare table name, since the count gives it no alias.
func TestEmbedPredicateInCount(t *testing.T) {
	m := embedModel()
	where := ir.Cond(ir.EmbedPredicate{Index: 0, Exists: true})
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Where:    &where,
		Embeds: []ir.Embed{{
			OutKey: "films",
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
		}},
	}
	st, err := CompileCount(embedStub{}, q)
	if err != nil {
		t.Fatalf("CompileCount: %v", err)
	}
	if !strings.Contains(st.SQL, `SELECT count(*) FROM "public"."directors" WHERE EXISTS (SELECT 1 FROM "public"."films" x1 WHERE x1."director_id" = "public"."directors"."id")`) {
		t.Errorf("count did not correlate embed EXISTS to the bare table\n in %q", st.SQL)
	}
}

// A count over a query carrying an !inner embed restricts the parent with the
// same EXISTS the row query adds, so an exact count matches the filtered body
// (item 07.7). The EXISTS correlates to the bare table name, since the count
// gives the parent no alias.
func TestCountAppliesInnerEmbedExists(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Embeds: []ir.Embed{{
			OutKey: "films",
			Join:   ir.JoinInner,
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
		}},
	}
	st, err := CompileCount(embedStub{}, q)
	if err != nil {
		t.Fatalf("CompileCount: %v", err)
	}
	want := `SELECT count(*) FROM "public"."directors" ` +
		`WHERE EXISTS (SELECT 1 FROM "public"."films" x1 ` +
		`WHERE x1."director_id" = "public"."directors"."id")`
	if st.SQL != want {
		t.Errorf("\n got %q\nwant %q", st.SQL, want)
	}
}

// A non-inner embed leaves the count unrestricted: only !inner embeds prune the
// parent, so a plain to-many embed adds no EXISTS to the count.
func TestCountIgnoresNonInnerEmbed(t *testing.T) {
	m := embedModel()
	q := &ir.Query{
		Relation: ir.Ref{Schema: "public", Name: "directors"},
		Embeds: []ir.Embed{{
			OutKey: "films",
			Target: ir.Ref{Schema: "public", Name: "films"},
			Rel:    relate(t, m, "directors", "films"),
		}},
	}
	st, err := CompileCount(embedStub{}, q)
	if err != nil {
		t.Fatalf("CompileCount: %v", err)
	}
	if strings.Contains(st.SQL, "WHERE") {
		t.Errorf("non-inner embed should add no predicate, got %q", st.SQL)
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
