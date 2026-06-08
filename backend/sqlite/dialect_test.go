package sqlite

import (
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
)

// Cast maps each canonical type to a SQLite affinity spelling. The cases mirror
// the dialect's switch so a future edit that drops an affinity is caught; the
// default arm (an unknown type coerced through TEXT) is covered too.
func TestDialectCast(t *testing.T) {
	var d dialect
	cases := []struct{ canonical, want string }{
		{"int", "CAST(x AS INTEGER)"},
		{"bigint", "CAST(x AS INTEGER)"},
		{"integer", "CAST(x AS INTEGER)"},
		{"numeric", "CAST(x AS REAL)"},
		{"double precision", "CAST(x AS REAL)"},
		{"text", "CAST(x AS TEXT)"},
		{"uuid", "CAST(x AS TEXT)"},
		{"timestamptz", "CAST(x AS TEXT)"},
		{"json", "json(x)"},
		{"jsonb", "json(x)"},
		{"bool", "CAST(x AS INTEGER)"},
		{"boolean", "CAST(x AS INTEGER)"},
		{"citext", "CAST(x AS TEXT)"}, // unknown type takes the TEXT default
	}
	for _, c := range cases {
		if got := d.Cast("x", c.canonical); got != c.want {
			t.Errorf("Cast(x, %q) = %q, want %q", c.canonical, got, c.want)
		}
	}
}

// SessionRead and SessionWrite report that SQLite has no engine-side setting
// store, and BoolValue renders the integer SQLite uses for a boolean. None of
// these is reached by the read-path tests, yet each is part of the dialect's
// contract with the compiler.
func TestDialectSessionAndBool(t *testing.T) {
	var d dialect
	if got := d.SessionRead("dbrest.role"); got != "" {
		t.Errorf("SessionRead = %q, want empty (no engine setting store)", got)
	}
	if got, ok := d.SessionWrite("dbrest.role"); ok || got != "" {
		t.Errorf("SessionWrite = (%q, %v), want (\"\", false)", got, ok)
	}
	if got := d.BoolValue(true); got != "1" {
		t.Errorf("BoolValue(true) = %q, want 1", got)
	}
	if got := d.BoolValue(false); got != "0" {
		t.Errorf("BoolValue(false) = %q, want 0", got)
	}
}

// canonicalType maps a SQLite declared type to the canonical PG name by the
// affinity substring rules. The table walks every arm, including the empty
// declaration (text) and the unrecognized default (numeric).
func TestCanonicalType(t *testing.T) {
	cases := []struct{ declared, want string }{
		{"", "text"},
		{"INTEGER", "integer"},
		{"BIGINT", "integer"},
		{"VARCHAR(40)", "text"},
		{"CLOB", "text"},
		{"TEXT", "text"},
		{"BLOB", "bytea"},
		{"REAL", "double precision"},
		{"FLOAT", "double precision"},
		{"DOUBLE", "double precision"},
		{"BOOLEAN", "boolean"},
		{"DATETIME", "timestamp"},
		{"DATE", "timestamp"},
		{"JSON", "json"},
		{"DECIMAL(10,2)", "numeric"}, // no affinity substring matches: NUMERIC
	}
	for _, c := range cases {
		if got := canonicalType(c.declared); got != c.want {
			t.Errorf("canonicalType(%q) = %q, want %q", c.declared, got, c.want)
		}
	}
}

// asString accepts the two textual driver forms SQLite hands back and rejects
// anything else, so a caller can tell a text value from a number without a panic.
func TestAsString(t *testing.T) {
	if s, ok := asString("hello"); !ok || s != "hello" {
		t.Errorf("asString(string) = (%q, %v), want (hello, true)", s, ok)
	}
	if s, ok := asString([]byte("bytes")); !ok || s != "bytes" {
		t.Errorf("asString([]byte) = (%q, %v), want (bytes, true)", s, ok)
	}
	if s, ok := asString(driver.Value(int64(7))); ok || s != "" {
		t.Errorf("asString(int64) = (%q, %v), want (\"\", false)", s, ok)
	}
}

