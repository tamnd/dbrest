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

// loadComputedRels maps every exposed relation's OID to its computed
// relationships: functions taking the relation's row type and returning rows of
// another exposed relation, exposed as embeddable edges (PostgREST computed
// relationships, the escape hatch for recursive embeds, spec 11). The first
// argument is the parent relation's composite row type (pg_class.reltype); the
// return type is another relation's composite type, set-returning for a to-many
// edge and a single row for to-one. A function returning RETURNS TABLE(...) or a
// bare scalar is not a computed relationship: its return type is a pseudo or base
// type, not a relation's composite, so it never matches here (a scalar return is a
// computed field instead, read by loadComputedFields).
//
// The function must live in the same schema as the parent relation, matching
// PostgREST, which exposes a computed relationship defined alongside its table.
func (b *Backend) loadComputedRels(ctx context.Context, schemas []string) (map[uint32][]schema.ComputedRel, error) {
	const q = `
SELECT pc.oid AS parent_oid,
       n.nspname  AS fn_schema,
       p.proname  AS fn_name,
       p.proretset,
       tn.nspname AS target_schema,
       tc.relname AS target_name
  FROM pg_proc p
  JOIN pg_namespace n  ON n.oid = p.pronamespace
  JOIN pg_type pat     ON pat.oid = p.proargtypes[0]
  JOIN pg_class pc     ON pc.reltype = pat.oid AND pc.relkind IN ('r','v','m','f','p')
  JOIN pg_namespace pn ON pn.oid = pc.relnamespace
  JOIN pg_type rt      ON rt.oid = p.prorettype AND rt.typtype = 'c'
  JOIN pg_class tc     ON tc.reltype = rt.oid AND tc.relkind IN ('r','v','m','f','p')
  JOIN pg_namespace tn ON tn.oid = tc.relnamespace
 WHERE n.nspname  = ANY($1)
   AND pn.nspname = ANY($1)
   AND tn.nspname = ANY($1)
   AND n.nspname = pn.nspname
   AND p.prokind = 'f'
   AND p.pronargs = 1
   AND p.provariadic = 0
 ORDER BY pc.oid, p.proname`

	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint32][]schema.ComputedRel{}
	for rows.Next() {
		var parentOID uint32
		var setof bool
		var cr schema.ComputedRel
		if err := rows.Scan(&parentOID, &cr.FuncSchema, &cr.Name, &setof, &cr.TargetSchema, &cr.TargetName); err != nil {
			return nil, err
		}
		cr.Card = schema.CardToOne
		if setof {
			cr.Card = schema.CardToMany
		}
		out[parentOID] = append(out[parentOID], cr)
	}
	return out, rows.Err()
}
