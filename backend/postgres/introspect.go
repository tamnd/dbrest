package postgres

import (
	"context"
	"strings"

	"github.com/tamnd/dbrest/schema"
)

// Introspect builds the unified schema model from PostgreSQL's system catalogs.
// The exposed schemas come from b.searchPath; if none are configured, only the
// default search_path ($user, public) is used. The exposed relations mirror
// PostgREST's schema cache: ordinary tables, views, materialized views, foreign
// tables, and partitioned parents (leaf partitions excluded). Columns, primary
// keys, unique sets, foreign keys, identity flags, and comments are read from
// pg_attribute, pg_constraint, pg_index, and pg_description. See spec 08.
func (b *Backend) Introspect(ctx context.Context) (*schema.Model, error) {
	schemas := b.searchPath
	if len(schemas) == 0 {
		schemas = []string{"public"}
	}

	rels, err := b.relationNames(ctx, schemas)
	if err != nil {
		return nil, err
	}
	fks, err := b.foreignKeys(ctx, schemas)
	if err != nil {
		return nil, err
	}
	fksByRel := groupFKs(fks)

	// View output columns mapped to their base-relation columns, so the model can
	// project base-table foreign keys onto views and embedding works through a view
	// the same way it does through its base table (spec 09).
	viewCols, err := b.loadViewColumns(ctx, schemas)
	if err != nil {
		return nil, err
	}

	// Computed fields are functions taking a relation's row type and returning a
	// scalar, exposed as virtual columns selectable, filterable, and orderable like
	// stored ones (spec 11). They are read here with the rest of the catalog so they
	// refresh on every rebuild and attach to their relation below.
	computed, err := b.loadComputedFields(ctx, schemas)
	if err != nil {
		return nil, err
	}

	// Computed relationships are functions taking a relation's row type and
	// returning rows of another relation, exposed as embeddable edges (spec 11, the
	// escape hatch for recursive embeds). Read here with the rest of the catalog and
	// attached to their parent relation below.
	computedRels, err := b.loadComputedRels(ctx, schemas)
	if err != nil {
		return nil, err
	}

	// Data representations are domain types whose casts to and from json/text
	// reformat a column's wire value (spec 11). Read once, keyed by domain type OID,
	// and attached to each column of that domain as the columns are loaded below.
	reps, err := b.loadRepresentations(ctx, schemas)
	if err != nil {
		return nil, err
	}

	// Function volatility drives the native RPC transaction access mode (a STABLE
	// or IMMUTABLE function runs read-only even on POST), so it is loaded here with
	// the rest of the catalog and refreshed whenever the model is rebuilt.
	vol, err := b.loadFunctionVolatility(ctx, schemas)
	if err != nil {
		return nil, err
	}
	b.funcVol = vol

	// Function return shapes drive the native RPC renderer (a SETOF scalar renders
	// as an array of bare values, a single composite as one object), loaded with the
	// rest of the catalog and refreshed on every rebuild like volatility.
	ret, err := b.loadFunctionReturns(ctx, schemas)
	if err != nil {
		return nil, err
	}
	b.funcRet = ret

	// The native function registry is the function half of PostgREST's schema cache:
	// full signatures per schema so the native RPC path resolves overloads, raises
	// PGRST202/PGRST203, and partitions GET arguments from result filters through the
	// shared planner. Loaded with the catalog and refreshed on every rebuild.
	reg, err := b.loadFunctionRegistry(ctx, schemas)
	if err != nil {
		return nil, err
	}
	b.funcReg = reg

	// Impersonated-role settings (ALTER ROLE ... SET) are replayed per request as
	// transaction-scoped settings, so they are loaded with the catalog and
	// refreshed on every rebuild, the same lifecycle PostgREST gives them.
	rs, iso, err := b.loadRoleSettings(ctx)
	if err != nil {
		return nil, err
	}
	b.roleSettings = rs
	b.roleIsolation = iso

	// Function SET clauses (pg_proc.proconfig) drive db-hoisted-tx-settings: an RPC
	// call hoists the named settings to the transaction. Loaded with the catalog so
	// it refreshes on every rebuild, like role settings and volatility.
	pc, err := b.loadFunctionProconfig(ctx, schemas)
	if err != nil {
		return nil, err
	}
	b.funcProconfig = pc

	var out []*schema.Relation
	for _, r := range rels {
		cols, pk, err := b.columns(ctx, r.oid, reps)
		if err != nil {
			return nil, err
		}
		uniq, err := b.uniques(ctx, r.oid)
		if err != nil {
			return nil, err
		}
		out = append(out, &schema.Relation{
			Schema:       r.schemaName,
			Name:         r.name,
			Kind:         r.kind,
			Comment:      r.comment,
			Columns:      cols,
			PrimaryKey:   pk,
			Unique:       uniq,
			ForeignKeys:  fksByRel[r.oid],
			ViewColumns:  viewCols[r.oid],
			Computed:     computed[r.oid],
			ComputedRels: computedRels[r.oid],
		})
	}

	// Schema-level comments feed the OpenAPI info block (title and description),
	// the same source PostgREST uses. They are attached to the model before it is
	// published, alongside the relation and column comments read above.
	comments, err := b.schemaComments(ctx, schemas)
	if err != nil {
		return nil, err
	}
	model := schema.NewModel(out)
	for name, comment := range comments {
		model.SetSchemaComment(name, comment)
	}
	return model, nil
}

