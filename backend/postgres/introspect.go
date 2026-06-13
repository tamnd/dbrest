package postgres

import (
	"context"
	"strings"

	"github.com/tamnd/dbrest/schema"
)

// Introspect builds the unified schema model from PostgreSQL's system catalogs.
// The exposed schemas come from b.searchPath; if none are configured, only the
// default search_path ($user, public) is used. Only ordinary tables and views are
// exposed; sequences, materialized views, and internal catalogs are omitted.
// Columns, primary keys, and foreign keys are read from pg_attribute and
// pg_constraint. See spec 08.
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

	// Function volatility drives the native RPC transaction access mode (a STABLE
	// or IMMUTABLE function runs read-only even on POST), so it is loaded here with
	// the rest of the catalog and refreshed whenever the model is rebuilt.
	vol, err := b.loadFunctionVolatility(ctx, schemas)
	if err != nil {
		return nil, err
	}
	b.funcVol = vol

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
		cols, pk, err := b.columns(ctx, r.oid)
		if err != nil {
			return nil, err
		}
		out = append(out, &schema.Relation{
			Schema:      r.schemaName,
			Name:        r.name,
			Kind:        r.kind,
			Columns:     cols,
			PrimaryKey:  pk,
			ForeignKeys: fksByRel[r.oid],
		})
	}
	return schema.NewModel(out), nil
}

type relInfo struct {
	oid        uint32
	schemaName string
	name       string
	kind       schema.Kind
}

func (b *Backend) relationNames(ctx context.Context, schemas []string) ([]relInfo, error) {
	// Build a literal array of quoted schema names for the ANY(...) test.
	q := `
SELECT c.oid, n.nspname, c.relname,
       CASE c.relkind WHEN 'v' THEN 'v' ELSE 't' END AS kind
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r','v','p')
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
		if err := rows.Scan(&r.oid, &r.schemaName, &r.name, &kindStr); err != nil {
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

func (b *Backend) columns(ctx context.Context, relOID uint32) ([]*schema.Column, []string, error) {
	// pg_attribute carries every attribute including system columns (attnum < 0)
	// and dropped columns (attisdropped). We want only live user columns.
	// pg_constraint with contype='p' gives the primary-key columns in confkey order
	// via unnest; the conkey[] entries are attribute numbers matching attnum.
	colQ := `
SELECT a.attname, format_type(a.atttypid, a.atttypmod),
       NOT a.attnotnull AS nullable,
       pg_get_expr(d.adbin, d.adrelid) IS NOT NULL AS has_default,
       a.attnum
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
		var name, pgType string
		var nullable, hasDef bool
		var attnum int
		if err := rows.Scan(&name, &pgType, &nullable, &hasDef, &attnum); err != nil {
			return nil, nil, err
		}
		cols = append(cols, &schema.Column{
			Name:       name,
			Type:       canonicalType(pgType),
			Nullable:   nullable,
			HasDefault: hasDef,
			Position:   attnum,
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
