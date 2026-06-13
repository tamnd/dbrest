package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// normalizeArgs converts ISO 8601 datetime strings (e.g. "2024-01-01T00:00:00Z")
// to time.Time so the MySQL driver can bind them correctly. MySQL rejects the ISO
// T-separator format; passing time.Time avoids the string-to-DATETIME cast entirely.
func normalizeArgs(args []any) []any {
	if len(args) == 0 {
		return args
	}
	out := make([]any, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				out[i] = t
				continue
			}
			if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
				out[i] = t
				continue
			}
		}
		out[i] = a
	}
	return out
}

// Execute lowers a resolved plan to MySQL operations and returns a streamable
// result. Reads stream from an open cursor; writes run in a transaction and
// buffer their rows (since MySQL 8 has no RETURNING, rows are re-selected after
// the write). MariaDB 10.5+ uses native RETURNING.
func (b *Backend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if plan.Call != nil {
		return b.executeCall(ctx, plan, rc)
	}
	if plan.Query == nil {
		return nil, pgerr.ErrUnsupported("this operation", "mysql")
	}
	switch plan.Query.Kind {
	case ir.Read:
		return b.executeRead(ctx, plan, rc)
	case ir.Insert, ir.Upsert, ir.Update, ir.Delete:
		return b.executeWrite(ctx, plan, rc)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "mysql")
	}
}

// executeRead compiles and runs a SELECT, returning a streaming cursor.
// On MySQL there is no per-request role switch or GUC push, so the session
// setup is a no-op; the main query runs on the pool connection directly.
// A separate COUNT(*) runs when an exact count was requested.
func (b *Backend) executeRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	res := &result{controls: rc.Controls()}

	if plan.Query.Count != ir.CountNone {
		cst, apiErr := sqlgen.CompileCount(Dialect{}, plan.Query)
		if apiErr != nil {
			return nil, apiErr
		}
		if err := b.db.QueryRowContext(ctx, cst.SQL, normalizeArgs(cst.Args)...).Scan(&res.count); err != nil {
			return nil, b.MapError(err)
		}
		res.hasCount = true
	}

	st, apiErr := sqlgen.CompileRead(Dialect{}, plan.Query)
	if apiErr != nil {
		return nil, apiErr
	}
	rows, err := b.db.QueryContext(ctx, st.SQL, normalizeArgs(st.Args)...)
	if err != nil {
		return nil, b.MapError(err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, b.MapError(err)
	}
	boolCols := buildBoolCols(plan.Rel)
	jsonIdx, boolIdx, timeIdx := buildColMaps(rows, boolCols)
	res.rows, res.cols, res.jsonIdx, res.boolIdx, res.timeIdx = rows, cols, jsonIdx, boolIdx, timeIdx
	return res, nil
}

