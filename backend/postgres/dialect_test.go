package postgres

import (
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// The dialect is stateless, so one value serves every test.
var d Dialect

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"id":       `"id"`,
		"my col":   `"my col"`,
		`we"ird`:   `"we""ird"`,
		"director": `"director"`,
	}
	for in, want := range cases {
		if got := d.QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholder(t *testing.T) {
	cases := map[int]string{1: "$1", 2: "$2", 17: "$17"}
	for n, want := range cases {
		if got := d.Placeholder(n); got != want {
			t.Errorf("Placeholder(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestLimitOffset(t *testing.T) {
	p := func(n int) *int { return &n }
	cases := []struct {
		name          string
		limit, offset *int
		want          string
	}{
		{"neither", nil, nil, ""},
		{"limit only", p(10), nil, "LIMIT 10"},
		{"offset only", nil, p(5), "OFFSET 5"},
		{"both", p(10), p(20), "LIMIT 10 OFFSET 20"},
	}
	for _, c := range cases {
		// hasOrder is irrelevant on PostgreSQL; pass both values to prove it.
		if got := d.LimitOffset(c.limit, c.offset, false); got != c.want {
			t.Errorf("%s: = %q, want %q", c.name, got, c.want)
		}
		if got := d.LimitOffset(c.limit, c.offset, true); got != c.want {
			t.Errorf("%s (hasOrder): = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNullsOrder(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name       string
		dir        string
		desc       bool
		nullsFirst *bool
		want       string
	}{
		{"asc default last", "ASC", false, nil, `"c" ASC NULLS LAST`},
		{"desc default first", "DESC", true, nil, `"c" DESC NULLS FIRST`},
		{"asc forced first", "ASC", false, &yes, `"c" ASC NULLS FIRST`},
		{"desc forced last", "DESC", true, &no, `"c" DESC NULLS LAST`},
	}
	for _, c := range cases {
		sortKey, term := d.NullsOrder(`"c"`, c.dir, c.desc, c.nullsFirst)
		if sortKey != "" {
			t.Errorf("%s: sortKey = %q, want empty (PG is native)", c.name, sortKey)
		}
		if term != c.want {
			t.Errorf("%s: term = %q, want %q", c.name, term, c.want)
		}
	}
}

func TestReturning(t *testing.T) {
	if _, ok := d.Returning(nil); ok {
		t.Error("empty column list should report ok=false")
	}
	clause, ok := d.Returning([]string{`"id"`, `"title"`})
	if !ok || clause != `RETURNING "id", "title"` {
		t.Errorf("Returning = %q, ok=%v", clause, ok)
	}
}

func TestUpsert(t *testing.T) {
	cases := []struct {
		name string
		spec sqlgen.UpsertSpec
		want string
	}{
		{
			"merge with target",
			sqlgen.UpsertSpec{Target: []string{`"id"`}, Update: []string{`"title"`, `"year"`}},
			`ON CONFLICT ("id") DO UPDATE SET "title" = excluded."title", "year" = excluded."year"`,
		},
		{
			"ignore with target",
			sqlgen.UpsertSpec{Target: []string{`"id"`}, Ignore: true},
			`ON CONFLICT ("id") DO NOTHING`,
		},
		{
			"merge no target",
			sqlgen.UpsertSpec{Update: []string{`"title"`}},
			`ON CONFLICT DO UPDATE SET "title" = excluded."title"`,
		},
		{
			"empty update degrades to nothing",
			sqlgen.UpsertSpec{Target: []string{`"id"`}},
			`ON CONFLICT ("id") DO NOTHING`,
		},
	}
	for _, c := range cases {
		got, err := d.Upsert(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestJSONObject(t *testing.T) {
	got := d.JSONObject([]sqlgen.Pair{
		{Key: "name", Value: `d."name"`},
		{Key: "born", Value: `d."born"`},
	})
	want := `json_build_object('name', d."name", 'born', d."born")`
	if got != want {
		t.Errorf("JSONObject = %q, want %q", got, want)
	}
	// A key with a quote is escaped so it cannot break the literal.
	got = d.JSONObject([]sqlgen.Pair{{Key: "a'b", Value: "x"}})
	if got != `json_build_object('a''b', x)` {
		t.Errorf("escaped key = %q", got)
	}
}

func TestJSONAgg(t *testing.T) {
	if got := d.JSONAgg("t", ""); got != "json_agg(t)" {
		t.Errorf("unordered = %q", got)
	}
	if got := d.JSONAgg("t", `t."title" DESC`); got != `json_agg(t ORDER BY t."title" DESC)` {
		t.Errorf("ordered = %q", got)
	}
}

func TestCast(t *testing.T) {
	cases := map[string]string{
		"int":              `("x")::int4`,
		"integer":          `("x")::int4`,
		"bigint":           `("x")::int8`,
		"smallint":         `("x")::int2`,
		"numeric":          `("x")::numeric`,
		"real":             `("x")::float4`,
		"double precision": `("x")::float8`,
		"bool":             `("x")::bool`,
		"text":             `("x")::text`,
		"date":             `("x")::date`,
		"timestamptz":      `("x")::timestamptz`,
		"uuid":             `("x")::uuid`,
		"json":             `("x")::json`,
		"jsonb":            `("x")::jsonb`,
		// Types outside the alias table pass through verbatim rather than
		// degrading to text, so they resolve the way PostgREST relies on.
		"money":            `("x")::money`,
		"interval":         `("x")::interval`,
		"bytea":            `("x")::bytea`,
		"inet":             `("x")::inet`,
		"mood":             `("x")::mood`,
		"numeric(10,2)":    `("x")::numeric(10,2)`,
		"int[]":            `("x")::int[]`,
		"public.color":     `("x")::public.color`,
	}
	for in, want := range cases {
		if got := d.Cast(`"x"`, in); got != want {
			t.Errorf("Cast(_, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegex(t *testing.T) {
	frag, ok := d.Regex(`"c"`, "^a", false)
	if !ok || frag != `"c" ~ `+sqlgen.PatternMark {
		t.Errorf("case-sensitive = %q, ok=%v", frag, ok)
	}
	frag, ok = d.Regex(`"c"`, "^a", true)
	if !ok || frag != `"c" ~* `+sqlgen.PatternMark {
		t.Errorf("case-insensitive = %q, ok=%v", frag, ok)
	}
}

func TestRegexFeatureGapNone(t *testing.T) {
	// PostgreSQL is the oracle: even a backreference, which an RE2 engine would
	// reject, is left to PG to evaluate.
	for _, p := range []string{`(a)\1`, `(?=foo)`, `^plain$`} {
		if gap := d.RegexFeatureGap(p); gap != "" {
			t.Errorf("RegexFeatureGap(%q) = %q, want empty", p, gap)
		}
	}
}

func TestFullText(t *testing.T) {
	cases := []struct {
		name    string
		variant ir.FTSVariant
		config  string
		want    string
	}{
		{"plain no config", ir.FTSPlain, "", `to_tsvector("body") @@ to_tsquery(` + sqlgen.PatternMark + `)`},
		{"plaintext", ir.FTSPlainText, "", `to_tsvector("body") @@ plainto_tsquery(` + sqlgen.PatternMark + `)`},
		{"phrase", ir.FTSPhrase, "", `to_tsvector("body") @@ phraseto_tsquery(` + sqlgen.PatternMark + `)`},
		{"web", ir.FTSWeb, "", `to_tsvector("body") @@ websearch_to_tsquery(` + sqlgen.PatternMark + `)`},
		{"with config", ir.FTSPlain, "english", `to_tsvector('english', "body") @@ to_tsquery('english', ` + sqlgen.PatternMark + `)`},
	}
	for _, c := range cases {
		frag, bind, ok := d.FullText(`"body"`, nil, c.variant, c.config, "cat")
		if !ok {
			t.Fatalf("%s: ok=false", c.name)
		}
		if bind != "cat" {
			t.Errorf("%s: bind = %q, want the raw value (PG parses the grammar)", c.name, bind)
		}
		if frag != c.want {
			t.Errorf("%s: frag = %q, want %q", c.name, frag, c.want)
		}
	}
}

func TestSession(t *testing.T) {
	if got := d.SessionRead("request.jwt.claims"); got != `current_setting('request.jwt.claims', true)` {
		t.Errorf("SessionRead = %q", got)
	}
	stmt, ok := d.SessionWrite("request.jwt.claims")
	if !ok || stmt != `set_config('request.jwt.claims', `+sqlgen.PatternMark+`, true)` {
		t.Errorf("SessionWrite = %q, ok=%v", stmt, ok)
	}
}

func TestBoolValue(t *testing.T) {
	if d.BoolValue(true) != "TRUE" || d.BoolValue(false) != "FALSE" {
		t.Error("BoolValue should render the PostgreSQL keywords")
	}
}
