package mongo

import (
	"context"
	"encoding/json"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
	mgodriver "go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// Execute lowers the plan to MongoDB operations and returns a streamable result.
func (b *Backend) Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	if plan.Call != nil {
		return nil, pgerr.ErrUnsupported("RPC (MongoDB has no stored procedures)", backendName)
	}
	if plan.Query == nil {
		return nil, pgerr.ErrUnsupported("this operation", backendName)
	}
	switch plan.Query.Kind {
	case ir.Read:
		return b.executeRead(ctx, plan, rc)
	case ir.Insert, ir.Upsert:
		return b.executeInsert(ctx, plan, rc)
	case ir.Update:
		return b.executeUpdate(ctx, plan, rc)
	case ir.Delete:
		return b.executeDelete(ctx, plan, rc)
	default:
		return nil, pgerr.ErrUnsupported("this operation", backendName)
	}
}

// executeRead runs an aggregation pipeline and returns the results as a JSON body.
func (b *Backend) executeRead(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	q := plan.Query
	if len(q.Embeds) > 0 {
		return nil, pgerr.ErrUnsupported("resource embedding (MongoDB $lookup not yet implemented)", backendName)
	}

	coll := b.db.Collection(q.Relation.Name)
	colTypes := columnTypes(plan.Rel)

	res := &bodyResult{controls: rc.Controls()}

	// Count pass if requested.
	if q.Count != ir.CountNone {
		filter := filterDoc(q.Where, colTypes)
		n, err := coll.CountDocuments(ctx, filter)
		if err != nil {
			return nil, b.MapError(err)
		}
		res.count, res.hasCount = n, true
	}

	// Build and run the read pipeline.
	stages, apiErr := BuildReadPipeline(q)
	if apiErr != nil {
		return nil, apiErr
	}

	// Coerce filter values in the $match stage.
	stages = coercePipelineValues(stages, colTypes)

	// Always exclude _id.
	stages = appendExcludeID(stages)

	cur, err := coll.Aggregate(ctx, pipelineToBSON(stages))
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = cur.Close(ctx) }()

	docs, err := cursorDocs(ctx, cur)
	if err != nil {
		return nil, b.MapError(err)
	}

	if q.Singular && len(docs) != 1 {
		return nil, pgerr.ErrSingularZeroMany()
	}
	res.rows = newDocRowStream(convertDocs(docs))
	return res, nil
}

// executeInsert inserts one or more documents and optionally returns them.
func (b *Backend) executeInsert(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	q := plan.Query
	coll := b.db.Collection(q.Relation.Name)
	res := &bodyResult{controls: rc.Controls()}

	docs := writePayloadToDocs(q.Write, plan.Rel)
	if len(docs) == 0 {
		return res, nil
	}

	// Prefer: max-affected. MongoDB writes here are not transactional, so the
	// guard refuses an over-broad insert before any document is written rather
	// than rolling one back; the would-insert count is known up front.
	if apiErr := backend.EnforceMaxAffected(q.Write, int64(len(docs)), true); apiErr != nil {
		return nil, apiErr
	}

	if len(docs) == 1 {
		_, err := coll.InsertOne(ctx, docs[0])
		if err != nil {
			return nil, b.MapError(err)
		}
	} else {
		idocs := make([]any, len(docs))
		for i, d := range docs {
			idocs[i] = d
		}
		_, err := coll.InsertMany(ctx, idocs)
		if err != nil {
			return nil, b.MapError(err)
		}
	}

	res.affected, res.hasAff = int64(len(docs)), true

	if q.Write != nil && q.Write.Return == ir.ReturnRepresentation {
		colTypes := columnTypes(plan.Rel)
		filter := filterDoc(q.Where, colTypes)
		rows, err := b.readForReturn(ctx, coll, filter)
		if err != nil {
			return nil, err
		}
		res.rows = rows
	}
	if res.rows == nil {
		res.rows = newDocRowStream(nil)
	}
	return res, nil
}

// executeUpdate updates matching documents and optionally returns them.
func (b *Backend) executeUpdate(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	q := plan.Query
	coll := b.db.Collection(q.Relation.Name)
	colTypes := columnTypes(plan.Rel)
	res := &bodyResult{controls: rc.Controls(), rows: newDocRowStream(nil)}

	filter := filterDoc(q.Where, colTypes)
	setDoc := writePayloadToSetDoc(q.Write, plan.Rel)

	// Prefer: max-affected. Without a transaction to roll back, count the
	// would-update documents first and refuse before touching any when the match
	// exceeds the bound.
	if q.Write != nil && q.Write.MaxRows != nil {
		n, err := coll.CountDocuments(ctx, filter)
		if err != nil {
			return nil, b.MapError(err)
		}
		if apiErr := backend.EnforceMaxAffected(q.Write, n, true); apiErr != nil {
			return nil, apiErr
		}
	}

	out, err := coll.UpdateMany(ctx, filter, bson.D{{Key: "$set", Value: setDoc}})
	if err != nil {
		return nil, b.MapError(err)
	}
	res.affected, res.hasAff = out.ModifiedCount, true

	if q.Write != nil && q.Write.Return == ir.ReturnRepresentation {
		rows, err := b.readForReturn(ctx, coll, filter)
		if err != nil {
			return nil, err
		}
		res.rows = rows
	}
	return res, nil
}

