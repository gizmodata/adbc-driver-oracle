package oratype

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"
)

// OSON (Oracle binary JSON) constants.
const (
	osonMagic1 = 0xff
	osonMagic2 = 0x4a // 'J'
	osonMagic3 = 0x5a // 'Z'

	osonVersionMaxFname255   = 1
	osonVersionMaxFname65535 = 3

	osonFlagHashIDUint8        = 0x0100
	osonFlagNumFnamesUint16    = 0x0400
	osonFlagFnamesSegUint32    = 0x0800
	osonFlagTinyNodesStat      = 0x2000
	osonFlagTreeSegUint32      = 0x1000
	osonFlagRelOffsetMode      = 0x01
	osonFlagInlineLeaf         = 0x02
	osonFlagLenInPcode         = 0x04
	osonFlagNumFnamesUint32    = 0x08
	osonFlagIsScalar           = 0x10
	osonFlagSecFnamesSegUint16 = 0x0100

	osonTypeNull            = 0x30
	osonTypeTrue            = 0x31
	osonTypeFalse           = 0x32
	osonTypeStringLenUint8  = 0x33
	osonTypeNumberLenUint8  = 0x34
	osonTypeBinaryDouble    = 0x36
	osonTypeStringLenUint16 = 0x37
	osonTypeStringLenUint32 = 0x38
	osonTypeTimestamp       = 0x39
	osonTypeBinaryLenUint16 = 0x3a
	osonTypeBinaryLenUint32 = 0x3b
	osonTypeDate            = 0x3c
	osonTypeIntervalYM      = 0x3d
	osonTypeIntervalDS      = 0x3e
	osonTypeTimestampTZ     = 0x7c
	osonTypeTimestamp7      = 0x7d
	osonTypeID              = 0x7e
	osonTypeBinaryFloat     = 0x7f
	osonTypeObject          = 0x84
	osonTypeArray           = 0xc0
	osonTypeExtended        = 0x7b
	osonTypeVector          = 0x01
)

// ErrOSON is returned for malformed OSON data.
var ErrOSON = errors.New("oratype: malformed OSON data")

type osonDecoder struct {
	data            []byte
	pos             int
	fieldNames      []string
	fieldIDLength   int
	treeSegPos      int
	relativeOffsets bool
	out             []byte
	scratch         []byte
}

func (d *osonDecoder) need(n int) error {
	if d.pos+n > len(d.data) || n < 0 {
		return fmt.Errorf("%w: need %d bytes at %d of %d", ErrOSON, n, d.pos, len(d.data))
	}
	return nil
}

func (d *osonDecoder) u8() (uint8, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	v := d.data[d.pos]
	d.pos++
	return v, nil
}

func (d *osonDecoder) u16() (uint16, error) {
	if err := d.need(2); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint16(d.data[d.pos:])
	d.pos += 2
	return v, nil
}

func (d *osonDecoder) u32() (uint32, error) {
	if err := d.need(4); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(d.data[d.pos:])
	d.pos += 4
	return v, nil
}

func (d *osonDecoder) raw(n int) ([]byte, error) {
	if err := d.need(n); err != nil {
		return nil, err
	}
	v := d.data[d.pos : d.pos+n]
	d.pos += n
	return v, nil
}

// DecodeOSONToJSON converts OSON bytes into JSON text appended to dst.
// Dates and timestamps are rendered as ISO-8601 strings, intervals as
// ISO-8601 durations, binary values as hex strings.
func DecodeOSONToJSON(data []byte, dst []byte) ([]byte, error) {
	d := &osonDecoder{data: data, out: dst[:0]}
	if err := d.decode(); err != nil {
		return dst, err
	}
	return d.out, nil
}

