package ir

import (
	"reflect"
	"testing"
)

// errCode asserts ParseRead fails with the given PGRST code.
func errCode(t *testing.T, query, code string) {
	t.Helper()
	_, err := ParseRead("films", query, nil)
	if err == nil {
		t.Fatalf("ParseRead(%q): want error %s, got nil", query, code)
	}
	if err.Code != code {
		t.Fatalf("ParseRead(%q): code = %s, want %s", query, err.Code, code)
	}
}

// --- 01.1: any/all quantifiers ---

func TestQuantifierParsesListForEachOperator(t *testing.T) {
	for _, op := range []string{"eq", "gt", "gte", "lt", "lte", "match", "imatch"} {
		cmp := fetchCompare(t, "id="+op+"(any).{1,2,3}")
		if cmp.Quant != QAny {
			t.Errorf("%s(any): Quant = %d, want QAny", op, cmp.Quant)
		}
		if !reflect.DeepEqual(cmp.Value.List, []string{"1", "2", "3"}) {
			t.Errorf("%s(any): List = %v, want [1 2 3]", op, cmp.Value.List)
		}
	}
}

func TestQuantifierLikeTranslatesWildcards(t *testing.T) {
	cmp := fetchCompare(t, "name=like(any).{*cat*,*dog*}")
	if cmp.Op != OpLike || cmp.Quant != QAny {
		t.Fatalf("got Op=%v Quant=%d", cmp.Op, cmp.Quant)
	}
	if !reflect.DeepEqual(cmp.Value.List, []string{"%cat%", "%dog%"}) {
		t.Errorf("List = %v, want [%%cat%% %%dog%%]", cmp.Value.List)
	}
}

func TestQuantifierRejectedOnNonQuantifiable(t *testing.T) {
	// neq and is do not take a quantifier in PostgREST.
	errCode(t, "id=neq(any).{1,2}", "PGRST100")
}

func TestQuantifierEmptyListRejected(t *testing.T) {
	errCode(t, "id=eq(any).{}", "PGRST100")
}

func TestQuantifierListInLogicalTree(t *testing.T) {
	// The comma inside {…} must not split the or= tree (item 01.1 splitTopLevel).
	q := mustRead(t, "or=(name.like(any).{*cat*,*dog*},year.eq.2000)")
	or, ok := (*q.Where).(Or)
	if !ok {
		t.Fatalf("Where = %T, want Or", *q.Where)
	}
	if len(or.Kids) != 2 {
		t.Fatalf("or has %d kids, want 2", len(or.Kids))
	}
	first := or.Kids[0].(Compare)
	if !reflect.DeepEqual(first.Value.List, []string{"%cat%", "%dog%"}) {
		t.Errorf("first kid list = %v", first.Value.List)
	}
}

// --- 01.2: quoted identifiers and in-list escapes ---

func TestQuotedIdentifierWithDotInFilter(t *testing.T) {
	cmp := fetchCompare(t, `%22weird.name%22=eq.1`)
	if !reflect.DeepEqual(cmp.Path, []string{"weird.name"}) {
		t.Errorf("Path = %v, want [weird.name]", cmp.Path)
	}
}

func TestQuotedIdentifierInSelect(t *testing.T) {
	q := mustRead(t, `select=%22a:b%22`)
	c := q.Select[0].(Column)
	if !reflect.DeepEqual(c.Path, []string{"a:b"}) {
		t.Errorf("Path = %v, want [a:b]", c.Path)
	}
	if c.Alias != "" {
		t.Errorf("Alias = %q, want empty (colon was inside quotes)", c.Alias)
	}
}

func TestQuotedIdentifierInOrder(t *testing.T) {
	q := mustRead(t, `order=%22weird.name%22.desc`)
	if len(q.Order) != 1 {
		t.Fatalf("got %d order terms", len(q.Order))
	}
	if !reflect.DeepEqual(q.Order[0].Path, []string{"weird.name"}) || !q.Order[0].Desc {
		t.Errorf("order = %+v", q.Order[0])
	}
}

func TestQuotedIdentifierInLogicalTree(t *testing.T) {
	q := mustRead(t, `or=(%22weird.name%22.eq.1,year.eq.2)`)
	or := (*q.Where).(Or)
	first := or.Kids[0].(Compare)
	if !reflect.DeepEqual(first.Path, []string{"weird.name"}) {
		t.Errorf("Path = %v, want [weird.name]", first.Path)
	}
}

func TestInListBackslashEscapes(t *testing.T) {
	// in.("a,b","c\"d","e\\f") -> elements with the comma, quote, and backslash.
	cmp := fetchCompare(t, `tag=in.("a,b","c\"d","e\\f")`)
	want := []string{"a,b", `c"d`, `e\f`}
	if !reflect.DeepEqual(cmp.Value.List, want) {
		t.Errorf("List = %v, want %v", cmp.Value.List, want)
	}
}

// --- 01.3: empty in.() ---

func TestEmptyInListRejected(t *testing.T) {
	errCode(t, "id=in.()", "PGRST100")
}

// --- 01.5: empty select= ---

func TestEmptySelectRejected(t *testing.T) {
	errCode(t, "select=", "PGRST100")
}

func TestOmittedSelectIsAllColumns(t *testing.T) {
	q := mustRead(t, "year=eq.2000")
	if len(q.Select) != 0 {
		t.Errorf("omitted select should leave an empty projection, got %v", q.Select)
	}
}

// --- 01.7: order modifier grammar ---

func TestOrderModifierGrammar(t *testing.T) {
	good := []string{
		"order=year",
		"order=year.asc",
		"order=year.desc",
		"order=year.asc.nullsfirst",
		"order=year.desc.nullslast",
	}
	for _, q := range good {
		if _, err := ParseRead("films", q, nil); err != nil {
			t.Errorf("%q: unexpected error %v", q, err)
		}
	}
	bad := []string{
		"order=year.nullsfirst.asc", // nulls before direction
		"order=year.asc.desc",       // two directions
		"order=year.nullsfirst.nullslast",
	}
	for _, q := range bad {
		errCode(t, q, "PGRST100")
	}
}
