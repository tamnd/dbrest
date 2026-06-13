package postgres

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/reqctx"
)

// loadFunctionProconfig reads pg_proc.proconfig (a function's SET clause) for
// every function in the exposed schemas into a map keyed by "schema.name", so an
// RPC call can hoist the settings db-hoisted-tx-settings selects to the
// transaction the way PostgREST does. proconfig is a text[] of "name=value"
// entries; the value half can itself contain '=', so only the first '=' splits.
//
// A name with several overloads collapses to one key: the entries are appended
// and hoistFor takes the last value per setting, so overloads that disagree on a
// hoisted setting resolve to the last one introspected. This is the documented
// limit of static introspection (the actual overload is resolved by argument
// types at call time, which the static map does not model).
func (b *Backend) loadFunctionProconfig(ctx context.Context, schemas []string) (map[string][]roleSetting, error) {
	const q = `
SELECT n.nspname, p.proname, p.proconfig
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = ANY($1) AND p.proconfig IS NOT NULL`
	rows, err := b.pool.Query(ctx, q, schemas)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]roleSetting{}
	for rows.Next() {
		var nsp, name string
		var cfg []string
		if err := rows.Scan(&nsp, &name, &cfg); err != nil {
			return nil, err
		}
		key := nsp + "." + name
		for _, kv := range cfg {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			out[key] = append(out[key], roleSetting{name: kv[:i], value: kv[i+1:]})
		}
	}
	return out, rows.Err()
}

// hoistFor returns the hoisted transaction settings for an RPC call: the function
// SET options whose names are in db-hoisted-tx-settings, with the last value per
// name winning. default_transaction_isolation is split out as an isolation level
// because it cannot be set with set_config once the transaction has run a
// statement; the caller applies it at BeginTx. The remaining settings are
// returned sorted by name so the replay order is deterministic.
func (b *Backend) hoistFor(plan *ir.Plan, rc *reqctx.Context) ([]roleSetting, pgx.TxIsoLevel) {
	if b.funcProconfig == nil || len(b.hoistedTxSettings) == 0 || plan.Call == nil {
		return nil, ""
	}
	key := b.callSchema(rc) + "." + plan.Call.Function.Name
	raw := b.funcProconfig[key]
	if len(raw) == 0 {
		return nil, ""
	}

	picked := map[string]string{}
	for _, s := range raw {
		if slices.Contains(b.hoistedTxSettings, s.name) {
			picked[s.name] = s.value // last wins
		}
	}

	var iso pgx.TxIsoLevel
	var sets []roleSetting
	for name, val := range picked {
		if name == "default_transaction_isolation" {
			if lvl, ok := isoLevelFromName(val); ok {
				iso = lvl
			}
			continue
		}
		sets = append(sets, roleSetting{name: name, value: val})
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].name < sets[j].name })
	return sets, iso
}

// applyHoisted replays the hoisted settings as transaction-scoped settings after
// the session is set up and before the call statement, so they override the role
// and connection settings for the whole statement (including the count query of a
// set-returning call), matching PostgREST. default_transaction_isolation is not
// here; it is applied at BeginTx by the caller.
func applyHoisted(ctx context.Context, tx pgx.Tx, sets []roleSetting) error {
	for _, s := range sets {
		if _, err := tx.Exec(ctx, "SELECT set_config($1,$2,true)", s.name, s.value); err != nil {
			return err
		}
	}
	return nil
}

// callTxOptions builds the transaction options for an RPC call: the role's
// options (access mode plus any role default_transaction_isolation) with the
// hoisted default_transaction_isolation overriding the role's, since a function's
// SET clause takes precedence over the role and connection settings.
func (b *Backend) callTxOptions(plan *ir.Plan, rc *reqctx.Context, mode pgx.TxAccessMode) (pgx.TxOptions, []roleSetting) {
	opts := b.txOptions(rc, mode)
	sets, iso := b.hoistFor(plan, rc)
	if iso != "" {
		opts.IsoLevel = iso
	}
	return opts, sets
}
