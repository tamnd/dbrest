package ir

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustRead(t *testing.T, rawQuery string) *Query {
	t.Helper()
	q, err := ParseRead("films", rawQuery, nil)
	if err != nil {
		t.Fatalf("ParseRead(%q) error: %v", rawQuery, err)
	}
	return q
}

func TestParseSelectColumns(t *testing.T) {
	q := mustRead(t, "select=title,year")
	if len(q.Select) != 2 {
		t.Fatalf("got %d select items", len(q.Select))
	}
	c0 := q.Select[0].(Column)
	if c0.Name() != "title" {
		t.Errorf("item0 = %q", c0.Name())
	}
}

func TestParseSelectAliasAndCast(t *testing.T) {
	q := mustRead(t, "select=t:title::text")
	c := q.Select[0].(Column)
	if c.Alias != "t" || c.Cast != "text" || !reflect.DeepEqual(c.Path, []string{"title"}) {
		t.Errorf("alias/cast/path = %q/%q/%v", c.Alias, c.Cast, c.Path)
	}
	if c.Name() != "t" {
		t.Errorf("Name = %q, want alias t", c.Name())
	}
}

func TestParseSelectJSONPath(t *testing.T) {
	q := mustRead(t, "select=data->meta->>id")
	c := q.Select[0].(Column)
	if !reflect.DeepEqual(c.Path, []string{"data", "meta", "id"}) {
		t.Errorf("path = %v", c.Path)
	}
	if c.Last != JSONArrow2 {
		t.Errorf("last = %v, want JSONArrow2", c.Last)
	}
}

func TestParseEmbed(t *testing.T) {
	q := mustRead(t, "select=title,director(name,bio)")
	if len(q.Embeds) != 1 {
		t.Fatalf("embeds = %d", len(q.Embeds))
	}
	if _, ok := q.Select[1].(EmbedRef); !ok {
		t.Errorf("item1 should be EmbedRef, got %T", q.Select[1])
	}
	emb := q.Embeds[0]
	if emb.Target.Name != "director" {
		t.Errorf("embed target = %q", emb.Target.Name)
	}
	if len(emb.Query.Select) != 2 {
		t.Errorf("nested select = %d", len(emb.Query.Select))
	}
}

func TestParseEmbedInnerHint(t *testing.T) {
	q := mustRead(t, "select=director!inner(name)")
	if q.Embeds[0].Join != JoinInner {
		t.Errorf("join = %v, want inner", q.Embeds[0].Join)
	}
}

func TestParseFiltersAnded(t *testing.T) {
	q := mustRead(t, "rating=gte.4&year=lt.2000")
	and, ok := (*q.Where).(And)
	if !ok {
		t.Fatalf("top filter should be And, got %T", *q.Where)
	}
	if len(and.Kids) != 2 {
		t.Fatalf("and kids = %d", len(and.Kids))
	}
	// keys are sorted: rating then year
	c0 := and.Kids[0].(Compare)
	if c0.Path[0] != "rating" || c0.Op != OpGte || c0.Value.Text != "4" {
		t.Errorf("kid0 = %+v", c0)
	}
}

func TestParseNotPrefix(t *testing.T) {
	q := mustRead(t, "rating=not.eq.5")
	c := (*q.Where).(Compare)
	if !c.Negate || c.Op != OpEq || c.Value.Text != "5" {
		t.Errorf("negate/op/val = %v/%v/%q", c.Negate, c.Op, c.Value.Text)
	}
}

func TestParseInList(t *testing.T) {
	q := mustRead(t, `id=in.(1,2,"3,4")`)
	c := (*q.Where).(Compare)
	if c.Op != OpIn {
		t.Fatalf("op = %v", c.Op)
	}
	want := []string{"1", "2", "3,4"}
	if !reflect.DeepEqual(c.Value.List, want) {
		t.Errorf("list = %v, want %v", c.Value.List, want)
	}
}

func TestParseIs(t *testing.T) {
	q := mustRead(t, "deleted=is.null")
	c := (*q.Where).(Compare)
	if c.Op != OpIs || c.Value.Text != "null" {
		t.Errorf("op/val = %v/%q", c.Op, c.Value.Text)
	}
	if _, err := ParseRead("films", "deleted=is.maybe", nil); err == nil {
		t.Error("is.maybe should be a parse error")
	}
}

func TestParseQuantifier(t *testing.T) {
	q := mustRead(t, "tags=eq(any).{a}")
	c := (*q.Where).(Compare)
	if c.Quant != QAny || c.Op != OpEq {
		t.Errorf("quant/op = %v/%v", c.Quant, c.Op)
	}
}

func TestParseLogicalOr(t *testing.T) {
	q := mustRead(t, "or=(rating.gte.4,year.lt.2000)")
	or, ok := (*q.Where).(Or)
	if !ok {
		t.Fatalf("want Or, got %T", *q.Where)
	}
	if len(or.Kids) != 2 {
		t.Fatalf("or kids = %d", len(or.Kids))
	}
}

func TestParseLogicalNested(t *testing.T) {
	q := mustRead(t, "and=(rating.gte.4,or(year.lt.2000,year.gt.2020))")
	and := (*q.Where).(And)
	if len(and.Kids) != 2 {
		t.Fatalf("and kids = %d", len(and.Kids))
	}
	if _, ok := and.Kids[1].(Or); !ok {
		t.Errorf("nested kid should be Or, got %T", and.Kids[1])
	}
}

