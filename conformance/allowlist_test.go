package conformance

import (
	"testing"

	"github.com/tamnd/dbrest/backend"
)

func TestAllowlistMatrixConsistent(t *testing.T) {
	al := NewAllowlist("sqlite", AllowEntry{
		Feature: "count-planned", Backend: "sqlite", Tier: "B",
		Request: "Prefer: count=planned", Expected: "total within tolerance",
		Reason: "no planner estimate", MatrixID: "count=planned",
	})
	features := map[string]backend.Tier{"count-planned": backend.BestEffort}
	if err := al.CheckMatrix(features); err != nil {
		t.Errorf("expected consistent allowlist, got %v", err)
	}
}

func TestAllowlistMatrixMismatchIsError(t *testing.T) {
	al := NewAllowlist("sqlite", AllowEntry{
		Feature: "count-planned", Backend: "sqlite", Tier: "N",
	})
	features := map[string]backend.Tier{"count-planned": backend.BestEffort}
	if err := al.CheckMatrix(features); err == nil {
		t.Error("an allowlist tier that disagrees with the matrix must be a build failure")
	}
}

func TestAllowlistUnknownFeatureIsError(t *testing.T) {
	al := NewAllowlist("sqlite", AllowEntry{Feature: "ghost", Tier: "B"})
	if err := al.CheckMatrix(map[string]backend.Tier{}); err == nil {
		t.Error("an allowlist feature absent from the matrix must be an error")
	}
}

func TestAllowlistEntryLookup(t *testing.T) {
	al := NewAllowlist("sqlite", AllowEntry{Feature: "fts", Tier: "B"})
	if _, ok := al.Entry("fts"); !ok {
		t.Error("expected to find the fts entry")
	}
	if _, ok := al.Entry("missing"); ok {
		t.Error("did not expect a missing entry")
	}
	var nilList *Allowlist
	if _, ok := nilList.Entry("fts"); ok {
		t.Error("a nil allowlist has no entries")
	}
}
