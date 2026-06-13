package sqlite

import "testing"

// SQLite's LIKE folds ASCII case by default, which would make the like operator
// silently case-insensitive and return different rows than PostgreSQL. The pool
// sets PRAGMA case_sensitive_like = ON to fix that. A lowercase pattern must not
// match the title-cased row through like. Finding 01-M08.
func TestLikeIsCaseSensitive(t *testing.T) {
	b := openSeeded(t)
	pl := planRead(t, b, "title=like.blade*")
	rows := execRead(t, b, pl.Query)
	if len(rows) != 0 {
		t.Fatalf("like.blade* matched %d rows, want 0 (case-sensitive); got %v", len(rows), rows)
	}

	pl = planRead(t, b, "title=like.Blade*")
	rows = execRead(t, b, pl.Query)
	if len(rows) != 1 {
		t.Fatalf("like.Blade* matched %d rows, want 1", len(rows))
	}
	if got := rows[0]["title"]; got != "Blade Runner" {
		t.Fatalf("like.Blade* matched %v, want Blade Runner", got)
	}
}

// ilike stays case-insensitive even though case_sensitive_like is ON, because
// the dialect folds both sides with lower(). A lowercase pattern matches the
// title-cased row. Finding 01-M08.
func TestILikeIsCaseInsensitive(t *testing.T) {
	b := openSeeded(t)
	pl := planRead(t, b, "title=ilike.blade*")
	rows := execRead(t, b, pl.Query)
	if len(rows) != 1 {
		t.Fatalf("ilike.blade* matched %d rows, want 1", len(rows))
	}
	if got := rows[0]["title"]; got != "Blade Runner" {
		t.Fatalf("ilike.blade* matched %v, want Blade Runner", got)
	}
}
