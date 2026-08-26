// Package oratype implements the on-the-wire encodings of Oracle's native
// data types (NUMBER, DATE/TIMESTAMP, INTERVAL, BINARY_FLOAT/DOUBLE, ROWID)
// independent of any transport.
package oratype

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

const (
	numberMaxDigits = 40
	numberMaxChars  = 172
)

// ErrNumberTooLarge is returned when an encoded NUMBER exceeds 21 bytes.
var ErrNumberTooLarge = errors.New("oratype: number too large")

// DecodedNumber is the textual form of a decoded Oracle NUMBER: an
// optional '-', digits, optional '.' and digits. It never uses exponent
// notation and has no leading/trailing zero padding beyond a single "0".
type DecodedNumber struct {
	Text               []byte // e.g. "-123.45"
	IsInteger          bool
	IsMaxNegativeValue bool // the special value -1e126 (no text)
}

// DecodeNumber decodes the wire bytes of an Oracle NUMBER into decimal text.
// out is used as scratch storage and may be reused between calls.
func DecodeNumber(b []byte, out []byte) (DecodedNumber, error) {
	var res DecodedNumber
	n := len(b)
	if n > 21 {
		return res, ErrNumberTooLarge
	}
	if n == 0 {
		return res, errors.New("oratype: empty number")
	}
	// The exponent byte is interpreted with wrapping int8 arithmetic,
	// exactly as the reference C implementation does.
	exponent := b[0]
	isPositive := exponent&0x80 != 0
	if !isPositive {
		exponent = ^exponent
	}
	exp := int(int8(exponent - 193))
	decimalPointIndex := exp*2 + 2

	out = out[:0]
	res.IsInteger = true
	if n == 1 {
		if isPositive {
			res.Text = append(out, '0')
			return res, nil
		}
		res.IsMaxNegativeValue = true
		return res, nil
	}
	if !isPositive && b[n-1] == 102 {
		n--
	}
	var digits [numberMaxDigits + 2]uint8
	numDigits := 0
	for i := 1; i < n; i++ {
		var byt uint8
		if isPositive {
			byt = b[i] - 1
		} else {
			byt = 101 - b[i]
		}
		digit := byt / 10
		if digit == 0 && numDigits == 0 {
			decimalPointIndex--
		} else if digit == 10 {
			digits[numDigits] = 1
			digits[numDigits+1] = 0
			numDigits += 2
			decimalPointIndex++
		} else if digit != 0 || i > 0 {
			digits[numDigits] = digit
			numDigits++
		}
		digit = byt % 10
		if digit != 0 || i < n-1 {
			digits[numDigits] = digit
			numDigits++
		}
	}
	if !isPositive {
		out = append(out, '-')
	}
	if decimalPointIndex <= 0 {
		out = append(out, '0', '.')
		res.IsInteger = false
		for i := decimalPointIndex; i < 0; i++ {
			out = append(out, '0')
		}
	}
	for i := 0; i < numDigits; i++ {
		if i > 0 && i == decimalPointIndex {
			out = append(out, '.')
			res.IsInteger = false
		}
		out = append(out, '0'+digits[i])
	}
	if decimalPointIndex > numDigits {
		for i := numDigits; i < decimalPointIndex; i++ {
			out = append(out, '0')
		}
	}
	res.Text = out
	return res, nil
}

