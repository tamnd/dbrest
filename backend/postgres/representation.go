package postgres

import (
	"context"

	"github.com/tamnd/dbrest/schema"
)

// loadRepresentations maps each domain type's OID to its data-representation cast
// set: the functions PostgREST applies to reformat a column of that domain on the
// wire (domain representations, spec 11). A data representation is a domain over a
// base type plus casts registered in pg_cast: a cast from the domain to json
// formats the value for a response, a cast from json to the domain parses a write
// body, and a cast from text to the domain parses a query-string filter literal.
//
// PostgreSQL ignores these casts in the `::` operator (it warns "cast will be
// ignored because the source/target data type is a domain"), so the cast cannot be
// applied as col::json. The cast function is what does the work, and that is what
// this reads: the introspector records the cast function per direction so the
// compiler calls it directly. Only function-method casts ('f') carry a function;
// the rare binary-coercible domain cast has none and drives no representation.
func (b *Backend) loadRepresentations(ctx context.Context, schemas []string) (map[uint32]*schema.Representation, error) {
	const q = `
SELECT dt.oid AS domain_oid,
       fn.nspname AS fn_schema,
       p.proname  AS fn_name,
       st.typname AS src_name,
       stt.typtype AS src_typtype,
       tt.typname AS tgt_name,
       ttt.typtype AS tgt_typtype
  FROM pg_cast c
  JOIN pg_proc p       ON p.oid = c.castfunc
  JOIN pg_namespace fn ON fn.oid = p.pronamespace
  JOIN pg_type stt     ON stt.oid = c.castsource
  JOIN pg_type ttt     ON ttt.oid = c.casttarget
  JOIN pg_type st      ON st.oid = c.castsource
  JOIN pg_type tt      ON tt.oid = c.casttarget
  JOIN pg_type dt      ON dt.oid = (CASE WHEN stt.typtype = 'd' THEN c.castsource ELSE c.casttarget END)
  JOIN pg_namespace dn ON dn.oid = dt.typnamespace
 WHERE c.castmethod = 'f'
   AND dt.typtype = 'd'
   AND dn.nspname = ANY($1)
   AND (
        (stt.typtype = 'd' AND tt.typname IN ('json', 'jsonb'))
     OR (st.typname IN ('json', 'jsonb') AND ttt.typtype = 'd')
     OR (st.typname = 'text' AND ttt.typtype = 'd')
   )`

	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint32]*schema.Representation{}
	for rows.Next() {
		var domainOID uint32
		var fnSchema, fnName, srcName, srcTyptype, tgtName, tgtTyptype string
		if err := rows.Scan(&domainOID, &fnSchema, &fnName, &srcName, &srcTyptype, &tgtName, &tgtTyptype); err != nil {
			return nil, err
		}
		rep := out[domainOID]
		if rep == nil {
			rep = &schema.Representation{}
			out[domainOID] = rep
		}
		ref := schema.FuncRef{Schema: fnSchema, Name: fnName}
		switch {
		case srcTyptype == "d" && (tgtName == "json" || tgtName == "jsonb"):
			rep.ToJSON = ref // domain -> json: format on read
		case (srcName == "json" || srcName == "jsonb") && tgtTyptype == "d":
			rep.FromJSON = ref // json -> domain: parse a write value
		case srcName == "text" && tgtTyptype == "d":
			rep.FromText = ref // text -> domain: parse a filter literal
		}
	}
	return out, rows.Err()
}
