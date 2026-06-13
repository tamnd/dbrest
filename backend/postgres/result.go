package postgres

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
)

// streamResult adapts an open pgx cursor to the backend.Result contract for a
// read. The role and request GUCs are transaction-local, so the cursor must stay
// inside its transaction while it streams; the result therefore owns the
// transaction and commits it when the renderer closes the stream. A read commits
// rather than rolls back so that anything a stable function did in passing (a
// session setting, say) is not needlessly discarded; a read changes no data, so
// the choice is immaterial to durability.
type streamResult struct {
	ctx      context.Context
	tx       pgx.Tx
	rows     pgx.Rows
	cols     []string
	controls *reqctx.ResponseControls
	count    int64
	hasCount bool
	loc      *time.Location
}

func (r *streamResult) Body() io.Reader { return nil }
func (r *streamResult) Rows() backend.RowStream {
	return &streamRows{ctx: r.ctx, tx: r.tx, rows: r.rows, cols: r.cols, loc: r.loc}
}
func (r *streamResult) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *streamResult) Affected() (int64, bool)                    { return 0, false }
func (r *streamResult) ResponseControls() *reqctx.ResponseControls { return r.controls }

// streamRows is a forward-only cursor over a read's rows. Close releases the
// cursor and commits the owning transaction; the transaction is committed once,
// on the renderer's deferred Close.
type streamRows struct {
	ctx  context.Context
	tx   pgx.Tx
	rows pgx.Rows
	cols []string
	loc  *time.Location
}

func (s *streamRows) Columns() []string { return s.cols }
func (s *streamRows) Next() bool        { return s.rows.Next() }
func (s *streamRows) Err() error        { return s.rows.Err() }

// Values returns the current row decoded to Go types. pgx maps PostgreSQL types
// to Go values (int for the integer types, float64, bool, string, time.Time,
// []byte for json/jsonb and bytea, and slices for arrays); normalizeValues folds
// the byte forms to the shapes the renderer expects.
func (s *streamRows) Values() ([]any, error) {
	vals, err := s.rows.Values()
	if err != nil {
		return nil, err
	}
	return normalizeValues(vals, s.rows.FieldDescriptions(), s.loc), nil
}

// Close releases the cursor and commits the transaction that scoped the role and
// request GUCs. Committing a read-only transaction is cheap and never fails on a
// healthy connection; a commit error is returned so the renderer can surface it.
func (s *streamRows) Close() error {
	s.rows.Close()
	if err := s.rows.Err(); err != nil {
		_ = s.tx.Rollback(s.ctx)
		return err
	}
	return s.tx.Commit(s.ctx)
}

// bufResult holds the buffered outcome of a write or a function call. A write
// runs inside a transaction that must commit (or roll back, under tx=rollback)
// before the response is sent, and a function call's response headers and status
// are read back from GUCs after the call returns, so both buffer their rows into
// memory rather than streaming from an open cursor. The buffer also lets the
// handler iterate twice (once for the Location primary key, once for the body)
// without re-running the statement.
type bufResult struct {
	cols     []string
	rows     [][]any
	affected int64
	hasAff   bool
	count    int64
	hasCount bool
	controls *reqctx.ResponseControls
}

func (r *bufResult) Body() io.Reader { return nil }
func (r *bufResult) Rows() backend.RowStream {
	return &bufStream{cols: r.cols, rows: r.rows, i: -1}
}
func (r *bufResult) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *bufResult) Affected() (int64, bool)                    { return r.affected, r.hasAff }
func (r *bufResult) ResponseControls() *reqctx.ResponseControls { return r.controls }

// bufStream replays buffered rows. Each call to Rows starts a fresh cursor at the
// first row, so a result can be iterated more than once.
type bufStream struct {
	cols []string
	rows [][]any
	i    int
}

func (s *bufStream) Columns() []string      { return s.cols }
func (s *bufStream) Next() bool             { s.i++; return s.i < len(s.rows) }
func (s *bufStream) Values() ([]any, error) { return s.rows[s.i], nil }
func (s *bufStream) Err() error             { return nil }
func (s *bufStream) Close() error           { return nil }

