package oracle

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/decimal256"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// numberArrowType decides the Arrow type for a NUMBER column.
func numberArrowType(c *ttc.Column, mode string) arrow.DataType {
	p, s := int32(c.Precision), int32(c.Scale)
	switch mode {
	case NumberModeString:
		return arrow.BinaryTypes.String
	case NumberModeDouble:
		return arrow.PrimitiveTypes.Float64
	case NumberModeDecimal:
		if p > 0 && p <= 38 && s >= 0 && s <= p {
			return &arrow.Decimal128Type{Precision: p, Scale: s}
		}
		if p == 0 && s == 0 {
			return &arrow.Decimal128Type{Precision: 38, Scale: 0}
		}
		return &arrow.Decimal128Type{Precision: 38, Scale: 10}
	}
	// auto
	if p > 0 && s == 0 && p <= 18 {
		return arrow.PrimitiveTypes.Int64
	}
	if p > 0 && p <= 38 && s >= 0 && s <= p {
		return &arrow.Decimal128Type{Precision: p, Scale: s}
	}
	if p == 0 && s == 0 && c.OraTypeNum == ttc.TypeNumber && c.BufferSize == 22 && false {
		return arrow.PrimitiveTypes.Int64
	}
	return arrow.PrimitiveTypes.Float64
}

func timestampUnit(scale int8) arrow.TimeUnit {
	switch {
	case scale <= 0:
		return arrow.Second
	case scale <= 3:
		return arrow.Millisecond
	case scale <= 6:
		return arrow.Microsecond
	default:
		return arrow.Nanosecond
	}
}

// arrowTypeFor maps a described column to its Arrow type.
func arrowTypeFor(c *ttc.Column, opts typeOptions) (arrow.DataType, error) {
	switch c.OraTypeNum {
	case ttc.TypeNumber:
		return numberArrowType(c, opts.numberMode), nil
	case ttc.TypeBinaryInteger:
		return arrow.PrimitiveTypes.Int64, nil
	case ttc.TypeBinaryFloat:
		return arrow.PrimitiveTypes.Float32, nil
	case ttc.TypeBinaryDouble:
		return arrow.PrimitiveTypes.Float64, nil
	case ttc.TypeVarchar, ttc.TypeChar, ttc.TypeLong, ttc.TypeClob, ttc.TypeRowid, ttc.TypeURowid, ttc.TypeJSON:
		return arrow.BinaryTypes.String, nil
	case ttc.TypeRaw, ttc.TypeLongRaw, ttc.TypeBlob:
		return arrow.BinaryTypes.Binary, nil
	case ttc.TypeDate:
		if opts.dateMode == DateModeDate32 {
			return arrow.FixedWidthTypes.Date32, nil
		}
		return &arrow.TimestampType{Unit: arrow.Second}, nil
	case ttc.TypeTimestamp:
		return &arrow.TimestampType{Unit: timestampUnit(c.Scale)}, nil
	case ttc.TypeTimestampTZ, ttc.TypeTimestampLTZ:
		return &arrow.TimestampType{Unit: timestampUnit(c.Scale), TimeZone: "UTC"}, nil
	case ttc.TypeIntervalDS:
		switch opts.intervalMode {
		case IntervalModeDuration:
			return &arrow.DurationType{Unit: timestampUnit(c.Scale)}, nil
		case IntervalModeString:
			return arrow.BinaryTypes.String, nil
		}
		return arrow.FixedWidthTypes.MonthDayNanoInterval, nil
	case ttc.TypeIntervalYM:
		if opts.intervalMode == IntervalModeString {
			return arrow.BinaryTypes.String, nil
		}
		return arrow.FixedWidthTypes.MonthDayNanoInterval, nil
	case ttc.TypeBoolean:
		return arrow.FixedWidthTypes.Boolean, nil
	case ttc.TypeVector:
		return nil, errStatus(adbc.StatusNotImplemented, "oracle: column %q: VECTOR columns are not supported yet; select TO_CLOB(FROM_VECTOR(col)) or VECTOR_SERIALIZE(col) instead", c.Name)
	}
	return nil, errStatus(adbc.StatusNotImplemented, "oracle: column %q: data type %s is not supported", c.Name, c.TypeName())
}

// schemaFor builds the Arrow schema for a described result set.
func schemaFor(cols []ttc.Column, opts typeOptions) *arrow.Schema {
	fields := make([]arrow.Field, len(cols))
	for i := range cols {
		dt, err := arrowTypeFor(&cols[i], opts)
		if err != nil {
			dt = arrow.BinaryTypes.String
		}
		md := arrow.NewMetadata([]string{"ORACLE:type"}, []string{cols[i].TypeName()})
		fields[i] = arrow.Field{Name: cols[i].Name, Type: dt, Nullable: cols[i].NullsAllowed, Metadata: md}
	}
	return arrow.NewSchema(fields, nil)
}

