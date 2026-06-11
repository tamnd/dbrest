package sqlserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// checkReadCaps returns PGRST127 if the query needs a feature this server version lacks.
func (b *Backend) checkReadCaps(q *ir.Query) *pgerr.APIError {
	if len(q.Embeds) > 0 && !b.version.modern() {
		return pgerr.ErrUnsupported("resource embedding (requires SQL Server 2022 or Azure SQL)", "sqlserver")
	}
	if q.Where != nil && condUsesRegex(*q.Where) && !b.version.hasRegex() {
		return pgerr.ErrUnsupported("regular-expression match (requires SQL Server 2025 or Azure SQL)", "sqlserver")
	}
	return nil
}

// condUsesRegex reports whether any node in the condition tree uses match/imatch.
func condUsesRegex(c ir.Cond) bool {
	switch v := c.(type) {
	case ir.Compare:
		return v.Op == ir.OpMatch || v.Op == ir.OpIMatch
	case ir.And:
		for _, k := range v.Kids {
			if condUsesRegex(k) {
				return true
			}
		}
	case ir.Or:
		for _, k := range v.Kids {
			if condUsesRegex(k) {
				return true
			}
		}
	case ir.Not:
		return condUsesRegex(v.Kid)
	}
	return false
}

// Execute lowers a resolved plan to SQL Server operations and returns a
// streamable result. Reads stream from an open cursor; writes run in a
// transaction and buffer their rows (since OUTPUT requires mid-statement
// placement, not a trailing RETURNING clause, the compiler is called without
// RETURNING and the data plane injects OUTPUT before VALUES / WHERE).
func (b *Backend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if plan.Call != nil {
		return b.executeCall(ctx, plan, rc)
	}
	if plan.Query == nil {
		return nil, pgerr.ErrUnsupported("this operation", "sqlserver")
	}
	switch plan.Query.Kind {
	case ir.Read:
		return b.executeRead(ctx, plan, rc)
	case ir.Insert, ir.Upsert, ir.Update, ir.Delete:
		return b.executeWrite(ctx, plan, rc)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "sqlserver")
	}
}

// executeRead compiles and runs a SELECT, returning a streaming cursor.
func (b *Backend) executeRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if apiErr := b.checkReadCaps(plan.Query); apiErr != nil {
		return nil, apiErr
	}
	res := &result{controls: rc.Controls()}

	if plan.Query.Count != ir.CountNone {
		cst, apiErr := sqlgen.CompileCount(Dialect{}, plan.Query)
		if apiErr != nil {
			return nil, apiErr
		}
		if err := b.db.QueryRowContext(ctx, cst.SQL, namedArgs(cst.Args)...).Scan(&res.count); err != nil {
			return nil, b.MapError(err)
		}
		res.hasCount = true
	}

	st, apiErr := sqlgen.CompileRead(Dialect{}, plan.Query)
	if apiErr != nil {
		return nil, apiErr
	}
	rows, err := b.db.QueryContext(ctx, st.SQL, namedArgs(st.Args)...)
	if err != nil {
		return nil, b.MapError(err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, b.MapError(err)
	}
	jsonIdx, timeIdx := buildColMaps(rows, nil)
	res.rows, res.cols, res.jsonIdx, res.timeIdx = rows, cols, jsonIdx, timeIdx
	return res, nil
}

// executeWrite runs a mutation in a transaction and returns the result.
func (b *Backend) executeWrite(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	q := plan.Query
	returning := returningCols(q, plan.Rel)

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	res := &writeResult{controls: rc.Controls()}

	switch q.Kind {
	case ir.Insert, ir.Upsert:
		err = b.executeInsert(ctx, tx, q, returning, plan.Rel, res)
	case ir.Update:
		err = b.executeUpdate(ctx, tx, q, returning, plan.Rel, res)
	case ir.Delete:
		err = b.executeDelete(ctx, tx, q, returning, plan.Rel, res)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "sqlserver")
	}
	if err != nil {
		return nil, b.MapError(err)
	}

	if q.Write != nil && q.Write.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// executeInsert runs INSERT ... OUTPUT INSERTED.* VALUES (...).
