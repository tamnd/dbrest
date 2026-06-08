package mongo

import (
	"encoding/json"
	"testing"

	"github.com/tamnd/dbrest/ir"
)

// jsonOf marshals a lowered document to its ordered JSON form, the database-free
// snapshot spec 06 section 7 prescribes for a dialect and that this backend
// reuses for its query-document lowering.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// lower lowers a condition and fails the test on an unexpected error.
func lower(t *testing.T, c ir.Cond) Doc {
	t.Helper()
	d, err := LowerFilter(c)
	if err != nil {
		t.Fatalf("LowerFilter: %v", err)
	}
	return d
}

func cmp(path string, op ir.Op, text string) ir.Compare {
	return ir.Compare{Path: []string{path}, Op: op, Value: ir.Value{Text: text}}
}

func TestComparisonOperators(t *testing.T) {
	cases := []struct {
		op   ir.Op
		want string
	}{
		{ir.OpEq, `{"year":{"$eq":"2000"}}`},
		{ir.OpNeq, `{"year":{"$ne":"2000"}}`},
		{ir.OpGt, `{"year":{"$gt":"2000"}}`},
		{ir.OpGte, `{"year":{"$gte":"2000"}}`},
		{ir.OpLt, `{"year":{"$lt":"2000"}}`},
		{ir.OpLte, `{"year":{"$lte":"2000"}}`},
	}
	for _, c := range cases {
		got := jsonOf(t, lower(t, cmp("year", c.op, "2000")))
		if got != c.want {
			t.Errorf("op %d = %s, want %s", c.op, got, c.want)
		}
	}
}

func TestInOperator(t *testing.T) {
	c := ir.Compare{Path: []string{"rating"}, Op: ir.OpIn, Value: ir.Value{List: []string{"PG", "G"}}}
	got := jsonOf(t, lower(t, c))
	want := `{"rating":{"$in":["PG","G"]}}`
	if got != want {
		t.Errorf("in = %s, want %s", got, want)
	}
}

func TestIsStates(t *testing.T) {
	cases := map[string]string{
		"null":     `{"deleted":{"$eq":null}}`,
		"not_null": `{"deleted":{"$ne":null}}`,
		"true":     `{"deleted":{"$eq":true}}`,
		"false":    `{"deleted":{"$eq":false}}`,
		"unknown":  `{"deleted":{"$eq":null}}`,
	}
	for state, want := range cases {
		got := jsonOf(t, lower(t, cmp("deleted", ir.OpIs, state)))
		if got != want {
			t.Errorf("is.%s = %s, want %s", state, got, want)
		}
	}
}

func TestLikeToRegex(t *testing.T) {
	// % becomes .*, _ becomes ., the literal dot is escaped, and the pattern is
	// anchored. ilike carries $options: "i".
	got := jsonOf(t, lower(t, cmp("title", ir.OpLike, "a_b%.c")))
	want := `{"title":{"$regex":"^a.b.*\\.c$"}}`
	if got != want {
		t.Errorf("like = %s, want %s", got, want)
	}
	got = jsonOf(t, lower(t, cmp("title", ir.OpILike, "x%")))
	want = `{"title":{"$regex":"^x.*$","$options":"i"}}`
	if got != want {
		t.Errorf("ilike = %s, want %s", got, want)
	}
}

func TestMatchOperators(t *testing.T) {
	// match passes the POSIX pattern through unanchored; imatch adds $options.
	got := jsonOf(t, lower(t, cmp("title", ir.OpMatch, "^bl")))
	if want := `{"title":{"$regex":"^bl"}}`; got != want {
		t.Errorf("match = %s, want %s", got, want)
	}
	got = jsonOf(t, lower(t, cmp("title", ir.OpIMatch, "^bl")))
	if want := `{"title":{"$regex":"^bl","$options":"i"}}`; got != want {
		t.Errorf("imatch = %s, want %s", got, want)
	}
}

func TestIsDistinct(t *testing.T) {
	got := jsonOf(t, lower(t, cmp("status", ir.OpIsDistinct, "active")))
	want := `{"$expr":{"$ne":[{"$ifNull":["$status",null]},"active"]}}`
	if got != want {
		t.Errorf("isdistinct = %s, want %s", got, want)
	}
}