func (d *osonDecoder) decode() error {
	hdr, err := d.raw(3)
	if err != nil {
		return err
	}
	if hdr[0] != osonMagic1 || hdr[1] != osonMagic2 || hdr[2] != osonMagic3 {
		return fmt.Errorf("%w: bad magic %x", ErrOSON, hdr)
	}
	version, err := d.u8()
	if err != nil {
		return err
	}
	if version != osonVersionMaxFname255 && version != osonVersionMaxFname65535 {
		return fmt.Errorf("oratype: OSON version %d is not supported", version)
	}
	primaryFlags, err := d.u16()
	if err != nil {
		return err
	}
	d.relativeOffsets = primaryFlags&osonFlagRelOffsetMode != 0
	if primaryFlags&osonFlagIsScalar != 0 {
		if primaryFlags&osonFlagTreeSegUint32 != 0 {
			d.pos += 4
		} else {
			d.pos += 2
		}
		return d.decodeNode()
	}
	var numShort uint32
	switch {
	case primaryFlags&osonFlagNumFnamesUint32 != 0:
		numShort, err = d.u32()
		d.fieldIDLength = 4
	case primaryFlags&osonFlagNumFnamesUint16 != 0:
		var v uint16
		v, err = d.u16()
		numShort = uint32(v)
		d.fieldIDLength = 2
	default:
		var v uint8
		v, err = d.u8()
		numShort = uint32(v)
		d.fieldIDLength = 1
	}
	if err != nil {
		return err
	}
	var shortSegSize uint32
	shortOffsetsSize := 2
	if primaryFlags&osonFlagFnamesSegUint32 != 0 {
		shortOffsetsSize = 4
		shortSegSize, err = d.u32()
	} else {
		var v uint16
		v, err = d.u16()
		shortSegSize = uint32(v)
	}
	if err != nil {
		return err
	}
	var numLong, longSegSize uint32
	longOffsetsSize := 0
	if version == osonVersionMaxFname65535 {
		secondaryFlags, err := d.u16()
		if err != nil {
			return err
		}
		if secondaryFlags&osonFlagSecFnamesSegUint16 != 0 {
			longOffsetsSize = 2
		} else {
			longOffsetsSize = 4
		}
		if numLong, err = d.u32(); err != nil {
			return err
		}
		if longSegSize, err = d.u32(); err != nil {
			return err
		}
	}
	if primaryFlags&osonFlagTreeSegUint32 != 0 {
		_, err = d.u32()
	} else {
		_, err = d.u16()
	}
	if err != nil {
		return err
	}
	if _, err = d.u16(); err != nil { // num tiny nodes
		return err
	}
	d.fieldNames = d.fieldNames[:0]
	if numShort > 0 {
		if err := d.readFieldNames(int(numShort), shortOffsetsSize, int(shortSegSize), false); err != nil {
			return err
		}
	}
	if numLong > 0 {
		if err := d.readFieldNames(int(numLong), longOffsetsSize, int(longSegSize), true); err != nil {
			return err
		}
	}
	d.treeSegPos = d.pos
	return d.decodeNode()
}

func (d *osonDecoder) readFieldNames(num, offsetsSize, segSize int, long bool) error {
	hashSize := 1
	if long {
		hashSize = 2
	}
	if err := d.need(num*hashSize + num*offsetsSize + segSize); err != nil {
		return err
	}
	d.pos += num * hashSize
	offsetsPos := d.pos
	d.pos += num * offsetsSize
	seg := d.data[d.pos : d.pos+segSize]
	d.pos += segSize
	for i := 0; i < num; i++ {
		var off int
		if offsetsSize == 2 {
			off = int(binary.BigEndian.Uint16(d.data[offsetsPos:]))
			offsetsPos += 2
		} else {
			off = int(binary.BigEndian.Uint32(d.data[offsetsPos:]))
			offsetsPos += 4
		}
		if long {
			if off+2 > len(seg) {
				return ErrOSON
			}
			n := int(binary.BigEndian.Uint16(seg[off:]))
			if off+2+n > len(seg) {
				return ErrOSON
			}
			d.fieldNames = append(d.fieldNames, string(seg[off+2:off+2+n]))
		} else {
			if off+1 > len(seg) {
				return ErrOSON
			}
			n := int(seg[off])
			if off+1+n > len(seg) {
				return ErrOSON
			}
			d.fieldNames = append(d.fieldNames, string(seg[off+1:off+1+n]))
		}
	}
	return nil
}

func (d *osonDecoder) numChildren(nodeType uint8) (n uint32, shared bool, err error) {
	bits := nodeType & 0x18
	shared = bits == 0x18
	switch bits {
	case 0:
		v, e := d.u8()
		return uint32(v), shared, e
	case 0x08:
		v, e := d.u16()
		return uint32(v), shared, e
	case 0x10:
		v, e := d.u32()
		return v, shared, e
	}
	return 0, shared, nil
}

func (d *osonDecoder) offset(nodeType uint8) (uint32, error) {
	if nodeType&0x20 != 0 {
		return d.u32()
	}
	v, err := d.u16()
	return uint32(v), err
}

