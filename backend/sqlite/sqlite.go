package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	sqlitedrv "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

func init() {
	// regexp(pattern, value) backs the match/imatch operators. It is registered
	// process-wide on the modernc driver; case-insensitivity rides on a (?i)
	// prefix the dialect prepends to the pattern.
	sqlitedrv.MustRegisterDeterministicScalarFunction("regexp", 2, regexpFunc)
}

func regexpFunc(_ *sqlitedrv.FunctionContext, args []driver.Value) (driver.Value, error) {
	pat, ok := asString(args[0])
	if !ok {
		return nil, fmt.Errorf("regexp: pattern is not text")
	}
	val, ok := asString(args[1])
	if !ok {
		// A NULL or non-text subject never matches, matching SQL NULL semantics.
		return false, nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("regexp: %w", err)
	}
	return re.MatchString(val), nil
}

func asString(v driver.Value) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return "", false
	}
}

// Backend is the SQLite implementation of the dbrest backend SPI.
type Backend struct {
	db    *sql.DB
	funcs rpc.Registry
}

// Open connects to a SQLite database by DSN (a file path, or ":memory:" for an
// in-process database) and returns a ready Backend.
func Open(dsn string) (*Backend, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite does not enforce FK constraints by default, and its LIKE folds ASCII
	// case by default, which makes the like operator silently case-insensitive
	// unlike PostgreSQL. Pin to one connection so both PRAGMAs stay in effect for
	// the lifetime of the pool.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, err
	}
	// case_sensitive_like = ON makes the like operator case-sensitive to match
	// PostgreSQL; ilike folds case explicitly in the dialect (lower() LIKE lower()).
	if _, err := db.Exec("PRAGMA case_sensitive_like = ON"); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Backend{db: db}, nil
}

// DB exposes the underlying pool, for tests that seed a database.
func (b *Backend) DB() *sql.DB { return b.db }

// Register installs the portable function registry exposed at /rpc/<fn>. SQLite
// has no function catalog of its own (NativeRPC is false), so every callable
// function is declared in config and supplied here. Passing nil clears it.
func (b *Backend) Register(reg rpc.Registry) { b.funcs = reg }

// Functions returns the registered function registry, or an empty one when none
// has been installed, so the /rpc/<fn> endpoint always has a registry to query.
func (b *Backend) Functions() rpc.Registry {
	if b.funcs == nil {
		return rpc.EmptyRegistry{}
	}
	return b.funcs
}

// Capabilities reports the SQLite feature tiers (spec 04/06). The security model
// (roles, RLS, GUCs) is emulated; most SQL features are native.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		Returning:            backend.Native,
		Upsert:               backend.Native,
		UpsertConflictTarget: true,
		NullsOrdering:        backend.Native,
		JSONAssembly:         backend.Native,
		IsDistinctFrom:       backend.Native,
		Transactions:         backend.TxFull,
		NativeRoles:          false,
		NativeRLS:            false,
		SessionContext:       backend.Emulated,
		NativeRPC:            false,
		Regex:                backend.Native,
		FullText:             backend.FTSQLite5,
		ArrayRangeTypes:      backend.Unsupported,
		Schemas:              backend.SchemaAttached,
		Aggregates:           backend.Native,
		Embedding:            backend.EmbedJoin,
		CountPlanned:         backend.BestEffort,
	}
}

// Close releases the pool.
func (b *Backend) Close() error { return b.db.Close() }