// executeWrite compiles the mutation, runs it in a transaction, and returns the
// affected rows. When the client requested return=representation, rows are
// re-selected after the write (MySQL 8 emulated RETURNING; MariaDB 10.5+ native).
func (b *Backend) executeWrite(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	q := plan.Query
	returning := returningCols(q, plan.Rel)

	// An empty column set (POST with an empty array, PATCH with an empty object)
	// is a no-op: nothing is compiled or run, the affected count is zero, and the
	// representation is the empty array. The HTTP layer turns that into 201/[] for
	// an insert and 204 or 200/[] for an update.
	if backend.IsNoOpMutation(q) {
		return &writeResult{
			controls: rc.Controls(),
			cols:     returning,
			affected: 0,
			hasAff:   true,
		}, nil
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	res := &writeResult{controls: rc.Controls()}

	if b.caps.Returning == backend.Native {
		// MariaDB 10.5+: native RETURNING.
		err = b.executeWriteNativeReturning(ctx, tx, q, returning, plan.Rel, res)
	} else {
		// MySQL 8: emulated RETURNING via re-select.
		err = b.executeWriteEmulated(ctx, tx, q, returning, plan.Rel, res)
	}
	if err != nil {
		return nil, b.MapError(err)
	}

	// Prefer: max-affected rolls an over-broad write back instead of committing.
	if apiErr := backend.EnforceMaxAffected(q.Write, res.affected, res.hasAff); apiErr != nil {
		return nil, apiErr
	}

	if q.Write != nil && q.Write.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// executeWriteNativeReturning uses MariaDB's native RETURNING.
func (b *Backend) executeWriteNativeReturning(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	st, apiErr := compileWrite(q, returning)
	if apiErr != nil {
		return apiErr
	}
	if len(returning) > 0 {
		rows, err := tx.QueryContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		boolCols := buildBoolCols(rel)
		jsonIdx, boolIdx, _ := buildColMaps(rows, boolCols)
		buf, err := drain(rows, cols, jsonIdx, boolIdx)
		rows.Close()
		if err != nil {
			return err
		}
		res.cols, res.rows = cols, buf
		res.affected, res.hasAff = int64(len(buf)), true
		return nil
	}
	out, err := tx.ExecContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true
	return nil
}

// executeWriteEmulated handles MySQL 8 (no RETURNING) via re-select.
func (b *Backend) executeWriteEmulated(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	switch q.Kind {
	case ir.Insert, ir.Upsert:
		return b.executeInsertEmulated(ctx, tx, q, returning, rel, res)
	case ir.Update:
		return b.executeUpdateEmulated(ctx, tx, q, returning, rel, res)
	case ir.Delete:
		return b.executeDeleteEmulated(ctx, tx, q, returning, rel, res)
	default:
		return pgerr.ErrUnsupported("this operation", "mysql")
	}
}

// executeInsertEmulated runs INSERT then re-selects by LAST_INSERT_ID().
func (b *Backend) executeInsertEmulated(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	st, apiErr := compileWrite(q, nil) // compile without RETURNING
	if apiErr != nil {
		return apiErr
	}
	out, err := tx.ExecContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true

	if len(returning) == 0 || n == 0 {
		return nil
	}
	// Re-select by auto-increment range. Requires the table to have a single
	// auto-increment integer PK (covers the compat test schema and common cases).
	if len(rel.PrimaryKey) != 1 {
		// No single PK — skip representation (safe: the write happened).
		return nil
	}
	lastID, err := lastInsertID(ctx, tx)
	if err != nil {
		return err
	}
	pk := (Dialect{}).QuoteIdent(rel.PrimaryKey[0])
	table := (Dialect{}).QuoteIdent(rel.Name)
	cols := quotedCols(returning)
	selectSQL := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s >= ? AND %s <= ?",
		cols, table, pk, pk,
	)
	rows, err := tx.QueryContext(ctx, selectSQL, lastID, lastID+n-1)
	if err != nil {
		return err
	}
	colNames, err := rows.Columns()
	if err != nil {
		rows.Close()
		return err
	}
	boolCols := buildBoolCols(rel)
	jsonIdx, boolIdx, _ := buildColMaps(rows, boolCols)
	buf, err := drain(rows, colNames, jsonIdx, boolIdx)
	rows.Close()
	if err != nil {
		return err
	}
	res.cols, res.rows = colNames, buf
	return nil
}

// executeUpdateEmulated runs UPDATE then re-selects by pre-captured primary keys.
// The re-select must use PKs, not the original filter, because the UPDATE may
// change the very column being filtered (e.g. PATCH /todos?task=eq.old sets
// task=new — after the UPDATE, task=eq.old matches nothing).
func (b *Backend) executeUpdateEmulated(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	st, apiErr := compileWrite(q, nil)
	if apiErr != nil {
		return apiErr
	}

	// Pre-capture PKs when we need to return representation.
	var pkValues []any
	if len(returning) > 0 && len(rel.PrimaryKey) == 1 {
		pkValues, apiErr = b.selectPKs(ctx, tx, q, rel.PrimaryKey[0])
		if apiErr != nil {
			return apiErr
		}
	}

	out, err := tx.ExecContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true

	if len(returning) == 0 || len(pkValues) == 0 {
		return nil
	}

	// Re-select by PK (post-update values).
	colNames, buf, err := b.selectByPKs(ctx, tx, rel, rel.PrimaryKey[0], pkValues, returning)
	if err != nil {
		return err
	}
	res.cols, res.rows = colNames, buf
	return nil
}

// selectPKs runs "SELECT pk FROM table WHERE <original filter>" and returns
// the raw PK values. Used to anchor the post-write re-select.
func (b *Backend) selectPKs(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, pkCol string,
) ([]any, *pgerr.APIError) {
	pkQ := *q
	pkQ.Kind = ir.Read
	pkQ.Select = []ir.SelectItem{ir.Column{Path: []string{pkCol}}}
	pkQ.Embeds = nil
	pkQ.Order = nil
	pkQ.Singular = false
	st, apiErr := sqlgen.CompileRead(Dialect{}, &pkQ)
	if apiErr != nil {
		return nil, apiErr
	}
	rows, err := tx.QueryContext(ctx, st.SQL, normalizeArgs(st.Args)...)
	if err != nil {
		return nil, pgerr.New(500, "XX000", err.Error())
	}
	defer rows.Close()
	var vals []any
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return nil, pgerr.New(500, "XX000", err.Error())
		}
		vals = append(vals, v)
	}
	if err := rows.Err(); err != nil {
		return nil, pgerr.New(500, "XX000", err.Error())
	}
	return vals, nil
}

// selectByPKs runs "SELECT cols FROM table WHERE pk IN (?,...)" using pre-captured
// PK values and returns the column names and buffered rows.
func (b *Backend) selectByPKs(
	ctx context.Context, tx *sql.Tx,
	rel *schema.Relation, pkCol string, pkValues []any, cols []string,
) ([]string, [][]any, error) {
	d := Dialect{}
	table := d.QuoteIdent(rel.Name)
	pk := d.QuoteIdent(pkCol)
	selCols := quotedCols(cols)
	placeholders := make([]string, len(pkValues))
	for i := range pkValues {
		placeholders[i] = "?"
	}
	sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)",
		selCols, table, pk, strings.Join(placeholders, ","))
	rows, err := tx.QueryContext(ctx, sql, pkValues...)
	if err != nil {
		return nil, nil, err
	}
	colNames, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, nil, err
	}
	boolCols := buildBoolCols(rel)
	jsonIdx, boolIdx, _ := buildColMaps(rows, boolCols)
	buf, err := drain(rows, colNames, jsonIdx, boolIdx)
	rows.Close()
	return colNames, buf, err
}