func (d *osonDecoder) decodeContainer(nodeType uint8) error {
	isObject := nodeType&0x40 == 0
	containerOffset := d.pos - d.treeSegPos - 1
	numChildren, shared, err := d.numChildren(nodeType)
	if err != nil {
		return err
	}
	var fieldIDsPos, offsetsPos int
	if shared {
		off, err := d.offset(nodeType)
		if err != nil {
			return err
		}
		offsetsPos = d.pos
		d.pos = d.treeSegPos + int(off)
		t, err := d.u8()
		if err != nil {
			return err
		}
		numChildren, _, err = d.numChildren(t)
		if err != nil {
			return err
		}
		fieldIDsPos = d.pos
	} else if isObject {
		fieldIDsPos = d.pos
		offsetsPos = d.pos + d.fieldIDLength*int(numChildren)
	} else {
		offsetsPos = d.pos
	}
	if isObject {
		d.out = append(d.out, '{')
	} else {
		d.out = append(d.out, '[')
	}
	for i := uint32(0); i < numChildren; i++ {
		if i > 0 {
			d.out = append(d.out, ',')
		}
		if isObject {
			d.pos = fieldIDsPos
			var id uint32
			switch d.fieldIDLength {
			case 1:
				v, err := d.u8()
				if err != nil {
					return err
				}
				id = uint32(v)
			case 2:
				v, err := d.u16()
				if err != nil {
					return err
				}
				id = uint32(v)
			default:
				v, err := d.u32()
				if err != nil {
					return err
				}
				id = v
			}
			fieldIDsPos = d.pos
			if id == 0 || int(id) > len(d.fieldNames) {
				return fmt.Errorf("%w: field id %d out of range", ErrOSON, id)
			}
			d.appendString(d.fieldNames[id-1])
			d.out = append(d.out, ':')
		}
		d.pos = offsetsPos
		off, err := d.offset(nodeType)
		if err != nil {
			return err
		}
		if d.relativeOffsets {
			off += uint32(containerOffset)
		}
		offsetsPos = d.pos
		d.pos = d.treeSegPos + int(off)
		if err := d.decodeNode(); err != nil {
			return err
		}
	}
	if isObject {
		d.out = append(d.out, '}')
	} else {
		d.out = append(d.out, ']')
	}
	return nil
}

func (d *osonDecoder) appendNumber(b []byte) error {
	n, err := DecodeNumber(b, d.scratch[:0])
	if err != nil {
		return err
	}
	d.scratch = n.Text
	if n.IsMaxNegativeValue {
		d.out = append(d.out, "-1e126"...)
		return nil
	}
	d.out = append(d.out, n.Text...)
	return nil
}

func (d *osonDecoder) appendDate(b []byte) error {
	dt, err := DecodeDate(b)
	if err != nil {
		return err
	}
	t := time.Unix(0, dt.UnixNanos()).UTC()
	d.out = append(d.out, '"')
	if len(b) == 7 {
		d.out = t.AppendFormat(d.out, "2006-01-02T15:04:05")
	} else {
		d.out = t.AppendFormat(d.out, "2006-01-02T15:04:05.000000")
		if len(b) >= 13 {
			d.out = append(d.out, 'Z')
		}
	}
	d.out = append(d.out, '"')
	return nil
}

