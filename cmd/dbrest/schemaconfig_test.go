package main

import (
	"slices"
	"testing"

	"github.com/tamnd/dbrest/config"
)

// schemaRecorder is a minimal stand-in for a backend that accepts both
// schema-shaped setters.
type schemaRecorder struct {
	schemas []string
	extra   []string
}

func (s *schemaRecorder) SetSchemas(v []string)         { s.schemas = v }
func (s *schemaRecorder) SetExtraSearchPath(v []string) { s.extra = v }

// TestApplySchemaConfig checks both options reach a backend that accepts
// them, and that a backend without the setters is simply left alone.
func TestApplySchemaConfig(t *testing.T) {
	cfg, err := config.FromMap(map[string]string{
		"db-uri":               "x",
		"db-schemas":           "api,private",
		"db-extra-search-path": "extensions,util",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := &schemaRecorder{}
	applySchemaConfig(rec, cfg)
	if !slices.Equal(rec.schemas, []string{"api", "private"}) {
		t.Errorf("schemas = %v", rec.schemas)
	}
	if !slices.Equal(rec.extra, []string{"extensions", "util"}) {
		t.Errorf("extra search path = %v", rec.extra)
	}

	applySchemaConfig(struct{}{}, cfg) // must not panic on a bare backend
}
