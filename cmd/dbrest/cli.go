// The PostgREST-shaped command line: the config file is a positional argument
// (`dbrest /etc/dbrest.conf`), and the maintenance verbs --version, --example,
// --dump-config, --dump-schema, and --ready mirror upstream's.
package main

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/tamnd/dbrest/config"
)

// resolveConfigPath reconciles the -config flag with the positional argument.
// Either spelling works; giving both with different paths is an error rather
// than a silent pick.
func resolveConfigPath(flagPath string, args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("expected at most one config file argument, got %d", len(args))
	}
	if len(args) == 0 {
		return flagPath, nil
	}
	if flagPath != "" && flagPath != args[0] {
		return "", fmt.Errorf("config file given twice: -config %s and argument %s", flagPath, args[0])
	}
	return args[0], nil
}

// versionString is the module version when built with one, "dev" otherwise.
func versionString() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// probeReady asks a running instance's admin server whether it is ready, the
// --ready verb orchestrators use as a health command. A non-200 answer or an
// unreachable admin server is an error, which main turns into exit status 1.
func probeReady(cfg *config.Config) error {
	if !cfg.AdminEnabled() {
		return fmt.Errorf("--ready needs admin-server-port to be configured")
	}
	url := "http://" + probeAddr(cfg.AdminServerHost, cfg.AdminServerPort) + "/ready"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("ready probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ready probe: %s answered %d", url, resp.StatusCode)
	}
	return nil
}

// exampleConfig is the --example output: a commented config covering the
// options most deployments touch, in the file syntax Load reads.
const exampleConfig = `## dbrest example configuration
## Every option also reads from the environment as PGRST_<UPPER_SNAKE> or
## DBREST_<UPPER_SNAKE>, with the DBREST_ spelling winning.

## The engine behind the API: postgres, sqlite, mysql, sqlserver, or mongodb.
db-backend = "sqlite"

## The connection string, in the engine's own syntax.
db-uri = "file:dbrest.db"

## The database schemas to expose, comma-separated. The first is the default.
# db-schemas = "public"

## The role used for requests that carry no JWT.
# db-anon-role = "web_anon"

## Hard cap on the rows a read or RPC response may return. Unset means no cap.
# db-max-rows = 1000

## How the request transaction ends: commit (default), commit-allow-override,
## rollback, or rollback-allow-override.
# db-tx-end = "commit"

## Secret for validating JWTs (HS256). Longer than 32 characters.
# jwt-secret = "reallyreallyreallyreallyverysafe"

## Where the API listens.
# server-host = "0.0.0.0"
# server-port = 3000

## The admin server with /live, /ready, /schema_cache, and /metrics.
## Disabled until a port is set; it must differ from server-port.
# admin-server-port = 3001

## Connection pool sizing.
# db-pool = 10

## OpenAPI output: follow-privileges (default), ignore-privileges, disabled.
# openapi-mode = "follow-privileges"

## Logging: crit, error (default), warn, info, or debug.
# log-level = "error"

## CORS. Unset serves the permissive default; a comma-separated list
## restricts the allowed origins.
# server-cors-allowed-origins = "https://example.com"

## Settings forwarded to the backend as transaction settings.
# app.settings.tenant = "acme"
`
