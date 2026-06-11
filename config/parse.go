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
	"db-backend", "db-uri", "db-schemas", "db-anon-role", "db-pre-request",
	"db-extra-search-path", "db-max-rows", "max-rows",
	"jwt-secret", "jwt-aud", "jwt-role-claim-key", "jwk-set", "jwt-cache-max-entries",
	"server-host", "server-port", "server-unix-socket",
	"admin-server-host", "admin-server-port",
	"db-pool", "db-pool-acquisition-timeout",
	"openapi-mode", "openapi-server-proxy-uri", "openapi-security-active",
	"log-level", "log-query", "server-cors-allowed-origins",
	"declared-schema", "declared-relationships", "function-registry",
	"policy-registry", "capability-overrides",
}

// envSuffix turns an option key into the variable suffix shared by both
// prefixes: "db-uri" becomes "DB_URI", read as PGRST_DB_URI or DBREST_DB_URI.
func envSuffix(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

// overlayEnv layers the environment over raw. For each known key it reads the
// PGRST_ spelling first, then the DBREST_ spelling, so DBREST_ wins on a
// conflict; either present overrides the file. environ is os.Environ() form.
func overlayEnv(raw map[string]string, environ []string) {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for _, key := range optionKeys {
		suffix := envSuffix(key)
		if v, ok := env["PGRST_"+suffix]; ok {
			raw[key] = v
		}
		if v, ok := env["DBREST_"+suffix]; ok {
			raw[key] = v
		}
	}
}

// parseFile reads a PostgREST-style flat configuration file into a raw map. The
// format is one "key = value" per line; values are bare, double-quoted, or
// triple-quoted for multi-line strings; '#' begins a comment outside a quoted
// value; blank lines are skipped. Unknown keys are an error, so a mistyped
// option fails loudly at startup rather than being ignored.
func parseFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	known := map[string]bool{}
	for _, k := range optionKeys {
		known[k] = true
	}
	raw := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		rawKey, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config: %s line %d: expected key = value", path, i+1)
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)
		if !known[key] {
			return nil, fmt.Errorf("config: %s line %d: unknown option %q", path, i+1, key)
		}
		if strings.HasPrefix(val, `"""`) {
			block, used, err := readTripleQuoted(lines, i, val)
			if err != nil {
				return nil, fmt.Errorf("config: %s line %d: %w", path, i+1, err)
			}
			raw[key] = block
			i = used
			continue
		}
		raw[key] = unquote(val)
	}
	return raw, nil
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

// pickBool reads key as a boolean (true/false, 1/0), recording a validation
// error on a malformed value and falling back to def.
func pickBool(raw map[string]string, errs *[]string, def bool, key string) bool {
	v, ok := raw[key]
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s %q is not a boolean", key, v))
		return def
	}
	return b
}

// pickDuration reads key as a Go duration (for example "10s"), recording a
// validation error on a malformed value and falling back to def.
func pickDuration(raw map[string]string, errs *[]string, def time.Duration, key string) time.Duration {
	v, ok := raw[key]
	if !ok {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s %q is not a duration", key, v))
		return def
	}
	return d
}
