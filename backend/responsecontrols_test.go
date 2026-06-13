package backend

import (
	"testing"

	"github.com/tamnd/dbrest/reqctx"
)

func TestLiftResponseControlsNoReservedColumns(t *testing.T) {
	cols := []string{"id", "title"}
	rows := [][]any{{int64(1), "a"}}
	var c reqctx.ResponseControls
	gotCols, gotRows := LiftResponseControls(cols, rows, &c)
	if len(gotCols) != 2 || len(gotRows) != 1 {
		t.Fatalf("result reshaped without reserved columns: %v %v", gotCols, gotRows)
	}
	if c.Status != 0 {
		t.Errorf("status set without a reserved column: %d", c.Status)
	}
}

func TestLiftResponseControlsStatusAndStrip(t *testing.T) {
	cols := []string{"message", ColResponseStatus}
	rows := [][]any{{"gone", int64(410)}}
	var c reqctx.ResponseControls
	gotCols, gotRows := LiftResponseControls(cols, rows, &c)
	if len(gotCols) != 1 || gotCols[0] != "message" {
		t.Errorf("columns = %v, want [message]", gotCols)
	}
	if len(gotRows[0]) != 1 || gotRows[0][0] != "gone" {
		t.Errorf("row = %v, want [gone]", gotRows[0])
	}
	if c.Status != 410 {
		t.Errorf("status = %d, want 410", c.Status)
	}
}

func TestLiftResponseControlsStatusFromString(t *testing.T) {
	cols := []string{ColResponseStatus}
	rows := [][]any{{"201"}}
	var c reqctx.ResponseControls
	LiftResponseControls(cols, rows, &c)
	if c.Status != 201 {
		t.Errorf("status = %d, want 201", c.Status)
	}
}

func TestLiftResponseControlsHeadersArray(t *testing.T) {
	cols := []string{ColResponseHeaders}
	rows := [][]any{{`[{"X-A":"1"},{"X-B":"2"}]`}}
	var c reqctx.ResponseControls
	LiftResponseControls(cols, rows, &c)
	if c.Headers["X-A"] != "1" || c.Headers["X-B"] != "2" {
		t.Errorf("headers = %v, want X-A=1 X-B=2", c.Headers)
	}
}

func TestLiftResponseControlsHeadersObject(t *testing.T) {
	cols := []string{ColResponseHeaders}
	rows := [][]any{{`{"X-A":"1"}`}}
	var c reqctx.ResponseControls
	LiftResponseControls(cols, rows, &c)
	if c.Headers["X-A"] != "1" {
		t.Errorf("headers = %v, want X-A=1", c.Headers)
	}
}

func TestLiftResponseControlsNoRowsStillStrips(t *testing.T) {
	cols := []string{"message", ColResponseStatus}
	var c reqctx.ResponseControls
	gotCols, gotRows := LiftResponseControls(cols, nil, &c)
	if len(gotCols) != 1 || gotCols[0] != "message" {
		t.Errorf("columns = %v, want [message]", gotCols)
	}
	if len(gotRows) != 0 {
		t.Errorf("rows = %v, want empty", gotRows)
	}
	if c.Status != 0 {
		t.Errorf("status set from an empty result: %d", c.Status)
	}
}

func TestHasResponseControlCols(t *testing.T) {
	if HasResponseControlCols([]string{"a", "b"}) {
		t.Error("false positive on plain columns")
	}
	if !HasResponseControlCols([]string{"a", ColResponseStatus}) {
		t.Error("missed response.status")
	}
	if !HasResponseControlCols([]string{ColResponseHeaders}) {
		t.Error("missed response.headers")
	}
}
