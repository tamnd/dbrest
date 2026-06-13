package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tamnd/dbrest/pgerr"
)

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
	got := b.MapError(context.DeadlineExceeded)
	if got == nil {
		t.Fatal("MapError(non-PG) = nil, want internal error")
	}
	if got.HTTPStatus != 500 {
		t.Errorf("HTTPStatus = %d, want 500", got.HTTPStatus)
	}
}

func TestStatusForSQLState(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		// well-known individual codes
		{"23503", 409},
		{"23505", 409},
		{"25006", 405},
		{"42883", 404},
		{"42P01", 404},
		{"42501", 403}, // insufficient_privilege: 403 base, anon lifted to 401 by mapExecError
		// PTxxx convention
		{"PT403", 403},
		{"PT201", 201},
		// class rules
		{"08000", 503},
		{"28000", 403},
		{"53100", 503},
		{"54001", 413},
		{"XX000", 500},
		{"P0001", 400},
		// default
		{"00000", 400},
		{"ZZZZZ", 400},
		{"short", 400},
	}
	for _, c := range cases {
		got := statusForSQLState(c.code)
		if got != c.want {
			t.Errorf("statusForSQLState(%q) = %d, want %d", c.code, got, c.want)
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
