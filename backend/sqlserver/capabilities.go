package sqlserver

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend"
)

// Version is a parsed SQL Server product version. Major maps to the product
// year (13 = 2016, 14 = 2017, 15 = 2019, 16 = 2022, 17 = 2025). Azure SQL is
// effectively evergreen and ships several features (the JSON constructors,
// IS DISTINCT FROM, REGEXP_LIKE) ahead of the boxed product, so the flavor is
// kept alongside the numbers and treated as having the modern surface.
type Version struct {
	Major int
	Minor int
	Azure bool
}

// ParseVersion reads the major.minor out of a SERVERPROPERTY('ProductVersion')
// string such as "16.0.1000.6". The Azure flavor is not in that string (it comes
// from SERVERPROPERTY('EngineEdition') = 5), so callers set Azure separately;
// ParseVersion leaves it false. An unrecognized string yields a zero Version,
// which Capabilities treats as the conservative floor.
func ParseVersion(s string) Version {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	parts := strings.Split(s[:end], ".")
	var v Version
	if len(parts) > 0 {
		v.Major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		v.Minor, _ = strconv.Atoi(parts[1])
	}
	return v
}

// AtLeast reports whether the version is at least major.minor.
func (v Version) AtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// modern reports whether the server has the SQL Server 2022 / Azure surface: the
// JSON_OBJECT and JSON_ARRAYAGG constructors and the IS DISTINCT FROM operator.
func (v Version) modern() bool { return v.Azure || v.AtLeast(16, 0) }

// hasRegex reports whether the server has REGEXP_LIKE: SQL Server 2025 and
// current Azure SQL. A stock server below that has no regex predicate at all.
func (v Version) hasRegex() bool { return v.Azure || v.AtLeast(17, 0) }

// Capabilities reports the SQL Server feature tiers for a server version (spec
// 04/06). The security model is near-native: SQL Server has roles, row-level
// security, a native session-context store, and functions/procedures, so roles,
// RLS, RPC, and SessionContext are all native (unlike MySQL, where they are
// emulated in-app). The SQL surface carries the friction: NULL ordering and the
// upsert are emulated, RETURNING is the native OUTPUT clause, and a few features
// are version-gated.
//
// Version and flavor gates, re-verified in the conformance suite (spec 22):
//
//   - JSON assembly and IS DISTINCT FROM are native from SQL Server 2022; below
//     that the embed path uses a FOR JSON PATH subquery and IS DISTINCT FROM is
//     expanded, so both drop to Emulated.
//   - REGEXP_LIKE arrived in SQL Server 2025 / Azure SQL. On an older stock
//     server there is no regex predicate, so the operator is Unsupported and a
//     request using it is rejected with PGRST127 before lowering.
func Capabilities(v Version) backend.Capabilities {
	caps := backend.Capabilities{
		Returning:            backend.Native,   // OUTPUT INSERTED.* / DELETED.*
		Upsert:               backend.Emulated, // multi-statement UPDATE/INSERT in a tx
		UpsertConflictTarget: true,             // a named unique index can be targeted
		NullsOrdering:        backend.Emulated, // CASE WHEN ... IS NULL sort key
		JSONAssembly:         backend.Native,
		IsDistinctFrom:       backend.Native,
		Transactions:         backend.TxFull,
		NativeRoles:          true,
		NativeRLS:            true,
		SessionContext:       backend.Native, // SESSION_CONTEXT / sp_set_session_context
		NativeRPC:            true,
		Regex:                backend.Native,
		FullText:             backend.FTMSSQL,
		ArrayRangeTypes:      backend.Unsupported,
		Schemas:              backend.SchemaNative,
		Aggregates:           backend.Native,
		Embedding:            backend.EmbedJoin,
		CountPlanned:         backend.BestEffort,
	}
	if !v.modern() {
		caps.JSONAssembly = backend.Emulated
		caps.IsDistinctFrom = backend.Emulated
	}
	if !v.hasRegex() {
		caps.Regex = backend.Unsupported
	}
	return caps
}
