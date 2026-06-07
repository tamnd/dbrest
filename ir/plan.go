package ir

import (
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// Plan is the resolved IR handed to Backend.Execute: names bound to the schema
// model, target relation resolved, capability decisions already made, and (on
// emulating backends) RLS predicates already injected into the Query. Execute
// receives a Plan, never a SQL string, so a non-SQL backend can lower it to its
// own engine operations. See spec 03/05.
//
// A plan is either a table operation (Query/Rel set) or a function call
// (Call/Func set), never both.
type Plan struct {
	// Query is the resolved query. Relation.Schema and Relation.Name are bound.
	Query *Query
	// Rel is the resolved target relation from the schema model.
	Rel *schema.Relation
	// Call is the resolved RPC call, set for a /rpc/<fn> request.
	Call *Call
	// Func is the resolved function descriptor for an RPC call.
	Func *rpc.Function
	// ReadOnly is true for GET/HEAD and non-volatile RPC; it selects the
	// backend's transaction mode.
	ReadOnly bool
}
