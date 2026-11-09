package oratype

import (
	"testing"
	"time"
)

// Oracle DATE/TIMESTAMP cover years 1..9999. Milli- and microsecond counts of
// that whole range fit in an int64; going through nanoseconds (which only
// reach 1677..2262) silently wrapped 0001-01-01 and 9999-12-31 into the 1700s
// and 1800s.
func TestUnixUnitFullOracleRange(t *testing.T) {
	cases := []struct {
		d         Date
		perSecond int64
		want      int64
	}{
		{Date{Year: 1, Month: 1, Day: 1}, 1e3, -62135596800000},
		{Date{Year: 9999, Month: 12, Day: 31, Hour: 23, Minute: 59, Second: 59, Nanosecond: 999999999}, 1e3, 253402300799999},
		{Date{Year: 1, Month: 1, Day: 1}, 1e6, -62135596800000000},
		{Date{Year: 9999, Month: 12, Day: 31, Hour: 23, Minute: 59, Second: 59, Nanosecond: 999999999}, 1e6, 253402300799999999},
		{Date{Year: 1969, Month: 12, Day: 31, Hour: 23, Minute: 59, Second: 59, Nanosecond: 500000000}, 1e3, -500},
		{Date{Year: 2023, Month: 5, Day: 15, Hour: 13, Minute: 45, Second: 30, Nanosecond: 123456789}, 1e9, 1684158330123456789},
		{Date{Year: 2023, Month: 5, Day: 15, Hour: 13, Minute: 45, Second: 30, Nanosecond: 123456789}, 1, 1684158330},
	}
	for _, c := range cases {
		got, ok := c.d.UnixUnit(c.perSecond)
		if !ok || got != c.want {
			t.Errorf("%+v.UnixUnit(%d) = %d, %v; want %d, true", c.d, c.perSecond, got, ok, c.want)
		}
		if c.perSecond == 1e3 {
			if back, want := time.UnixMilli(got).UTC(), c.d.Time().Truncate(time.Millisecond); !back.Equal(want) {
				t.Errorf("UnixMilli(%d) = %s, want %s", got, back, want)
			}
		}
	}
}

func TestUnixUnitNanosecondsOutOfRange(t *testing.T) {
	for _, d := range []Date{{Year: 1, Month: 1, Day: 1}, {Year: 9999, Month: 12, Day: 31}, {Year: 1677, Month: 9, Day: 21}, {Year: 2262, Month: 4, Day: 12}} {
		if v, ok := d.UnixUnit(1e9); ok {
			t.Errorf("%+v.UnixUnit(1e9) = %d, true; want out of range", d, v)
		}
	}
	// The edges of the int64-nanosecond range still convert.
	if v, ok := (Date{Year: 1677, Month: 9, Day: 21, Minute: 12, Second: 43, Nanosecond: 145224191}).UnixUnit(1e9); ok {
		t.Errorf("one nanosecond below MinInt64: got %d, true; want out of range", v)
	}
	for _, d := range []Date{
		{Year: 1677, Month: 9, Day: 21, Minute: 12, Second: 43, Nanosecond: 145224192}, // exactly MinInt64 ns
		{Year: 1677, Month: 9, Day: 21, Minute: 12, Second: 44},
		{Year: 2262, Month: 4, Day: 11, Hour: 23, Minute: 47, Second: 16, Nanosecond: 854775807}, // exactly MaxInt64 ns
	} {
		if v, ok := d.UnixUnit(1e9); !ok || v != d.Time().UnixNano() {
			t.Errorf("%+v.UnixUnit(1e9) = %d, %v; want %d", d, v, ok, d.Time().UnixNano())
		}
	}
}
