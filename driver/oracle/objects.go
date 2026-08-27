package oracle

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// Object-typed columns (user-defined types, collections, XMLType,
// SDO_GEOMETRY) arrive as opaque images whose layout depends on type
// metadata that can only be looked up between round trips. The sink
// therefore buffers the raw images per column and decodes them when a
// record batch is materialised (after the statement's execute/fetch
// round trip has completed).

// objectKind classifies how an object column is rendered.
type objectKind int

const (
	objectJSON     objectKind = iota // user-defined type / collection -> JSON text
	objectXML                        // SYS.XMLTYPE -> XML text
	objectGeometry                   // MDSYS.SDO_GEOMETRY -> WKB
)

func objectKindFor(c *ttc.Column) objectKind {
	switch {
	case c.ObjectTypeSchema == "SYS" && c.ObjectTypeName == "XMLTYPE":
		return objectXML
	case c.ObjectTypeSchema == "MDSYS" && c.ObjectTypeName == "SDO_GEOMETRY":
		return objectGeometry
	}
	return objectJSON
}

// objectArrowType returns the storage type and (optionally) extension
// metadata for an object column.
func objectArrowType(c *ttc.Column, useExt bool) (arrow.DataType, []string, []string) {
	switch objectKindFor(c) {
	case objectXML:
		if useExt {
			return arrow.BinaryTypes.String, []string{"ARROW:extension:name", "ARROW:extension:metadata"},
				[]string{"arrow.opaque", `{"type_name":"XMLTYPE","vendor_name":"Oracle Database"}`}
		}
		return arrow.BinaryTypes.String, nil, nil
	case objectGeometry:
		if useExt {
			return arrow.BinaryTypes.Binary, []string{"ARROW:extension:name", "ARROW:extension:metadata"}, []string{"geoarrow.wkb", "{}"}
		}
		return arrow.BinaryTypes.Binary, nil, nil
	}
	if useExt {
		return arrow.BinaryTypes.String, []string{"ARROW:extension:name", "ARROW:extension:metadata"}, []string{"arrow.json", ""}
	}
	return arrow.BinaryTypes.String, nil, nil
}

// objEntry is one buffered value of an object column.
type objEntry struct {
	data []byte
	null bool
}

// objectColumnState buffers images for one object column.
type objectColumnState struct {
	col     *ttc.Column
	kind    objectKind
	entries []objEntry
	typ     *ttc.ObjectType
}

// flushObjects decodes buffered object images into their builders.
func (s *arrowSink) flushObjects(ctx context.Context, conn *ttc.Conn) error {
	for i, st := range s.objects {
		if st == nil {
			continue
		}
		if len(st.entries) == 0 {
			continue
		}
		if st.typ == nil && st.kind != objectXML {
			t, err := conn.ObjectType(ctx, st.col.ObjectTypeSchema, st.col.ObjectTypeName)
			if err != nil {
				return fromTTCError(err)
			}
			st.typ = t
		}
		b := s.bldr.Field(i)
		for _, e := range st.entries {
			if e.null {
				b.AppendNull()
				continue
			}
			if err := s.appendObject(ctx, conn, st, b, e.data); err != nil {
				return err
			}
		}
		st.entries = st.entries[:0]
	}
	return nil
}

func (s *arrowSink) appendObject(ctx context.Context, conn *ttc.Conn, st *objectColumnState, b array.Builder, image []byte) error {
	switch st.kind {
	case objectXML:
		v, err := ttc.DecodeXMLTypeImage(image)
		if err != nil {
			return errStatus(adbc.StatusInvalidData, "oracle: column %q: %v", st.col.Name, err)
		}
		text, err := xmlText(ctx, conn, v)
		if err != nil {
			return err
		}
		b.(*array.StringBuilder).Append(text)
		return nil
	case objectGeometry:
		v, err := ttc.DecodeObjectImage(st.typ, image)
		if err != nil {
			return errStatus(adbc.StatusInvalidData, "oracle: column %q: %v", st.col.Name, err)
		}
		wkb, err := sdoGeometryToWKB(v)
		if err != nil {
			return errStatus(adbc.StatusInvalidData, "oracle: column %q: %v", st.col.Name, err)
		}
		b.(*array.BinaryBuilder).Append(wkb)
		return nil
	}
	v, err := ttc.DecodeObjectImage(st.typ, image)
	if err != nil {
		return errStatus(adbc.StatusInvalidData, "oracle: column %q: %v", st.col.Name, err)
	}
	w := &jsonWriter{ctx: ctx, conn: conn}
	if err := w.value(v); err != nil {
		return errStatus(adbc.StatusInvalidData, "oracle: column %q: %v", st.col.Name, err)
	}
	b.(*array.StringBuilder).BinaryBuilder.Append(w.out)
	return nil
}

