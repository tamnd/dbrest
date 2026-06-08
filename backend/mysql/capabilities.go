package mysql

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend"
)

// Version is a parsed MySQL/MariaDB server version. MariaDB reports itself with
// a "-MariaDB" suffix and its own version line (10.x/11.x), which a few gates
// read, so the flavor is kept alongside the numbers.
type Version struct {
	Major   int
	Minor   int
	MariaDB bool
}

// ParseVersion reads the major.minor (and the MariaDB flavor) out of a
// version() string such as "8.0.36", "8.0.36-0ubuntu0.22.04.1", or
// "10.11.6-MariaDB-1:10.11.6+maria~ubu2204". An unrecognized string yields a
// zero Version, which Capabilities treats as "assume the conservative floor".
func ParseVersion(s string) Version {
	s = strings.TrimSpace(s)
	v := Version{MariaDB: strings.Contains(strings.ToLower(s), "mariadb")}
	// Keep only the leading numeric dotted run; a suffix like "-0ubuntu..." or
	// "-MariaDB" starts at the first non-digit, non-dot byte.
	end := 0
	for end < len(s) && (s[end] == '.' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	parts := strings.Split(s[:end], ".")
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

// Capabilities reports the MySQL/MariaDB feature tiers for a server version
// (spec 04/06). The PostgreSQL security model (roles, RLS, the request-context
// store) is emulated in-app, since MySQL has no GUC-in-SQL and a different role
// model; most SQL features are native or cleanly emulated.
//
// Version and flavor gates, re-verified in the conformance suite (spec 22):
//
//   - RETURNING: MySQL has none, so a representation re-selects the written keys
//     in the same transaction (Emulated). MariaDB 10.5+ has INSERT/DELETE
//     RETURNING, which lifts the tier to Native for those statements.
//   - REGEXP_LIKE with a match-control argument arrived in MySQL 8.0. On an
//     older MySQL the case-insensitive form is not expressible the same way, so
//     regex drops to Best-effort. MariaDB has REGEXP_LIKE throughout the
//     supported range.
func Capabilities(v Version) backend.Capabilities {
	caps := backend.Capabilities{
		Returning:            backend.Emulated,
		Upsert:               backend.Native,
		UpsertConflictTarget: false, // ON DUPLICATE KEY fires on any unique key
		NullsOrdering:        backend.Emulated,
		JSONAssembly:         backend.Native,
		IsDistinctFrom:       backend.Native, // NOT (a <=> b)
		Transactions:         backend.TxFull,
		NativeRoles:          false,
		NativeRLS:            false,
		SessionContext:       backend.Emulated,
		NativeRPC:            false,
		Regex:                backend.Native,
		FullText:             backend.FTMySQL,
		ArrayRangeTypes:      backend.Unsupported,
		Schemas:              backend.SchemaNative, // a MySQL database is the schema
		Aggregates:           backend.Native,
		Embedding:            backend.EmbedJoin,
		CountPlanned:         backend.BestEffort, // EXPLAIN row estimate is approximate
	}
	if v.MariaDB && v.AtLeast(10, 5) {
		// MariaDB returns written rows for INSERT/DELETE; UPDATE RETURNING is later
		// and re-verified in spec 22, so the tier is Native for the common case.
		caps.Returning = backend.Native
	}
	if !v.MariaDB && !v.AtLeast(8, 0) {
		// Pre-8.0 MySQL has no REGEXP_LIKE match-control argument.
		caps.Regex = backend.BestEffort
	}
	return caps
}
