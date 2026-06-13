package postgres

import (
	"context"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
)

// loadFunctionVolatility reads pg_proc.provolatile for every function in the
// exposed schemas into a map keyed by "schema.name", so the native RPC path can
// pick the transaction access mode from volatility the way PostgREST does:
// a STABLE or IMMUTABLE function runs read-only even when called with POST, only
// a VOLATILE function runs read-write. When a name has several overloads with
// differing volatility, the most write-capable one wins (Volatile), so a POST
// never loses its write transaction; the cost is only that a read-only overload
// sharing a name with a volatile one runs read-write, the safe direction.
func (b *Backend) loadFunctionVolatility(ctx context.Context, schemas []string) (map[string]rpc.Volatility, error) {
	const q = `
SELECT n.nspname, p.proname, p.provolatile::text
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = ANY($1)`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]rpc.Volatility{}
	for rows.Next() {
		var nsp, name, vol string
		if err := rows.Scan(&nsp, &name, &vol); err != nil {
			return nil, err
		}
		v := volatilityFromChar(vol)
		key := nsp + "." + name
		// Volatile wins for a name with mixed overloads, so a write overload keeps
		// its read-write transaction. Volatile is the zero value, so an existing
		// Volatile entry is never downgraded.
		if cur, ok := out[key]; ok && cur == rpc.Volatile {
			continue
		}
		out[key] = v
	}
	return out, rows.Err()
}

// volatilityFromChar maps a pg_proc.provolatile char to the portable Volatility.
// Anything unexpected falls back to Volatile, the safe (read-write) classification.
func volatilityFromChar(c string) rpc.Volatility {
	switch c {
	case "i":
		return rpc.Immutable
	case "s":
		return rpc.Stable
	default:
		return rpc.Volatile
	}
}

// loadFunctionReturns reads each function's return shape (pg_proc.proretset and
// the return type's class) for every function in the exposed schemas, keyed by
// "schema.name". The native RPC renderer uses it to shape a result the way
// PostgREST does instead of guessing from column names: a SETOF scalar function
// renders as a JSON array of bare values, a function returning a single composite
// row renders as one object, a SETOF or TABLE function as an array of objects, a
// scalar function as the bare value, and a void function as a null body.
//
// When a name has several overloads with differing return shapes the first by oid
// wins; resolving the exact overload's shape needs full parameter introspection
// (the native registry, a later slice), and same-named overloads almost always
// share a return shape in practice.
func (b *Backend) loadFunctionReturns(ctx context.Context, schemas []string) (map[string]rpc.ReturnShape, error) {
	const q = `
SELECT n.nspname, p.proname, p.proretset, p.prorettype, t.typtype::text, t.typname
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  JOIN pg_type t ON t.oid = p.prorettype
 WHERE n.nspname = ANY($1)
 ORDER BY n.nspname, p.proname, p.oid`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]rpc.ReturnShape{}
	for rows.Next() {
		var (
			nsp, name, typtype, typname string
			retset                      bool
			rettype                     uint32
		)
		if err := rows.Scan(&nsp, &name, &retset, &rettype, &typtype, &typname); err != nil {
			return nil, err
		}
		key := nsp + "." + name
		if _, ok := out[key]; ok {
			continue // first overload by oid wins
		}
		out[key] = returnShapeFor(retset, rettype, typtype, typname)
	}
	return out, rows.Err()
}

// PostgreSQL OIDs for the pseudo-types the return shape keys on.
const (
	oidRecord = 2249 // RETURNS record / RETURNS TABLE(...) carry this prorettype
	oidVoid   = 2278 // RETURNS void
)

// returnShapeFor maps a function's pg_proc return facts to a portable ReturnShape.
// A composite return (pg_type.typtype 'c') or a record (the TABLE/OUT-parameter
// form) is object-shaped; everything else is scalar-shaped. proretset then decides
// array vs single: a set of objects is a table, a single object is one object; a
// set of scalars is a setof, a single scalar is a scalar. Type carries the return
// type name so the scalar renderer can embed a json/jsonb value verbatim.
func returnShapeFor(retset bool, rettype uint32, typtype, typname string) rpc.ReturnShape {
	if rettype == oidVoid {
		return rpc.ReturnShape{Kind: rpc.ReturnVoid}
	}
	composite := typtype == "c" || rettype == oidRecord
	switch {
	case retset && composite:
		return rpc.ReturnShape{Kind: rpc.ReturnTable}
	case retset:
		return rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: typname}
	case composite:
		return rpc.ReturnShape{Kind: rpc.ReturnObject}
	default:
		return rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: typname}
	}
}

// nativeFunc builds the descriptor for a native RPC call from the introspected
// catalog: the return shape (funcRet) and volatility (funcVol), keyed by the
// call's schema and name. Query stays nil, which marks the function native so the
// executor keeps lowering it through the literal-splice path; the descriptor only
// gives the renderer and the access-mode check a real return kind instead of a
// column-name guess. It returns nil when the function was not introspected (for
// example a search-path schema outside the exposed set), leaving the legacy
// heuristic in place rather than asserting a shape that may be wrong.
func (b *Backend) nativeFunc(c *ir.Call, schema string) *rpc.Function {
	if b.funcRet == nil {
		return nil
	}
	key := schema + "." + c.Function.Name
	shape, ok := b.funcRet[key]
	if !ok {
		return nil
	}
	fn := &rpc.Function{Name: c.Function.Name, Returns: shape}
	if v, ok := b.funcVol[key]; ok {
		fn.Volatility = v
	}
	return fn
}

// nativeCallReadOnly reports whether a native RPC call should run in a read-only
// transaction. A GET/HEAD is already read-only (plan.ReadOnly). For a POST, it
// downgrades to read-only when the resolved function is known to be STABLE or
// IMMUTABLE, matching PostgREST's access-mode table; an unknown function (not yet
// introspected) keeps the method-derived mode so a write still runs read-write.
func (b *Backend) nativeCallReadOnly(plan *ir.Plan, rc *reqctx.Context) bool {
	if plan.ReadOnly {
		return true
	}
	if b.funcVol == nil {
		return false
	}
	key := b.callSchema(rc) + "." + plan.Call.Function.Name
	v, ok := b.funcVol[key]
	return ok && v.ReadOnly()
}