// normalizeValues adjusts pgx's decoded values to the shapes the renderer maps to
// JSON, so the wire value matches the JSON PostgREST assembles inside the
// database. json and jsonb arrive as raw bytes turned into strings so raw-JSON
// columns pass through verbatim; a bytea value also arrives as bytes and renders
// as a string. Temporal columns are formatted by OID to PostgreSQL's own JSON
// spellings rather than left to Go's default struct/RFC3339 marshalling:
//
//   - date (1082): "2006-01-02".
//   - time (1083): pgx returns pgtype.Time, which json would render as a struct;
//     format as "HH:MM:SS[.ffffff]".
//   - timetz (1266): pgx already returns the correct "HH:MM:SS[.ffffff]+TZ"
//     string, so it passes through.
//   - interval (1186): pgx returns pgtype.Interval; format in PostgreSQL's
//     default (postgres) IntervalStyle.
//   - timestamp (1114): no zone, so format the wall clock as
//     "2006-01-02T15:04:05[.ffffff]" with no suffix (Go would append "Z").
//   - timestamptz (1184): render the instant in the server TimeZone with an ISO
//     "+HH:MM" offset, matching PostgreSQL (Go's RFC3339 emits "Z" for UTC).
//   - range / multirange: pgx returns pgtype.Range / pgtype.Multirange structs;
//     format them to PostgreSQL's own range text ("[10,20)", "{[1,2),[5,8)}")
//     rather than the Go struct json would marshal.
//
// loc is the server TimeZone; a nil loc defaults to UTC. Every other value is
// left as pgx decoded it.
func normalizeValues(vals []any, fields []pgconn.FieldDescription, loc *time.Location) []any {
	if loc == nil {
		loc = time.UTC
	}
	for i, v := range vals {
		var oid uint32
		if i < len(fields) {
			oid = fields[i].DataTypeOID
		}
		switch t := v.(type) {
		case []byte:
			vals[i] = string(t)
		case time.Time:
			switch oid {
			case pgtype.DateOID:
				vals[i] = t.Format("2006-01-02")
			case pgtype.TimestamptzOID:
				vals[i] = t.In(loc).Format("2006-01-02T15:04:05.999999-07:00")
			case pgtype.TimestampOID:
				vals[i] = t.Format("2006-01-02T15:04:05.999999")
			}
		case pgtype.Time:
			if t.Valid {
				vals[i] = formatTimeOfDay(t.Microseconds)
			}
		case pgtype.Interval:
			if t.Valid {
				vals[i] = formatInterval(t)
			}
		case pgtype.Range[any]:
			if t.Valid {
				vals[i] = formatRange(t, oid, loc)
			}
		case pgtype.Multirange[pgtype.Range[any]]:
			vals[i] = formatMultirange(t, oid, loc)
		}
	}
	return vals
}

// formatRange renders a range value as PostgreSQL's own text output (the spelling
// `col::text` produces and the form PostgREST emits in JSON), instead of the
// pgtype.Range Go struct json would otherwise marshal. An empty range is "empty";
// otherwise the bracket reflects each bound's inclusivity ('[' / '(' lower, ']' /
// ')' upper), an unbounded side renders as the empty string, and each present
// bound is formatted by the range's element type and quoted by PostgreSQL's range
// rules. oid is the range column OID, which selects the element formatting.
func formatRange(r pgtype.Range[any], oid uint32, loc *time.Location) string {
	if r.LowerType == pgtype.Empty || r.UpperType == pgtype.Empty {
		return "empty"
	}
	var sb strings.Builder
	if r.LowerType == pgtype.Inclusive {
		sb.WriteByte('[')
	} else {
		sb.WriteByte('(')
	}
	if r.LowerType != pgtype.Unbounded {
		sb.WriteString(quoteRangeBound(formatRangeElem(r.Lower, oid, loc)))
	}
	sb.WriteByte(',')
	if r.UpperType != pgtype.Unbounded {
		sb.WriteString(quoteRangeBound(formatRangeElem(r.Upper, oid, loc)))
	}
	if r.UpperType == pgtype.Inclusive {
		sb.WriteByte(']')
	} else {
		sb.WriteByte(')')
	}
	return sb.String()
}

// formatMultirange renders a multirange as PostgreSQL's text output: the
// brace-wrapped, comma-separated list of its member ranges. Each member is
// formatted with the corresponding range OID so its element type is rendered the
// same as a bare range.
func formatMultirange(m pgtype.Multirange[pgtype.Range[any]], oid uint32, loc *time.Location) string {
	relemOID := multirangeRangeOID(oid)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, r := range m {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(formatRange(r, relemOID, loc))
	}
	sb.WriteByte('}')
	return sb.String()
}

// multirangeRangeOID maps a multirange OID to the OID of its member range type,
// so formatRange formats each member's bounds by the right element type.
func multirangeRangeOID(oid uint32) uint32 {
	switch oid {
	case pgtype.Int4multirangeOID:
		return pgtype.Int4rangeOID
	case pgtype.Int8multirangeOID:
		return pgtype.Int8rangeOID
	case pgtype.NummultirangeOID:
		return pgtype.NumrangeOID
	case pgtype.DatemultirangeOID:
		return pgtype.DaterangeOID
	case pgtype.TsmultirangeOID:
		return pgtype.TsrangeOID
	case pgtype.TstzmultirangeOID:
		return pgtype.TstzrangeOID
	}
	return 0
}

