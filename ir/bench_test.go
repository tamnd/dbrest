package ir

import "testing"

// ParseRead is on the hot path of every request: it runs once per call before
// any planning or SQL. The benchmark uses a realistic query string carrying a
// projection, an embed, a logic tree, ordering, and a window, the work a typical
// list endpoint does, alongside a Prefer header so the header parse counts too.
func BenchmarkParseRead(b *testing.B) {
	const rawQuery = "select=id,title,director(name)&" +
		"and=(year.gte.2000,or(rating.gte.8,votes.gte.1000))&" +
		"order=year.desc.nullslast,title.asc&limit=20&offset=40"
	prefer := []string{"count=exact", "return=representation"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseRead("films", rawQuery, prefer); err != nil {
			b.Fatal(err)
		}
	}
}

// ParsePrefer is split out because it runs for write and RPC requests too, and a
// client may send several Prefer tokens across multiple header lines.
func BenchmarkParsePrefer(b *testing.B) {
	headers := []string{
		"return=representation, resolution=merge-duplicates",
		"count=exact",
		"missing=default, tx=rollback",
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = ParsePrefer(headers)
	}
}