func xmlText(ctx context.Context, conn *ttc.Conn, v ttc.Value) (string, error) {
	switch v.Kind {
	case ttc.KindXMLString:
		return string(v.Raw), nil
	case ttc.KindLOB:
		data, err := conn.ReadLOB(ctx, v.Raw, true, false)
		if err != nil {
			return "", fromTTCError(err)
		}
		return string(data), nil
	}
	return "", nil
}

// ---- JSON rendering of decoded objects ----

type jsonWriter struct {
	ctx  context.Context
	conn *ttc.Conn
	out  []byte
}

func (w *jsonWriter) value(v ttc.Value) error {
	switch v.Kind {
	case ttc.KindNull:
		w.out = append(w.out, "null"...)
	case ttc.KindObject:
		w.out = append(w.out, '{')
		for i, f := range v.Fields {
			if i > 0 {
				w.out = append(w.out, ',')
			}
			w.str(v.Type.Attrs[i].Name)
			w.out = append(w.out, ':')
			if err := w.value(f); err != nil {
				return err
			}
		}
		w.out = append(w.out, '}')
	case ttc.KindCollection:
		if v.Indexes != nil {
			w.out = append(w.out, '{')
			for i, e := range v.Elements {
				if i > 0 {
					w.out = append(w.out, ',')
				}
				w.str(strconv.Itoa(int(v.Indexes[i])))
				w.out = append(w.out, ':')
				if err := w.value(e); err != nil {
					return err
				}
			}
			w.out = append(w.out, '}')
			return nil
		}
		w.out = append(w.out, '[')
		for i, e := range v.Elements {
			if i > 0 {
				w.out = append(w.out, ',')
			}
			if err := w.value(e); err != nil {
				return err
			}
		}
		w.out = append(w.out, ']')
	case ttc.KindXMLString:
		w.str(string(v.Raw))
	case ttc.KindLOB:
		isChar := v.Meta.OraTypeNum == ttc.TypeClob
		data, err := w.conn.ReadLOB(w.ctx, v.Raw, isChar, v.Meta.CSForm == ttc.CSFormNChar)
		if err != nil {
			return err
		}
		if isChar {
			w.str(string(data))
		} else {
			w.str(hex.EncodeToString(data))
		}
	case ttc.KindScalar:
		return w.scalar(v.Meta, v.Raw)
	}
	return nil
}