func TestFullTextSearch(t *testing.T) {
	// A phrase keeps order in quotes; the web and plain forms pass terms through.
	phrase := ir.Compare{Path: []string{"body"}, Op: ir.OpFTS, FTS: ir.FTSPhrase, Value: ir.Value{Text: "quick brown"}}
	if got := jsonOf(t, lower(t, phrase)); got != `{"$text":{"$search":"\"quick brown\""}}` {
		t.Errorf("phfts = %s", got)
	}
	web := ir.Compare{Path: []string{"body"}, Op: ir.OpFTS, FTS: ir.FTSWeb, Value: ir.Value{Text: "cat dog"}}
	if got := jsonOf(t, lower(t, web)); got != `{"$text":{"$search":"cat dog"}}` {
		t.Errorf("wfts = %s", got)
	}
}

func TestLogicalGroups(t *testing.T) {
	and := ir.And{Kids: []ir.Cond{cmp("year", ir.OpGte, "2000"), cmp("rating", ir.OpEq, "PG")}}
	got := jsonOf(t, lower(t, and))
	want := `{"$and":[{"year":{"$gte":"2000"}},{"rating":{"$eq":"PG"}}]}`
	if got != want {
		t.Errorf("and = %s, want %s", got, want)
	}
	or := ir.Or{Kids: []ir.Cond{cmp("a", ir.OpEq, "1"), cmp("b", ir.OpEq, "2")}}
	got = jsonOf(t, lower(t, or))
	want = `{"$or":[{"a":{"$eq":"1"}},{"b":{"$eq":"2"}}]}`
	if got != want {
		t.Errorf("or = %s, want %s", got, want)
	}
}

func TestNegation(t *testing.T) {
	// not over a single comparison negates at the operator level with $not.
	not := ir.Not{Kid: cmp("year", ir.OpEq, "2000")}
	got := jsonOf(t, lower(t, not))
	if want := `{"year":{"$not":{"$eq":"2000"}}}`; got != want {
		t.Errorf("not.eq = %s, want %s", got, want)
	}
	// not over a logical group negates with a single-element $nor.
	notAnd := ir.Not{Kid: ir.And{Kids: []ir.Cond{cmp("a", ir.OpEq, "1"), cmp("b", ir.OpEq, "2")}}}
	got = jsonOf(t, lower(t, notAnd))
	want := `{"$nor":[{"$and":[{"a":{"$eq":"1"}},{"b":{"$eq":"2"}}]}]}`
	if got != want {
		t.Errorf("not.and = %s, want %s", got, want)
	}
}

func TestInlineNegate(t *testing.T) {
	// An inline not. prefix on a single operator negates the same way.
	c := cmp("year", ir.OpEq, "2000")
	c.Negate = true
	got := jsonOf(t, lower(t, c))
	if want := `{"year":{"$not":{"$eq":"2000"}}}`; got != want {
		t.Errorf("not.eq inline = %s, want %s", got, want)
	}
}

func TestDottedPath(t *testing.T) {
	// A JSON sub-path lowers to a dotted field path.
	c := ir.Compare{Path: []string{"address", "city"}, Op: ir.OpEq, Value: ir.Value{Text: "Berlin"}}
	got := jsonOf(t, lower(t, c))
	if want := `{"address.city":{"$eq":"Berlin"}}`; got != want {
		t.Errorf("dotted = %s, want %s", got, want)
	}
}

func TestUnsupportedOperators(t *testing.T) {
	for _, op := range []ir.Op{ir.OpContains, ir.OpContained, ir.OpOverlap,
		ir.OpRangeSL, ir.OpRangeSR, ir.OpRangeNXR, ir.OpRangeNXL, ir.OpRangeAdj} {
		_, err := LowerFilter(cmp("tags", op, "x"))
		if err == nil {
			t.Errorf("op %d should be unsupported", op)
			continue
		}
		if err.Code != "PGRST127" {
			t.Errorf("op %d code = %s, want PGRST127", op, err.Code)
		}
	}
}

func BenchmarkLowerFilter(b *testing.B) {
	c := ir.And{Kids: []ir.Cond{
		ir.Compare{Path: []string{"year"}, Op: ir.OpGte, Value: ir.Value{Text: "2000"}},
		ir.Compare{Path: []string{"rating"}, Op: ir.OpEq, Value: ir.Value{Text: "PG"}},
	}}
	for b.Loop() {
		if _, err := LowerFilter(c); err != nil {
			b.Fatal(err)
		}
	}
}
