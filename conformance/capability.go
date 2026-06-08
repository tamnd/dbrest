package conformance

import (
	"encoding/json"
	"net/http"

	"github.com/tamnd/dbrest/backend"
)

// Probe drives one request that depends on a single capability and resolves the
// tier that capability currently holds on the backend. The check asserts the two
// agree: an Unsupported tier must return PGRST127, and any serving tier (Native,
// Emulated, Best-effort) must not. This is the spec's "the matrix is executable"
// and "Capabilities cannot lie" rules turned into a test (spec 22 section 4, 5).
type Probe struct {
	Feature string
	Request Request
	Tier    func(backend.Capabilities) backend.Tier
}

// CapResult is the outcome of one probe: the feature, the resolved tier, whether
// the response was a PGRST127, and whether that is consistent with the tier.
type CapResult struct {
	Feature     string
	Tier        backend.Tier
	GotPGRST127 bool
	Consistent  bool
}

// CheckCapabilities runs every probe against the in-process handler and checks
// each response against the tier the capability resolves. A serving tier that
// returns PGRST127, or an Unsupported tier that does not, is inconsistent: the
// declared capability and the engine's real behavior have diverged.
func CheckCapabilities(handler http.Handler, caps backend.Capabilities, probes []Probe) []CapResult {
	out := make([]CapResult, 0, len(probes))
	for _, p := range probes {
		tier := p.Tier(caps)
		resp := issue(handler, p.Request)
		got127 := isPGRST127(resp)
		consistent := got127 == (tier == backend.Unsupported)
		out = append(out, CapResult{
			Feature:     p.Feature,
			Tier:        tier,
			GotPGRST127: got127,
			Consistent:  consistent,
		})
	}
	return out
}

// CapabilitiesConsistent reports whether every probe agreed with its tier.
func CapabilitiesConsistent(results []CapResult) bool {
	for _, r := range results {
		if !r.Consistent {
			return false
		}
	}
	return true
}

// isPGRST127 reports whether a response is the unsupported-feature error: status
// 400 with the PGRST127 code in the envelope.
func isPGRST127(resp Response) bool {
	if resp.Status != http.StatusBadRequest {
		return false
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &env); err != nil {
		return false
	}
	return env.Code == "PGRST127"
}

// DefaultProbes is the relational probe set over the films fixture: a regex
// filter (Regex), a full-text filter (FullText), and an array-contains filter
// (ArrayRangeTypes). It exercises both branches, since on SQLite the first two
// serve and the last is Unsupported and must return PGRST127.
func DefaultProbes() []Probe {
	return []Probe{
		{
			Feature: "regex",
			Request: Request{Method: http.MethodGet, Path: "/films", Query: "title=match.^Bl"},
			Tier:    func(c backend.Capabilities) backend.Tier { return c.Regex },
		},
		{
			Feature: "fts",
			Request: Request{Method: http.MethodGet, Path: "/films", Query: "title=fts.metropolis"},
			Tier:    fullTextTier,
		},
		{
			Feature: "array-contains",
			Request: Request{Method: http.MethodGet, Path: "/films", Query: "title=cs.{a}"},
			Tier:    func(c backend.Capabilities) backend.Tier { return c.ArrayRangeTypes },
		},
	}
}

// fullTextTier maps the full-text engine flavor to a serving or Unsupported
// tier: no flavor means the feature is unsupported, any flavor means it serves.
func fullTextTier(c backend.Capabilities) backend.Tier {
	if c.FullText == backend.FTNone {
		return backend.Unsupported
	}
	return backend.Native
}
