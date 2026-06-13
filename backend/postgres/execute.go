package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

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
	txOpts := b.txOptions(rc, pgx.ReadOnly)
	// A counted read runs the count and the page as two statements. At READ
	// COMMITTED each takes its own snapshot, so a concurrent write between them can
	// make the Content-Range total disagree with the rows returned. PostgREST
	// reads both from one statement, hence one snapshot; pinning the transaction to
	// REPEATABLE READ gives the two statements that same single snapshot. A
	// read-only REPEATABLE READ transaction never raises a serialization error, so
	// this only fixes consistency without adding a failure mode. A role that pins a
	// stronger level (its default_transaction_isolation) keeps it.
	if plan.Query.Count != ir.CountNone && !isoAtLeastRepeatableRead(txOpts.IsoLevel) {
		txOpts.IsoLevel = pgx.RepeatableRead
	}
	tx, err := b.pool.BeginTx(ctx, txOpts)
	if err != nil {
		return nil, b.MapError(err)
	}
	rollback := func() { _ = tx.Rollback(ctx) }

	if err := applySession(ctx, tx, b, rc); err != nil {
		rollback()
		return nil, b.MapError(err)
	}

	res := &streamResult{ctx: ctx, tx: tx, controls: rc.Controls(), loc: b.loc}

	// db-pre-request runs inside applySession and may have set response.status or
	// response.headers (PostgREST lets the pre-request hook steer any response,
	// including a plain GET). Those headers must be read before the body streams, so
	// read them now: the GUCs are already set, and a table SELECT does not set them
	// itself, so reading here captures the same value PostgREST would.
	if err := readResponseControls(ctx, tx, res.controls); err != nil {
		rollback()
		return nil, b.MapError(err)
	}

	if plan.Query.Count != ir.CountNone {
		total, err := b.computeCount(ctx, tx, plan.Query)
		if err != nil {
			rollback()
			return nil, err
		}
		res.count = total
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
	tx, err := b.pool.BeginTx(ctx, b.txOptions(rc, pgx.ReadWrite))
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	q := plan.Query
	returning := returningCols(q, plan.Rel)

	// An empty column set (POST with an empty array, PATCH with an empty object)
	// is a no-op: nothing is compiled or run, the affected count is zero, and the
	// representation is the empty array. The HTTP layer turns that into 201/[] for
	// an insert and 204 or 200/[] for an update.
	if backend.IsNoOpMutation(q) {
		return &bufResult{
			controls: rc.Controls(),
			cols:     returning,
			affected: 0,
			hasAff:   true,
		}, nil
	}

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
		buf, err := drainRows(rows, b.loc)
		if err != nil {
			return nil, b.MapError(err)
		}
		// Strip the xmax column from the result and use it to decide insert/update status.
		if isUpsert && xmaxIdx >= 0 && xmaxIdx < len(cols) {
			inserted := 0
			cleaned := make([][]any, len(buf))
			for i, row := range buf {
				// A zero (or empty) xmax means the row had no prior version: it was
				// newly inserted. A non-zero xmax means ON CONFLICT DO UPDATE replaced
				// an existing row.
				rowInserted := true
				if xmaxIdx < len(row) {
					switch xv := row[xmaxIdx].(type) {
					case []byte:
						if string(xv) != "0" && string(xv) != "" {
							rowInserted = false
						}
					case string:
						if xv != "0" && xv != "" {
							rowInserted = false
						}
					case int64:
						if xv != 0 {
							rowInserted = false
						}
					case uint32:
						if xv != 0 {
							rowInserted = false
						}
					}
				}
				if rowInserted {
					inserted++
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
			res.controls.InsertedRows = inserted
		}
		// The affected count is the full mutated set, taken before the
		// representation is shaped: order/limit/offset bound only the returned
		// body, not the mutation (v13 dropped limited update/delete).
		res.affected, res.hasAff = int64(len(buf)), true
		res.cols, res.rows = cols, backend.ShapeWriteRepresentation(cols, buf, q)
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

	// Prefer: max-affected rolls an over-broad write back instead of committing.
	if apiErr := backend.EnforceMaxAffected(q.Write, res.affected, res.hasAff); apiErr != nil {
		return nil, apiErr
	}

	// A singular write (vnd.pgrst.object+json) that touched zero or many rows
	// fails closed before commit, so the deferred rollback discards it rather
	// than the renderer rejecting an already-durable mutation.
	if apiErr := backend.EnforceSingularWrite(q.Singular, res.affected, res.hasAff); apiErr != nil {
		return nil, apiErr
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
		st, apiErr = sqlgen.CompileCall(Dialect{}, plan.Call, plan.Func, sqlgen.ContextArgs(rc))
	} else {
		st, apiErr = b.compileNativeCall(plan.Call, b.callSchema(rc))
		if apiErr == nil {
			// A table-valued function result supports the same select, filters,
			// ordering, and window a table read does; the registry path wraps for
			// these inside CompileCall, so the native path wraps here too.
			st, apiErr = sqlgen.CompileNativeCallWrap(Dialect{}, plan.Call, st)
		}
	}
	if apiErr != nil {
		return nil, apiErr
	}

	// On the native path the access mode follows volatility, not only the method:
	// PostgREST runs a STABLE or IMMUTABLE function read-only even on POST, and
	// only a VOLATILE function read-write. The registry path already set plan.ReadOnly
	// from volatility (plan.Func != nil), so only the native path needs the check.
	readOnly := plan.ReadOnly
	if plan.Func == nil {
		readOnly = b.nativeCallReadOnly(plan, rc)
	}
	if readOnly {
		return b.executeCallRead(ctx, plan, rc, st)
	}

	tx, err := b.pool.BeginTx(ctx, b.txOptions(rc, pgx.ReadWrite))
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
	buf, err := drainRows(rows, b.loc)
	if err != nil {
		return nil, b.MapError(err)
	}

	res := &bufResult{cols: cols, rows: buf, controls: rc.Controls()}
	if err := readResponseControls(ctx, tx, res.controls); err != nil {
		return nil, b.MapError(err)
	}
	// A portable registry function may steer the response with reserved columns
	// instead of the GUCs (the engine-agnostic mechanism); lift them out here too
	// so a portable function behaves the same on postgres as on an emulated
	// backend. A native function sets the GUCs and carries no such columns, so
	// this is a no-op for it. An invalid status or header set is PGRST112/111
	// before commit, so the deferred rollback discards the mutation.
	var ctrlErr *pgerr.APIError
	res.cols, res.rows, ctrlErr = backend.LiftResponseControls(res.cols, res.rows, res.controls)
	if ctrlErr != nil {
		return nil, ctrlErr
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
//
// Unlike a table read, the rows are buffered rather than streamed: a function
// invoked through GET can still set response.status or response.headers (the
// documented Cache-Control and 418 patterns), and those GUCs must be read back
// before the response is sent. Buffering lets readResponseControls run after the
// rows are drained and before the transaction commits, the same shape the write
// and volatile-call paths use; RPC results are small, so this costs little.
func (b *Backend) executeCallRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context, st *sqlgen.Statement) (backend.Result, error) {
	tx, err := b.pool.BeginTx(ctx, b.txOptions(rc, pgx.ReadOnly))
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	res := &bufResult{controls: rc.Controls()}

	if plan.Call.Count != ir.CountNone {
		var (
			cst    *sqlgen.Statement
			apiErr *pgerr.APIError
		)
		if plan.Func != nil {
			cst, apiErr = sqlgen.CompileCallCount(Dialect{}, plan.Call, plan.Func, sqlgen.ContextArgs(rc))
		} else {
			cst, apiErr = b.compileNativeCallCount(plan.Call, b.callSchema(rc))
		}
		if apiErr != nil {
			return nil, apiErr
		}
		if err := tx.QueryRow(ctx, cst.SQL, cst.Args...).Scan(&res.count); err != nil {
			return nil, b.MapError(err)
		}
		res.hasCount = true
	}

	rows, err := tx.Query(ctx, st.SQL, st.Args...)
	if err != nil {
		return nil, b.MapError(err)
	}
	isVoid := isVoidResult(rows)
	cols := fieldNames(rows)
	buf, err := drainRows(rows, b.loc)
	if err != nil {
		return nil, b.MapError(err)
	}
	res.cols, res.rows = cols, buf

	// Read response.status / response.headers a stable function may have set, then
	// lift any portable-registry reserved control columns, matching the volatile
	// and write paths so a GET to a function steers its response the same way.
	if err := readResponseControls(ctx, tx, res.controls); err != nil {
		return nil, b.MapError(err)
	}
	var ctrlErr *pgerr.APIError
	res.cols, res.rows, ctrlErr = backend.LiftResponseControls(res.cols, res.rows, res.controls)
	if ctrlErr != nil {
		return nil, ctrlErr
	}
	if isVoid && res.controls.Status == 0 {
		res.controls.Status = 204
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// callSchema is the schema a native RPC resolves in: the request's negotiated
// profile (Accept-Profile on GET/HEAD, Content-Profile on POST) when set, else
// the first configured search-path schema, else public. The HTTP layer already
// rejected a profile outside the exposed list with PGRST106, so rc.Schema is a
// vetted member of the configured set by the time it reaches here. This lets a
// multi-schema deployment dispatch /rpc to the function in the active schema
// instead of always calling the first one.
func (b *Backend) callSchema(rc *reqctx.Context) string {
	if rc != nil && rc.Schema != "" {
		return rc.Schema
	}
	if len(b.searchPath) > 0 {
		return b.searchPath[0]
	}
	return "public"
}

// compileNativeCall generates the PostgreSQL function-call SQL for the native
// RPC path (NativeRPC=true), where there is no declared function registry. It
// renders SELECT * FROM schema.fn(arg := <literal>, ...) with values embedded
// as SQL literals so PostgreSQL infers the parameter types from the function
// signature and the call does not depend on pgx OID mapping. String values are
// single-quote escaped; numeric JSON values are written as numeric literals;
// booleans become TRUE/FALSE; null or absent values become NULL.
func (b *Backend) compileNativeCall(c *ir.Call, schema string) (*sqlgen.Statement, *pgerr.APIError) {
	d := Dialect{}
	var sb strings.Builder
	sb.WriteString("SELECT * FROM ")
	sb.WriteString(d.QuoteIdent(schema))
	sb.WriteString(".")
	sb.WriteString(d.QuoteIdent(c.Function.Name))
	sb.WriteString("(")

	i := 0
	for name, val := range c.Args {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(d.QuoteIdent(name))
		sb.WriteString(" := ")
		appendNativeArg(&sb, val)
		i++
	}
	sb.WriteString(")")
	return &sqlgen.Statement{SQL: sb.String()}, nil
}

// compileNativeCallCount wraps the native function call in a count, the exact-count
// statement for a native RPC. There is no registry function to drive
// sqlgen.CompileCallCount (plan.Func is nil), so the count is built here over the
// same SELECT * FROM schema.fn(...) the row query runs; a scalar-returning function
// yields its single row and counts as one, a setof yields its rows.
func (b *Backend) compileNativeCallCount(c *ir.Call, schema string) (*sqlgen.Statement, *pgerr.APIError) {
	inner, apiErr := b.compileNativeCall(c, schema)
	if apiErr != nil {
		return nil, apiErr
	}
	// Count over the same post-filter the row query applies, so a count=exact
	// total matches the rows returned (the select, order, and window do not
	// change the count).
	return sqlgen.CompileNativeCallCountWrap(Dialect{}, c, inner)
}

// appendNativeArg writes one function argument as a safe SQL literal. Numbers
// are written unquoted so PostgreSQL resolves their type from context; strings
// use single-quote escaping; booleans are TRUE/FALSE; anything else (including
// absent values) becomes NULL. Objects and arrays are JSON-quoted.
func appendNativeArg(sb *strings.Builder, val ir.Value) {
	if val.JSON != nil {
		switch v := val.JSON.(type) {
		case string:
			sb.WriteString("'")
			sb.WriteString(strings.ReplaceAll(v, "'", "''"))
			sb.WriteString("'")
		case json.Number:
			// json.Number from dec.UseNumber() — write as-is; it is a valid SQL numeric literal.
			sb.WriteString(v.String())
		case float64:
			sb.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			if v {
				sb.WriteString("TRUE")
			} else {
				sb.WriteString("FALSE")
			}
		default:
			// JSON object / array: splice the encoded text as an UNTYPED literal.
			// PostgreSQL's function resolution applies implicit casts only, and the
			// json->jsonb cast is assignment-context, so a '...'::json literal fails
			// to match an fn(jsonb) signature (42883 -> 404). An unknown-type literal
			// instead coerces to json, jsonb, or text alike, which is also why the
			// string/number/bool branches already work against any parameter type.
			enc, _ := json.Marshal(v)
			sb.WriteString("'")
			sb.WriteString(strings.ReplaceAll(string(enc), "'", "''"))
			sb.WriteString("'")
		}
		return
	}
	if val.Text != "" {
		sb.WriteString("'")
		sb.WriteString(strings.ReplaceAll(val.Text, "'", "''"))
		sb.WriteString("'")
		return
	}
	sb.WriteString("NULL")
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

// explainPrefix builds the "EXPLAIN (...) " clause for a plan request from the
// parsed options: the output format plus whichever of analyze/verbose/settings/
// buffers/wal were asked for, in PostgreSQL's option syntax.
func explainPrefix(opts backend.PlanOptions) string {
	args := []string{"FORMAT TEXT"}
	if opts.Format == backend.PlanJSON {
		args[0] = "FORMAT JSON"
	}
	for _, o := range []struct {
		on   bool
		name string
	}{
		{opts.Analyze, "ANALYZE"}, {opts.Verbose, "VERBOSE"},
		{opts.Settings, "SETTINGS"}, {opts.Buffers, "BUFFERS"}, {opts.Wal, "WAL"},
	} {
		if o.on {
			args = append(args, o.name)
		}
	}
	return "EXPLAIN (" + strings.Join(args, ", ") + ") "
}

// runExplain executes the prefixed statement and returns the plan bytes. The
// JSON format yields a single document; the text format yields one row per plan
// line, which are joined with newlines into the text body PostgREST returns.
func (b *Backend) runExplain(ctx context.Context, tx pgx.Tx, opts backend.PlanOptions, sql string, args []any) ([]byte, error) {
	rows, err := tx.Query(ctx, explainPrefix(opts)+sql, args...)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer rows.Close()
	if opts.Format == backend.PlanJSON {
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
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, b.MapError(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, b.MapError(err)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// ExplainRead runs EXPLAIN on the read query and returns the plan in the
// requested format. The request runs in a read-only transaction with the full
// session setup (role + GUCs) so the planner sees the same context as a real
// request.
func (b *Backend) ExplainRead(ctx context.Context, p *ir.Plan, rc *reqctx.Context, opts backend.PlanOptions) ([]byte, error) {
	tx, err := b.pool.BeginTx(ctx, b.txOptions(rc, pgx.ReadOnly))
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
	return b.runExplain(ctx, tx, opts, st.SQL, st.Args)
}

// ExplainWrite runs EXPLAIN on the mutation. It uses a read-write transaction
// that always rolls back, so EXPLAIN ANALYZE (which executes the statement)
// leaves nothing behind, matching PostgREST's plan-only contract.
func (b *Backend) ExplainWrite(ctx context.Context, p *ir.Plan, rc *reqctx.Context, opts backend.PlanOptions) ([]byte, error) {
	tx, err := b.pool.BeginTx(ctx, b.txOptions(rc, ""))
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	st, apiErr := compileWrite(p.Query, returningCols(p.Query, p.Rel))
	if apiErr != nil {
		return nil, apiErr
	}
	return b.runExplain(ctx, tx, opts, st.SQL, st.Args)
}

// ExplainCall runs EXPLAIN on the RPC function call. The read-write transaction
// rolls back, so an EXPLAIN ANALYZE of a volatile function discards its effects.
func (b *Backend) ExplainCall(ctx context.Context, p *ir.Plan, rc *reqctx.Context, opts backend.PlanOptions) ([]byte, error) {
	tx, err := b.pool.BeginTx(ctx, b.txOptions(rc, ""))
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applySession(ctx, tx, b, rc); err != nil {
		return nil, b.MapError(err)
	}

	st, apiErr := sqlgen.CompileCall(Dialect{}, p.Call, p.Func, sqlgen.ContextArgs(rc))
	if apiErr != nil {
		return nil, apiErr
	}
	return b.runExplain(ctx, tx, opts, st.SQL, st.Args)
}

// drainRows reads every row of a pgx cursor into memory, normalizing values so
// json/jsonb, bytea, and date columns render correctly. The rows are closed by
// drainRows; the caller must not close them again.
func drainRows(rows pgx.Rows, loc *time.Location) ([][]any, error) {
	defer rows.Close()
	fields := rows.FieldDescriptions()
	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, normalizeValues(vals, fields, loc))
	}
	return out, rows.Err()
}