// executeDeleteEmulated selects the rows to return, then deletes them.
func (b *Backend) executeDeleteEmulated(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	// If the client wants return=representation, select first.
	if len(returning) > 0 {
		readQ := *q
		readQ.Kind = ir.Read
		readST, apiErr := sqlgen.CompileRead(Dialect{}, &readQ)
		if apiErr != nil {
			return apiErr
		}
		rows, err := tx.QueryContext(ctx, readST.SQL, normalizeArgs(readST.Args)...)
		if err != nil {
			return err
		}
		colNames, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		boolCols := buildBoolCols(rel)
		jsonIdx, boolIdx, _ := buildColMaps(rows, boolCols)
		buf, err := drain(rows, colNames, jsonIdx, boolIdx)
		rows.Close()
		if err != nil {
			return err
		}
		res.cols, res.rows = colNames, buf
	}

	st, apiErr := compileWrite(q, nil)
	if apiErr != nil {
		return apiErr
	}
	out, err := tx.ExecContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true
	return nil
}

// executeCall runs a stored procedure or portable RPC function.
func (b *Backend) executeCall(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	st, apiErr := sqlgen.CompileCall(Dialect{}, plan.Call, plan.Func, sqlgen.ContextArgs(rc))
	if apiErr != nil {
		return nil, apiErr
	}
	st.Args = normalizeArgs(st.Args)

	if plan.ReadOnly {
		res := &result{controls: rc.Controls()}
		if plan.Call.Count != ir.CountNone {
			cst, apiErr := sqlgen.CompileCallCount(Dialect{}, plan.Call, plan.Func, sqlgen.ContextArgs(rc))
			if apiErr != nil {
				return nil, apiErr
			}
			if err := b.db.QueryRowContext(ctx, cst.SQL, cst.Args...).Scan(&res.count); err != nil {
				return nil, b.MapError(err)
			}
			res.hasCount = true
		}
		rows, err := b.db.QueryContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, b.MapError(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, b.MapError(err)
		}
		jsonIdx, boolIdx, timeIdx := buildColMaps(rows, nil)
		res.rows, res.cols, res.jsonIdx, res.boolIdx, res.timeIdx = rows, cols, jsonIdx, boolIdx, timeIdx
		return res, nil
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return nil, b.MapError(err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, b.MapError(err)
	}
	jsonIdx, boolIdx, _ := buildColMaps(rows, nil)
	buf, err := drain(rows, cols, jsonIdx, boolIdx)
	rows.Close()
	if err != nil {
		return nil, b.MapError(err)
	}

	res := &writeResult{cols: cols, rows: buf, controls: rc.Controls()}
	if plan.Call.Prefer.Tx != nil && *plan.Call.Prefer.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// compileWrite dispatches to the right compiler for the mutation kind.
// When returning is empty the compiler omits the RETURNING / OUTPUT clause.
// Args are normalized for MySQL (ISO 8601 → time.Time) before returning.
func compileWrite(q *ir.Query, returning []string) (*sqlgen.Statement, *pgerr.APIError) {
	var (
		st     *sqlgen.Statement
		apiErr *pgerr.APIError
	)
	switch q.Kind {
	case ir.Insert, ir.Upsert:
		st, apiErr = sqlgen.CompileInsert(Dialect{}, q, returning)
	case ir.Update:
		st, apiErr = sqlgen.CompileUpdate(Dialect{}, q, returning)
	case ir.Delete:
		st, apiErr = sqlgen.CompileDelete(Dialect{}, q, returning)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "mysql")
	}
	if st != nil {
		st.Args = normalizeArgs(st.Args)
	}
	return st, apiErr
}

// returningCols decides which columns to read back after a write.
// For representation requests it is all columns; for minimal inserts it is the
// primary key only (for the Location header); for minimal updates/deletes it is nil.
func returningCols(q *ir.Query, rel *schema.Relation) []string {
	if q.Write != nil && q.Write.Return == ir.ReturnRepresentation {
		if cols := q.ProjectedColumns(); cols != nil {
			return cols
		}
		return rel.ColumnNames()
	}
	if q.Kind == ir.Insert || q.Kind == ir.Upsert {
		return rel.PrimaryKey
	}
	return nil
}

// lastInsertID queries LAST_INSERT_ID() inside a transaction.
func lastInsertID(ctx context.Context, tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT LAST_INSERT_ID()").Scan(&id)
	return id, err
}

// quotedCols builds a comma-separated list of quoted identifiers for a SELECT.
func quotedCols(cols []string) string {
	d := Dialect{}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = d.QuoteIdent(c)
	}
	return strings.Join(parts, ", ")
}
