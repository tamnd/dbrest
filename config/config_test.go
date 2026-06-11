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

func TestDBURIRequired(t *testing.T) {
	_, err := FromMap(map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing db-uri")
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
	if c.DBPoolMaxIdleTime != 60 || c.DBPoolMaxLifetime != 600 {
		t.Errorf("pool times = %d/%d", c.DBPoolMaxIdleTime, c.DBPoolMaxLifetime)
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
	if c.DBPoolMaxIdleTime != 30 || c.DBPoolMaxLifetime != 1800 || !c.DBPoolAutomaticRecovery {
		t.Error("pool defaults wrong")
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
	if !c.JWTSecretIsBase64 || c.DBPoolMaxIdleTime != 55 {
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