// MapError turns a driver error into the unified envelope. A SQLite constraint
// violation maps to the PostgreSQL SQLSTATE PostgREST would report (so clients
// see the same code on every backend) with the matching HTTP status, and to a
// PG-shaped message synthesized from what SQLite reports. SQLite names the
// relation and column in its NOT NULL and UNIQUE text, so those reconstruct
// PostgreSQL's own wording; it gives no constraint name for a unique key and no
// offending value, so neither is invented (an emulation limitation, not a
// fabricated wire contract). The native text is never leaked into details.
// Anything else is surfaced as internal.
func (b *Backend) MapError(err error) *pgerr.APIError {
	if err == nil {
		return nil
	}
	if se, ok := errors.AsType[*sqlitedrv.Error](err); ok {
		// The primary result code is the low byte; the rest is the extended code.
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return pgerr.ErrConstraintViolation(pgerr.CodeUniqueViolation,
				"duplicate key value violates unique constraint", "", "")
		case sqlite3.SQLITE_CONSTRAINT_NOTNULL:
			return pgerr.ErrConstraintViolation(pgerr.CodeNotNullViolation,
				notNullMessage(se.Error()), "", "")
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return pgerr.ErrConstraintViolation(pgerr.CodeForeignKeyViolation,
				"insert or update on table violates foreign key constraint", "", "")
		case sqlite3.SQLITE_CONSTRAINT_CHECK:
			return pgerr.ErrConstraintViolation(pgerr.CodeCheckViolation,
				checkMessage(se.Error()), "", "")
		}
		if se.Code()&0xff == sqlite3.SQLITE_CONSTRAINT {
			return pgerr.ErrConstraintViolation(pgerr.CodeCheckViolation,
				"new row violates check constraint", "", "")
		}
	}
	return pgerr.ErrInternal(err.Error())
}

// constraintTarget matches the "table.column" SQLite names after the colon in a
// constraint failure ("NOT NULL constraint failed: films.title").
var constraintTarget = regexp.MustCompile(`([^\s,.]+)\.([^\s,.]+)`)

// notNullMessage reconstructs PostgreSQL's not-null wording from SQLite's "NOT
// NULL constraint failed: relation.column" text. PostgreSQL reports `null value
// in column "c" of relation "t" violates not-null constraint`; SQLite supplies
// both names, so the message matches verbatim. When the text does not parse the
// generic message stands.
func notNullMessage(text string) string {
	if m := constraintTarget.FindStringSubmatch(text); m != nil {
		return fmt.Sprintf(
			"null value in column %q of relation %q violates not-null constraint",
			m[2], m[1])
	}
	return "null value violates not-null constraint"
}

// checkMessage reconstructs PostgreSQL's check wording from SQLite's "CHECK
// constraint failed: name" text. PostgreSQL names the constraint (`new row for
// relation "t" violates check constraint "c"`); SQLite gives only the
// constraint name (or the expression for an anonymous check), so the name rides
// through when present and the generic message stands otherwise.
func checkMessage(text string) string {
	const prefix = "CHECK constraint failed: "
	if name := strings.TrimPrefix(text, prefix); name != text && name != "" {
		return fmt.Sprintf("new row violates check constraint %q", name)
	}
	return "new row violates check constraint"
}

// Execute lowers a resolved plan to SQLite operations and returns a streamable
// result. Reads stream from an open cursor; writes run in a short transaction
// and buffer their returned rows. RPC arrives with its subsystem.
func (b *Backend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if plan.Call != nil {
		return b.executeCall(ctx, plan, rc)
	}
	if plan.Query == nil {
		return nil, pgerr.ErrUnsupported("this operation", "sqlite")
	}
	switch plan.Query.Kind {
	case ir.Read:
		return b.executeRead(ctx, plan, rc)
	case ir.Insert, ir.Upsert, ir.Update, ir.Delete:
		return b.executeWrite(ctx, plan, rc)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "sqlite")
	}
}