// arrowSink implements ttc.RowSink by appending wire values directly to
// Arrow builders.
type arrowSink struct {
	alloc     memory.Allocator
	stmt      *ttc.Statement
	opts      typeOptions
	schema    *arrow.Schema
	bldr      *array.RecordBuilder
	appenders []func(data []byte) error
	lastRaw   [][]byte
	lastNull  []bool
	rows      int
	bytes     int64 // approximate bytes appended to the current batch
	err       error
	scratch   []byte
}

func newArrowSink(alloc memory.Allocator, stmt *ttc.Statement, opts typeOptions) *arrowSink {
	return &arrowSink{alloc: alloc, stmt: stmt, opts: opts}
}

// ensure builds the schema and builders once describe info is available.
func (s *arrowSink) ensure() error {
	if s.bldr != nil {
		return nil
	}
	cols := s.stmt.Columns()
	fields := make([]arrow.Field, len(cols))
	for i := range cols {
		dt, err := arrowTypeFor(&cols[i], s.opts)
		if err != nil {
			return err
		}
		md := arrow.NewMetadata([]string{"ORACLE:type"}, []string{cols[i].TypeName()})
		fields[i] = arrow.Field{Name: cols[i].Name, Type: dt, Nullable: cols[i].NullsAllowed, Metadata: md}
	}
	s.schema = arrow.NewSchema(fields, nil)
	s.bldr = array.NewRecordBuilder(s.alloc, s.schema)
	s.appenders = make([]func([]byte) error, len(cols))
	s.lastRaw = make([][]byte, len(cols))
	s.lastNull = make([]bool, len(cols))
	for i := range cols {
		s.appenders[i] = s.makeAppender(&cols[i], s.bldr.Field(i))
	}
	return nil
}

func (s *arrowSink) release() {
	if s.bldr != nil {
		s.bldr.Release()
		s.bldr = nil
	}
}

func (s *arrowSink) newRecord() arrow.Record {
	rec := s.bldr.NewRecord()
	s.rows = 0
	s.bytes = 0
	return rec
}

func (s *arrowSink) AppendValue(col int, data []byte) error {
	if s.err != nil {
		return s.err
	}
	if err := s.ensure(); err != nil {
		s.err = err
		return err
	}
	if err := s.appenders[col](data); err != nil {
		s.err = err
		return err
	}
	s.bytes += int64(len(data)) + 8
	s.lastRaw[col] = append(s.lastRaw[col][:0], data...)
	s.lastNull[col] = false
	return nil
}

func (s *arrowSink) AppendNull(col int) error {
	if s.err != nil {
		return s.err
	}
	if err := s.ensure(); err != nil {
		s.err = err
		return err
	}
	s.bldr.Field(col).AppendNull()
	s.lastNull[col] = true
	return nil
}

func (s *arrowSink) AppendDuplicate(col int) error {
	if s.err != nil {
		return s.err
	}
	if err := s.ensure(); err != nil {
		s.err = err
		return err
	}
	if s.lastNull[col] {
		s.bldr.Field(col).AppendNull()
		return nil
	}
	if err := s.appenders[col](s.lastRaw[col]); err != nil {
		s.err = err
		return err
	}
	s.bytes += int64(len(s.lastRaw[col])) + 8
	return nil
}

func (s *arrowSink) FinishRow() error {
	s.rows++
	return s.err
}

