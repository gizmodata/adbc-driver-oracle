package ttc

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func renderValue(v Value) string {
	switch v.Kind {
	case KindNull:
		return "null"
	case KindScalar:
		switch v.Meta.OraTypeNum {
		case TypeNumber:
			s, _ := oratypeDecodeNumber(v.Raw)
			return s
		case TypeVarchar, TypeChar:
			return fmt.Sprintf("%q", v.Raw)
		}
		return fmt.Sprintf("%s(%x)", v.Meta.TypeName(), v.Raw)
	case KindObject:
		parts := []string{}
		for i, f := range v.Fields {
			parts = append(parts, v.Type.Attrs[i].Name+"="+renderValue(f))
		}
		return v.Type.Name + "{" + strings.Join(parts, ", ") + "}"
	case KindCollection:
		parts := []string{}
		for _, e := range v.Elements {
			parts = append(parts, renderValue(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case KindXMLString:
		return "xml:" + string(v.Raw)
	case KindLOB:
		return fmt.Sprintf("lob(%d bytes locator)", len(v.Raw))
	}
	return "?"
}

func TestLiveObjects(t *testing.T) {
	ctx := context.Background()
	c, err := Dial(ctx, liveConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exec := func(sql string) {
		t.Helper()
		st := c.Prepare(sql)
		if err := st.Execute(ctx, ExecuteOptions{AutoCommit: true}); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st.Close()
	}
	for _, sql := range []string{"DROP TABLE ttc_obj_t PURGE", "DROP TYPE ttc_person FORCE", "DROP TYPE ttc_addr_list FORCE", "DROP TYPE ttc_addr FORCE"} {
		c.Prepare(sql).Execute(ctx, ExecuteOptions{})
	}
	exec("CREATE TYPE ttc_addr AS OBJECT (street VARCHAR2(40), zip NUMBER(5), since DATE)")
	exec("CREATE TYPE ttc_addr_list AS VARRAY(5) OF ttc_addr")
	exec("CREATE TYPE ttc_person AS OBJECT (name VARCHAR2(30), age NUMBER, home ttc_addr, others ttc_addr_list, tags SYS.ODCIVARCHAR2LIST, score BINARY_DOUBLE)")
	hasSpatial := false
	if rows, err := c.dictQuery(ctx, "SELECT COUNT(*) FROM ALL_TYPES WHERE OWNER = 'MDSYS' AND TYPE_NAME = 'SDO_GEOMETRY'"); err == nil && len(rows) == 1 && rows[0].vals[0] == "1" {
		hasSpatial = true
	}
	t.Logf("spatial available: %v", hasSpatial)
	geomCol, geomVal1, geomVal2 := "", "NULL", "NULL"
	if hasSpatial {
		geomCol = ", g MDSYS.SDO_GEOMETRY"
		geomVal1 = "SDO_GEOMETRY(2001, 4326, SDO_POINT_TYPE(-122.4, 37.8, NULL), NULL, NULL)"
		geomVal2 = "SDO_GEOMETRY(2003, NULL, NULL, SDO_ELEM_INFO_ARRAY(1,1003,1), SDO_ORDINATE_ARRAY(0,0, 10,0, 10,10, 0,10, 0,0))"
	} else {
		geomCol = ", g NUMBER"
	}
	exec("CREATE TABLE ttc_obj_t (id NUMBER, p ttc_person, x XMLTYPE" + geomCol + ")")
	exec(`INSERT INTO ttc_obj_t VALUES (1,
		ttc_person('Ann', 41, ttc_addr('1 Main St', 12345, DATE '2020-01-02'),
		           ttc_addr_list(ttc_addr('2 Side St', 54321, NULL), NULL),
		           SYS.ODCIVARCHAR2LIST('a', 'b'), 2.5),
		XMLTYPE('<doc><a>1</a></doc>'), ` + geomVal1 + `)`)
	exec("INSERT INTO ttc_obj_t VALUES (2, NULL, NULL, " + geomVal2 + ")")

	st := c.Prepare("SELECT id, p, x, g FROM ttc_obj_t ORDER BY id")
	type rawRow struct {
		vals  [][]byte
		nulls []bool
	}
	var rows []rawRow
	var cur rawRow
	sink := sinkFuncs{
		value: func(col int, data []byte) error {
			cur.vals = append(cur.vals, append([]byte(nil), data...))
			cur.nulls = append(cur.nulls, false)
			return nil
		},
		null:   func(col int) error { cur.vals = append(cur.vals, nil); cur.nulls = append(cur.nulls, true); return nil },
		finish: func() error { rows = append(rows, cur); cur = rawRow{}; return nil },
	}
	if err := st.Execute(ctx, ExecuteOptions{Sink: sink}); err != nil {
		t.Fatal(err)
	}
	for st.MoreRows() {
		if err := st.Fetch(ctx, sink, 100); err != nil {
			t.Fatal(err)
		}
	}
	cols := st.Columns()
	for _, col := range cols {
		t.Logf("col %s type=%d obj=%s.%s oid=%x", col.Name, col.OraTypeNum, col.ObjectTypeSchema, col.ObjectTypeName, col.ObjectTypeOID)
	}
	st.Close()
	pt, err := c.ObjectType(ctx, cols[1].ObjectTypeSchema, cols[1].ObjectTypeName)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range pt.Attrs {
		t.Logf("attr %s %s", a.Name, a.TypeName())
	}
	var gt *ObjectType
	if hasSpatial {
		gt, err = c.ObjectType(ctx, "MDSYS", "SDO_GEOMETRY")
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range rows {
		if !r.nulls[1] {
			v, err := DecodeObjectImage(pt, r.vals[1])
			if err != nil {
				t.Fatalf("row %d person: %v", i, err)
			}
			t.Logf("row %d person: %s", i, renderValue(v))
			if i == 0 && !strings.Contains(renderValue(v), `ZIP=12345`) {
				t.Fatal("bad decode")
			}
		}
		if !r.nulls[2] {
			v, err := DecodeXMLTypeImage(r.vals[2])
			if err != nil {
				t.Fatalf("row %d xml: %v", i, err)
			}
			if v.Kind == KindLOB {
				data, err := c.ReadLOB(ctx, v.Raw, true, false)
				if err != nil {
					t.Fatalf("xml lob read: %v", err)
				}
				t.Logf("row %d xml(lob): %s", i, data)
			} else {
				t.Logf("row %d xml: %s", i, renderValue(v))
			}
		}
		if gt != nil && !r.nulls[3] {
			v, err := DecodeObjectImage(gt, r.vals[3])
			if err != nil {
				t.Fatalf("row %d geom: %v", i, err)
			}
			t.Logf("row %d geom: %s", i, renderValue(v))
		}
	}
	for _, sql := range []string{"DROP TABLE ttc_obj_t PURGE", "DROP TYPE ttc_person", "DROP TYPE ttc_addr_list", "DROP TYPE ttc_addr"} {
		exec(sql)
	}
}

type sinkFuncs struct {
	value  func(col int, data []byte) error
	null   func(col int) error
	finish func() error
}

func (s sinkFuncs) AppendValue(col int, data []byte) error { return s.value(col, data) }
func (s sinkFuncs) AppendNull(col int) error               { return s.null(col) }
func (s sinkFuncs) AppendDuplicate(col int) error          { return s.null(col) }
func (s sinkFuncs) FinishRow() error                       { return s.finish() }
