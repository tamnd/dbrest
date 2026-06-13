package httpapi

import (
	"sort"
	"strconv"
	"strings"
)

// The response media types dbrest can produce, in preference order. A wildcard
// range (*/*, application/*, text/*) resolves to the first entry it matches, so
// this order also decides the default for each family. application/json is the
// overall default. See spec 17-content-negotiation.
const (
	mediaJSON   = "application/json"
	mediaArray  = "application/vnd.pgrst.array+json"
	mediaObject = "application/vnd.pgrst.object+json"
	mediaPlan   = "application/vnd.pgrst.plan+json"
	mediaCSV    = "text/csv"
	mediaOctet  = "application/octet-stream"
	mediaText   = "text/plain"
)

var supportedMedia = []string{mediaJSON, mediaArray, mediaObject, mediaPlan, mediaCSV, mediaOctet, mediaText}

// The internal media keys for the nulls=stripped variants of the vendor array
// and object types. They are not real Accept literals; negotiate returns them so
// the render path knows to drop null-valued keys and echo the parameterized
// Content-Type.
const (
	mediaArrayStripped  = "application/vnd.pgrst.array+json;nulls=stripped"
	mediaObjectStripped = "application/vnd.pgrst.object+json;nulls=stripped"
)

// singularMedia reports whether a negotiated media type asks for a single object
// (the object vendor type or its nulls=stripped variant).
func singularMedia(media string) bool {
	return media == mediaObject || media == mediaObjectStripped
}

// mediaRange is one parsed entry of an Accept header: a type/subtype pair, its
// quality value, its position in the header for stable tie-breaking, and whether
// it carried the nulls=stripped parameter.
type mediaRange struct {
	typ        string
	sub        string
	q          float64
	order      int
	stripNulls bool
}

// parseAccept parses the Accept header values into media ranges sorted by
// descending quality, preserving header order within a quality class.
func parseAccept(headers []string) []mediaRange {
	var ranges []mediaRange
	n := 0
	for _, h := range headers {
		for part := range strings.SplitSeq(h, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			segs := strings.Split(part, ";")
			typ, sub, ok := strings.Cut(strings.TrimSpace(segs[0]), "/")
			if !ok {
				continue
			}
			q := 1.0
			stripNulls := false
			for _, p := range segs[1:] {
				p = strings.TrimSpace(p)
				if v, ok := strings.CutPrefix(p, "q="); ok {
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						q = f
					}
				}
				if v, ok := strings.CutPrefix(strings.ToLower(p), "nulls="); ok && strings.TrimSpace(v) == "stripped" {
					stripNulls = true
				}
			}
			ranges = append(ranges, mediaRange{strings.ToLower(typ), strings.ToLower(sub), q, n, stripNulls})
			n++
		}
	}
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].q > ranges[j].q })
	return ranges
}

// planAnalyze reports whether the Accept header for vnd.pgrst.plan+json carries
// "options=analyze", which asks for EXPLAIN ANALYZE rather than plain EXPLAIN.
func planAnalyze(headers []string) bool {
	for _, h := range headers {
		for part := range strings.SplitSeq(h, ",") {
			part = strings.TrimSpace(part)
			segs := strings.Split(part, ";")
			typ, sub, ok := strings.Cut(strings.TrimSpace(segs[0]), "/")
			if !ok {
				continue
			}
			if strings.ToLower(typ)+"/"+strings.ToLower(sub) != "application/vnd.pgrst.plan+json" {
				continue
			}
			for _, p := range segs[1:] {
				p = strings.TrimSpace(p)
				if v, ok := strings.CutPrefix(strings.ToLower(p), "options="); ok {
					if strings.Contains(v, "analyze") {
						return true
					}
				}
			}
		}
	}
	return false
}

// vendorSynonym maps the suffixless PostgREST vendor spellings to their +json
// forms, which PostgREST accepts as synonyms. Any other type passes through
// unchanged.
func vendorSynonym(full string) string {
	switch full {
	case "application/vnd.pgrst.array":
		return mediaArray
	case "application/vnd.pgrst.object":
		return mediaObject
	default:
		return full
	}
}

// negotiate picks the best supported response media type for the Accept header.
// An absent or fully wildcard Accept yields application/json. The second return
// is false when no listed media type can be produced, which the caller turns
// into a 406.
func negotiate(headers []string) (string, bool) {
	ranges := parseAccept(headers)
	if len(ranges) == 0 {
		return mediaJSON, true
	}
	for _, r := range ranges {
		if r.q <= 0 {
			// q=0 explicitly refuses this type; skip it.
			continue
		}
		switch {
		case r.typ == "*" && r.sub == "*":
			return mediaJSON, true
		case r.sub == "*":
			for _, m := range supportedMedia {
				if strings.HasPrefix(m, r.typ+"/") {
					return m, true
				}
			}
		default:
			full := vendorSynonym(r.typ + "/" + r.sub)
			for _, m := range supportedMedia {
				if m == full {
					// nulls=stripped applies only to the vendor array and object
					// types; on plain application/json the parameter is ignored,
					// matching PostgREST.
					if r.stripNulls {
						switch m {
						case mediaArray:
							return mediaArrayStripped, true
						case mediaObject:
							return mediaObjectStripped, true
						}
					}
					return m, true
				}
			}
		}
	}
	return "", false
}
