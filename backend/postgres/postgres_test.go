package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tamnd/dbrest/pgerr"
)

// TestResolveExecMode covers finding 02-P09: the pooler-tolerant cache_describe
// default is used only when the DSN does not name a mode, and an operator's
// default_query_exec_mode choice in the DSN (the documented PgBouncer escape
// hatch) is honored rather than clobbered.
func TestResolveExecMode(t *testing.T) {
	cases := []struct {
		name   string
		dsn    string
		parsed pgx.QueryExecMode
		want   pgx.QueryExecMode
	}{
		{"omitted defaults to cache_describe", "postgres://u:p@h/db", pgx.QueryExecModeCacheStatement, pgx.QueryExecModeCacheDescribe},
		{"simple_protocol honored", "postgres://u:p@h/db?default_query_exec_mode=simple_protocol", pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeSimpleProtocol},
		{"exec honored", "postgres://u:p@h/db?default_query_exec_mode=exec", pgx.QueryExecModeExec, pgx.QueryExecModeExec},
		{"explicit cache_statement honored", "postgres://u:p@h/db?default_query_exec_mode=cache_statement", pgx.QueryExecModeCacheStatement, pgx.QueryExecModeCacheStatement},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveExecMode(tc.dsn, tc.parsed); got != tc.want {
				t.Errorf("resolveExecMode(%q, %v) = %v, want %v", tc.dsn, tc.parsed, got, tc.want)
			}
		})
	}
}

// MapError maps PostgreSQL SQLSTATE codes to the API error envelope the way
// PostgREST does. Unit tests drive mapPgError and statusForSQLState directly so
// there is no need for a live server.

func TestMapErrorConstraintViolations(t *testing.T) {
	cases := []struct {
		code       string
		wantAPIErr string // the expected Code in the returned APIError
		wantStatus int
	}{
		{"23505", pgerr.CodeUniqueViolation, 409},
		{"23502", pgerr.CodeNotNullViolation, 400},
		{"23503", pgerr.CodeForeignKeyViolation, 409},
		{"23514", pgerr.CodeCheckViolation, 400},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			pg := &pgconn.PgError{Code: c.code, Message: "test", Detail: "detail"}
			got := mapPgError(pg)
			if got.Code != c.wantAPIErr {
				t.Errorf("code %s: Code = %q, want %q", c.code, got.Code, c.wantAPIErr)
			}
			if got.HTTPStatus != c.wantStatus {
				t.Errorf("code %s: HTTPStatus = %d, want %d", c.code, got.HTTPStatus, c.wantStatus)
			}
		})
	}
}

