package sqlserver

import (
	"context"
	"database/sql"

	"github.com/tamnd/dbrest/schema"
)

// Introspect builds the unified schema model from SQL Server's INFORMATION_SCHEMA.
// Only BASE TABLEs and VIEWs in the current schema are surfaced. Columns, primary
// keys, and foreign keys are read from KEY_COLUMN_USAGE and REFERENTIAL_CONSTRAINTS.
func (b *Backend) Introspect(ctx context.Context) (*schema.Model, error) {
	return b.introspect(ctx)
}

func (b *Backend) introspect(ctx context.Context) (*schema.Model, error) {
	rels, err := b.relationNames(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]*schema.Relation, 0, len(rels))
	for _, r := range rels {
		cols, pk, err := b.columns(ctx, r.name)
		if err != nil {
			return nil, err
		}
		fks, err := b.foreignKeys(ctx, r.name)
		if err != nil {
			return nil, err
		}
		out = append(out, &schema.Relation{
			Name:        r.name,
			Kind:        r.kind,
			Columns:     cols,
			PrimaryKey:  pk,
			ForeignKeys: fks,
		})
	}
	return schema.NewModel(out), nil
}

type relInfo struct {
	name string
	kind schema.Kind
}

func (b *Backend) relationNames(ctx context.Context) ([]relInfo, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT TABLE_NAME, TABLE_TYPE
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = SCHEMA_NAME()
		  AND TABLE_TYPE IN ('BASE TABLE', 'VIEW')
		ORDER BY TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []relInfo
	for rows.Next() {
		var name, ttype string
		if err := rows.Scan(&name, &ttype); err != nil {
			return nil, err
		}
		kind := schema.KindTable
		if ttype == "VIEW" {
			kind = schema.KindView
		}
		out = append(out, relInfo{name: name, kind: kind})
	}
	return out, rows.Err()
}

func (b *Backend) columns(ctx context.Context, table string) ([]*schema.Column, []string, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT
			c.COLUMN_NAME,
			c.DATA_TYPE,
			c.IS_NULLABLE,
			c.COLUMN_DEFAULT,
			CASE WHEN k.COLUMN_NAME IS NOT NULL THEN 1 ELSE 0 END AS is_pk,
			ISNULL(k.ORDINAL_POSITION, 0) AS pk_ord
		FROM INFORMATION_SCHEMA.COLUMNS c
		LEFT JOIN (
			SELECT kcu.COLUMN_NAME, kcu.ORDINAL_POSITION
			FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
			JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
				ON  tc.CONSTRAINT_NAME   = kcu.CONSTRAINT_NAME
				AND tc.TABLE_SCHEMA      = kcu.TABLE_SCHEMA
				AND tc.TABLE_NAME        = kcu.TABLE_NAME
				AND tc.CONSTRAINT_TYPE   = 'PRIMARY KEY'
			WHERE kcu.TABLE_SCHEMA = SCHEMA_NAME()
			  AND kcu.TABLE_NAME   = @p1
		) k ON k.COLUMN_NAME = c.COLUMN_NAME
		WHERE c.TABLE_SCHEMA = SCHEMA_NAME()
		  AND c.TABLE_NAME   = @p1
		ORDER BY c.ORDINAL_POSITION`,
		sql.Named("p1", table))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type colRow struct {
		col   *schema.Column
		isPK  bool
		pkOrd int
	}

	var colRows []colRow
	for rows.Next() {
		var name, dataType, isNullable string
		var colDefault sql.NullString
		var isPK, pkOrd int
		if err := rows.Scan(&name, &dataType, &isNullable, &colDefault, &isPK, &pkOrd); err != nil {
			return nil, nil, err
		}
		hasDefault := isPK == 1 || colDefault.Valid
		col := &schema.Column{
			Name:       name,
			Type:       sqlServerCanonicalType(dataType),
			Nullable:   isNullable == "YES",
			HasDefault: hasDefault,
		}
		colRows = append(colRows, colRow{col: col, isPK: isPK == 1, pkOrd: pkOrd})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	cols := make([]*schema.Column, len(colRows))
	pkByOrd := make(map[int]string)
	for i, r := range colRows {
		cols[i] = r.col
		if r.isPK {
			pkByOrd[r.pkOrd] = r.col.Name
		}
	}

	var pk []string
	for i := 1; i <= len(pkByOrd); i++ {
		if name, ok := pkByOrd[i]; ok {
			pk = append(pk, name)
		}
	}

	return cols, pk, nil
}

func (b *Backend) foreignKeys(ctx context.Context, table string) ([]*schema.ForeignKey, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT
			kcu.COLUMN_NAME,
			kcu2.TABLE_NAME   AS ref_table,
			kcu2.COLUMN_NAME  AS ref_col,
			kcu.CONSTRAINT_NAME
		FROM INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS rc
		JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
			ON  kcu.CONSTRAINT_NAME  = rc.CONSTRAINT_NAME
			AND kcu.TABLE_SCHEMA     = rc.CONSTRAINT_SCHEMA
		JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu2
			ON  kcu2.CONSTRAINT_NAME = rc.UNIQUE_CONSTRAINT_NAME
			AND kcu2.ORDINAL_POSITION = kcu.ORDINAL_POSITION
		WHERE kcu.TABLE_SCHEMA = SCHEMA_NAME()
		  AND kcu.TABLE_NAME   = @p1
		ORDER BY kcu.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`,
		sql.Named("p1", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type fkRow struct {
		col    string
		refTab string
		refCol string
		name   string
	}
	var fkRows []fkRow
	for rows.Next() {
		var r fkRow
		if err := rows.Scan(&r.col, &r.refTab, &r.refCol, &r.name); err != nil {
			return nil, err
		}
		fkRows = append(fkRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	fkMap := make(map[string]*schema.ForeignKey)
	fkOrder := make([]string, 0)
	for _, r := range fkRows {
		fk, ok := fkMap[r.name]
		if !ok {
			fk = &schema.ForeignKey{RefRelation: r.refTab}
			fkMap[r.name] = fk
			fkOrder = append(fkOrder, r.name)
		}
		fk.Columns = append(fk.Columns, r.col)
		fk.RefColumns = append(fk.RefColumns, r.refCol)
	}

	out := make([]*schema.ForeignKey, 0, len(fkOrder))
	for _, name := range fkOrder {
		out = append(out, fkMap[name])
	}
	return out, nil
}
