package ir

import (
	"encoding/json"
	"testing"
)

func TestParseCallGetArgsFromQuery(t *testing.T) {
	c, err := ParseCall("add_them", "a=2&b=3", nil, true, "", nil)
	if err != nil {
		t.Fatalf("ParseCall: %v", err)
	}
	if c.Function.Name != "add_them" {
		t.Errorf("function = %q", c.Function.Name)
	}
	if len(c.Args) != 2 {
		t.Fatalf("args = %v, want 2", c.Args)
	}
	if c.Args["a"].Text != "2" || c.Args["b"].Text != "3" {
		t.Errorf("args = %+v, want text 2 and 3", c.Args)
	}
	if c.Args["a"].JSON != nil {
		t.Error("a GET argument carries text, not JSON")
	}
}

func TestParseCallGetReservedKeysArePostFilters(t *testing.T) {
	c, err := ParseCall("list_films", "select=title&order=year.desc&limit=5&year=gte.2000", nil, true, "", nil)
	if err != nil {
		t.Fatalf("ParseCall: %v", err)
	}
	// select/order/limit are post-filters, not arguments.
	if _, ok := c.Args["select"]; ok {
		t.Error("select must not be an argument")
	}
	// year is a non-reserved key, so on a GET it is an argument, not a filter.
	if c.Args["year"].Text != "gte.2000" {
		t.Errorf("year argument = %+v", c.Args["year"])
	}
	if len(c.Select) != 1 {
		t.Errorf("select post-filter = %v", c.Select)
	}
	if len(c.Order) != 1 || c.Order[0].Path[0] != "year" || !c.Order[0].Desc {
		t.Errorf("order post-filter = %v", c.Order)
	}
	if c.Limit == nil || *c.Limit != 5 {
		t.Errorf("limit post-filter = %v", c.Limit)
	}
}

func TestParseCallPostArgsFromBody(t *testing.T) {
	c, err := ParseCall("add_them", "", nil, false, "application/json", []byte(`{"a":2,"b":3}`))
	if err != nil {
		t.Fatalf("ParseCall: %v", err)
	}
	if len(c.Args) != 2 {
		t.Fatalf("args = %v, want 2", c.Args)
	}
	// A POST argument preserves its JSON type; numbers stay json.Number so an
	// integer does not widen to float.
	if n, ok := c.Args["a"].JSON.(json.Number); !ok || n.String() != "2" {
		t.Errorf("a = %#v, want JSON number 2", c.Args["a"].JSON)
	}
}

func TestParseCallPostQueryStringIsPostFilter(t *testing.T) {
	c, err := ParseCall("list_films", "year=gte.2000&order=year", nil, false, "application/json", []byte(`{"genre":"scifi"}`))
	if err != nil {
		t.Fatalf("ParseCall: %v", err)
	}
	// On POST the body is the arguments; the query string post-filters.
	if _, ok := c.Args["year"]; ok {
		t.Error("on POST the query string is a post-filter, not an argument")
	}
	if c.Args["genre"].JSON != "scifi" {
		t.Errorf("genre = %+v", c.Args["genre"])
	}
	if c.Where == nil {
		t.Error("year=gte.2000 should parse into a post-filter Where")
	}
	if len(c.Order) != 1 {
		t.Errorf("order post-filter = %v", c.Order)
	}
}

func TestParseCallPostNoBody(t *testing.T) {
	c, err := ParseCall("now", "", nil, false, "application/json", nil)
	if err != nil {
		t.Fatalf("ParseCall: %v", err)
	}
	if len(c.Args) != 0 {
		t.Errorf("a no-argument call has no args, got %v", c.Args)
	}
}

func TestParseCallCountPrefer(t *testing.T) {
	c, err := ParseCall("list_films", "", []string{"count=exact"}, true, "", nil)
	if err != nil {
		t.Fatalf("ParseCall: %v", err)
	}
	if c.Count != CountExact {
		t.Errorf("count = %v, want exact", c.Count)
	}
}

func TestParseCallBadJSONBody(t *testing.T) {
	if _, err := ParseCall("f", "", nil, false, "application/json", []byte(`{nope`)); err == nil {
		t.Error("malformed body should error")
	}
}
