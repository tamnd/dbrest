package mongo

import (
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
)

func TestParseTopology(t *testing.T) {
	cases := map[string]Topology{
		"ReplicaSetWithPrimary": TopologyReplicaSet,
		"ReplicaSetNoPrimary":   TopologyReplicaSet,
		"rs":                    TopologyReplicaSet,
		"Sharded":               TopologySharded,
		"mongos":                TopologySharded,
		"Single":                TopologyStandalone,
		"":                      TopologyStandalone,
	}
	for in, want := range cases {
		if got := ParseTopology(in); got != want {
			t.Errorf("ParseTopology(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in           string
		major, minor int
	}{
		{"7.0.5", 7, 0},
		{"6.0", 6, 0},
		{"4.2.1 (build)", 4, 2},
		{"", 0, 0},
	}
	for _, c := range cases {
		v := ParseVersion(c.in)
		if v.Major != c.major || v.Minor != c.minor {
			t.Errorf("ParseVersion(%q) = %d.%d, want %d.%d", c.in, v.Major, v.Minor, c.major, c.minor)
		}
	}
}

func TestTransactionTier(t *testing.T) {
	cases := []struct {
		ver  string
		topo Topology
		want backend.TxTier
	}{
		// A replica set needs 4.0; a sharded cluster needs 4.2.
		{"7.0", TopologyReplicaSet, backend.TxFull},
		{"4.0", TopologyReplicaSet, backend.TxFull},
		{"3.6", TopologyReplicaSet, backend.TxNone},
		{"7.0", TopologySharded, backend.TxFull},
		{"4.2", TopologySharded, backend.TxFull},
		{"4.0", TopologySharded, backend.TxNone},
		// A standalone mongod has no multi-document transaction at any version.
		{"7.0", TopologyStandalone, backend.TxNone},
	}
	for _, c := range cases {
		caps := Capabilities(ParseVersion(c.ver), c.topo)
		if caps.Transactions != c.want {
			t.Errorf("Transactions(%s, %d) = %d, want %d", c.ver, c.topo, caps.Transactions, c.want)
		}
	}
}

func TestCapabilitiesShape(t *testing.T) {
	caps := Capabilities(ParseVersion("7.0"), TopologyReplicaSet)
	if caps.FullText != backend.FTMongo {
		t.Errorf("FullText = %d, want FTMongo", caps.FullText)
	}
	if caps.Embedding != backend.EmbedPipeline {
		t.Errorf("Embedding = %d, want EmbedPipeline", caps.Embedding)
	}
	if caps.JSONAssembly != backend.Native {
		t.Errorf("JSONAssembly = %d, want Native", caps.JSONAssembly)
	}
	// The security model is emulated app-side: no native roles, RLS, or RPC.
	if caps.NativeRoles || caps.NativeRLS || caps.NativeRPC {
		t.Errorf("native roles/rls/rpc = %v/%v/%v, want all false", caps.NativeRoles, caps.NativeRLS, caps.NativeRPC)
	}
	if caps.ArrayRangeTypes != backend.Unsupported {
		t.Errorf("ArrayRangeTypes = %d, want Unsupported", caps.ArrayRangeTypes)
	}
}

func TestCapabilitiesOperators(t *testing.T) {
	caps := Capabilities(ParseVersion("7.0"), TopologyReplicaSet)
	// The array and range operators are Unsupported; the planner reads this and
	// raises PGRST127 before lowering.
	for _, op := range []ir.Op{ir.OpContains, ir.OpContained, ir.OpOverlap,
		ir.OpRangeSL, ir.OpRangeSR, ir.OpRangeNXR, ir.OpRangeNXL, ir.OpRangeAdj} {
		if got := caps.Operator(int(op)); got != backend.Unsupported {
			t.Errorf("operator %d = %v, want Unsupported", op, got)
		}
	}
	// The regex-family operators are Best-effort.
	for _, op := range []ir.Op{ir.OpLike, ir.OpILike, ir.OpMatch, ir.OpIMatch} {
		if got := caps.Operator(int(op)); got != backend.BestEffort {
			t.Errorf("operator %d = %v, want BestEffort", op, got)
		}
	}
	// A plain equality passes straight through as Native.
	if got := caps.Operator(int(ir.OpEq)); got != backend.Native {
		t.Errorf("eq = %v, want Native", got)
	}
}
