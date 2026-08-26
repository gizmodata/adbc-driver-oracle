package oratype

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	tzHourOffset   = 20
	tzMinuteOffset = 60
	hasRegionID    = 0x80
	durationMid    = 0x80000000
	durationOffset = 60
)

// ErrNamedTimezone is returned for TIMESTAMP WITH TIME ZONE values that
// carry a named region rather than an offset.
var ErrNamedTimezone = errors.New("oratype: named time zone regions are not supported")

// Date is a decoded DATE/TIMESTAMP value.
type Date struct {
	Year        int
	Month       int
	Day         int
	Hour        int
	Minute      int
	Second      int
	Nanosecond  int
	TZHourOff   int
	TZMinuteOff int
	HasTZ       bool
}

// DecodeDate decodes 7 (DATE), 11 (TIMESTAMP) or 13 (TIMESTAMP TZ) bytes.
func DecodeDate(b []byte) (Date, error) {
	var d Date
	if len(b) < 7 {
		return d, errors.New("oratype: date value too short")
	}
	d.Year = (int(b[0])-100)*100 + int(b[1]) - 100
	d.Month = int(b[2])
	d.Day = int(b[3])
	d.Hour = int(b[4]) - 1
	d.Minute = int(b[5]) - 1
	d.Second = int(b[6]) - 1
	if len(b) >= 11 {
		d.Nanosecond = int(binary.BigEndian.Uint32(b[7:]))
	}
	if len(b) > 11 && b[11] != 0 && b[12] != 0 {
		if b[11]&hasRegionID != 0 {
			// Region-id timezone: the value is expressed in UTC; treat as UTC.
			d.HasTZ = true
			d.TZHourOff, d.TZMinuteOff = 0, 0
		} else {
			d.HasTZ = true
			d.TZHourOff = int(b[11]) - tzHourOffset
			d.TZMinuteOff = int(b[12]) - tzMinuteOffset
		}
	}
	return d, nil
}

// UnixNanos returns the value as nanoseconds since the Unix epoch. Oracle
// transmits TIMESTAMP WITH TIME ZONE values as UTC wall-clock plus the
// display offset, so the fields are always interpreted as UTC; naive
// DATE/TIMESTAMP values are treated as UTC wall-clock too.
func (d Date) UnixNanos() int64 {
	return time.Date(d.Year, time.Month(d.Month), d.Day, d.Hour, d.Minute, d.Second, d.Nanosecond, time.UTC).UnixNano()
}

// Unix returns whole seconds since the epoch.
func (d Date) Unix() int64 {
	return time.Date(d.Year, time.Month(d.Month), d.Day, d.Hour, d.Minute, d.Second, 0, time.UTC).Unix()
}

// EncodeDate encodes a wall-clock time (fields taken from t in its own
// location) as 7 bytes.
func EncodeDate(dst []byte, t time.Time) []byte {
	y := t.Year()
	return append(dst,
		uint8(y/100+100), uint8(y%100+100),
		uint8(t.Month()), uint8(t.Day()),
		uint8(t.Hour()+1), uint8(t.Minute()+1), uint8(t.Second()+1))
}

// EncodeTimestamp encodes an 11-byte TIMESTAMP. If the fractional seconds
// are zero the protocol requires the 7-byte DATE form, which is what is
// returned in that case.
func EncodeTimestamp(dst []byte, t time.Time) []byte {
	dst = EncodeDate(dst, t)
	ns := uint32(t.Nanosecond())
	if ns == 0 {
		return dst
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ns)
	return append(dst, b[:]...)
}

// EncodeTimestampTZ encodes a 13-byte TIMESTAMP WITH TIME ZONE using the
// UTC representation (offset +00:00).
func EncodeTimestampTZ(dst []byte, t time.Time) []byte {
	t = t.UTC()
	dst = EncodeDate(dst, t)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(t.Nanosecond()))
	dst = append(dst, b[:]...)
	return append(dst, tzHourOffset, tzMinuteOffset)
}