func (s *arrowSink) makeAppender(c *ttc.Column, b array.Builder) func([]byte) error {
	name := c.Name
	switch c.FetchType {
	case ttc.TypeNumber, ttc.TypeBinaryInteger:
		switch bb := b.(type) {
		case *array.Int64Builder:
			return func(data []byte) error {
				d, err := oratype.DecodeNumber(data, s.scratch[:0])
				if err != nil {
					return err
				}
				s.scratch = d.Text
				v, err := strconv.ParseInt(string(d.Text), 10, 64)
				if err != nil {
					f, ferr := strconv.ParseFloat(string(d.Text), 64)
					if ferr != nil || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
						return errStatus(adbc.StatusInvalidData, "oracle: column %q: value %s does not fit in int64 (set %s=decimal)", name, d.Text, OptionNumberMode)
					}
					v = int64(f)
				}
				bb.Append(v)
				return nil
			}
		case *array.Float64Builder:
			return func(data []byte) error {
				d, err := oratype.DecodeNumber(data, s.scratch[:0])
				if err != nil {
					return err
				}
				s.scratch = d.Text
				if d.IsMaxNegativeValue {
					bb.Append(-1e126)
					return nil
				}
				f, err := strconv.ParseFloat(string(d.Text), 64)
				if err != nil {
					return errStatus(adbc.StatusInvalidData, "oracle: column %q: cannot parse number %s: %v", name, d.Text, err)
				}
				bb.Append(f)
				return nil
			}
		case *array.Decimal128Builder:
			dt := bb.Type().(*arrow.Decimal128Type)
			return func(data []byte) error {
				d, err := oratype.DecodeNumber(data, s.scratch[:0])
				if err != nil {
					return err
				}
				s.scratch = d.Text
				v, err := decimal128.FromString(string(d.Text), dt.Precision, dt.Scale)
				if err != nil {
					return errStatus(adbc.StatusInvalidData, "oracle: column %q: value %s does not fit decimal(%d,%d): %v", name, d.Text, dt.Precision, dt.Scale, err)
				}
				bb.Append(v)
				return nil
			}
		case *array.StringBuilder:
			return func(data []byte) error {
				d, err := oratype.DecodeNumber(data, s.scratch[:0])
				if err != nil {
					return err
				}
				s.scratch = d.Text
				if d.IsMaxNegativeValue {
					bb.Append("-1E+126")
					return nil
				}
				bb.Append(string(d.Text))
				return nil
			}
		}
	case ttc.TypeBinaryFloat:
		bb := b.(*array.Float32Builder)
		return func(data []byte) error {
			f, err := oratype.DecodeBinaryFloat(data)
			if err != nil {
				return err
			}
			bb.Append(f)
			return nil
		}
	case ttc.TypeBinaryDouble:
		bb := b.(*array.Float64Builder)
		return func(data []byte) error {
			f, err := oratype.DecodeBinaryDouble(data)
			if err != nil {
				return err
			}
			bb.Append(f)
			return nil
		}
	case ttc.TypeVarchar, ttc.TypeChar, ttc.TypeLong, ttc.TypeRowid, ttc.TypeURowid:
		bb := b.(*array.StringBuilder)
		return func(data []byte) error {
			bb.BinaryBuilder.Append(data)
			return nil
		}
	case ttc.TypeJSON:
		bb := b.(*array.StringBuilder)
		return func(data []byte) error {
			text, err := oratype.DecodeOSONToJSON(data, s.scratch[:0])
			if err != nil {
				return errStatus(adbc.StatusInvalidData, "oracle: column %q: %v", name, err)
			}
			s.scratch = text
			bb.BinaryBuilder.Append(text)
			return nil
		}
	case ttc.TypeRaw, ttc.TypeLongRaw:
		bb := b.(*array.BinaryBuilder)
		return func(data []byte) error {
			bb.Append(data)
			return nil
		}
	case ttc.TypeDate, ttc.TypeTimestamp, ttc.TypeTimestampTZ, ttc.TypeTimestampLTZ:
		if db, ok := b.(*array.Date32Builder); ok {
			return func(data []byte) error {
				d, err := oratype.DecodeDate(data)
				if err != nil {
					return err
				}
				db.Append(arrow.Date32(d.Unix() / 86400))
				return nil
			}
		}
		bb := b.(*array.TimestampBuilder)
		unit := bb.Type().(*arrow.TimestampType).Unit
		return func(data []byte) error {
			d, err := oratype.DecodeDate(data)
			if err != nil {
				return err
			}
			var v int64
			switch unit {
			case arrow.Second:
				v = d.Unix()
			case arrow.Millisecond:
				v = d.UnixNanos() / int64(time.Millisecond)
			case arrow.Microsecond:
				v = d.UnixNanos() / int64(time.Microsecond)
			default:
				v = d.UnixNanos()
			}
			bb.Append(arrow.Timestamp(v))
			return nil
		}
	case ttc.TypeIntervalDS:
		switch bb := b.(type) {
		case *array.DurationBuilder:
			unit := bb.Type().(*arrow.DurationType).Unit
			return func(data []byte) error {
				v, err := oratype.DecodeIntervalDS(data)
				if err != nil {
					return err
				}
				nanos := int64(v.Days)*24*int64(time.Hour) + int64(v.Hours)*int64(time.Hour) + int64(v.Minutes)*int64(time.Minute) + int64(v.Seconds)*int64(time.Second) + int64(v.Nanos)
				bb.Append(arrow.Duration(nanos / int64(unit.Multiplier())))
				return nil
			}
		case *array.StringBuilder:
			return func(data []byte) error {
				v, err := oratype.DecodeIntervalDS(data)
				if err != nil {
					return err
				}
				bb.Append(isoIntervalDS(v))
				return nil
			}
		default:
			mb := b.(*array.MonthDayNanoIntervalBuilder)
			return func(data []byte) error {
				v, err := oratype.DecodeIntervalDS(data)
				if err != nil {
					return err
				}
				nanos := int64(v.Hours)*int64(time.Hour) + int64(v.Minutes)*int64(time.Minute) + int64(v.Seconds)*int64(time.Second) + int64(v.Nanos)
				mb.Append(arrow.MonthDayNanoInterval{Months: 0, Days: v.Days, Nanoseconds: nanos})
				return nil
			}
		}
	case ttc.TypeIntervalYM:
		if sb, ok := b.(*array.StringBuilder); ok {
			return func(data []byte) error {
				v, err := oratype.DecodeIntervalYM(data)
				if err != nil {
					return err
				}
				sb.Append(fmt.Sprintf("P%dY%dM", v.Years, v.Months))
				return nil
			}
		}
		bb := b.(*array.MonthDayNanoIntervalBuilder)
		return func(data []byte) error {
			v, err := oratype.DecodeIntervalYM(data)
			if err != nil {
				return err
			}
			bb.Append(arrow.MonthDayNanoInterval{Months: v.Years*12 + v.Months})
			return nil
		}
	case ttc.TypeBoolean:
		bb := b.(*array.BooleanBuilder)
		return func(data []byte) error {
			bb.Append(len(data) > 0 && data[0] == 1)
			return nil
		}
	}
	return func([]byte) error {
		return errStatus(adbc.StatusNotImplemented, "oracle: column %q: data type %s is not supported", name, c.TypeName())
	}
}

