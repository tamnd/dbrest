package postgres

import (
	"context"

	"github.com/tamnd/dbrest/rpc"
)

// loadFunctionRegistry reads the full callable signature of every function in the
// exposed schemas from pg_proc and materializes one rpc.Registry per schema. This
// is the function half of the schema cache PostgREST keeps: with it the native RPC
// path resolves overloads (raising PGRST202 for no match and PGRST203 for an
// ambiguous one), partitions GET arguments from result filters by parameter name,
// and runs a POST to a STABLE or IMMUTABLE function read-only, all through the same
// planner code the portable registry uses. The descriptors carry Query nil, which
// keeps the executor lowering them through the native splice path.
//
// One row per input argument would be simpler to scan but loses the per-function
// grouping; instead each function row carries its input names and type names as
// arrays, reconstructed in SQL so the OUT and TABLE columns (which are not call
// arguments) are filtered out by argument mode. proargtypes already lists only the
// input arguments in order, so the type array needs no filtering; proargnames lists
// every argument, so it is filtered to the input modes ('i' in, 'b' inout, 'v'
// variadic) when proargmodes is present.
func (b *Backend) loadFunctionRegistry(ctx context.Context, schemas []string) (map[string]rpc.Registry, error) {
	const q = `
SELECT n.nspname, p.proname,
       p.provolatile::text,
       p.proretset, p.prorettype, rt.typtype::text, rt.typname,
       p.pronargs, p.pronargdefaults, (p.provariadic <> 0),
       (SELECT array_agg(tt.typname ORDER BY u.ord)
          FROM unnest(p.proargtypes) WITH ORDINALITY AS u(typoid, ord)
          JOIN pg_type tt ON tt.oid = u.typoid) AS in_types,
       CASE WHEN p.proargmodes IS NULL THEN p.proargnames
            ELSE (SELECT array_agg(nm ORDER BY ord)
                    FROM unnest(p.proargnames, p.proargmodes) WITH ORDINALITY AS m(nm, mode, ord)
                   WHERE mode IN ('i', 'b', 'v')) END AS in_names,
       (p.prosecdef) AS secdef,
       COALESCE(d.description, '') AS comment
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  JOIN pg_type rt ON rt.oid = p.prorettype
  LEFT JOIN pg_description d ON d.objoid = p.oid AND d.classoid = 'pg_proc'::regclass AND d.objsubid = 0
 WHERE n.nspname = ANY($1)
   AND p.prokind = 'f'
 ORDER BY n.nspname, p.proname, p.oid`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySchema := map[string][]*rpc.Function{}
	for rows.Next() {
		var (
			nsp, name, vol, retTyptype, retTypname string
			retset, variadic, secdef               bool
			rettype                                uint32
			nargs, ndefaults                       int
			inTypes                                []string
			inNames                                []*string
			comment                                string
		)
		if err := rows.Scan(&nsp, &name, &vol, &retset, &rettype, &retTyptype, &retTypname,
			&nargs, &ndefaults, &variadic, &inTypes, &inNames, &secdef, &comment); err != nil {
			return nil, err
		}
		fn := &rpc.Function{
			Name:       name,
			Returns:    returnShapeFor(retset, rettype, retTyptype, retTypname),
			Volatility: volatilityFromChar(vol),
			Comment:    comment,
		}
		if secdef {
			fn.Security = rpc.Definer
		}
		fn.Params = buildParams(nargs, ndefaults, variadic, inTypes, inNames)
		bySchema[nsp] = append(bySchema[nsp], fn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]rpc.Registry, len(bySchema))
	for schema, fns := range bySchema {
		out[schema] = rpc.NewStaticRegistry(fns)
	}
	return out, nil
}

// buildParams reconstructs a function's input parameters in signature order from
// the pg_proc facts. proargtypes (inTypes) holds exactly the input argument types
// in order, so its length is the input arity; inNames is the input names in the
// same order, possibly nil (no names at all) or holding a nil entry for an unnamed
// argument. The trailing ndefaults inputs are optional, and a variadic function's
// last input collects its trailing arguments. A function whose single input is
// unnamed and of a raw-body type takes the whole request body as that argument.
func buildParams(nargs, ndefaults int, variadic bool, inTypes []string, inNames []*string) []rpc.Param {
	if len(inTypes) == 0 {
		return nil
	}
	params := make([]rpc.Param, 0, len(inTypes))
	for i, typ := range inTypes {
		name := ""
		if i < len(inNames) && inNames[i] != nil {
			name = *inNames[i]
		}
		params = append(params, rpc.Param{
			Name:     name,
			Type:     typ,
			Optional: i >= len(inTypes)-ndefaults,
			Variadic: variadic && i == len(inTypes)-1,
		})
	}
	// The single-unnamed-parameter form: one input, no name, and a body-shaped type.
	// PostgREST binds the whole request body to it rather than reading the body as a
	// JSON object of named arguments.
	if len(params) == 1 && params[0].Name == "" && isRawBodyType(inTypes[0]) {
		params[0].RawBody = true
		// The lowering references the argument by name, so give the unnamed parameter
		// a stable synthetic name; the wire contract is positional, so the spelling is
		// internal only.
		params[0].Name = "__raw_body"
	}
	return params
}

// isRawBodyType reports whether a type can stand as a single-unnamed-parameter raw
// body. PostgREST accepts json, jsonb, text, xml, and bytea in this position: the
// request body is bound whole, decoded by Content-Type.
func isRawBodyType(typname string) bool {
	switch typname {
	case "json", "jsonb", "text", "xml", "bytea":
		return true
	}
	return false
}
