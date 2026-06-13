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
