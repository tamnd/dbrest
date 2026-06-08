package mongo

import (
	"strconv"
	"strings"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
)

// Topology is the connected MongoDB deployment shape, which decides whether
// multi-document transactions are available (spec 07). A replica set or a
// sharded cluster supports them; a standalone mongod has only single-operation
// atomicity.
type Topology uint8

const (
	TopologyStandalone Topology = iota
	TopologyReplicaSet
	TopologySharded
)

// ParseTopology reads a topology kind reported by the driver. The driver names
// it "ReplicaSetWithPrimary", "Sharded", "Single", and so on; this recognizes
// the replica-set and sharded families and treats anything else as standalone,
// the conservative choice that degrades transactions rather than assuming them.
func ParseTopology(s string) Topology {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "replica"), strings.Contains(l, "replset"), l == "rs":
		return TopologyReplicaSet
	case strings.Contains(l, "shard"), strings.Contains(l, "mongos"):
		return TopologySharded
	default:
		return TopologyStandalone
	}
}

// Version is a parsed MongoDB server version. Only the major and minor gate a
// dbrest feature (transactions on a replica set need 4.0, on a sharded cluster
// 4.2), but both are kept so a caller can log the full version.
type Version struct {
	Major int
	Minor int
}

// ParseVersion reads the major.minor out of a version string such as "7.0.5" or
// "6.0". An unrecognized string yields a zero Version, which Capabilities treats
// as too old to assume the transaction floor, degrading rather than guessing.
func ParseVersion(s string) Version {
	s = strings.TrimSpace(s)
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

// Capabilities reports the MongoDB feature tiers for a server version and
// deployment topology (spec 04/07). MongoDB is the engine furthest from
// PostgreSQL: the data plane passes basic filtering, projection, sort/skip/limit,
// counts, writes, and $lookup embedding straight through, but the whole security
// model is emulated app-side and the array, range, and stored-procedure features
// are Unsupported.
//
//   - Transactions resolve from the topology: a replica set (4.0+) or a sharded
//     cluster (4.2+) runs the per-request transaction inside a session; a
//     standalone mongod has no multi-document transaction, so the tier is TxNone
//     and a request asking for rollback semantics it cannot get is rejected
//     rather than partially applied.
//   - Returning is Emulated (FindOneAndUpdate or a re-query), NULLS ordering and
//     full text and the LIKE-family regex are Best-effort, and roles, RLS,
//     session context, and casts are Emulated app-side.
//   - The array and range operators and types are Unsupported and return
//     PGRST127 naming the operator and this backend.
func Capabilities(v Version, topo Topology) backend.Capabilities {
	return backend.Capabilities{
		Returning:            backend.Emulated,
		Upsert:               backend.Native,
		UpsertConflictTarget: false,
		NullsOrdering:        backend.BestEffort,
		JSONAssembly:         backend.Native,
		IsDistinctFrom:       backend.Emulated,
		Transactions:         transactionTier(v, topo),
		NativeRoles:          false,
		NativeRLS:            false,
		SessionContext:       backend.Emulated,
		NativeRPC:            false,
		Regex:                backend.BestEffort,
		FullText:             backend.FTMongo,
		ArrayRangeTypes:      backend.Unsupported,
		Schemas:              backend.SchemaNative,
		Aggregates:           backend.BestEffort,
		Embedding:            backend.EmbedPipeline,
		CountPlanned:         backend.BestEffort,
		Operators: map[int]backend.Tier{
			int(ir.OpLike):       backend.BestEffort,
			int(ir.OpILike):      backend.BestEffort,
			int(ir.OpMatch):      backend.BestEffort,
			int(ir.OpIMatch):     backend.BestEffort,
			int(ir.OpIs):         backend.BestEffort,
			int(ir.OpFTS):        backend.BestEffort,
			int(ir.OpIsDistinct): backend.Emulated,
			int(ir.OpContains):   backend.Unsupported,
			int(ir.OpContained):  backend.Unsupported,
			int(ir.OpOverlap):    backend.Unsupported,
			int(ir.OpRangeSL):    backend.Unsupported,
			int(ir.OpRangeSR):    backend.Unsupported,
			int(ir.OpRangeNXR):   backend.Unsupported,
			int(ir.OpRangeNXL):   backend.Unsupported,
			int(ir.OpRangeAdj):   backend.Unsupported,
		},
	}
}

// transactionTier resolves the transaction grade from the topology and version.
// A replica set needs 4.0, a sharded cluster 4.2; a standalone mongod has none.
func transactionTier(v Version, topo Topology) backend.TxTier {
	switch topo {
	case TopologyReplicaSet:
		if v.AtLeast(4, 0) {
			return backend.TxFull
		}
	case TopologySharded:
		if v.AtLeast(4, 2) {
			return backend.TxFull
		}
	}
	return backend.TxNone
}
