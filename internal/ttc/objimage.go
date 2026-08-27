package ttc

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// Object image ("pickle") constants.
const (
	objIsVersion81  = 0x80
	objIsDegenerate = 0x10
	objIsCollection = 0x08
	objNoPrefixSeg  = 0x04
	objAtomicNull   = 253
	objMaxShortLen  = 245

	xmlTypeLOB       = 0x0001
	xmlTypeString    = 0x0004
	xmlTypeSkipNext4 = 0x100000
)

// ValueKind classifies a decoded object value.
type ValueKind int

// Value kinds.
const (
	KindNull ValueKind = iota
	KindScalar
	KindObject
	KindCollection
	KindXMLString
	KindLOB
)

// Value is a node of a decoded object image.
type Value struct {
	Kind     ValueKind
	Meta     *AttrMeta   // scalar type info (nil for the root)
	Type     *ObjectType // for objects / collections
	Raw      []byte      // scalar wire bytes, XML text, or LOB locator
	Fields   []Value     // object attributes (parallel to Type.Attrs)
	Elements []Value     // collection elements
	Indexes  []int32     // associative-array indexes (nil otherwise)
}

func oratypeDecodeNumber(b []byte) (string, error) {
	d, err := oratype.DecodeNumber(b, nil)
	if err != nil {
		return "", err
	}
	return string(d.Text), nil
}

// imageReader walks a packed object image.
type imageReader struct {
	data []byte
	pos  int
}

func (r *imageReader) need(n int) error {
	if n < 0 || r.pos+n > len(r.data) {
		return fmt.Errorf("oracle: truncated object image (need %d bytes at %d of %d)", n, r.pos, len(r.data))
	}
	return nil
}

func (r *imageReader) u8() (uint8, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.data[r.pos]
	r.pos++
	return v, nil
}

func (r *imageReader) u32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *imageReader) raw(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	v := r.data[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

// length reads a short length or a 254-prefixed 32-bit length.
func (r *imageReader) length() (int, error) {
	b, err := r.u8()
	if err != nil {
		return 0, err
	}
	if b == tns.LongLengthIndicator {
		v, err := r.u32()
		return int(v), err
	}
	return int(b), nil
}

// bytesWithLength reads a length-prefixed value; nil for NULL (255) or 0.
func (r *imageReader) bytesWithLength() ([]byte, error) {
	b, err := r.u8()
	if err != nil {
		return nil, err
	}
	if b == 0 || b == tns.NullLengthIndicator {
		return nil, nil
	}
	n := int(b)
	if b == tns.LongLengthIndicator {
		v, err := r.u32()
		if err != nil {
			return nil, err
		}
		n = int(v)
	}
	return r.raw(n)
}

func (r *imageReader) header() error {
	flags, err := r.u8()
	if err != nil {
		return err
	}
	if _, err := r.u8(); err != nil { // version
		return err
	}
	if _, err := r.length(); err != nil { // total length
		return err
	}
	if flags&objIsDegenerate != 0 {
		return errors.New("oracle: objects stored in a LOB (degenerate image) are not supported")
	}
	if flags&objNoPrefixSeg != 0 {
		return nil
	}
	n, err := r.length()
	if err != nil {
		return err
	}
	_, err = r.raw(n)
	return err
}

// DecodeObjectImage decodes a top-level object/collection image.
func DecodeObjectImage(t *ObjectType, image []byte) (Value, error) {
	r := &imageReader{data: image}
	if err := r.header(); err != nil {
		return Value{}, err
	}
	return r.decodeBody(t)
}

func (r *imageReader) decodeBody(t *ObjectType) (Value, error) {
	if t.IsCollection {
		v := Value{Kind: KindCollection, Type: t}
		if _, err := r.u8(); err != nil { // collection flags
			return v, err
		}
		n, err := r.length()
		if err != nil {
			return v, err
		}
		for i := 0; i < n; i++ {
			if t.CollKind == CollIndexTable {
				idx, err := r.u32()
				if err != nil {
					return v, err
				}
				v.Indexes = append(v.Indexes, int32(idx))
			}
			e, err := r.decodeValue(t.Element, true)
			if err != nil {
				return v, err
			}
			v.Elements = append(v.Elements, e)
		}
		return v, nil
	}
	v := Value{Kind: KindObject, Type: t, Fields: make([]Value, len(t.Attrs))}
	for i, a := range t.Attrs {
		f, err := r.decodeValue(a, false)
		if err != nil {
			return v, fmt.Errorf("attribute %s: %w", a.Name, err)
		}
		v.Fields[i] = f
	}
	return v, nil
}

func (r *imageReader) decodeValue(m *AttrMeta, parentIsCollection bool) (Value, error) {
	switch m.OraTypeNum {
	case TypeClob, TypeBlob, TypeBfile:
		loc, err := r.bytesWithLength()
		if err != nil || loc == nil {
			return Value{Kind: KindNull, Meta: m}, err
		}
		return Value{Kind: KindLOB, Meta: m, Raw: append([]byte(nil), loc...)}, nil
	case TypeObject:
		if m.ObjType == nil || m.ObjType.IsXMLType {
			xml, err := r.bytesWithLength()
			if err != nil || xml == nil {
				return Value{Kind: KindNull, Meta: m}, err
			}
			return decodeXMLImage(m, xml)
		}
		isColl := m.ObjType.IsCollection || parentIsCollection
		b, err := r.u8()
		if err != nil {
			return Value{}, err
		}
		if b == objAtomicNull || (isColl && b == tns.NullLengthIndicator) {
			return Value{Kind: KindNull, Meta: m}, nil
		}
		r.pos--
		if isColl {
			packed, err := r.bytesWithLength()
			if err != nil {
				return Value{}, err
			}
			if packed == nil {
				return Value{Kind: KindNull, Meta: m}, nil
			}
			v, err := DecodeObjectImage(m.ObjType, packed)
			v.Meta = m
			return v, err
		}
		v, err := r.decodeBody(m.ObjType)
		v.Meta = m
		return v, err
	}
	raw, err := r.bytesWithLength()
	if err != nil {
		return Value{}, err
	}
	if raw == nil {
		return Value{Kind: KindNull, Meta: m}, nil
	}
	return Value{Kind: KindScalar, Meta: m, Raw: raw}, nil
}

// decodeXMLImage handles the XMLType image layout.
func decodeXMLImage(m *AttrMeta, image []byte) (Value, error) {
	r := &imageReader{data: image}
	if err := r.header(); err != nil {
		return Value{}, err
	}
	if _, err := r.u8(); err != nil { // XML version
		return Value{}, err
	}
	flag, err := r.u32()
	if err != nil {
		return Value{}, err
	}
	if flag&xmlTypeSkipNext4 != 0 {
		if _, err := r.raw(4); err != nil {
			return Value{}, err
		}
	}
	rest := r.data[r.pos:]
	switch {
	case flag&xmlTypeString != 0:
		return Value{Kind: KindXMLString, Meta: m, Raw: rest}, nil
	case flag&xmlTypeLOB != 0:
		return Value{Kind: KindLOB, Meta: &AttrMeta{Name: m.Name, OraTypeNum: TypeClob, CSForm: CSFormImplicit}, Raw: append([]byte(nil), rest...)}, nil
	}
	return Value{}, fmt.Errorf("oracle: unexpected XMLType flag %#x", flag)
}

// DecodeXMLTypeImage decodes a top-level XMLType column image.
func DecodeXMLTypeImage(image []byte) (Value, error) {
	return decodeXMLImage(&AttrMeta{OraTypeNum: TypeObject}, image)
}
