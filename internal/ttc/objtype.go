package ttc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Collection kinds.
const (
	CollNone       = 0
	CollIndexTable = 1 // PL/SQL associative array
	CollNestedTbl  = 2
	CollVarray     = 3
)

// ObjectType describes a user-defined object or collection type.
type ObjectType struct {
	Schema       string
	Name         string
	IsCollection bool
	CollKind     int
	MaxElements  int
	Element      *AttrMeta // collection element type
	Attrs        []*AttrMeta
	IsXMLType    bool
	resolved     bool
}

// FQN returns SCHEMA.NAME.
func (t *ObjectType) FQN() string { return t.Schema + "." + t.Name }

// AttrMeta describes an attribute or collection element.
type AttrMeta struct {
	Name       string
	OraTypeNum uint8
	CSForm     uint8
	Precision  int8
	Scale      int8
	MaxSize    int
	ObjType    *ObjectType // for object / collection valued attributes
}

// TypeName renders the attribute's Oracle type for display.
func (a *AttrMeta) TypeName() string {
	if a.ObjType != nil {
		return a.ObjType.FQN()
	}
	return (&Column{OraTypeNum: a.OraTypeNum, CSForm: a.CSForm, Scale: a.Scale}).TypeName()
}

// objTypeCache memoizes resolved types per connection.
type objTypeCache struct {
	byName map[string]*ObjectType
}

// ObjectType resolves (and caches) the metadata of a user-defined type
// from the data dictionary. It must not be called while a response is
// being processed.
func (c *Conn) ObjectType(ctx context.Context, schema, name string) (*ObjectType, error) {
	if c.objTypes == nil {
		c.objTypes = &objTypeCache{byName: map[string]*ObjectType{}}
	}
	key := schema + "." + name
	if t, ok := c.objTypes.byName[key]; ok {
		if !t.resolved {
			return nil, fmt.Errorf("oracle: type %s is recursively defined", key)
		}
		return t, nil
	}
	t := &ObjectType{Schema: schema, Name: name, IsXMLType: schema == "SYS" && name == "XMLTYPE"}
	c.objTypes.byName[key] = t
	if t.IsXMLType {
		t.resolved = true
		return t, nil
	}
	if err := c.resolveObjectType(ctx, t); err != nil {
		delete(c.objTypes.byName, key)
		return nil, err
	}
	t.resolved = true
	return t, nil
}

type dictRow struct {
	vals  []string
	nulls []bool
}

type dictSink struct {
	stmt *Statement
	rows []dictRow
	cur  dictRow
	last dictRow
}

func (s *dictSink) AppendValue(col int, data []byte) error {
	var v string
	switch s.stmt.Columns()[col].FetchType {
	case TypeNumber, TypeBinaryInteger:
		d, err := decodeNumberText(data)
		if err != nil {
			return err
		}
		v = d
	default:
		v = string(data)
	}
	s.cur.vals = append(s.cur.vals, v)
	s.cur.nulls = append(s.cur.nulls, false)
	return nil
}
func (s *dictSink) AppendNull(int) error {
	s.cur.vals = append(s.cur.vals, "")
	s.cur.nulls = append(s.cur.nulls, true)
	return nil
}
func (s *dictSink) AppendDuplicate(col int) error {
	s.cur.vals = append(s.cur.vals, s.last.vals[col])
	s.cur.nulls = append(s.cur.nulls, s.last.nulls[col])
	return nil
}
func (s *dictSink) FinishRow() error {
	s.rows = append(s.rows, s.cur)
	s.last = s.cur
	s.cur = dictRow{}
	return nil
}

func (c *Conn) dictQuery(ctx context.Context, sql string, args ...string) ([]dictRow, error) {
	st := c.Prepare(sql)
	defer st.Close()
	binds := make([]BindColumn, len(args))
	for i, a := range args {
		v := a
		binds[i] = BindColumn{OraTypeNum: TypeVarchar, CSForm: CSFormImplicit, BufferSize: uint32(max(len(v), 1)), Value: func(int) ([]byte, error) {
			if v == "" {
				return nil, nil
			}
			return []byte(v), nil
		}}
	}
	sink := &dictSink{stmt: st}
	if err := st.Execute(ctx, ExecuteOptions{Binds: binds, PrefetchRows: 500, ArraySize: 500, Sink: sink}); err != nil {
		return nil, err
	}
	for st.MoreRows() {
		if err := st.Fetch(ctx, sink, 500); err != nil {
			return nil, err
		}
	}
	return sink.rows, nil
}

