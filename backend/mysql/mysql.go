// Package mysql is the MySQL/MariaDB backend: the dialect (dialect.go), the
// version-computed capabilities (capabilities.go), and the runnable data plane
// in this file and its siblings (mysql.go, execute.go, introspect.go, result.go).
// MySQL and MariaDB share the same driver (go-sql-driver/mysql, via database/sql)
// and the same dialect, but differ on a few capability gates (RETURNING on
// MariaDB 10.5+, regex availability). Both connect with the same DSN format:
//
//	dbrest:pass@tcp(host:3306)/dbname?parseTime=true
//
// The PostgreSQL security model (roles, RLS, GUC session context) is emulated
// entirely in-app: there is no SET LOCAL ROLE, no GUC push to the engine, and no
// RETURNING on MySQL 8.x. See spec 13/14/15 for the emulation contracts.
//
// NOTE: tinyInt1IsBool was removed in go-sql-driver v1.8. BOOL/TINYINT(1) → bool
// coercion is done by the result layer using the introspected schema.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	mysqldrv "github.com/go-sql-driver/mysql"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// defaultPoolSize mirrors PostgREST's default db-pool-size.
const defaultPoolSize = 10

// Backend is the MySQL/MariaDB implementation of the dbrest backend SPI.
type Backend struct {
	db      *sql.DB
	version Version
	caps    backend.Capabilities
	funcs   rpc.Registry
	schema  string // MySQL database name = exposed schema
}

// Open connects to MySQL/MariaDB by DSN, reads the server version, grades
// capabilities, and returns a ready Backend. The DSN should include parseTime=true
// (for DATE/DATETIME → time.Time); it is injected when absent. The removed
// tinyInt1IsBool DSN param is stripped if present (BOOL coercion is schema-driven).
func Open(dsn string) (*Backend, error) {
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL DSN: %w", err)
	}
	cfg.ParseTime = true
	delete(cfg.Params, "tinyInt1IsBool") // removed in v1.8; schema-layer handles coercion

	connector, err := mysqldrv.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("create MySQL connector: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(defaultPoolSize)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	var verStr string
	if err := db.QueryRowContext(context.Background(), "SELECT VERSION()").Scan(&verStr); err != nil {
		db.Close()
		return nil, err
	}
	ver := ParseVersion(verStr)

	var schema string
	if err := db.QueryRowContext(context.Background(), "SELECT DATABASE()").Scan(&schema); err != nil {
		db.Close()
		return nil, err
	}

	return &Backend{db: db, version: ver, caps: Capabilities(ver), schema: schema}, nil
}

// DB exposes the underlying pool, for tests.
func (b *Backend) DB() *sql.DB { return b.db }

// ServerVersion reports the parsed server version.
func (b *Backend) ServerVersion() Version { return b.version }

// Register installs the portable function registry exposed at /rpc/<fn>.
// MySQL has no function catalog of its own (NativeRPC is false), so every
// callable function must be declared in config.
func (b *Backend) Register(reg rpc.Registry) { b.funcs = reg }

// Functions returns the registered function registry, or an empty one.
func (b *Backend) Functions() rpc.Registry {
	if b.funcs == nil {
		return rpc.EmptyRegistry{}
	}
	return b.funcs
}

// Capabilities reports the MySQL/MariaDB feature tiers.
func (b *Backend) Capabilities() backend.Capabilities { return b.caps }

// Close releases the pool.
func (b *Backend) Close() error { return b.db.Close() }

// MapError turns a driver error into the unified envelope. MySQL errors carry a
// numeric error number; the mapping mirrors the SQLSTATE codes PostgREST returns
// for equivalent PostgreSQL violations.
func (b *Backend) MapError(err error) *pgerr.APIError {
	if err == nil {
		return nil
	}
	var me *mysqldrv.MySQLError
	if errors.As(err, &me) {
		return mapMySQLError(me)
	}
	return pgerr.ErrInternal(err.Error())
}

// mapMySQLError builds the unified API error from a MySQL driver error.
func mapMySQLError(me *mysqldrv.MySQLError) *pgerr.APIError {
	switch me.Number {
	case 1062: // ER_DUP_ENTRY
		return pgerr.ErrUniqueViolation(me.Message)
	case 1048: // ER_BAD_NULL_ERROR
		return pgerr.ErrNotNullViolation(me.Message)
	case 1406, 1264: // ER_DATA_TOO_LONG, ER_WARN_DATA_OUT_OF_RANGE
		return pgerr.ErrCheckViolation(me.Message)
	case 1451, 1452: // ER_ROW_IS_REFERENCED_2, ER_NO_REFERENCED_ROW_2
		return pgerr.ErrForeignKeyViolation(me.Message)
	case 1054, 1247: // ER_BAD_FIELD_ERROR, ER_ILLEGAL_REFERENCE
		return pgerr.New(400, "42703", me.Message)
	case 1146: // ER_NO_SUCH_TABLE
		return pgerr.New(404, "42P01", me.Message)
	case 1305, 1630: // ER_SP_DOES_NOT_EXIST, ER_FUNC_INEXIST
		return pgerr.New(404, "42883", me.Message)
	case 1045: // ER_ACCESS_DENIED_ERROR
		return pgerr.New(403, "28000", me.Message)
	case 1292, 1366: // ER_TRUNCATED_WRONG_VALUE, ER_INCORRECT_INTEGER_VALUE
		return pgerr.New(400, "22P02", me.Message)
	}
	if me.Number >= 1000 && me.Number < 2000 {
		return pgerr.New(400, fmt.Sprintf("%05d", me.Number), me.Message)
	}
	return pgerr.ErrInternal(me.Message)
}

// mysqlCanonicalType maps a MySQL DATA_TYPE + COLUMN_TYPE from INFORMATION_SCHEMA
// to the dbrest canonical PostgreSQL type name (spec 16).
func mysqlCanonicalType(dataType, columnType string) string {
	switch strings.ToLower(dataType) {
	case "tinyint":
		if strings.Contains(strings.ToLower(columnType), "tinyint(1)") {
			return "boolean"
		}
		return "smallint"
	case "smallint", "year":
		return "smallint"
	case "mediumint", "int", "integer":
		return "integer"
	case "bigint":
		return "bigint"
	case "decimal", "numeric":
		return "numeric"
	case "float":
		return "real"
	case "double", "double precision":
		return "double precision"
	case "bool", "boolean":
		return "boolean"
	case "char", "varchar", "tinytext", "mediumtext", "longtext", "text":
		return "text"
	case "binary", "varbinary", "tinyblob", "mediumblob", "longblob", "blob":
		return "bytea"
	case "date":
		return "date"
	case "datetime":
		return "timestamp"
	case "timestamp":
		return "timestamptz"
	case "time":
		return "time"
	case "json":
		return "json"
	case "uuid":
		return "uuid"
	case "enum", "set":
		return "text"
	default:
		return "text"
	}
}

// buildBoolCols returns the set of column names whose canonical type is
// "boolean" for the given relation. Used by buildColMaps to detect BOOL columns
// whose scan values (int8/uint8) need coercion to Go bool.
func buildBoolCols(rel *schema.Relation) map[string]bool {
	if rel == nil {
		return nil
	}
	var m map[string]bool
	for _, col := range rel.Columns {
		if col.Type == "boolean" {
			if m == nil {
				m = make(map[string]bool)
			}
			m[col.Name] = true
		}
	}
	return m
}