func (w *jsonWriter) scalar(m *ttc.AttrMeta, raw []byte) error {
	switch m.OraTypeNum {
	case ttc.TypeNumber:
		d, err := oratype.DecodeNumber(raw, nil)
		if err != nil {
			return err
		}
		if d.IsMaxNegativeValue {
			w.out = append(w.out, "-1e126"...)
			return nil
		}
		w.out = append(w.out, d.Text...)
	case ttc.TypeBinaryInteger:
		var n int64
		for _, c := range raw {
			n = n<<8 | int64(c)
		}
		w.out = strconv.AppendInt(w.out, int64(int32(n)), 10)
	case ttc.TypeBinaryFloat:
		f, err := oratype.DecodeBinaryFloat(raw)
		if err != nil {
			return err
		}
		w.float(float64(f), 32)
	case ttc.TypeBinaryDouble:
		f, err := oratype.DecodeBinaryDouble(raw)
		if err != nil {
			return err
		}
		w.float(f, 64)
	case ttc.TypeBoolean:
		// Encoded as a 32-bit integer inside object images.
		truth := false
		for _, c := range raw {
			if c != 0 {
				truth = true
			}
		}
		if truth {
			w.out = append(w.out, "true"...)
		} else {
			w.out = append(w.out, "false"...)
		}
	case ttc.TypeVarchar, ttc.TypeChar, ttc.TypeLong:
		if m.CSForm == ttc.CSFormNChar {
			w.str(tns.DecodeUTF16BE(raw))
		} else {
			w.strBytes(raw)
		}
	case ttc.TypeRaw, ttc.TypeLongRaw:
		w.str(hex.EncodeToString(raw))
	case ttc.TypeDate, ttc.TypeTimestamp, ttc.TypeTimestampTZ, ttc.TypeTimestampLTZ:
		d, err := oratype.DecodeDate(raw)
		if err != nil {
			return err
		}
		t := time.Unix(0, d.UnixNanos()).UTC()
		w.out = append(w.out, '"')
		if len(raw) == 7 {
			w.out = t.AppendFormat(w.out, "2006-01-02T15:04:05")
		} else {
			w.out = t.AppendFormat(w.out, "2006-01-02T15:04:05.000000000")
			if len(raw) >= 13 {
				w.out = append(w.out, 'Z')
			}
		}
		w.out = append(w.out, '"')
	case ttc.TypeIntervalDS:
		v, err := oratype.DecodeIntervalDS(raw)
		if err != nil {
			return err
		}
		w.str(isoIntervalDS(v))
	case ttc.TypeIntervalYM:
		v, err := oratype.DecodeIntervalYM(raw)
		if err != nil {
			return err
		}
		w.str(fmt.Sprintf("P%dY%dM", v.Years, v.Months))
	case ttc.TypeJSON:
		text, err := oratype.DecodeOSONToJSON(raw, nil)
		if err != nil {
			return err
		}
		w.out = append(w.out, text...)
	default:
		return fmt.Errorf("attribute %q: type %s is not supported inside objects", m.Name, m.TypeName())
	}
	return nil
}

func (w *jsonWriter) float(f float64, bits int) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		w.out = append(w.out, "null"...)
		return
	}
	w.out = strconv.AppendFloat(w.out, f, 'g', -1, bits)
}

func (w *jsonWriter) str(s string) { w.strBytes([]byte(s)) }

func (w *jsonWriter) strBytes(s []byte) {
	w.out = append(w.out, '"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			w.out = append(w.out, '\\', c)
		case c == '\n':
			w.out = append(w.out, '\\', 'n')
		case c == '\r':
			w.out = append(w.out, '\\', 'r')
		case c == '\t':
			w.out = append(w.out, '\\', 't')
		case c < 0x20:
			w.out = append(w.out, '\\', 'u', '0', '0', "0123456789abcdef"[c>>4], "0123456789abcdef"[c&0xf])
		default:
			w.out = append(w.out, c)
		}
	}
	w.out = append(w.out, '"')
}

// ---- SDO_GEOMETRY -> WKB ----

func numberOf(v ttc.Value) (float64, bool, error) {
	if v.Kind != ttc.KindScalar {
		return 0, false, nil
	}
	d, err := oratype.DecodeNumber(v.Raw, nil)
	if err != nil {
		return 0, false, err
	}
	f, err := strconv.ParseFloat(string(d.Text), 64)
	return f, true, err
}

func attrByName(v ttc.Value, name string) (ttc.Value, bool) {
	if v.Kind != ttc.KindObject {
		return ttc.Value{}, false
	}
	for i, a := range v.Type.Attrs {
		if strings.EqualFold(a.Name, name) {
			return v.Fields[i], true
		}
	}
	return ttc.Value{}, false
}

func numberArray(v ttc.Value) ([]float64, error) {
	if v.Kind != ttc.KindCollection {
		return nil, nil
	}
	out := make([]float64, 0, len(v.Elements))
	for _, e := range v.Elements {
		f, ok, err := numberOf(e)
		if err != nil {
			return nil, err
		}
		if !ok {
			f = math.NaN()
		}
		out = append(out, f)
	}
	return out, nil
}

// WKB geometry type codes.
const (
	wkbPoint           = 1
	wkbLineString      = 2
	wkbPolygon         = 3
	wkbMultiPoint      = 4
	wkbMultiLineString = 5
	wkbMultiPolygon    = 6
	wkbCollection      = 7
)

type wkbWriter struct {
	out  []byte
	dims int
}

func (w *wkbWriter) typeCode(base uint32) uint32 {
	switch w.dims {
	case 3:
		return base + 1000
	case 4:
		return base + 3000
	}
	return base
}

func (w *wkbWriter) header(base uint32) {
	w.out = append(w.out, 1) // little endian
	w.out = binary.LittleEndian.AppendUint32(w.out, w.typeCode(base))
}

