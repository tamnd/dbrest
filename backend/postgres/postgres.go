// Package postgres is the PostgreSQL backend: the dialect (dialect.go), the
// version-computed capabilities (capabilities.go), and the runnable data plane
// in this file and its siblings (postgres.go, session.go, execute.go,
// introspect.go, result.go). PostgreSQL is dbrest's reference oracle, so the
// data plane mirrors PostgREST's own transaction model: every request runs in a
// transaction that sets the request role with SET LOCAL ROLE and pushes the
// request context (claims, method, path, headers, cookies) as GUCs with
// set_config, so row-level security and SQL policies see exactly what they see
// under PostgREST. See spec 14/15 and the implementation note.
package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
)

// defaultPoolMaxConns is the connection pool maximum when the DSN does not
// specify pool_max_conns. PostgREST defaults to 10; we match that.
const defaultPoolMaxConns = 10

// Backend is the PostgreSQL implementation of the dbrest backend SPI. It holds a
// connection pool, the server version (which grades a couple of capabilities),
// the function registry, and the search path applied to every request.
type Backend struct {
	pool          *pgxpool.Pool
	version       Version
	funcs         rpc.Registry
	searchPath    []string
	searchPathSQL string // pre-built "SET LOCAL search_path TO ..." statement
}

// Open connects to PostgreSQL by connection string (a libpq URI or keyword/value
// DSN), verifies the connection, and reads the server version so capabilities
// can be graded. The pool's own sizing is taken from the DSN (pool_max_conns
// and friends); when the DSN omits pool_max_conns the default is 10, matching
// PostgREST's default.
//
// pgx prepared-statement caching is enabled by default on the pool so repeated
// queries avoid a server-side parse on every execution. This is one of the key
// throughput advantages over PostgREST.
func Open(dsn string) (*Backend, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Mirror PostgREST's default pool size when the DSN does not specify one.
	if cfg.MaxConns < 1 {
		cfg.MaxConns = defaultPoolMaxConns
	}
	// Enable automatic prepared-statement caching so the server parses each
	// distinct query only once per connection. pgx stores the type-descriptor
	// cache on the connection; pgxpool serializes reuse so the cache is
	// consistent per connection lifetime.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	var ver string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&ver); err != nil {
		pool.Close()
		return nil, err
	}
	return &Backend{pool: pool, version: ParseVersion(ver)}, nil
}

// Pool exposes the underlying connection pool, for tests that seed a database.
func (b *Backend) Pool() *pgxpool.Pool { return b.pool }

// ServerVersion reports the parsed server version, for logging and tests.
func (b *Backend) ServerVersion() Version { return b.version }

// SetSchemas records the exposed schemas as the search path applied to every
// request (SET LOCAL search_path), matching PostgREST's db-schemas behaviour so
// unqualified names in policies and functions resolve the same way. The
// corresponding SQL statement is pre-built once here and reused per request.
func (b *Backend) SetSchemas(schemas []string) {
	b.searchPath = schemas
	b.searchPathSQL = buildSearchPathSQL(schemas)
}

// Register installs the portable function registry exposed at /rpc/<fn>. On
// PostgreSQL the engine has its own function catalog (NativeRPC is true), but a
// declared registry can still be supplied to expose portable functions; passing
// nil clears it.
func (b *Backend) Register(reg rpc.Registry) { b.funcs = reg }

// Functions returns the registered function registry, or an empty one when none
// has been installed, so the /rpc/<fn> endpoint always has a registry to query.
func (b *Backend) Functions() rpc.Registry {
	if b.funcs == nil {
		return rpc.EmptyRegistry{}
	}
	return b.funcs
}

// Capabilities reports the PostgreSQL feature tiers for the connected server
// version (spec 04/06).
func (b *Backend) Capabilities() backend.Capabilities {
	return Capabilities(b.version)
}

// Close releases the pool.
func (b *Backend) Close() error {
	b.pool.Close()
	return nil
}

// MapError turns a driver error into the unified envelope. A PostgreSQL error
// carries a SQLSTATE, message, detail, and hint; PostgREST surfaces all four and
// derives the HTTP status from the SQLSTATE class, so dbrest does the same: the
// SQLSTATE becomes the response code, the server's message/detail/hint pass
// through, and the status follows the same table PostgREST uses. A non-PG error
// (a dropped connection before a SQLSTATE was seen, say) is surfaced as internal.
func (b *Backend) MapError(err error) *pgerr.APIError {
	if err == nil {
		return nil
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return mapPgError(pg)
	}
	return pgerr.ErrInternal(err.Error())
}

