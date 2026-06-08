package conformance

import (
	"net/http"
	"net/http/httptest"
	"strings"
)

// defaultFloatTolerance is the absolute tolerance for comparing two JSON
// numbers; it absorbs the last-digit differences between engines' float
// rendering without hiding a real value difference.
const defaultFloatTolerance = 1e-9

// Verdict is the outcome of comparing one subject response to its golden.
type Verdict string

const (
	// Pass: the subject reproduced the golden response after normalization.
	Pass Verdict = "pass"
	// Allowlisted: the subject matched, and the case exercises a feature whose
	// divergence is recorded in the allowlist (a non-Native tier).
	Allowlisted Verdict = "allowlisted"
	// Fail: the subject differs from the golden after normalization, with no
	// allowlist cover.
	Fail Verdict = "fail"
)

// CaseResult is the verdict for one replayed case, with the diffs that produced
// a failure (empty otherwise).
type CaseResult struct {
	Name    string
	Feature string
	Verdict Verdict
	Diffs   []Diff
}

// Report is the result of replaying a corpus: the per-case verdicts and the
// rollup counts. OK reports whether the run passed (no failures).
type Report struct {
	Results []CaseResult
	Passed  int
	Allowed int
	Failed  int
}

// OK reports whether every case passed or was allowlisted.
func (r Report) OK() bool { return r.Failed == 0 }

// Replay sends every case's request to the in-process handler, compares the
// captured response to the case's golden after normalization, and rolls the
// verdicts into a report. A case whose feature is on the allowlist is graded
// Allowlisted when it matches; a mismatch is always a failure (spec 22).
func Replay(handler http.Handler, cases []Case, allow *Allowlist) Report {
	var rep Report
	for _, c := range cases {
		sub := issue(handler, c.Request)
		opts := CompareOptions{
			Ordered:        c.Request.Ordered(),
			Mask:           c.Mask,
			FloatTolerance: defaultFloatTolerance,
		}
		_, allowlisted := allow.Entry(c.Feature)
		// A set-divergence (a Best-effort ranking, for example) is compared as a
		// row set rather than a sequence even when the request pins order.
		if allowlisted {
			opts.Ordered = false
		}
		diffs := Compare(c.Golden, sub, opts)

		res := CaseResult{Name: c.Name, Feature: c.Feature, Diffs: diffs}
		switch {
		case len(diffs) > 0:
			res.Verdict = Fail
			rep.Failed++
		case allowlisted:
			res.Verdict = Allowlisted
			rep.Allowed++
		default:
			res.Verdict = Pass
			rep.Passed++
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// issue runs one request against the handler in process and captures the
// response as a neutral Response: status, the headers flattened to single
// strings, and the body.
func issue(handler http.Handler, req Request) Response {
	var body *strings.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	} else {
		body = strings.NewReader("")
	}
	httpReq := httptest.NewRequest(req.Method, req.Target(), body)
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httpReq)

	result := rec.Result()
	headers := make(map[string]string, len(result.Header))
	for k, vs := range result.Header {
		headers[k] = strings.Join(vs, ", ")
	}
	return Response{
		Status:  result.StatusCode,
		Headers: headers,
		Body:    rec.Body.String(),
	}
}
