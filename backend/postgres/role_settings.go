package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/reqctx"
)

// roleSetting is one ALTER ROLE ... SET key/value the backend replays as a
// transaction-scoped setting for the impersonated role.
type roleSetting struct {
	name  string
	value string
}

// loadRoleSettings reads the per-role configuration the impersonated role carries
// (ALTER ROLE <role> SET ...), so the backend can replay it as transaction-scoped
// settings the way PostgREST does. It mirrors PostgREST's queryRoleSettings
// (src/PostgREST/Config/Database.hs): settings come from pg_roles.rolconfig for
// every role the connected authenticator is a member of, and a setting is kept
// only when it is USERSET (pg_settings.context = 'user') or, on PostgreSQL 15+,
// the authenticator holds SET privilege on it (has_parameter_privilege), so a
// setting the session could not apply is skipped instead of aborting the request.
//
// default_transaction_isolation is pulled out separately: it cannot be applied
// with set_config once the transaction has run a statement, so it is returned as
// a per-role isolation level the execute paths pass to BeginTx, while the rest are
// returned as set_config replays.
func (b *Backend) loadRoleSettings(ctx context.Context) (map[string][]roleSetting, map[string]pgx.TxIsoLevel, error) {
	// has_parameter_privilege is only available on PostgreSQL 15+, matching the
	// gate PostgREST applies to the same filter.
	privClause := ""
	if b.version.Major >= 15 {
		privClause = " OR has_parameter_privilege(quote_ident(current_user)::regrole::oid, ps.name, 'set')"
	}
	q := `
WITH role_setting AS (
	SELECT r.rolname, unnest(r.rolconfig) AS setting
	  FROM pg_auth_members m
	  JOIN pg_roles r ON r.oid = m.roleid
	 WHERE m.member = quote_ident(current_user)::regrole::oid
),
kv AS (
	SELECT rolname,
	       substr(setting, 1, strpos(setting, '=') - 1) AS key,
	       substr(setting, strpos(setting, '=') + 1) AS value
	  FROM role_setting
)
SELECT kv.rolname, kv.key, kv.value
  FROM kv
  LEFT JOIN pg_settings ps ON ps.name = kv.key
 WHERE kv.value IS NOT NULL
   AND (kv.key = 'default_transaction_isolation' OR ps.context = 'user'` + privClause + `)`

	rows, err := b.pool.Query(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	settings := map[string][]roleSetting{}
	isolation := map[string]pgx.TxIsoLevel{}
	for rows.Next() {
		var role, key, value string
		if err := rows.Scan(&role, &key, &value); err != nil {
			return nil, nil, err
		}
		if key == "default_transaction_isolation" {
			if lvl, ok := isoLevelFromName(value); ok {
				isolation[role] = lvl
			}
			continue
		}
		settings[role] = append(settings[role], roleSetting{name: key, value: value})
	}
	return settings, isolation, rows.Err()
}

// isoLevelFromName maps a default_transaction_isolation value to the pgx isolation
// level. The names are PostgreSQL's canonical spellings; an unrecognized value
// leaves the server default in place.
func isoLevelFromName(name string) (pgx.TxIsoLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "serializable":
		return pgx.Serializable, true
	case "repeatable read":
		return pgx.RepeatableRead, true
	case "read committed":
		return pgx.ReadCommitted, true
	case "read uncommitted":
		return pgx.ReadUncommitted, true
	default:
		return "", false
	}
}

// roleIso returns the impersonated role's default_transaction_isolation level, or
// "" when the role pins none, so BeginTx keeps the server default.
func (b *Backend) roleIso(rc *reqctx.Context) pgx.TxIsoLevel {
	if b.roleIsolation == nil || rc == nil || rc.Role == "" {
		return ""
	}
	return b.roleIsolation[rc.Role]
}

// txOptions builds the transaction options for a request: the given access mode
// plus the impersonated role's default_transaction_isolation when it pins one, so
// a role's ALTER ROLE ... SET default_transaction_isolation takes effect on every
// request the way it does under PostgREST. An empty access mode keeps pgx's server
// default (read-write).
func (b *Backend) txOptions(rc *reqctx.Context, mode pgx.TxAccessMode) pgx.TxOptions {
	o := pgx.TxOptions{AccessMode: mode}
	if iso := b.roleIso(rc); iso != "" {
		o.IsoLevel = iso
	}
	return o
}

// isoAtLeastRepeatableRead reports whether lvl already gives a single transaction
// snapshot (REPEATABLE READ or SERIALIZABLE), so the counted-read path does not
// downgrade a role that pins a stronger level.
func isoAtLeastRepeatableRead(lvl pgx.TxIsoLevel) bool {
	return lvl == pgx.RepeatableRead || lvl == pgx.Serializable
}
