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
	"strconv"
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

// Transaction termination modes (db-tx-end).
var knownTxEnds = map[string]bool{
	"commit": true, "commit-allow-override": true,
	"rollback": true, "rollback-allow-override": true,
}

// Config is the resolved option set. Fields are grouped by the spec's sections.
// A zero value is not valid; build one through Load, which applies defaults and
// validates.
type Config struct {
	// Backend and connection (section 2).
	Backend string
	DBURI   string

	// Exposed surface (section 3).
	Schemas           []string
	AnonRole          string
	PreRequest        string
	ExtraSearchPath   []string
	MaxRows           int // 0 means no cap
	AggregatesEnabled bool
	RootSpec          string

	// Transaction behavior.
	TxEnd             string // commit / commit-allow-override / rollback / rollback-allow-override
	HoistedTxSettings []string

	// Application settings forwarded to the backend as transaction settings
	// (the app.settings.* namespace). Keys are stored without the prefix.
	AppSettings map[string]string

	// Auth, a frontend concern identical on every backend (spec 13).
	JWTSecret          string
	JWTSecretIsBase64  bool
	JWTAud             string
	JWTRoleClaimKey    string
	JWKSet             string
	JWTCacheMaxEntries int

	// Servers (section 5).
	ServerHost           string
	ServerPort           int
	ServerUnixSocket     string
	ServerUnixSocketMode string
	AdminServerHost      string
	AdminServerPort      int // 0 disables the admin server

	// Pooling and limits (section 7).
	DBPool                   int
	DBPoolAcquisitionTimeout time.Duration
	DBPoolMaxIdleTime        int // seconds
	DBPoolMaxLifetime        int // seconds
	DBPoolAutomaticRecovery  bool

	// Reload and in-database configuration.
	DBChannel            string
	DBChannelEnabled     bool
	DBConfig             bool
	DBPreConfig          string
	DBPreparedStatements bool

	// OpenAPI (spec 19).
	OpenAPIMode           string
	OpenAPIServerProxyURI string
	OpenAPISecurityActive bool

	// Observability and CORS (section 8).
	LogLevel            string
	LogQuery            bool
	CORSAllowedOrigins  []string
	PlanEnabled         bool
	ServerTraceHeader   string
	ServerTimingEnabled bool

	// Warnings collected while loading: accepted-but-unenforced options,
	// unknown keys, and risky postures. The command logs them at startup;
	// none of them is fatal.
	Warnings []string

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
		TxEnd:              "commit",
		HoistedTxSettings: []string{
			"statement_timeout", "plan_filter.statement_cost_limit",
			"default_transaction_isolation",
		},
		DBChannel:               "pgrst",
		DBChannelEnabled:        true,
		DBConfig:                true,
		DBPreparedStatements:    true,
		DBPoolMaxIdleTime:       30,
		DBPoolMaxLifetime:       1800,
		DBPoolAutomaticRecovery: true,
		ServerUnixSocketMode:    "660",
	}
}

