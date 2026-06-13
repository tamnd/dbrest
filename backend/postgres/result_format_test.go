package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The expected strings below are PostgreSQL's own to_json spellings, captured
// from a live server (IntervalStyle=postgres). formatInterval and formatTimeOfDay
// must reproduce them exactly so a temporal column renders on the wire the way
// PostgREST renders it. Finding 03-P07 / E05.

func TestFormatInterval(t *testing.T) {
	cases := []struct {
		months, days int32
		micros       int64
		want         string
	}{
		{0, 0, 90000000, "00:01:30"},
		{0, 0, -90000000, "-00:01:30"},
		{0, 1, 90000000, "1 day 00:01:30"},
		{14, 3, 14706000000, "1 year 2 mons 3 days 04:05:06"},
		{1, 0, 0, "1 mon"},
		{0, 2, 0, "2 days"},
		{0, -1, -7384000000, "-1 days -02:03:04"},
		{0, 1, 7384500000, "1 day 02:03:04.5"},
		{13, 0, 0, "1 year 1 mon"},
		{12, 0, 0, "1 year"},
		{-14, 0, 0, "-1 years -2 mons"},
		{0, 3, -14706000000, "3 days -04:05:06"},
		{0, 0, 0, "00:00:00"},
		{0, 0, 5400000000, "01:30:00"},
		{-1, 2, 0, "-1 mons +2 days"},
		{0, 0, 86400000000, "24:00:00"},
		{2, -3, 0, "2 mons -3 days"},
		{0, -3, 90000000, "-3 days +00:01:30"},
		{-1, 0, 90000000, "-1 mons +00:01:30"},
		{1, -2, -4000000, "1 mon -2 days -00:00:04"},
		{0, -3, 14706000000, "-3 days +04:05:06"},
		{-2, 3, -4000000, "-2 mons +3 days -00:00:04"},
		{0, 2, -90000000, "2 days -00:01:30"},
		{0, -2, 90000000, "-2 days +00:01:30"},
		{11, 0, 0, "11 mons"},
		{-11, 0, 0, "-11 mons"},
		{5, -10, 1000000, "5 mons -10 days +00:00:01"},
		{0, 0, 1, "00:00:00.000001"},
		{0, 0, -500000, "-00:00:00.5"},
	}
	for _, c := range cases {
		iv := pgtype.Interval{Months: c.months, Days: c.days, Microseconds: c.micros, Valid: true}
		if got := formatInterval(iv); got != c.want {
			t.Errorf("formatInterval(mon=%d days=%d us=%d) = %q, want %q", c.months, c.days, c.micros, got, c.want)
		}
	}
}

func TestFormatTimeOfDay(t *testing.T) {
	cases := []struct {
		micros int64
		want   string
	}{
		{46800500000, "13:00:00.5"},
		{46800000000, "13:00:00"},
		{0, "00:00:00"},
		{1, "00:00:00.000001"},
		{86399999999, "23:59:59.999999"},
	}
	for _, c := range cases {
		if got := formatTimeOfDay(c.micros); got != c.want {
			t.Errorf("formatTimeOfDay(%d) = %q, want %q", c.micros, got, c.want)
		}
	}
}