func (c *Conn) resolveObjectType(ctx context.Context, t *ObjectType) error {
	rows, err := c.dictQuery(ctx, "SELECT TYPECODE FROM ALL_TYPES WHERE OWNER = :1 AND TYPE_NAME = :2", t.Schema, t.Name)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("oracle: object type %s not found in ALL_TYPES", t.FQN())
	}
	if rows[0].vals[0] == "COLLECTION" {
		t.IsCollection = true
		rows, err = c.dictQuery(ctx, `SELECT COLL_TYPE, UPPER_BOUND, ELEM_TYPE_OWNER, ELEM_TYPE_NAME, LENGTH, PRECISION, SCALE, CHARACTER_SET_NAME
			FROM ALL_COLL_TYPES WHERE OWNER = :1 AND TYPE_NAME = :2`, t.Schema, t.Name)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("oracle: collection type %s not found in ALL_COLL_TYPES", t.FQN())
		}
		r := rows[0]
		if strings.HasPrefix(r.vals[0], "VARYING") {
			t.CollKind = CollVarray
		} else {
			t.CollKind = CollNestedTbl
		}
		t.MaxElements, _ = strconv.Atoi(r.vals[1])
		elem, err := c.attrMeta(ctx, "", r.vals[2], r.nulls[2], r.vals[3], r.vals[4], r.vals[5], r.nulls[5], r.vals[6], r.nulls[6], r.vals[7])
		if err != nil {
			return err
		}
		t.Element = elem
		return nil
	}
	rows, err = c.dictQuery(ctx, `SELECT ATTR_NAME, ATTR_TYPE_OWNER, ATTR_TYPE_NAME, LENGTH, PRECISION, SCALE, CHARACTER_SET_NAME
		FROM ALL_TYPE_ATTRS WHERE OWNER = :1 AND TYPE_NAME = :2 ORDER BY ATTR_NO`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	for _, r := range rows {
		a, err := c.attrMeta(ctx, r.vals[0], r.vals[1], r.nulls[1], r.vals[2], r.vals[3], r.vals[4], r.nulls[4], r.vals[5], r.nulls[5], r.vals[6])
		if err != nil {
			return err
		}
		t.Attrs = append(t.Attrs, a)
	}
	return nil
}

func (c *Conn) attrMeta(ctx context.Context, name, typeOwner string, ownerNull bool, typeName, length, precision string, precNull bool, scale string, scaleNull bool, charset string) (*AttrMeta, error) {
	a := &AttrMeta{Name: name}
	if !ownerNull && typeOwner != "" {
		ot, err := c.ObjectType(ctx, typeOwner, typeName)
		if err != nil {
			return nil, err
		}
		a.OraTypeNum = TypeObject
		a.ObjType = ot
		return a, nil
	}
	a.MaxSize, _ = strconv.Atoi(length)
	p, _ := strconv.Atoi(precision)
	s, _ := strconv.Atoi(scale)
	base := typeName
	if i := strings.IndexByte(base, '('); i > 0 {
		if j := strings.IndexByte(base, ')'); j > i {
			base = base[:i] + base[j+1:]
		}
	}
	base = strings.TrimSpace(base)
	nchar := charset == "NCHAR_CS"
	switch {
	case base == "VARCHAR2" || base == "VARCHAR" || base == "NVARCHAR2":
		a.OraTypeNum, a.CSForm = TypeVarchar, CSFormImplicit
	case base == "CHAR" || base == "NCHAR":
		a.OraTypeNum, a.CSForm = TypeChar, CSFormImplicit
	case base == "NUMBER" || base == "DECIMAL" || base == "NUMERIC":
		a.OraTypeNum = TypeNumber
		if !precNull {
			a.Precision = int8(p)
		}
		if !scaleNull {
			a.Scale = int8(s)
		} else if precNull {
			a.Scale = -127
		}
	case base == "INTEGER" || base == "SMALLINT" || base == "INT":
		a.OraTypeNum, a.Precision, a.Scale = TypeNumber, 38, 0
	case base == "FLOAT" || base == "REAL" || base == "DOUBLE PRECISION":
		a.OraTypeNum, a.Scale = TypeNumber, -127
	case base == "BINARY_FLOAT":
		a.OraTypeNum = TypeBinaryFloat
	case base == "BINARY_DOUBLE":
		a.OraTypeNum = TypeBinaryDouble
	case base == "BINARY_INTEGER" || base == "PLS_INTEGER":
		a.OraTypeNum = TypeBinaryInteger
	case base == "RAW":
		a.OraTypeNum = TypeRaw
	case base == "DATE":
		a.OraTypeNum = TypeDate
	case strings.HasPrefix(base, "TIMESTAMP") && strings.Contains(base, "LOCAL"):
		a.OraTypeNum, a.Scale = TypeTimestampLTZ, int8(s)
	case strings.HasPrefix(base, "TIMESTAMP") && strings.Contains(base, "TIME ZONE"):
		a.OraTypeNum, a.Scale = TypeTimestampTZ, int8(s)
	case strings.HasPrefix(base, "TIMESTAMP"):
		a.OraTypeNum, a.Scale = TypeTimestamp, int8(s)
	case strings.HasPrefix(base, "INTERVAL DAY"):
		a.OraTypeNum, a.Scale = TypeIntervalDS, int8(s)
	case strings.HasPrefix(base, "INTERVAL YEAR"):
		a.OraTypeNum = TypeIntervalYM
	case base == "CLOB" || base == "NCLOB":
		a.OraTypeNum, a.CSForm = TypeClob, CSFormImplicit
	case base == "BLOB":
		a.OraTypeNum = TypeBlob
	case base == "BFILE":
		a.OraTypeNum = TypeBfile
	case base == "BOOLEAN":
		a.OraTypeNum = TypeBoolean
	case base == "ROWID" || base == "UROWID":
		a.OraTypeNum, a.CSForm = TypeVarchar, CSFormImplicit
	case base == "JSON":
		a.OraTypeNum = TypeJSON
	default:
		return nil, fmt.Errorf("oracle: attribute %q: type %s is not supported inside objects", name, typeName)
	}
	if nchar && a.CSForm != 0 {
		a.CSForm = CSFormNChar
	}
	if strings.HasPrefix(typeName, "N") && (base == "NVARCHAR2" || base == "NCHAR" || base == "NCLOB") {
		a.CSForm = CSFormNChar
	}
	return a, nil
}

func decodeNumberText(data []byte) (string, error) {
	d, err := oratypeDecodeNumber(data)
	if err != nil {
		return "", err
	}
	return d, nil
}
