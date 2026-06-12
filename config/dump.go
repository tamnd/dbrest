package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// seconds renders a pool timeout the way upstream's config does: a bare
// integer of seconds, falling back to the duration extension form below one
// second so the value round-trips.
func seconds(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d / time.Second))
	}
	return strconv.Quote(d.String())
}

// Dump renders the resolved configuration in the config-file syntax, the
// answer to --dump-config: every option with its effective value, defaults
// included, sorted by key. The output parses back to the same configuration,
// which is also how the tests pin it.
func (c *Config) Dump() string {
	q := strconv.Quote
	pairs := map[string]string{
		"db-backend":                  q(c.Backend),
		"db-uri":                      q(c.DBURI),
		"db-schemas":                  q(strings.Join(c.Schemas, ",")),
		"db-anon-role":                q(c.AnonRole),
		"db-pre-request":              q(c.PreRequest),
		"db-extra-search-path":        q(strings.Join(c.ExtraSearchPath, ",")),
		"db-max-rows":                 strconv.Itoa(c.MaxRows),
		"db-aggregates-enabled":       strconv.FormatBool(c.AggregatesEnabled),
		"db-root-spec":                q(c.RootSpec),
		"db-tx-end":                   q(c.TxEnd),
		"db-hoisted-tx-settings":      q(strings.Join(c.HoistedTxSettings, ",")),
		"db-plan-enabled":             strconv.FormatBool(c.PlanEnabled),
		"db-channel":                  q(c.DBChannel),
		"db-channel-enabled":          strconv.FormatBool(c.DBChannelEnabled),
		"db-config":                   strconv.FormatBool(c.DBConfig),
		"db-pre-config":               q(c.DBPreConfig),
		"db-prepared-statements":      strconv.FormatBool(c.DBPreparedStatements),
		"db-pool":                     strconv.Itoa(c.DBPool),
		"db-pool-acquisition-timeout": seconds(c.DBPoolAcquisitionTimeout),
		"db-pool-max-idletime":        seconds(c.DBPoolMaxIdleTime),
		"db-pool-max-lifetime":        seconds(c.DBPoolMaxLifetime),
		"db-pool-automatic-recovery":  strconv.FormatBool(c.DBPoolAutomaticRecovery),
		"jwt-secret":                  q(c.JWTSecret),
		"jwt-secret-is-base64":        strconv.FormatBool(c.JWTSecretIsBase64),
		"jwt-aud":                     q(c.JWTAud),
		"jwt-role-claim-key":          q(c.JWTRoleClaimKey),
		"jwk-set":                     q(c.JWKSet),
		"jwt-cache-max-entries":       strconv.Itoa(c.JWTCacheMaxEntries),
		"server-host":                 q(c.ServerHost),
		"server-port":                 strconv.Itoa(c.ServerPort),
		"server-unix-socket":          q(c.ServerUnixSocket),
		"server-unix-socket-mode":     q(c.ServerUnixSocketMode),
		"admin-server-host":           q(c.AdminServerHost),
		"admin-server-port":           strconv.Itoa(c.AdminServerPort),
		"openapi-mode":                q(c.OpenAPIMode),
		"openapi-security-active":     strconv.FormatBool(c.OpenAPISecurityActive),
		"openapi-server-proxy-uri":    q(c.OpenAPIServerProxyURI),
		"log-level":                   q(c.LogLevel),
		"log-query":                   strconv.FormatBool(c.LogQuery),
		"server-cors-allowed-origins": q(strings.Join(c.CORSAllowedOrigins, ",")),
		"server-trace-header":         q(c.ServerTraceHeader),
		"server-timing-enabled":       strconv.FormatBool(c.ServerTimingEnabled),
		"declared-schema":             q(c.DeclaredSchema),
		"declared-relationships":      q(c.DeclaredRelationships),
		"function-registry":           q(c.FunctionRegistry),
		"policy-registry":             q(c.PolicyRegistry),
		"capability-overrides":        q(c.CapabilityOverrides),
	}
	for name, v := range c.AppSettings {
		pairs["app.settings."+name] = q(v)
	}

	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, pairs[k])
	}
	return b.String()
}
