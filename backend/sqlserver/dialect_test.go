package sqlserver

import (
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
)

// The dialect is stateless, so one value serves every test.
var d Dialect

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"id":     "[id]",
		"my col": "[my col]",
		"a]b":    "[a]]b]",
	}
	for in, want := range cases {
		if got := d.QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholder(t *testing.T) {
	// SQL Server placeholders are named @pN, one name per position.
	cases := map[int]string{1: "@p1", 2: "@p2", 17: "@p17"}
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
		hasOrder      bool
		want          string
	}{
		{"neither", nil, nil, false, ""},
		{"limit, ordered", p(10), nil, true, "OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY"},
		{"limit, no order injects", p(10), nil, false, "ORDER BY (SELECT 1) OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY"},
		{"offset only, ordered", nil, p(5), true, "OFFSET 5 ROWS"},
		{"both, ordered", p(10), p(20), true, "OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY"},
		{"both, no order injects", p(10), p(20), false, "ORDER BY (SELECT 1) OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY"},
	}
	for _, c := range cases {
		if got := d.LimitOffset(c.limit, c.offset, c.hasOrder); got != c.want {
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
		{"asc default last", "ASC", false, nil, "CASE WHEN [c] IS NULL THEN 1 ELSE 0 END", "[c] ASC"},
		{"desc default first", "DESC", true, nil, "CASE WHEN [c] IS NULL THEN 0 ELSE 1 END", "[c] DESC"},
		{"asc forced first", "ASC", false, &yes, "CASE WHEN [c] IS NULL THEN 0 ELSE 1 END", "[c] ASC"},
		{"desc forced last", "DESC", true, &no, "CASE WHEN [c] IS NULL THEN 1 ELSE 0 END", "[c] DESC"},
	}
	for _, c := range cases {
		key, term := d.NullsOrder("[c]", c.dir, c.desc, c.nullsFirst)
		if key != c.wantKey || term != c.wantTerm {
			t.Errorf("%s: key=%q term=%q, want key=%q term=%q", c.name, key, term, c.wantKey, c.wantTerm)
		}
	}
}

func TestReturningOutput(t *testing.T) {
	clause, ok := d.Returning([]string{"[id]", "[name]"})
	if !ok || clause != "OUTPUT INSERTED.[id], INSERTED.[name]" {
		t.Errorf("Returning = %q, ok=%v", clause, ok)
	}
}

func TestUpsertIsMultiStatement(t *testing.T) {
	// SQL Server has no single-statement upsert this seam can build; it reports an
	// error so the data plane drives the multi-statement transaction.
	if _, err := d.Upsert(sqlgen.UpsertSpec{Update: []string{"[name]"}}); err == nil {
		t.Error("SQL Server Upsert should report it cannot be a single statement")
	}
}

func TestJSON(t *testing.T) {
	// The SQL Server 2022 JSON_OBJECT separator is a colon, not a comma.
	obj := d.JSONObject([]sqlgen.Pair{{Key: "name", Value: "d.[name]"}, {Key: "year", Value: "d.[year]"}})
	if obj != "JSON_OBJECT('name': d.[name], 'year': d.[year])" {
		t.Errorf("JSONObject = %q", obj)
	}
	if got := d.JSONAgg("t", "t.[id] DESC"); got != "JSON_ARRAYAGG(t)" {
		t.Errorf("JSONAgg = %q", got)
	}
}

func TestCast(t *testing.T) {
	cases := map[string]string{
		"smallint":         "CAST([x] AS SMALLINT)",
		"int":              "CAST([x] AS INT)",
		"bigint":           "CAST([x] AS BIGINT)",
		"numeric":          "CAST([x] AS DECIMAL)",
		"real":             "CAST([x] AS REAL)",
		"double precision": "CAST([x] AS FLOAT)",
		"bool":             "CAST([x] AS BIT)",
		"uuid":             "CAST([x] AS UNIQUEIDENTIFIER)",
		"date":             "CAST([x] AS DATE)",
		"timestamptz":      "CAST([x] AS DATETIME2)",
		"text":             "CAST([x] AS NVARCHAR(MAX))",
		"json":             "CAST([x] AS NVARCHAR(MAX))",
		"mystery":          "CAST([x] AS NVARCHAR(MAX))",
	}
	for in, want := range cases {
		if got := d.Cast("[x]", in); got != want {
			t.Errorf("Cast(_, %q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegex(t *testing.T) {
	frag, ok := d.Regex("[c]", "^a", false)
	if !ok || frag != "REGEXP_LIKE([c], "+sqlgen.PatternMark+")" {
		t.Errorf("case-sensitive = %q, ok=%v", frag, ok)
	}
	frag, ok = d.Regex("[c]", "^a", true)
	if !ok || frag != "REGEXP_LIKE([c], "+sqlgen.PatternMark+", 'i')" {
		t.Errorf("case-insensitive = %q, ok=%v", frag, ok)
	}
}

func TestSessionContext(t *testing.T) {
	if got := d.SessionRead("request.jwt.claims"); got != "SESSION_CONTEXT(N'request.jwt.claims')" {
		t.Errorf("SessionRead = %q", got)
	}
	stmt, ok := d.SessionWrite("request.jwt.claims")
	if !ok || stmt != "EXEC sp_set_session_context N'request.jwt.claims', "+sqlgen.PatternMark {
		t.Errorf("SessionWrite = %q, ok=%v", stmt, ok)
	}
}

func TestBoolValue(t *testing.T) {
	if d.BoolValue(true) != "1" || d.BoolValue(false) != "0" {
		t.Error("SQL Server booleans render as the BIT 1/0")
	}
}
