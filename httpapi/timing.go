package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// phaseTimer accumulates per-phase durations for a single request and renders
// them as a Server-Timing header. PostgREST emits this header under
// server-timing-enabled with the phases jwt, parse, plan, transaction, and
// response; dbrest records the same names in that pipeline order. The zero
// value is unused: a request that does not enable timing carries a nil
// *phaseTimer, and every method is nil-safe so the handlers stay uncluttered.
type phaseTimer struct {
	marks []phaseMark
}

type phaseMark struct {
	name string
	dur  time.Duration
}

// mark records the time since start under name. A nil receiver (timing
// disabled) is a no-op, so a handler can call t.mark unconditionally.
func (t *phaseTimer) mark(name string, start time.Time) {
	if t == nil {
		return
	}
	t.marks = append(t.marks, phaseMark{name, time.Since(start)})
}

// header renders the accumulated marks as a Server-Timing value, durations in
// milliseconds, the encoding PostgREST uses. An empty timer yields "".
func (t *phaseTimer) header() string {
	if t == nil || len(t.marks) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range t.marks {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(m.name)
		b.WriteString(";dur=")
		b.WriteString(strconv.FormatFloat(float64(m.dur.Microseconds())/1000, 'f', -1, 64))
	}
	return b.String()
}

// timerKey is the private context key under which a request's phaseTimer
// travels from ServeHTTP into the handlers.
type timerKey struct{}

// withTimer attaches a phaseTimer to a context.
func withTimer(ctx context.Context, t *phaseTimer) context.Context {
	return context.WithValue(ctx, timerKey{}, t)
}

// timerFrom returns the request's phaseTimer, or nil when timing is disabled.
func timerFrom(ctx context.Context) *phaseTimer {
	t, _ := ctx.Value(timerKey{}).(*phaseTimer)
	return t
}

// timingWriter sets the Server-Timing header from its phaseTimer the first time
// the response is committed, so every exit path (success, error, plan) carries
// the header without each call site knowing about it.
type timingWriter struct {
	http.ResponseWriter
	timer *phaseTimer
	wrote bool
}

func (tw *timingWriter) WriteHeader(code int) {
	if !tw.wrote {
		tw.wrote = true
		if h := tw.timer.header(); h != "" {
			tw.ResponseWriter.Header().Set("Server-Timing", h)
		}
	}
	tw.ResponseWriter.WriteHeader(code)
}

func (tw *timingWriter) Write(b []byte) (int, error) {
	if !tw.wrote {
		tw.WriteHeader(http.StatusOK)
	}
	return tw.ResponseWriter.Write(b)
}
