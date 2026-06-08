package mongo

import (
	"strings"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
)

// backendName labels every PGRST127 this backend raises, so the error names the
// feature and the engine (spec 18).
const backendName = "mongodb"

// LowerFilter lowers an IR condition tree to a MongoDB query document (spec 07).
// The result is the find filter for a simple read and the body of a $match stage
// inside a pipeline. Operator nodes become field-keyed comparison documents;
// logical nodes become $and / $or and negation becomes a field-level $not or a
// $nor. An operator with no faithful MongoDB analog (the array and range
// operators) returns PGRST127 before any value is lowered, the never-silently-
// wrong rule the SQL dialects also follow.
//
// Values ride into the document as the literal text the client wrote, exactly as
// the SQL dialects bind ir.Value.Text as a string argument and let the engine
// coerce it. Coercing a value to its field's BSON type from the schema model
// (the string "18" to an int, an _id hex string to an ObjectId) is the data
// plane's job, run against the resolved model just before the document reaches
// the driver; keeping it out of the lowering is what lets this half be tested
// with no server.
func LowerFilter(cond ir.Cond) (Doc, *pgerr.APIError) {
	switch c := cond.(type) {
	case ir.And:
		return lowerGroup("$and", c.Kids)
	case ir.Or:
		return lowerGroup("$or", c.Kids)
	case ir.Not:
		inner, err := LowerFilter(c.Kid)
		if err != nil {
			return nil, err
		}
		return negate(inner), nil
	case ir.Compare:
		return lowerCompare(c)
	default:
		return nil, pgerr.ErrUnsupported("this filter node", backendName)
	}
}

// lowerGroup lowers an And/Or to its {$and: [...]} / {$or: [...]} document.
func lowerGroup(op string, kids []ir.Cond) (Doc, *pgerr.APIError) {
	parts := make(Arr, 0, len(kids))
	for _, k := range kids {
		d, err := LowerFilter(k)
		if err != nil {
			return nil, err
		}
		parts = append(parts, d)
	}
	return Doc{{Key: op, Value: parts}}, nil
}

// negate wraps a lowered condition in its negation. A single field-keyed
// comparison ({field: {$op: v}}) negates at the operator level with $not, the
// form MongoDB allows; anything else (a logical group, or an $expr document)
// negates with a single-element $nor, which is a plain NOT of its one member.
func negate(d Doc) Doc {
	if len(d) == 1 {
		if op, ok := d[0].Value.(Doc); ok && !strings.HasPrefix(d[0].Key, "$") {
			return Doc{{Key: d[0].Key, Value: Doc{{Key: "$not", Value: op}}}}
		}
	}
	return Doc{{Key: "$nor", Value: Arr{d}}}
}

// lowerCompare lowers a single column-operator-value predicate. The positive
// form is built first; an inline not. prefix (Compare.Negate) wraps it with the
// same negation rule LowerFilter uses for a not.and / not.or group.
func lowerCompare(c ir.Compare) (Doc, *pgerr.APIError) {
	d, err := comparePositive(c)
	if err != nil {
		return nil, err
	}
	if c.Negate {
		return negate(d), nil
	}
	return d, nil
}

