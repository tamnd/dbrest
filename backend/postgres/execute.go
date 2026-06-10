package postgres

import (
	"context"
	"strings"

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

// executeRead compiles and runs the windowed read in a read-only transaction.
// Session setup (SET LOCAL ROLE, search_path, GUCs) is applied via applySession
// before the main query is sent so the PostgreSQL planner sees the correct role
// at parse time. Rows stream from within the open transaction; Close commits it.
//
// Note: a single-batch approach (BEGIN + session + query + ROLLBACK in one
// pipeline) would let pgx pre-parse the main SELECT while the connection is still
// authenticator (NOINHERIT, no schema USAGE), causing a 42501 error. applySession
// completes its batch before the main query is issued, so Parse runs as the
// request role, which has the required privileges.
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

	// For upserts, append xmax to the RETURNING list so we can distinguish
	// an INSERT from an ON CONFLICT UPDATE and set the 201/200 status correctly.
	isUpsert := q.Kind == ir.Upsert
	xmaxIdx := -1
	returningForSQL := returning
	if isUpsert {
		xmaxIdx = len(returning)
		tmp := make([]string, len(returning)+1)
		copy(tmp, returning)
		tmp[len(returning)] = "xmax"
		returningForSQL = tmp
	}

	st, apiErr := compileWrite(q, returningForSQL)
	if apiErr != nil {
		return nil, apiErr
	}

	res := &bufResult{controls: rc.Controls()}
	if len(returningForSQL) > 0 {
		rows, err := tx.Query(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, b.MapError(err)
		}
		cols := fieldNames(rows)
		buf, err := drainRows(rows)
		if err != nil {
			return nil, b.MapError(err)
		}
		// Strip the xmax column from the result and use it to decide insert/update status.
		if isUpsert && xmaxIdx >= 0 && xmaxIdx < len(cols) {
			allInsert := true
			cleaned := make([][]any, len(buf))
			for i, row := range buf {
				// Check if xmax indicates an update (non-zero value means the row
				// existed before and was updated via ON CONFLICT DO UPDATE).
				if xmaxIdx < len(row) {
					switch xv := row[xmaxIdx].(type) {
					case []byte:
						if string(xv) != "0" && string(xv) != "" {
							allInsert = false
						}
					case string:
						if xv != "0" && xv != "" {
							allInsert = false
						}
					case int64:
						if xv != 0 {
							allInsert = false
						}
					case uint32:
						if xv != 0 {
							allInsert = false
						}
					}
				}
				// Remove the xmax column from the row.
				r := make([]any, 0, len(row)-1)
				for j, v := range row {
					if j != xmaxIdx {
						r = append(r, v)
					}
				}
				cleaned[i] = r
			}
			buf = cleaned
			cols = append(cols[:xmaxIdx], cols[xmaxIdx+1:]...)
			res.controls.UpsertStatusKnown = true
			res.controls.UpsertInsert = allInsert
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
	var (
		st     *sqlgen.Statement
		apiErr *pgerr.APIError
	)
	if plan.Func != nil {
		st, apiErr = sqlgen.CompileCall(Dialect{}, plan.Call, plan.Func)
	} else {
		st, apiErr = b.compileNativeCall(plan.Call)
	}
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
	isVoid := isVoidResult(rows)
	cols := fieldNames(rows)
	buf, err := drainRows(rows)
	if err != nil {
		return nil, b.MapError(err)
	}

	res := &bufResult{cols: cols, rows: buf, controls: rc.Controls()}
	if err := readResponseControls(ctx, tx, res.controls); err != nil {
		return nil, b.MapError(err)
	}
	// Void-returning functions produce no meaningful body; signal 204 to the
	// HTTP layer unless the function already set a status override via GUC.
	if isVoid && res.controls.Status == 0 {
		res.controls.Status = 204
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

// compileNativeCall generates the PostgreSQL function-call SQL for the native
// RPC path (NativeRPC=true), where there is no declared function registry. It
// renders SELECT * FROM schema.fn(arg := $1, ...) using the search path's first
// schema as the function schema. Arguments come from the call's parsed arg map;
// they are bound as named parameters (fn_name := $N) which is how PostgREST
// calls PG functions. When no args are supplied the call has an empty arg list.
func (b *Backend) compileNativeCall(c *ir.Call) (*sqlgen.Statement, *pgerr.APIError) {
	schema := "public"
	if len(b.searchPath) > 0 {
		schema = b.searchPath[0]
	}

	d := Dialect{}
	var sb strings.Builder
	sb.WriteString("SELECT * FROM ")
	sb.WriteString(d.QuoteIdent(schema))
	sb.WriteString(".")
	sb.WriteString(d.QuoteIdent(c.Function.Name))
	sb.WriteString("(")

	args := make([]any, 0, len(c.Args))
	i := 0
	for name, val := range c.Args {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(d.QuoteIdent(name))
		sb.WriteString(" := ")
		sb.WriteString(d.Placeholder(i + 1))
		if val.JSON != nil {
			args = append(args, val.JSON)
		} else {
			args = append(args, val.Text)
		}
		i++
	}
	sb.WriteString(")")
	return &sqlgen.Statement{SQL: sb.String(), Args: args}, nil
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

// isVoidResult reports whether the pgx result represents a void-returning function.
// PostgreSQL void has OID 2278; a SELECT * FROM void_fn() returns exactly one column
// with that OID and value null.
func isVoidResult(rows pgx.Rows) bool {
	fields := rows.FieldDescriptions()
	return len(fields) == 1 && fields[0].DataTypeOID == 2278
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

// ExplainRead runs EXPLAIN (FORMAT JSON) on the read query and returns the raw
// JSON plan from PostgreSQL. When analyze is true EXPLAIN ANALYZE is used
// instead, which also executes the query and includes timing. The request runs
// in a read-only transaction with the full session setup (role + GUCs) so the
// planner sees the same context as a real request.
func (b *Backend) ExplainRead(ctx context.Context, p *ir.Plan, rc *reqctx.Context, analyze bool) ([]byte, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	st, apiErr := sqlgen.CompileRead(Dialect{}, p.Query)
	if apiErr != nil {
		return nil, apiErr
	}

	var prefix string
	if analyze {
		prefix = "EXPLAIN (ANALYZE, FORMAT JSON) "
	} else {
		prefix = "EXPLAIN (FORMAT JSON) "
	}
	rows, err := tx.Query(ctx, prefix+st.SQL, st.Args...)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer rows.Close()
	var plan []byte
	for rows.Next() {
		if err := rows.Scan(&plan); err != nil {
			return nil, b.MapError(err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, b.MapError(err)
	}
	return plan, nil
}

// drainRows reads every row of a pgx cursor into memory, normalizing values so
// json/jsonb, bytea, and date columns render correctly. The rows are closed by
// drainRows; the caller must not close them again.
func drainRows(rows pgx.Rows) ([][]any, error) {
	defer rows.Close()
	fields := rows.FieldDescriptions()
	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, normalizeValues(vals, fields))
	}
	return out, rows.Err()
}
