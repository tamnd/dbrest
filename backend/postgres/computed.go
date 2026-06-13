package postgres

import (
	"context"

	"github.com/tamnd/dbrest/schema"
)

// loadComputedFields maps every exposed relation's OID to the computed fields it
// carries: functions that take the relation's row type and return a scalar,
// exposed as virtual columns a client can select, filter, and order by (PostgREST
// computed fields, spec 11). PostgreSQL records a function's first argument type
// in pg_proc.proargtypes; when that type is a relation's composite row type
// (pg_class.reltype), the function is a candidate. A scalar return (pg_type.typtype
// other than 'c') marks it a computed field; a composite or set-returning function
// over the same row type is a computed relationship instead and is read elsewhere.
//
// The function must live in the same schema as the relation, matching PostgREST,
// which only exposes a computed field defined alongside its table. A real column of
// the same name takes precedence: the model indexes columns first, so a function
// shadowing a stored column is simply never reached.
func (b *Backend) loadComputedFields(ctx context.Context, schemas []string) (map[uint32][]schema.ComputedField, error) {
	const q = `
SELECT cls.oid AS rel_oid,
       n.nspname AS fn_schema,
       p.proname AS fn_name,
       format_type(p.prorettype, NULL) AS ret_type
  FROM pg_proc p
  JOIN pg_namespace n   ON n.oid = p.pronamespace
  JOIN pg_type at       ON at.oid = p.proargtypes[0]
  JOIN pg_class cls     ON cls.reltype = at.oid AND cls.relkind IN ('r','v','m','f','p')
  JOIN pg_namespace cn  ON cn.oid = cls.relnamespace
  JOIN pg_type rt       ON rt.oid = p.prorettype
 WHERE n.nspname = ANY($1)
   AND cn.nspname = ANY($1)
   AND n.nspname = cn.nspname
   AND p.prokind = 'f'
   AND p.pronargs = 1
   AND NOT p.proretset
   AND p.provariadic = 0
   AND rt.typtype <> 'c'
 ORDER BY cls.oid, p.proname`

	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint32][]schema.ComputedField{}
	for rows.Next() {
		var relOID uint32
		var cf schema.ComputedField
		var retType string
		if err := rows.Scan(&relOID, &cf.FuncSchema, &cf.Name, &retType); err != nil {
			return nil, err
		}
		cf.Type = canonicalType(retType)
		out[relOID] = append(out[relOID], cf)
	}
	return out, rows.Err()
}