func TestParseOrder(t *testing.T) {
	q := mustRead(t, "order=year.desc.nullsfirst,title")
	if len(q.Order) != 2 {
		t.Fatalf("order terms = %d", len(q.Order))
	}
	t0 := q.Order[0]
	if t0.Path[0] != "year" || !t0.Desc || t0.NullsFirst == nil || !*t0.NullsFirst {
		t.Errorf("term0 = %+v", t0)
	}
	if q.Order[1].Desc {
		t.Error("term1 should default to asc")
	}
}

func TestParseLimitOffset(t *testing.T) {
	q := mustRead(t, "limit=10&offset=20")
	if q.Limit == nil || *q.Limit != 10 || q.Offset == nil || *q.Offset != 20 {
		t.Errorf("limit/offset = %v/%v", q.Limit, q.Offset)
	}
	if _, err := ParseRead("films", "limit=-1", nil); err == nil {
		t.Error("negative limit should error")
	}
	if _, err := ParseRead("films", "limit=abc", nil); err == nil {
		t.Error("non-numeric limit should error")
	}
}

func TestParseBadOperator(t *testing.T) {
	if _, err := ParseRead("films", "rating=zz.4", nil); err == nil {
		t.Error("unknown operator should error PGRST100")
	} else if err.Code != "PGRST100" {
		t.Errorf("code = %s, want PGRST100", err.Code)
	}
}

func TestParsePreferCount(t *testing.T) {
	q := mustRead(t, "")
	_ = q
	q2, err := ParseRead("films", "", []string{"count=exact"})
	if err != nil {
		t.Fatal(err)
	}
	if q2.Count != CountExact {
		t.Errorf("count = %v, want exact", q2.Count)
	}
}

func TestParseWriteInsertSingle(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", nil, "", []byte(`{"title":"Dune","year":2021}`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if q.Kind != Insert {
		t.Errorf("Kind = %v, want Insert", q.Kind)
	}
	if len(q.Write.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(q.Write.Rows))
	}
	// Columns are the sorted keys of the first object.
	if got := q.Write.Columns; len(got) != 2 || got[0] != "title" || got[1] != "year" {
		t.Errorf("columns = %v, want [title year]", got)
	}
	// A JSON number is carried as json.Number to preserve integer precision.
	if v := q.Write.Rows[0]["year"].JSON; v != json.Number("2021") {
		t.Errorf("year value = %#v, want json.Number 2021", v)
	}
}

func TestParseWriteInsertArray(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", nil, "", []byte(`[{"title":"A"},{"title":"B"}]`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if len(q.Write.Rows) != 2 {
		t.Errorf("rows = %d, want 2", len(q.Write.Rows))
	}
}

func TestParseWriteColumnsParam(t *testing.T) {
	// The explicit columns= parameter overrides the inferred set.
	q, err := ParseWrite(Insert, "films", "columns=title", nil, "", []byte(`{"title":"Dune","year":2021}`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if got := q.Write.Columns; len(got) != 1 || got[0] != "title" {
		t.Errorf("columns = %v, want [title]", got)
	}
}

func TestParseWriteUpdate(t *testing.T) {
	q, err := ParseWrite(Update, "films", "id=eq.2", nil, "", []byte(`{"rating":"PG"}`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if q.Kind != Update {
		t.Errorf("Kind = %v, want Update", q.Kind)
	}
	if len(q.Write.Set) != 1 || q.Write.Set["rating"].JSON != "PG" {
		t.Errorf("set = %v", q.Write.Set)
	}
	if q.Where == nil {
		t.Error("update should carry the filter as WHERE")
	}
}

func TestParseWriteUpsertViaResolution(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", []string{"resolution=merge-duplicates"}, "", []byte(`{"id":1,"title":"X"}`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if q.Kind != Upsert {
		t.Errorf("Kind = %v, want Upsert (resolution promotes insert)", q.Kind)
	}
	if q.Write.Conflict == nil || q.Write.Conflict.Resolution != ConflictMerge {
		t.Errorf("conflict = %#v", q.Write.Conflict)
	}
}

func TestParseWriteOnConflictTarget(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "on_conflict=id", nil, "", []byte(`{"id":1}`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if q.Kind != Upsert {
		t.Errorf("on_conflict should make it an upsert, got %v", q.Kind)
	}
	if got := q.Write.Conflict.Target; len(got) != 1 || got[0] != "id" {
		t.Errorf("conflict target = %v, want [id]", got)
	}
}

func TestParseWriteReturnAndMissing(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", []string{"return=representation", "missing=null"}, "", []byte(`{"title":"X"}`))
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if q.Write.Return != ReturnRepresentation {
		t.Errorf("return = %v, want representation", q.Write.Return)
	}
	if q.Write.Missing != MissingNull {
		t.Errorf("missing = %v, want null", q.Write.Missing)
	}
}

func TestParseWriteBadJSON(t *testing.T) {
	if _, err := ParseWrite(Insert, "films", "", nil, "", []byte(`{not json`)); err == nil {
		t.Error("malformed JSON body should error PGRST100")
	}
}

func TestParseWriteDeleteNoBody(t *testing.T) {
	q, err := ParseWrite(Delete, "films", "id=eq.1", nil, "", nil)
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	if q.Kind != Delete || q.Where == nil {
		t.Errorf("delete = %#v", q)
	}
}
