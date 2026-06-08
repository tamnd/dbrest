package mongo

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// pipe builds a read pipeline and fails on an unexpected error.
func pipe(t *testing.T, q *ir.Query) Arr {
	t.Helper()
	p, err := BuildReadPipeline(q)
	if err != nil {
		t.Fatalf("BuildReadPipeline: %v", err)
	}
	return p
}

func selCol(name string) ir.Column { return ir.Column{Path: []string{name}} }

func TestReadPipelineSnapshot(t *testing.T) {
	where := ir.Cond(ir.Compare{Path: []string{"age"}, Op: ir.OpGte, Value: ir.Value{Text: "18"}})
	offset, limit := 40, 20
	got := jsonOf(t, pipe(t, &ir.Query{
		Relation: ir.Ref{Name: "people"},
		Select:   []ir.SelectItem{selCol("_id"), selCol("name"), selCol("age")},
		Where:    &where,
		Order:    []ir.OrderTerm{{Path: []string{"name"}}},
		Offset:   &offset,
		Limit:    &limit,
	}))
	want := `[{"$match":{"age":{"$gte":"18"}}},` +
		`{"$sort":{"name":1}},` +
		`{"$skip":40},` +
		`{"$limit":20},` +
		`{"$project":{"_id":1,"name":1,"age":1}}]`
	if got != want {
		t.Errorf("pipeline =\n  %s\nwant\n  %s", got, want)
	}
}

func TestReadPipelineBareSelect(t *testing.T) {
	// No select projects nothing; the whole document passes through.
	got := jsonOf(t, pipe(t, &ir.Query{Relation: ir.Ref{Name: "people"}}))
	if want := `[]`; got != want {
		t.Errorf("bare pipeline = %s, want %s", got, want)
	}
}

func TestReadPipelineDescSort(t *testing.T) {
	got := jsonOf(t, pipe(t, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"year"}, Desc: true}},
	}))
	if want := `[{"$sort":{"year":-1}}]`; got != want {
		t.Errorf("desc sort = %s, want %s", got, want)
	}
}

func TestReadPipelineNullsOrdering(t *testing.T) {
	// An explicit nullsfirst adds a 0/1 rank field and sorts on it first.
	first := true
	got := jsonOf(t, pipe(t, &ir.Query{
		Relation: ir.Ref{Name: "films"},
		Order:    []ir.OrderTerm{{Path: []string{"rating"}, Desc: true, NullsFirst: &first}},
	}))
	want := `[{"$addFields":{"__dbrest_nulls_0":{"$cond":[{"$eq":["$rating",null]},0,1]}}},` +
		`{"$sort":{"__dbrest_nulls_0":1,"rating":-1}}]`
	if got != want {
		t.Errorf("nulls first =\n  %s\nwant\n  %s", got, want)
	}
}

func TestReadPipelineProjectionAliasAndDotted(t *testing.T) {
	got := jsonOf(t, pipe(t, &ir.Query{
		Relation: ir.Ref{Name: "people"},
		Select: []ir.SelectItem{
			ir.Column{Path: []string{"full_name"}, Alias: "name"},
			ir.Column{Path: []string{"address", "city"}},
		},
	}))
	// A rename projects {alias: "$path"}; a dotted JSON sub-path projects under its
	// last element from the dotted source.
	want := `[{"$project":{"name":"$full_name","city":"$address.city"}}]`
	if got != want {
		t.Errorf("projection = %s, want %s", got, want)
	}
}

func TestReadPipelineCast(t *testing.T) {
	got := jsonOf(t, pipe(t, &ir.Query{
		Relation: ir.Ref{Name: "people"},
		Select:   []ir.SelectItem{ir.Column{Path: []string{"age"}, Cast: "text", Alias: "a"}},
	}))
	if want := `[{"$project":{"a":{"$toString":"$age"}}}]`; got != want {
		t.Errorf("cast = %s, want %s", got, want)
	}
}

func TestConvertOps(t *testing.T) {
	cases := map[string]string{
		"int":     "$toInt",
		"integer": "$toInt",
		"bigint":  "$toLong",
		"float8":  "$toDouble",
		"numeric": "$toDecimal",
		"bool":    "$toBool",
		"date":    "$toDate",
		"text":    "$toString",
		"uuid":    "$toString",
	}
	for canonical, want := range cases {
		if got := convertOp(canonical); got != want {
			t.Errorf("convertOp(%q) = %q, want %q", canonical, got, want)
		}
	}
}

func TestReadPipelineUnsupportedSelect(t *testing.T) {
	// An aggregate select item is the deferred aggregate slice's; the read
	// projection rejects it rather than dropping it silently.
	_, err := BuildReadPipeline(&ir.Query{
		Relation: ir.Ref{Name: "people"},
		Select:   []ir.SelectItem{ir.Aggregate{Func: ir.AggCount}},
	})
	if err == nil || err.Code != "PGRST127" {
		t.Errorf("aggregate select err = %v, want PGRST127", err)
	}
}

func BenchmarkBuildReadPipeline(b *testing.B) {
	where := ir.Cond(ir.Compare{Path: []string{"age"}, Op: ir.OpGte, Value: ir.Value{Text: "18"}})
	limit := 20
	q := &ir.Query{
		Relation: ir.Ref{Name: "people"},
		Select:   []ir.SelectItem{selCol("_id"), selCol("name"), selCol("age")},
		Where:    &where,
		Order:    []ir.OrderTerm{{Path: []string{"name"}}},
		Limit:    &limit,
	}
	for b.Loop() {
		if _, err := BuildReadPipeline(q); err != nil {
			b.Fatal(err)
		}
	}
}
