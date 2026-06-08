package postgres

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend"
)

// Version is a parsed PostgreSQL server version. Only the major number gates any
// dbrest feature, but the minor is kept so a caller can log the full version.
type Version struct {
	Major int
	Minor int
}

// ParseVersion reads the major.minor out of a server_version string such as
// "16.3", "15.6 (Debian ...)", or the older "9.6.24" three-part form. An
// unrecognized string yields a zero Version, which Capabilities treats as "too
// old to assume anything", degrading the version-gated features rather than
// guessing they are present.
func ParseVersion(s string) Version {
	s = strings.TrimSpace(s)
	// Cut at the first space so a trailing build tag ("(Debian ...)") is ignored.
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
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

// Capabilities reports the PostgreSQL feature tiers for a given server version
// (spec 04/06). PostgreSQL is the reference oracle, so nearly everything is
// Native and matches PostgREST exactly: roles, RLS, and the session-context
// store are all engine-native, the security model is not emulated, every filter
// operator passes straight through, and arrays and ranges are first-class.
//
// Two facts are version-gated and re-verified in the conformance suite (spec 22):
//
//   - IS DISTINCT FROM has been native since PostgreSQL 7.x, so it is Native on
//     every server dbrest connects to; it is listed here for symmetry with the
//     other SQL backends, which gate it.
//   - The minimum supported server is 12 (the oldest PostgreSQL with community
//     support during this project and the floor PostgREST itself targets). On an
//     older server the count-estimate path and a few JSON niceties are not
//     assumed; CountPlanned drops to BestEffort so the planner does not lean on
//     an EXPLAIN-derived estimate it cannot trust.
func Capabilities(v Version) backend.Capabilities {
	caps := backend.Capabilities{
		Returning:            backend.Native,
		Upsert:               backend.Native,
		UpsertConflictTarget: true,
		NullsOrdering:        backend.Native,
		JSONAssembly:         backend.Native,
		IsDistinctFrom:       backend.Native,
		Transactions:         backend.TxFull,
		NativeRoles:          true,
		NativeRLS:            true,
		SessionContext:       backend.Native,
		NativeRPC:            true,
		Regex:                backend.Native,
		FullText:             backend.FTTSVector,
		ArrayRangeTypes:      backend.Native,
		Schemas:              backend.SchemaNative,
		Aggregates:           backend.Native,
		Embedding:            backend.EmbedJoin,
		CountPlanned:         backend.Native,
	}
	// The planned-count strategy reads an estimate from the planner statistics
	// (EXPLAIN / reltuples). On a server older than the supported floor, do not
	// promise an estimate the version may render differently; serve the exact
	// count path instead by grading the estimate Best-effort.
	if !v.AtLeast(12, 0) {
		caps.CountPlanned = backend.BestEffort
	}
	return caps
}
