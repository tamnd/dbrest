package mysql

import (
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// The dialect is stateless, so one value serves every test.
var d Dialect

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"id":     "`id`",
		"my col": "`my col`",
		"ba`ck":  "`ba``ck`",
	}
	for in, want := range cases {
		if got := d.QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholder(t *testing.T) {
	// MySQL placeholders are positional ?, not numbered; every position is "?".
	for _, n := range []int{1, 2, 17} {
		if got := d.Placeholder(n); got != "?" {
			t.Errorf("Placeholder(%d) = %q, want ?", n, got)
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
		{"offset only", nil, p(5), "LIMIT 18446744073709551615 OFFSET 5"},
		{"both", p(10), p(20), "LIMIT 10 OFFSET 20"},
	}
	for _, c := range cases {
		if got := d.LimitOffset(c.limit, c.offset, false); got != c.want {
			t.Errorf("%s: = %q, want %q", c.name, got, c.want)
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
		wantKey    string
		wantTerm   string
	}{
		{"asc default last", "ASC", false, nil, "(`c` IS NULL) ASC", "`c` ASC"},
		{"desc default first", "DESC", true, nil, "(`c` IS NULL) DESC", "`c` DESC"},
		{"asc forced first", "ASC", false, &yes, "(`c` IS NULL) DESC", "`c` ASC"},
		{"desc forced last", "DESC", true, &no, "(`c` IS NULL) ASC", "`c` DESC"},
	}
	for _, c := range cases {
		key, term := d.NullsOrder("`c`", c.dir, c.desc, c.nullsFirst)
		if key != c.wantKey || term != c.wantTerm {
			t.Errorf("%s: key=%q term=%q, want key=%q term=%q", c.name, key, term, c.wantKey, c.wantTerm)
		}
	}
}

func TestReturningUnsupported(t *testing.T) {
	if _, ok := d.Returning([]string{"`id`"}); ok {
		t.Error("MySQL 8 has no RETURNING; ok should be false")
	}
}

func TestUpsert(t *testing.T) {
	cases := []struct {
		name string
		spec sqlgen.UpsertSpec
		want string
	}{
		{
			"merge",
			sqlgen.UpsertSpec{Update: []string{"`id`", "`name`"}},
			"ON DUPLICATE KEY UPDATE `id` = VALUES(`id`), `name` = VALUES(`name`)",
		},
		{
			"ignore is a no-op update over the first column",
			sqlgen.UpsertSpec{Update: []string{"`id`", "`name`"}, Ignore: true},
			"ON DUPLICATE KEY UPDATE `id` = `id`",
		},
		{
			// The conflict target is ignored: ON DUPLICATE KEY fires on any unique key.
			"target is ignored",
			sqlgen.UpsertSpec{Target: []string{"`id`"}, Update: []string{"`name`"}},
			"ON DUPLICATE KEY UPDATE `name` = VALUES(`name`)",
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
	if _, err := d.Upsert(sqlgen.UpsertSpec{}); err == nil {
		t.Error("an upsert with no columns should error")
	}
}

func TestJSON(t *testing.T) {
	obj := d.JSONObject([]sqlgen.Pair{{Key: "name", Value: "d.`name`"}})
	if obj != "JSON_OBJECT('name', d.`name`)" {
		t.Errorf("JSONObject = %q", obj)
	}
	// The order argument is unused; MySQL JSON_ARRAYAGG takes none.
	if got := d.JSONAgg("t", "t.`id` DESC"); got != "JSON_ARRAYAGG(t)" {
		t.Errorf("JSONAgg = %q", got)
	}
}

func TestCast(t *testing.T) {
	cases := map[string]string{
		"int":         "CAST(`x` AS SIGNED)",
		"bigint":      "CAST(`x` AS SIGNED)",
		"numeric":     "CAST(`x` AS DECIMAL)",
		"bool":        "CAST(`x` AS SIGNED)",
		"text":        "CAST(`x` AS CHAR)",
		"date":        "CAST(`x` AS DATE)",
		"timestamptz": "CAST(`x` AS DATETIME)",
		"json":        "CAST(`x` AS JSON)",
		"mystery":     "CAST(`x` AS CHAR)",
	}
	for in, want := range cases {
		if got := d.Cast("`x`", in); got != want {
			t.Errorf("Cast(_, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegex(t *testing.T) {
	frag, ok := d.Regex("`c`", "^a", false)
	if !ok || frag != "REGEXP_LIKE(`c`, "+sqlgen.PatternMark+")" {
		t.Errorf("case-sensitive = %q, ok=%v", frag, ok)
	}
	frag, ok = d.Regex("`c`", "^a", true)
	if !ok || frag != "REGEXP_LIKE(`c`, "+sqlgen.PatternMark+", 'i')" {
		t.Errorf("case-insensitive = %q, ok=%v", frag, ok)
	}
}

func TestSessionNoStore(t *testing.T) {
	if d.SessionRead("role") != "" {
		t.Error("MySQL has no SQL-readable session store")
	}
	if _, ok := d.SessionWrite("role"); ok {
		t.Error("MySQL SessionWrite should report ok=false")
	}
}

func TestBoolValue(t *testing.T) {
	if d.BoolValue(true) != "1" || d.BoolValue(false) != "0" {
		t.Error("MySQL booleans render as 1/0")
	}
}