// executeDelete deletes matching documents and optionally returns them.
func (b *Backend) executeDelete(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (backend.Result, error) {
	q := plan.Query
	coll := b.db.Collection(q.Relation.Name)
	colTypes := columnTypes(plan.Rel)
	res := &bodyResult{controls: rc.Controls(), rows: newDocRowStream(nil)}

	filter := filterDoc(q.Where, colTypes)

	// Prefer: max-affected. Count the would-delete documents and refuse before
	// removing any when the match exceeds the bound, since the delete cannot be
	// rolled back.
	if q.Write != nil && q.Write.MaxRows != nil {
		n, err := coll.CountDocuments(ctx, filter)
		if err != nil {
			return nil, b.MapError(err)
		}
		if apiErr := backend.EnforceMaxAffected(q.Write, n, true); apiErr != nil {
			return nil, apiErr
		}
	}

	if q.Write != nil && q.Write.Return == ir.ReturnRepresentation {
		// Capture rows before deleting.
		returnDocs, err := b.findDocs(ctx, coll, filter)
		if err != nil {
			return nil, err
		}
		res.rows = newDocRowStream(convertDocs(returnDocs))
	}

	out, err := coll.DeleteMany(ctx, filter)
	if err != nil {
		return nil, b.MapError(err)
	}
	res.affected, res.hasAff = out.DeletedCount, true
	return res, nil
}

// readForReturn re-queries after a write to produce the RETURNING row stream.
func (b *Backend) readForReturn(ctx context.Context, coll *mgodriver.Collection, filter bson.D) (*docRowStream, error) {
	docs, err := b.findDocs(ctx, coll, filter)
	if err != nil {
		return nil, err
	}
	return newDocRowStream(convertDocs(docs)), nil
}

// findDocs runs a find with the given filter, returning raw BSON maps.
func (b *Backend) findDocs(ctx context.Context, coll *mgodriver.Collection, filter bson.D) ([]map[string]any, error) {
	cur, err := coll.Find(ctx, filter)
	if err != nil {
		return nil, b.MapError(err)
	}
	defer func() { _ = cur.Close(ctx) }()
	return cursorDocs(ctx, cur)
}

// cursorDocs drains a cursor into raw Go maps (no _id).
func cursorDocs(ctx context.Context, cur *mgodriver.Cursor) ([]map[string]any, error) {
	var docs []map[string]any
	for cur.Next(ctx) {
		var m map[string]any
		if err := cur.Decode(&m); err != nil {
			return nil, err
		}
		delete(m, "_id")
		docs = append(docs, m)
	}
	return docs, cur.Err()
}

// filterDoc builds a BSON filter document from an IR condition, with type coercion.
func filterDoc(cond *ir.Cond, colTypes map[string]string) bson.D {
	if cond == nil {
		return bson.D{}
	}
	d, _ := LowerFilter(*cond)
	if d == nil {
		return bson.D{}
	}
	return docToBSON(coerceFilterValues(d, colTypes))
}

// writePayloadToDocs converts the IR write payload to []bson.D for InsertOne/Many.
func writePayloadToDocs(w *ir.WriteSpec, _ *schema.Relation) []bson.D {
	if w == nil || len(w.Rows) == 0 {
		return nil
	}
	docs := make([]bson.D, 0, len(w.Rows))
	for _, row := range w.Rows {
		var d bson.D
		for _, col := range w.Columns {
			d = append(d, bson.E{Key: col, Value: writeVal(row[col])})
		}
		docs = append(docs, d)
	}
	return docs
}

// writePayloadToSetDoc converts the IR update payload (WriteSpec.Set) into a $set document.
func writePayloadToSetDoc(w *ir.WriteSpec, _ *schema.Relation) bson.D {
	if w == nil || len(w.Set) == 0 {
		return bson.D{}
	}
	var d bson.D
	for col, val := range w.Set {
		d = append(d, bson.E{Key: col, Value: writeVal(val)})
	}
	return d
}

// writeVal extracts the Go-native value from an ir.Value for BSON insertion.
// JSON is preferred (populated for write payloads); json.Number is resolved to
// int64 or float64 so MongoDB stores the right BSON type.
func writeVal(v ir.Value) any {
	switch x := v.JSON.(type) {
	case nil:
		return nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	default:
		return x
	}
}

// columnTypes builds a map from column name to canonical type for a relation.
func columnTypes(rel *schema.Relation) map[string]string {
	if rel == nil {
		return nil
	}
	m := make(map[string]string, len(rel.Columns))
	for _, col := range rel.Columns {
		m[col.Name] = col.Type
	}
	return m
}

// coerceFilterValues walks a filter Doc and coerces string values to their
// canonical Go types based on the column type map. This is needed because the
// IR always carries values as strings but MongoDB is type-sensitive.
func coerceFilterValues(d Doc, colTypes map[string]string) Doc {
	out := make(Doc, len(d))
	for i, f := range d {
		switch f.Key {
		case "$and", "$or", "$nor":
			if arr, ok := f.Value.(Arr); ok {
				newArr := make(Arr, len(arr))
				for j, item := range arr {
					if inner, ok := item.(Doc); ok {
						newArr[j] = coerceFilterValues(inner, colTypes)
					} else {
						newArr[j] = item
					}
				}
				out[i] = Field{Key: f.Key, Value: newArr}
			} else {
				out[i] = f
			}
		default:
			if inner, ok := f.Value.(Doc); ok {
				ct := colTypes[f.Key]
				out[i] = Field{Key: f.Key, Value: coerceOpDoc(inner, ct)}
			} else {
				out[i] = f
			}
		}
	}
	return out
}

// coerceOpDoc coerces values inside a {$op: value} document.
func coerceOpDoc(d Doc, colType string) Doc {
	out := make(Doc, len(d))
	for i, f := range d {
		switch f.Key {
		case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte":
			out[i] = Field{Key: f.Key, Value: coerceValue(f.Value, colType)}
		case "$in":
			if arr, ok := f.Value.(Arr); ok {
				newArr := make(Arr, len(arr))
				for j, v := range arr {
					newArr[j] = coerceValue(v, colType)
				}
				out[i] = Field{Key: f.Key, Value: newArr}
			} else {
				out[i] = f
			}
		default:
			out[i] = f
		}
	}
	return out
}

// coerceValue converts a string value to the appropriate Go type for MongoDB.
func coerceValue(v any, colType string) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch colType {
	case "integer", "int", "int4", "int2", "smallint":
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			return int32(n)
		}
	case "bigint", "int8":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case "boolean":
		switch s {
		case "true":
			return true
		case "false":
			return false
		}
	case "double precision", "real", "float4", "float8":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}

