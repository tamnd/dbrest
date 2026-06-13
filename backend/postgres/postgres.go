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
	"strings"
	"time"

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
	pool            *pgxpool.Pool
	version         Version
	funcs           rpc.Registry
	searchPath      []string
	extraSearchPath []string                  // db-extra-search-path, appended after the active schema
	loc             *time.Location            // server TimeZone, for rendering timestamptz like PostgREST
	funcVol         map[string]rpc.Volatility  // "schema.name" -> volatility, for native RPC access mode
	funcRet         map[string]rpc.ReturnShape // "schema.name" -> return shape, for native RPC result rendering
	funcReg         map[string]rpc.Registry    // schema -> native function registry, the function half of the schema cache
	roleSettings    map[string][]roleSetting  // impersonated-role ALTER ROLE ... SET replays
	roleIsolation   map[string]pgx.TxIsoLevel // impersonated-role default_transaction_isolation

	hoistedTxSettings []string                 // db-hoisted-tx-settings: which function SET options hoist to the tx
	funcProconfig     map[string][]roleSetting // "schema.name" -> function SET clause (pg_proc.proconfig)
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
	return OpenWith(dsn, Options{PreparedStatements: true})
}

// Options carries the open-time settings the postgres backend can vary. The zero
// value is not the default: callers use Open (prepared statements on) or pass an
// explicit Options.
type Options struct {
	// PreparedStatements maps PostgREST's db-prepared-statements. On (the default),
	// the pool uses cache_describe so each distinct query is parsed once per
	// connection. Off selects the unprepared exec protocol, which parameterizes
	// every query over the extended protocol without caching a statement, the
	// pooler-safe equivalent of PostgREST's "parameterized but not prepared".
	PreparedStatements bool
}

// OpenWith connects like Open but honors the supplied Options.
func OpenWith(dsn string, opts Options) (*Backend, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Mirror PostgREST's default pool size when the DSN does not specify one.
	if cfg.MaxConns < 1 {
		cfg.MaxConns = defaultPoolMaxConns
	}
	cfg.ConnConfig.DefaultQueryExecMode = resolveExecMode(dsn, cfg.ConnConfig.DefaultQueryExecMode, opts.PreparedStatements)
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
	// PostgREST assembles JSON in the database, so a timestamptz renders in the
	// server's TimeZone. Capture it once here and render timestamptz in the same
	// zone (DST included) so the wire value matches; fall back to UTC when the
	// name does not resolve to a Go location.
	loc := time.UTC
	var tz string
	if err := pool.QueryRow(ctx, "SHOW timezone").Scan(&tz); err == nil {
		if l, lerr := time.LoadLocation(tz); lerr == nil {
			loc = l
		}
	}
	return &Backend{pool: pool, version: ParseVersion(ver), loc: loc}, nil
}

// resolveExecMode picks the pool's default query exec mode. An explicit DSN
// choice wins (default_query_exec_mode=simple_protocol or exec, pgx's documented
// escape hatch for poolers); honor it rather than clobbering it. pgx parses the
// param into parsed, but an omitted value and an explicit cache_statement both
// decode to the same zero value, so the presence test keys on the raw DSN string,
// where the param name is unambiguous. With no DSN choice, db-prepared-statements
// decides: on (the default) selects cache_describe, which parses each distinct
// query once per connection while keeping unnamed statements a transaction-mode
// pooler (PgBouncer) tolerates; off selects exec, which parameterizes every query
// without preparing one, matching PostgREST's db-prepared-statements=false.
func resolveExecMode(dsn string, parsed pgx.QueryExecMode, prepared bool) pgx.QueryExecMode {
	if strings.Contains(dsn, "default_query_exec_mode") {
		return parsed
	}
	if !prepared {
		return pgx.QueryExecModeExec
	}
	return pgx.QueryExecModeCacheDescribe
}

// Pool exposes the underlying connection pool, for tests that seed a database.
func (b *Backend) Pool() *pgxpool.Pool { return b.pool }

// ServerVersion reports the parsed server version, for logging and tests.
func (b *Backend) ServerVersion() Version { return b.version }

