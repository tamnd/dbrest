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
// transaction, so row-level security and SQL policies observe the same state on
// dbrest as on PostgREST. It runs inside the caller's transaction, before the
// main statement, and sets three things in order:
//
//  1. the request role, with SET LOCAL ROLE, so RLS and table privileges are
//     enforced by the engine as that role (and reset at COMMIT);
//  2. the search path, with SET LOCAL search_path, so unqualified names in
//     policies and functions resolve against the exposed schemas;
//  3. the request context GUCs (request.jwt.claims, request.method, request.path,
//     request.headers, request.cookies), with set_config(..., true) so they are
//     transaction-local, the GUCs a policy reads through current_setting.
//
// The role identifier cannot be a bind parameter, so it is quoted through the
// dialect; the GUC values are all bound.
func applySession(ctx context.Context, tx pgx.Tx, b *Backend, rc *reqctx.Context) error {
	if rc.Role != "" {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+(Dialect{}).QuoteIdent(rc.Role)); err != nil {
			return err
		}
	}
	if len(b.searchPath) > 0 {
		var path strings.Builder
		for i, s := range b.searchPath {
			if i > 0 {
				path.WriteString(", ")
			}
			path.WriteString((Dialect{}).QuoteIdent(s))
		}
		if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+path.String()); err != nil {
			return err
		}
	}

	batch := &pgx.Batch{}
	setGUC := func(key, val string) {
		batch.Queue("SELECT set_config($1, $2, true)", key, val)
	}
	setGUC("request.jwt.claims", string(rc.ClaimsJSON()))
	setGUC("request.method", rc.Method)
	setGUC("request.path", rc.Path)
	setGUC("request.headers", string(rc.HeadersJSON()))
	setGUC("request.cookies", string(rc.CookiesJSON()))

	br := tx.SendBatch(ctx, batch)
	defer br.Close()
	for range batch.Len() {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// readResponseControls reads back the response.status and response.headers GUCs a
// SQL function may have set during the request, and folds them into the response
// controls, exactly as PostgREST does after running the main statement. A GUC
// that was never set comes back empty (current_setting with the missing_ok flag),
// and is ignored. response.headers is a JSON array of single-key objects, each
// one header name to value; response.status is the integer status to apply.
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
