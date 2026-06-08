package mongo

import (
	"strconv"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
)

// BuildReadPipeline assembles the aggregation pipeline for a read (spec 07). The
// stages are emitted in execution order: $match (the lowered filter), an
// optional $addFields carrying NULLS-ordering sort keys, $sort, $skip, $limit,
// and $project (vertical filter, aliases, and casts). The pipeline assembles the
// response document in-engine, so the backend returns the bytes as Result.Body
// and field order comes from the $project rather than from Go-side reshaping
// (the JSONAssembly capability).
//
// Embedding ($lookup / $graphLookup) and aggregates ($group) are the deferred
// data-plane slice's; a select item that needs them returns PGRST127 here rather
// than a silently incomplete pipeline.
func BuildReadPipeline(q *ir.Query) (Arr, *pgerr.APIError) {
	// A non-nil slice so an unfiltered select-all read renders as an empty pipeline
	// [], which aggregate accepts, rather than a null.
	stages := Arr{}

	if q.Where != nil {
		match, err := LowerFilter(*q.Where)
		if err != nil {
			return nil, err
		}
		stages = append(stages, Doc{{Key: "$match", Value: match}})
	}

	addFields, sort := orderStages(q.Order)
	if len(addFields) > 0 {
		stages = append(stages, Doc{{Key: "$addFields", Value: addFields}})
	}
	if len(sort) > 0 {
		stages = append(stages, Doc{{Key: "$sort", Value: sort}})
	}

	if q.Offset != nil {
		stages = append(stages, Doc{{Key: "$skip", Value: *q.Offset}})
	}
	if q.Limit != nil {
		stages = append(stages, Doc{{Key: "$limit", Value: *q.Limit}})
	}

	proj, err := projection(q.Select)
	if err != nil {
		return nil, err
	}
	if proj != nil {
		stages = append(stages, Doc{{Key: "$project", Value: proj}})
	}
	return stages, nil
}

// orderStages builds the $sort document and, when a term requests explicit NULLS
// placement, the $addFields sort keys that emulate PostgreSQL's placement.
// MongoDB orders null and missing values in a fixed position that is not
// PostgreSQL's NULLS LAST on ASC / NULLS FIRST on DESC, so for a term carrying an
// explicit nullsfirst/nullslast dbrest computes a 0/1 rank key and sorts on it
// ahead of the field. This is Best-effort [m6]; without an explicit request the
// field is sorted directly and BSON's own null placement stands.
func orderStages(order []ir.OrderTerm) (Doc, Doc) {
	var addFields, sort Doc
	for i, t := range order {
		field := dotted(t.Path)
		if t.NullsFirst != nil {
			// NULLS LAST: a null ranks 1 and sorts after the 0 non-nulls. NULLS
			// FIRST flips the ranks. The key is always sorted ascending so the rank
			// order holds regardless of the field's own direction.
			nullRank, nonNullRank := 1, 0
			if *t.NullsFirst {
				nullRank, nonNullRank = 0, 1
			}
			key := "__dbrest_nulls_" + strconv.Itoa(i)
			addFields = append(addFields, Field{Key: key, Value: Doc{{Key: "$cond", Value: Arr{
				Doc{{Key: "$eq", Value: Arr{"$" + field, nil}}},
				nullRank,
				nonNullRank,
			}}}})
			sort = append(sort, Field{Key: key, Value: 1})
		}
		dir := 1
		if t.Desc {
			dir = -1
		}
		sort = append(sort, Field{Key: field, Value: dir})
	}
	return addFields, sort
}

// projection builds the $project document for the select list. A plain column is
// an inclusion ({field: 1}); a renamed column, a JSON sub-path, or a cast is a
// computed field ({outkey: "$path"} or {outkey: <convert>}). An empty select
// projects nothing (the whole document passes through), matching a bare GET.
func projection(items []ir.SelectItem) (Doc, *pgerr.APIError) {
	if len(items) == 0 {
		return nil, nil
	}
	var proj Doc
	for _, item := range items {
		col, ok := item.(ir.Column)
		if !ok {
			// Aggregates and embed references are assembled by the deferred
			// aggregate/embedding slice, not this read projection.
			return nil, pgerr.ErrUnsupported("this select item", backendName)
		}
		name := col.Name()
		path := dotted(col.Path)
		switch {
		case col.Cast != "":
			proj = append(proj, Field{Key: name, Value: convertExpr(path, col.Cast)})
		case col.Alias != "" || len(col.Path) > 1:
			// A rename or a dotted JSON sub-path is a computed projection.
			proj = append(proj, Field{Key: name, Value: "$" + path})
		default:
			proj = append(proj, Field{Key: name, Value: 1})
		}
	}
	return proj, nil
}

// convertExpr builds the aggregation conversion for a ::type cast. Casts lower to
// $toInt / $toLong / $toString / $toDouble / $toDecimal / $toBool / $toDate, the
// closest aggregation conversions to SQL CAST, which is why casts are Best-effort
// [16]: $convert semantics are close to but not identical to PostgreSQL CAST.
func convertExpr(path, canonicalType string) Doc {
	return Doc{{Key: convertOp(canonicalType), Value: "$" + path}}
}

// convertOp maps a canonical type name to its aggregation conversion operator.
func convertOp(canonical string) string {
	switch canonical {
	case "smallint", "int2", "int", "integer", "int4":
		return "$toInt"
	case "bigint", "int8":
		return "$toLong"
	case "real", "float4", "double precision", "float", "float8":
		return "$toDouble"
	case "numeric", "decimal":
		return "$toDecimal"
	case "bool", "boolean":
		return "$toBool"
	case "date", "timestamp", "timestamptz":
		return "$toDate"
	default:
		// text/varchar/char/uuid/json and any unknown type render as a string, the
		// widest safe target.
		return "$toString"
	}
}
