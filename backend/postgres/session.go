package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/reqctx"
)

// queueSessionItems appends the per-request GUC setup items to batch and
// returns the number of items added. The caller must send and drain the batch.
//
//  1. SET LOCAL ROLE <role> (quoted, not parameterizable)
//  2. SET LOCAL search_path TO <schemas>
//  3. SELECT set_config(...) x5 GUCs in one SQL row
func queueSessionItems(batch *pgx.Batch, b *Backend, rc *reqctx.Context) int {
	n := 0
	if rc.Role != "" {
		batch.Queue("SET LOCAL ROLE " + (Dialect{}).QuoteIdent(rc.Role))
		n++
	}
	if sp := b.searchPathValue(rc); sp != "" {
		// set_config(...,true) is SET LOCAL search_path. PostgREST sets it the same
		// way rather than with SET ... TO <idents>, so the GUC string is the literal
		// quoted value verbatim ("schema", "public"); a SET ... TO would let the
		// server re-canonicalize and strip quotes from simple names, so a policy that
		// reads current_setting('search_path') would see a different string.
		batch.Queue("SELECT set_config('search_path',$1,true)", sp)
		n++
	}
	if rc.TimeZone != "" {
		// set_config(...,true) is the SET LOCAL timezone analog, parameterized so a
		// name with a slash (America/Los_Angeles) needs no identifier quoting.
		batch.Queue("SELECT set_config('timezone',$1,true)", rc.TimeZone)
		n++
	}
	batch.Queue(
		"SELECT set_config($1,$2,true),set_config($3,$4,true),"+
			"set_config($5,$6,true),set_config($7,$8,true),set_config($9,$10,true)",
		"request.jwt.claims", string(rc.ClaimsJSON()),
		"request.method", rc.Method,
		"request.path", rc.Path,
		"request.headers", string(rc.HeadersJSON()),
		"request.cookies", string(rc.CookiesJSON()),
	)
	n++
	if rc.PreRequest != "" {
		// db-pre-request runs after the transaction-scoped settings and before the
		// main query, in the same transaction, so it sees the request context and
		// can raise to abort or write response.status/response.headers. A raised
		// error surfaces when the batch is drained and aborts the request.
		batch.Queue("SELECT " + preRequestCall(rc.PreRequest) + "()")
		n++
	}
	return n
}

// preRequestCall renders the db-pre-request function name as a quoted, possibly
// schema-qualified callable, so a name like auth.check or one needing quoting is
// safe to interpolate.
func preRequestCall(fn string) string {
	parts := strings.Split(fn, ".")
	for i, p := range parts {
		parts[i] = (Dialect{}).QuoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// applySession sends the per-request GUC setup as a SINGLE batch within tx,
// occupying one network round-trip.
func applySession(ctx context.Context, tx pgx.Tx, b *Backend, rc *reqctx.Context) error {
	batch := &pgx.Batch{}
	n := queueSessionItems(batch, b, rc)
	br := tx.SendBatch(ctx, batch)
	for range n {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}

// readResponseControls reads back the response.status and response.headers GUCs a
// SQL function may have set during the request, and folds them into the response
// controls. A GUC that was never set returns empty string (current_setting with
// the missing_ok flag); that is silently ignored. response.headers is a JSON
// array of single-key name→value objects, matching PostgREST's convention.
func readResponseControls(ctx context.Context, tx pgx.Tx, controls *reqctx.ResponseControls) error {
	var status, headers string
	err := tx.QueryRow(ctx,
		"SELECT COALESCE(current_setting('response.status', true), ''),"+
			" COALESCE(current_setting('response.headers', true), '')",
	).Scan(&status, &headers)
	if err != nil {
		return err
	}
	if status != "" {
		if code, err := strconv.Atoi(status); err == nil {
			controls.SetStatus(code)
		}
	}
	if headers != "" {
		var list []map[string]string
		if err := json.Unmarshal([]byte(headers), &list); err == nil {
			for _, obj := range list {
				for name, val := range obj {
					controls.SetHeader(name, val)
				}
			}
		}
	}
	return nil
}

// searchPathValue builds the per-request search_path GUC value. The path is the
// request's active schema (the Accept-Profile/Content-Profile choice, or the
// first configured schema by default) followed by db-extra-search-path, matching
// PostgREST: only the active schema is on the path, not the whole exposed set,
// and the extra entries are appended without deduplication. Each name is quoted,
// so the joined string is the literal value PostgREST writes ("schema", "public").
// An empty active schema (the emulated-namespace marker the engines without named
// schemas use) yields an empty value, so no search_path is set.
func (b *Backend) searchPathValue(rc *reqctx.Context) string {
	active := b.callSchema(rc)
	if active == "" {
		return ""
	}
	schemas := append([]string{active}, b.extraSearchPath...)
	parts := make([]string, len(schemas))
	for i, s := range schemas {
		parts[i] = (Dialect{}).QuoteIdent(s)
	}
	return strings.Join(parts, ", ")
}