// comparePositive builds the un-negated comparison document for an operator.
func comparePositive(c ir.Compare) (Doc, *pgerr.APIError) {
	field := dotted(c.Path)
	switch c.Op {
	case ir.OpEq:
		return fieldOp(field, "$eq", c.Value.Text), nil
	case ir.OpNeq:
		return fieldOp(field, "$ne", c.Value.Text), nil
	case ir.OpGt:
		return fieldOp(field, "$gt", c.Value.Text), nil
	case ir.OpGte:
		return fieldOp(field, "$gte", c.Value.Text), nil
	case ir.OpLt:
		return fieldOp(field, "$lt", c.Value.Text), nil
	case ir.OpLte:
		return fieldOp(field, "$lte", c.Value.Text), nil
	case ir.OpIn:
		list := make(Arr, len(c.Value.List))
		for i, v := range c.Value.List {
			list[i] = v
		}
		return fieldOp(field, "$in", list), nil
	case ir.OpIs:
		return lowerIs(field, c.Value.Text), nil
	case ir.OpLike:
		return regexDoc(field, likeToRegex(c.Value.Text), false), nil
	case ir.OpILike:
		return regexDoc(field, likeToRegex(c.Value.Text), true), nil
	case ir.OpMatch:
		return regexDoc(field, c.Value.Text, false), nil
	case ir.OpIMatch:
		return regexDoc(field, c.Value.Text, true), nil
	case ir.OpIsDistinct:
		// Null-safe inequality: a missing field reads as null via $ifNull, then
		// $ne compares against the literal. $expr lets an aggregation expression
		// stand in for a query operator.
		expr := Doc{{Key: "$ne", Value: Arr{
			Doc{{Key: "$ifNull", Value: Arr{"$" + field, nil}}},
			c.Value.Text,
		}}}
		return Doc{{Key: "$expr", Value: expr}}, nil
	case ir.OpFTS:
		return Doc{{Key: "$text", Value: Doc{{Key: "$search", Value: ftsSearch(c.FTS, c.Value.Text)}}}}, nil
	case ir.OpContains, ir.OpContained, ir.OpOverlap,
		ir.OpRangeSL, ir.OpRangeSR, ir.OpRangeNXR, ir.OpRangeNXL, ir.OpRangeAdj:
		// The array and range operators have no faithful MongoDB analog; faking
		// containment with $in would be quietly wrong, so reject before lowering.
		return nil, pgerr.ErrUnsupported("the "+operatorName(c.Op)+" operator", backendName)
	default:
		return nil, pgerr.ErrUnsupported("this operator", backendName)
	}
}

// fieldOp builds a {field: {op: value}} comparison document.
func fieldOp(field, op string, value any) Doc {
	return Doc{{Key: field, Value: Doc{{Key: op, Value: value}}}}
}

// lowerIs lowers an is.<state> predicate. is.null matches missing-or-null (the
// documented Best-effort divergence [m3]); the boolean and not_null states are
// exact.
func lowerIs(field, state string) Doc {
	switch state {
	case "null", "unknown":
		return fieldOp(field, "$eq", nil)
	case "not_null":
		return fieldOp(field, "$ne", nil)
	case "true":
		return fieldOp(field, "$eq", true)
	case "false":
		return fieldOp(field, "$eq", false)
	default:
		return fieldOp(field, "$eq", nil)
	}
}

// regexDoc builds a {field: {$regex: pat[, $options: "i"]}} document. The
// case-insensitive form carries $options: "i".
func regexDoc(field, pattern string, ci bool) Doc {
	inner := Doc{{Key: "$regex", Value: pattern}}
	if ci {
		inner = append(inner, Field{Key: "$options", Value: "i"})
	}
	return Doc{{Key: field, Value: inner}}
}

// likeToRegex translates a SQL LIKE pattern to an anchored MongoDB regex. % maps
// to .*, _ maps to ., the literal portions have their regex metacharacters
// escaped, and the whole is anchored ^...$. This is the Best-effort mapping
// [m2]: PostgreSQL LIKE collation and Unicode case folding are not byte-identical
// to PCRE $regex, a divergence the conformance allowlist records (spec 22).
func likeToRegex(pat string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		case '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('$')
	return b.String()
}

// ftsSearch translates an fts-family value into a MongoDB $text search string.
// A phrase keeps word order as a quoted string; the plain, plainto, and web
// forms pass their terms through, since $text already ANDs bare terms and reads
// "..." as a phrase. This is Best-effort [m4]: MongoDB text-index tokenization
// and ranking are not PostgreSQL tsvector semantics (spec 21).
func ftsSearch(variant ir.FTSVariant, value string) string {
	switch variant {
	case ir.FTSPhrase:
		return `"` + strings.Join(strings.Fields(value), " ") + `"`
	default:
		return strings.Join(strings.Fields(value), " ")
	}
}

// dotted renders a column path as a MongoDB dotted field path, mapping
// PostgREST's col->key / col->>key JSON access onto field.sub access.
func dotted(path []string) string {
	return strings.Join(path, ".")
}

// operatorName returns the PostgREST spelling of an array or range operator for
// the PGRST127 message.
func operatorName(op ir.Op) string {
	switch op {
	case ir.OpContains:
		return "cs"
	case ir.OpContained:
		return "cd"
	case ir.OpOverlap:
		return "ov"
	case ir.OpRangeSL:
		return "sl"
	case ir.OpRangeSR:
		return "sr"
	case ir.OpRangeNXR:
		return "nxr"
	case ir.OpRangeNXL:
		return "nxl"
	case ir.OpRangeAdj:
		return "adj"
	default:
		return "unknown"
	}
}
