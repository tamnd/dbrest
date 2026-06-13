package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
)

// computeCount returns the total a read's Content-Range reports, by the strategy
// the request asked for (item 07.7):
//
//   - exact: count(*) over the same relation and predicates the body ran.
//   - planned: the planner's row estimate, read from EXPLAIN. Fast and
//     approximate, it never touches the heap.
//   - estimated: exact while the result is small, the planner estimate once it
//     grows past db-max-rows. The capped exact count stops at the threshold, so a
//     large table pays only for the estimate.
//
// PostgreSQL's EXPLAIN output is a stable, documented format; the estimate is the
// root plan node's Plan Rows, which for a plain SELECT is the predicted output
// row count.
func (b *Backend) computeCount(ctx context.Context, tx pgx.Tx, q *ir.Query) (int64, error) {
	switch q.Count {
	case ir.CountPlanned:
		return b.plannedCount(ctx, tx, q)
	case ir.CountEstimated:
		return b.estimatedCount(ctx, tx, q)
	default: // CountExact
		return b.exactCount(ctx, tx, q)
	}
}

// exactCount runs count(*) over the relation and predicates.
func (b *Backend) exactCount(ctx context.Context, tx pgx.Tx, q *ir.Query) (int64, error) {
	cst, apiErr := sqlgen.CompileCount(Dialect{}, q)
	if apiErr != nil {
		return 0, apiErr
	}
	var n int64
	if err := tx.QueryRow(ctx, cst.SQL, cst.Args...).Scan(&n); err != nil {
		return 0, b.MapError(err)
	}
	return n, nil
}

// plannedCount returns the planner's row estimate for the count's source query.
func (b *Backend) plannedCount(ctx context.Context, tx pgx.Tx, q *ir.Query) (int64, error) {
	src, apiErr := sqlgen.CompileRowEstimateSource(Dialect{}, q)
	if apiErr != nil {
		return 0, apiErr
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+src.SQL, src.Args...).Scan(&raw); err != nil {
		return 0, b.MapError(err)
	}
	rows, err := parseExplainRows(raw)
	if err != nil {
		return 0, b.MapError(err)
	}
	return rows, nil
}

// estimatedCount counts exactly until the result passes db-max-rows, then falls
// back to the planner estimate. With no threshold configured it is exact.
func (b *Backend) estimatedCount(ctx context.Context, tx pgx.Tx, q *ir.Query) (int64, error) {
	if q.CountMax <= 0 {
		return b.exactCount(ctx, tx, q)
	}
	src, apiErr := sqlgen.CompileRowEstimateSource(Dialect{}, q)
	if apiErr != nil {
		return 0, apiErr
	}
	// Count the source rows but stop one past the threshold: a result at or below
	// it is the exact total, while reaching threshold+1 only proves there are more,
	// at which point the planner estimate is cheaper and good enough.
	capped := fmt.Sprintf("SELECT count(*) FROM (%s LIMIT %d) _pgrst_capped",
		src.SQL, q.CountMax+1)
	var n int64
	if err := tx.QueryRow(ctx, capped, src.Args...).Scan(&n); err != nil {
		return 0, b.MapError(err)
	}
	if n <= q.CountMax {
		return n, nil
	}
	return b.plannedCount(ctx, tx, q)
}

// explainNode is the slice of EXPLAIN (FORMAT JSON) output the estimate needs:
// the top plan node carries the planner's output-row estimate in "Plan Rows".
type explainNode struct {
	Plan struct {
		PlanRows float64 `json:"Plan Rows"`
	} `json:"Plan"`
}

// parseExplainRows reads the root node's row estimate out of EXPLAIN (FORMAT
// JSON) output, which is a one-element array of plan trees. The estimate is a
// float in the plan (PostgreSQL prints fractional estimates), rounded to the
// nearest whole row.
func parseExplainRows(raw []byte) (int64, error) {
	var plans []explainNode
	if err := json.Unmarshal(raw, &plans); err != nil {
		return 0, fmt.Errorf("parse EXPLAIN output: %w", err)
	}
	if len(plans) == 0 {
		return 0, fmt.Errorf("EXPLAIN output held no plan")
	}
	rows := plans[0].Plan.PlanRows
	if rows < 0 {
		rows = 0
	}
	return int64(rows + 0.5), nil
}
