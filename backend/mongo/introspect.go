package mongo

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	mgoptions "go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/tamnd/dbrest/schema"
)

const sampleSize = int64(100)

// Introspect builds the unified schema model from MongoDB collection metadata.
// Each collection becomes a schema.Relation; columns are inferred by sampling up
// to 100 documents; foreign keys are detected by convention: a field X_id where X
// matches another collection name is treated as a FK referencing X.id.
func (b *Backend) Introspect(ctx context.Context) (*schema.Model, error) {
	names, err := b.db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, err
	}

	rels := make([]*schema.Relation, 0, len(names))
	for _, name := range names {
		rel, err := b.introspectCollection(ctx, name)
		if err != nil {
			return nil, err
		}
		rels = append(rels, rel)
	}

	// Convention-based FK detection: X_id → collection X on field id.
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	for _, rel := range rels {
		for _, col := range rel.Columns {
			if !strings.HasSuffix(col.Name, "_id") {
				continue
			}
			stem := strings.TrimSuffix(col.Name, "_id")
			// Try exact match first, then plural form (e.g. person_id → persons).
			refName := stem
			if !nameSet[refName] {
				refName = stem + "s"
				if !nameSet[refName] {
					continue
				}
			}
			rel.ForeignKeys = append(rel.ForeignKeys, &schema.ForeignKey{
				Name:        rel.Name + "_" + col.Name + "_fkey",
				Columns:     []string{col.Name},
				RefRelation: refName,
				RefColumns:  []string{"id"},
			})
		}
	}

	return schema.NewModel(rels), nil
}

// introspectCollection samples documents from a collection and infers column
// types from BSON value kinds.
func (b *Backend) introspectCollection(ctx context.Context, name string) (*schema.Relation, error) {
	cur, err := b.db.Collection(name).Find(ctx, bson.D{}, mgoptions.Find().SetLimit(sampleSize))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	fieldOrder := []string{}
	fieldTypes := map[string]string{}
	for cur.Next(ctx) {
		var doc bson.D
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		for _, el := range doc {
			if el.Key == "_id" {
				continue
			}
			if _, seen := fieldTypes[el.Key]; !seen {
				fieldOrder = append(fieldOrder, el.Key)
				fieldTypes[el.Key] = bsonCanonicalType(el.Value)
			}
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}

	var pk []string
	cols := make([]*schema.Column, 0, len(fieldTypes))
	pos := 1
	if t, ok := fieldTypes["id"]; ok {
		cols = append(cols, &schema.Column{Name: "id", Type: t, Nullable: false, Position: pos})
		pk = []string{"id"}
		pos++
	}
	for _, k := range fieldOrder {
		if k == "id" {
			continue
		}
		cols = append(cols, &schema.Column{Name: k, Type: fieldTypes[k], Nullable: true, Position: pos})
		pos++
	}

	return &schema.Relation{
		Schema:     "",
		Name:       name,
		Kind:       schema.KindTable,
		Columns:    cols,
		PrimaryKey: pk,
	}, nil
}

// bsonCanonicalType maps a BSON value to a dbrest canonical PostgreSQL type name.
func bsonCanonicalType(v any) string {
	switch v.(type) {
	case int32:
		return "integer"
	case int64:
		return "bigint"
	case float64:
		return "double precision"
	case bool:
		return "boolean"
	case bson.DateTime, time.Time:
		return "timestamp"
	case bson.ObjectID:
		return "text"
	case bson.Binary:
		return "bytea"
	case nil:
		return "text"
	case bson.A:
		return "jsonb"
	case bson.D, bson.M:
		return "json"
	default:
		return "text"
	}
}
