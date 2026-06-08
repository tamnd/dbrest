package sqlserver

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
		{"16.0.1000.6", 16, 0},
		{"15.0.4322.2", 15, 0},
		{"13.0.5026.0", 13, 0},
		{"  17.0  ", 17, 0},
		{"garbage", 0, 0},
		{"", 0, 0},
	}
	for _, c := range cases {
		v := ParseVersion(c.in)
		if v.Major != c.major || v.Minor != c.minor {
			t.Errorf("ParseVersion(%q) = %d.%d, want %d.%d", c.in, v.Major, v.Minor, c.major, c.minor)
		}
		if v.Azure {
			t.Errorf("ParseVersion(%q) should leave Azure false", c.in)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	v := Version{Major: 16, Minor: 0}
	if !v.AtLeast(16, 0) || !v.AtLeast(13, 0) {
		t.Error("16.0 should satisfy >= 16.0 and >= 13.0")
	}
	if v.AtLeast(16, 1) || v.AtLeast(17, 0) {
		t.Error("16.0 should not satisfy >= 16.1 or >= 17.0")
	}
}

// TestCapabilitiesNativeSecurity checks the near-native profile: SQL Server has
// real roles, RLS, a session-context store, and functions, so unlike MySQL those
// are native rather than emulated.
func TestCapabilitiesNativeSecurity(t *testing.T) {
	c := Capabilities(Version{Major: 16, Minor: 0})
	if !c.NativeRoles || !c.NativeRLS || !c.NativeRPC {
		t.Error("SQL Server has native roles, RLS, and functions")
	}
	if c.SessionContext != backend.Native {
		t.Errorf("SessionContext = %s, want Native", c.SessionContext)
	}
	if c.Returning != backend.Native {
		t.Errorf("Returning = %s, want Native (OUTPUT)", c.Returning)
	}
	if c.Upsert != backend.Emulated {
		t.Errorf("Upsert = %s, want Emulated (multi-statement)", c.Upsert)
	}
	if !c.UpsertConflictTarget {
		t.Error("a named unique index can be targeted")
	}
	if c.NullsOrdering != backend.Emulated {
		t.Errorf("NullsOrdering = %s, want Emulated (CASE)", c.NullsOrdering)
	}
	if c.FullText != backend.FTMSSQL {
		t.Errorf("FullText = %v, want FTMSSQL", c.FullText)
	}
	if c.ArrayRangeTypes != backend.Unsupported {
		t.Errorf("ArrayRangeTypes = %s, want Unsupported", c.ArrayRangeTypes)
	}
}

// TestModernGate checks the SQL Server 2022 gate: JSON assembly and
// IS DISTINCT FROM are native from 2022 and emulated below.
func TestModernGate(t *testing.T) {
	new2022 := Capabilities(Version{Major: 16, Minor: 0})
	if new2022.JSONAssembly != backend.Native || new2022.IsDistinctFrom != backend.Native {
		t.Error("SQL Server 2022 has native JSON assembly and IS DISTINCT FROM")
	}
	old2019 := Capabilities(Version{Major: 15, Minor: 0})
	if old2019.JSONAssembly != backend.Emulated || old2019.IsDistinctFrom != backend.Emulated {
		t.Error("SQL Server 2019 emulates JSON assembly and IS DISTINCT FROM")
	}
	// Azure is evergreen: it has the modern surface regardless of the major.
	azure := Capabilities(Version{Major: 12, Minor: 0, Azure: true})
	if azure.JSONAssembly != backend.Native {
		t.Error("Azure SQL has native JSON assembly")
	}
}

// TestRegexGate checks the SQL Server 2025 gate: REGEXP_LIKE is Native from 2025
// and Azure, Unsupported on an older stock server.
func TestRegexGate(t *testing.T) {
	new2025 := Capabilities(Version{Major: 17, Minor: 0})
	if new2025.Regex != backend.Native {
		t.Errorf("SQL Server 2025 Regex = %s, want Native", new2025.Regex)
	}
	stock := Capabilities(Version{Major: 16, Minor: 0})
	if stock.Regex != backend.Unsupported {
		t.Errorf("SQL Server 2022 Regex = %s, want Unsupported", stock.Regex)
	}
	azure := Capabilities(Version{Major: 12, Minor: 0, Azure: true})
	if azure.Regex != backend.Native {
		t.Errorf("Azure SQL Regex = %s, want Native", azure.Regex)
	}
}
