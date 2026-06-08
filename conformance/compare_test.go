package conformance

import "testing"

func resp(status, body string) Response {
	return Response{Status: statusOf(status), Body: body}
}

func statusOf(s string) int {
	switch s {
	case "200":
		return 200
	case "400":
		return 400
	default:
		return 0
	}
}

func TestCompareIdentical(t *testing.T) {
	g := Response{Status: 200, Body: `[{"id":1,"title":"Metropolis"}]`}
	s := Response{Status: 200, Body: `[{"id":1,"title":"Metropolis"}]`}
	if d := Compare(g, s, CompareOptions{Ordered: true}); len(d) != 0 {
		t.Errorf("expected no diffs, got %v", d)
	}
}

func TestCompareKeyOrderInsensitive(t *testing.T) {
	g := Response{Status: 200, Body: `[{"id":1,"title":"Metropolis"}]`}
	s := Response{Status: 200, Body: `[{"title":"Metropolis","id":1}]`}
	if d := Compare(g, s, CompareOptions{Ordered: true}); len(d) != 0 {
		t.Errorf("object key order should not matter, got %v", d)
	}
}

func TestCompareWhitespaceInsensitive(t *testing.T) {
	g := Response{Status: 200, Body: `[{"id":1}]`}
	s := Response{Status: 200, Body: "[\n  {\n    \"id\": 1\n  }\n]"}
	if d := Compare(g, s, CompareOptions{Ordered: true}); len(d) != 0 {
		t.Errorf("insignificant whitespace should not matter, got %v", d)
	}
}

func TestCompareArrayUnorderedPasses(t *testing.T) {
	g := Response{Status: 200, Body: `[{"id":1},{"id":2}]`}
	s := Response{Status: 200, Body: `[{"id":2},{"id":1}]`}
	if d := Compare(g, s, CompareOptions{Ordered: false}); len(d) != 0 {
		t.Errorf("unordered request should compare arrays as a set, got %v", d)
	}
}

func TestCompareArrayOrderedFails(t *testing.T) {
	g := Response{Status: 200, Body: `[{"id":1},{"id":2}]`}
	s := Response{Status: 200, Body: `[{"id":2},{"id":1}]`}
	if d := Compare(g, s, CompareOptions{Ordered: true}); len(d) == 0 {
		t.Error("ordered request should treat row order as significant")
	}
}

func TestMaskVolatileField(t *testing.T) {
	g := Response{Status: 200, Body: `[{"id":1,"created_at":"2020-01-01T00:00:00Z"}]`}
	s := Response{Status: 200, Body: `[{"id":1,"created_at":"2026-06-08T09:30:00Z"}]`}
	opts := CompareOptions{Ordered: true, Mask: []string{"/0/created_at"}}
	if d := Compare(g, s, opts); len(d) != 0 {
		t.Errorf("masked field should not cause a diff, got %v", d)
	}
	// Without the mask the same pair must differ, proving the mask did the work.
	if d := Compare(g, s, CompareOptions{Ordered: true}); len(d) == 0 {
		t.Error("expected a diff without the mask")
	}
}

func TestStatusDiff(t *testing.T) {
	d := Compare(resp("200", "[]"), resp("400", "[]"), CompareOptions{})
	if len(d) == 0 || d[0].Field != "status" {
		t.Errorf("expected a status diff, got %v", d)
	}
}

func TestFloatTolerance(t *testing.T) {
	g := Response{Status: 200, Body: `[{"x":1.0000000001}]`}
	s := Response{Status: 200, Body: `[{"x":1.0000000002}]`}
	if d := Compare(g, s, CompareOptions{Ordered: true, FloatTolerance: 1e-6}); len(d) != 0 {
		t.Errorf("floats within tolerance should match, got %v", d)
	}
	if d := Compare(g, s, CompareOptions{Ordered: true, FloatTolerance: 0}); len(d) == 0 {
		t.Error("floats should differ with zero tolerance")
	}
}

func TestContractualHeaderCompared(t *testing.T) {
	g := Response{Status: 206, Headers: map[string]string{"Content-Range": "0-9/100"}, Body: "[]"}
	s := Response{Status: 206, Headers: map[string]string{"Content-Range": "0-9/200"}, Body: "[]"}
	d := Compare(g, s, CompareOptions{})
	if len(d) == 0 {
		t.Fatal("expected a Content-Range diff")
	}
}

func TestTransportHeaderIgnored(t *testing.T) {
	g := Response{Status: 200, Headers: map[string]string{"Date": "Mon"}, Body: "[]"}
	s := Response{Status: 200, Headers: map[string]string{"Date": "Tue"}, Body: "[]"}
	if d := Compare(g, s, CompareOptions{}); len(d) != 0 {
		t.Errorf("Date is transport and must be ignored, got %v", d)
	}
}

func TestNonJSONBodyCompared(t *testing.T) {
	g := Response{Status: 200, Body: "id,title\n1,Metropolis\n"}
	s := Response{Status: 200, Body: "id,title\n1,Metropolis\n"}
	if d := Compare(g, s, CompareOptions{}); len(d) != 0 {
		t.Errorf("identical CSV should match, got %v", d)
	}
	s2 := Response{Status: 200, Body: "id,title\n2,Blade Runner\n"}
	if d := Compare(g, s2, CompareOptions{}); len(d) == 0 {
		t.Error("different CSV should differ")
	}
}

func BenchmarkCompare(b *testing.B) {
	body := `[{"id":1,"title":"Metropolis","year":1927},{"id":2,"title":"Blade Runner","year":1982},{"id":3,"title":"Arrival","year":2016}]`
	g := Response{Status: 200, Headers: map[string]string{"Content-Range": "0-2/3"}, Body: body}
	s := Response{Status: 200, Headers: map[string]string{"Content-Range": "0-2/3"}, Body: body}
	opts := CompareOptions{Ordered: false, FloatTolerance: defaultFloatTolerance}
	for b.Loop() {
		if d := Compare(g, s, opts); len(d) != 0 {
			b.Fatalf("unexpected diffs: %v", d)
		}
	}
}