// coercePipelineValues walks the pipeline stages and coerces $match values.
func coercePipelineValues(stages Arr, colTypes map[string]string) Arr {
	if len(colTypes) == 0 {
		return stages
	}
	out := make(Arr, len(stages))
	for i, s := range stages {
		if d, ok := s.(Doc); ok && len(d) == 1 && d[0].Key == "$match" {
			if match, ok := d[0].Value.(Doc); ok {
				out[i] = Doc{{Key: "$match", Value: coerceFilterValues(match, colTypes)}}
				continue
			}
		}
		out[i] = s
	}
	return out
}

// appendExcludeID adds a $project stage that hides _id if not already projected.
func appendExcludeID(stages Arr) Arr {
	for _, s := range stages {
		if d, ok := s.(Doc); ok && len(d) == 1 && d[0].Key == "$project" {
			// Already has a $project. Add _id:0 to it.
			if proj, ok := d[0].Value.(Doc); ok {
				return append(stages[:len(stages)-1],
					Doc{{Key: "$project", Value: append(Doc{{Key: "_id", Value: 0}}, proj...)}})
			}
		}
	}
	return append(stages, Doc{{Key: "$project", Value: Doc{{Key: "_id", Value: 0}}}})
}

// pipelineToBSON converts Arr (our Doc/Field types) to a BSON pipeline value
// that the MongoDB driver accepts as []any.
func pipelineToBSON(stages Arr) []any {
	out := make([]any, len(stages))
	for i, s := range stages {
		out[i] = docToBSONAny(s)
	}
	return out
}

// docToBSON converts a Doc to bson.D.
func docToBSON(d Doc) bson.D {
	out := make(bson.D, len(d))
	for i, f := range d {
		out[i] = bson.E{Key: f.Key, Value: docToBSONAny(f.Value)}
	}
	return out
}

// docToBSONAny converts any Doc/Arr value recursively to BSON-native types.
func docToBSONAny(v any) any {
	switch t := v.(type) {
	case Doc:
		return docToBSON(t)
	case Arr:
		out := make(bson.A, len(t))
		for i, el := range t {
			out[i] = docToBSONAny(el)
		}
		return out
	default:
		return v
	}
}
