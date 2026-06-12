package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// optionKeys is every configuration key dbrest reads. The file uses these names
// verbatim; the environment uses the upper-snake form under the PGRST_ and
// DBREST_ prefixes. Keeping the set explicit means an environment variable is
// only consulted for a key we actually understand, so a typo in PGRST_DB_URY is
// ignored rather than silently dropped into a catch-all map.
var optionKeys = []string{
	"db-backend", "db-uri", "db-schemas", "db-schema", "db-anon-role",
	"db-pre-request", "pre-request",
	"db-extra-search-path", "db-max-rows", "max-rows",
	"db-aggregates-enabled", "db-root-spec", "root-spec",
	"db-tx-end", "db-hoisted-tx-settings",
	"db-channel", "db-channel-enabled", "db-config", "db-pre-config",
	"db-prepared-statements", "db-plan-enabled",
	"jwt-secret", "jwt-secret-is-base64", "secret-is-base64", "jwt-aud",
	"jwt-role-claim-key", "role-claim-key", "jwk-set", "jwt-cache-max-entries",
	"server-host", "server-port", "server-unix-socket", "server-unix-socket-mode",
	"admin-server-host", "admin-server-port",
	"db-pool", "db-pool-acquisition-timeout",
	"db-pool-max-idletime", "db-pool-timeout", "db-pool-max-lifetime",
	"db-pool-automatic-recovery",
	"openapi-mode", "openapi-server-proxy-uri", "openapi-security-active",
	"log-level", "log-query", "server-cors-allowed-origins",
	"server-trace-header", "server-timing-enabled",
	"declared-schema", "declared-relationships", "function-registry",
	"policy-registry", "capability-overrides",
}

// appSettingsPrefix is the dynamic option namespace: any app.settings.<name>
// key is accepted and carried to the backend as a transaction setting.
const appSettingsPrefix = "app.settings."

// appSettingsEnvPrefix is the env-suffix spelling of the same namespace:
// PGRST_APP_SETTINGS_FOO maps to app.settings.foo.
const appSettingsEnvPrefix = "APP_SETTINGS_"

// envSuffix turns an option key into the variable suffix shared by both
// prefixes: "db-uri" becomes "DB_URI", read as PGRST_DB_URI or DBREST_DB_URI.
func envSuffix(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

// overlayEnv layers the environment over raw and returns warnings for
// namespaced variables that match no known option. For each known key it reads
// the PGRST_ spelling first, then the DBREST_ spelling, so DBREST_ wins on a
// conflict; either present overrides the file. The dynamic
// PGRST_APP_SETTINGS_* / DBREST_APP_SETTINGS_* namespace maps to
// app.settings.* keys with a lowercased name. environ is os.Environ() form.
func overlayEnv(raw map[string]string, environ []string) []string {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	known := map[string]bool{}
	for _, k := range optionKeys {
		known[envSuffix(k)] = true
	}
	var warnings []string
	for _, prefix := range []string{"PGRST_", "DBREST_"} {
		for _, key := range optionKeys {
			if v, ok := env[prefix+envSuffix(key)]; ok {
				raw[key] = v
			}
		}
		// The dynamic namespace and the unknown-suffix warnings need a scan
		// over what is actually set, not over what we expect.
		for name, v := range env {
			suffix, ok := strings.CutPrefix(name, prefix)
			if !ok {
				continue
			}
			if setting, ok := strings.CutPrefix(suffix, appSettingsEnvPrefix); ok && setting != "" {
				raw[appSettingsPrefix+strings.ToLower(setting)] = v
				continue
			}
			if !known[suffix] {
				warnings = append(warnings, fmt.Sprintf("ignoring %s: no option named %q", name, strings.ToLower(strings.ReplaceAll(suffix, "_", "-"))))
			}
		}
	}
	return warnings
}

// parseFile reads a PostgREST-style flat configuration file into a raw map. The
// format is one "key = value" per line; values are bare, double-quoted, or
// triple-quoted for multi-line strings; '#' begins a comment outside a quoted
// value; blank lines are skipped. An unknown key is kept out of the map and
// reported as a warning, matching PostgREST, which ignores keys it does not
// own; the same posture applies to unknown namespaced environment variables in
// overlayEnv, so the two sources fail symmetrically.
func parseFile(path string) (map[string]string, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	known := map[string]bool{}
	for _, k := range optionKeys {
		known[k] = true
	}
	raw := map[string]string{}
	var warnings []string
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		rawKey, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			return nil, nil, fmt.Errorf("config: %s line %d: expected key = value", path, i+1)
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)
		if !known[key] && !strings.HasPrefix(key, appSettingsPrefix) {
			warnings = append(warnings, fmt.Sprintf("%s line %d: ignoring unknown option %q", path, i+1, key))
			key = ""
		}
		if strings.HasPrefix(val, `"""`) {
			block, used, err := readTripleQuoted(lines, i, val)
			if err != nil {
				return nil, nil, fmt.Errorf("config: %s line %d: %w", path, i+1, err)
			}
			if key != "" {
				expanded, err := interpolate(block, raw)
				if err != nil {
					return nil, nil, fmt.Errorf("config: %s line %d: %w", path, i+1, err)
				}
				raw[key] = expanded
			}
			i = used
			continue
		}
		if key != "" {
			v := val
			if quoted := strings.HasPrefix(val, `"`); quoted {
				// Only quoted strings interpolate, as in upstream's config
				// format; a bare number or boolean is taken verbatim.
				expanded, err := interpolate(unquote(val), raw)
				if err != nil {
					return nil, nil, fmt.Errorf("config: %s line %d: %w", path, i+1, err)
				}
				v = expanded
			}
			raw[key] = v
		}
	}
	return raw, warnings, nil
}