// SetSchemas records the exposed schemas. The first is the default active
// schema; the rest are reachable by Accept-Profile/Content-Profile. The
// per-request search_path is built from the active schema (not the whole set),
// matching PostgREST, which puts only the active schema plus db-extra-search-path
// on the path so unqualified names resolve the same way (see queueSessionItems).
func (b *Backend) SetSchemas(schemas []string) {
	b.searchPath = schemas
}

// SetExtraSearchPath records db-extra-search-path: schemas appended to the
// search_path after the active schema so type and function resolution can reach
// them without exposing them as queryable schemas. PostgREST defaults this to
// "public" and does not dedup, so a request on the public schema gets the path
// "public", "public"; dbrest reproduces that verbatim.
func (b *Backend) SetExtraSearchPath(schemas []string) {
	b.extraSearchPath = schemas
}

// SetHoistedTxSettings records db-hoisted-tx-settings: the function SET options
// (statement_timeout, plan_filter.statement_cost_limit,
// default_transaction_isolation by default) that an RPC call hoists to the
// transaction so they override the role and connection settings for the whole
// statement, matching PostgREST. The named settings are applied per call from the
// function's introspected proconfig (see hoistFor).
func (b *Backend) SetHoistedTxSettings(names []string) {
	b.hoistedTxSettings = names
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

// SchemaFunctions returns the native function registry introspected for one
// exposed schema, the function half of the schema cache. It is empty until
// Introspect has run, and empty for a schema with no functions, so a caller always
// has a registry to resolve against. The native RPC path uses it to resolve
// overloads and partition GET arguments through the shared planner.
func (b *Backend) SchemaFunctions(schema string) rpc.Registry {
	if reg, ok := b.funcReg[schema]; ok {
		return reg
	}
	return rpc.EmptyRegistry{}
}

// Capabilities reports the PostgreSQL feature tiers for the connected server
// version (spec 04/06).
func (b *Backend) Capabilities() backend.Capabilities {
	return Capabilities(b.version)
}

// SupportsPreRequest reports that this backend runs the db-pre-request function.
// queueSessionItems issues SELECT <fn>() in the request transaction after the
// session settings, so main.go accepts the option here rather than refusing it.
func (b *Backend) SupportsPreRequest() bool { return true }

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
	return mapTransportError(err)
}

// mapTransportError classifies a driver-level failure that never reached a
// SQLSTATE into PostgREST's connection-error family (group 0). A failed or
// refused dial surfaces from pgx as *pgconn.ConnectError and becomes PGRST000
// (503, retryable); a pool-acquisition timeout surfaces as a context deadline
// and becomes PGRST003 (504, the "Timed out acquiring connection" case);
// anything else stays an internal 500. PostgREST also has PGRST002 for a schema
// cache that cannot be built, but dbrest builds the cache at startup and refuses
// to start on failure, so that code has no runtime analog here.
func mapTransportError(err error) *pgerr.APIError {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return pgerr.ErrDBConnection(err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return pgerr.ErrAcquireTimeout()
	}
	return pgerr.ErrInternal(err.Error())
}

// mapPgError builds the API envelope from a PostgreSQL error, passing the
// SQLSTATE through as the code, the server's own message and detail and hint
// verbatim, and the HTTP status graded by the same rules PostgREST applies (its
// Error module's pgErrorStatus). PostgREST forwards PostgreSQL errors unchanged,
// constraint name and "Key (col)=(val)" detail included, so the postgres backend
// does too rather than rewriting them to a canonical text; the SQLSTATE class
// alone fixes the status (a unique or foreign-key violation is 409, the rest of
// class 23 is 400). The named constructors stay for the backends whose driver
// reports a constraint without PostgreSQL's wording.
func mapPgError(pg *pgconn.PgError) *pgerr.APIError {
	// A function can take full control of the response by raising SQLSTATE
	// 'PGRST': the server reports the chosen envelope in MESSAGE and the status
	// and headers in DETAIL, both as JSON. FromRaise parses both (or yields
	// PGRST121 on a malformed payload); its headers ride on the error so the
	// renderer emits them. This is distinct from the PTxxx status-only convention
	// handled in statusForSQLState.
	if pg.Code == "PGRST" {
		e, headers := pgerr.FromRaise(pg.Message, pg.Detail)
		return e.WithHeaders(headers)
	}
	e := pgerr.New(statusForSQLState(pg.Code, pg.Message), pg.Code, pg.Message)
	if pg.Detail != "" {
		e = e.WithDetails(pg.Detail)
	}
	if pg.Hint != "" {
		e = e.WithHint(pg.Hint)
	}
	return e
}