// PostgREST forwards a PostgreSQL constraint error's message and detail
// verbatim, so an application reading the constraint name out of the message or
// the offending key out of the detail still finds them. The postgres backend
// passes both through unchanged rather than rewriting them to a canonical text.
func TestMapErrorConstraintMessageVerbatim(t *testing.T) {
	pg := &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "films_pkey"`,
		Detail:  "Key (id)=(1) already exists.",
		Hint:    "use a different id",
	}
	got := mapPgError(pg)
	if got.Message != pg.Message {
		t.Errorf("Message = %q, want verbatim %q", got.Message, pg.Message)
	}
	if got.Details == nil || *got.Details != pg.Detail {
		t.Errorf("Details = %v, want verbatim %q", got.Details, pg.Detail)
	}
	if got.Hint == nil || *got.Hint != pg.Hint {
		t.Errorf("Hint = %v, want verbatim %q", got.Hint, pg.Hint)
	}
}

// A function raising SQLSTATE 'PGRST' takes full control: mapPgError reads the
// envelope from MESSAGE and the status and headers from DETAIL, surfacing the
// headers on the error so the renderer emits them (item 04.9).
func TestMapErrorRaisePGRSTFullControl(t *testing.T) {
	pg := &pgconn.PgError{
		Code:    "PGRST",
		Message: `{"code":"123","message":"Payment Required","details":"pay up","hint":"add a card"}`,
		Detail:  `{"status":402,"headers":{"X-Reason":"quota"}}`,
	}
	got := mapPgError(pg)
	if got.Code != "123" || got.Message != "Payment Required" {
		t.Errorf("envelope = %q/%q, want 123/Payment Required", got.Code, got.Message)
	}
	if got.HTTPStatus != 402 {
		t.Errorf("status = %d, want 402 from detail.status", got.HTTPStatus)
	}
	if got.Details == nil || *got.Details != "pay up" {
		t.Errorf("details = %v, want 'pay up'", got.Details)
	}
	if h := got.Headers.Get("X-Reason"); h != "quota" {
		t.Errorf("X-Reason header = %q, want quota", h)
	}
}

// A malformed full-control payload is PGRST121 (500), not a leaked raw string
// (item 04.9). The DETAIL here is not valid JSON.
func TestMapErrorRaisePGRSTMalformed(t *testing.T) {
	pg := &pgconn.PgError{
		Code:    "PGRST",
		Message: `{"code":"123","message":"ok"}`,
		Detail:  `not json`,
	}
	got := mapPgError(pg)
	if got.Code != "PGRST121" {
		t.Errorf("code = %q, want PGRST121", got.Code)
	}
	if got.HTTPStatus != 500 {
		t.Errorf("status = %d, want 500", got.HTTPStatus)
	}
	if len(got.Headers) != 0 {
		t.Errorf("a malformed payload must apply no headers, got %v", got.Headers)
	}
}

func TestMapErrorPassthrough(t *testing.T) {
	pg := &pgconn.PgError{Code: "42P01", Message: "relation does not exist", Hint: "check your schema"}
	got := mapPgError(pg)
	if got.Code != "42P01" {
		t.Errorf("Code = %q, want 42P01", got.Code)
	}
	if got.HTTPStatus != 404 {
		t.Errorf("HTTPStatus = %d, want 404", got.HTTPStatus)
	}
	if got.Hint == nil || *got.Hint != "check your schema" {
		t.Errorf("Hint = %v, want 'check your schema'", got.Hint)
	}
}

func TestMapErrorNil(t *testing.T) {
	b := &Backend{}
	if got := b.MapError(nil); got != nil {
		t.Errorf("MapError(nil) = %v, want nil", got)
	}
}

func TestMapErrorNonPg(t *testing.T) {
	b := &Backend{}
	got := b.MapError(errors.New("boom"))
	if got == nil {
		t.Fatal("MapError(non-PG) = nil, want internal error")
	}
	if got.HTTPStatus != 500 {
		t.Errorf("HTTPStatus = %d, want 500", got.HTTPStatus)
	}
	if got.Code != pgerr.CodeInternal {
		t.Errorf("Code = %q, want %q", got.Code, pgerr.CodeInternal)
	}
}

// TestMapTransportError covers finding 03-P19(e): a driver failure that never
// reached a SQLSTATE is classified into PostgREST's connection-error family
// instead of collapsing to 500. A refused dial (pgx *pgconn.ConnectError) is
// PGRST000/503; a pool-acquisition timeout (context deadline) is PGRST003/504.
func TestMapTransportError(t *testing.T) {
	b := &Backend{}

	// A real *pgconn.ConnectError from a refused dial (the wrapped error is
	// unexported, so a refused localhost port is the way to get a valid one).
	_, dialErr := pgconn.Connect(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	var ce *pgconn.ConnectError
	if !errors.As(dialErr, &ce) {
		t.Fatalf("expected a *pgconn.ConnectError from a refused dial, got %T (%v)", dialErr, dialErr)
	}
	conn := b.MapError(dialErr)
	if conn.HTTPStatus != 503 || conn.Code != pgerr.CodeDBConnection {
		t.Errorf("connect error => %d %q, want 503 %q", conn.HTTPStatus, conn.Code, pgerr.CodeDBConnection)
	}

	to := b.MapError(fmt.Errorf("acquire: %w", context.DeadlineExceeded))
	if to.HTTPStatus != 504 || to.Code != pgerr.CodeAcquireTimeout {
		t.Errorf("acquire timeout => %d %q, want 504 %q", to.HTTPStatus, to.Code, pgerr.CodeAcquireTimeout)
	}
	if to.Message != "Timed out acquiring connection from connection pool." {
		t.Errorf("acquire timeout message = %q", to.Message)
	}
}

func TestStatusForSQLState(t *testing.T) {
	cases := []struct {
		code string
		msg  string
		want int
	}{
		// well-known individual codes
		{"23503", "", 409},
		{"23505", "", 409},
		{"25006", "", 405},
		{"42883", "function foo() does not exist", 404},
		{"42883", "function xmlagg() does not exist", 406}, // xmlagg ambiguity
		{"42P01", "", 404},
		{"42501", "", 403}, // insufficient_privilege: 403 base, anon lifted to 401 by mapExecError
		// message-sniffing rows
		{"21000", "UPDATE requires a WHERE clause", 400},
		{"21000", "more than one row returned by a subquery", 500},
		{"22023", `role "ghost" does not exist`, 401},
		{"22023", "time zone displacement out of range", 400},
		// PTxxx convention
		{"PT403", "", 403},
		{"PT201", "", 201},
		{"PT999", "", 999}, // out of the 100-599 band but still a parsed status
		{"PTabc", "", 500}, // unparseable suffix falls back to 500
		{"PT042", "", 500}, // below 100, not emittable, falls back to 500
		// class rules and corrected edge rows
		{"08000", "", 503},
		{"28000", "", 403},
		{"53100", "", 503},
		{"53400", "", 500}, // config limit exceeded: 500, not its class 503
		{"54001", "", 500}, // program limit exceeded: 500, not 413
		{"57000", "", 500},
		{"57P01", "", 503}, // admin shutdown: 503, not its class 500
		{"XX000", "", 500},
		{"P0001", "", 400}, // raise default: client error
		{"P0002", "", 500}, // other PL/pgSQL: server error
		// default
		{"00000", "", 400},
		{"ZZZZZ", "", 400},
		{"short", "", 400},
	}
	for _, c := range cases {
		got := statusForSQLState(c.code, c.msg)
		if got != c.want {
			t.Errorf("statusForSQLState(%q, %q) = %d, want %d", c.code, c.msg, got, c.want)
		}
	}
}

func BenchmarkMapError(b *testing.B) {
	pg := &pgconn.PgError{Code: "23505", Message: "dup", Detail: "Key (id)=(1) already exists."}
	b.ReportAllocs()
	for b.Loop() {
		mapPgError(pg)
	}
}
