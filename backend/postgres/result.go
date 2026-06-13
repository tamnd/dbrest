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
		}
	}
	return vals
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
	wrote := false    // a field has been emitted
	prevNeg := false  // the previous emitted field was negative

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
