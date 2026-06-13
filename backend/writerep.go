package backend

import (
	"encoding/json"
	"sort"

	"github.com/tamnd/dbrest/ir"
)

// ShapeWriteRepresentation orders and paginates the rows a mutation returns for
// its representation. PostgREST v13 dropped limited update/delete (#3013), so
// order, limit and offset never bound the mutation itself: every matching row
// is written. The affected count the caller has already taken stays the full
// total, so Prefer: max-affected and the write Content-Range are unchanged.
// These query parameters only shape the returned body, matching v14 where the
// mutation's RETURNING is wrapped in an ordered, limited outer select (see
// PostgREST UpdateSpec "with ordering on top-level resource").
//
// Ordering compares the buffered values directly, which matches an engine's
// binary/C collation; under a locale-aware text collation a column's order can
// differ, a representation-layer divergence. A term whose column is not in the
// returned projection is skipped, since the buffered representation cannot carry
// a value it never selected.
func ShapeWriteRepresentation(cols []string, rows [][]any, q *ir.Query) [][]any {
	if q == nil || len(rows) == 0 {
		return rows
	}
	rows = orderWriteRows(cols, rows, q.Order)
	rows = pageWriteRows(rows, q.Limit, q.Offset)
	return rows
}

// orderSortKey binds an order term to the column index it sorts on.
type orderSortKey struct {
	idx        int
	desc       bool
	nullsFirst bool
}

// orderWriteRows stably sorts the buffered rows by the plain-column order terms
// that name a returned column. JSON-path terms and terms whose column is absent
// from the projection are skipped (the representation does not carry them).
func orderWriteRows(cols []string, rows [][]any, terms []ir.OrderTerm) [][]any {
	if len(terms) == 0 {
		return rows
	}
	index := make(map[string]int, len(cols))
	for i, c := range cols {
		index[c] = i
	}
	var keys []orderSortKey
	for _, t := range terms {
		if len(t.Path) != 1 || t.Last != ir.JSONNone {
			continue // a JSON sub-path is not a plain returned column
		}
		i, ok := index[t.Path[0]]
		if !ok {
			continue
		}
		// PostgreSQL default: NULLS LAST for ascending, NULLS FIRST for
		// descending; an explicit nullsfirst/nullslast modifier overrides it.
		nullsFirst := t.Desc
		if t.NullsFirst != nil {
			nullsFirst = *t.NullsFirst
		}
		keys = append(keys, orderSortKey{idx: i, desc: t.Desc, nullsFirst: nullsFirst})
	}
	if len(keys) == 0 {
		return rows
	}
	sort.SliceStable(rows, func(a, b int) bool {
		for _, k := range keys {
			av, bv := rows[a][k.idx], rows[b][k.idx]
			aNull, bNull := av == nil, bv == nil
			if aNull || bNull {
				if aNull && bNull {
					continue
				}
				// NULL placement is absolute: descending reverses the value
				// order but not which end the NULLs land on.
				if aNull {
					return k.nullsFirst
				}
				return !k.nullsFirst
			}
			cmp := compareCells(av, bv)
			if cmp == 0 {
				continue
			}
			if k.desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	return rows
}

// compareCells orders two non-NULL buffered cell values: numbers compare
// numerically, booleans false-before-true, and everything else by its text form
// (matching binary/C collation).
func compareCells(a, b any) int {
	if af, aok := cellFloat(a); aok {
		if bf, bok := cellFloat(b); bok {
			switch {
			case af < bf:
				return -1
			case af > bf:
				return 1
			default:
				return 0
			}
		}
	}
	if ab, aok := a.(bool); aok {
		if bb, bok := b.(bool); bok {
			switch {
			case ab == bb:
				return 0
			case !ab:
				return -1
			default:
				return 1
			}
		}
	}
	as, bs := cellString(a), cellString(b)
	switch {
	case as < bs:
		return -1
	case as > bs:
		return 1
	default:
		return 0
	}
}

// cellFloat reports a numeric value for the integer and float types the drivers
// decode into, so numeric columns sort by magnitude rather than text.
func cellFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// cellString renders a cell to its text form for collation and as the fallback
// when two cells are of unlike kinds.
func cellString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case json.RawMessage:
		return string(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// pageWriteRows applies offset then limit to the ordered representation. A nil
// bound leaves that end open; an offset past the end yields no rows.
func pageWriteRows(rows [][]any, limit, offset *int) [][]any {
	if offset != nil {
		o := max(*offset, 0)
		if o >= len(rows) {
			return rows[:0]
		}
		rows = rows[o:]
	}
	if limit != nil {
		l := max(*limit, 0)
		if l < len(rows) {
			rows = rows[:l]
		}
	}
	return rows
}
