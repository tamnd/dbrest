package ir

import (
	"strings"

	"github.com/tamnd/dbrest/pgerr"
)

// Handling is the Prefer: handling= mode for unrecognized parameters/preferences.
type Handling uint8

const (
	HandlingLenient Handling = iota // default: ignore unknowns
	HandlingStrict                  // 400 on an unknown preference/parameter
)

// PreferSet is the parsed Prefer header. A nil pointer field means the client
// did not state that preference. applied records the honored "key=value" tokens
// for the Preference-Applied response header; invalid records the tokens a
// handling=strict request rejects.
type PreferSet struct {
	Return     *ReturnMode
	Count      *CountKind
	Resolution *ConflictRes
	Missing    *MissingMode
	Tx         *TxMode
	Handling   Handling

	// applied maps a preference key to its honored "key=value" token. The header
	// is emitted in PostgREST's canonical order, not encounter order.
	applied map[string]string
	// invalid lists the verbatim tokens that named an unknown preference or gave a
	// known one a bad value; handling=strict rejects a request carrying any.
	invalid []string
}

// preferKeys are the preference keys dbrest recognizes. A token whose key is not
// here is an unknown preference, an offender under handling=strict.
var preferKeys = map[string]bool{
	"return": true, "count": true, "resolution": true,
	"missing": true, "tx": true, "handling": true,
}

// applyOrder is PostgREST's fixed Preference-Applied ordering. timezone and
// max-affected are listed for when those preferences land (02.2, 02.3); an
// absent key is skipped.
var applyOrder = []string{"resolution", "missing", "return", "count", "tx", "handling", "timezone", "max-affected"}

// ParsePrefer parses one or more Prefer header values (comma-separated tokens)
// into a PreferSet. Only the first occurrence of a duplicated preference is
// honored, matching PostgREST. Unknown keys and bad values are recorded on
// invalid so a handling=strict caller can be rejected; under the default lenient
// handling they are ignored.
func ParsePrefer(headers []string) PreferSet {
	p := PreferSet{applied: map[string]string{}}
	seen := map[string]bool{}
	for _, h := range headers {
		for tok := range strings.SplitSeq(h, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			k, v, _ := strings.Cut(tok, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !preferKeys[k] {
				p.invalid = append(p.invalid, tok)
				continue
			}
			if seen[k] {
				// Only the first occurrence of a preference is honored.
				continue
			}
			seen[k] = true
			if p.set(k, v) {
				p.applied[k] = k + "=" + v
			} else {
				p.invalid = append(p.invalid, tok)
			}
		}
	}
	return p
}

// set applies one recognized preference and reports whether the value was valid.
// A bad value leaves the field untouched and the token is recorded as an
// offender by the caller.
func (p *PreferSet) set(k, v string) bool {
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
			return false
		}
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
			return false
		}
	case "resolution":
		switch v {
		case "merge-duplicates":
			r := ConflictMerge
			p.Resolution = &r
		case "ignore-duplicates":
			r := ConflictIgnore
			p.Resolution = &r
		default:
			return false
		}
	case "missing":
		switch v {
		case "default":
			m := MissingDefault
			p.Missing = &m
		case "null":
			m := MissingNull
			p.Missing = &m
		default:
			return false
		}
	case "tx":
		switch v {
		case "commit":
			t := TxCommit
			p.Tx = &t
		case "rollback":
			t := TxRollback
			p.Tx = &t
		default:
			return false
		}
	case "handling":
		switch v {
		case "strict":
			p.Handling = HandlingStrict
		case "lenient":
			p.Handling = HandlingLenient
		default:
			return false
		}
	}
	return true
}

// StrictError returns the PGRST122 a handling=strict request earns when it
// carries any unknown preference or bad value, and nil otherwise (including the
// default lenient handling, which ignores the offenders).
func (p *PreferSet) StrictError() *pgerr.APIError {
	if p.Handling != HandlingStrict || len(p.invalid) == 0 {
		return nil
	}
	return pgerr.ErrInvalidPreferences(p.invalid)
}

// AppliedHeader returns the Preference-Applied header value in PostgREST's
// canonical order, or "" if nothing was applied.
func (p *PreferSet) AppliedHeader() string {
	if len(p.applied) == 0 {
		return ""
	}
	out := make([]string, 0, len(p.applied))
	for _, k := range applyOrder {
		if v, ok := p.applied[k]; ok {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}
