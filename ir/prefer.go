package ir

import "strings"

// Handling is the Prefer: handling= mode for unrecognized parameters/preferences.
type Handling uint8

const (
	HandlingLenient Handling = iota // default: ignore unknowns
	HandlingStrict                  // 400 on an unknown preference/parameter
)

// PreferSet is the parsed Prefer header. A nil pointer field means the client
// did not state that preference. Applied records, in order, the preferences the
// server actually honored, for the Preference-Applied response header.
type PreferSet struct {
	Return     *ReturnMode
	Count      *CountKind
	Resolution *ConflictRes
	Missing    *MissingMode
	Tx         *TxMode
	Handling   Handling

	// applied is the list of "key=value" tokens that were honored.
	applied []string
}

// ParsePrefer parses one or more Prefer header values (comma-separated tokens)
// into a PreferSet. Unknown tokens are ignored here; strict handling is enforced
// by the caller against the recognized set.
func ParsePrefer(headers []string) PreferSet {
	var p PreferSet
	for _, h := range headers {
		for tok := range strings.SplitSeq(h, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			k, v, _ := strings.Cut(tok, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch k {
			case "return":
				switch v {
				case "minimal":
					m := ReturnMinimal
					p.Return = &m
				case "headers-only":
					m := ReturnHeadersOnly
					p.Return = &m
				case "representation":
					m := ReturnRepresentation
					p.Return = &m
				default:
					continue
				}
				p.markApplied(k + "=" + v)
			case "count":
				switch v {
				case "exact":
					c := CountExact
					p.Count = &c
				case "planned":
					c := CountPlanned
					p.Count = &c
				case "estimated":
					c := CountEstimated
					p.Count = &c
				default:
					continue
				}
				p.markApplied(k + "=" + v)
			case "resolution":
				switch v {
				case "merge-duplicates":
					r := ConflictMerge
					p.Resolution = &r
				case "ignore-duplicates":
					r := ConflictIgnore
					p.Resolution = &r
				default:
					continue
				}
				p.markApplied(k + "=" + v)
			case "missing":
				switch v {
				case "default":
					m := MissingDefault
					p.Missing = &m
				case "null":
					m := MissingNull
					p.Missing = &m
				default:
					continue
				}
				p.markApplied(k + "=" + v)
			case "tx":
				switch v {
				case "commit":
					t := TxCommit
					p.Tx = &t
				case "rollback":
					t := TxRollback
					p.Tx = &t
				default:
					continue
				}
				p.markApplied(k + "=" + v)
			case "handling":
				if v == "strict" {
					p.Handling = HandlingStrict
					p.markApplied(k + "=" + v)
				}
			}
		}
	}
	return p
}

// markApplied records that a "key=value" preference was honored.
func (p *PreferSet) markApplied(kv string) { p.applied = append(p.applied, kv) }

// AppliedHeader returns the Preference-Applied header value, or "" if nothing
// was applied.
func (p *PreferSet) AppliedHeader() string {
	return strings.Join(p.applied, ", ")
}
