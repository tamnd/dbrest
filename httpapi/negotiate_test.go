package httpapi

import "testing"

func TestNegotiateDefaults(t *testing.T) {
	cases := []struct {
		name   string
		accept []string
		want   string
		wantOK bool
	}{
		{"absent", nil, mediaJSON, true},
		{"empty string", []string{""}, mediaJSON, true},
		{"star", []string{"*/*"}, mediaJSON, true},
		{"application star", []string{"application/*"}, mediaJSON, true},
		{"text star picks csv", []string{"text/*"}, mediaCSV, true},
		{"explicit json", []string{"application/json"}, mediaJSON, true},
		{"object", []string{"application/vnd.pgrst.object+json"}, mediaObject, true},
		{"array", []string{"application/vnd.pgrst.array+json"}, mediaArray, true},
		{"csv", []string{"text/csv"}, mediaCSV, true},
		{"octet", []string{"application/octet-stream"}, mediaOctet, true},
		{"text plain", []string{"text/plain"}, mediaText, true},
		{"unsupported only", []string{"application/xml"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := negotiate(c.accept)
			if got != c.want || ok != c.wantOK {
				t.Errorf("negotiate(%v) = (%q,%v), want (%q,%v)", c.accept, got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestNegotiateQualityOrder(t *testing.T) {
	// CSV is offered at higher quality than JSON, so it wins despite JSON's
	// position in the preference list.
	got, ok := negotiate([]string{"application/json;q=0.5, text/csv;q=0.9"})
	if !ok || got != mediaCSV {
		t.Errorf("got (%q,%v), want csv", got, ok)
	}
}

func TestNegotiateSkipsUnsupportedThenMatches(t *testing.T) {
	// The first listed type is unsupported; negotiation falls through to the
	// next acceptable one rather than failing.
	got, ok := negotiate([]string{"application/xml, text/csv"})
	if !ok || got != mediaCSV {
		t.Errorf("got (%q,%v), want csv", got, ok)
	}
}

func TestNegotiateZeroQualityRefuses(t *testing.T) {
	// q=0 explicitly refuses a type; with nothing else acceptable this is a 406.
	if got, ok := negotiate([]string{"application/json;q=0"}); ok {
		t.Errorf("q=0 should refuse, got (%q,%v)", got, ok)
	}
}
