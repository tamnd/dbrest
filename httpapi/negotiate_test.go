package httpapi

import (
	"testing"

	"github.com/tamnd/dbrest/backend"
)

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

// TestNegotiateSuffixlessVendorTypes checks the suffixless PostgREST vendor
// spellings resolve to the same renderers as their +json forms.
func TestNegotiateSuffixlessVendorTypes(t *testing.T) {
	if got, ok := negotiate([]string{"application/vnd.pgrst.object"}); !ok || got != mediaObject {
		t.Errorf("object synonym got (%q,%v), want %q", got, ok, mediaObject)
	}
	if got, ok := negotiate([]string{"application/vnd.pgrst.array"}); !ok || got != mediaArray {
		t.Errorf("array synonym got (%q,%v), want %q", got, ok, mediaArray)
	}
}

func TestNegotiateZeroQualityRefuses(t *testing.T) {
	// q=0 explicitly refuses a type; with nothing else acceptable this is a 406.
	if got, ok := negotiate([]string{"application/json;q=0"}); ok {
		t.Errorf("q=0 should refuse, got (%q,%v)", got, ok)
	}
}

// TestNegotiatePlanFamily checks every spelling of the plan media type (bare,
// +text, +json, and a parameterized form) negotiates to the single mediaPlan
// sentinel that servePlan keys on.
func TestNegotiatePlanFamily(t *testing.T) {
	cases := []string{
		"application/vnd.pgrst.plan",
		"application/vnd.pgrst.plan+text",
		"application/vnd.pgrst.plan+json",
		`application/vnd.pgrst.plan+json; for="application/json"; options=analyze`,
	}
	for _, accept := range cases {
		t.Run(accept, func(t *testing.T) {
			got, ok := negotiate([]string{accept})
			if !ok || got != mediaPlan {
				t.Errorf("negotiate(%q) = (%q,%v), want (%q,true)", accept, got, ok, mediaPlan)
			}
		})
	}
}

// TestParsePlanFormat checks the output format each plan spelling selects: bare
// and +text are PostgREST's text default, +json the machine-readable form.
func TestParsePlanFormat(t *testing.T) {
	cases := []struct {
		accept string
		want   backend.PlanFormat
	}{
		{"application/vnd.pgrst.plan", backend.PlanText},
		{"application/vnd.pgrst.plan+text", backend.PlanText},
		{"application/vnd.pgrst.plan+json", backend.PlanJSON},
	}
	for _, c := range cases {
		t.Run(c.accept, func(t *testing.T) {
			opts, ok := parsePlan([]string{c.accept})
			if !ok {
				t.Fatalf("parsePlan(%q) not recognized", c.accept)
			}
			if opts.Format != c.want {
				t.Errorf("Format = %d, want %d", opts.Format, c.want)
			}
			// for= defaults to application/json when the parameter is absent.
			if opts.For != mediaJSON {
				t.Errorf("For = %q, want %q", opts.For, mediaJSON)
			}
		})
	}
}

// TestParsePlanNonPlan reports that a non-plan Accept is not recognized as a
// plan request.
func TestParsePlanNonPlan(t *testing.T) {
	if _, ok := parsePlan([]string{"application/json"}); ok {
		t.Error("application/json should not parse as a plan request")
	}
}

// TestParsePlanForAndOptions checks the for="<media>" target and the options=
// flag list are both parsed off the plan media type.
func TestParsePlanForAndOptions(t *testing.T) {
	accept := `application/vnd.pgrst.plan+json; for="text/csv"; options=analyze|verbose|settings|buffers|wal`
	opts, ok := parsePlan([]string{accept})
	if !ok {
		t.Fatalf("parsePlan(%q) not recognized", accept)
	}
	if opts.For != "text/csv" {
		t.Errorf("For = %q, want text/csv", opts.For)
	}
	if !opts.Analyze || !opts.Verbose || !opts.Settings || !opts.Buffers || !opts.Wal {
		t.Errorf("options not all set: %+v", opts)
	}
}