// interpolate expands $(NAME) inside a config-file string value, the upstream
// configurator behavior: NAME resolves against the options bound earlier in
// the file first, then the process environment, and an unset name is a hard
// error rather than an empty string. The sequence $$ collapses to a literal
// dollar. Environment-sourced option values are never interpolated; this runs
// only on file values.
func interpolate(v string, raw map[string]string) (string, error) {
	if !strings.Contains(v, "$") {
		return v, nil
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '$' {
			b.WriteByte(v[i])
			continue
		}
		if i+1 < len(v) && v[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 < len(v) && v[i+1] == '(' {
			end := strings.IndexByte(v[i+2:], ')')
			if end < 0 {
				return "", fmt.Errorf("unterminated $( in %q", v)
			}
			name := v[i+2 : i+2+end]
			if prior, ok := raw[name]; ok {
				b.WriteString(prior)
			} else if env, ok := os.LookupEnv(name); ok {
				b.WriteString(env)
			} else {
				return "", fmt.Errorf("no such variable %q", name)
			}
			i += 2 + end
			continue
		}
		b.WriteByte('$')
	}
	return b.String(), nil
}

// stripComment removes a trailing '#' comment from a line, leaving '#' that sits
// inside a double-quoted value alone.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

// readTripleQuoted gathers a """...""" block that may span lines. first is the
// text after the key on the opening line, including the opening """. It returns
// the unquoted contents and the index of the last consumed line.
func readTripleQuoted(lines []string, start int, first string) (string, int, error) {
	body := strings.TrimPrefix(first, `"""`)
	if rest, ok := strings.CutSuffix(body, `"""`); ok && rest != body {
		return rest, start, nil
	}
	var b strings.Builder
	b.WriteString(body)
	for i := start + 1; i < len(lines); i++ {
		if idx := strings.Index(lines[i], `"""`); idx >= 0 {
			b.WriteString("\n")
			b.WriteString(lines[i][:idx])
			return b.String(), i, nil
		}
		b.WriteString("\n")
		b.WriteString(lines[i])
	}
	return "", start, fmt.Errorf("unterminated triple-quoted value")
}

// unquote strips one layer of surrounding double quotes from a bare value.
func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// splitList parses a comma-separated option into trimmed, non-empty elements.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// pickString reads the first present key among aliases as a string, falling
// back to def when none is set. PostgREST keeps a handful of pre-rename
// aliases (pre-request, root-spec, db-schema, role-claim-key) working; this is
// the string side of that.
func pickString(raw map[string]string, def string, keys ...string) string {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			return v
		}
	}
	return def
}

// pickInt reads the first present key among aliases as an integer, recording a
// validation error on a malformed value and falling back to def.
func pickInt(raw map[string]string, errs *[]string, def int, keys ...string) int {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("%s %q is not an integer", key, v))
			return def
		}
		return n
	}
	return def
}

// pickBool reads the first present key among aliases as a boolean (true/false,
// 1/0), recording a validation error on a malformed value and falling back to
// def.
func pickBool(raw map[string]string, errs *[]string, def bool, keys ...string) bool {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("%s %q is not a boolean", key, v))
			return def
		}
		return b
	}
	return def
}

// pickSeconds reads the first present key as an integer number of seconds,
// the unit upstream uses for the pool timeouts (`db-pool-acquisition-timeout
// = 10`). A Go duration string ("500ms") is also accepted, a dbrest extension
// for sub-second values.
func pickSeconds(raw map[string]string, errs *[]string, def time.Duration, keys ...string) time.Duration {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		s := strings.TrimSpace(v)
		if n, err := strconv.Atoi(s); err == nil {
			return time.Duration(n) * time.Second
		}
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
		*errs = append(*errs, fmt.Sprintf("%s %q is not a number of seconds", key, v))
		return def
	}
	return def
}
