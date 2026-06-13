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
	p, err := Read(model(), q, nil, Options{})
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
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "PGRST205" {
		t.Fatalf("want PGRST205, got %v", err)
	}
}

func TestReadUnknownColumnInSelect(t *testing.T) {
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Select: []ir.SelectItem{ir.Column{Path: []string{"bogus"}}}}
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

func TestReadUnknownColumnInFilter(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"missing"}, Op: ir.OpEq, Value: ir.Value{Text: "x"}})
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Where: &where}
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

func TestReadUnknownColumnInOrder(t *testing.T) {
	q := &ir.Query{Relation: ir.Ref{Name: "films"}, Order: []ir.OrderTerm{{Path: []string{"nope"}}}}
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
	}
}

// The planner stamps an eq/neq filter with its column's canonical type so the
// compiler can decide whether "true"/"false" binds as a boolean (item 07.4).
func TestReadStampsColumnTypeOnEq(t *testing.T) {
	m := schema.NewModel([]*schema.Relation{
		{Name: "flags", Kind: schema.KindTable, Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "done", Type: "bool", Position: 2},
			{Name: "label", Type: "text", Position: 3},
		}},
	})
	where := ir.Cond(ir.And{Kids: []ir.Cond{
		ir.Compare{Path: []string{"done"}, Op: ir.OpEq, Value: ir.Value{Text: "true"}},
		ir.Compare{Path: []string{"label"}, Op: ir.OpNeq, Value: ir.Value{Text: "x"}},
	}})
	q := &ir.Query{Relation: ir.Ref{Name: "flags"}, Where: &where}
	p, err := Read(m, q, nil, Options{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	kids := (*p.Query.Where).(ir.And).Kids
	if ct := kids[0].(ir.Compare).ColumnType; ct != "bool" {
		t.Errorf("done ColumnType = %q, want bool", ct)
	}
	if ct := kids[1].(ir.Compare).ColumnType; ct != "text" {
		t.Errorf("label ColumnType = %q, want text", ct)
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
	_, err := Read(model(), q, nil, Options{})
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("nested unknown column should be caught, got %v", err)
	}
}

func modelPK() *schema.Model {
	return schema.NewModel([]*schema.Relation{
		{Name: "films", Kind: schema.KindTable, PrimaryKey: []string{"id"}, Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "title", Type: "text", Position: 2},
			{Name: "year", Type: "integer", Position: 3},
		}},
	})
}

func TestWriteResolvesRelation(t *testing.T) {
	q := &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write:    &ir.WriteSpec{Columns: []string{"title"}, Rows: []map[string]ir.Value{{"title": {JSON: "X"}}}},
	}
	p, err := Write(model(), q, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if p.Rel == nil || p.Rel.Name != "films" {
		t.Fatalf("relation not bound: %+v", p.Rel)
	}
	if p.ReadOnly {
		t.Error("write plan should not be ReadOnly")
	}
}

func TestWriteUnknownTable(t *testing.T) {
	q := &ir.Query{Kind: ir.Insert, Relation: ir.Ref{Name: "ghosts"}, Write: &ir.WriteSpec{}}
	_, err := Write(model(), q, nil)
	if err == nil || err.Code != "PGRST205" {
		t.Fatalf("want PGRST205, got %v", err)
	}
}

func TestWriteUnknownInsertColumn(t *testing.T) {
	q := &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Write:    &ir.WriteSpec{Columns: []string{"bogus"}, Rows: []map[string]ir.Value{{"bogus": {JSON: "X"}}}},
	}
	_, err := Write(model(), q, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204 for unknown insert column, got %v", err)
	}
}

func TestWriteUnknownUpdateColumn(t *testing.T) {
	q := &ir.Query{
		Kind:     ir.Update,
		Relation: ir.Ref{Name: "films"},
		Write:    &ir.WriteSpec{Set: map[string]ir.Value{"bogus": {JSON: "X"}}},
	}
	_, err := Write(model(), q, nil)
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204 for unknown update column, got %v", err)
	}
}

