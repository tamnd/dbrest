package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/reqctx"
)

// applySession reproduces the per-request setup PostgREST runs at the top of its
// transaction. All steps run in a SINGLE SendBatch so they occupy one network
// round-trip instead of three or more separate Exec calls:
//
//  1. SET LOCAL ROLE <role> (quoted, not parameterizable)
//  2. SET LOCAL search_path TO <schemas>
//  3. SELECT set_config('request.jwt.claims', $1, true), ... (5 GUCs in one row)
//
// Using a single batch is the primary per-request latency win over PostgREST,
// which issues these sequentially. On a 1 ms RTT link the saving is ~2 ms per
// request, doubling sustained throughput at modest concurrency.
func applySession(ctx context.Context, tx pgx.Tx, b *Backend, rc *reqctx.Context) error {
	batch := &pgx.Batch{}

	// SET LOCAL ROLE must be a literal identifier, not a bind parameter, so it
	// is quoted and inlined into the SQL text.
	if rc.Role != "" {
		batch.Queue("SET LOCAL ROLE " + (Dialect{}).QuoteIdent(rc.Role))
	}

	// SET LOCAL search_path is stable per Backend (only changes on config reload).
	// Use the pre-computed string from b.searchPathSQL when available.
	if b.searchPathSQL != "" {
		batch.Queue(b.searchPathSQL)
	}

	// Five GUCs in a single query: one parse, one network message, one result row.
	batch.Queue(
		"SELECT set_config($1,$2,true),set_config($3,$4,true),"+
			"set_config($5,$6,true),set_config($7,$8,true),set_config($9,$10,true)",
		"request.jwt.claims", string(rc.ClaimsJSON()),
		"request.method", rc.Method,
		"request.path", rc.Path,
		"request.headers", string(rc.HeadersJSON()),
		"request.cookies", string(rc.CookiesJSON()),
	)

	br := tx.SendBatch(ctx, batch)
	for range batch.Len() {
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
		"SELECT current_setting('response.status', true), current_setting('response.headers', true)",
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

// buildSearchPathSQL pre-computes the SET LOCAL search_path statement for a
// Backend so the string is built once and reused per request.
func buildSearchPathSQL(schemas []string) string {
	if len(schemas) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("SET LOCAL search_path TO ")
	for i, s := range schemas {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString((Dialect{}).QuoteIdent(s))
	}
	return b.String()
}
