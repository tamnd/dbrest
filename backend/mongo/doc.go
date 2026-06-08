// Package mongo is the MongoDB backend (spec 07). Unlike the four SQL engines it
// does not use the dialect-parameterized compiler (spec 06): it lowers the query
// IR straight onto the aggregation framework. A filter becomes a $match query
// document, a read becomes a find or an aggregation pipeline, and embedding and
// aggregation are assembled in-engine so the body streams as Result.Body.
//
// This package supplies the database-free half of that lowering: the filter to
// query-document mapping (query.go), the read-pipeline assembly (pipeline.go),
// and the topology-computed capabilities (capabilities.go). The driver-facing
// half (the live mongo.Client, embedding execution with $lookup and
// $graphLookup, writes, and introspection) is a separate slice that needs a
// running server to test, mirroring how the SQL dialects landed ahead of their
// driver data planes.
package mongo

import (
	"bytes"
	"encoding/json"
)

// Doc is an ordered MongoDB document: the bson.D analog. The lowering builds
// these because field order is observable in a $project and in the assembled
// body, and the data-plane slice maps each Field onto a bson.E one-to-one. Doc
// marshals to ordered JSON so the lowering can be snapshot-tested with no server,
// the same database-free strategy the SQL dialects use (spec 06 section 7).
type Doc []Field

// Field is one ordered key/value entry in a Doc (the bson.E analog). A Value may
// be a scalar, a nested Doc, or an Arr.
type Field struct {
	Key   string
	Value any
}

// Arr is an ordered MongoDB array (the bson.A analog). It is a plain slice, so
// encoding/json already preserves its order; only Doc needs custom marshaling.
type Arr []any

// MarshalJSON renders the document with its fields in insertion order, which a
// map cannot promise. Mongo query documents and $project stages are
// order-sensitive, so the snapshot tests compare against ordered JSON.
func (d Doc) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range d {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		val, err := json.Marshal(f.Value)
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
