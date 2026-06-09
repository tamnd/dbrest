package mysql

import (
	"context"
	"database/sql"
	"strings"

	"github.com/tamnd/dbrest/schema"
)

// Introspect builds the unified schema model from MySQL's INFORMATION_SCHEMA.
// The exposed schema is the active database (DATABASE()). Only BASE TABLEs and
// VIEWs are surfaced; columns, primary keys, and foreign keys are read from the
// key column usage and referential constraints tables. See spec 08.
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
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_TYPE IN ('BASE TABLE','VIEW')
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
	// Read column list and primary key membership in one pass via LEFT JOIN.
	rows, err := b.db.QueryContext(ctx, `
		SELECT
			c.COLUMN_NAME,
			c.DATA_TYPE,
			c.COLUMN_TYPE,
			c.IS_NULLABLE,
			c.EXTRA,
			CASE WHEN k.COLUMN_NAME IS NOT NULL THEN 1 ELSE 0 END AS is_pk,
			k.ORDINAL_POSITION AS pk_ord
		FROM INFORMATION_SCHEMA.COLUMNS c
		LEFT JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE k
			ON  k.TABLE_SCHEMA     = c.TABLE_SCHEMA
			AND k.TABLE_NAME       = c.TABLE_NAME
			AND k.COLUMN_NAME      = c.COLUMN_NAME
			AND k.CONSTRAINT_NAME  = 'PRIMARY'
		WHERE c.TABLE_SCHEMA = DATABASE() AND c.TABLE_NAME = ?
		ORDER BY c.ORDINAL_POSITION`, table)
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
		var name, dataType, columnType, isNullable, extra string
		var isPK int
		var pkOrd sql.NullInt64
		if err := rows.Scan(&name, &dataType, &columnType, &isNullable, &extra, &isPK, &pkOrd); err != nil {
			return nil, nil, err
		}
		hasDefault := isPK == 1 || strings.Contains(strings.ToLower(extra), "auto_increment")
		col := &schema.Column{
			Name:       name,
			Type:       mysqlCanonicalType(dataType, columnType),
			Nullable:   isNullable == "YES",
			HasDefault: hasDefault,
		}
		colRows = append(colRows, colRow{col: col, isPK: isPK == 1, pkOrd: int(pkOrd.Int64)})
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

	// Build PK slice in ordinal order.
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
			kcu.REFERENCED_TABLE_NAME,
			kcu.REFERENCED_COLUMN_NAME,
			kcu.CONSTRAINT_NAME
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu
		JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS rc
			ON  rc.CONSTRAINT_SCHEMA = kcu.TABLE_SCHEMA
			AND rc.CONSTRAINT_NAME   = kcu.CONSTRAINT_NAME
		WHERE kcu.TABLE_SCHEMA = DATABASE()
		  AND kcu.TABLE_NAME   = ?
		  AND kcu.REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY kcu.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Group columns by constraint name.
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

	// Assemble into schema.ForeignKey (one per constraint, multi-column support).
	fkMap := make(map[string]*schema.ForeignKey)
	fkOrder := make([]string, 0)
	for _, r := range fkRows {
		fk, ok := fkMap[r.name]
		if !ok {
			fk = &schema.ForeignKey{
				RefRelation: r.refTab,
			}
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