// ---- binds: Arrow -> TTC ----

// bindsFromRecord converts each column of a record into a bind column.
func bindsFromRecord(rec arrow.Record, conn *ttc.Conn) ([]ttc.BindColumn, error) {
	binds := make([]ttc.BindColumn, rec.NumCols())
	maxString := conn.MaxStringSize()
	for i := 0; i < int(rec.NumCols()); i++ {
		col := rec.Column(i)
		b, err := bindForArray(col, rec.Schema().Field(i), maxString, conn.SupportsBoolean())
		if err != nil {
			return nil, err
		}
		binds[i] = b
	}
	return binds, nil
}

func bindForArray(col arrow.Array, field arrow.Field, maxString uint32, supportsBool bool) (ttc.BindColumn, error) {
	var b ttc.BindColumn
	nullOr := func(i int, f func(i int) ([]byte, error)) ([]byte, error) {
		if col.IsNull(i) {
			return nil, nil
		}
		return f(i)
	}
	var buf []byte
	switch a := col.(type) {
	case *array.Null:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeVarchar, CSForm: ttc.CSFormImplicit, BufferSize: 1, Value: func(int) ([]byte, error) { return nil, nil }}
	case *array.Boolean:
		if supportsBool {
			b = ttc.BindColumn{OraTypeNum: ttc.TypeBoolean, BufferSize: 4, Value: func(i int) ([]byte, error) {
				return nullOr(i, func(i int) ([]byte, error) {
					if a.Value(i) {
						return []byte{1, 1}, nil
					}
					return []byte{0}, nil
				})
			}}
		} else {
			b = ttc.BindColumn{OraTypeNum: ttc.TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) {
				return nullOr(i, func(i int) ([]byte, error) {
					if a.Value(i) {
						return oratype.EncodeInt64(buf[:0], 1)
					}
					return oratype.EncodeInt64(buf[:0], 0)
				})
			}}
		}
	case *array.Int8:
		b = intBind(col, func(i int) int64 { return int64(a.Value(i)) })
	case *array.Int16:
		b = intBind(col, func(i int) int64 { return int64(a.Value(i)) })
	case *array.Int32:
		b = intBind(col, func(i int) int64 { return int64(a.Value(i)) })
	case *array.Int64:
		b = intBind(col, func(i int) int64 { return a.Value(i) })
	case *array.Uint8:
		b = intBind(col, func(i int) int64 { return int64(a.Value(i)) })
	case *array.Uint16:
		b = intBind(col, func(i int) int64 { return int64(a.Value(i)) })
	case *array.Uint32:
		b = intBind(col, func(i int) int64 { return int64(a.Value(i)) })
	case *array.Uint64:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) {
				return oratype.EncodeNumber(buf[:0], strconv.AppendUint(nil, a.Value(i), 10))
			})
		}}
	case *array.Float16:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeBinaryFloat, BufferSize: 4, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) { return oratype.EncodeBinaryFloat(buf[:0], a.Value(i).Float32()), nil })
		}}
	case *array.Float32:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeBinaryFloat, BufferSize: 4, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) { return oratype.EncodeBinaryFloat(buf[:0], a.Value(i)), nil })
		}}
	case *array.Float64:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeBinaryDouble, BufferSize: 8, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) { return oratype.EncodeBinaryDouble(buf[:0], a.Value(i)), nil })
		}}
	case *array.Decimal128:
		scale := a.DataType().(*arrow.Decimal128Type).Scale
		b = ttc.BindColumn{OraTypeNum: ttc.TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) {
				return oratype.EncodeNumber(buf[:0], []byte(a.Value(i).ToString(scale)))
			})
		}}
	case *array.Decimal256:
		scale := a.DataType().(*arrow.Decimal256Type).Scale
		b = ttc.BindColumn{OraTypeNum: ttc.TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) {
				return oratype.EncodeNumber(buf[:0], []byte(decimal256.Num(a.Value(i)).ToString(scale)))
			})
		}}
	case *array.String:
		b = stringBind(col, maxString, func(i int) []byte { return []byte(a.Value(i)) }, a.ValueLen)
	case *array.LargeString:
		b = stringBind(col, maxString, func(i int) []byte { return []byte(a.Value(i)) }, func(i int) int { return len(a.Value(i)) })
	case *array.StringView:
		b = stringBind(col, maxString, func(i int) []byte { return []byte(a.Value(i)) }, func(i int) int { return len(a.Value(i)) })
	case *array.Binary:
		b = binaryBind(col, maxString, a.Value, a.ValueLen)
	case *array.LargeBinary:
		b = binaryBind(col, maxString, a.Value, func(i int) int { return len(a.Value(i)) })
	case *array.BinaryView:
		b = binaryBind(col, maxString, a.Value, func(i int) int { return len(a.Value(i)) })
	case *array.FixedSizeBinary:
		w := a.DataType().(*arrow.FixedSizeBinaryType).ByteWidth
		b = binaryBind(col, maxString, a.Value, func(int) int { return w })
	case *array.Date32:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeDate, BufferSize: 7, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) { return oratype.EncodeDate(buf[:0], a.Value(i).ToTime()), nil })
		}}
	case *array.Date64:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeDate, BufferSize: 7, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) { return oratype.EncodeDate(buf[:0], a.Value(i).ToTime()), nil })
		}}
	case *array.Timestamp:
		dt := a.DataType().(*arrow.TimestampType)
		if dt.TimeZone != "" {
			loc, err := dt.GetZone()
			if err != nil {
				loc = time.UTC
			}
			b = ttc.BindColumn{OraTypeNum: ttc.TypeTimestampTZ, BufferSize: 13, Value: func(i int) ([]byte, error) {
				return nullOr(i, func(i int) ([]byte, error) {
					return oratype.EncodeTimestampTZ(buf[:0], a.Value(i).ToTime(dt.Unit).In(loc)), nil
				})
			}}
		} else {
			b = ttc.BindColumn{OraTypeNum: ttc.TypeTimestamp, BufferSize: 11, Value: func(i int) ([]byte, error) {
				return nullOr(i, func(i int) ([]byte, error) {
					return oratype.EncodeTimestamp(buf[:0], a.Value(i).ToTime(dt.Unit)), nil
				})
			}}
		}
	case *array.MonthDayNanoInterval:
		b = ttc.BindColumn{OraTypeNum: ttc.TypeIntervalDS, BufferSize: 11, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) {
				v := a.Value(i)
				if v.Months != 0 {
					return nil, errStatus(adbc.StatusNotImplemented, "oracle: intervals with a month component cannot be bound as INTERVAL DAY TO SECOND")
				}
				ns := v.Nanoseconds
				h := ns / int64(time.Hour)
				ns -= h * int64(time.Hour)
				m := ns / int64(time.Minute)
				ns -= m * int64(time.Minute)
				sec := ns / int64(time.Second)
				ns -= sec * int64(time.Second)
				return oratype.EncodeIntervalDS(buf[:0], oratype.IntervalDS{Days: v.Days, Hours: int32(h), Minutes: int32(m), Seconds: int32(sec), Nanos: int32(ns)}), nil
			})
		}}
	case *array.Duration:
		unit := a.DataType().(*arrow.DurationType).Unit
		b = ttc.BindColumn{OraTypeNum: ttc.TypeIntervalDS, BufferSize: 11, Value: func(i int) ([]byte, error) {
			return nullOr(i, func(i int) ([]byte, error) {
				d := time.Duration(a.Value(i)) * unit.Multiplier()
				days := int64(d / (24 * time.Hour))
				d -= time.Duration(days) * 24 * time.Hour
				h := int64(d / time.Hour)
				d -= time.Duration(h) * time.Hour
				m := int64(d / time.Minute)
				d -= time.Duration(m) * time.Minute
				sec := int64(d / time.Second)
				d -= time.Duration(sec) * time.Second
				return oratype.EncodeIntervalDS(buf[:0], oratype.IntervalDS{Days: int32(days), Hours: int32(h), Minutes: int32(m), Seconds: int32(sec), Nanos: int32(d)}), nil
			})
		}}
	default:
		return b, errStatus(adbc.StatusNotImplemented, "oracle: cannot bind Arrow type %s (column %q)", col.DataType(), field.Name)
	}
	return b, nil
}

