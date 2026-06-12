package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeConf writes a config file into a temp dir and returns its path.
func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dbrest.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsApplied(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "file:x.db"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != BackendSQLite {
		t.Errorf("backend default = %q, want sqlite", c.Backend)
	}
	if c.ServerPort != 3000 {
		t.Errorf("server-port default = %d, want 3000", c.ServerPort)
	}
	if c.LogLevel != "error" {
		t.Errorf("log-level default = %q, want error", c.LogLevel)
	}
	if c.OpenAPIMode != OpenAPIFollowPrivileges {
		t.Errorf("openapi-mode default = %q", c.OpenAPIMode)
	}
	if c.JWTRoleClaimKey != ".role" {
		t.Errorf("role-claim-key default = %q", c.JWTRoleClaimKey)
	}
	if c.DBPool != 10 {
		t.Errorf("db-pool default = %d, want 10", c.DBPool)
	}
}

func TestOpenAPISecurityActiveParsed(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "file:x.db"})
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenAPISecurityActive {
		t.Error("openapi-security-active should default to false")
	}
	c, err = FromMap(map[string]string{"db-uri": "file:x.db", "openapi-security-active": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.OpenAPISecurityActive {
		t.Error("openapi-security-active = true not parsed")
	}
	if _, err = FromMap(map[string]string{"db-uri": "file:x.db", "openapi-security-active": "banana"}); err == nil {
		t.Error("a non-boolean openapi-security-active should abort boot")
	}
}

func TestDBURIRequired(t *testing.T) {
	_, err := FromMap(map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing db-uri")
	}
}

// TestDBURIDefaultsOnPostgres pins the upstream stock workflow: with the
// postgres backend an unset db-uri becomes "postgresql://", the empty URI the
// driver fills from the PG* environment. Every other engine keeps the hard
// requirement.
func TestDBURIDefaultsOnPostgres(t *testing.T) {
	c, err := FromMap(map[string]string{"db-backend": "postgres"})
	if err != nil {
		t.Fatalf("postgres without db-uri should boot: %v", err)
	}
	if c.DBURI != "postgresql://" {
		t.Errorf("db-uri = %q, want postgresql://", c.DBURI)
	}

	for _, be := range []string{"sqlite", "mysql", "sqlserver", "mongodb"} {
		if _, err := FromMap(map[string]string{"db-backend": be}); err == nil {
			t.Errorf("%s without db-uri should be rejected", be)
		}
	}

	c, err = FromMap(map[string]string{"db-backend": "postgres", "db-uri": "postgresql://u@h/db"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "postgresql://u@h/db" {
		t.Errorf("explicit db-uri lost: %q", c.DBURI)
	}
}

func TestFileParsing(t *testing.T) {
	path := writeConf(t, `
# dbrest configuration
db-uri = "file:demo.db"
db-schemas = "api, api2"
server-port = 8080
log-query = true
db-pool-acquisition-timeout = 10s
jwt-secret = "a-secret-value"   # inline comment
`)
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "file:demo.db" {
		t.Errorf("db-uri = %q", c.DBURI)
	}
	if !slices.Equal(c.Schemas, []string{"api", "api2"}) {
		t.Errorf("schemas = %v", c.Schemas)
	}
	if c.ServerPort != 8080 {
		t.Errorf("server-port = %d", c.ServerPort)
	}
	if !c.LogQuery {
		t.Error("log-query should be true")
	}
	if c.DBPoolAcquisitionTimeout != 10*time.Second {
		t.Errorf("acquisition timeout = %v", c.DBPoolAcquisitionTimeout)
	}
	if c.JWTSecret != "a-secret-value" {
		t.Errorf("jwt-secret = %q (comment not stripped?)", c.JWTSecret)
	}
}

func TestCommentInsideQuoteKept(t *testing.T) {
	path := writeConf(t, `db-uri = "postgres://u:p@h/db?opt=a#b"`)
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "postgres://u:p@h/db?opt=a#b" {
		t.Errorf("db-uri = %q, '#' inside quotes was treated as a comment", c.DBURI)
	}
}

func TestTripleQuotedMultiline(t *testing.T) {
	path := writeConf(t, `db-uri = "file:x.db"
declared-relationships = """
films.director_id -> directors.id
reviews.film_id -> films.id
"""`)
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "\nfilms.director_id -> directors.id\nreviews.film_id -> films.id\n"
	if c.DeclaredRelationships != want {
		t.Errorf("declared-relationships = %q, want %q", c.DeclaredRelationships, want)
	}
}

func TestUnknownFileOptionWarnsAndBoots(t *testing.T) {
	// PostgREST ignores config keys it does not own, so a postgrest.conf
	// carrying someone else's keys must boot. dbrest keeps a warning so the
	// typo is visible.
	path := writeConf(t, "db-uri = \"x\"\ndb-ury = \"typo\"")
	c, err := Load(path, nil)
	if err != nil {
		t.Fatalf("unknown file option must not abort: %v", err)
	}
	if len(c.Warnings) == 0 || !strings.Contains(strings.Join(c.Warnings, "\n"), "db-ury") {
		t.Errorf("expected a warning naming db-ury, got %q", c.Warnings)
	}
}

func TestUnknownEnvKeyWarns(t *testing.T) {
	// The env path matches the file path: an unrecognized PGRST-namespaced
	// variable warns instead of being silently dropped.
	c, err := Load("", []string{"PGRST_DB_URY=typo", "DBREST_DB_URI=file:real.db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) == 0 || !strings.Contains(strings.Join(c.Warnings, "\n"), "PGRST_DB_URY") {
		t.Errorf("expected a warning naming PGRST_DB_URY, got %q", c.Warnings)
	}
}

func TestV14KeySetAccepted(t *testing.T) {
	// Every documented v14 option a real postgrest.conf may carry must parse.
	path := writeConf(t, `
db-uri = "file:demo.db"
app.settings.jwt_lifetime = "3600"
app.settings.name = "demo"
db-aggregates-enabled = true
db-channel = "custom"
db-channel-enabled = false
db-config = false
db-hoisted-tx-settings = "statement_timeout"
db-plan-enabled = true
db-pool-automatic-recovery = false
db-pool-max-idletime = 60
db-pool-max-lifetime = 600
db-pre-config = "postgrest.pre_config"
db-prepared-statements = false
db-root-spec = "root"
db-tx-end = "rollback-allow-override"
jwt-secret-is-base64 = true
openapi-security-active = true
server-trace-header = "X-Request-Id"
server-timing-enabled = true
server-unix-socket-mode = "770"
`)
	c, err := Load(path, nil)
	if err != nil {
		t.Fatalf("v14 key set rejected: %v", err)
	}
	if c.AppSettings["jwt_lifetime"] != "3600" || c.AppSettings["name"] != "demo" {
		t.Errorf("app.settings = %v", c.AppSettings)
	}
	if !c.AggregatesEnabled || !c.PlanEnabled || !c.OpenAPISecurityActive || !c.ServerTimingEnabled || !c.JWTSecretIsBase64 {
		t.Error("boolean options did not parse")
	}
	if c.DBChannel != "custom" || c.DBChannelEnabled || c.DBConfig || c.DBPreparedStatements || c.DBPoolAutomaticRecovery {
		t.Error("channel/config/pool options did not parse")
	}
	if c.DBPoolMaxIdleTime != 60*time.Second || c.DBPoolMaxLifetime != 600*time.Second {
		t.Errorf("pool times = %v/%v", c.DBPoolMaxIdleTime, c.DBPoolMaxLifetime)
	}
	if c.TxEnd != "rollback-allow-override" {
		t.Errorf("db-tx-end = %q", c.TxEnd)
	}
	if !slices.Equal(c.HoistedTxSettings, []string{"statement_timeout"}) {
		t.Errorf("db-hoisted-tx-settings = %v", c.HoistedTxSettings)
	}
	if c.RootSpec != "root" || c.DBPreConfig != "postgrest.pre_config" {
		t.Errorf("root-spec/pre-config = %q/%q", c.RootSpec, c.DBPreConfig)
	}
	if c.ServerTraceHeader != "X-Request-Id" || c.ServerUnixSocketMode != "770" {
		t.Errorf("trace header/socket mode = %q/%q", c.ServerTraceHeader, c.ServerUnixSocketMode)
	}
}

func TestV14Defaults(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DBChannel != "pgrst" || !c.DBChannelEnabled || !c.DBConfig || !c.DBPreparedStatements {
		t.Error("channel/config defaults wrong")
	}
	if c.DBPoolMaxIdleTime != 30*time.Second || c.DBPoolMaxLifetime != 1800*time.Second || !c.DBPoolAutomaticRecovery {
		t.Error("pool defaults wrong")
	}
	if c.DBPoolAcquisitionTimeout != 10*time.Second {
		t.Errorf("db-pool-acquisition-timeout default = %v, want 10s", c.DBPoolAcquisitionTimeout)
	}
	if c.TxEnd != "commit" || c.ServerUnixSocketMode != "660" {
		t.Errorf("tx-end/socket-mode defaults = %q/%q", c.TxEnd, c.ServerUnixSocketMode)
	}
	if c.PlanEnabled || c.AggregatesEnabled || c.ServerTimingEnabled || c.OpenAPISecurityActive || c.JWTSecretIsBase64 {
		t.Error("boolean defaults should be false")
	}
	if !slices.Equal(c.HoistedTxSettings, []string{"statement_timeout", "plan_filter.statement_cost_limit", "default_transaction_isolation"}) {
		t.Errorf("hoisted settings default = %v", c.HoistedTxSettings)
	}
}

func TestV14Aliases(t *testing.T) {
	c, err := FromMap(map[string]string{
		"db-uri": "x", "pre-request": "fn", "root-spec": "rs",
		"db-schema": "api", "role-claim-key": ".r",
		"secret-is-base64": "true", "db-pool-timeout": "55",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.PreRequest != "fn" || c.RootSpec != "rs" || c.JWTRoleClaimKey != ".r" {
		t.Errorf("string aliases = %q/%q/%q", c.PreRequest, c.RootSpec, c.JWTRoleClaimKey)
	}
	if !slices.Equal(c.Schemas, []string{"api"}) {
		t.Errorf("db-schema alias = %v", c.Schemas)
	}
	if !c.JWTSecretIsBase64 || c.DBPoolMaxIdleTime != 55*time.Second {
		t.Error("secret-is-base64 or db-pool-timeout alias did not parse")
	}
}

func TestAppSettingsFromEnv(t *testing.T) {
	c, err := Load("", []string{
		"DBREST_DB_URI=x",
		"PGRST_APP_SETTINGS_JWT_LIFETIME=1800",
		"DBREST_APP_SETTINGS_LOCAL=yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.AppSettings["jwt_lifetime"] != "1800" || c.AppSettings["local"] != "yes" {
		t.Errorf("app settings from env = %v", c.AppSettings)
	}
}

func TestBadTxEndAndSocketMode(t *testing.T) {
	if _, err := FromMap(map[string]string{"db-uri": "x", "db-tx-end": "explode"}); err == nil {
		t.Error("expected error for bad db-tx-end")
	}
	if _, err := FromMap(map[string]string{"db-uri": "x", "server-unix-socket-mode": "555"}); err == nil {
		t.Error("expected error for socket mode below 600")
	}
	if _, err := FromMap(map[string]string{"db-uri": "x", "server-unix-socket-mode": "9x"}); err == nil {
		t.Error("expected error for non-octal socket mode")
	}
}

func TestUnenforcedOptionWarns(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "x", "db-tx-end": "rollback"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "db-tx-end") {
		t.Errorf("expected an unenforced warning for db-tx-end, got %q", c.Warnings)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeConf(t, `db-uri = "file:from-file.db"`)
	c, err := Load(path, []string{"PGRST_DB_URI=file:from-env.db"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "file:from-env.db" {
		t.Errorf("db-uri = %q, env did not override file", c.DBURI)
	}
}

func TestDBRESTWinsOverPGRST(t *testing.T) {
	c, err := Load("", []string{
		"PGRST_DB_URI=file:pgrst.db",
		"DBREST_DB_URI=file:dbrest.db",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "file:dbrest.db" {
		t.Errorf("db-uri = %q, DBREST_ should win over PGRST_", c.DBURI)
	}
}

func TestMaxRowsAlias(t *testing.T) {
	c, err := Load("", []string{"DBREST_MAX_ROWS=50", "DBREST_DB_URI=x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRows != 50 {
		t.Errorf("max-rows alias = %d, want 50", c.MaxRows)
	}
}

// TestNativeKeysScopedToDBREST pins the namespace split: a dbrest extension
// does not bind from the PGRST_ environment prefix (a future PostgREST
// release adding the same name must not change dbrest behavior), it warns
// there instead, and the DBREST_ spelling keeps working.
func TestNativeKeysScopedToDBREST(t *testing.T) {
	c, err := Load("", []string{"DBREST_DB_URI=x", "PGRST_DB_BACKEND=postgres", "PGRST_MAX_ROWS=5"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != BackendSQLite {
		t.Errorf("backend = %q, PGRST_DB_BACKEND must not bind", c.Backend)
	}
	if c.MaxRows != 0 {
		t.Errorf("max-rows = %d, PGRST_MAX_ROWS must not bind", c.MaxRows)
	}
	joined := strings.Join(c.Warnings, "\n")
	for _, name := range []string{"PGRST_DB_BACKEND", "PGRST_MAX_ROWS"} {
		if !strings.Contains(joined, name) {
			t.Errorf("expected a warning naming %s, got %q", name, c.Warnings)
		}
	}

	c, err = Load("", []string{"DBREST_DB_URI=x", "DBREST_DB_BACKEND=postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != BackendPostgres {
		t.Errorf("backend = %q, DBREST_DB_BACKEND should bind", c.Backend)
	}
}

// TestNativeKeysFilePrefix covers the explicit dbrest. file spelling: it maps
// onto the bare extension key, and a non-extension name under the prefix gets
// the unknown-option warning.
func TestNativeKeysFilePrefix(t *testing.T) {
	path := writeConf(t, `
db-uri = "x"
dbrest.max-rows = 25
dbrest.function-registry = "fns.json"
dbrest.server-port = 9999
`)
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRows != 25 || c.FunctionRegistry != "fns.json" {
		t.Errorf("dbrest. prefixed keys did not bind: max-rows=%d registry=%q", c.MaxRows, c.FunctionRegistry)
	}
	if c.ServerPort != 3000 {
		t.Errorf("server-port = %d, dbrest.server-port is not an extension and must not bind", c.ServerPort)
	}
	if !strings.Contains(strings.Join(c.Warnings, "\n"), "dbrest.server-port") {
		t.Errorf("expected an unknown-option warning for dbrest.server-port, got %q", c.Warnings)
	}
}

// TestNativeKeysAreKnown guards the extension list: every native key must be
// a real option, so a rename cannot silently orphan the scoping.
func TestNativeKeysAreKnown(t *testing.T) {
	known := map[string]bool{}
	for _, k := range optionKeys {
		known[k] = true
	}
	for _, k := range nativeOptionKeys {
		if !known[k] {
			t.Errorf("native key %q is not in optionKeys", k)
		}
	}
}

func TestUnknownEnvKeyIgnored(t *testing.T) {
	// A typo in the variable name is not a known option, so it must not leak in.
	c, err := Load("", []string{"PGRST_DB_URY=typo", "DBREST_DB_URI=file:real.db"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "file:real.db" {
		t.Errorf("db-uri = %q", c.DBURI)
	}
}

func TestValidationRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown backend":   {"db-uri": "x", "db-backend": "oracle"},
		"bad openapi-mode":  {"db-uri": "x", "openapi-mode": "sometimes"},
		"bad log-level":     {"db-uri": "x", "log-level": "loud"},
		"port out of range": {"db-uri": "x", "server-port": "70000"},
		"negative max-rows": {"db-uri": "x", "db-max-rows": "-1"},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromMap(raw); err == nil {
				t.Errorf("%s: expected validation error", name)
			}
		})
	}
}

func TestMalformedIntIsError(t *testing.T) {
	if _, err := FromMap(map[string]string{"db-uri": "x", "server-port": "abc"}); err == nil {
		t.Fatal("expected error for non-integer server-port")
	}
}

func TestServerAndAdminAddr(t *testing.T) {
	c, err := FromMap(map[string]string{
		"db-uri": "x", "server-host": "127.0.0.1", "server-port": "3001",
		"admin-server-host": "127.0.0.1", "admin-server-port": "3002",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ServerAddr(); got != "127.0.0.1:3001" {
		t.Errorf("ServerAddr = %q", got)
	}
	if !c.AdminEnabled() {
		t.Error("admin should be enabled when admin-server-port is set")
	}
	if got := c.AdminAddr(); got != "127.0.0.1:3002" {
		t.Errorf("AdminAddr = %q", got)
	}
}

func TestAdminDisabledByDefault(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminEnabled() {
		t.Error("admin should be disabled by default")
	}
}

func TestMissingFileIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.conf"), nil); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func BenchmarkLoad(b *testing.B) {
	env := []string{"DBREST_DB_URI=file:bench.db", "DBREST_SERVER_PORT=3000"}
	for b.Loop() {
		if _, err := Load("", env); err != nil {
			b.Fatal(err)
		}
	}
}

// TestAllAnonymousPostureWarns covers the startup validation gap: a config
// with neither db-anon-role nor JWT key material boots, but says what that
// means. Configuring either side silences the warning.
func TestAllAnonymousPostureWarns(t *testing.T) {
	hasAnonWarning := func(c *Config) bool {
		return strings.Contains(strings.Join(c.Warnings, "\n"), "anonymously with no role")
	}

	c, err := FromMap(map[string]string{"db-uri": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasAnonWarning(c) {
		t.Errorf("expected the all-anonymous warning, got %q", c.Warnings)
	}

	silenced := []map[string]string{
		{"db-uri": "x", "db-anon-role": "web_anon"},
		{"db-uri": "x", "jwt-secret": "reallyreallyreallyreallyverysafe"},
		{"db-uri": "x", "jwk-set": `{"keys":[]}`},
	}
	for _, raw := range silenced {
		c, err := FromMap(raw)
		if err != nil {
			t.Fatal(err)
		}
		if hasAnonWarning(c) {
			t.Errorf("warning should be silent for %v, got %q", raw, c.Warnings)
		}
	}
}

// TestAdminPortCannotEqualServerPort mirrors the upstream boot failure: the
// admin server cannot share the API port.
func TestAdminPortCannotEqualServerPort(t *testing.T) {
	if _, err := FromMap(map[string]string{"db-uri": "x", "server-port": "3000", "admin-server-port": "3000"}); err == nil {
		t.Error("expected error for admin-server-port == server-port")
	}
	if _, err := FromMap(map[string]string{"db-uri": "x", "server-port": "3000", "admin-server-port": "3001"}); err != nil {
		t.Errorf("distinct ports should boot: %v", err)
	}
}

// TestAdminHostDefaultsToServerHost checks the upstream default: an unset
// admin-server-host follows server-host.
func TestAdminHostDefaultsToServerHost(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "x", "server-host": "127.0.0.5", "admin-server-port": "3001"})
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminServerHost != "127.0.0.5" {
		t.Errorf("admin-server-host = %q, want the server-host 127.0.0.5", c.AdminServerHost)
	}
	c, err = FromMap(map[string]string{"db-uri": "x", "admin-server-host": "10.0.0.1", "admin-server-port": "3001"})
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminServerHost != "10.0.0.1" {
		t.Errorf("admin-server-host = %q, explicit value lost", c.AdminServerHost)
	}
}

// TestMergeReloadable checks the SIGUSR2 merge: runtime options follow the new
// config, boot-time options stay put and are reported.
func TestMergeReloadable(t *testing.T) {
	old, err := FromMap(map[string]string{"db-uri": "file:a.db", "db-max-rows": "100", "server-port": "3000"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := FromMap(map[string]string{"db-uri": "file:b.db", "db-max-rows": "50", "server-port": "4000", "db-anon-role": "web_anon"})
	if err != nil {
		t.Fatal(err)
	}
	merged, kept := old.MergeReloadable(next)
	if merged.MaxRows != 50 || merged.AnonRole != "web_anon" {
		t.Errorf("reloadable fields not applied: max-rows=%d anon=%q", merged.MaxRows, merged.AnonRole)
	}
	if merged.DBURI != "file:a.db" || merged.ServerPort != 3000 {
		t.Errorf("boot-time fields changed: db-uri=%q port=%d", merged.DBURI, merged.ServerPort)
	}
	joined := strings.Join(kept, "\n")
	for _, name := range []string{"db-uri", "server-port"} {
		if !strings.Contains(joined, name) {
			t.Errorf("expected a kept-value message for %s, got %q", name, kept)
		}
	}
	if strings.Contains(joined, "db-max-rows") {
		t.Errorf("db-max-rows is reloadable, should not be reported: %q", kept)
	}
}

// TestDumpRoundTrips pins the --dump-config format: the output is valid
// config-file syntax and loads back to the same resolved values.
func TestDumpRoundTrips(t *testing.T) {
	first, err := FromMap(map[string]string{
		"db-uri":              "file:dump.db",
		"db-schemas":          "public,api",
		"db-anon-role":        "web_anon",
		"db-max-rows":         "500",
		"db-tx-end":           "rollback",
		"app.settings.tenant": "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := writeConf(t, first.Dump())
	second, err := Load(path, nil)
	if err != nil {
		t.Fatalf("dump output does not load: %v", err)
	}
	if second.DBURI != first.DBURI || second.AnonRole != first.AnonRole ||
		second.MaxRows != first.MaxRows || second.TxEnd != first.TxEnd {
		t.Errorf("round trip drifted: %+v vs %+v", second, first)
	}
	if len(second.Schemas) != 2 || second.Schemas[0] != "public" {
		t.Errorf("schemas drifted: %v", second.Schemas)
	}
	if second.AppSettings["tenant"] != "acme" {
		t.Errorf("app settings drifted: %v", second.AppSettings)
	}
	if second.Dump() != first.Dump() {
		t.Error("Dump is not a fixed point of Load(Dump)")
	}
}

// TestEnvInterpolation covers $(NAME) in file string values: an environment
// variable, an earlier config key, the $$ escape, and the hard error on an
// unset name, all upstream configurator behavior.
func TestEnvInterpolation(t *testing.T) {
	t.Setenv("DBREST_TEST_SECRET", "from-env")
	path := writeConf(t, `
db-uri = "file:interp.db"
db-anon-role = "web_anon"
jwt-secret = "$(DBREST_TEST_SECRET)"
db-pre-request = "check_$(db-anon-role)"
app.settings.cost = "5$$ per row"
`)
	c, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.JWTSecret != "from-env" {
		t.Errorf("jwt-secret = %q, want the env value", c.JWTSecret)
	}
	if c.PreRequest != "check_web_anon" {
		t.Errorf("pre-request = %q, earlier config key did not resolve", c.PreRequest)
	}
	if c.AppSettings["cost"] != "5$ per row" {
		t.Errorf("$$ escape: got %q", c.AppSettings["cost"])
	}

	bad := writeConf(t, `jwt-secret = "$(DBREST_TEST_UNSET_VAR)"`)
	if _, err := Load(bad, nil); err == nil || !strings.Contains(err.Error(), "no such variable") {
		t.Errorf("unset variable should be a hard error, got %v", err)
	}
}

// TestEnvValuesAreNotInterpolated pins the asymmetry: only file values
// expand; an env-sourced value keeps its dollars verbatim.
func TestEnvValuesAreNotInterpolated(t *testing.T) {
	c, err := Load("", []string{"PGRST_DB_URI=x", "PGRST_JWT_SECRET=pa$(ss)word"})
	if err != nil {
		t.Fatal(err)
	}
	if c.JWTSecret != "pa$(ss)word" {
		t.Errorf("jwt-secret = %q, env values must stay literal", c.JWTSecret)
	}
}

// TestAtFileReferences covers the @path form for the two options that support
// it: jwt-secret (one trailing newline chomped) and db-uri (whitespace
// trimmed), plus the error on a missing file.
func TestAtFileReferences(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte("hush hush hush hush hush hush 32\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uriPath := filepath.Join(dir, "uri")
	if err := os.WriteFile(uriPath, []byte("  file:from-file.db \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := FromMap(map[string]string{
		"db-uri":     "@" + uriPath,
		"jwt-secret": "@" + secretPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.DBURI != "file:from-file.db" {
		t.Errorf("db-uri = %q, want the trimmed file contents", c.DBURI)
	}
	if c.JWTSecret != "hush hush hush hush hush hush 32" {
		t.Errorf("jwt-secret = %q, want the file contents with one newline chomped", c.JWTSecret)
	}

	if _, err := FromMap(map[string]string{"db-uri": "@" + filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing @file should be an error")
	}
}

// TestListenSpecs pins the special host values to their candidate lists, and
// the default host to upstream's !4.
func TestListenSpecs(t *testing.T) {
	c, err := FromMap(map[string]string{"db-uri": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerHost != "!4" {
		t.Errorf("default server-host = %q, want !4", c.ServerHost)
	}

	cases := []struct {
		host string
		want []ListenSpec
	}{
		{"*", []ListenSpec{{"tcp", ":3000"}}},
		{"*4", []ListenSpec{{"tcp4", "0.0.0.0:3000"}, {"tcp6", "[::]:3000"}}},
		{"!4", []ListenSpec{{"tcp4", "0.0.0.0:3000"}}},
		{"*6", []ListenSpec{{"tcp6", "[::]:3000"}, {"tcp4", "0.0.0.0:3000"}}},
		{"!6", []ListenSpec{{"tcp6", "[::]:3000"}}},
		{"127.0.0.1", []ListenSpec{{"tcp", "127.0.0.1:3000"}}},
		{"::1", []ListenSpec{{"tcp", "[::1]:3000"}}},
	}
	for _, tc := range cases {
		c.ServerHost = tc.host
		got := c.Listeners()
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.host, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s[%d]: %v, want %v", tc.host, i, got[i], tc.want[i])
			}
		}
	}
}

// TestUnixSocketReplacesTCP pins the listener selection: with
// server-unix-socket set the only candidate is the socket, and the admin
// listeners stay TCP.
func TestUnixSocketReplacesTCP(t *testing.T) {
	c, err := FromMap(map[string]string{
		"db-uri": "x", "server-unix-socket": "/tmp/dbrest.sock", "admin-server-port": "3001",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := c.Listeners()
	if len(got) != 1 || got[0] != (ListenSpec{"unix", "/tmp/dbrest.sock"}) {
		t.Errorf("Listeners = %v, want the unix socket only", got)
	}
	for _, spec := range c.AdminListeners() {
		if spec.Network == "unix" {
			t.Errorf("admin listener went to the socket: %v", spec)
		}
	}
}

// TestSchemasDefaultFollowsBackend pins the engine-aware db-schemas default:
// public on postgres, main on sqlite, the backend's own default elsewhere,
// with an explicit value always winning and an explicitly empty one rejected.
func TestSchemasDefaultFollowsBackend(t *testing.T) {
	cases := []struct {
		backend string
		want    string
	}{
		{"postgres", "public"},
		{"sqlite", ""},
		{"mysql", ""},
	}
	for _, tc := range cases {
		c, err := FromMap(map[string]string{"db-uri": "x", "db-backend": tc.backend})
		if err != nil {
			t.Fatalf("%s: %v", tc.backend, err)
		}
		if len(c.Schemas) != 1 || c.Schemas[0] != tc.want {
			t.Errorf("%s: schemas = %v, want [%q]", tc.backend, c.Schemas, tc.want)
		}
	}

	c, err := FromMap(map[string]string{"db-uri": "x", "db-backend": "postgres", "db-schemas": "api,private"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schemas) != 2 || c.Schemas[0] != "api" {
		t.Errorf("explicit schemas lost: %v", c.Schemas)
	}

	if _, err := FromMap(map[string]string{"db-uri": "x", "db-schemas": ""}); err == nil {
		t.Error("explicitly empty db-schemas should be rejected")
	}
}
