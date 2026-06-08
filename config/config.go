// Package config loads dbrest's configuration once at startup and holds it in
// memory; the process is otherwise stateless. The option surface follows
// PostgREST: the same key names and meanings where they carry over, plus a
// small set of dbrest additions for selecting a backend and for the declared
// registries that fill the gap on engines without engine-side metadata.
//
// There are two sources, applied in order: a flat key/value file using the
// PostgREST option names, then the environment. An option is settable under the
// PostgREST PGRST_* spelling (so an existing deployment's environment keeps
// working) and the native DBREST_* spelling; when both are present DBREST_*
// wins. The environment overrides the file. See spec 20.
package config

import (
	"fmt"
	"maps"
	"strings"
	"time"
)

// Backend names the engine implementation handling every request. Exactly one
// is active per process.
const (
	BackendPostgres  = "postgres"
	BackendSQLite    = "sqlite"
	BackendMySQL     = "mysql"
	BackendSQLServer = "sqlserver"
	BackendMongoDB   = "mongodb"
)

// knownBackends is the accepted db-backend set. A backend may be known to the
// configuration yet not built into this binary; selecting an unbuilt one is the
// command's error to raise, not the parser's.
var knownBackends = map[string]bool{
	BackendPostgres: true, BackendSQLite: true, BackendMySQL: true,
	BackendSQLServer: true, BackendMongoDB: true,
}

// OpenAPI generation modes (spec 19).
const (
	OpenAPIFollowPrivileges = "follow-privileges"
	OpenAPIIgnorePrivileges = "ignore-privileges"
	OpenAPIDisabled         = "disabled"
)

var knownOpenAPIModes = map[string]bool{
	OpenAPIFollowPrivileges: true, OpenAPIIgnorePrivileges: true, OpenAPIDisabled: true,
}

var knownLogLevels = map[string]bool{
	"crit": true, "error": true, "warn": true, "info": true, "debug": true,
}

// Config is the resolved option set. Fields are grouped by the spec's sections.
// A zero value is not valid; build one through Load, which applies defaults and
// validates.
type Config struct {
	// Backend and connection (section 2).
	Backend string
	DBURI   string

	// Exposed surface (section 3).
	Schemas         []string
	AnonRole        string
	PreRequest      string
	ExtraSearchPath []string
	MaxRows         int // 0 means no cap

	// Auth, a frontend concern identical on every backend (spec 13).
	JWTSecret          string
	JWTAud             string
	JWTRoleClaimKey    string
	JWKSet             string
	JWTCacheMaxEntries int

	// Servers (section 5).
	ServerHost       string
	ServerPort       int
	ServerUnixSocket string
	AdminServerHost  string
	AdminServerPort  int // 0 disables the admin server

	// Pooling and limits (section 7).
	DBPool                   int
	DBPoolAcquisitionTimeout time.Duration

	// OpenAPI (spec 19).
	OpenAPIMode           string
	OpenAPIServerProxyURI string

	// Observability and CORS (section 8).
	LogLevel           string
	LogQuery           bool
	CORSAllowedOrigins []string

	// dbrest-specific declared registries (section 4). Carried as raw text here;
	// each is parsed by the subsystem that consumes it (introspection, RPC,
	// authorization) when that backend lands. They are optional on PostgreSQL and
	// load-bearing on MongoDB and FK-less SQL schemas.
	DeclaredSchema        string
	DeclaredRelationships string
	FunctionRegistry      string
	PolicyRegistry        string
	CapabilityOverrides   string
}

// defaults returns a Config carrying the unambiguous PostgREST defaults, before
// the file and environment are layered on.
func defaults() *Config {
	return &Config{
		Backend:            BackendSQLite,
		Schemas:            []string{""},
		JWTRoleClaimKey:    ".role",
		JWTCacheMaxEntries: 1000,
		ServerHost:         "0.0.0.0",
		ServerPort:         3000,
		DBPool:             10,
		OpenAPIMode:        OpenAPIFollowPrivileges,
		LogLevel:           "error",
	}
}

// Load reads the configuration from the file at path (skipped when path is
// empty) and overlays the environment, then applies defaults and validates.
// environ is the process environment in "KEY=VALUE" form (os.Environ()); both
// the PGRST_* and DBREST_* spellings are read, with DBREST_* winning.
func Load(path string, environ []string) (*Config, error) {
	raw := map[string]string{}
	if path != "" {
		fileRaw, err := parseFile(path)
		if err != nil {
			return nil, err
		}
		maps.Copy(raw, fileRaw)
	}
	overlayEnv(raw, environ)
	return fromRaw(raw)
}

// FromMap builds a Config from an already-merged option map, applying defaults
// and validation. It is the seam tests use to exercise typing and validation
// without a file or a real environment.
func FromMap(raw map[string]string) (*Config, error) {
	return fromRaw(raw)
}