// statusForSQLState maps a PostgreSQL SQLSTATE to the HTTP status PostgREST
// returns for it. The table mirrors PostgREST v14's pgErrorStatus (Error.hs)
// row for row: most classes fold to 500, a few auth and resource classes have
// their own status, the constraint codes are 4xx, two codes (21000, 22023)
// disambiguate on the server message, a function can drive a custom status with
// the PTxxx convention, and the default for an unrecognized code is 400. msg is
// the server message, needed only for the two message-sniffing rows.
//
// PostgREST reads the status off the raw integer for PTxxx and would emit even a
// nonsensical value; Go's response writer rejects a status below 100, so a PT
// status under 100 falls back to 500 here rather than panicking. Every PTxxx in
// the realistic 100-599 range (and up to 999) passes through unchanged.
func statusForSQLState(code, msg string) int {
	if len(code) != 5 {
		return 400
	}
	// PTxxx lets a function set the response status directly (PostgREST's
	// "RAISE sqlstate 'PT403'" convention); the digits after PT are the status.
	// PostgREST falls back to 500 when the suffix does not parse.
	if code[:2] == "PT" {
		if n, err := strconv.Atoi(code[2:]); err == nil && n >= 100 && n <= 999 {
			return n
		}
		return 500
	}
	switch code {
	case "23503", "23505": // foreign_key / unique violation
		return 409
	case "25006": // read_only_sql_transaction
		return 405
	case "21000": // cardinality_violation: pg-safeupdate's missing-WHERE guard is
		// a client error (400); the generic "more than one row" form is a server
		// error (500), matching PostgREST's suffix test.
		if strings.HasSuffix(msg, "requires a WHERE clause") {
			return 400
		}
		return 500
	case "22023": // invalid_parameter_value: a JWT naming a role that does not
		// exist is an auth failure (401); everything else is a client error (400).
		if strings.HasPrefix(msg, "role") && strings.HasSuffix(msg, "does not exist") {
			return 401
		}
		return 400
	case "53400": // configuration_limit_exceeded: 500, not the 503 of its class
		return 500
	case "57P01": // admin_shutdown: 503-with-retry, not the 500 of its class
		return 503
	case "42P01": // undefined_table
		return 404
	case "42P17": // infinite_recursion
		return 500
	case "42501": // insufficient_privilege: 403 base, lifted to 401 for an
		// anonymous request by mapExecError, mirroring PostgREST's pgErrorStatus.
		return 403
	case "P0001": // raise_exception default code: client error
		return 400
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
		return 500
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
	case "P0": // PL/pgSQL raise_exception and friends (P0001 handled above)
		return 500
	case "XX": // internal error
		return 500
	case "42": // syntax / access rule violation; 42883 splits on the message
		if code == "42883" { // undefined_function: xmlagg ambiguity is a 406
			if strings.HasPrefix(msg, "function xmlagg(") {
				return 406
			}
			return 404
		}
		return 400
	}
	return 400
}

func init() { backend.Register("postgres", postgresDriver{}) }

type postgresDriver struct{}

func (postgresDriver) Open(dsn string) (backend.Backend, error) { return Open(dsn) }

// OpenWithOptions implements backend.OptionsDriver so the server can thread
// db-prepared-statements through the generic registry. PreparedStatements
// defaults to on when the option is unset.
func (postgresDriver) OpenWithOptions(dsn string, opts backend.OpenOptions) (backend.Backend, error) {
	prepared := true
	if opts.PreparedStatements != nil {
		prepared = *opts.PreparedStatements
	}
	return OpenWith(dsn, Options{PreparedStatements: prepared})
}