func intBind(col arrow.Array, get func(i int) int64) ttc.BindColumn {
	var buf []byte
	return ttc.BindColumn{OraTypeNum: ttc.TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) {
		if col.IsNull(i) {
			return nil, nil
		}
		var err error
		buf, err = oratype.EncodeInt64(buf[:0], get(i))
		return buf, err
	}}
}

func stringBind(col arrow.Array, maxString uint32, get func(i int) []byte, length func(i int) int) ttc.BindColumn {
	maxLen := 0
	for i := 0; i < col.Len(); i++ {
		if !col.IsNull(i) {
			if l := length(i); l > maxLen {
				maxLen = l
			}
		}
	}
	b := ttc.BindColumn{OraTypeNum: ttc.TypeVarchar, CSForm: ttc.CSFormImplicit, BufferSize: uint32(maxLen)}
	if maxLen == 0 {
		b.BufferSize = 1
	}
	if uint32(maxLen) > maxString {
		b.OraTypeNum = ttc.TypeLong
		b.BufferSize = 0x7fffffff
	}
	b.Value = func(i int) ([]byte, error) {
		if col.IsNull(i) {
			return nil, nil
		}
		v := get(i)
		if len(v) == 0 {
			// Oracle treats '' as NULL; an empty string bind is sent as NULL.
			return nil, nil
		}
		return v, nil
	}
	return b
}

