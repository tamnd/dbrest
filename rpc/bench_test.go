package rpc

import "testing"

// Lookup runs once per RPC request and must pick the right overload from the
// posted argument set. The benchmark resolves against a registry holding several
// overloads of the same name, so the match walks candidates rather than hitting
// a single entry.
func BenchmarkStaticRegistryLookup(b *testing.B) {
	reg := NewStaticRegistry([]*Function{
		{Name: "search", Params: []Param{{Name: "q"}}},
		{Name: "search", Params: []Param{{Name: "q"}, {Name: "lang"}}},
		{Name: "search", Params: []Param{{Name: "q"}, {Name: "lang"}, {Name: "limit", Optional: true}}},
		{Name: "add", Params: []Param{{Name: "a"}, {Name: "b"}}},
	})
	args := ArgSet{"q": true, "lang": true}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := reg.Lookup("search", args); !ok {
			b.Fatal("search(q,lang) should resolve")
		}
	}
}