// formatRangeElem renders one range bound by the range's element type. Temporal
// element types are formatted to PostgreSQL's range text spelling (which uses the
// raw timestamp output, not the ISO json spelling, so the timestamptz offset is
// "+07" rather than "+07:00"); numeric elements use their decimal text, and the
// rest fall back to their default string form.
func formatRangeElem(v any, oid uint32, loc *time.Location) string {
	switch oid {
	case pgtype.DaterangeOID, pgtype.DatemultirangeOID:
		if t, ok := v.(time.Time); ok {
			return t.Format("2006-01-02")
		}
	case pgtype.TsrangeOID, pgtype.TsmultirangeOID:
		if t, ok := v.(time.Time); ok {
			return t.Format("2006-01-02 15:04:05.999999")
		}
	case pgtype.TstzrangeOID, pgtype.TstzmultirangeOID:
		if t, ok := v.(time.Time); ok {
			return formatTimestamptzText(t, loc)
		}
	}
	switch x := v.(type) {
	case pgtype.Numeric:
		if b, err := x.MarshalJSON(); err == nil {
			return string(b)
		}
	case []byte:
		return string(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// formatTimestamptzText renders a timestamptz the way PostgreSQL's text output
// (and thus range text) does: the wall clock in the server zone followed by a
// signed offset that carries minutes only when non-zero and seconds rarer still,
// e.g. "+07", "+05:30". This differs from the ISO "+07:00" json spelling used for
// a bare timestamptz column.
func formatTimestamptzText(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	base := t.Format("2006-01-02 15:04:05.999999")
	_, off := t.Zone()
	sign := byte('+')
	if off < 0 {
		sign = '-'
		off = -off
	}
	h := off / 3600
	m := (off % 3600) / 60
	s := off % 60
	out := fmt.Sprintf("%s%c%02d", base, sign, h)
	if m != 0 || s != 0 {
		out += fmt.Sprintf(":%02d", m)
	}
	if s != 0 {
		out += fmt.Sprintf(":%02d", s)
	}
	return out
}

// quoteRangeBound quotes a range bound the way PostgreSQL does: an empty string
// or one containing a comma, brackets, parentheses, a quote, a backslash, or
// whitespace is double-quoted with embedded quotes and backslashes escaped;
// anything else is left bare.
func quoteRangeBound(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, "(),[]\"\\ \t\n\r") {
		return s
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	sb.WriteByte('"')
	return sb.String()
}

// formatTimeOfDay renders a time-of-day microsecond count as PostgreSQL's JSON
// time spelling "HH:MM:SS" with a fractional part only when non-zero, trailing
// zeros trimmed (so 13:00:00.5, not 13:00:00.500000).
func formatTimeOfDay(micros int64) string {
	h := micros / 3_600_000_000
	m := (micros / 60_000_000) % 60
	s := (micros / 1_000_000) % 60
	frac := micros % 1_000_000
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if frac != 0 {
		out += strings.TrimRight(fmt.Sprintf(".%06d", frac), "0")
	}
	return out
}

// formatInterval renders a pgtype.Interval in PostgreSQL's default (postgres)
// IntervalStyle, matching EncodeInterval: each non-zero year/month/day field is
// emitted with its unit (pluralized unless the value is exactly 1), a field
// after a negative one gets an explicit leading "+" when positive, and the time
// part carries a "-" when negative or a "+" when it follows a negative field.
// The all-zero interval is "00:00:00". Months fold to years (12 per year).
func formatInterval(iv pgtype.Interval) string {
	years := iv.Months / 12
	mons := iv.Months % 12

	var sb strings.Builder
	wrote := false   // a field has been emitted
	prevNeg := false // the previous emitted field was negative

	addInt := func(value int32, unit string) {
		if value == 0 {
			return
		}
		if wrote {
			sb.WriteByte(' ')
		}
		if prevNeg && value > 0 {
			sb.WriteByte('+')
		}
		plural := "s"
		if value == 1 {
			plural = ""
		}
		fmt.Fprintf(&sb, "%d %s%s", value, unit, plural)
		prevNeg = value < 0
		wrote = true
	}

	addInt(years, "year")
	addInt(mons, "mon")
	addInt(iv.Days, "day")

	micros := iv.Microseconds
	if !wrote || micros != 0 {
		neg := micros < 0
		abs := micros
		if neg {
			abs = -micros
		}
		h := abs / 3_600_000_000
		m := (abs / 60_000_000) % 60
		s := (abs / 1_000_000) % 60
		frac := abs % 1_000_000
		if wrote {
			sb.WriteByte(' ')
		}
		switch {
		case neg:
			sb.WriteByte('-')
		case prevNeg:
			sb.WriteByte('+')
		}
		fmt.Fprintf(&sb, "%02d:%02d:%02d", h, m, s)
		if frac != 0 {
			sb.WriteString(strings.TrimRight(fmt.Sprintf(".%06d", frac), "0"))
		}
	}
	return sb.String()
}
