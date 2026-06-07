package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"

	sqlitedrv "modernc.org/sqlite"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
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
	db *sql.DB
}

// Open connects to a SQLite database by DSN (a file path, or ":memory:" for an
// in-process database) and returns a ready Backend.
func Open(dsn string) (*Backend, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
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

// MapError turns a driver error into the unified envelope. The read path raises
// no integrity-constraint violations; the constraint-to-SQLSTATE mapping arrives
// with the writes subsystem, so for now any driver error is surfaced as internal.
func (b *Backend) MapError(err error) *pgerr.APIError {
	if err == nil {
		return nil
	}
	return pgerr.ErrInternal(err.Error())
}

// Execute lowers a resolved plan to SQLite operations and returns a streamable
// result. Reads stream from an open cursor; writes and RPC arrive with their
// subsystems.
func (b *Backend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if plan.Query == nil {
		return nil, pgerr.ErrUnsupported("this operation", "sqlite")
	}
	switch plan.Query.Kind {
	case ir.Read:
		return b.executeRead(ctx, plan, rc)
	default:
		return nil, pgerr.ErrUnsupported("this operation", "sqlite")
	}
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
