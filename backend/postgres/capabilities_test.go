package postgres

import (
	"testing"

	"github.com/tamnd/dbrest/backend"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in    string
		major int
		minor int
	}{
		{"16.3", 16, 3},
		{"15.6 (Debian 15.6-1.pgdg120+2)", 15, 6},
		{"9.6.24", 9, 6},
		{"  14.1  ", 14, 1},
		{"17", 17, 0},
		{"garbage", 0, 0},
		{"", 0, 0},
	}
	for _, c := range cases {
		v := ParseVersion(c.in)
		if v.Major != c.major || v.Minor != c.minor {
			t.Errorf("ParseVersion(%q) = %d.%d, want %d.%d", c.in, v.Major, v.Minor, c.major, c.minor)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	v := Version{Major: 15, Minor: 4}
	if !v.AtLeast(15, 4) {
		t.Error("15.4 should satisfy >= 15.4")
	}
	if !v.AtLeast(12, 0) {
		t.Error("15.4 should satisfy >= 12.0")
	}
	if !v.AtLeast(15, 0) {
		t.Error("15.4 should satisfy >= 15.0")
	}
	if v.AtLeast(15, 5) {
		t.Error("15.4 should not satisfy >= 15.5")
	}
	if v.AtLeast(16, 0) {
		t.Error("15.4 should not satisfy >= 16.0")
	}
}

// TestCapabilitiesNative checks the reference-oracle profile: on a supported
// server PostgreSQL is Native across the board, the security model is engine
// native (not emulated), and arrays and ranges are first-class.
func TestCapabilitiesNative(t *testing.T) {
	c := Capabilities(Version{Major: 16, Minor: 0})

	natives := map[string]backend.Tier{
		"Returning":       c.Returning,
		"Upsert":          c.Upsert,
		"NullsOrdering":   c.NullsOrdering,
		"JSONAssembly":    c.JSONAssembly,
		"IsDistinctFrom":  c.IsDistinctFrom,
		"SessionContext":  c.SessionContext,
		"Regex":           c.Regex,
		"ArrayRangeTypes": c.ArrayRangeTypes,
		"Aggregates":      c.Aggregates,
		"CountPlanned":    c.CountPlanned,
	}
	for name, tier := range natives {
		if tier != backend.Native {
			t.Errorf("%s = %s, want Native", name, tier)
		}
	}

	if !c.NativeRoles || !c.NativeRLS || !c.NativeRPC {
		t.Error("PostgreSQL has native roles, RLS, and functions")
	}
	if !c.UpsertConflictTarget {
		t.Error("PostgreSQL honors a conflict target")
	}
	if c.Transactions != backend.TxFull {
		t.Errorf("Transactions = %v, want TxFull", c.Transactions)
	}
	if c.FullText != backend.FTTSVector {
		t.Errorf("FullText = %v, want FTTSVector", c.FullText)
	}
	if c.Schemas != backend.SchemaNative {
		t.Errorf("Schemas = %v, want SchemaNative", c.Schemas)
	}
	if c.Embedding != backend.EmbedJoin {
		t.Errorf("Embedding = %v, want EmbedJoin", c.Embedding)
	}
	// Every filter operator passes straight through, including the array and
	// range operators that SQLite cannot serve.
	if c.Operator(int(0)) != backend.Native {
		t.Error("unspecified operators default to Native on PostgreSQL")
	}
}

// TestCapabilitiesOldServerDegrades checks the one version gate: a server below
// the supported floor drops the planned-count estimate to Best-effort.
func TestCapabilitiesOldServerDegrades(t *testing.T) {
	c := Capabilities(Version{Major: 10, Minor: 0})
	if c.CountPlanned != backend.BestEffort {
		t.Errorf("CountPlanned on PG 10 = %s, want BestEffort", c.CountPlanned)
	}
	// The rest of the matrix is unchanged: the gate is narrow.
	if c.ArrayRangeTypes != backend.Native || c.Regex != backend.Native {
		t.Error("only CountPlanned should degrade on an old server")
	}
}
