package oratype

import (
	"encoding/binary"
	"errors"
)

// IntervalDS is a decoded INTERVAL DAY TO SECOND.
type IntervalDS struct {
	Days    int32
	Hours   int32
	Minutes int32
	Seconds int32
	Nanos   int32
}

// IntervalYM is a decoded INTERVAL YEAR TO MONTH.
type IntervalYM struct {
	Years  int32
	Months int32
}

// DecodeIntervalDS decodes 11 bytes.
func DecodeIntervalDS(b []byte) (IntervalDS, error) {
	var v IntervalDS
	if len(b) < 11 {
		return v, errors.New("oratype: interval day to second too short")
	}
	v.Days = int32(int64(binary.BigEndian.Uint32(b)) - durationMid)
	v.Hours = int32(b[4]) - durationOffset
	v.Minutes = int32(b[5]) - durationOffset
	v.Seconds = int32(b[6]) - durationOffset
	v.Nanos = int32(int64(binary.BigEndian.Uint32(b[7:])) - durationMid)
	return v, nil
}

// DecodeIntervalYM decodes 5 bytes.
func DecodeIntervalYM(b []byte) (IntervalYM, error) {
	var v IntervalYM
	if len(b) < 5 {
		return v, errors.New("oratype: interval year to month too short")
	}
	v.Years = int32(int64(binary.BigEndian.Uint32(b)) - durationMid)
	v.Months = int32(b[4]) - durationOffset
	return v, nil
}

// EncodeIntervalDS encodes 11 bytes.
func EncodeIntervalDS(dst []byte, v IntervalDS) []byte {
	var b [11]byte
	binary.BigEndian.PutUint32(b[0:], uint32(int64(v.Days)+durationMid))
	b[4] = uint8(v.Hours + durationOffset)
	b[5] = uint8(v.Minutes + durationOffset)
	b[6] = uint8(v.Seconds + durationOffset)
	binary.BigEndian.PutUint32(b[7:], uint32(int64(v.Nanos)+durationMid))
	return append(dst, b[:]...)
}

// EncodeIntervalYM encodes 5 bytes.
func EncodeIntervalYM(dst []byte, v IntervalYM) []byte {
	var b [5]byte
	binary.BigEndian.PutUint32(b[0:], uint32(int64(v.Years)+durationMid))
	b[4] = uint8(v.Months + durationOffset)
	return append(dst, b[:]...)
}
