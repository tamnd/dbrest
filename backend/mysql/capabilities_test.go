package mysql

import (
	"testing"

	"github.com/tamnd/dbrest/backend"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in      string
		major   int
		minor   int
		mariadb bool
	}{
		{"8.0.36", 8, 0, false},
		{"8.0.36-0ubuntu0.22.04.1", 8, 0, false},
		{"5.7.44", 5, 7, false},
		{"10.11.6-MariaDB-1:10.11.6+maria~ubu2204", 10, 11, true},
		{"11.4.2-MariaDB", 11, 4, true},
		{"  8.4  ", 8, 4, false},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		v := ParseVersion(c.in)
		if v.Major != c.major || v.Minor != c.minor || v.MariaDB != c.mariadb {
			t.Errorf("ParseVersion(%q) = %d.%d mariadb=%v, want %d.%d mariadb=%v",
				c.in, v.Major, v.Minor, v.MariaDB, c.major, c.minor, c.mariadb)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	v := Version{Major: 8, Minor: 0}
	if !v.AtLeast(8, 0) || !v.AtLeast(5, 7) {
		t.Error("8.0 should satisfy >= 8.0 and >= 5.7")
	}
	if v.AtLeast(8, 1) || v.AtLeast(9, 0) {
		t.Error("8.0 should not satisfy >= 8.1 or >= 9.0")
	}
}

// TestCapabilitiesMySQL8 pins the modern-MySQL profile: native upsert and JSON,
// emulated RETURNING and the security model, and the conflict target unavailable
// because ON DUPLICATE KEY cannot name one.
func TestCapabilitiesMySQL8(t *testing.T) {
	c := Capabilities(Version{Major: 8, Minor: 0})

	if c.Upsert != backend.Native || c.JSONAssembly != backend.Native || c.Regex != backend.Native {
		t.Error("MySQL 8 has native upsert, JSON assembly, and regex")
	}
	if c.Returning != backend.Emulated {
		t.Errorf("Returning = %s, want Emulated", c.Returning)
	}
	if c.NullsOrdering != backend.Emulated {
		t.Errorf("NullsOrdering = %s, want Emulated", c.NullsOrdering)
	}
	if c.SessionContext != backend.Emulated {
		t.Errorf("SessionContext = %s, want Emulated", c.SessionContext)
	}
	if c.UpsertConflictTarget {
		t.Error("ON DUPLICATE KEY cannot name a conflict target")
	}
	if c.NativeRoles || c.NativeRLS || c.NativeRPC {
		t.Error("the PostgreSQL security model is emulated, not native, on MySQL")
	}
	if c.FullText != backend.FTMySQL {
		t.Errorf("FullText = %v, want FTMySQL", c.FullText)
	}
	if c.ArrayRangeTypes != backend.Unsupported {
		t.Errorf("ArrayRangeTypes = %s, want Unsupported", c.ArrayRangeTypes)
	}
	if c.Schemas != backend.SchemaNative {
		t.Errorf("Schemas = %v, want SchemaNative", c.Schemas)
	}
	if c.CountPlanned != backend.BestEffort {
		t.Errorf("CountPlanned = %s, want BestEffort", c.CountPlanned)
	}
}

// TestMariaDBReturning checks the flavor gate: MariaDB 10.5+ returns written
// rows, so RETURNING lifts to Native.
func TestMariaDBReturning(t *testing.T) {
	c := Capabilities(Version{Major: 10, Minor: 5, MariaDB: true})
	if c.Returning != backend.Native {
		t.Errorf("MariaDB 10.5 Returning = %s, want Native", c.Returning)
	}
	// An older MariaDB stays on the emulated re-select.
	old := Capabilities(Version{Major: 10, Minor: 3, MariaDB: true})
	if old.Returning != backend.Emulated {
		t.Errorf("MariaDB 10.3 Returning = %s, want Emulated", old.Returning)
	}
}

// TestOldMySQLRegexDegrades checks the version gate: pre-8.0 MySQL has no
// REGEXP_LIKE match-control argument, so regex drops to Best-effort.
func TestOldMySQLRegexDegrades(t *testing.T) {
	c := Capabilities(Version{Major: 5, Minor: 7})
	if c.Regex != backend.BestEffort {
		t.Errorf("MySQL 5.7 Regex = %s, want BestEffort", c.Regex)
	}
	// MariaDB keeps REGEXP_LIKE across its supported range.
	maria := Capabilities(Version{Major: 10, Minor: 3, MariaDB: true})
	if maria.Regex != backend.Native {
		t.Errorf("MariaDB Regex = %s, want Native", maria.Regex)
	}
}
