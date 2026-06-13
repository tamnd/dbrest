package postgres

import (
	"context"

	"github.com/tamnd/dbrest/schema"
)

// loadViewColumns maps every exposed view's output columns to the base-relation
// columns they project, the data the model needs to carry base-table foreign keys
// onto views (spec 09, finding 03-P09). PostgreSQL records the origin of each view
// output column directly in the view's rewrite rule: every TARGETENTRY of the
// _RETURN rule carries resorigtbl (the OID of the relation the column came from)
// and resorigcol (its attribute number there), which survive column renames and
// point at the immediate source even through a view-over-view chain. We read those
// out of pg_rewrite.ev_action, the rule's parsed query tree rendered as text, and
// resolve them to names through pg_attribute.
//
// The mapping is per immediate source, not the ultimate base table: a view over a
// view points at the inner view, and the model's projection runs to a fixpoint so
// the inner view's inherited keys are available when the outer view projects.
// Columns with resorigtbl 0 (an expression or a literal, not a plain column
// reference) are skipped, so an expression column simply carries no mapping and
// inherits nothing. Set-operation views (UNION/INTERSECT/EXCEPT) are skipped
// entirely, matching PostgREST, because a set operation can combine rows from
// unrelated relations under one output column, so no single base column owns it.
func (b *Backend) loadViewColumns(ctx context.Context, schemas []string) (map[uint32][]schema.ViewColumn, error) {
	// resname (the output column name) escapes spaces and other specials with a
	// backslash in the node-tree text, so it is matched as a run of escaped chars
	// or non-spaces and discarded; the output name is read from pg_attribute by
	// resno instead, which avoids unescaping. resorigtbl and resorigcol are the
	// fields we keep. The set-operation guard drops any view whose rule carries a
	// SETOPERATIONSTMT node (an empty :setOperations <> stays).
	const q = `
WITH views AS (
  SELECT c.oid AS view_oid, rw.ev_action::text AS act
    FROM pg_rewrite rw
    JOIN pg_class c ON c.oid = rw.ev_class
    JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relkind IN ('v','m')
     AND n.nspname = ANY($1)
     AND rw.rulename = '_RETURN'
     AND rw.ev_action::text NOT LIKE '%:setOperations {%'
),
entries AS (
  SELECT view_oid,
         (m[1])::int AS resno,
         (m[2])::oid AS resorigtbl,
         (m[3])::int AS resorigcol
    FROM views,
         regexp_matches(act,
           ':resno (\d+) :resname (?:\\.|[^ ])+ :ressortgroupref \d+ :resorigtbl (\d+) :resorigcol (\d+) :resjunk false',
           'g') AS m
   WHERE (m[2])::oid <> 0
)
SELECT e.view_oid,
       va.attname  AS view_column,
       bn.nspname  AS base_schema,
       bc.relname  AS base_relation,
       ba.attname  AS base_column
  FROM entries e
  JOIN pg_attribute va ON va.attrelid = e.view_oid    AND va.attnum = e.resno
  JOIN pg_class     bc ON bc.oid = e.resorigtbl
  JOIN pg_namespace bn ON bn.oid = bc.relnamespace
  JOIN pg_attribute ba ON ba.attrelid = e.resorigtbl  AND ba.attnum = e.resorigcol
 ORDER BY e.view_oid, e.resno`

	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint32][]schema.ViewColumn{}
	for rows.Next() {
		var viewOID uint32
		var vc schema.ViewColumn
		if err := rows.Scan(&viewOID, &vc.Name, &vc.BaseSchema, &vc.BaseRelation, &vc.BaseColumn); err != nil {
			return nil, err
		}
		out[viewOID] = append(out[viewOID], vc)
	}
	return out, rows.Err()
}