// fromRaw types every option out of the merged raw map, layering it on the
// defaults and collecting validation problems into one error.
func fromRaw(raw map[string]string) (*Config, error) {
	c := defaults()
	var errs []string
	get := func(key string) (string, bool) { v, ok := raw[key]; return v, ok }

	if v, ok := get("db-backend"); ok {
		c.Backend = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := get("db-uri"); ok {
		c.DBURI = v
	}
	if v, ok := get("db-schemas"); ok {
		c.Schemas = splitList(v)
	}
	if v, ok := get("db-anon-role"); ok {
		c.AnonRole = v
	}
	if v, ok := get("db-pre-request"); ok {
		c.PreRequest = v
	}
	if v, ok := get("db-extra-search-path"); ok {
		c.ExtraSearchPath = splitList(v)
	}
	c.MaxRows = pickInt(raw, &errs, c.MaxRows, "db-max-rows", "max-rows")

	if v, ok := get("jwt-secret"); ok {
		c.JWTSecret = v
	}
	if v, ok := get("jwt-aud"); ok {
		c.JWTAud = v
	}
	if v, ok := get("jwt-role-claim-key"); ok {
		c.JWTRoleClaimKey = v
	}
	if v, ok := get("jwk-set"); ok {
		c.JWKSet = v
	}
	c.JWTCacheMaxEntries = pickInt(raw, &errs, c.JWTCacheMaxEntries, "jwt-cache-max-entries")

	if v, ok := get("server-host"); ok {
		c.ServerHost = v
	}
	c.ServerPort = pickInt(raw, &errs, c.ServerPort, "server-port")
	if v, ok := get("server-unix-socket"); ok {
		c.ServerUnixSocket = v
	}
	if v, ok := get("admin-server-host"); ok {
		c.AdminServerHost = v
	}
	c.AdminServerPort = pickInt(raw, &errs, c.AdminServerPort, "admin-server-port")

	c.DBPool = pickInt(raw, &errs, c.DBPool, "db-pool")
	c.DBPoolAcquisitionTimeout = pickDuration(raw, &errs, c.DBPoolAcquisitionTimeout, "db-pool-acquisition-timeout")

	if v, ok := get("openapi-mode"); ok {
		c.OpenAPIMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := get("openapi-server-proxy-uri"); ok {
		c.OpenAPIServerProxyURI = strings.TrimSpace(v)
	}

	if v, ok := get("log-level"); ok {
		c.LogLevel = strings.ToLower(strings.TrimSpace(v))
	}
	c.LogQuery = pickBool(raw, &errs, c.LogQuery, "log-query")
	if v, ok := get("server-cors-allowed-origins"); ok {
		c.CORSAllowedOrigins = splitList(v)
	}

	c.DeclaredSchema = raw["declared-schema"]
	c.DeclaredRelationships = raw["declared-relationships"]
	c.FunctionRegistry = raw["function-registry"]
	c.PolicyRegistry = raw["policy-registry"]
	c.CapabilityOverrides = raw["capability-overrides"]

	c.validate(&errs)
	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// validate appends a message for every option whose value is outside its
// accepted set. It does not check key material (the auth layer does that) or
// reachability (the backend does, at Open).
func (c *Config) validate(errs *[]string) {
	if !knownBackends[c.Backend] {
		*errs = append(*errs, fmt.Sprintf("db-backend %q is not one of postgres/sqlite/mysql/sqlserver/mongodb", c.Backend))
	}
	if strings.TrimSpace(c.DBURI) == "" {
		*errs = append(*errs, "db-uri is required")
	}
	if !knownOpenAPIModes[c.OpenAPIMode] {
		*errs = append(*errs, fmt.Sprintf("openapi-mode %q is not one of follow-privileges/ignore-privileges/disabled", c.OpenAPIMode))
	}
	if !knownLogLevels[c.LogLevel] {
		*errs = append(*errs, fmt.Sprintf("log-level %q is not one of crit/error/warn/info/debug", c.LogLevel))
	}
	if c.ServerPort < 0 || c.ServerPort > 65535 {
		*errs = append(*errs, fmt.Sprintf("server-port %d is out of range", c.ServerPort))
	}
	if c.AdminServerPort < 0 || c.AdminServerPort > 65535 {
		*errs = append(*errs, fmt.Sprintf("admin-server-port %d is out of range", c.AdminServerPort))
	}
	if c.MaxRows < 0 {
		*errs = append(*errs, "db-max-rows must not be negative")
	}
	if c.JWTCacheMaxEntries < 0 {
		*errs = append(*errs, "jwt-cache-max-entries must not be negative")
	}
}

// ServerAddr is the API listen address in host:port form.
func (c *Config) ServerAddr() string {
	return fmt.Sprintf("%s:%d", c.ServerHost, c.ServerPort)
}

// AdminEnabled reports whether the admin server should run.
func (c *Config) AdminEnabled() bool { return c.AdminServerPort != 0 }

// AdminAddr is the admin listen address in host:port form.
func (c *Config) AdminAddr() string {
	return fmt.Sprintf("%s:%d", c.AdminServerHost, c.AdminServerPort)
}