func binaryBind(col arrow.Array, maxString uint32, get func(i int) []byte, length func(i int) int) ttc.BindColumn {
	maxLen := 0
	for i := 0; i < col.Len(); i++ {
		if !col.IsNull(i) {
			if l := length(i); l > maxLen {
				maxLen = l
			}
		}
	}
	b := ttc.BindColumn{OraTypeNum: ttc.TypeRaw, BufferSize: uint32(maxLen)}
	if maxLen == 0 {
		b.BufferSize = 1
	}
	if uint32(maxLen) > maxString {
		b.OraTypeNum = ttc.TypeLongRaw
		b.BufferSize = 0x7fffffff
	}
	b.Value = func(i int) ([]byte, error) {
		if col.IsNull(i) {
			return nil, nil
		}
		v := get(i)
		if len(v) == 0 {
			return nil, nil
		}
		return v, nil
	}
	return b
}

// oracleTypeForArrow renders the Oracle column type used by bulk-ingest
// CREATE TABLE for an Arrow field.
func oracleTypeForArrow(dt arrow.DataType, s *statementImpl, supportsBool bool) (string, error) {
	varcharLen := s.ingestVarcharLength
	if varcharLen == 0 {
		varcharLen = 4000
	}
	rawLen := s.ingestRawLength
	if rawLen == 0 {
		rawLen = 2000
	}
	stringType := s.ingestStringType
	if stringType == "" {
		stringType = "VARCHAR2"
	}
	binaryType := s.ingestBinaryType
	if binaryType == "" {
		binaryType = "RAW"
	}
	switch t := dt.(type) {
	case *arrow.NullType:
		return "VARCHAR2(1)", nil
	case *arrow.BooleanType:
		if supportsBool {
			return "BOOLEAN", nil
		}
		return "NUMBER(1)", nil
	case *arrow.Int8Type, *arrow.Uint8Type:
		return "NUMBER(3)", nil
	case *arrow.Int16Type, *arrow.Uint16Type:
		return "NUMBER(5)", nil
	case *arrow.Int32Type, *arrow.Uint32Type:
		return "NUMBER(10)", nil
	case *arrow.Int64Type:
		return "NUMBER(19)", nil
	case *arrow.Uint64Type:
		return "NUMBER(20)", nil
	case *arrow.Float16Type, *arrow.Float32Type:
		return "BINARY_FLOAT", nil
	case *arrow.Float64Type:
		return "BINARY_DOUBLE", nil
	case *arrow.Decimal128Type:
		if t.Precision > 38 {
			return "NUMBER", nil
		}
		return fmt.Sprintf("NUMBER(%d,%d)", t.Precision, t.Scale), nil
	case *arrow.Decimal256Type:
		if t.Precision > 38 {
			return "NUMBER", nil
		}
		return fmt.Sprintf("NUMBER(%d,%d)", t.Precision, t.Scale), nil
	case *arrow.StringType, *arrow.StringViewType:
		if stringType == "CLOB" || stringType == "NCLOB" {
			return stringType, nil
		}
		return fmt.Sprintf("%s(%d)", stringType, varcharLen), nil
	case *arrow.LargeStringType:
		if stringType == "CLOB" || stringType == "NCLOB" {
			return stringType, nil
		}
		return "CLOB", nil
	case *arrow.BinaryType, *arrow.BinaryViewType, *arrow.FixedSizeBinaryType:
		if binaryType == "BLOB" {
			return "BLOB", nil
		}
		if fb, ok := t.(*arrow.FixedSizeBinaryType); ok {
			return fmt.Sprintf("RAW(%d)", fb.ByteWidth), nil
		}
		return fmt.Sprintf("RAW(%d)", rawLen), nil
	case *arrow.LargeBinaryType:
		return "BLOB", nil
	case *arrow.Date32Type, *arrow.Date64Type:
		return "DATE", nil
	case *arrow.TimestampType:
		scale := 0
		switch t.Unit {
		case arrow.Millisecond:
			scale = 3
		case arrow.Microsecond:
			scale = 6
		case arrow.Nanosecond:
			scale = 9
		}
		if t.TimeZone != "" {
			return fmt.Sprintf("TIMESTAMP(%d) WITH TIME ZONE", scale), nil
		}
		return fmt.Sprintf("TIMESTAMP(%d)", scale), nil
	case *arrow.MonthDayNanoIntervalType, *arrow.DurationType:
		return "INTERVAL DAY(9) TO SECOND(9)", nil
	}
	return "", fmt.Errorf("unsupported Arrow type %s", dt)
}

