// Package sqlserver is the SQL Server backend: the dialect (dialect.go), the
// version-computed capabilities (capabilities.go), and the runnable data plane
// in this file and its siblings (sqlserver.go, execute.go, introspect.go, result.go).
// SQL Server has a near-native security model (roles, row-level security, a
// session-context store, and stored procedures/functions), so the authz plane is
// mostly passed through to the engine. The SQL surface carries more friction than
// PostgreSQL: bracket-quoted identifiers, named @pN placeholders, OFFSET/FETCH paging,
// CASE WHEN NULL sort keys, OUTPUT instead of RETURNING (positioned mid-statement),
// and a multi-statement upsert pattern. See spec 13/14/15 for security emulation
// contracts and spec 06 for the dialect seam.
//
// DSN format (go-mssqldb):
//
//	sqlserver://user:pass@host:port?database=dbname
package sqlserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
)

const defaultPoolSize = 10

// Backend is the SQL Server implementation of the dbrest backend SPI.
type Backend struct {
	db      *sql.DB
	version Version
	caps    backend.Capabilities
	funcs   rpc.Registry
	schema  string // default schema (dbo)
}

// Open connects to SQL Server by DSN (sqlserver:// URL), reads the server
// version and edition, grades capabilities, and returns a ready Backend.
func Open(dsn string) (*Backend, error) {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQL Server: %w", err)
	}
	db.SetMaxOpenConns(defaultPoolSize)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// Read version string and EngineEdition (5 = Azure SQL Database).
	var verStr string
	var edition int
	row := db.QueryRowContext(context.Background(),
		"SELECT CAST(SERVERPROPERTY('ProductVersion') AS NVARCHAR(50)), CAST(SERVERPROPERTY('EngineEdition') AS INT)")
	if err := row.Scan(&verStr, &edition); err != nil {
		db.Close()
		return nil, err
	}
	ver := ParseVersion(verStr)
	// EngineEdition 5 = Azure SQL Database. Edition 9 = Azure SQL Edge but its
	// feature set matches SQL Server 2019 (v15), not Azure SQL Database, so it is
	// NOT treated as Azure here — it falls through to the version-based gates.
	ver.Azure = edition == 5

	// Determine the current schema.
	var sch string
	if err := db.QueryRowContext(context.Background(), "SELECT SCHEMA_NAME()").Scan(&sch); err != nil {
		sch = "dbo"
	}

	return &Backend{
		db:      db,
		version: ver,
		caps:    Capabilities(ver),
		schema:  sch,
	}, nil
}

// DB exposes the underlying pool, for tests.
func (b *Backend) DB() *sql.DB { return b.db }

// ServerVersion reports the parsed server version.
func (b *Backend) ServerVersion() Version { return b.version }

// Register installs the portable function registry exposed at /rpc/<fn>.
func (b *Backend) Register(reg rpc.Registry) { b.funcs = reg }

// Functions returns the registered function registry, or an empty one.
func (b *Backend) Functions() rpc.Registry {
	if b.funcs == nil {
		return rpc.EmptyRegistry{}
	}
	return b.funcs
}

// Schema returns the default schema name (usually "dbo").
func (b *Backend) Schema() string { return b.schema }

// Capabilities returns the computed capability tiers for this server version.
func (b *Backend) Capabilities() backend.Capabilities { return b.caps }

// Close closes the underlying connection pool.
func (b *Backend) Close() error { return b.db.Close() }

// MapError converts a go-mssqldb error to a PostgREST-compatible API error.
func (b *Backend) MapError(err error) *pgerr.APIError {
	if err == nil {
		return nil
	}
	if me, ok := asMSSQLError(err); ok {
		return mapSQLServerError(me)
	}
	return pgerr.ErrInternal(err.Error())
}

// mapSQLServerError builds the unified API error from a SQL Server error.
func mapSQLServerError(me mssql.Error) *pgerr.APIError {
	switch me.Number {
	case 2627, 2601: // unique constraint / unique index violation
		return pgerr.ErrUniqueViolation(me.Message)
	case 515: // cannot insert NULL
		return pgerr.ErrNotNullViolation(me.Message)
	case 547: // FK constraint violation
		return pgerr.ErrForeignKeyViolation(me.Message)
	case 207: // invalid column name
		return pgerr.New(400, "42703", me.Message)
	case 208: // invalid object name (table not found)
		return pgerr.New(404, "42P01", me.Message)
	case 2812: // procedure not found
		return pgerr.New(404, "42883", me.Message)
	case 229, 230: // permission denied
		return pgerr.New(403, "42501", me.Message)
	case 8152: // string or binary data would be truncated
		return pgerr.New(400, "22001", me.Message)
	case 245: // conversion failed (invalid value)
		return pgerr.New(400, "22P02", me.Message)
	}
	n := int(me.Number)
	if n >= 1 && n < 10000 {
		return pgerr.New(400, fmt.Sprintf("%05d", n), me.Message)
	}
	return pgerr.ErrInternal(me.Message)
}

// namedArgs wraps positional []any args as sql.Named("p1", ...) etc. so that
// go-mssqldb matches them against the @p1 / @p2 named placeholders the dialect emits.
func namedArgs(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = sql.Named("p"+strconv.Itoa(i+1), a)
	}
	return out
}

// sqlServerCanonicalType maps a SQL Server DATA_TYPE from INFORMATION_SCHEMA to
// the dbrest canonical PostgreSQL type name (spec 16).
func sqlServerCanonicalType(dataType string) string {
	switch strings.ToLower(dataType) {
	case "bit":
		return "boolean"
	case "tinyint":
		return "smallint"
	case "smallint":
		return "smallint"
	case "int":
		return "integer"
	case "bigint":
		return "bigint"
	case "decimal", "numeric", "money", "smallmoney":
		return "numeric"
	case "real":
		return "real"
	case "float":
		return "double precision"
	case "char", "nchar":
		return "text"
	case "varchar", "nvarchar", "text", "ntext":
		return "text"
	case "date":
		return "date"
	case "datetime", "datetime2", "smalldatetime":
		return "timestamp"
	case "datetimeoffset":
		return "timestamptz"
	case "time":
		return "time"
	case "uniqueidentifier":
		return "uuid"
	case "binary", "varbinary", "image":
		return "bytea"
	case "xml":
		return "text"
	case "json":
		return "json"
	default:
		return "text"
	}
}

// asMSSQLError unwraps err as a mssql.Error. mssql.Error implements error via a
// value receiver so errors.As requires a value target, not a pointer.
func asMSSQLError(err error) (mssql.Error, bool) {
	var me mssql.Error
	ok := errors.As(err, &me)
	return me, ok
}

func init() { backend.Register("sqlserver", sqlserverDriver{}) }

type sqlserverDriver struct{}

func (sqlserverDriver) Open(dsn string) (backend.Backend, error) { return Open(dsn) }