func (d *osonDecoder) decodeNode() error {
	nodeType, err := d.u8()
	if err != nil {
		return err
	}
	if nodeType&0x80 != 0 {
		return d.decodeContainer(nodeType)
	}
	switch nodeType {
	case osonTypeNull:
		d.out = append(d.out, "null"...)
		return nil
	case osonTypeTrue:
		d.out = append(d.out, "true"...)
		return nil
	case osonTypeFalse:
		d.out = append(d.out, "false"...)
		return nil
	case osonTypeDate, osonTypeTimestamp7:
		b, err := d.raw(7)
		if err != nil {
			return err
		}
		return d.appendDate(b)
	case osonTypeTimestamp:
		b, err := d.raw(11)
		if err != nil {
			return err
		}
		return d.appendDate(b)
	case osonTypeTimestampTZ:
		b, err := d.raw(13)
		if err != nil {
			return err
		}
		return d.appendDate(b)
	case osonTypeBinaryFloat:
		b, err := d.raw(4)
		if err != nil {
			return err
		}
		f, err := DecodeBinaryFloat(b)
		if err != nil {
			return err
		}
		d.appendFloat(float64(f), 32)
		return nil
	case osonTypeBinaryDouble:
		b, err := d.raw(8)
		if err != nil {
			return err
		}
		f, err := DecodeBinaryDouble(b)
		if err != nil {
			return err
		}
		d.appendFloat(f, 64)
		return nil
	case osonTypeIntervalDS:
		b, err := d.raw(11)
		if err != nil {
			return err
		}
		v, err := DecodeIntervalDS(b)
		if err != nil {
			return err
		}
		d.out = append(d.out, '"')
		d.out = append(d.out, fmt.Sprintf("P%dDT%dH%dM%d.%09dS", v.Days, v.Hours, v.Minutes, v.Seconds, v.Nanos)...)
		d.out = append(d.out, '"')
		return nil
	case osonTypeIntervalYM:
		b, err := d.raw(5)
		if err != nil {
			return err
		}
		v, err := DecodeIntervalYM(b)
		if err != nil {
			return err
		}
		d.out = append(d.out, '"')
		d.out = append(d.out, fmt.Sprintf("P%dY%dM", v.Years, v.Months)...)
		d.out = append(d.out, '"')
		return nil
	case osonTypeStringLenUint8:
		n, err := d.u8()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		d.appendStringBytes(b)
		return nil
	case osonTypeStringLenUint16:
		n, err := d.u16()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		d.appendStringBytes(b)
		return nil
	case osonTypeStringLenUint32:
		n, err := d.u32()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		d.appendStringBytes(b)
		return nil
	case osonTypeNumberLenUint8:
		n, err := d.u8()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		return d.appendNumber(b)
	case osonTypeID:
		n, err := d.u8()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		d.appendHex(b)
		return nil
	case osonTypeBinaryLenUint16:
		n, err := d.u16()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		d.appendHex(b)
		return nil
	case osonTypeBinaryLenUint32:
		n, err := d.u32()
		if err != nil {
			return err
		}
		b, err := d.raw(int(n))
		if err != nil {
			return err
		}
		d.appendHex(b)
		return nil
	case osonTypeExtended:
		ext, err := d.u8()
		if err != nil {
			return err
		}
		if ext == osonTypeVector {
			return errors.New("oratype: VECTOR values inside JSON are not supported")
		}
		return fmt.Errorf("oratype: OSON extended node type %#x is not supported", ext)
	}
	switch nodeType & 0xf0 {
	case 0x20, 0x60:
		n := int(nodeType&0x0f) + 1
		b, err := d.raw(n)
		if err != nil {
			return err
		}
		return d.appendNumber(b)
	case 0x40, 0x50:
		n := int(nodeType & 0x0f)
		b, err := d.raw(n)
		if err != nil {
			return err
		}
		return d.appendNumber(b)
	}
	if nodeType&0xe0 == 0 {
		if nodeType == 0 {
			d.out = append(d.out, '"', '"')
			return nil
		}
		b, err := d.raw(int(nodeType))
		if err != nil {
			return err
		}
		d.appendStringBytes(b)
		return nil
	}
	return fmt.Errorf("oratype: OSON node type %#x is not supported", nodeType)
}

func (d *osonDecoder) appendFloat(f float64, bits int) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		d.out = append(d.out, "null"...)
		return
	}
	d.out = strconv.AppendFloat(d.out, f, 'g', -1, bits)
}

func (d *osonDecoder) appendHex(b []byte) {
	d.out = append(d.out, '"')
	d.out = append(d.out, []byte(hex.EncodeToString(b))...)
	d.out = append(d.out, '"')
}

func (d *osonDecoder) appendString(s string) { d.appendStringBytes([]byte(s)) }

const hexDigits = "0123456789abcdef"

// appendStringBytes writes a JSON string literal with escaping.
func (d *osonDecoder) appendStringBytes(s []byte) {
	d.out = append(d.out, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			d.out = append(d.out, s[start:i]...)
			switch c {
			case '"', '\\':
				d.out = append(d.out, '\\', c)
			case '\n':
				d.out = append(d.out, '\\', 'n')
			case '\r':
				d.out = append(d.out, '\\', 'r')
			case '\t':
				d.out = append(d.out, '\\', 't')
			default:
				d.out = append(d.out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		_, size := utf8.DecodeRune(s[i:])
		i += size
	}
	d.out = append(d.out, s[start:]...)
	d.out = append(d.out, '"')
}