// The compiler emits: INSERT INTO [t] ([c1],[c2]) VALUES (@p1,@p2)
// The data plane rewrites to: INSERT INTO [t] ([c1],[c2]) OUTPUT INSERTED.[c1],... VALUES (@p1,@p2)
// by injecting the OUTPUT fragment before the " VALUES " marker.
// Upsert (on_conflict) is routed to executeUpsert instead of the single-statement
// compiler, which returns errUpsertMultiStatement.
func (b *Backend) executeInsert(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	if q.Kind == ir.Upsert {
		return b.executeUpsert(ctx, tx, q, returning, res)
	}

	st, apiErr := sqlgen.CompileInsert(Dialect{}, q, nil)
	if apiErr != nil {
		return apiErr
	}

	if len(returning) > 0 {
		outputFrag := buildOutputFragment("INSERTED", returning)
		sqlWithOutput := injectBeforeValues(st.SQL, outputFrag)
		rows, err := tx.QueryContext(ctx, sqlWithOutput, namedArgs(st.Args)...)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		jsonIdx, timeIdx := buildColMaps(rows, nil)
		buf, err := drain(rows, cols, jsonIdx, timeIdx)
		rows.Close()
		if err != nil {
			return err
		}
		res.cols, res.rows = cols, buf
		res.affected, res.hasAff = int64(len(buf)), true
		return nil
	}

	out, err := tx.ExecContext(ctx, st.SQL, namedArgs(st.Args)...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true
	return nil
}

// executeUpsert implements the SQL Server multi-statement upsert pattern:
// for each row emit UPDATE … WHERE pk=@pN; IF @@ROWCOUNT=0 INSERT …
// inside the request transaction. Named @pN placeholders let each value be
// referenced by both the UPDATE and the INSERT within the same batch.
//
// After the batch, when returning columns are requested, the upserted rows are
// read back via SELECT … WHERE (pk1=@q1 AND pk2=@q2) OR …
func (b *Backend) executeUpsert(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string,
	res *writeResult,
) error {
	w := q.Write
	if len(w.Rows) == 0 {
		res.affected, res.hasAff = 0, true
		return nil
	}

	d := Dialect{}
	sch := q.Relation.Schema
	if sch == "" {
		sch = b.schema
		if sch == "" {
			sch = "dbo"
		}
	}
	tableName := d.QuoteIdent(sch) + "." + d.QuoteIdent(q.Relation.Name)

	conflictCols := w.Conflict.Target
	conflictSet := make(map[string]bool, len(conflictCols))
	for _, c := range conflictCols {
		conflictSet[c] = true
	}
	nonConflictCols := make([]string, 0, len(w.Columns))
	for _, c := range w.Columns {
		if !conflictSet[c] {
			nonConflictCols = append(nonConflictCols, c)
		}
	}

	var batchSQL strings.Builder
	batchRaw := []any{} // raw values; wrapped by namedArgs() as p1, p2, ...
	argN := 0
	bind := func(v any) string {
		argN++
		batchRaw = append(batchRaw, v)
		return "@p" + strconv.Itoa(argN)
	}

	for _, row := range w.Rows {
		// Bind each column value once; named placeholders can be reused.
		colP := make(map[string]string, len(w.Columns))
		for _, c := range w.Columns {
			colP[c] = bind(sqlgen.WriteArg(row[c]))
		}

		if len(nonConflictCols) > 0 {
			// UPDATE … SET non-pk cols WHERE pk cols
			batchSQL.WriteString("UPDATE ")
			batchSQL.WriteString(tableName)
			batchSQL.WriteString(" WITH (UPDLOCK,HOLDLOCK) SET ")
			for i, c := range nonConflictCols {
				if i > 0 {
					batchSQL.WriteString(",")
				}
				batchSQL.WriteString(d.QuoteIdent(c) + "=" + colP[c])
			}
			batchSQL.WriteString(" WHERE ")
			for i, c := range conflictCols {
				if i > 0 {
					batchSQL.WriteString(" AND ")
				}
				batchSQL.WriteString(d.QuoteIdent(c) + "=" + colP[c])
			}
			batchSQL.WriteString("; IF @@ROWCOUNT=0 ")
		} else {
			// No non-conflict columns: row is pk-only; insert if absent.
			batchSQL.WriteString("IF NOT EXISTS(SELECT 1 FROM ")
			batchSQL.WriteString(tableName)
			batchSQL.WriteString(" WITH (UPDLOCK,HOLDLOCK) WHERE ")
			for i, c := range conflictCols {
				if i > 0 {
					batchSQL.WriteString(" AND ")
				}
				batchSQL.WriteString(d.QuoteIdent(c) + "=" + colP[c])
			}
			batchSQL.WriteString(") ")
		}

		batchSQL.WriteString("INSERT INTO ")
		batchSQL.WriteString(tableName)
		batchSQL.WriteString("(")
		for i, c := range w.Columns {
			if i > 0 {
				batchSQL.WriteString(",")
			}
			batchSQL.WriteString(d.QuoteIdent(c))
		}
		batchSQL.WriteString(") VALUES(")
		for i, c := range w.Columns {
			if i > 0 {
				batchSQL.WriteString(",")
			}
			batchSQL.WriteString(colP[c])
		}
		batchSQL.WriteString(");")
	}

	if _, err := tx.ExecContext(ctx, batchSQL.String(), namedArgs(batchRaw)...); err != nil {
		return err
	}
	res.affected, res.hasAff = int64(len(w.Rows)), true

	if len(returning) == 0 {
		return nil
	}

	// SELECT the upserted rows back by their conflict key values.
	var selSQL strings.Builder
	selSQL.WriteString("SELECT ")
	for i, c := range returning {
		if i > 0 {
			selSQL.WriteString(",")
		}
		selSQL.WriteString(d.QuoteIdent(c))
	}
	selSQL.WriteString(" FROM ")
	selSQL.WriteString(tableName)
	selSQL.WriteString(" WHERE ")
	selRaw := []any{}
	selN := 0
	for ri, row := range w.Rows {
		if ri > 0 {
			selSQL.WriteString(" OR ")
		}
		selSQL.WriteString("(")
		for ci, c := range conflictCols {
			if ci > 0 {
				selSQL.WriteString(" AND ")
			}
			selN++
			selSQL.WriteString(d.QuoteIdent(c) + "=@p" + strconv.Itoa(selN))
			selRaw = append(selRaw, sqlgen.WriteArg(row[c]))
		}
		selSQL.WriteString(")")
	}

	rows, err := tx.QueryContext(ctx, selSQL.String(), namedArgs(selRaw)...)
	if err != nil {
		return err
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return err
	}
	jsonIdx, timeIdx := buildColMaps(rows, nil)
	buf, err := drain(rows, cols, jsonIdx, timeIdx)
	rows.Close()
	if err != nil {
		return err
	}
	res.cols, res.rows = cols, buf
	res.affected, res.hasAff = int64(len(buf)), true
	return nil
}

// executeUpdate runs UPDATE [t] SET ... OUTPUT INSERTED.* WHERE ...
// Compiler emits: UPDATE [t] SET [c]=@p1 WHERE [id]=@p2
// Rewritten to:   UPDATE [t] SET [c]=@p1 OUTPUT INSERTED.[c],... WHERE [id]=@p2
func (b *Backend) executeUpdate(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	st, apiErr := sqlgen.CompileUpdate(Dialect{}, q, nil)
	if apiErr != nil {
		return apiErr
	}

	if len(returning) > 0 {
		outputFrag := buildOutputFragment("INSERTED", returning)
		sqlWithOutput := injectBeforeWhere(st.SQL, outputFrag)
		rows, err := tx.QueryContext(ctx, sqlWithOutput, namedArgs(st.Args)...)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		jsonIdx, timeIdx := buildColMaps(rows, nil)
		buf, err := drain(rows, cols, jsonIdx, timeIdx)
		rows.Close()
		if err != nil {
			return err
		}
		res.cols, res.rows = cols, buf
		res.affected, res.hasAff = int64(len(buf)), true
		return nil
	}

	out, err := tx.ExecContext(ctx, st.SQL, namedArgs(st.Args)...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true
	return nil
}

// executeDelete runs DELETE FROM [t] OUTPUT DELETED.* WHERE ...
// Compiler emits: DELETE FROM [t] WHERE [id]=@p1
// Rewritten to:   DELETE FROM [t] OUTPUT DELETED.[c],... WHERE [id]=@p1
func (b *Backend) executeDelete(
	ctx context.Context, tx *sql.Tx,
	q *ir.Query, returning []string, rel *schema.Relation,
	res *writeResult,
) error {
	st, apiErr := sqlgen.CompileDelete(Dialect{}, q, nil)
	if apiErr != nil {
		return apiErr
	}

	if len(returning) > 0 {
		outputFrag := buildOutputFragment("DELETED", returning)
		sqlWithOutput := injectBeforeWhere(st.SQL, outputFrag)
		rows, err := tx.QueryContext(ctx, sqlWithOutput, namedArgs(st.Args)...)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		jsonIdx, timeIdx := buildColMaps(rows, nil)
		buf, err := drain(rows, cols, jsonIdx, timeIdx)
		rows.Close()
		if err != nil {
			return err
		}
		res.cols, res.rows = cols, buf
		res.affected, res.hasAff = int64(len(buf)), true
		return nil
	}

	out, err := tx.ExecContext(ctx, st.SQL, namedArgs(st.Args)...)
	if err != nil {
		return err
	}
	n, _ := out.RowsAffected()
	res.affected, res.hasAff = n, true
	return nil
}

// executeCall runs a stored procedure or portable RPC function.
func (b *Backend) executeCall(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	var st *sqlgen.Statement
	var apiErr *pgerr.APIError
	if plan.Func != nil {
		// Portable registry function: the function body is a parameterised SQL
		// statement whose :name placeholders are bound by CompileCall.
		st, apiErr = sqlgen.CompileCall(Dialect{}, plan.Call, plan.Func)
	} else {
		// Native RPC (NativeRPC=true): no registry function — generate EXEC
		// [schema].[name] @param = @pN from the call's argument map.
		st, apiErr = b.compileNativeCall(plan.Call)
	}
	if apiErr != nil {
		return nil, apiErr
	}

	if plan.ReadOnly {
		res := &result{controls: rc.Controls()}
		// count=exact is only supported for portable registry functions; native
		// stored procedures cannot be wrapped in SELECT count(*) in T-SQL.
		if plan.Call.Count != ir.CountNone && plan.Func != nil {
			cst, apiErr := sqlgen.CompileCallCount(Dialect{}, plan.Call, plan.Func)
			if apiErr != nil {
				return nil, apiErr
			}
			if err := b.db.QueryRowContext(ctx, cst.SQL, namedArgs(cst.Args)...).Scan(&res.count); err != nil {
				return nil, b.MapError(err)
			}
			res.hasCount = true
		}
		rows, err := b.db.QueryContext(ctx, st.SQL, namedArgs(st.Args)...)
		if err != nil {
			return nil, b.MapError(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, b.MapError(err)
		}
		jsonIdx, timeIdx := buildColMaps(rows, nil)
		res.rows, res.cols, res.jsonIdx, res.timeIdx = rows, cols, jsonIdx, timeIdx
		return res, nil
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, st.SQL, namedArgs(st.Args)...)
	if err != nil {
		return nil, b.MapError(err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, b.MapError(err)
	}
	jsonIdx, timeIdx := buildColMaps(rows, nil)
	buf, err := drain(rows, cols, jsonIdx, timeIdx)
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

// buildOutputFragment builds "OUTPUT INSERTED.col1, INSERTED.col2" or DELETED.*.
func buildOutputFragment(table string, cols []string) string {
	d := Dialect{}
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = table + "." + d.QuoteIdent(c)
	}
	return "OUTPUT " + strings.Join(parts, ", ")
}

// injectBeforeValues inserts fragment before " VALUES " in an INSERT statement.
// The compiler always emits " VALUES " (with spaces) for non-empty inserts.
// DEFAULT VALUES inserts have no RETURNING in practice (no identity range to read
// back), so a fallback appends at the end.
func injectBeforeValues(sqlStr, fragment string) string {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " VALUES ")
	if idx < 0 {
		return sqlStr + " " + fragment
	}
	return sqlStr[:idx] + " " + fragment + sqlStr[idx:]
}

// injectBeforeWhere inserts fragment before " WHERE " in an UPDATE or DELETE.
// When there is no WHERE clause (bulk update/delete), the fragment is appended.
func injectBeforeWhere(sqlStr, fragment string) string {
	upper := strings.ToUpper(sqlStr)
	idx := strings.Index(upper, " WHERE ")
	if idx < 0 {
		return sqlStr + " " + fragment
	}
	return sqlStr[:idx] + " " + fragment + sqlStr[idx:]
}

// returningCols decides which columns to read back after a write.
func returningCols(q *ir.Query, rel *schema.Relation) []string {
	if q.Write != nil && q.Write.Return == ir.ReturnRepresentation {
		return rel.ColumnNames()
	}
	if q.Kind == ir.Insert || q.Kind == ir.Upsert {
		return rel.PrimaryKey
	}
	return nil
}

// compileNativeCall generates EXEC [schema].[name] @arg1 = @p1, @arg2 = @p2 for
// the NativeRPC path (plan.Func == nil). SQL Server stored procedures accept
// named parameters in any order, so the argument map can be emitted as-is.
// Scalar stored procedures should SELECT the result in a column named after the
// function (e.g. SELECT @a + @b AS [add]) so renderCall can detect scalar return
// by seeing a single column whose name matches the function name.
func (b *Backend) compileNativeCall(c *ir.Call) (*sqlgen.Statement, *pgerr.APIError) {
	sch := b.schema
	if sch == "" {
		sch = "dbo"
	}
	d := Dialect{}
	var sb strings.Builder
	sb.WriteString("EXEC ")
	sb.WriteString(d.QuoteIdent(sch))
	sb.WriteString(".")
	sb.WriteString(d.QuoteIdent(c.Function.Name))

	args := make([]any, 0, len(c.Args))
	i := 1
	for name, val := range c.Args {
		if i == 1 {
			sb.WriteString(" ")
		} else {
			sb.WriteString(", ")
		}
		sb.WriteString("@" + name + " = @p" + strconv.Itoa(i))
		// A POST arg has a decoded JSON value; a GET arg is raw text.
		if val.JSON != nil {
			args = append(args, nativeArgValue(val.JSON))
		} else {
			args = append(args, val.Text)
		}
		i++
	}
	return &sqlgen.Statement{SQL: sb.String(), Args: args}, nil
}

// nativeArgValue converts a decoded JSON argument value to a driver-ready type.
// Scalars (string, float64, bool, nil) pass through; composite values are
// re-encoded as JSON text so the stored procedure can receive them as NVARCHAR.
func nativeArgValue(v any) any {
	switch v.(type) {
	case string, float64, bool, nil:
		return v
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// _ is a compile-time check that Backend implements backend.DB.
var _ interface {
	Execute(context.Context, *ir.Plan, *reqctx.Context) (backend.Result, error)
} = (*Backend)(nil)