// EncodeNumber encodes decimal text (optional sign, digits, optional
// fraction, optional exponent) into Oracle NUMBER wire bytes appended to
// dst.
func EncodeNumber(dst []byte, value []byte) ([]byte, error) {
	if len(value) == 0 {
		return dst, errors.New("oratype: cannot encode empty number string")
	}
	if len(value) > numberMaxChars {
		return dst, errors.New("oratype: number string too long")
	}
	var digits [numberMaxChars + 2]uint8
	numDigits := 0
	pos := 0
	isNegative := false
	if value[0] == '-' {
		isNegative = true
		pos++
	} else if value[0] == '+' {
		pos++
	}
	for pos < len(value) {
		c := value[pos]
		if c == '.' || c == 'e' || c == 'E' {
			break
		}
		if c < '0' || c > '9' {
			return dst, fmt.Errorf("oratype: invalid number %q", value)
		}
		pos++
		d := c - '0'
		if d == 0 && numDigits == 0 {
			continue
		}
		digits[numDigits] = d
		numDigits++
	}
	decimalPointIndex := numDigits
	if pos < len(value) && value[pos] == '.' {
		pos++
		for pos < len(value) {
			c := value[pos]
			if c == 'e' || c == 'E' {
				break
			}
			if c < '0' || c > '9' {
				return dst, fmt.Errorf("oratype: invalid number %q", value)
			}
			pos++
			d := c - '0'
			if d == 0 && numDigits == 0 {
				decimalPointIndex--
				continue
			}
			digits[numDigits] = d
			numDigits++
		}
	}
	if pos < len(value) && (value[pos] == 'e' || value[pos] == 'E') {
		pos++
		expNeg := false
		if pos < len(value) {
			if value[pos] == '-' {
				expNeg = true
				pos++
			} else if value[pos] == '+' {
				pos++
			}
		}
		expStart := pos
		for pos < len(value) {
			if value[pos] < '0' || value[pos] > '9' {
				return dst, fmt.Errorf("oratype: invalid exponent in %q", value)
			}
			pos++
		}
		if expStart == pos {
			return dst, fmt.Errorf("oratype: empty exponent in %q", value)
		}
		e, _ := strconv.Atoi(string(value[expStart:pos]))
		if expNeg {
			e = -e
		}
		decimalPointIndex += e
	}
	if pos < len(value) {
		return dst, fmt.Errorf("oratype: invalid content after number %q", value)
	}
	for numDigits > 0 && digits[numDigits-1] == 0 {
		numDigits--
	}
	if numDigits > numberMaxDigits || decimalPointIndex > 126 || decimalPointIndex < -129 {
		return dst, fmt.Errorf("oratype: value %q has no Oracle NUMBER representation", value)
	}
	prependZero := false
	if decimalPointIndex%2 == 1 || decimalPointIndex%2 == -1 {
		prependZero = true
		if numDigits > 0 {
			digits[numDigits] = 0
			numDigits++
			decimalPointIndex++
		}
	}
	if numDigits%2 == 1 {
		digits[numDigits] = 0
		numDigits++
	}
	numPairs := numDigits / 2
	if numDigits == 0 {
		return append(dst, 128), nil
	}
	exponentOnWire := int8(decimalPointIndex/2) + int8(-64) // == +192 as int8
	if isNegative {
		exponentOnWire = ^exponentOnWire
	}
	dst = append(dst, uint8(exponentOnWire))
	digitsPos := 0
	for pair := 0; pair < numPairs; pair++ {
		var d uint8
		if pair == 0 && prependZero {
			d = digits[digitsPos]
			digitsPos++
		} else {
			d = digits[digitsPos]*10 + digits[digitsPos+1]
			digitsPos += 2
		}
		if isNegative {
			d = 101 - d
		} else {
			d++
		}
		dst = append(dst, d)
	}
	if isNegative && numDigits < numberMaxDigits {
		dst = append(dst, 102)
	}
	return dst, nil
}

// EncodeInt64 encodes an integer as an Oracle NUMBER.
func EncodeInt64(dst []byte, v int64) ([]byte, error) {
	var buf [24]byte
	return EncodeNumber(dst, strconv.AppendInt(buf[:0], v, 10))
}

// EncodeFloat64 encodes a float as an Oracle NUMBER (via shortest decimal
// text; NaN/Inf are rejected).
func EncodeFloat64(dst []byte, v float64) ([]byte, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return dst, errors.New("oratype: NaN/Inf cannot be stored in NUMBER")
	}
	var buf [32]byte
	return EncodeNumber(dst, strconv.AppendFloat(buf[:0], v, 'g', -1, 64))
}