// toString coerces a PRAGMA scalar to text, reporting false for a NULL (a nil
// or any non-textual form) so introspection can skip an absent value.
func TestToString(t *testing.T) {
	if s, ok := toString("col"); !ok || s != "col" {
		t.Errorf("toString(string) = (%q, %v), want (col, true)", s, ok)
	}
	if s, ok := toString([]byte("col")); !ok || s != "col" {
		t.Errorf("toString([]byte) = (%q, %v), want (col, true)", s, ok)
	}
	if s, ok := toString(nil); ok || s != "" {
		t.Errorf("toString(nil) = (%q, %v), want (\"\", false)", s, ok)
	}
}

// Capabilities is static, but the read path never inspects it; this pins the
// security-emulation tiers (no native roles or RLS, emulated session context)
// that the rest of the stack branches on.
func TestCapabilities(t *testing.T) {
	caps := (&Backend{}).Capabilities()
	if caps.NativeRoles || caps.NativeRLS || caps.NativeRPC {
		t.Errorf("SQLite emulates security: roles=%v rls=%v rpc=%v",
			caps.NativeRoles, caps.NativeRLS, caps.NativeRPC)
	}
	if caps.SessionContext != backend.Emulated {
		t.Errorf("SessionContext = %v, want Emulated", caps.SessionContext)
	}
	if caps.Returning != backend.Native || caps.Upsert != backend.Native {
		t.Error("RETURNING and upsert are native on SQLite")
	}
}

// Functions returns an empty registry until one is installed, then the one that
// was registered; Register(nil) clears it back to empty. The endpoint relies on
// always getting a non-nil registry to query.
func TestRegisterAndFunctions(t *testing.T) {
	b := &Backend{}
	if _, ok := b.Functions().(rpc.EmptyRegistry); !ok {
		t.Errorf("unregistered Functions = %T, want EmptyRegistry", b.Functions())
	}
	reg := rpc.NewStaticRegistry([]*rpc.Function{{Name: "ping"}})
	b.Register(reg)
	if b.Functions() != reg {
		t.Error("Functions should return the registered registry")
	}
	b.Register(nil)
	if _, ok := b.Functions().(rpc.EmptyRegistry); !ok {
		t.Error("Register(nil) should clear back to EmptyRegistry")
	}
}

// MapError turns nil into nil and a non-driver error into an internal error;
// the constraint branches are driven by real driver errors below.
func TestMapErrorNilAndNonDriver(t *testing.T) {
	b := &Backend{}
	if got := b.MapError(nil); got != nil {
		t.Errorf("MapError(nil) = %#v, want nil", got)
	}
	got := b.MapError(errors.New("disk gone"))
	if got == nil || got.Code != pgerr.CodeInternal || got.HTTPStatus != 500 {
		t.Fatalf("MapError(plain) = %#v, want internal/500", got)
	}
}

// The remaining MapError constraint arms (NOT NULL, CHECK, FOREIGN KEY) need a
// real driver error, so this seeds a table carrying each constraint and trips
// it, then asserts MapError reports the PostgreSQL SQLSTATE PostgREST would.
func TestMapErrorConstraintCodes(t *testing.T) {
	b := openConstraintDB(t)
	cases := []struct {
		name   string
		exec   string
		code   string
		status int
	}{
		{"not-null", `INSERT INTO widgets (id, name) VALUES (1, NULL)`, pgerr.CodeNotNullViolation, 400},
		{"check", `INSERT INTO widgets (id, name, qty) VALUES (2, 'a', -1)`, pgerr.CodeCheckViolation, 400},
		{"foreign-key", `INSERT INTO parts (id, widget_id) VALUES (1, 999)`, pgerr.CodeForeignKeyViolation, 409},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := b.DB().Exec(c.exec)
			if err == nil {
				t.Fatal("want a constraint error")
			}
			api := b.MapError(err)
			if api == nil || api.Code != c.code || api.HTTPStatus != c.status {
				t.Fatalf("MapError = %#v, want %s/%d", api, c.code, c.status)
			}
		})
	}
}

// openConstraintDB seeds a database whose tables carry a NOT NULL column, a
// CHECK constraint, and a foreign key, with FK enforcement on.
func openConstraintDB(t *testing.T) *Backend {
	t.Helper()
	b, err := Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	_, err = b.DB().Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE widgets (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			qty  INTEGER CHECK (qty >= 0)
		);
		CREATE TABLE parts (
			id        INTEGER PRIMARY KEY,
			widget_id INTEGER REFERENCES widgets(id)
		);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return b
}
