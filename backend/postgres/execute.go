package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// Execute lowers the plan to PostgreSQL operations and returns a streamable
// result. Reads keep the transaction open so the cursor can stream rows while the
// role and GUCs are still in effect; writes and calls buffer their rows, commit
// (or roll back under Prefer: tx=rollback), and then return. All paths begin with
// applySession so the engine enforces RLS as the resolved request role.
func (b *Backend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if plan.Call != nil {
		return b.executeCall(ctx, plan, rc)
	}
	if plan.Query == nil {
		return nil, pgerr.ErrUnsupported("this operation", "postgres")
	}
	switch plan.Query.Kind {
	case ir.Read:
		return b.executeRead(ctx, plan, rc)
	case ir.Insert, ir.Upsert, ir.Update, ir.Delete:
		return b.executeWrite(ctx, plan, rc)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "postgres")
	}
}

// executeRead compiles and runs the windowed read. A separate COUNT(*) runs first
// when Content-Range requested an exact count. The transaction stays open while
// rows stream; the streamRows.Close commits it.
func (b *Backend) executeRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, b.MapError(err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	if err := applySession(ctx, tx, b, rc); err != nil {
		rollback()
		return nil, b.MapError(err)
	}

	res := &streamResult{ctx: ctx, tx: tx, controls: rc.Controls()}

	if plan.Query.Count != ir.CountNone {
		cst, apiErr := sqlgen.CompileCount(Dialect{}, plan.Query)
		if apiErr != nil {
			rollback()
			return nil, apiErr
		}
		if err := tx.QueryRow(ctx, cst.SQL, cst.Args...).Scan(&res.count); err != nil {
			rollback()
			return nil, b.MapError(err)
		}
		res.hasCount = true
	}

	st, apiErr := sqlgen.CompileRead(Dialect{}, plan.Query)
	if apiErr != nil {
		rollback()
		return nil, apiErr
	}
	rows, err := tx.Query(ctx, st.SQL, st.Args...)
	if err != nil {
		rollback()
		return nil, b.MapError(err)
	}
	res.rows = rows
	res.cols = fieldNames(rows)
	return res, nil
}

// executeWrite compiles the mutation, runs it in a transaction, and buffers any
// returned rows. The transaction commits unless the client requested tx=rollback,
// in which case the computed representation is returned but nothing is persisted.
func (b *Backend) executeWrite(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	q := plan.Query
	returning := returningCols(q, plan.Rel)

	st, apiErr := compileWrite(q, returning)
	if apiErr != nil {
		return nil, apiErr
	}

	res := &bufResult{controls: rc.Controls()}
	if len(returning) > 0 {
		rows, err := tx.Query(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, b.MapError(err)
		}
		cols := fieldNames(rows)
		buf, err := drainRows(rows)
		if err != nil {
			return nil, b.MapError(err)
		}
		res.cols, res.rows = cols, buf
		res.affected, res.hasAff = int64(len(buf)), true
	} else {
		tag, err := tx.Exec(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, b.MapError(err)
		}
		res.affected, res.hasAff = tag.RowsAffected(), true
	}

	if err := readResponseControls(ctx, tx, res.controls); err != nil {
		return nil, b.MapError(err)
	}

	if q.Write != nil && q.Write.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// executeCall lowers and runs an RPC call. A read-only function (stable or
// immutable) runs in a read-only transaction like executeRead; a volatile
// function runs in a read-write transaction that commits (or rolls back under
// Prefer: tx=rollback) so its side effects persist.
func (b *Backend) executeCall(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	st, apiErr := sqlgen.CompileCall(Dialect{}, plan.Call, plan.Func)
	if apiErr != nil {
		return nil, apiErr
	}

	if plan.ReadOnly {
		return b.executeCallRead(ctx, plan, rc, st)
	}

	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	rows, err := tx.Query(ctx, st.SQL, st.Args...)
	if err != nil {
		return nil, b.MapError(err)
	}
	cols := fieldNames(rows)
	buf, err := drainRows(rows)
	if err != nil {
		return nil, b.MapError(err)
	}

	res := &bufResult{cols: cols, rows: buf, controls: rc.Controls()}
	if err := readResponseControls(ctx, tx, res.controls); err != nil {
		return nil, b.MapError(err)
	}

	if plan.Call.Prefer.Tx != nil && *plan.Call.Prefer.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// executeCallRead handles a stable/immutable function in a read-only transaction.
// An optional count runs as a separate statement before the function call itself.
func (b *Backend) executeCallRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context, st *sqlgen.Statement) (backend.Result, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, b.MapError(err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	if err := applySession(ctx, tx, b, rc); err != nil {
		rollback()
		return nil, b.MapError(err)
	}

	res := &streamResult{ctx: ctx, tx: tx, controls: rc.Controls()}

	if plan.Call.Count != ir.CountNone {
		cst, apiErr := sqlgen.CompileCallCount(Dialect{}, plan.Call, plan.Func)
		if apiErr != nil {
			rollback()
			return nil, apiErr
		}
		if err := tx.QueryRow(ctx, cst.SQL, cst.Args...).Scan(&res.count); err != nil {
			rollback()
			return nil, b.MapError(err)
		}
		res.hasCount = true
	}

	rows, err := tx.Query(ctx, st.SQL, st.Args...)
	if err != nil {
		rollback()
		return nil, b.MapError(err)
	}
	res.rows = rows
	res.cols = fieldNames(rows)
	return res, nil
}

// compileWrite dispatches to the right compiler for the mutation kind.
func compileWrite(q *ir.Query, returning []string) (*sqlgen.Statement, *pgerr.APIError) {
	switch q.Kind {
	case ir.Insert, ir.Upsert:
		return sqlgen.CompileInsert(Dialect{}, q, returning)
	case ir.Update:
		return sqlgen.CompileUpdate(Dialect{}, q, returning)
	case ir.Delete:
		return sqlgen.CompileDelete(Dialect{}, q, returning)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "postgres")
	}
}

// returningCols decides which columns a write reads back. The representation
// returns the whole row; a minimal insert/upsert still returns the primary key
// so the handler can build the Location header; a minimal update/delete returns
// nothing and runs as a plain affected-rows exec.
func returningCols(q *ir.Query, rel *schema.Relation) []string {
	if rel == nil {
		return nil
	}
	if q.Write != nil && q.Write.Return == ir.ReturnRepresentation {
		return rel.ColumnNames()
	}
	if q.Kind == ir.Insert || q.Kind == ir.Upsert {
		return rel.PrimaryKey
	}
	return nil
}

// fieldNames extracts column names from pgx.Rows without advancing the cursor.
func fieldNames(rows pgx.Rows) []string {
	descs := rows.FieldDescriptions()
	names := make([]string, len(descs))
	for i, d := range descs {
		names[i] = d.Name
	}
	return names
}

// drainRows reads every row of a pgx cursor into memory, normalizing values so
// json/jsonb and bytea render correctly. The rows are closed by drainRows; the
// caller must not close them again.
func drainRows(rows pgx.Rows) ([][]any, error) {
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, normalizeValues(vals))
	}
	return out, rows.Err()
}