func (w *wkbWriter) u32(v uint32) { w.out = binary.LittleEndian.AppendUint32(w.out, v) }

func (w *wkbWriter) coords(ords []float64) {
	for _, f := range ords {
		w.out = binary.LittleEndian.AppendUint64(w.out, math.Float64bits(f))
	}
}

// sdoGeometryToWKB converts a decoded MDSYS.SDO_GEOMETRY value to WKB.
// Supported: points (SDO_POINT or element type 1), line strings, polygons
// (incl. rectangles), multi-points/-lines/-polygons and collections, in
// 2D/3D/4D with straight-line interpretation. Circular arcs and circles
// are rejected.
func sdoGeometryToWKB(g ttc.Value) ([]byte, error) {
	gtypeV, _ := attrByName(g, "SDO_GTYPE")
	gtypeF, ok, err := numberOf(gtypeV)
	if err != nil || !ok {
		return nil, fmt.Errorf("SDO_GEOMETRY without SDO_GTYPE")
	}
	gtype := int(gtypeF)
	dims := gtype / 1000
	if dims < 2 || dims > 4 {
		return nil, fmt.Errorf("SDO_GTYPE %d: unsupported dimensionality", gtype)
	}
	lrs := (gtype / 100) % 10
	_ = lrs
	shape := gtype % 100
	w := &wkbWriter{dims: dims}

	pointV, _ := attrByName(g, "SDO_POINT")
	elemV, _ := attrByName(g, "SDO_ELEM_INFO")
	ordV, _ := attrByName(g, "SDO_ORDINATES")
	elem, err := numberArray(elemV)
	if err != nil {
		return nil, err
	}
	ords, err := numberArray(ordV)
	if err != nil {
		return nil, err
	}

	// Optimised point representation.
	if pointV.Kind == ttc.KindObject && shape == 1 && len(elem) == 0 {
		xv, _ := attrByName(pointV, "X")
		yv, _ := attrByName(pointV, "Y")
		zv, _ := attrByName(pointV, "Z")
		x, _, _ := numberOf(xv)
		y, _, _ := numberOf(yv)
		z, hasZ, _ := numberOf(zv)
		if dims == 2 {
			w.header(wkbPoint)
			w.coords([]float64{x, y})
		} else {
			if !hasZ {
				z = math.NaN()
			}
			w.dims = 3
			w.header(wkbPoint)
			w.coords([]float64{x, y, z})
		}
		return w.out, nil
	}
	if len(elem)%3 != 0 {
		return nil, fmt.Errorf("SDO_ELEM_INFO length %d is not a multiple of 3", len(elem))
	}
	type element struct {
		offset, etype, interp int
		next                  int // ordinate offset of the next element (1-based, exclusive)
	}
	elems := make([]element, 0, len(elem)/3)
	for i := 0; i+2 < len(elem); i += 3 {
		elems = append(elems, element{offset: int(elem[i]), etype: int(elem[i+1]), interp: int(elem[i+2])})
	}
	for i := range elems {
		if i+1 < len(elems) {
			elems[i].next = elems[i+1].offset
		} else {
			elems[i].next = len(ords) + 1
		}
	}
	ordsOf := func(e element) []float64 {
		start := e.offset - 1
		end := e.next - 1
		if start < 0 || end > len(ords) || start > end {
			return nil
		}
		return ords[start:end]
	}
	// ring expands a polygon ring element to coordinates (rectangles are
	// expanded to 5 points).
	ring := func(e element) ([]float64, error) {
		o := ordsOf(e)
		switch e.interp {
		case 1:
			return o, nil
		case 3:
			if len(o) < 2*dims {
				return nil, fmt.Errorf("rectangle element with %d ordinates", len(o))
			}
			x1, y1, x2, y2 := o[0], o[1], o[dims], o[dims+1]
			extra := o[2:dims]
			var r []float64
			add := func(x, y float64) {
				r = append(r, x, y)
				r = append(r, extra...)
			}
			if e.etype == 1003 { // exterior: counter-clockwise
				add(x1, y1)
				add(x2, y1)
				add(x2, y2)
				add(x1, y2)
				add(x1, y1)
			} else { // interior: clockwise
				add(x1, y1)
				add(x1, y2)
				add(x2, y2)
				add(x2, y1)
				add(x1, y1)
			}
			return r, nil
		}
		return nil, fmt.Errorf("polygon element interpretation %d (arcs/circles) is not supported", e.interp)
	}
	writeRing := func(o []float64) {
		w.u32(uint32(len(o) / dims))
		w.coords(o)
	}
	writeLine := func(e element) error {
		if e.interp != 1 {
			return fmt.Errorf("line element interpretation %d (arcs) is not supported", e.interp)
		}
		w.header(wkbLineString)
		writeRing(ordsOf(e))
		return nil
	}
	// Group polygon elements: an exterior ring followed by interior rings.
	writePolygon := func(i int) (int, error) {
		e := elems[i]
		if e.etype != 1003 && e.etype != 1005 {
			return i, fmt.Errorf("expected exterior ring, got element type %d", e.etype)
		}
		if e.etype == 1005 {
			return i, fmt.Errorf("compound polygon elements (etype 1005) are not supported")
		}
		rings := [][]float64{}
		r, err := ring(e)
		if err != nil {
			return i, err
		}
		rings = append(rings, r)
		i++
		for i < len(elems) && elems[i].etype == 2003 {
			r, err := ring(elems[i])
			if err != nil {
				return i, err
			}
			rings = append(rings, r)
			i++
		}
		w.header(wkbPolygon)
		w.u32(uint32(len(rings)))
		for _, r := range rings {
			writeRing(r)
		}
		return i, nil
	}
	writePoint := func(o []float64) {
		w.header(wkbPoint)
		w.coords(o[:dims])
	}

	switch shape {
	case 1: // point
		if len(elems) == 0 {
			return nil, fmt.Errorf("point geometry without coordinates")
		}
		o := ordsOf(elems[0])
		if len(o) < dims {
			return nil, fmt.Errorf("point element with %d ordinates", len(o))
		}
		writePoint(o)
	case 2: // line string
		if len(elems) == 0 {
			return nil, fmt.Errorf("line geometry without elements")
		}
		if err := writeLine(elems[0]); err != nil {
			return nil, err
		}
	case 3: // polygon
		if len(elems) == 0 {
			return nil, fmt.Errorf("polygon geometry without elements")
		}
		if _, err := writePolygon(0); err != nil {
			return nil, err
		}
	case 5: // multipoint
		var pts [][]float64
		for _, e := range elems {
			o := ordsOf(e)
			if e.etype != 1 {
				return nil, fmt.Errorf("multipoint element type %d", e.etype)
			}
			for j := 0; j+dims <= len(o); j += dims {
				pts = append(pts, o[j:j+dims])
			}
		}
		w.header(wkbMultiPoint)
		w.u32(uint32(len(pts)))
		for _, p := range pts {
			writePoint(p)
		}
	case 6: // multiline
		w.header(wkbMultiLineString)
		w.u32(uint32(len(elems)))
		for _, e := range elems {
			if err := writeLine(e); err != nil {
				return nil, err
			}
		}
	case 7: // multipolygon
		var polys int
		for i := range elems {
			if elems[i].etype == 1003 {
				polys++
			}
		}
		w.header(wkbMultiPolygon)
		w.u32(uint32(polys))
		for i := 0; i < len(elems); {
			var err error
			i, err = writePolygon(i)
			if err != nil {
				return nil, err
			}
		}
	case 4: // heterogeneous collection
		var count int
		for i := range elems {
			if elems[i].etype != 2003 {
				count++
			}
		}
		w.header(wkbCollection)
		w.u32(uint32(count))
		for i := 0; i < len(elems); {
			e := elems[i]
			switch e.etype {
			case 1:
				o := ordsOf(e)
				if e.interp > 1 {
					w.header(wkbMultiPoint)
					w.u32(uint32(len(o) / dims))
					for j := 0; j+dims <= len(o); j += dims {
						writePoint(o[j : j+dims])
					}
				} else {
					writePoint(o)
				}
				i++
			case 2:
				if err := writeLine(e); err != nil {
					return nil, err
				}
				i++
			case 1003:
				var err error
				i, err = writePolygon(i)
				if err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("collection element type %d is not supported", e.etype)
			}
		}
	default:
		return nil, fmt.Errorf("SDO_GTYPE %d is not supported", gtype)
	}
	return w.out, nil
}
