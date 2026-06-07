package sqlite

import (
	"context"
	"sort"
	"strings"

	"github.com/tamnd/dbrest/schema"
)

// Introspect builds the unified schema model from SQLite's catalogs. Tables and
// views come from sqlite_master; columns and primary keys from PRAGMA
// table_info. SQLite's single main database maps to the unqualified namespace
// (empty schema); attached databases become named schemas when that subsystem
// lands. See spec 08.
func (b *Backend) Introspect(ctx context.Context) (*schema.Model, error) {
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
	rows, err := b.db.QueryContext(ctx,
		`SELECT name, type FROM sqlite_master
		 WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []relInfo
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		kind := schema.KindTable
		if typ == "view" {
			kind = schema.KindView
		}
		out = append(out, relInfo{name: name, kind: kind})
	}
	return out, rows.Err()
}

// columns reads PRAGMA table_info for one relation, returning its columns in
// ordinal order and the primary-key column names in key order.
func (b *Backend) columns(ctx context.Context, table string) ([]*schema.Column, []string, error) {
	// PRAGMA does not accept a bind parameter for the table name; the name comes
	// from sqlite_master (trusted catalog), not from user input, so it is safe to
	// quote and inline.
	rows, err := b.db.QueryContext(ctx, `PRAGMA table_info(`+dialect{}.QuoteIdent(table)+`)`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type pkEntry struct {
		name string
		ord  int
	}
	var cols []*schema.Column
	var pks []pkEntry
	for rows.Next() {
		var (
			cid       int
			name      string
			declType  string
			notNull   int
			dfltValue any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dfltValue, &pk); err != nil {
			return nil, nil, err
		}
		cols = append(cols, &schema.Column{
			Name:       name,
			Type:       canonicalType(declType),
			Nullable:   notNull == 0,
			HasDefault: dfltValue != nil,
			Position:   cid + 1,
		})
		if pk > 0 {
			pks = append(pks, pkEntry{name: name, ord: pk})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	sort.Slice(pks, func(i, j int) bool { return pks[i].ord < pks[j].ord })
	pkNames := make([]string, len(pks))
	for i, p := range pks {
		pkNames[i] = p.name
	}
	return cols, pkNames, nil
}

// foreignKeys reads PRAGMA foreign_key_list for one relation and groups it into
// composite foreign keys. SQLite reports one row per referencing column, keyed by
// an id that ties a composite key together and a seq that orders its columns; a
// NULL "to" means the key references the parent's primary key, which is resolved
// here. SQLite keys carry no name, so a stable one is synthesized as
// {child}_{from-columns}_fkey, matching the introspection contract (spec 08).
func (b *Backend) foreignKeys(ctx context.Context, table string) ([]*schema.ForeignKey, error) {
	rows, err := b.db.QueryContext(ctx, `PRAGMA foreign_key_list(`+dialect{}.QuoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type fkRow struct {
		seq      int
		from, to string
		toNull   bool
	}
	groups := make(map[int]*struct {
		refTable string
		cols     []fkRow
	})
	var order []int
	for rows.Next() {
		var (
			id, seq           int
			refTable, from    string
			to                any
			onUpd, onDel, mat string
		)
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpd, &onDel, &mat); err != nil {
			return nil, err
		}
		g, ok := groups[id]
		if !ok {
			g = &struct {
				refTable string
				cols     []fkRow
			}{refTable: refTable}
			groups[id] = g
			order = append(order, id)
		}
		fr := fkRow{seq: seq}
		fr.from = from
		if s, ok := toString(to); ok {
			fr.to = s
		} else {
			fr.toNull = true
		}
		g.cols = append(g.cols, fr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Ints(order)
	var out []*schema.ForeignKey
	for _, id := range order {
		g := groups[id]
		sort.Slice(g.cols, func(i, j int) bool { return g.cols[i].seq < g.cols[j].seq })

		fromCols := make([]string, len(g.cols))
		toCols := make([]string, 0, len(g.cols))
		needPK := false
		for i, c := range g.cols {
			fromCols[i] = c.from
			if c.toNull {
				needPK = true
			} else {
				toCols = append(toCols, c.to)
			}
		}
		// A key with NULL targets references the parent's primary key.
		if needPK {
			_, refPK, err := b.columns(ctx, g.refTable)
			if err != nil {
				return nil, err
			}
			toCols = refPK
		}
		out = append(out, &schema.ForeignKey{
			Name:        table + "_" + strings.Join(fromCols, "_") + "_fkey",
			Columns:     fromCols,
			RefRelation: g.refTable,
			RefColumns:  toCols,
		})
	}
	return out, nil
}

// toString coerces a scalar from PRAGMA into a string, reporting false for NULL.
func toString(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return "", false
	}
}