type relInfo struct {
	oid        uint32
	schemaName string
	name       string
	kind       schema.Kind
	comment    string
}

func (b *Backend) relationNames(ctx context.Context, schemas []string) ([]relInfo, error) {
	// The relation set mirrors PostgREST's schema cache: ordinary tables ('r'),
	// views ('v'), materialized views ('m'), foreign tables ('f'), and partitioned
	// parents ('p'). Materialized views map to the view kind (read-mostly; a write
	// fails with PostgreSQL's own error, the same passthrough as PostgREST), while
	// foreign tables map to the table kind since an FDW can accept writes.
	// Partitions are excluded with NOT c.relispartition so only the partitioned
	// parent is an endpoint, matching upstream; this drops both leaf partitions
	// ('r') and intermediate sub-partitioned tables ('p').
	q := `
SELECT c.oid, n.nspname, c.relname,
       CASE c.relkind WHEN 'v' THEN 'v' WHEN 'm' THEN 'v' ELSE 't' END AS kind,
       COALESCE(obj_description(c.oid, 'pg_class'), '') AS comment
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r','v','m','f','p')
   AND NOT c.relispartition
   AND n.nspname = ANY($1)
 ORDER BY n.nspname, c.relname`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []relInfo
	for rows.Next() {
		var r relInfo
		var kindStr string
		if err := rows.Scan(&r.oid, &r.schemaName, &r.name, &kindStr, &r.comment); err != nil {
			return nil, err
		}
		if kindStr == "v" {
			r.kind = schema.KindView
		} else {
			r.kind = schema.KindTable
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (b *Backend) columns(ctx context.Context, relOID uint32, reps map[uint32]*schema.Representation) ([]*schema.Column, []string, error) {
	// pg_attribute carries every attribute including system columns (attnum < 0)
	// and dropped columns (attisdropped). We want only live user columns.
	// pg_constraint with contype='p' gives the primary-key columns in confkey order
	// via unnest; the conkey[] entries are attribute numbers matching attnum.
	// atttypid is the column's exact type OID, which carries the representation cast
	// set when the type is a domain.
	colQ := `
SELECT a.attname, format_type(a.atttypid, a.atttypmod),
       NOT a.attnotnull AS nullable,
       pg_get_expr(d.adbin, d.adrelid) IS NOT NULL AS has_default,
       a.attidentity <> '' AS is_identity,
       COALESCE(col_description(a.attrelid, a.attnum), '') AS comment,
       a.attnum, a.atttypid
  FROM pg_attribute a
  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
 WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
 ORDER BY a.attnum`
	rows, err := b.pool.Query(ctx, colQ, relOID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	attByNum := map[int]string{}
	var cols []*schema.Column
	for rows.Next() {
		var name, pgType, comment string
		var nullable, hasDef, isIdentity bool
		var attnum int
		var typOID uint32
		if err := rows.Scan(&name, &pgType, &nullable, &hasDef, &isIdentity, &comment, &attnum, &typOID); err != nil {
			return nil, nil, err
		}
		cols = append(cols, &schema.Column{
			Name: name,
			Type: canonicalType(pgType),
			// An identity column has no pg_attrdef row, so fold it into HasDefault:
			// it is server-generated and never required, the same way PostgREST
			// treats GENERATED AS IDENTITY. Generated (STORED) columns already carry
			// a pg_attrdef row, so has_default covers them.
			Nullable:   nullable,
			HasDefault: hasDef || isIdentity,
			Identity:   isIdentity,
			Comment:    comment,
			Position:   attnum,
			// A column whose type is a domain with representation casts carries the
			// cast set so the compiler reformats it on the wire (spec 11).
			Rep: reps[typOID],
		})
		attByNum[attnum] = name
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// primary key
	pkQ := `
SELECT a.attname
  FROM pg_constraint c
  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
 WHERE c.conrelid = $1 AND c.contype = 'p'
 ORDER BY array_position(c.conkey, a.attnum)`
	pkRows, err := b.pool.Query(ctx, pkQ, relOID)
	if err != nil {
		return nil, nil, err
	}
	defer pkRows.Close()
	var pk []string
	for pkRows.Next() {
		var col string
		if err := pkRows.Scan(&col); err != nil {
			return nil, nil, err
		}
		pk = append(pk, col)
	}
	return cols, pk, pkRows.Err()
}

// uniques reads the relation's unique column sets, the data the model needs to
// see a foreign key as one-to-one (an FK whose columns equal a unique set embeds
// as an object, not an array; spec 09). It reads unique indexes rather than only
// unique constraints, which captures both: every unique constraint is backed by a
// unique index, and a bare CREATE UNIQUE INDEX is just as good a one-to-one
// witness, which is what PostgREST's pks_uniques_cols covers. The primary key is
// excluded (indisprimary) because the model already carries it separately; a
// partial index (indpred) cannot guarantee uniqueness over the whole table, and an
// expression index has a zero attnum, so both are dropped. Only the key columns
// count, not INCLUDE columns past indnkeyatts.
func (b *Backend) uniques(ctx context.Context, relOID uint32) ([][]string, error) {
	q := `
SELECT array_agg(a.attname ORDER BY k.ord)
  FROM pg_index i
  CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
 WHERE i.indrelid = $1
   AND i.indisunique
   AND NOT i.indisprimary
   AND i.indpred IS NULL
   AND k.ord <= i.indnkeyatts
 GROUP BY i.indexrelid
HAVING bool_and(k.attnum > 0)`
	rows, err := b.pool.Query(ctx, q, relOID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][]string
	for rows.Next() {
		var cols []string
		if err := rows.Scan(&cols); err != nil {
			return nil, err
		}
		out = append(out, cols)
	}
	return out, rows.Err()
}

// schemaComments reads the database comment on each exposed schema, the source of
// the OpenAPI info title (first line) and description (rest), the same as
// PostgREST. A schema with no comment is omitted from the map.
func (b *Backend) schemaComments(ctx context.Context, schemas []string) (map[string]string, error) {
	q := `
SELECT n.nspname, obj_description(n.oid, 'pg_namespace')
  FROM pg_namespace n
 WHERE n.nspname = ANY($1)
   AND obj_description(n.oid, 'pg_namespace') IS NOT NULL`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, comment string
		if err := rows.Scan(&name, &comment); err != nil {
			return nil, err
		}
		out[name] = comment
	}
	return out, rows.Err()
}

type fkInfo struct {
	relOID      uint32
	name        string
	fromCols    []string
	refSchema   string
	refRelation string
	refCols     []string
}

func (b *Backend) foreignKeys(ctx context.Context, schemas []string) ([]fkInfo, error) {
	q := `
SELECT c.conrelid,
       c.conname,
       ARRAY(SELECT a.attname FROM pg_attribute a
              WHERE a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
              ORDER BY array_position(c.conkey, a.attnum)) AS from_cols,
       fn.nspname AS ref_schema,
       fc.relname  AS ref_rel,
       ARRAY(SELECT a.attname FROM pg_attribute a
              WHERE a.attrelid = c.confrelid AND a.attnum = ANY(c.confkey)
              ORDER BY array_position(c.confkey, a.attnum)) AS to_cols
  FROM pg_constraint c
  JOIN pg_class rc ON rc.oid = c.conrelid
  JOIN pg_namespace rn ON rn.oid = rc.relnamespace
  JOIN pg_class fc ON fc.oid = c.confrelid
  JOIN pg_namespace fn ON fn.oid = fc.relnamespace
 WHERE c.contype = 'f'
   AND rn.nspname = ANY($1)
 ORDER BY c.conrelid, c.conname`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []fkInfo
	for rows.Next() {
		var fk fkInfo
		if err := rows.Scan(&fk.relOID, &fk.name, &fk.fromCols, &fk.refSchema, &fk.refRelation, &fk.refCols); err != nil {
			return nil, err
		}
		out = append(out, fk)
	}
	return out, rows.Err()
}

func groupFKs(fks []fkInfo) map[uint32][]*schema.ForeignKey {
	out := map[uint32][]*schema.ForeignKey{}
	for _, fk := range fks {
		out[fk.relOID] = append(out[fk.relOID], &schema.ForeignKey{
			Name:        fk.name,
			Columns:     fk.fromCols,
			RefSchema:   fk.refSchema,
			RefRelation: fk.refRelation,
			RefColumns:  fk.refCols,
		})
	}
	return out
}

// canonicalType maps a PostgreSQL format_type string to dbrest's canonical type
// name (spec 16). The canonical names match the PostgreSQL display names PostgREST
// uses in its introspection response, so column types are portable across clients.
func canonicalType(pgType string) string {
	// Strip any length modifier: varchar(255) -> varchar.
	if i := strings.IndexByte(pgType, '('); i > 0 {
		pgType = strings.TrimSpace(pgType[:i])
	}
	switch pgType {
	case "integer", "int", "int4":
		return "integer"
	case "bigint", "int8":
		return "bigint"
	case "smallint", "int2":
		return "smallint"
	case "boolean", "bool":
		return "boolean"
	case "real", "float4":
		return "real"
	case "double precision", "float8":
		return "double precision"
	case "numeric", "decimal":
		return "numeric"
	case "text":
		return "text"
	case "character varying", "varchar":
		return "character varying"
	case "character", "char", "bpchar":
		return "character"
	case "bytea":
		return "bytea"
	case "json":
		return "json"
	case "jsonb":
		return "jsonb"
	case "uuid":
		return "uuid"
	case "date":
		return "date"
	case "time without time zone", "time":
		return "time without time zone"
	case "time with time zone", "timetz":
		return "time with time zone"
	case "timestamp without time zone", "timestamp":
		return "timestamp without time zone"
	case "timestamp with time zone", "timestamptz":
		return "timestamp with time zone"
	case "interval":
		return "interval"
	case "inet":
		return "inet"
	case "cidr":
		return "cidr"
	case "macaddr":
		return "macaddr"
	case "bit", "bit varying", "varbit":
		return pgType
	case "tsvector":
		return "tsvector"
	case "tsquery":
		return "tsquery"
	case "xml":
		return "xml"
	case "point", "line", "lseg", "box", "path", "polygon", "circle":
		return pgType
	case "money":
		return "money"
	case "oid":
		return "oid"
	}
	// Array types come as "type[]"; pass through unchanged so the client sees
	// the engine spelling.
	if strings.HasSuffix(pgType, "[]") {
		return pgType
	}
	// Range types, custom domains, and extension types pass through.
	return pgType
}
