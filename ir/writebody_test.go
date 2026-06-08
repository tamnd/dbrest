package ir

import "testing"

// A CSV insert body decodes to one row per data line, keyed by the header. The
// header order fixes the write column order, and an empty field is SQL NULL.
func TestParseWriteCSVBody(t *testing.T) {
	body := []byte("title,year\nDune,2021\nArrival,\n")
	q, err := ParseWrite(Insert, "films", "", nil, "text/csv", body)
	if err != nil {
		t.Fatalf("ParseWrite CSV: %v", err)
	}
	if got := q.Write.Columns; len(got) != 2 || got[0] != "title" || got[1] != "year" {
		t.Fatalf("Columns = %v, want [title year] in header order", got)
	}
	if len(q.Write.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2", len(q.Write.Rows))
	}
	if v := q.Write.Rows[0]["title"]; v.JSON != "Dune" {
		t.Errorf("row0 title = %#v, want Dune", v.JSON)
	}
	// The empty year field on the second row is NULL, not the empty string.
	if v := q.Write.Rows[1]["year"]; v.JSON != nil {
		t.Errorf("row1 year = %#v, want nil (NULL)", v.JSON)
	}
}

// A CSV body with a header but no data rows is a valid empty insert: the columns
// are fixed by the header and there are no rows.
func TestParseWriteCSVHeaderOnly(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", nil, "text/csv", []byte("title,year\n"))
	if err != nil {
		t.Fatalf("ParseWrite CSV header-only: %v", err)
	}
	if len(q.Write.Rows) != 0 {
		t.Errorf("Rows = %d, want 0", len(q.Write.Rows))
	}
	if len(q.Write.Columns) != 2 {
		t.Errorf("Columns = %v, want the two header columns", q.Write.Columns)
	}
}

func TestParseWriteCSVEmptyRejected(t *testing.T) {
	if _, err := ParseWrite(Insert, "films", "", nil, "text/csv", nil); err == nil {
		t.Fatal("an empty CSV body should be rejected (no header row)")
	}
}

func TestParseWriteCSVMalformedRejected(t *testing.T) {
	// A bare quote opens a field the row never closes: not valid RFC 4180.
	if _, err := ParseWrite(Insert, "films", "", nil, "text/csv", []byte("a,b\n\"x,y\n")); err == nil {
		t.Fatal("a malformed CSV body should be rejected")
	}
}

// A form-urlencoded insert body decodes to a single row of string columns.
func TestParseWriteFormBody(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", nil,
		"application/x-www-form-urlencoded", []byte("title=Dune&year=2021"))
	if err != nil {
		t.Fatalf("ParseWrite form: %v", err)
	}
	if len(q.Write.Rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(q.Write.Rows))
	}
	if v := q.Write.Rows[0]["title"]; v.JSON != "Dune" {
		t.Errorf("title = %#v, want Dune", v.JSON)
	}
	if v := q.Write.Rows[0]["year"]; v.JSON != "2021" {
		t.Errorf("year = %#v, want the string 2021", v.JSON)
	}
}

// A content type carrying a charset parameter still classifies by its base type.
func TestParseWriteFormWithCharsetParam(t *testing.T) {
	q, err := ParseWrite(Insert, "films", "", nil,
		"application/x-www-form-urlencoded; charset=utf-8", []byte("title=X"))
	if err != nil {
		t.Fatalf("ParseWrite form+charset: %v", err)
	}
	if v := q.Write.Rows[0]["title"]; v.JSON != "X" {
		t.Errorf("title = %#v, want X", v.JSON)
	}
}

// A form body is a meaningful update patch too, so PATCH accepts it.
func TestParseWriteUpdateFormBody(t *testing.T) {
	q, err := ParseWrite(Update, "films", "id=eq.1", nil,
		"application/x-www-form-urlencoded", []byte("rating=PG"))
	if err != nil {
		t.Fatalf("ParseWrite update form: %v", err)
	}
	if v := q.Write.Set["rating"]; v.JSON != "PG" {
		t.Errorf("set rating = %#v, want PG", v.JSON)
	}
}

func TestParseWriteUnsupportedMediaType(t *testing.T) {
	_, err := ParseWrite(Insert, "films", "", nil, "text/yaml", []byte("title: X"))
	if err == nil || err.Code != "PGRST107" {
		t.Fatalf("insert with unknown media type err = %v, want PGRST107", err)
	}
}

// CSV is not a patch format, so an update body in CSV is rejected as an
// unsupported media type rather than silently parsed.
func TestParseWriteUpdateCSVRejected(t *testing.T) {
	_, err := ParseWrite(Update, "films", "id=eq.1", nil, "text/csv", []byte("rating\nPG\n"))
	if err == nil || err.Code != "PGRST107" {
		t.Fatalf("update with CSV err = %v, want PGRST107", err)
	}
}

