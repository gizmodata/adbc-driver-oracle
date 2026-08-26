package oratype

import (
	"errors"
	"math"
)

// DecodeBinaryDouble decodes 8 bytes of BINARY_DOUBLE.
func DecodeBinaryDouble(b []byte) (float64, error) {
	if len(b) < 8 {
		return 0, errors.New("oratype: binary double too short")
	}
	var v [8]byte
	copy(v[:], b[:8])
	if v[0]&0x80 != 0 {
		v[0] &= 0x7f
	} else {
		for i := range v {
			v[i] = ^v[i]
		}
	}
	bits := uint64(v[0])<<56 | uint64(v[1])<<48 | uint64(v[2])<<40 | uint64(v[3])<<32 |
		uint64(v[4])<<24 | uint64(v[5])<<16 | uint64(v[6])<<8 | uint64(v[7])
	return math.Float64frombits(bits), nil
}

// DecodeBinaryFloat decodes 4 bytes of BINARY_FLOAT.
func DecodeBinaryFloat(b []byte) (float32, error) {
	if len(b) < 4 {
		return 0, errors.New("oratype: binary float too short")
	}
	var v [4]byte
	copy(v[:], b[:4])
	if v[0]&0x80 != 0 {
		v[0] &= 0x7f
	} else {
		for i := range v {
			v[i] = ^v[i]
		}
	}
	bits := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	return math.Float32frombits(bits), nil
}

// EncodeBinaryDouble encodes a BINARY_DOUBLE.
func EncodeBinaryDouble(dst []byte, f float64) []byte {
	bits := math.Float64bits(f)
	var v [8]byte
	for i := 7; i >= 0; i-- {
		v[i] = uint8(bits)
		bits >>= 8
	}
	if v[0]&0x80 == 0 {
		v[0] |= 0x80
	} else {
		for i := range v {
			v[i] = ^v[i]
		}
	}
	return append(dst, v[:]...)
}

// EncodeBinaryFloat encodes a BINARY_FLOAT.
func EncodeBinaryFloat(dst []byte, f float32) []byte {
	bits := math.Float32bits(f)
	var v [4]byte
	for i := 3; i >= 0; i-- {
		v[i] = uint8(bits)
		bits >>= 8
	}
	if v[0]&0x80 == 0 {
		v[0] |= 0x80
	} else {
		for i := range v {
			v[i] = ^v[i]
		}
	}
	return append(dst, v[:]...)
}