// identFor renders an identifier for DDL: simple names are upper-cased
// (Oracle's default case), anything else is quoted verbatim.
func identFor(name string) string {
	simple := len(name) > 0 && len(name) <= 128
	for i := 0; i < len(name) && simple; i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9', c == '_', c == '$', c == '#':
			if i == 0 {
				simple = false
			}
		default:
			simple = false
		}
	}
	if simple && !isReservedWord(name) {
		return strings.ToUpper(name)
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isReservedWord(name string) bool {
	switch strings.ToUpper(name) {
	case "ACCESS", "ADD", "ALL", "ALTER", "AND", "ANY", "AS", "ASC", "AUDIT", "BETWEEN", "BY", "CHAR", "CHECK",
		"CLUSTER", "COLUMN", "COMMENT", "COMPRESS", "CONNECT", "CREATE", "CURRENT", "DATE", "DECIMAL", "DEFAULT",
		"DELETE", "DESC", "DISTINCT", "DROP", "ELSE", "EXCLUSIVE", "EXISTS", "FILE", "FLOAT", "FOR", "FROM", "GRANT",
		"GROUP", "HAVING", "IDENTIFIED", "IMMEDIATE", "IN", "INCREMENT", "INDEX", "INITIAL", "INSERT", "INTEGER",
		"INTERSECT", "INTO", "IS", "LEVEL", "LIKE", "LOCK", "LONG", "MAXEXTENTS", "MINUS", "MLSLABEL", "MODE",
		"MODIFY", "NOAUDIT", "NOCOMPRESS", "NOT", "NOWAIT", "NULL", "NUMBER", "OF", "OFFLINE", "ON", "ONLINE",
		"OPTION", "OR", "ORDER", "PCTFREE", "PRIOR", "PUBLIC", "RAW", "RENAME", "RESOURCE", "REVOKE", "ROW",
		"ROWID", "ROWNUM", "ROWS", "SELECT", "SESSION", "SET", "SHARE", "SIZE", "SMALLINT", "START", "SUCCESSFUL",
		"SYNONYM", "SYSDATE", "TABLE", "THEN", "TO", "TRIGGER", "UID", "UNION", "UNIQUE", "UPDATE", "USER",
		"VALIDATE", "VALUES", "VARCHAR", "VARCHAR2", "VIEW", "WHENEVER", "WHERE", "WITH":
		return true
	}
	return false
}

// isoIntervalDS renders an INTERVAL DAY TO SECOND as an ISO-8601 duration.
func isoIntervalDS(v oratype.IntervalDS) string {
	neg := v.Days < 0 || v.Hours < 0 || v.Minutes < 0 || v.Seconds < 0 || v.Nanos < 0
	abs := func(x int32) int32 {
		if x < 0 {
			return -x
		}
		return x
	}
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	sb.WriteByte('P')
	if v.Days != 0 {
		fmt.Fprintf(&sb, "%dD", abs(v.Days))
	}
	sb.WriteByte('T')
	if v.Hours != 0 {
		fmt.Fprintf(&sb, "%dH", abs(v.Hours))
	}
	if v.Minutes != 0 {
		fmt.Fprintf(&sb, "%dM", abs(v.Minutes))
	}
	if v.Nanos != 0 {
		secs := fmt.Sprintf("%d.%09d", abs(v.Seconds), abs(v.Nanos))
		sb.WriteString(strings.TrimRight(strings.TrimRight(secs, "0"), "."))
		sb.WriteByte('S')
		return sb.String()
	}
	fmt.Fprintf(&sb, "%dS", abs(v.Seconds))
	return sb.String()
}

// outBindColumns synthesizes describe-style columns for PL/SQL OUT / IN OUT
// binds so the standard Arrow conversion can be reused.
func outBindColumns(st *ttc.Statement, binds []ttc.BindColumn, bound *arrow.Schema) []ttc.Column {
	dirs := st.OutBindDirs()
	names := st.BindNames()
	var cols []ttc.Column
	for i, dir := range dirs {
		if dir == ttc.BindDirInput || i >= len(binds) {
			continue
		}
		name := ""
		if i < len(names) {
			name = names[i]
		}
		b := binds[i]
		c := ttc.Column{Name: name, OraTypeNum: b.OraTypeNum, CSForm: b.CSForm, NullsAllowed: true, BufferSize: b.BufferSize, FetchType: b.OraTypeNum, FetchCSForm: b.CSForm}
		switch b.OraTypeNum {
		case ttc.TypeTimestamp, ttc.TypeTimestampTZ, ttc.TypeTimestampLTZ:
			c.Scale = 9
		case ttc.TypeNumber:
			// Shape the NUMBER after the Arrow type the caller bound so an
			// int64 placeholder comes back as int64, a decimal as decimal.
			if bound != nil && i < bound.NumFields() {
				switch t := bound.Field(i).Type.(type) {
				case *arrow.Int8Type, *arrow.Int16Type, *arrow.Int32Type, *arrow.Int64Type,
					*arrow.Uint8Type, *arrow.Uint16Type, *arrow.Uint32Type, *arrow.Uint64Type, *arrow.BooleanType:
					c.Precision, c.Scale = 18, 0
				case *arrow.Decimal128Type:
					c.Precision, c.Scale = int8(t.Precision), int8(t.Scale)
				case *arrow.Decimal256Type:
					if t.Precision <= 38 {
						c.Precision, c.Scale = int8(t.Precision), int8(t.Scale)
					}
				}
			}
		}
		cols = append(cols, c)
	}
	return cols
}

// recordFromOutBinds builds the one-row result set carrying PL/SQL OUT /
// IN OUT bind values (Columnar-compatible: metadata ORACLE:parameter_type).
func recordFromOutBinds(alloc memory.Allocator, st *ttc.Statement, binds []ttc.BindColumn, bound *arrow.Schema, opts typeOptions) (arrow.Record, error) {
	cols := outBindColumns(st, binds, bound)
	dirs := st.OutBindDirs()
	fields := make([]arrow.Field, len(cols))
	appIdx := make([]int, 0, len(cols))
	for i := range dirs {
		if dirs[i] != ttc.BindDirInput && i < len(binds) {
			appIdx = append(appIdx, i)
		}
	}
	for i := range cols {
		dt, err := arrowTypeFor(&cols[i], opts)
		if err != nil {
			return nil, err
		}
		pt := "OUT"
		if dirs[appIdx[i]] == ttc.BindDirInputOutput {
			pt = "IN OUT"
		}
		md := arrow.NewMetadata([]string{"ORACLE:type", "ORACLE:parameter_type"}, []string{cols[i].TypeName(), pt})
		fields[i] = arrow.Field{Name: cols[i].Name, Type: dt, Nullable: true, Metadata: md}
	}
	schema := arrow.NewSchema(fields, nil)
	bldr := array.NewRecordBuilder(alloc, schema)
	defer bldr.Release()
	sink := &arrowSink{alloc: alloc, stmt: st, opts: opts}
	for i := range cols {
		v, null := st.OutBindValue(appIdx[i])
		if null {
			bldr.Field(i).AppendNull()
			continue
		}
		app := sink.makeAppender(&cols[i], bldr.Field(i))
		if err := app(v); err != nil {
			return nil, err
		}
	}
	return bldr.NewRecord(), nil
}
