package config

import (
	"os"
	"path/filepath"
	"slices"
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

func TestUnknownOptionIsError(t *testing.T) {
	path := writeConf(t, "db-uri = \"x\"\ndb-ury = \"typo\"")
	if _, err := Load(path, nil); err == nil {
		t.Fatal("expected error for unknown option")
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