// Load reads the configuration from the file at path (skipped when path is
// empty) and overlays the environment, then applies defaults and validates.
// environ is the process environment in "KEY=VALUE" form (os.Environ()); both
// the PGRST_* and DBREST_* spellings are read, with DBREST_* winning.
func Load(path string, environ []string) (*Config, error) {
	raw := map[string]string{}
	var warnings []string
	if path != "" {
		fileRaw, fileWarnings, err := parseFile(path)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, fileWarnings...)
		maps.Copy(raw, fileRaw)
	}
	warnings = append(warnings, overlayEnv(raw, environ)...)
	c, err := fromRaw(raw)
	if err != nil {
		return nil, err
	}
	c.Warnings = append(warnings, c.Warnings...)
	return c, nil
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
	for _, key := range []string{"db-schemas", "db-schema"} {
		if v, ok := get(key); ok {
			c.Schemas = splitList(v)
			break
		}
	}
	if v, ok := get("db-anon-role"); ok {
		c.AnonRole = v
	}
	c.PreRequest = pickString(raw, c.PreRequest, "db-pre-request", "pre-request")
	if v, ok := get("db-extra-search-path"); ok {
		c.ExtraSearchPath = splitList(v)
	}
	c.MaxRows = pickInt(raw, &errs, c.MaxRows, "db-max-rows", "max-rows")
	c.AggregatesEnabled = pickBool(raw, &errs, c.AggregatesEnabled, "db-aggregates-enabled")
	c.RootSpec = pickString(raw, c.RootSpec, "db-root-spec", "root-spec")

	if v, ok := get("db-tx-end"); ok {
		c.TxEnd = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := get("db-hoisted-tx-settings"); ok {
		c.HoistedTxSettings = splitList(v)
	}
	for key, v := range raw {
		if name, ok := strings.CutPrefix(key, "app.settings."); ok && name != "" {
			if c.AppSettings == nil {
				c.AppSettings = map[string]string{}
			}
			c.AppSettings[name] = v
		}
	}

	if v, ok := get("jwt-secret"); ok {
		c.JWTSecret = v
	}
	c.JWTSecretIsBase64 = pickBool(raw, &errs, c.JWTSecretIsBase64, "jwt-secret-is-base64", "secret-is-base64")
	if v, ok := get("jwt-aud"); ok {
		c.JWTAud = v
	}
	c.JWTRoleClaimKey = pickString(raw, c.JWTRoleClaimKey, "jwt-role-claim-key", "role-claim-key")
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
	if v, ok := get("server-unix-socket-mode"); ok {
		c.ServerUnixSocketMode = strings.TrimSpace(v)
	}
	if v, ok := get("admin-server-host"); ok {
		c.AdminServerHost = v
	}
	c.AdminServerPort = pickInt(raw, &errs, c.AdminServerPort, "admin-server-port")
	if c.AdminServerHost == "" {
		// Upstream defaults the admin host to the API host.
		c.AdminServerHost = c.ServerHost
	}

	c.DBPool = pickInt(raw, &errs, c.DBPool, "db-pool")
	c.DBPoolAcquisitionTimeout = pickDuration(raw, &errs, c.DBPoolAcquisitionTimeout, "db-pool-acquisition-timeout")
	c.DBPoolMaxIdleTime = pickInt(raw, &errs, c.DBPoolMaxIdleTime, "db-pool-max-idletime", "db-pool-timeout")
	c.DBPoolMaxLifetime = pickInt(raw, &errs, c.DBPoolMaxLifetime, "db-pool-max-lifetime")
	c.DBPoolAutomaticRecovery = pickBool(raw, &errs, c.DBPoolAutomaticRecovery, "db-pool-automatic-recovery")

	if v, ok := get("db-channel"); ok {
		c.DBChannel = v
	}
	c.DBChannelEnabled = pickBool(raw, &errs, c.DBChannelEnabled, "db-channel-enabled")
	c.DBConfig = pickBool(raw, &errs, c.DBConfig, "db-config")
	if v, ok := get("db-pre-config"); ok {
		c.DBPreConfig = v
	}
	c.DBPreparedStatements = pickBool(raw, &errs, c.DBPreparedStatements, "db-prepared-statements")

	if v, ok := get("openapi-mode"); ok {
		c.OpenAPIMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := get("openapi-server-proxy-uri"); ok {
		c.OpenAPIServerProxyURI = strings.TrimSpace(v)
	}
	c.OpenAPISecurityActive = pickBool(raw, &errs, c.OpenAPISecurityActive, "openapi-security-active")

	if v, ok := get("log-level"); ok {
		c.LogLevel = strings.ToLower(strings.TrimSpace(v))
	}
	c.LogQuery = pickBool(raw, &errs, c.LogQuery, "log-query")
	if v, ok := get("server-cors-allowed-origins"); ok {
		c.CORSAllowedOrigins = splitList(v)
	}
	c.PlanEnabled = pickBool(raw, &errs, c.PlanEnabled, "db-plan-enabled")
	if v, ok := get("server-trace-header"); ok {
		c.ServerTraceHeader = v
	}
	c.ServerTimingEnabled = pickBool(raw, &errs, c.ServerTimingEnabled, "server-timing-enabled")

	c.Warnings = append(c.Warnings, unenforcedWarnings(raw)...)

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
	if c.AdminServerPort != 0 && c.AdminServerPort == c.ServerPort {
		*errs = append(*errs, "admin-server-port cannot be the same as server-port")
	}
	if c.MaxRows < 0 {
		*errs = append(*errs, "db-max-rows must not be negative")
	}
	if c.JWTCacheMaxEntries < 0 {
		*errs = append(*errs, "jwt-cache-max-entries must not be negative")
	}
	if !knownTxEnds[c.TxEnd] {
		*errs = append(*errs, fmt.Sprintf("db-tx-end %q is not one of commit/commit-allow-override/rollback/rollback-allow-override", c.TxEnd))
	}
	if mode, err := strconv.ParseUint(c.ServerUnixSocketMode, 8, 32); err != nil {
		*errs = append(*errs, fmt.Sprintf("server-unix-socket-mode %q is not an octal", c.ServerUnixSocketMode))
	} else if mode < 0o600 || mode > 0o777 {
		*errs = append(*errs, fmt.Sprintf("server-unix-socket-mode %q needs to be between 600 and 777", c.ServerUnixSocketMode))
	}
}

// unenforcedOptions are options dbrest parses for PostgREST compatibility but
// whose behavior has not landed yet. Setting one is accepted with a warning so
// a working postgrest.conf boots, but the operator is told the knob does not
// turn anything yet. An entry leaves this list when its subsystem ships.
var unenforcedOptions = []string{
	"db-aggregates-enabled", "db-channel", "db-channel-enabled", "db-config",
	"db-hoisted-tx-settings", "db-pool-automatic-recovery",
	"db-pool-max-idletime", "db-pool-max-lifetime", "db-pre-config",
	"db-prepared-statements", "db-root-spec", "root-spec", "db-tx-end",
	"jwt-secret-is-base64", "secret-is-base64", "openapi-security-active",
	"server-trace-header", "server-timing-enabled",
}

// unenforcedWarnings returns one warning per explicitly set option that parses
// but is not yet enforced.
func unenforcedWarnings(raw map[string]string) []string {
	var out []string
	for _, key := range unenforcedOptions {
		if _, ok := raw[key]; ok {
			out = append(out, fmt.Sprintf("option %s is accepted but not enforced yet", key))
		}
	}
	return out
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
