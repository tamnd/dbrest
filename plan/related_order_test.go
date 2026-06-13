package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/schema"
)

// planOrder parses and plans a films read with the given select and order
// strings, returning the plan or the planner error.
func planOrder(t *testing.T, m *schema.Model, relation, query string) (*ir.Plan, *pgerr.APIError) {
	t.Helper()
	q, perr := ir.ParseRead(relation, query, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	return Read(m, q, nil, Options{})
}

// A related order over a to-one embed resolves: the term carries the relation
// name and the planner accepts it once the embed and its column check out (item
// 07.6).
func TestRelatedOrderToOneResolves(t *testing.T) {
	m := embedModel()
	pl, err := planOrder(t, m, "films",
		"select=title,people!director_id(name)&order=people(name).asc")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var found bool
	for _, ot := range pl.Query.Order {
		if ot.Rel == "people" && len(ot.Path) == 1 && ot.Path[0] == "name" {
			found = true
		}
	}
	if !found {
		t.Errorf("planned order missing related term, got %+v", pl.Query.Order)
	}
}

// Ordering by a relation not embedded in the request is PGRST108: the term names
// a resource the select never pulled in.
func TestRelatedOrderNotEmbeddedIsPGRST108(t *testing.T) {
	m := embedModel()
	_, err := planOrder(t, m, "films", "select=title&order=people(name).asc")
	if err == nil || err.Code != "PGRST108" {
		t.Fatalf("want PGRST108, got %v", err)
	}
	if err.HTTPStatus != 400 {
		t.Errorf("status = %d, want 400", err.HTTPStatus)
	}
}

// Ordering by a to-many embed is PGRST118: a parent cannot sort on a column of a
// resource it has many of. people own many films, so people?order=films(title)
// is not a to-one relation.
func TestRelatedOrderToManyIsPGRST118(t *testing.T) {
	m := embedModel()
	_, err := planOrder(t, m, "people",
		"select=name,films!director_id(title)&order=films(title).asc")
	if err == nil || err.Code != "PGRST118" {
		t.Fatalf("want PGRST118, got %v", err)
	}
	if err.HTTPStatus != 400 {
		t.Errorf("status = %d, want 400", err.HTTPStatus)
	}
}

// A related order naming a real embed but an unknown column on the target is the
// ordinary unknown-column rejection (42703, the reference reaches PostgreSQL),
// not a relation error.
func TestRelatedOrderUnknownColumnIsRejected(t *testing.T) {
	m := embedModel()
	_, err := planOrder(t, m, "films",
		"select=title,people!director_id(name)&order=people(nope).asc")
	if err == nil || err.Code != "42703" {
		t.Fatalf("want 42703, got %v", err)
	}
}