// Every PostgREST operator spelling maps to its IR op through the public parser.
// This is the table clients depend on, so each token is parsed end to end rather
// than only the handful the other tests happen to use.
func TestParseEveryOperatorToken(t *testing.T) {
	cases := []struct {
		filter string
		op     Op
	}{
		{"x=eq.1", OpEq},
		{"x=neq.1", OpNeq},
		{"x=gt.1", OpGt},
		{"x=gte.1", OpGte},
		{"x=lt.1", OpLt},
		{"x=lte.1", OpLte},
		{"x=like.a*", OpLike},
		{"x=ilike.a*", OpILike},
		{"x=match.^a", OpMatch},
		{"x=imatch.^a", OpIMatch},
		{"x=in.(1,2)", OpIn},
		{"x=is.null", OpIs},
		{"x=isdistinct.1", OpIsDistinct},
		{"x=cs.{1,2}", OpContains},
		{"x=cd.{1,2}", OpContained},
		{"x=ov.{1,2}", OpOverlap},
		{"x=sl.(1,10)", OpRangeSL},
		{"x=sr.(1,10)", OpRangeSR},
		{"x=nxr.(1,10)", OpRangeNXR},
		{"x=nxl.(1,10)", OpRangeNXL},
		{"x=adj.(1,10)", OpRangeAdj},
	}
	for _, c := range cases {
		t.Run(c.filter, func(t *testing.T) {
			q, err := ParseRead("t", c.filter, nil)
			if err != nil {
				t.Fatalf("ParseRead(%q): %v", c.filter, err)
			}
			cmp, ok := (*q.Where).(Compare)
			if !ok {
				t.Fatalf("filter is %T, want Compare", *q.Where)
			}
			if cmp.Op != c.op {
				t.Errorf("op = %v, want %v", cmp.Op, c.op)
			}
		})
	}
	if _, err := ParseRead("t", "x=bogus.1", nil); err == nil {
		t.Error("an unknown operator token should be rejected")
	}
}

// Each Prefer enum value that the other tests do not reach maps to its mode.
// The token table is part of the wire contract, so the alternates are exercised
// directly rather than left to whichever value a happenstance test picked.
func TestParsePreferAlternateValues(t *testing.T) {
	p := ParsePrefer([]string{
		"return=headers-only", "count=planned", "resolution=ignore-duplicates",
		"missing=default", "tx=commit",
	})
	if p.Return == nil || *p.Return != ReturnHeadersOnly {
		t.Errorf("return = %v, want headers-only", p.Return)
	}
	if p.Count == nil || *p.Count != CountPlanned {
		t.Errorf("count = %v, want planned", p.Count)
	}
	if p.Resolution == nil || *p.Resolution != ConflictIgnore {
		t.Errorf("resolution = %v, want ignore-duplicates", p.Resolution)
	}
	if p.Missing == nil || *p.Missing != MissingDefault {
		t.Errorf("missing = %v, want default", p.Missing)
	}
	if p.Tx == nil || *p.Tx != TxCommit {
		t.Errorf("tx = %v, want commit", p.Tx)
	}

	// return=minimal and count=estimated round out the remaining enum values.
	p2 := ParsePrefer([]string{"return=minimal", "count=estimated"})
	if p2.Return == nil || *p2.Return != ReturnMinimal {
		t.Errorf("return = %v, want minimal", p2.Return)
	}
	if p2.Count == nil || *p2.Count != CountEstimated {
		t.Errorf("count = %v, want estimated", p2.Count)
	}

	// An unknown value for a known key is ignored, leaving the field unset.
	p3 := ParsePrefer([]string{"tx=maybe", "missing=sometimes", "handling=lax"})
	if p3.Tx != nil || p3.Missing != nil {
		t.Errorf("unknown values should not set fields: tx=%v missing=%v", p3.Tx, p3.Missing)
	}
}

// A query-string param prefixed by an embed's response key is routed to that
// embed's nested query, not the parent: actors.order=name.asc orders the
// embedded actors. This exercises the embed-scoped param split.
func TestParseEmbedScopedParam(t *testing.T) {
	q, err := ParseRead("films", "select=title,actors(name)&actors.order=name.desc", nil)
	if err != nil {
		t.Fatalf("ParseRead embed-scoped: %v", err)
	}
	if len(q.Embeds) != 1 {
		t.Fatalf("Embeds = %d, want 1", len(q.Embeds))
	}
	// The order landed on the embed, and the parent kept none.
	if len(q.Order) != 0 {
		t.Errorf("parent Order = %v, want none", q.Order)
	}
	emb := q.Embeds[0].Query
	if len(emb.Order) != 1 || emb.Order[0].Path[0] != "name" || !emb.Order[0].Desc {
		t.Errorf("embed Order = %+v, want name desc", emb.Order)
	}
}
