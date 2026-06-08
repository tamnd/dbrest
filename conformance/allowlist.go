package conformance

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tamnd/dbrest/backend"
)

// AllowEntry is one documented divergence: a feature on a backend whose tier is
// not Native, the request that triggers it, what dbrest does instead of matching
// the golden PostgreSQL output, and why. The allowlist is the ledger of every
// such divergence; a divergence that is not listed is a test failure, and a
// listed tier that disagrees with the live capability matrix is a build failure
// (spec 22 section 3 and 4).
type AllowEntry struct {
	Feature  string `json:"feature"`
	Backend  string `json:"backend"`
	Tier     string `json:"tier"` // N/E/B/U, as the matrix renders it
	Request  string `json:"request"`
	Expected string `json:"expected"`
	Reason   string `json:"reason"`
	MatrixID string `json:"matrix_id,omitempty"`
}

// Allowlist is the per-backend set of documented divergences, keyed by feature.
// It is loaded from a checked-in data file (or built in code) and reconciled
// against the live capability matrix before a run, so it cannot grow silently or
// claim a tier the engine does not resolve.
type Allowlist struct {
	Backend string
	entries map[string]AllowEntry // keyed by feature
}

// LoadAllowlist reads the allowlist data file for one backend.
func LoadAllowlist(path string) (*Allowlist, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("conformance: reading allowlist %s: %w", path, err)
	}
	var raw struct {
		Backend string       `json:"backend"`
		Entries []AllowEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("conformance: parsing allowlist %s: %w", path, err)
	}
	al := &Allowlist{Backend: raw.Backend, entries: map[string]AllowEntry{}}
	for _, e := range raw.Entries {
		if e.Feature == "" {
			return nil, fmt.Errorf("conformance: allowlist %s: entry with no feature", path)
		}
		al.entries[e.Feature] = e
	}
	return al, nil
}

// NewAllowlist builds an allowlist in memory, used by tests and by a backend
// whose divergences are declared in code.
func NewAllowlist(backendName string, entries ...AllowEntry) *Allowlist {
	al := &Allowlist{Backend: backendName, entries: map[string]AllowEntry{}}
	for _, e := range entries {
		al.entries[e.Feature] = e
	}
	return al
}

// Entry returns the allowlist entry for a feature, if any.
func (a *Allowlist) Entry(feature string) (AllowEntry, bool) {
	if a == nil {
		return AllowEntry{}, false
	}
	e, ok := a.entries[feature]
	return e, ok
}

// CheckMatrix reconciles every allowlist entry against the live capability
// matrix: the tier the entry states must equal the tier the backend's
// Capabilities resolves for that feature. A mismatch is returned as an error, so
// the build fails rather than letting the allowlist drift into a claim the
// engine does not honor (spec 22 section 4). features maps a feature label to
// the tier it currently resolves to on this backend.
func (a *Allowlist) CheckMatrix(features map[string]backend.Tier) error {
	if a == nil {
		return nil
	}
	for feat, e := range a.entries {
		got, ok := features[feat]
		if !ok {
			return fmt.Errorf("conformance: allowlist names feature %q with no tier in the capability matrix", feat)
		}
		if got.String() != e.Tier {
			return fmt.Errorf("conformance: allowlist tier for %q is %q but the matrix resolves %q", feat, e.Tier, got.String())
		}
	}
	return nil
}
