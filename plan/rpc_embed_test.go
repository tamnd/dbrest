package plan

import (
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/rpc"
)

// callReg wraps one function in a static registry for the call planner.
func callReg(fn *rpc.Function) rpc.Registry {
	return rpc.NewStaticRegistry([]*rpc.Function{fn})
}

// filmsSetof returns rows of the films relation, the embeddable RPC shape.
func filmsSetof() *rpc.Function {
	return &rpc.Function{
		Name:       "recent_films",
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "films"},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: "SELECT * FROM films"},
	}
}

// A call over a function returning rows of a relation resolves its embeds against
// that relation, binding the relationship the same way a table read does.
func TestCallResolvesEmbedAgainstReturnRelation(t *testing.T) {
	m := embedModel()
	c, perr := ir.ParseCall("recent_films", "select=title,people!director_id(name)", nil, true, "", nil, "", "")
	if perr != nil {
		t.Fatalf("ParseCall: %v", perr)
	}
	pl, err := Call(callReg(filmsSetof()), m, c, true, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(pl.Call.Embeds) != 1 {
		t.Fatalf("got %d embeds, want 1", len(pl.Call.Embeds))
	}
	emb := pl.Call.Embeds[0]
	if emb.Rel == nil {
		t.Fatal("embed relationship not bound")
	}
	if emb.Cardinality != ir.CardToOne {
		t.Errorf("cardinality = %v, want to-one", emb.Cardinality)
	}
	if emb.Query.Relation.Name != "people" {
		t.Errorf("embed relation = %q, want people", emb.Query.Relation.Name)
	}
}

// A function whose result is not a known relation has nothing to embed against,
// which is the read path's PGRST200.
func TestCallEmbedOnScalarReturnIsPGRST200(t *testing.T) {
	m := embedModel()
	scalar := &rpc.Function{
		Name:       "film_titles",
		Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "text"},
		Volatility: rpc.Stable,
		Query:      &rpc.PortableQuery{SQL: "SELECT title FROM films"},
	}
	c, perr := ir.ParseCall("film_titles", "select=people(name)", nil, true, "", nil, "", "")
	if perr != nil {
		t.Fatalf("ParseCall: %v", perr)
	}
	_, err := Call(callReg(scalar), m, c, true, nil)
	if err == nil {
		t.Fatal("want an error embedding on a scalar-set return")
	}
	if err.Code != pgerrCodeNoRelationship {
		t.Errorf("code = %q, want %q", err.Code, pgerrCodeNoRelationship)
	}
}

const pgerrCodeNoRelationship = "PGRST200"