func TestWriteUpsertDefaultsConflictToPK(t *testing.T) {
	q := &ir.Query{
		Kind:     ir.Upsert,
		Relation: ir.Ref{Name: "films"},
		Write: &ir.WriteSpec{
			Columns:  []string{"id"},
			Rows:     []map[string]ir.Value{{"id": {JSON: "1"}}},
			Conflict: &ir.Conflict{Resolution: ir.ConflictMerge},
		},
	}
	_, err := Write(modelPK(), q, nil)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := q.Write.Conflict.Target; len(got) != 1 || got[0] != "id" {
		t.Errorf("conflict target = %v, want the primary key [id]", got)
	}
}

// putQuery builds a PUT upsert addressing id=eq.<idFilter> with a single-object
// body carrying id=<idBody>, plus optional extra filters and limit.
func putQuery(idFilter, idBody string, extra *ir.Cond, limit *int) *ir.Query {
	eq := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpEq, Value: ir.Value{Text: idFilter}})
	where := eq
	if extra != nil {
		where = ir.And{Kids: []ir.Cond{eq, *extra}}
	}
	return &ir.Query{
		Kind:     ir.Upsert,
		IsPut:    true,
		Relation: ir.Ref{Name: "films"},
		Where:    &where,
		Limit:    limit,
		Write: &ir.WriteSpec{
			Columns:  []string{"id", "title"},
			Rows:     []map[string]ir.Value{{"id": {JSON: idBody}, "title": {JSON: "X"}}},
			Conflict: &ir.Conflict{},
		},
	}
}

func TestWritePutHappyPath(t *testing.T) {
	q := putQuery("1", "1", nil, nil)
	if _, err := Write(modelPK(), q, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestWritePutPartialKeyIs405(t *testing.T) {
	// A non-eq filter on the key column is not a valid PUT addressing.
	where := ir.Cond(ir.Compare{Path: []string{"id"}, Op: ir.OpGt, Value: ir.Value{Text: "1"}})
	q := &ir.Query{
		Kind: ir.Upsert, IsPut: true, Relation: ir.Ref{Name: "films"}, Where: &where,
		Write: &ir.WriteSpec{Columns: []string{"id"}, Rows: []map[string]ir.Value{{"id": {JSON: float64(1)}}}, Conflict: &ir.Conflict{}},
	}
	_, err := Write(modelPK(), q, nil)
	if err == nil || err.Code != "PGRST105" {
		t.Fatalf("want PGRST105, got %v", err)
	}
}

func TestWritePutExtraFilterIs405(t *testing.T) {
	extra := ir.Cond(ir.Compare{Path: []string{"title"}, Op: ir.OpEq, Value: ir.Value{Text: "X"}})
	_, err := Write(modelPK(), putQuery("1", "1", &extra, nil), nil)
	if err == nil || err.Code != "PGRST105" {
		t.Fatalf("want PGRST105 for a non-PK filter, got %v", err)
	}
}

func TestWritePutLimitIs400(t *testing.T) {
	lim := 1
	_, err := Write(modelPK(), putQuery("1", "1", nil, &lim), nil)
	if err == nil || err.Code != "PGRST114" {
		t.Fatalf("want PGRST114, got %v", err)
	}
}

func TestWritePutPayloadMismatchIs400(t *testing.T) {
	// URL says id=eq.999, body says id=1: the keys disagree.
	_, err := Write(modelPK(), putQuery("999", "1", nil, nil), nil)
	if err == nil || err.Code != "PGRST115" {
		t.Fatalf("want PGRST115, got %v", err)
	}
}

func TestWritePutMultiRowIs400(t *testing.T) {
	q := putQuery("1", "1", nil, nil)
	q.Write.Rows = append(q.Write.Rows, map[string]ir.Value{"id": {JSON: float64(1)}, "title": {JSON: "Y"}})
	_, err := Write(modelPK(), q, nil)
	if err == nil || err.Code != "PGRST115" {
		t.Fatalf("want PGRST115 for a multi-row PUT body, got %v", err)
	}
}

// embedModel wires directors (one) <- films (many) through a forward FK so an
// embed of films on directors resolves to a single relationship.
func nullEmbedModel() *schema.Model {
	directors := &schema.Relation{Schema: "public", Name: "directors", Kind: schema.KindTable, Columns: []*schema.Column{
		{Name: "id", Type: "integer", Position: 1},
		{Name: "name", Type: "text", Position: 2},
	}}
	films := &schema.Relation{Schema: "public", Name: "films", Kind: schema.KindTable, Columns: []*schema.Column{
		{Name: "id", Type: "integer", Position: 1},
		{Name: "title", Type: "text", Position: 2},
		{Name: "director_id", Type: "integer", Position: 3},
	}, ForeignKeys: []*schema.ForeignKey{{
		Name: "films_director_id_fkey", Columns: []string{"director_id"},
		RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
	}}}
	return schema.NewModel([]*schema.Relation{directors, films})
}

// A filter naming an embed (directors?films=not.is.null) is reclassified into an
// EmbedPredicate before column validation, rather than being rejected as an
// unknown parent column (item 01.12). not.is.null sets Exists.
func TestReadReclassifiesEmbedNullFilter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		negate bool
		exists bool
	}{
		{"not.is.null is a semi-join", true, true},
		{"is.null is an anti-join", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			where := ir.Cond(ir.Compare{Path: []string{"films"}, Op: ir.OpIs, Value: ir.Value{Text: "null"}, Negate: tc.negate})
			q := &ir.Query{
				Relation: ir.Ref{Name: "directors"},
				Select:   []ir.SelectItem{ir.EmbedRef{Index: 0}},
				Where:    &where,
				Embeds:   []ir.Embed{{Target: ir.Ref{Name: "films"}, OutKey: "films"}},
			}
			if _, err := Read(nullEmbedModel(), q, []string{"public"}, Options{}); err != nil {
				t.Fatalf("Read: %v", err)
			}
			pred, ok := (*q.Where).(ir.EmbedPredicate)
			if !ok {
				t.Fatalf("Where = %T, want ir.EmbedPredicate", *q.Where)
			}
			if pred.Index != 0 || pred.Exists != tc.exists {
				t.Errorf("predicate = %+v, want Index 0 Exists %v", pred, tc.exists)
			}
		})
	}
}

