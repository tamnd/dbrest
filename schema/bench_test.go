package schema

import "testing"

// Relationships is the embed resolver: for each embed the planner asks the model
// which edges connect the parent to a target. The benchmark resolves a
// many-to-many edge over the junction fixture, the most involved case, since it
// scans foreign keys on both the parent and the junction.
func BenchmarkRelationshipsManyToMany(b *testing.B) {
	m := buildEmbedModel()
	films, _ := m.Lookup("films", []string{"public"})
	path := []string{"public"}
	b.ReportAllocs()
	for b.Loop() {
		cands, found := m.Relationships(films, "actors", path)
		if !found || len(cands) != 1 {
			b.Fatalf("films -> actors: found=%v candidates=%d", found, len(cands))
		}
	}
}

// Lookup resolves a relation name against the search path on every request,
// before anything else touches the model.
func BenchmarkModelLookup(b *testing.B) {
	m := buildEmbedModel()
	path := []string{"public"}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := m.Lookup("films", path); !ok {
			b.Fatal("films should resolve")
		}
	}
}
