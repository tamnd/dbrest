package mongo

import (
	"io"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
)

// bodyResult holds the response assembled from MongoDB cursor results.
type bodyResult struct {
	rows     backend.RowStream
	count    int64
	hasCount bool
	affected int64
	hasAff   bool
	controls *reqctx.ResponseControls
}

func (r *bodyResult) Body() io.Reader                            { return nil }
func (r *bodyResult) Rows() backend.RowStream                    { return r.rows }
func (r *bodyResult) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *bodyResult) Affected() (int64, bool)                    { return r.affected, r.hasAff }
func (r *bodyResult) ResponseControls() *reqctx.ResponseControls { return r.controls }

// docRowStream is a RowStream over pre-decoded BSON documents.
type docRowStream struct {
	cols []string
	docs []map[string]any
	pos  int
}

// newDocRowStream wraps a slice of BSON-decoded maps as a RowStream.
// Each map value has already been converted to JSON-safe types via bsonToJSONMap.
func newDocRowStream(docs []map[string]any) *docRowStream {
	var cols []string
	if len(docs) > 0 {
		for k := range docs[0] {
			cols = append(cols, k)
		}
		sort.Strings(cols)
	}
	return &docRowStream{cols: cols, docs: docs, pos: -1}
}

func (s *docRowStream) Columns() []string { return s.cols }

func (s *docRowStream) Next() bool {
	s.pos++
	return s.pos < len(s.docs)
}

func (s *docRowStream) Values() ([]any, error) {
	doc := s.docs[s.pos]
	vals := make([]any, len(s.cols))
	for i, c := range s.cols {
		vals[i] = doc[c]
	}
	return vals, nil
}

func (s *docRowStream) Err() error   { return nil }
func (s *docRowStream) Close() error { return nil }

// convertDocs converts a slice of raw BSON maps to JSON-safe maps.
func convertDocs(raw []map[string]any) []map[string]any {
	out := make([]map[string]any, len(raw))
	for i, d := range raw {
		out[i] = bsonToJSONMap(d)
	}
	return out
}

// bsonToJSONMap converts a raw BSON decoded map to a JSON-safe map.
func bsonToJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "_id" {
			continue
		}
		out[k] = bsonToJSON(v)
	}
	return out
}

// bsonToJSON converts a single BSON value to a JSON-representable type.
func bsonToJSON(v any) any {
	switch t := v.(type) {
	case bson.ObjectID:
		return t.Hex()
	case bson.DateTime:
		return t.Time().UTC().Format(time.RFC3339)
	case time.Time:
		if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
			return t.UTC().Format("2006-01-02")
		}
		return t.UTC().Format(time.RFC3339)
	case bson.A:
		arr := make([]any, len(t))
		for i, el := range t {
			arr[i] = bsonToJSON(el)
		}
		return arr
	case map[string]any:
		return bsonToJSONMap(t)
	case nil:
		return nil
	default:
		return v
	}
}