// executeCall lowers and runs an RPC call. A read-only function (stable or
// immutable) streams from an open cursor, like a read; a volatile function runs
// inside a committing transaction, like a write, so its side effects persist
// (or roll back under Prefer: tx=rollback). The returned rows carry the
// function's output for the renderer to shape by return kind.
func (b *Backend) executeCall(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	st, apiErr := sqlgen.CompileCall(dialect{}, plan.Call, plan.Func, sqlgen.ContextArgs(rc))
	if apiErr != nil {
		return nil, apiErr
	}

	if plan.ReadOnly {
		res := &result{controls: rc.Controls()}
		// A count over a read-only function runs as its own statement, like a read.
		if plan.Call.Count != ir.CountNone {
			cst, apiErr := sqlgen.CompileCallCount(dialect{}, plan.Call, plan.Func, sqlgen.ContextArgs(rc))
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
		// A portable function that steers the response projects reserved
		// response-control columns. Buffer only then, so the common streaming
		// path is untouched, lift the controls out, and strip them from the body.
		if backend.HasResponseControlCols(cols) {
			buf, err := drain(rows, len(cols))
			rows.Close()
			if err != nil {
				return nil, b.MapError(err)
			}
			cols, buf, apiErr := backend.LiftResponseControls(cols, buf, res.controls)
			if apiErr != nil {
				return nil, apiErr
			}
			return &writeResult{cols: cols, rows: buf, count: res.count, hasCount: res.hasCount, controls: res.controls}, nil
		}
		res.rows, res.cols = rows, cols
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
	buf, err := drain(rows, len(cols))
	rows.Close()
	if err != nil {
		return nil, b.MapError(err)
	}

	// A volatile function steers the response the same way a read-only one does:
	// reserved columns lift into the controls and drop out of the body. An invalid
	// status or header set fails the call before commit, so the deferred rollback
	// discards the mutation.
	controls := rc.Controls()
	cols, buf, apiErr = backend.LiftResponseControls(cols, buf, controls)
	if apiErr != nil {
		return nil, apiErr
	}

	res := &writeResult{cols: cols, rows: buf, controls: controls}
	if plan.Call.Prefer.Tx != nil && *plan.Call.Prefer.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// executeRead compiles and runs the windowed read, plus a separate COUNT(*) when
// one is requested so Content-Range carries the total.
func (b *Backend) executeRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	res := &result{controls: rc.Controls()}

	// On SQLite every count strategy resolves to an exact count (the
	// planned/estimated tiers are best-effort and downgrade here, per the matrix).
	if plan.Query.Count != ir.CountNone {
		cst, apiErr := sqlgen.CompileCount(dialect{}, plan.Query)
		if apiErr != nil {
			return nil, apiErr
		}
		if err := b.db.QueryRowContext(ctx, cst.SQL, cst.Args...).Scan(&res.count); err != nil {
			return nil, b.MapError(err)
		}
		res.hasCount = true
	}

	st, apiErr := sqlgen.CompileRead(dialect{}, plan.Query)
	if apiErr != nil {
		return nil, apiErr
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
	res.rows, res.cols = rows, cols
	return res, nil
}

// executeWrite compiles the mutation, runs it in a transaction, and buffers any
// returned rows. The transaction commits unless the client asked for a
// rollback (Prefer: tx=rollback), in which case the representation still
// reflects the would-be result but nothing is persisted.
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

	st, apiErr := compileWrite(q, returning)
	if apiErr != nil {
		return nil, apiErr
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, b.MapError(err)
	}
	// A single deferred rollback covers every early return; it is a no-op once
	// the transaction has committed below, so the happy path is unaffected.
	defer func() { _ = tx.Rollback() }()

	res := &writeResult{controls: rc.Controls()}

	// An upsert's 200-vs-201 status turns on whether any row updated an existing
	// one. SQLite has no xmax to read back (the PostgreSQL signal), so before the
	// write we check, in the same transaction, whether any payload row's
	// conflict-target key already exists; if none does the upsert is all-insert.
	if q.Kind == ir.Upsert {
		if inserted, ok, derr := detectUpsertInsert(ctx, tx, q, plan.Rel); derr != nil {
			return nil, b.MapError(derr)
		} else if ok {
			res.controls.UpsertStatusKnown = true
			res.controls.InsertedRows = inserted
		}
	}
	if len(returning) > 0 {
		rows, err := tx.QueryContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, b.MapError(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, b.MapError(err)
		}
		buf, err := drain(rows, len(cols))
		rows.Close()
		if err != nil {
			return nil, b.MapError(err)
		}
		// The affected count is the full mutated set, taken before the
		// representation is shaped: order/limit/offset bound only the returned
		// body, not the mutation (v13 dropped limited update/delete).
		res.affected, res.hasAff = int64(len(buf)), true
		res.cols, res.rows = cols, backend.ShapeWriteRepresentation(cols, buf, q)
	} else {
		out, err := tx.ExecContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, b.MapError(err)
		}
		n, _ := out.RowsAffected()
		res.affected, res.hasAff = n, true
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

	// Prefer: tx=rollback returns the computed representation but discards the
	// work; leaving the transaction for the deferred rollback does exactly that.
	if q.Write != nil && q.Write.Tx == ir.TxRollback {
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, b.MapError(err)
	}
	return res, nil
}

// detectUpsertInsert counts how many of the payload rows the upsert will insert
// as new (those whose conflict-target key does not already exist) so the HTTP
// layer can choose 200 vs 201. It runs inside the write transaction, before the
// upsert statement, and returns ok=false when the target columns are unknown (no
// explicit on_conflict and no primary key), leaving the status to the default.
// The conflict target defaults to the relation's primary key, matching the
// upsert's own ON CONFLICT.
func detectUpsertInsert(ctx context.Context, tx *sql.Tx, q *ir.Query, rel *schema.Relation) (inserted int, ok bool, err error) {
	if q.Write == nil || len(q.Write.Rows) == 0 {
		return 0, false, nil
	}
	// Only merge-duplicates can turn into an update; an ignore-duplicates upsert
	// (ON CONFLICT DO NOTHING) is a no-op insert on a conflict, which PostgreSQL
	// reports through RETURNING as all-insert and PostgREST renders as 201. So a
	// PUT (no Conflict spec) and a merge upsert run detection; an ignore upsert
	// keeps the 201 default.
	if q.Write.Conflict != nil && q.Write.Conflict.Resolution == ir.ConflictIgnore {
		return 0, false, nil
	}
	target := rel.PrimaryKey
	if q.Write.Conflict != nil && len(q.Write.Conflict.Target) > 0 {
		target = q.Write.Conflict.Target
	}
	if len(target) == 0 {
		return 0, false, nil
	}

	d := dialect{}
	var where strings.Builder
	for i, c := range target {
		if i > 0 {
			where.WriteString(" AND ")
		}
		where.WriteString(d.QuoteIdent(c))
		where.WriteString(" = ?")
	}
	query := "SELECT 1 FROM " + d.QuoteIdent(rel.Name) + " WHERE " + where.String() + " LIMIT 1"

	for _, row := range q.Write.Rows {
		args := make([]any, len(target))
		for i, c := range target {
			// A payload missing a key column cannot match an existing row by it;
			// treat that row as an insert and move on.
			v, present := row[c]
			if !present {
				args = nil
				break
			}
			args[i] = sqlgen.WriteArg(d, v)
		}
		if args == nil {
			inserted++
			continue
		}
		var dummy int
		switch scanErr := tx.QueryRowContext(ctx, query, args...).Scan(&dummy); scanErr {
		case nil:
			// This row matches an existing key: an ON CONFLICT update, not an insert.
		case sql.ErrNoRows:
			// No existing row: this one is a new insert.
			inserted++
		default:
			return 0, false, scanErr
		}
	}
	return inserted, true, nil
}

// compileWrite dispatches to the right compiler for the mutation kind.
func compileWrite(q *ir.Query, returning []string) (*sqlgen.Statement, *pgerr.APIError) {
	switch q.Kind {
	case ir.Insert, ir.Upsert:
		return sqlgen.CompileInsert(dialect{}, q, returning)
	case ir.Update:
		return sqlgen.CompileUpdate(dialect{}, q, returning)
	case ir.Delete:
		return sqlgen.CompileDelete(dialect{}, q, returning)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "sqlite")
	}
}

// returningCols decides which columns a write reads back. The representation
// returns the whole row; a minimal insert/upsert still returns the primary key
// so the handler can build the Location header; a minimal update/delete returns
// nothing and runs as a plain affected-rows statement.
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

// drain reads every row of a returning cursor into memory, applying the same
// type coercions as rowStream.Values: []byte→string, BOOLEAN int64→bool,
// JSON string→json.RawMessage.
func drain(rows *sql.Rows, ncols int) ([][]any, error) {
	colTypes, _ := rows.ColumnTypes()
	var out [][]any
	for rows.Next() {
		holders := make([]any, ncols)
		ptrs := make([]any, ncols)
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range holders {
			if bs, ok := v.([]byte); ok {
				v = string(bs)
				holders[i] = v
			}
			if colTypes != nil && i < len(colTypes) {
				switch strings.ToUpper(colTypes[i].DatabaseTypeName()) {
				case "BOOLEAN", "BOOL":
					if n, ok := v.(int64); ok {
						holders[i] = n != 0
					}
				case "JSON":
					if str, ok := v.(string); ok && json.Valid([]byte(str)) {
						holders[i] = json.RawMessage(str)
					}
				}
			}
		}
		out = append(out, holders)
	}
	return out, rows.Err()
}

func init() { backend.Register("sqlite", sqliteDriver{}) }

type sqliteDriver struct{}

func (sqliteDriver) Open(dsn string) (backend.Backend, error) { return Open(dsn) }