// A null filter naming a real parent column (not an embed) stays an ordinary
// Compare and is column-validated as usual.
func TestReadEmbedNullReclassifyLeavesColumns(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"title"}, Op: ir.OpIs, Value: ir.Value{Text: "null"}, Negate: true})
	q := &ir.Query{
		Relation: ir.Ref{Name: "directors"},
		Select:   []ir.SelectItem{ir.EmbedRef{Index: 0}},
		Where:    &where,
		Embeds:   []ir.Embed{{Target: ir.Ref{Name: "films"}, OutKey: "films"}},
	}
	// title is not a directors column; the filter is a Compare, so column
	// validation rejects it rather than mistaking it for an embed predicate.
	_, err := Read(nullEmbedModel(), q, []string{"public"}, Options{})
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204 for unknown column, got %v", err)
	}
}

// A write whose select embeds a resource with no relationship is the read
// path's PGRST200 rather than silently dropping the embed (item 01.19).
func TestWriteResolvesEmbedsRejectsUnknown(t *testing.T) {
	q := &ir.Query{
		Kind:     ir.Insert,
		Relation: ir.Ref{Name: "films"},
		Select:   []ir.SelectItem{ir.EmbedRef{Index: 0}},
		Embeds:   []ir.Embed{{Target: ir.Ref{Name: "ghosts"}, OutKey: "ghosts"}},
		Write:    &ir.WriteSpec{Columns: []string{"title"}, Rows: []map[string]ir.Value{{"title": {JSON: "X"}}}, Return: ir.ReturnRepresentation},
	}
	_, err := Write(model(), q, nil)
	if err == nil || err.Code != "PGRST200" {
		t.Fatalf("want PGRST200 for an unknown write embed, got %v", err)
	}
}