// mapPgError builds the API envelope from a PostgreSQL error, passing the
// SQLSTATE through as the code and grading the HTTP status by the same rules
// PostgREST applies (its Error module's pgErrorStatus). The well-known
// constraint violations reuse dbrest's named constructors so their message and
// status match the other backends; everything else carries the server's own
// message with the status the SQLSTATE class implies.
func mapPgError(pg *pgconn.PgError) *pgerr.APIError {
	switch pg.Code {
	case "23505": // unique_violation
		return withServerText(pgerr.ErrUniqueViolation(pg.Detail), pg)
	case "23502": // not_null_violation
		return withServerText(pgerr.ErrNotNullViolation(pg.Detail), pg)
	case "23503": // foreign_key_violation
		return withServerText(pgerr.ErrForeignKeyViolation(pg.Detail), pg)
	case "23514": // check_violation
		return withServerText(pgerr.ErrCheckViolation(pg.Detail), pg)
	}
	e := pgerr.New(statusForSQLState(pg.Code), pg.Code, pg.Message)
	if pg.Detail != "" {
		e = e.WithDetails(pg.Detail)
	}
	if pg.Hint != "" {
		e = e.WithHint(pg.Hint)
	}
	return e
}

// withServerText keeps a named constructor's code and status but lets the
// server's hint ride through when it carries one, so a constraint error still
// reads like PostgREST's.
func withServerText(e *pgerr.APIError, pg *pgconn.PgError) *pgerr.APIError {
	if pg.Hint != "" {
		e = e.WithHint(pg.Hint)
	}
	return e
}

// statusForSQLState maps a PostgreSQL SQLSTATE to the HTTP status PostgREST
// returns for it. The table mirrors PostgREST's pgErrorStatus: most classes fold
// to 500, a few auth and resource classes have their own status, the constraint
// codes are 4xx, and a function can drive a custom status by raising a SQLSTATE
// in the PTxxx form (the three digits after PT are the status). The default for
// an unrecognized code is 400, as in PostgREST.
func statusForSQLState(code string) int {
	if len(code) != 5 {
		return 400
	}
	// PTxxx lets a function set the response status directly (PostgREST's
	// "RAISE sqlstate 'PT403'" convention); the digits after PT are the status.
	if code[:2] == "PT" {
		if n, err := strconv.Atoi(code[2:]); err == nil && n >= 100 && n <= 599 {
			return n
		}
	}
	switch code {
	case "23503", "23505": // foreign_key / unique violation
		return 409
	case "25006": // read_only_sql_transaction
		return 405
	case "42883": // undefined_function
		return 404
	case "42P01": // undefined_table
		return 404
	case "42501": // insufficient_privilege → 401 matching PostgREST
		return 401
	case "42P17": // infinite_recursion
		return 500
	}
	switch code[:2] {
	case "08": // connection exception
		return 503
	case "09": // triggered action exception
		return 500
	case "0L", "0P": // invalid grantor / invalid role specification
		return 403
	case "23": // integrity constraint (not_null, check, ...) default
		return 400
	case "25": // invalid transaction state
		return 500
	case "28": // invalid authorization specification
		return 403
	case "2D", "38", "39", "3B": // routine / savepoint exceptions
		return 500
	case "40": // transaction rollback
		return 500
	case "53": // insufficient resources
		return 503
	case "54": // program limit exceeded (statement too complex)
		return 413
	case "55": // object not in prerequisite state
		return 500
	case "57": // operator intervention
		return 500
	case "58": // system error
		return 500
	case "F0": // configuration file error
		return 500
	case "HV": // foreign data wrapper error
		return 500
	case "P0": // PL/pgSQL raise_exception and friends
		return 400
	case "XX": // internal error
		return 500
	case "42": // syntax error / access rule violation (undefined column, ...)
		return 400
	}
	return 400
}

func init() { backend.Register("postgres", postgresDriver{}) }

type postgresDriver struct{}

func (postgresDriver) Open(dsn string) (backend.Backend, error) { return Open(dsn) }
